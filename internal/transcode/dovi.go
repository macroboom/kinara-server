package transcode

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"kinara/internal/ffmpeg"
	"kinara/internal/probe"
)

// Dolby Vision when streaming. A Dolby Vision file is HEVC with extra
// metadata (and, in profile 7, a second "enhancement" layer). What a player
// can do with it decides how it's sent:
//
//   - A player that shows Dolby Vision gets it as is. In MP4 and HLS it is
//     tagged dvh1, the tag Apple players need to switch Dolby Vision on.
//   - Profile 7 into MP4 or HLS, or profile 7 / 8.1 to a player without
//     Dolby Vision, has its Dolby Vision parts stripped while copying: what
//     is left is the HDR10 base layer, which any HDR player shows. Players
//     choke on profile 7's second layer, and Apple's never decode it.
//   - Profile 5 has no usable base layer (copied as plain HEVC it shows
//     green and purple), so a player without Dolby Vision gets it converted
//     with libplacebo, which applies the Dolby Vision metadata first.
//
// Players say whether they show Dolby Vision with dovi=1 or dovi=0; without
// it the old behaviour holds except for the profile 7 strip into MP4/HLS.

// ErrDVNeedsPlacebo means a profile 5 file can't be shown right here.
var ErrDVNeedsPlacebo = errors.New("this Dolby Vision file (profile 5) needs a Dolby Vision player, or ffmpeg with libplacebo (Vulkan) on the server to convert it; without that its colours come out green and purple")

// Routes for copied Dolby Vision video.
const (
	dvKeep  = ""      // copy as is
	dvTag   = "dvh1"  // copy, tagged as Dolby Vision for Apple players
	dvStrip = "strip" // copy the HDR10 base layer only
)

// prepare finishes a request before ffmpeg starts: reads the file's Dolby
// Vision configuration and picks how its video goes out. It may turn a
// copy into a conversion (profile 5 to a player without Dolby Vision).
func (t *Transcoder) prepare(ctx context.Context, r *Request, caps ffmpeg.Caps) error {
	r.dvRoute = dvKeep
	if r.HDR != "dolby_vision" || !strings.EqualFold(r.SourceCodec, "hevc") {
		return nil
	}
	if !r.DV.Present() {
		r.DV = t.dvInfo(ctx, r.Path)
	}
	dv := r.DV
	mp4 := r.Container == "mp4" || r.Container == "hls"
	// Tags only exist in MP4 (fragmented MP4 HLS); MPEG-TS segments have none.
	tagged := mp4 && !(r.Container == "hls" && r.Segments == "ts")

	if r.Video == "h264" {
		// Converting: libplacebo applies Dolby Vision itself; zscale only
		// sees the base layer, which profile 5 doesn't have.
		if dv.BaseTransfer() == "" && !caps.Placebo {
			return ErrDVNeedsPlacebo
		}
		return nil
	}

	switch {
	case dv.Profile == 5 || dv.BaseTransfer() == "":
		switch {
		case r.ClientDV == "1" && tagged:
			r.dvRoute = dvTag
		case r.ClientDV == "0":
			if !caps.Placebo {
				return ErrDVNeedsPlacebo
			}
			if caps.H264Encoder == "" {
				return errors.New("this ffmpeg has no H.264 encoder")
			}
			t.logInfo("dolby vision profile 5 to a player without it: converting", "file", filepath.Base(r.Path))
			r.Video = "h264"
		}
	case dv.Profile == 7:
		// The enhancement layer has to go for MP4 and HLS players, and for
		// any player that said it has no Dolby Vision.
		if mp4 || r.ClientDV == "0" {
			r.dvRoute = t.stripOrKeep(ctx, r.Path, caps)
		}
	default: // profile 8 (and 4, 9 and others with a base layer)
		switch {
		case r.ClientDV == "1" && tagged:
			r.dvRoute = dvTag
		case r.ClientDV == "0" && dv.BaseTransfer() == "pq":
			// HDR10 players mostly ignore the metadata, but some stumble on
			// it; the base layer alone is exactly HDR10.
			r.dvRoute = t.stripOrKeep(ctx, r.Path, caps)
		}
	}
	if r.dvRoute != dvKeep {
		t.logInfo("dolby vision route", "file", filepath.Base(r.Path), "profile", dv.Profile, "compat", dv.Compat,
			"route", r.dvRoute, "player_dv", r.ClientDV)
	}
	return nil
}

