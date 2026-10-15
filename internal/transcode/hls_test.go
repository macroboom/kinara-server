package transcode

import (
	"bytes"
	"context"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"kinora/internal/ffmpeg"
)

func TestStartAtBeginning(t *testing.T) {
	in := []byte("#EXTM3U\n#EXT-X-VERSION:7\n#EXTINF:4.0,\nseg00000.m4s\n")
	out := string(StartAtBeginning(in))
	if !strings.HasPrefix(out, "#EXTM3U\n#EXT-X-START:TIME-OFFSET=0,PRECISE=YES\n#EXT-X-VERSION:7") {
		t.Errorf("playlist = %q", out)
	}
	if again := StartAtBeginning([]byte(out)); string(again) != out {
		t.Error("tag added twice")
	}
	if got := StartAtBeginning([]byte("junk")); string(got) != "junk" {
		t.Error("non-playlist changed")
	}
}

func TestHLSSession(t *testing.T) {
	if _, err := exec.LookPath("ffprobe"); err != nil {
		t.Skip("ffprobe not installed")
	}
	dir := t.TempDir()
	src := filepath.Join(dir, "in.mkv")
	gen := exec.Command("ffmpeg", "-hide_banner", "-loglevel", "error",
		"-f", "lavfi", "-i", "testsrc2=size=320x180:rate=24:duration=12", "-f", "lavfi", "-i", "sine=duration=12",
		"-c:v", "libx264", "-g", "48", "-c:a", "ac3", src)
	if out, err := gen.CombinedOutput(); err != nil {
		t.Skipf("can't make clip: %v %s", err, out)
	}
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	h := &HLS{T: &Transcoder{Tool: &ffmpeg.Tool{}, Log: log}, Dir: filepath.Join(dir, "hls"), Idle: 400 * time.Millisecond}
	defer h.Close()

	id, err := h.Start(context.Background(), Request{Path: src, Video: "copy", Audio: -1, AudioCodec: "aac"})
	if err != nil {
		t.Fatal(err)
	}
	_, playlist, err := h.File(id, "index.m3u8")
	if err != nil || !bytes.Contains(playlist, []byte("#EXT-X-START")) || !bytes.Contains(playlist, []byte("#EXTINF")) {
		t.Fatalf("playlist %q %v", playlist, err)
	}
	if p, _, err := h.File(id, "init.mp4"); err != nil {
		t.Fatal(err)
	} else if _, err := os.Stat(p); err != nil {
		t.Fatalf("init segment: %v", err)
	}
	for _, bad := range [][2]string{{id, "../../etc/passwd"}, {id, "seg1.m4s"}, {"nothex", "index.m3u8"}, {strings.Repeat("0", 32), "index.m3u8"}} {
		if _, _, err := h.File(bad[0], bad[1]); err == nil {
			t.Errorf("File(%q, %q) allowed", bad[0], bad[1])
		}
	}

	// Let it finish, then the whole thing must be readable as HLS.
	h.mu.Lock()
	s := h.sessions[id]
	h.mu.Unlock()
	// (A player keeps asking for the playlist, which keeps it alive.)
	deadline := time.Now().Add(30 * time.Second)
	for finished := false; !finished; {
		select {
		case <-s.done:
			finished = true
		case <-time.After(100 * time.Millisecond):
			h.File(id, "index.m3u8")
			if time.Now().After(deadline) {
				t.Fatal("conversion didn't finish")
			}
		}
	}
	out, err := exec.Command("ffprobe", "-v", "error", "-show_entries", "stream=codec_name", "-of", "csv=p=0",
		filepath.Join(s.dir, "index.m3u8")).Output()
	if err != nil || !strings.Contains(string(out), "h264") || !strings.Contains(string(out), "aac") {
		t.Fatalf("ffprobe: %q %v", out, err)
	}

	// Nobody asks for it: it's ended and its files are gone.
	time.Sleep(1500 * time.Millisecond)
	if _, _, err := h.File(id, "index.m3u8"); err == nil {
		t.Fatal("idle session still alive")
	}
	if _, err := os.Stat(s.dir); !os.IsNotExist(err) {
		t.Fatalf("session folder left behind: %v", err)
	}
}

func TestHLSBadFile(t *testing.T) {
	if _, err := exec.LookPath("ffmpeg"); err != nil {
		t.Skip("ffmpeg not installed")
	}
	dir := t.TempDir()
	bad := filepath.Join(dir, "bad.mkv")
	os.WriteFile(bad, []byte("not a video"), 0o644)
	h := &HLS{T: &Transcoder{Tool: &ffmpeg.Tool{}, Log: slog.New(slog.NewTextHandler(io.Discard, nil))}, Dir: filepath.Join(dir, "hls")}
	defer h.Close()
	if _, err := h.Start(context.Background(), Request{Path: bad, Video: "copy", Audio: -1, AudioCodec: "aac"}); err == nil {
		t.Fatal("a broken file must fail")
	}
	if entries, _ := os.ReadDir(filepath.Join(dir, "hls")); len(entries) != 0 {
		t.Fatalf("left %d folders", len(entries))
	}
	// Its slot was given back: another start isn't refused as busy.
	if len(h.T.slots()) != 0 {
		t.Fatalf("slots in use: %d", len(h.T.slots()))
	}
}

// Roku players ask for MPEG-TS segments, with audio cut to stereo.
func TestHLSSessionTS(t *testing.T) {
	if _, err := exec.LookPath("ffprobe"); err != nil {
		t.Skip("ffprobe not installed")
	}
	dir := t.TempDir()
	src := filepath.Join(dir, "in.mkv")
	gen := exec.Command("ffmpeg", "-hide_banner", "-loglevel", "error",
		"-f", "lavfi", "-i", "testsrc2=size=320x180:rate=24:duration=6", "-f", "lavfi", "-i", "sine=duration=6",
		"-c:v", "libx264", "-g", "48", "-c:a", "ac3", "-ac", "6", src)
	if out, err := gen.CombinedOutput(); err != nil {
		t.Skipf("can't make clip: %v %s", err, out)
	}
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	h := &HLS{T: &Transcoder{Tool: &ffmpeg.Tool{}, Log: log}, Dir: filepath.Join(dir, "hls")}
	defer h.Close()

	id, err := h.Start(context.Background(), Request{Path: src, Video: "copy", Audio: -1, AudioCodec: "aac",
		AudioChannels: 6, MaxAudioChannels: 2, Segments: "ts"})
	if err != nil {
		t.Fatal(err)
	}
	_, playlist, err := h.File(id, "index.m3u8")
	if err != nil || !bytes.Contains(playlist, []byte("seg00000.ts")) || bytes.Contains(playlist, []byte("EXT-X-MAP")) {
		t.Fatalf("playlist %q %v", playlist, err)
	}
	p, _, err := h.File(id, "seg00000.ts")
	if err != nil {
		t.Fatal(err)
	}
	out, err := exec.Command("ffprobe", "-v", "error", "-show_entries", "format=format_name:stream=codec_name,channels", "-of", "csv=p=0", p).Output()
	if err != nil {
		t.Fatal(err)
	}
	got := string(out)
	if !strings.Contains(got, "mpegts") || !strings.Contains(got, "aac,2") {
		t.Errorf("segment = %q, want MPEG-TS with stereo AAC", got)
	}
}

func TestSegmentsValidate(t *testing.T) {
	r := Request{Video: "copy", AudioCodec: "aac", Container: "hls", OutDir: "x", Segments: "webm"}
	if r.Validate() == nil {
		t.Error("unknown segment kind allowed")
	}
}
