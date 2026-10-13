package trickplay

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"kinora/internal/ffmpeg"
	"kinora/internal/store"
)

func TestMakeReal(t *testing.T) {
	if _, err := exec.LookPath("ffmpeg"); err != nil {
		t.Skip("ffmpeg not installed")
	}
	dir := t.TempDir()
	src := filepath.Join(dir, "in.mkv")
	// 125 s at 1 fps keyframes every 2 s: 13 previews on one sheet.
	gen := exec.Command("ffmpeg", "-hide_banner", "-loglevel", "error",
		"-f", "lavfi", "-i", "testsrc2=size=1280x720:rate=4:duration=125",
		"-c:v", "libx264", "-preset", "ultrafast", "-g", "8", src)
	if out, err := gen.CombinedOutput(); err != nil {
		t.Skipf("can't make test clip: %v %s", err, out)
	}
	fi, _ := os.Stat(src)
	v := store.Version{ID: "abc", Path: src, DurationSec: 125, Mtime: fi.ModTime().Unix()}
	m := &Maker{Tool: &ffmpeg.Tool{}, Dir: filepath.Join(dir, "tp")}
	if !m.Needed(v) {
		t.Fatal("should be needed before making")
	}
	man, err := m.Make(context.Background(), v)
	if err != nil {
		t.Fatal(err)
	}
	if man.Width != Width || man.Height < 134 || man.Height > 136 {
		t.Errorf("size %dx%d", man.Width, man.Height)
	}
	if man.Count != 13 || man.Sheets != 1 {
		t.Errorf("count %d sheets %d", man.Count, man.Sheets)
	}
	if _, ok := m.Ready(v); !ok || m.Needed(v) {
		t.Error("should be ready")
	}
	if _, err := os.Stat(m.Sheet("abc", 1)); err != nil {
		t.Error(err)
	}
	v.Mtime++
	if !m.Needed(v) {
		t.Error("a changed file needs new previews")
	}
}

func TestFailureRecorded(t *testing.T) {
	if _, err := exec.LookPath("ffmpeg"); err != nil {
		t.Skip("ffmpeg not installed")
	}
	dir := t.TempDir()
	bad := filepath.Join(dir, "bad.mkv")
	os.WriteFile(bad, []byte("not a video"), 0o644)
	v := store.Version{ID: "bad", Path: bad, DurationSec: 60, Mtime: 5}
	m := &Maker{Tool: &ffmpeg.Tool{}, Dir: filepath.Join(dir, "tp")}
	if _, err := m.Make(context.Background(), v); err == nil {
		t.Fatal("expected an error")
	}
	if m.Needed(v) {
		t.Error("a failure should not be retried straight away")
	}
	if _, ok := m.Ready(v); ok {
		t.Error("failed previews are not ready")
	}
}
