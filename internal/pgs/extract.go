package pgs

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"image/png"
	"io"
	"log/slog"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"kinora/internal/ffmpeg"
)

// Timeline is a PGS track ready for an app to draw: when each set of
// pictures shows, and where. Pictures are numbered PNGs.
type Timeline struct {
	// Width and Height are the video frame that positions refer to.
	Width  int             `json:"width"`
	Height int             `json:"height"`
	Events []TimelineEvent `json:"events"`
	// Done is false while the track is still being read; ask again (from
	// the number of events already had) for the rest.
	Done bool `json:"done"`
	// Total is how many events there are so far.
	Total int    `json:"total"`
	Error string `json:"error,omitempty"`
}

type TimelineEvent struct {
	Start  float64         `json:"start"`
	End    float64         `json:"end"`
	Images []TimelineImage `json:"images"`
}

type TimelineImage struct {
	// N names the picture: n.png.
	N      int  `json:"n"`
	X      int  `json:"x"`
	Y      int  `json:"y"`
	W      int  `json:"w"`
	H      int  `json:"h"`
	Forced bool `json:"forced,omitempty"`
}

// Extractor reads PGS tracks out of video files into timelines and PNGs, once
// per track, kept on disk. Reading a track copies just that track out of the
// file (no decoding of picture or sound), so it goes at the speed of the
// disk, and the first subtitles are ready within moments.
type Extractor struct {
	Tool *ffmpeg.Tool
	Dir  string
	Log  *slog.Logger
	// Parallel caps tracks read at once (default 2).
	Parallel int

	mu    sync.Mutex
	jobs  map[string]*job
	slots chan struct{}
}

type job struct {
	mu       sync.Mutex
	tl       Timeline
	images   int
	keys     map[string]TimelineImage
	failedAt time.Time
}

// ErrNoFFmpeg is returned when the server has no ffmpeg to read tracks with.
var ErrNoFFmpeg = errors.New("ffmpeg was not found")

// cacheVersion changes when what's stored changes, so old reads are redone
// (v2: pictures told apart by their pixels; v1 mixed up lines).
const cacheVersion = 2

func key(versionID string, index int, mtime int64) string {
	return fmt.Sprintf("v%d-%s-%d-%d", cacheVersion, versionID, index, mtime)
}

// Timeline returns a track's timeline from event `from` on, starting the
// read if it hasn't been. path is the video file; mtime invalidates the
// cache when the file changes.
func (x *Extractor) Timeline(versionID, path string, mtime int64, index, from int) (Timeline, error) {
	k := key(versionID, index, mtime)
	dir := filepath.Join(x.Dir, k)

	x.mu.Lock()
	if x.jobs == nil {
		x.jobs = map[string]*job{}
		n := x.Parallel
		if n <= 0 {
			n = 2
		}
		x.slots = make(chan struct{}, n)
		go x.dropOld()
	}
	j := x.jobs[k]
	if j == nil {
		// Read before? The finished timeline is on disk.
		if b, err := os.ReadFile(filepath.Join(dir, "timeline.json")); err == nil {
			var tl Timeline
			if json.Unmarshal(b, &tl) == nil && tl.Done {
				x.mu.Unlock()
				return slice(tl, from), nil
			}
		}
		if !x.Tool.Caps().Found {
			x.mu.Unlock()
			return Timeline{}, ErrNoFFmpeg
		}
		j = &job{keys: map[string]TimelineImage{}, tl: Timeline{Events: []TimelineEvent{}}}
		x.jobs[k] = j
		go x.run(j, k, dir, path, index)
	}
	x.mu.Unlock()

	j.mu.Lock()
	defer j.mu.Unlock()
	tl := slice(j.tl, from)
	// A failed read is retried by the next request after a minute.
	if j.tl.Error != "" && time.Since(j.failedAt) > time.Minute {
		x.mu.Lock()
		if x.jobs[k] == j {
			delete(x.jobs, k)
		}
		x.mu.Unlock()
	}
	return tl, nil
}

// dropOld removes tracks read by an older version of this code.
func (x *Extractor) dropOld() {
	entries, err := os.ReadDir(x.Dir)
	if err != nil {
		return
	}
	prefix := fmt.Sprintf("v%d-", cacheVersion)
	for _, e := range entries {
		if e.IsDir() && !strings.HasPrefix(e.Name(), prefix) {
			_ = os.RemoveAll(filepath.Join(x.Dir, e.Name()))
		}
	}
}

// slice copies a timeline with events from `from` on.
func slice(tl Timeline, from int) Timeline {
	out := tl
	out.Total = len(tl.Events)
	from = min(max(from, 0), len(tl.Events))
	out.Events = append([]TimelineEvent{}, tl.Events[from:]...)
	return out
}

// ImagePath is where picture n of a track is, once made.
func (x *Extractor) ImagePath(versionID string, mtime int64, index, n int) string {
	return filepath.Join(x.Dir, key(versionID, index, mtime), strconv.Itoa(n)+".png")
}

