package transcode

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"kinora/internal/ffmpeg"
)

func TestSubtitlesVTT(t *testing.T) {
	if _, err := exec.LookPath("ffmpeg"); err != nil {
		t.Skip("ffmpeg not installed")
	}
	dir := t.TempDir()
	srt := filepath.Join(dir, "s.srt")
	os.WriteFile(srt, []byte("1\n00:00:01,000 --> 00:00:02,500\nHello there\n\n2\n00:00:03,000 --> 00:00:04,000\nSecond line\n"), 0o644)
	src := filepath.Join(dir, "in.mkv")
	gen := exec.Command("ffmpeg", "-hide_banner", "-loglevel", "error",
		"-f", "lavfi", "-i", "testsrc2=size=160x90:rate=10:duration=5", "-i", srt,
		"-map", "0:v", "-map", "1:s", "-c:v", "libx264", "-c:s", "srt", src)
	if out, err := gen.CombinedOutput(); err != nil {
		t.Skipf("can't make clip: %v %s", err, out)
	}
	s := &Subtitles{Tool: &ffmpeg.Tool{}, Dir: filepath.Join(dir, "subs")}
	vtt, err := s.VTT(context.Background(), "v1", src, 100, 1, "subrip")
	if err != nil {
		t.Fatal(err)
	}
	text := string(vtt)
	if !strings.HasPrefix(text, "WEBVTT") || !strings.Contains(text, "Hello there") || !strings.Contains(text, "00:01.000 --> 00:02.500") {
		t.Fatalf("vtt = %q", text)
	}
	// Kept: a second ask doesn't need the file at all.
	os.Remove(src)
	again, err := s.VTT(context.Background(), "v1", src, 100, 1, "subrip")
	if err != nil || string(again) != text {
		t.Fatalf("cached = %v", err)
	}
	if _, err := s.VTT(context.Background(), "v1", src, 100, 2, "hdmv_pgs_subtitle"); err != ErrNotText {
		t.Fatalf("pictures: %v", err)
	}
}
