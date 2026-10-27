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

	"kinara/internal/ffmpeg"
)

func TestDownloadPresets(t *testing.T) {
	medium, ok := FindPreset("medium")
	if !ok || medium.MaxHeight != 720 {
		t.Fatalf("medium %+v", medium)
	}
	if _, ok := FindPreset("huge"); ok {
		t.Fatal("unknown preset found")
	}
	cases := []struct {
		codec, hdr string
		height     int
		bps        int64
		want       bool
	}{
		{"h264", "", 720, 3_000_000, true},
		{"h264", "", 1080, 3_000_000, false}, // too tall
		{"h264", "", 720, 9_000_000, false},  // too busy
		{"hevc", "", 720, 2_000_000, false},  // not every phone plays it
		{"h264", "hdr10", 720, 2_000_000, false},
		{"h264", "", 720, 0, false}, // unknown rate
	}
	for _, c := range cases {
		if got := medium.CanCopyVideo(c.codec, c.hdr, c.height, c.bps); got != c.want {
			t.Errorf("%+v: %v", c, got)
		}
	}
	orig, _ := FindPreset("original")
	if !orig.CanCopyVideo("hevc", "dolby_vision", 2160, 60_000_000) || orig.Estimate(7200, 40<<30, true) != 40<<30 {
		t.Fatal("original keeps everything")
	}
	// Two hours at medium: (4000*0.8 + 192) kbit/s.
	if got := medium.Estimate(7200, 40<<30, false); got != int64((3200+192)*1000/8*7200) {
		t.Fatalf("estimate %d", got)
	}
}

func TestArgsFile(t *testing.T) {
	r := Request{Path: "/in.mkv", Video: "h264", AudioCodec: "aac", Audio: -1, Container: "file", OutFile: "/out.mp4.part",
		MaxHeight: 720, MaxBitrate: 4000, MaxAudioChannels: 2, AudioChannels: 6}
	if err := r.Validate(); err != nil {
		t.Fatal(err)
	}
	a := strings.Join(Args(r, ffmpeg.Caps{Found: true, H264Encoder: "libx264"}), " ")
	for _, want := range []string{"-movflags +faststart -f mp4 -y /out.mp4.part", "-c:a aac", "aformat=channel_layouts=stereo"} {
		if !strings.Contains(a, want) {
			t.Errorf("missing %q in %s", want, a)
		}
	}
	if strings.Contains(a, "pipe:1") || strings.Contains(a, "-flush_packets") {
		t.Errorf("file args %s", a)
	}
	if err := (&Request{Video: "copy", AudioCodec: "copy", Container: "file"}).Validate(); err == nil {
		t.Fatal("file without a path accepted")
	}
}

func TestAcquireSpare(t *testing.T) {
	tr := &Transcoder{Tool: &ffmpeg.Tool{}, MaxJobs: 2, Log: slog.New(slog.NewTextHandler(io.Discard, nil))}
	viewer, err := tr.acquire(context.Background(), false)
	if err != nil {
		t.Fatal(err)
	}
	// One of two taken: a download would leave none for the next viewer.
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	if _, err := tr.acquireSpare(ctx); err == nil {
		t.Fatal("download took the last slot")
	}
	viewer()
	rel, err := tr.acquireSpare(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	rel()
}

func TestConvertReal(t *testing.T) {
	if _, err := exec.LookPath("ffmpeg"); err != nil {
		t.Skip("ffmpeg not installed")
	}
	dir := t.TempDir()
	src := filepath.Join(dir, "in.mkv")
	gen := exec.Command("ffmpeg", "-hide_banner", "-loglevel", "error",
		"-f", "lavfi", "-i", "testsrc2=size=640x360:rate=24:duration=12",
		"-f", "lavfi", "-i", "sine=frequency=440:duration=12",
		"-filter_complex", "[1:a]pan=5.1|c0=c0|c1=c0|c2=c0|c3=c0|c4=c0|c5=c0[a]",
		"-map", "0:v", "-map", "[a]", "-c:v", "libx264", "-g", "24", "-c:a", "ac3", src)
	if out, err := gen.CombinedOutput(); err != nil {
		t.Skipf("can't make test clip: %v %s", err, out)
	}
	tr := &Transcoder{Tool: &ffmpeg.Tool{}, Log: slog.New(slog.NewTextHandler(io.Discard, nil))}
	for _, video := range []string{"h264", "copy"} {
		out := filepath.Join(dir, video+".mp4")
		var last float64
		err := tr.Convert(context.Background(), Request{Path: src, Video: video, MaxHeight: 240, MaxBitrate: 800,
			Audio: -1, AudioCodec: "aac", AudioChannels: 6, MaxAudioChannels: 2, SourceCodec: "h264"}, out,
			func(sec float64) { last = sec })
		if err != nil {
			t.Fatalf("%s: %v", video, err)
		}
		if _, err := os.Stat(out + ".part"); err == nil {
			t.Errorf("%s: part file left", video)
		}
		probe, err := exec.Command("ffprobe", "-v", "error", "-show_entries", "stream=codec_name,height,channels:format=format_name,duration",
			"-of", "csv=p=0", out).Output()
		if err != nil {
			t.Fatalf("%s: probe: %v", video, err)
		}
		got := string(probe)
		if !strings.Contains(got, "h264") || !strings.Contains(got, "aac,2") || !strings.Contains(got, "mp4") {
			t.Errorf("%s: streams %q", video, got)
		}
		if video == "h264" && !strings.Contains(got, "240") {
			t.Errorf("not scaled: %q", got)
		}
		// faststart: the index comes before the pictures.
		b, _ := os.ReadFile(out)
		if m, d := bytes.Index(b, []byte("moov")), bytes.Index(b, []byte("mdat")); m < 0 || d < 0 || m > d {
			t.Errorf("%s: moov at %d, mdat at %d", video, m, d)
		}
		_ = last // ffmpeg reports every 5 s; a 12 s clip may finish first
	}
	// A cancelled conversion leaves nothing behind.
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	out := filepath.Join(dir, "cancelled.mp4")
	if err := tr.Convert(ctx, Request{Path: src, Video: "h264", Audio: -1, AudioCodec: "aac"}, out, nil); err == nil {
		t.Fatal("cancelled conversion succeeded")
	}
	if _, err := os.Stat(out); err == nil {
		t.Fatal("cancelled conversion left a file")
	}
}