func (x *Extractor) logger() *slog.Logger {
	if x.Log != nil {
		return x.Log
	}
	return slog.New(slog.DiscardHandler)
}

func (x *Extractor) run(j *job, k, dir, path string, index int) {
	x.slots <- struct{}{}
	defer func() { <-x.slots }()
	started := time.Now()
	err := x.read(j, dir, path, index)

	j.mu.Lock()
	defer j.mu.Unlock()
	if err != nil {
		j.tl.Error = err.Error()
		j.tl.Done = true
		j.failedAt = time.Now()
		x.logger().Warn("picture subtitles failed", "file", path, "track", index, "err", err)
		return
	}
	j.tl.Done = true
	j.tl.Total = len(j.tl.Events)
	if b, err := json.Marshal(j.tl); err == nil {
		tmp := filepath.Join(dir, "timeline.json.tmp")
		if os.WriteFile(tmp, b, 0o644) == nil {
			_ = os.Rename(tmp, filepath.Join(dir, "timeline.json"))
		}
	}
	x.logger().Info("picture subtitles ready", "file", filepath.Base(path), "track", index,
		"events", len(j.tl.Events), "pictures", j.images, "took", time.Since(started).Round(time.Millisecond))
	// Done jobs read from disk next time; drop it from memory after a while.
	time.AfterFunc(10*time.Minute, func() {
		x.mu.Lock()
		if x.jobs[k] == j {
			delete(x.jobs, k)
		}
		x.mu.Unlock()
	})
}

// read copies the track out with ffmpeg and decodes it as it arrives.
func (x *Extractor) read(j *job, dir, path string, index int) error {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Hour)
	defer cancel()
	cmd := exec.CommandContext(ctx, x.Tool.Path(), "-nostdin", "-hide_banner", "-loglevel", "error",
		"-i", path, "-map", "0:"+strconv.Itoa(index), "-c:s", "copy", "-f", "sup", "pipe:1")
	tail := &ffmpeg.Tail{N: 1024}
	cmd.Stderr = tail
	out, err := cmd.StdoutPipe()
	if err != nil {
		return err
	}
	if err := ffmpeg.StartLow(cmd); err != nil {
		return err
	}
	decodeErr := Decode(out, func(ev Event) error { return x.add(j, dir, ev) })
	if decodeErr != nil {
		// Stop ffmpeg rather than wait for it to fill a pipe nobody reads.
		cancel()
		_, _ = io.Copy(io.Discard, out)
	}
	waitErr := cmd.Wait()
	if decodeErr != nil {
		return decodeErr
	}
	if waitErr != nil {
		return fmt.Errorf("ffmpeg: %v %s", waitErr, tail.String())
	}
	return nil
}

// add stores an event's pictures (each distinct bitmap once) and appends
// it to the timeline, or extends the last event if it's the same pictures
// carrying on.
func (x *Extractor) add(j *job, dir string, ev Event) error {
	imgs := make([]TimelineImage, 0, len(ev.Images))
	for _, im := range ev.Images {
		j.mu.Lock()
		ti, ok := j.keys[im.Key]
		j.mu.Unlock()
		if !ok {
			j.mu.Lock()
			n := j.images
			j.images++
			j.mu.Unlock()
			if err := writePNG(filepath.Join(dir, strconv.Itoa(n)+".png"), im); err != nil {
				return err
			}
			b := im.Img.Bounds()
			ti = TimelineImage{N: n, W: b.Dx(), H: b.Dy()}
			j.mu.Lock()
			j.keys[im.Key] = ti
			j.mu.Unlock()
		}
		ti.X, ti.Y, ti.Forced = im.X, im.Y, im.Forced
		imgs = append(imgs, ti)
	}
	te := TimelineEvent{Start: round(ev.Start), End: round(ev.End), Images: imgs}

	j.mu.Lock()
	defer j.mu.Unlock()
	if j.tl.Width == 0 {
		j.tl.Width, j.tl.Height = ev.Width, ev.Height
	}
	if n := len(j.tl.Events); n > 0 {
		last := &j.tl.Events[n-1]
		if last.End == te.Start && sameImages(last.Images, te.Images) {
			last.End = te.End
			return nil
		}
	}
	j.tl.Events = append(j.tl.Events, te)
	j.tl.Total = len(j.tl.Events)
	return nil
}

func sameImages(a, b []TimelineImage) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func round(v float64) float64 { return math.Round(v*1000) / 1000 }

func writePNG(path string, im Image) error {
	tmp := path + ".tmp"
	f, err := os.Create(tmp)
	if err != nil {
		return err
	}
	enc := png.Encoder{CompressionLevel: png.BestSpeed}
	if err := enc.Encode(f, im.Img); err != nil {
		f.Close()
		os.Remove(tmp)
		return err
	}
	if err := f.Close(); err != nil {
		os.Remove(tmp)
		return err
	}
	return os.Rename(tmp, path)
}
