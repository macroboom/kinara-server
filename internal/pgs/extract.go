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

	"kinara/internal/ffmpeg"
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

// Extractor reads picture subtitle tracks (Blu-ray PGS, DVD and DVB) out of
// video files into timelines and PNGs, once per track, kept on disk. Reading
// a track copies just that track out of the file (no decoding of picture or
// sound), so it goes at the speed of the disk, and the first subtitles are
// ready within moments.
type Extractor struct {
	Tool *ffmpeg.Tool
	// FFprobe reads DVD and DVB tracks ("" for ffprobe on PATH).
	FFprobe string
	Dir     string
	Log     *slog.Logger
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

// Codecs are the subtitle codecs (as ffprobe names them) the extractor reads.
var Codecs = map[string]bool{"hdmv_pgs_subtitle": true, "dvd_subtitle": true, "dvb_subtitle": true}

// Timeline returns a track's timeline from event `from` on, starting the
// read if it hasn't been. path is the video file; mtime invalidates the
// cache when the file changes; codec is the track's (see Codecs).
func (x *Extractor) Timeline(versionID, path string, mtime int64, index int, codec string, from int) (Timeline, error) {
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
		go x.run(j, k, dir, path, index, codec)
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

func (x *Extractor) run(j *job, k, dir, path string, index int, codec string) {
	x.slots <- struct{}{}
	defer func() { <-x.slots }()
	started := time.Now()
	var err error
	if codec == "dvd_subtitle" || codec == "dvb_subtitle" {
		err = x.readDVD(j, dir, path, index, codec)
	} else {
		err = x.read(j, dir, path, index)
	}

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
	// The first event sets the frame; a DVB track whose pictures turn out
	// to reach past it (a high definition one) widens it.
	if j.tl.Width == 0 || ev.Width > j.tl.Width || ev.Height > j.tl.Height {
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

// --- DVD and DVB ---------------------------------------------------------------------------

// readDVD reads a DVD or DVB track. ffmpeg copies the track out (DVB is
// converted to DVD subpictures on the way, with a known palette) into a
// pipe that ffprobe lists packet by packet, so subtitles are added as the
// read goes, as with PGS.
func (x *Extractor) readDVD(j *job, dir, path string, index int, codec string) error {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Hour)
	defer cancel()
	probeBin := x.FFprobe
	if probeBin == "" {
		probeBin = "ffprobe"
	}
	head, err := probeHeader(ctx, probeBin, path)
	if err != nil {
		return err
	}
	var pal dvdPalette
	var fw, fh int
	args := []string{"-nostdin", "-hide_banner", "-loglevel", "error", "-i", path, "-map", "0:" + strconv.Itoa(index)}
	if codec == "dvd_subtitle" {
		pal, fw, fh = parseIdx(head.extradata(index))
		if fw <= 0 || fh <= 0 {
			fw, fh = frameFor(head.videoW, head.videoH)
		}
		args = append(args, "-c:s", "copy")
	} else {
		pal, _, _ = parseIdx("palette: " + strings.ReplaceAll(dvbPalette, ",", ", "))
		// Standard DVB pictures sit in a 720 by 576 frame; high definition
		// ones widen it as they come (see add).
		fw, fh = 720, 576
		args = append(args, "-c:s", "dvdsub", "-palette", dvbPalette)
	}
	args = append(args, "-f", "matroska", "pipe:1")

	copier := exec.CommandContext(ctx, x.Tool.Path(), args...)
	copyTail := &ffmpeg.Tail{N: 1024}
	copier.Stderr = copyTail
	lister := exec.CommandContext(ctx, probeBin, "-v", "error", "-show_packets", "-show_data", "-of", "json", "-i", "pipe:0")
	listTail := &ffmpeg.Tail{N: 1024}
	lister.Stderr = listTail
	pipe, err := copier.StdoutPipe()
	if err != nil {
		return err
	}
	lister.Stdin = pipe
	out, err := lister.StdoutPipe()
	if err != nil {
		return err
	}
	if err := ffmpeg.StartLow(copier); err != nil {
		return err
	}
	if err := ffmpeg.StartLow(lister); err != nil {
		cancel()
		_ = copier.Wait()
		return err
	}

	videoFrame := func() (int, int) {
		if head.videoW > 0 && head.videoH > 0 {
			return head.videoW, head.videoH
		}
		return 1920, 1080
	}
	var pending *Event
	emit := func(next float64) error {
		if pending == nil {
			return nil
		}
		ev := *pending
		pending = nil
		if ev.End <= ev.Start {
			// No stop time: until the next subtitle, at most 10 seconds.
			ev.End = ev.Start + 5
			if next > ev.Start {
				ev.End = min(next, ev.Start+10)
			}
		}
		// One subtitle at a time: the next one ends this one. DVB states a
		// page timeout rather than an end, which can be minutes.
		if next > ev.Start && ev.End > next {
			ev.End = next
		}
		ev.End = min(ev.End, ev.Start+maxSubtitle)
		return x.add(j, dir, ev)
	}
	decodeErr := listPackets(out, func(pts, dur float64, data []byte) error {
		sp, err := decodeSPU(data)
		if err != nil {
			return nil // one bad packet doesn't lose the track
		}
		start := pts + sp.start
		if err := emit(start); err != nil {
			return err
		}
		img, ix, iy, key := sp.image(&pal)
		if img == nil {
			return nil // a clearing subpicture: it only ended the one before
		}
		b := img.Bounds()
		if ix+b.Dx() > fw || iy+b.Dy() > fh {
			if vw, vh := videoFrame(); vw >= ix+b.Dx() && vh >= iy+b.Dy() {
				fw, fh = vw, vh
			} else {
				fw, fh = max(fw, ix+b.Dx()), max(fh, iy+b.Dy())
			}
		}
		ev := Event{Start: start, Width: fw, Height: fh,
			Images: []Image{{X: ix, Y: iy, Img: img, Forced: sp.forced, Key: key}}}
		switch {
		case sp.stop >= 0:
			ev.End = pts + sp.stop
		case dur > 0:
			ev.End = pts + dur
		}
		pending = &ev
		return nil
	})
	if decodeErr == nil {
		decodeErr = emit(-1)
	}
	if decodeErr != nil {
		cancel()
		_, _ = io.Copy(io.Discard, out)
	}
	listErr := lister.Wait()
	copyErr := copier.Wait()
	if decodeErr != nil {
		return decodeErr
	}
	if copyErr != nil {
		return fmt.Errorf("ffmpeg: %v %s", copyErr, copyTail.String())
	}
	if listErr != nil {
		return fmt.Errorf("ffprobe: %v %s", listErr, listTail.String())
	}
	return nil
}

// maxSubtitle is the longest a DVD or DVB subtitle stays up, in seconds.
const maxSubtitle = 15.0

// header is what readDVD needs from the file's streams.
type header struct {
	streams        []probeStream
	videoW, videoH int
}

type probeStream struct {
	Index     int    `json:"index"`
	CodecType string `json:"codec_type"`
	Width     int    `json:"width"`
	Height    int    `json:"height"`
	Extradata string `json:"extradata"`
}

func (h header) extradata(index int) string {
	for _, s := range h.streams {
		if s.Index == index {
			return string(parseHexDump(s.Extradata))
		}
	}
	return ""
}

// probeHeader lists the file's streams (with their private data, which
// holds a DVD track's palette); it reads only the start of the file.
func probeHeader(ctx context.Context, probeBin, path string) (header, error) {
	cmd := exec.CommandContext(ctx, probeBin, "-v", "error", "-show_streams", "-show_data", "-of", "json", path)
	ffmpeg.HideWindow(cmd)
	tail := &ffmpeg.Tail{N: 1024}
	cmd.Stderr = tail
	b, err := cmd.Output()
	if err != nil {
		return header{}, fmt.Errorf("ffprobe: %v %s", err, tail.String())
	}
	var res struct {
		Streams []probeStream `json:"streams"`
	}
	if err := json.Unmarshal(b, &res); err != nil {
		return header{}, fmt.Errorf("ffprobe: %w", err)
	}
	h := header{streams: res.Streams}
	for _, s := range res.Streams {
		if s.CodecType == "video" && s.Width > 0 && h.videoW == 0 {
			h.videoW, h.videoH = s.Width, s.Height
		}
	}
	return h, nil
}

// listPackets reads ffprobe's JSON packet list as it arrives and calls fn
// with each packet's time, duration (0 when unknown) and bytes.
func listPackets(r io.Reader, fn func(pts, dur float64, data []byte) error) error {
	dec := json.NewDecoder(r)
	// {"packets": [ ... ] ...}
	for {
		t, err := dec.Token()
		if err == io.EOF {
			return nil // no packets at all
		}
		if err != nil {
			return fmt.Errorf("ffprobe output: %w", err)
		}
		if key, ok := t.(string); ok && key == "packets" {
			break
		}
	}
	if t, err := dec.Token(); err != nil || t != json.Delim('[') {
		return fmt.Errorf("ffprobe output: no packet list")
	}
	for dec.More() {
		var p struct {
			PTS  string `json:"pts_time"`
			Dur  string `json:"duration_time"`
			Data string `json:"data"`
		}
		if err := dec.Decode(&p); err != nil {
			return fmt.Errorf("ffprobe output: %w", err)
		}
		pts, err := strconv.ParseFloat(p.PTS, 64)
		if err != nil {
			continue
		}
		dur, _ := strconv.ParseFloat(p.Dur, 64)
		if err := fn(pts, dur, parseHexDump(p.Data)); err != nil {
			return err
		}
	}
	return nil
}

// parseHexDump reads ffprobe's -show_data dump
// ("00000000: 016a 014c 0100 ...  .j.L..") back into bytes.
func parseHexDump(s string) []byte {
	var out []byte
	for _, line := range strings.Split(s, "\n") {
		_, rest, ok := strings.Cut(line, ": ")
		if !ok {
			continue
		}
		if i := strings.Index(rest, "  "); i >= 0 {
			rest = rest[:i]
		}
		for _, group := range strings.Fields(rest) {
			for k := 0; k+1 < len(group); k += 2 {
				v, err := strconv.ParseUint(group[k:k+2], 16, 8)
				if err != nil {
					break
				}
				out = append(out, byte(v))
			}
		}
	}
	return out
}