// stripOrKeep returns dvStrip when this ffmpeg can strip this file's Dolby
// Vision, else dvKeep (sent as before, with a warning in the log).
func (t *Transcoder) stripOrKeep(ctx context.Context, path string, caps ffmpeg.Caps) string {
	if !caps.DoviStrip {
		t.logWarn("dolby vision sent as is: this ffmpeg can't strip it (needs FFmpeg 7.1 or later)", "file", filepath.Base(path))
		return dvKeep
	}
	if !t.stripWorks(ctx, path) {
		t.logWarn("dolby vision sent as is: stripping failed on this file", "file", filepath.Base(path))
		return dvKeep
	}
	return dvStrip
}

// --- Per-file checks, remembered ------------------------------------------------------

type fileKey struct {
	path  string
	size  int64
	mtime int64
}

func keyFor(path string) (fileKey, bool) {
	st, err := os.Stat(path)
	if err != nil {
		return fileKey{}, false
	}
	return fileKey{path, st.Size(), st.ModTime().UnixNano()}, true
}

type dvCache struct {
	mu    sync.Mutex
	info  map[fileKey]probe.DolbyVision
	strip map[fileKey]bool
}

func (t *Transcoder) cache() *dvCache {
	t.dvOnce.Do(func() {
		t.dv = &dvCache{info: map[fileKey]probe.DolbyVision{}, strip: map[fileKey]bool{}}
	})
	return t.dv
}

// dvInfo reads a file's Dolby Vision configuration (the library keeps only
// the profile). A failed read counts as profile-only information.
func (t *Transcoder) dvInfo(ctx context.Context, path string) probe.DolbyVision {
	c := t.cache()
	k, ok := keyFor(path)
	if ok {
		c.mu.Lock()
		d, hit := c.info[k]
		c.mu.Unlock()
		if hit {
			return d
		}
	}
	d, err := probe.DV(ctx, t.FFprobe, path)
	if err != nil {
		t.logWarn("dolby vision probe failed", "file", filepath.Base(path), "err", err)
		return probe.DolbyVision{Compat: -1}
	}
	if ok {
		c.mu.Lock()
		c.info[k] = d
		c.mu.Unlock()
	}
	return d
}

// stripWorks strips the first two seconds of a file to nowhere: some files
// make the filters fail (and ffmpeg still exits 0, so its messages are
// what tells). A check that runs out of time counts as working.
func (t *Transcoder) stripWorks(ctx context.Context, path string) bool {
	c := t.cache()
	k, ok := keyFor(path)
	if ok {
		c.mu.Lock()
		res, hit := c.strip[k]
		c.mu.Unlock()
		if hit {
			return res
		}
	}
	tctx, cancel := context.WithTimeout(ctx, 6*time.Second)
	defer cancel()
	cmd := exec.CommandContext(tctx, t.Tool.Path(), "-hide_banner", "-nostats", "-nostdin", "-v", "error",
		"-i", path, "-t", "2", "-map", "0:v:0", "-c:v", "copy", "-bsf:v", ffmpeg.DoviStripBSF,
		"-an", "-sn", "-dn", "-f", "null", "-")
	ffmpeg.HideWindow(cmd)
	tail := &ffmpeg.Tail{N: 4096}
	cmd.Stderr = tail
	err := cmd.Run()
	if tctx.Err() != nil {
		return true // slow disk or a huge file: assume it works, don't remember
	}
	msg := strings.ToLower(tail.String())
	works := err == nil && !strings.Contains(msg, "error applying bitstream filters") &&
		!strings.Contains(msg, "failed to read access unit") && !strings.Contains(msg, "failed to read unit")
	if !works {
		t.logWarn("dolby vision strip check failed", "file", filepath.Base(path), "err", err, "ffmpeg", tail.String())
	}
	if ok {
		c.mu.Lock()
		c.strip[k] = works
		c.mu.Unlock()
	}
	return works
}

func (t *Transcoder) logInfo(msg string, args ...any) {
	if t.Log != nil {
		t.Log.Info(msg, args...)
	}
}

func (t *Transcoder) logWarn(msg string, args ...any) {
	if t.Log != nil {
		t.Log.Warn(msg, args...)
	}
}

// dvSummary describes a request's Dolby Vision handling for the log.
func dvSummary(r Request) string {
	if r.HDR != "dolby_vision" {
		return ""
	}
	route := r.dvRoute
	if route == "" {
		route = "as is"
	}
	return fmt.Sprintf("profile %d, %s", r.DV.Profile, route)
}
