//go:build !nosqlite

package scanner

import (
	"context"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"kinara/internal/probe"
	"kinara/internal/store"
)

// Subtitle files next to a video are found by the scan, kept up to date,
// and dropped when they go.
func TestScanFindsSidecarSubtitles(t *testing.T) {
	if _, err := exec.LookPath("ffmpeg"); err != nil {
		t.Skip("ffmpeg not installed")
	}
	if _, err := exec.LookPath("ffprobe"); err != nil {
		t.Skip("ffprobe not installed")
	}
	ctx := context.Background()
	dir := t.TempDir()
	video := filepath.Join(dir, "Clip.mp4")
	cmd := exec.Command("ffmpeg", "-loglevel", "error", "-f", "lavfi", "-i", "testsrc2=size=160x90:rate=10:duration=1",
		"-c:v", "libx264", "-pix_fmt", "yuv420p", video)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Skipf("can't make a test video: %v %s", err, out)
	}
	old := time.Now().Add(-time.Hour)
	os.Chtimes(video, old, old)
	srt := "1\n00:00:00,100 --> 00:00:00,900\nHello\n"
	os.WriteFile(filepath.Join(dir, "Clip.en.srt"), []byte(srt), 0o644)
	os.WriteFile(filepath.Join(dir, "Clip.es.forced.srt"), []byte(srt), 0o644)
	os.WriteFile(filepath.Join(dir, "Other.en.srt"), []byte(srt), 0o644)

	st, err := store.Open(filepath.Join(t.TempDir(), "db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	lib, _ := st.CreateLibrary(ctx, "Home videos", dir, store.KindOther)
	sc := &Scanner{Store: st, Prober: &probe.Prober{}, Log: slog.New(slog.NewTextHandler(io.Discard, nil)), Workers: 1, StableAge: time.Nanosecond}
	if _, err := sc.Run(ctx, lib.ID); err != nil {
		t.Fatal(err)
	}
	versions, _ := st.LibraryVersions(ctx, lib.ID)
	v, ok := versions[video]
	if !ok {
		t.Fatalf("video not scanned: %v", versions)
	}
	files, _ := st.SubtitleFiles(ctx, v.ID)
	if len(files) != 2 || files[0].Language != "eng" || files[1].Language != "spa" || !files[1].Forced {
		t.Fatalf("subtitles %+v", files)
	}

	os.Remove(filepath.Join(dir, "Clip.es.forced.srt"))
	if _, err := sc.Run(ctx, lib.ID); err != nil {
		t.Fatal(err)
	}
	files, _ = st.SubtitleFiles(ctx, v.ID)
	if len(files) != 1 || files[0].Language != "eng" {
		t.Fatalf("after removing one: %+v", files)
	}
}
