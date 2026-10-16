package probe

import (
	"context"
	"os/exec"
	"path/filepath"
	"testing"
)

const sampleDV = `{
  "streams": [
    {"index": 0, "codec_type": "video", "codec_name": "hevc", "profile": "Main 10",
     "width": 3840, "height": 2160, "color_transfer": "smpte2084",
     "disposition": {"default": 1, "forced": 0},
     "side_data_list": [{"side_data_type": "DOVI configuration record", "dv_profile": 7}]},
    {"index": 1, "codec_type": "audio", "codec_name": "truehd", "channels": 8,
     "tags": {"language": "eng", "title": "TrueHD Atmos 7.1"}, "disposition": {"default": 1}},
    {"index": 2, "codec_type": "audio", "codec_name": "ac3", "channels": 6,
     "tags": {"language": "eng"}, "disposition": {"default": 0}},
    {"index": 3, "codec_type": "subtitle", "codec_name": "hdmv_pgs_subtitle",
     "tags": {"language": "eng"}, "disposition": {"default": 0, "forced": 1}},
    {"index": 4, "codec_type": "video", "codec_name": "mjpeg", "disposition": {"attached_pic": 1}},
    {"index": 5, "codec_type": "attachment", "codec_name": "ttf"}
  ],
  "format": {"format_name": "matroska,webm", "duration": "7442.115000", "bit_rate": "58321456"}
}`

func TestParseDolbyVision(t *testing.T) {
	r, err := Parse([]byte(sampleDV), ".mkv")
	if err != nil {
		t.Fatal(err)
	}
	if r.Container != "mkv" || r.VideoCodec != "hevc" || r.Width != 3840 || r.Height != 2160 {
		t.Errorf("basic fields: %+v", r)
	}
	if r.HDR != "dolby_vision" || r.DVProfile != 7 {
		t.Errorf("hdr = %q profile %d", r.HDR, r.DVProfile)
	}
	if r.DurationSec < 7442 || r.DurationSec > 7443 || r.Bitrate != 58321456 {
		t.Errorf("duration %v bitrate %d", r.DurationSec, r.Bitrate)
	}
	if len(r.Streams) != 4 {
		t.Fatalf("want 4 streams (cover art and attachment skipped), got %d", len(r.Streams))
	}
	if s := r.Streams[1]; s.Codec != "truehd" || s.Channels != 8 || s.Language != "eng" || !s.Default {
		t.Errorf("audio stream: %+v", s)
	}
	if s := r.Streams[3]; s.Type != "subtitle" || !s.Forced {
		t.Errorf("subtitle stream: %+v", s)
	}
}

func TestContainerName(t *testing.T) {
	for _, c := range []struct{ format, ext, want string }{
		{"matroska,webm", ".mkv", "mkv"},
		{"matroska,webm", ".webm", "webm"},
		{"mov,mp4,m4a,3gp,3g2,mj2", ".mp4", "mp4"},
		{"mov,mp4,m4a,3gp,3g2,mj2", ".m4v", "m4v"},
		{"mpegts", ".m2ts", "ts"},
		{"avi", ".avi", "avi"},
	} {
		if got := containerName(c.format, c.ext); got != c.want {
			t.Errorf("containerName(%q, %q) = %q, want %q", c.format, c.ext, got, c.want)
		}
	}
}

func TestParseNoVideo(t *testing.T) {
	_, err := Parse([]byte(`{"streams":[{"index":0,"codec_type":"audio","codec_name":"aac"}],"format":{"format_name":"mp4"}}`), ".mp4")
	if err == nil {
		t.Error("want error for audio-only file")
	}
}

// TestProbeRealFile runs ffprobe on a generated clip when ffmpeg is installed.
func TestProbeRealFile(t *testing.T) {
	if _, err := exec.LookPath("ffmpeg"); err != nil {
		t.Skip("ffmpeg not installed")
	}
	if _, err := exec.LookPath("ffprobe"); err != nil {
		t.Skip("ffprobe not installed")
	}
	path := filepath.Join(t.TempDir(), "clip.mkv")
	cmd := exec.Command("ffmpeg", "-v", "error", "-f", "lavfi", "-i", "testsrc=size=320x180:rate=24:duration=2",
		"-f", "lavfi", "-i", "sine=frequency=440:duration=2",
		"-c:v", "libx264", "-c:a", "aac", "-metadata:s:a:0", "language=eng", path)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Skipf("ffmpeg could not make a clip: %v %s", err, out)
	}
	p := &Prober{}
	r, err := p.Probe(context.Background(), path)
	if err != nil {
		t.Fatal(err)
	}
	if r.Container != "mkv" || r.VideoCodec != "h264" || r.Width != 320 || r.Height != 180 {
		t.Errorf("got %+v", r)
	}
	if r.DurationSec < 1.5 || r.DurationSec > 2.5 {
		t.Errorf("duration %v", r.DurationSec)
	}
	if len(r.Streams) != 2 || r.Streams[1].Language != "eng" {
		t.Errorf("streams %+v", r.Streams)
	}
}

func TestParseDolbyVisionConfig(t *testing.T) {
	out := []byte(`{"streams":[{"side_data_list":[{"side_data_type":"DOVI configuration record","dv_version_major":1,
		"dv_profile":5,"dv_level":6,"rpu_present_flag":1,"el_present_flag":0,"bl_present_flag":1,"dv_bl_signal_compatibility_id":0}]}]}`)
	d, err := parseDV(out)
	if err != nil {
		t.Fatal(err)
	}
	if d.Profile != 5 || d.Level != 6 || !d.RPU || d.EL || !d.BL || d.Compat != 0 || d.BaseTransfer() != "" {
		t.Errorf("%+v", d)
	}
	// A missing compatibility id is "not stated", not 0.
	d, _ = parseDV([]byte(`{"streams":[{"side_data_list":[{"side_data_type":"DOVI configuration record","dv_profile":8}]}]}`))
	if d.Compat != -1 || d.BaseTransfer() != "pq" {
		t.Errorf("%+v", d)
	}
	d, _ = parseDV([]byte(`{"streams":[{"side_data_list":[{"side_data_type":"DOVI configuration record","dv_profile":8,"dv_bl_signal_compatibility_id":4}]}]}`))
	if d.BaseTransfer() != "hlg" {
		t.Errorf("%+v", d)
	}
}
