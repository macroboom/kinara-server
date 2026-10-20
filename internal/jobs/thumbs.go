package jobs

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"

	"kinara/internal/ffmpeg"
	"kinara/internal/store"
)

// ThumbTime picks the moment to grab a picture from: a tenth of the way in
// (past fades from black and title cards), at least a second, and never
// past the end of a very short clip.
func ThumbTime(duration float64) float64 {
	if duration <= 0 {
		return 1
	}
	t := duration * 0.1
	if t < 1 {
		t = 1
	}
	if t > duration-0.5 {
		t = duration / 2
	}
	return t
}

// ThumbArgs is the ffmpeg command for one frame of path at t, 1280 wide,
// converted to SDR first when the file is HDR (and this ffmpeg can).
func ThumbArgs(path string, t float64, hdr bool, caps ffmpeg.Caps, out string) []string {
	vf := "scale=1280:-2"
	if tm := caps.HDRToSDR(); hdr && tm != "" {
		vf += "," + tm
	} else {
		vf += ",format=yuvj420p"
	}
	return []string{"-nostdin", "-hide_banner", "-loglevel", "error", "-y",
		"-ss", strconv.FormatFloat(t, 'f', 2, 64), "-i", path,
		"-map", "0:v:0", "-frames:v", "1", "-vf", vf, "-q:v", "3", out}
}

// makeThumb grabs a frame for an "Other videos" title, or a movie or show
// that didn't match, and records it as the title's backdrop and poster.
func (w *Worker) makeThumb(ctx context.Context, t store.ThumbTarget) {
	task := w.begin("picture", "picture", t.Path)
	caps := w.Tool.Caps()
	out := filepath.Join(w.ThumbDir, t.VersionID+".jpg")
	err := func() error {
		if !caps.Found {
			return fmt.Errorf("ffmpeg not found")
		}
		if err := os.MkdirAll(w.ThumbDir, 0o755); err != nil {
			return err
		}
		tmp := out + ".tmp.jpg"
		cmd := exec.CommandContext(ctx, w.Tool.Path(), ThumbArgs(t.Path, ThumbTime(t.DurationSec), t.HDR != "", caps, tmp)...)
		tail := &ffmpeg.Tail{N: 1024}
		cmd.Stderr = tail
		if err := ffmpeg.StartLow(cmd); err != nil {
			return err
		}
		if err := cmd.Wait(); err != nil {
			os.Remove(tmp)
			return fmt.Errorf("ffmpeg: %v %s", err, tail.String())
		}
		if fi, err := os.Stat(tmp); err != nil || fi.Size() == 0 {
			os.Remove(tmp)
			return fmt.Errorf("ffmpeg made no picture")
		}
		return os.Rename(tmp, out)
	}()
	if ctx.Err() != nil {
		return
	}
	if err != nil {
		w.Log.Warn("picture failed", "file", t.Path, "err", err)
		w.markFailed("thumb:" + t.VersionID)
		w.finish(task, true, shortErr(err))
		return
	}
	abs, _ := filepath.Abs(out)
	for _, kind := range []string{"backdrop", "poster"} {
		if err := w.Store.SetImage(ctx, t.ItemID, kind, "file:"+abs); err != nil {
			w.Log.Warn("picture save", "file", t.Path, "err", err)
			w.markFailed("thumb:" + t.VersionID)
			w.finish(task, true, shortErr(err))
			return
		}
	}
	w.mu.Lock()
	w.status.PicturesMade++
	w.mu.Unlock()
	w.finish(task, false, "cover made from a frame")
}

func (w *Worker) markFailed(key string) {
	w.mu.Lock()
	if w.failed == nil {
		w.failed = map[string]bool{}
	}
	w.failed[key] = true
	w.mu.Unlock()
}

// nextThumb finds a file still needing a picture, skipping
// ones that failed since the server started.
func (w *Worker) nextThumb(ctx context.Context) (store.ThumbTarget, bool) {
	list, err := w.Store.VideosNeedingThumbs(ctx, 50)
	if err != nil {
		w.Log.Warn("picture queue", "err", err)
		return store.ThumbTarget{}, false
	}
	for _, t := range list {
		if w.hasFailed("thumb:" + t.VersionID) {
			continue
		}
		if _, err := os.Stat(t.Path); err != nil {
			continue
		}
		return t, true
	}
	return store.ThumbTarget{}, false
}
