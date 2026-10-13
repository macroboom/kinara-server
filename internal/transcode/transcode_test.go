package transcode

import (
	"bytes"
	"context"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"kinora/internal/ffmpeg"
	"kinora/internal/store"
)

func has(args []string, seq ...string) bool {
	joined := "\x00" + strings.Join(args, "\x00") + "\x00"
	return strings.Contains(joined, "\x00"+strings.Join(seq, "\x00")+"\x00")
}

func TestArgsCopyVideoConvertAudio(t *testing.T) {
	r := Request{Path: "/m/a.mkv", Start: 61.5, Video: "copy", Audio: 2, AudioCodec: "aac", AudioChannels: 8,
		Subtitles: []store.Stream{{Index: 3, Codec: "subrip"}, {Index: 4, Codec: "mov_text"}, {Index: 5, Codec: "eia_608"}}}
	if err := r.Validate(); err != nil {
		t.Fatal(err)
	}
	a := Args(r, ffmpeg.Caps{Found: true, H264Encoder: "libx264"})
	for _, want := range [][]string{
		{"-ss", "61.500", "-i", "/m/a.mkv"},
		{"-map", "0:2"},
		{"-c:v", "copy"},
		{"-c:a", "aac", "-b:a", "384k"},
		{"-ac", "6"},
		{"-map", "0:3"}, {"-map", "0:4"},
		{"-c:s:0", "copy"}, {"-c:s:1", "srt"},
		{"-f", "matroska", "pipe:1"},
	} {
		if !has(a, want...) {
			t.Errorf("missing %v in %v", want, a)
		}
	}
	if has(a, "-map", "0:5") {
		t.Error("unsupported subtitle codec should be dropped")
	}
	if has(a, "-hwaccel", "auto") {
		t.Error("no GPU decode when copying video")
	}
}

func TestArgsEncodeHDR(t *testing.T) {
	r := Request{Path: "x.mkv", Video: "h264", MaxBitrate: 4000, MaxHeight: 720, HDR: "hdr10", Audio: -1, AudioCodec: "copy"}
	if err := r.Validate(); err != nil {
		t.Fatal(err)
	}
	a := Args(r, ffmpeg.Caps{Found: true, H264Encoder: "h264_nvenc", ToneMap: true})
	if !has(a, "-c:v", "h264_nvenc") || !has(a, "-b:v", "4000k") || !has(a, "-map", "0:a:0?") {
		t.Errorf("args %v", a)
	}
	vf := a[indexOf(a, "-vf")+1]
	if !strings.Contains(vf, "min(ih,720)") || !strings.Contains(vf, "tonemap") {
		t.Errorf("filter %q", vf)
	}
	a = Args(r, ffmpeg.Caps{Found: true, H264Encoder: "libx264"})
	vf = a[indexOf(a, "-vf")+1]
	if strings.Contains(vf, "tonemap") || !strings.Contains(vf, "format=yuv420p") {
		t.Errorf("without zscale: filter %q", vf)
	}
}

func TestValidate(t *testing.T) {
	r := Request{Video: "vp9", AudioCodec: "copy"}
	if r.Validate() == nil {
		t.Error("bad video accepted")
	}
	r = Request{Video: "copy", AudioCodec: "aac", MaxBitrate: 999999, MaxHeight: 50, Start: -3}
	if err := r.Validate(); err != nil {
		t.Fatal(err)
	}
	if r.MaxBitrate != 80000 || r.MaxHeight != 240 || r.Start != 0 {
		t.Errorf("clamped %+v", r)
	}
}

func indexOf(a []string, s string) int {
	for i, v := range a {
		if v == s {
			return i
		}
	}
	return -1
}

// TestStreamReal converts a generated clip with the real ffmpeg, when it's
// installed: AC-3 5.1 audio becomes AAC, and H.264 is re-encoded.
func TestStreamReal(t *testing.T) {
	if _, err := exec.LookPath("ffmpeg"); err != nil {
		t.Skip("ffmpeg not installed")
	}
	dir := t.TempDir()
	src := filepath.Join(dir, "in.mkv")
	gen := exec.Command("ffmpeg", "-hide_banner", "-loglevel", "error",
		"-f", "lavfi", "-i", "testsrc2=size=640x360:rate=24:duration=6",
		"-f", "lavfi", "-i", "sine=frequency=440:duration=6",
		"-filter_complex", "[1:a]pan=5.1|c0=c0|c1=c0|c2=c0|c3=c0|c4=c0|c5=c0[a]",
		"-map", "0:v", "-map", "[a]", "-c:v", "libx264", "-g", "24", "-c:a", "ac3", src)
	if out, err := gen.CombinedOutput(); err != nil {
		t.Skipf("can't make test clip: %v %s", err, out)
	}
	tr := &Transcoder{Tool: &ffmpeg.Tool{}, Log: slog.New(slog.NewTextHandler(io.Discard, nil))}
	for _, video := range []string{"copy", "h264"} {
		var buf bytes.Buffer
		err := tr.Stream(context.Background(), &buf, Request{Path: src, Start: 2, Video: video, MaxHeight: 240,
			Audio: 1, AudioCodec: "aac", AudioChannels: 6})
		if err != nil {
			t.Fatalf("%s: %v", video, err)
		}
		out := filepath.Join(dir, video+".mkv")
		if err := os.WriteFile(out, buf.Bytes(), 0o644); err != nil {
			t.Fatal(err)
		}
		probe, err := exec.Command("ffprobe", "-v", "error", "-show_entries", "stream=codec_name,height,channels",
			"-of", "csv=p=0", out).Output()
		if err != nil {
			t.Fatalf("%s: probe: %v", video, err)
		}
		got := string(probe)
		if !strings.Contains(got, "aac") || !strings.Contains(got, "h264") {
			t.Errorf("%s: streams %q", video, got)
		}
		if video == "h264" && !strings.Contains(got, "240") {
			t.Errorf("not scaled: %q", got)
		}
	}
}

func TestPickStart(t *testing.T) {
	keys := []float64{0, 10, 20, 30}
	for _, c := range []struct {
		t       float64
		forward bool
		key     float64
	}{{15, false, 10}, {15, true, 20}, {20, true, 20}, {20.02, false, 20}, {35, true, 30}, {5, false, 0}} {
		got := pickStart(keys, c.t, c.forward)
		if got.Keyframe != c.key {
			t.Errorf("pickStart(%v, %v) = %v, want %v", c.t, c.forward, got.Keyframe, c.key)
		}
		if got.Keyframe > 0 && (got.Request <= got.Keyframe || got.Request > got.Keyframe+0.25) {
			t.Errorf("request %v for keyframe %v", got.Request, got.Keyframe)
		}
	}
}

// TestStartAlignsReal checks the whole point: a stream started at the
// requested time begins with the keyframe's picture, so the player's time 0
// is the keyframe. It uses B-frames and MKV, where asking for the
// keyframe's exact time would start a whole keyframe interval early, and
// MPEG-TS, whose clock doesn't start at 0.
func TestStartAlignsReal(t *testing.T) {
	if _, err := exec.LookPath("ffprobe"); err != nil {
		t.Skip("ffprobe not installed")
	}
	dir := t.TempDir()
	for _, ext := range []string{"mkv", "ts", "mp4"} {
		src := filepath.Join(dir, "k."+ext)
		gen := exec.Command("ffmpeg", "-hide_banner", "-loglevel", "error",
			"-f", "lavfi", "-i", "testsrc2=size=320x180:rate=24:duration=40",
			"-f", "lavfi", "-i", "sine=duration=40",
			"-c:v", "libx264", "-bf", "3", "-g", "240", "-keyint_min", "240", "-sc_threshold", "0",
			"-c:a", "aac", src)
		if out, err := gen.CombinedOutput(); err != nil {
			t.Skipf("can't make clip: %v %s", err, out)
		}
		back, err := FindStart(context.Background(), "", src, 15, false)
		if err != nil || back.Keyframe < 9.8 || back.Keyframe > 10.2 {
			t.Fatalf("%s back: %+v %v", ext, back, err)
		}
		fwd, err := FindStart(context.Background(), "", src, 15, true)
		if err != nil || fwd.Keyframe < 19.8 || fwd.Keyframe > 20.2 {
			t.Fatalf("%s forward: %+v %v", ext, fwd, err)
		}
		// Convert from the requested time and compare where audio and video
		// begin: audio starts at the request, video at the keyframe, so the
		// gap is the nudge (not a whole 10 s keyframe interval).
		var buf bytes.Buffer
		tr := &Transcoder{Tool: &ffmpeg.Tool{}, Log: slog.New(slog.NewTextHandler(io.Discard, nil))}
		if err := tr.Stream(context.Background(), &buf, Request{Path: src, Start: fwd.Request, Video: "copy", Audio: -1, AudioCodec: "aac"}); err != nil {
			t.Fatal(err)
		}
		out := filepath.Join(dir, ext+"-out.mkv")
		os.WriteFile(out, buf.Bytes(), 0o644)
		probe, err := exec.Command("ffprobe", "-v", "error", "-show_entries", "stream=codec_type,start_time", "-of", "csv=p=0", out).Output()
		if err != nil {
			t.Fatal(err)
		}
		var v, a float64
		for _, line := range strings.Split(strings.TrimSpace(string(probe)), "\n") {
			parts := strings.Split(line, ",")
			if len(parts) < 2 {
				continue
			}
			x, _ := strconv.ParseFloat(parts[1], 64)
			if parts[0] == "video" {
				v = x
			} else {
				a = x
			}
		}
		if gap := a - v; gap < -0.1 || gap > 0.6 {
			t.Errorf("%s: audio starts %.3f s after video (want about %.2f): video began on the wrong keyframe", ext, gap, fwd.Request-fwd.Keyframe)
		}
	}
}
