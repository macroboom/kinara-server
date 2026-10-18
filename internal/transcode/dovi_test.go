package transcode

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"kinora/internal/ffmpeg"
	"kinora/internal/probe"
)

// dvFile makes a stand-in file and marks it as one the strip check passed
// (or failed) for, so tests don't run ffmpeg.
func dvFile(t *testing.T, tr *Transcoder, stripOK bool) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "dv.mkv")
	if err := os.WriteFile(p, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	k, _ := keyFor(p)
	c := tr.cache()
	c.strip[k] = stripOK
	return p
}

func dvReq(path string, profile, compat int, container, segments, client string) Request {
	return Request{Path: path, Video: "copy", AudioCodec: "copy", Audio: -1, HDR: "dolby_vision", SourceCodec: "hevc",
		Container: container, OutDir: "/tmp/x", Segments: segments, ClientDV: client,
		DV: probe.DolbyVision{Profile: profile, BL: true, RPU: true, EL: profile == 7, Compat: compat}}
}

func TestDolbyVisionRoutes(t *testing.T) {
	caps := ffmpeg.Caps{Found: true, H264Encoder: "libx264", DoviStrip: true}
	tr := &Transcoder{Tool: &ffmpeg.Tool{}}
	path := dvFile(t, tr, true)
	cases := []struct {
		name                        string
		profile, compat             int
		container, segments, client string
		caps                        ffmpeg.Caps
		wantRoute, wantVideo        string
		wantErr                     bool
	}{
		{"p7 into hls is stripped", 7, 6, "hls", "fmp4", "", caps, dvStrip, "copy", false},
		{"p7 into mkv for an unknown player is kept", 7, 6, "mkv", "", "", caps, dvKeep, "copy", false},
		{"p7 to a player without dv is stripped", 7, 6, "mkv", "", "0", caps, dvStrip, "copy", false},
		{"p7 without the filters is kept", 7, 6, "hls", "fmp4", "", ffmpeg.Caps{Found: true, H264Encoder: "libx264"}, dvKeep, "copy", false},
		{"p8.1 to a dv player in hls is tagged", 8, 1, "hls", "fmp4", "1", caps, dvTag, "copy", false},
		{"p8.1 in ts segments is never tagged", 8, 1, "hls", "ts", "1", caps, dvKeep, "copy", false},
		{"p8.1 to a player without dv is stripped", 8, 1, "mp4", "", "0", caps, dvStrip, "copy", false},
		{"p8.4 (hlg) to a player without dv is kept", 8, 4, "mp4", "", "0", caps, dvKeep, "copy", false},
		{"p5 to a dv player is tagged", 5, 0, "hls", "fmp4", "1", caps, dvTag, "copy", false},
		{"p5 to an unknown player is kept", 5, 0, "hls", "fmp4", "", caps, dvKeep, "copy", false},
		{"p5 to a player without dv is converted with libplacebo", 5, 0, "hls", "fmp4", "0",
			ffmpeg.Caps{Found: true, H264Encoder: "libx264", Placebo: true}, dvKeep, "h264", false},
		{"p5 to a player without dv and no libplacebo fails", 5, 0, "hls", "fmp4", "0", caps, dvKeep, "copy", true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			r := dvReq(path, c.profile, c.compat, c.container, c.segments, c.client)
			err := tr.prepare(context.Background(), &r, c.caps)
			if (err != nil) != c.wantErr {
				t.Fatalf("err %v", err)
			}
			if err != nil {
				return
			}
			if r.dvRoute != c.wantRoute || r.Video != c.wantVideo {
				t.Errorf("route %q video %q, want %q %q", r.dvRoute, r.Video, c.wantRoute, c.wantVideo)
			}
		})
	}
}

func TestDolbyVisionStripFailureKeeps(t *testing.T) {
	tr := &Transcoder{Tool: &ffmpeg.Tool{}}
	path := dvFile(t, tr, false)
	r := dvReq(path, 7, 6, "hls", "fmp4", "")
	if err := tr.prepare(context.Background(), &r, ffmpeg.Caps{Found: true, DoviStrip: true}); err != nil {
		t.Fatal(err)
	}
	if r.dvRoute != dvKeep {
		t.Errorf("route %q", r.dvRoute)
	}
}

func TestDolbyVisionArgs(t *testing.T) {
	caps := ffmpeg.Caps{Found: true, H264Encoder: "libx264", DoviStrip: true}
	r := dvReq("in.mkv", 7, 6, "hls", "fmp4", "")
	r.OutDir = t.TempDir()
	if err := r.Validate(); err != nil {
		t.Fatal(err)
	}
	r.dvRoute = dvStrip
	a := Args(r, caps)
	if !has(a, "-bsf:v", ffmpeg.DoviStripBSF) || !has(a, "-tag:v", "hvc1") {
		t.Errorf("strip: %v", a)
	}
	r.dvRoute = dvTag
	a = Args(r, caps)
	if !has(a, "-tag:v", "dvh1", "-strict", "unofficial") || has(a, "-bsf:v", ffmpeg.DoviStripBSF) {
		t.Errorf("tag: %v", a)
	}
	r.Segments = "ts"
	r.dvRoute = dvStrip
	a = Args(r, caps)
	if !has(a, "-bsf:v", ffmpeg.DoviStripBSF) || indexOf(a, "-tag:v") >= 0 {
		t.Errorf("ts strip: %v", a)
	}
}

func TestToneMapDeclaresSourceAndCleansUp(t *testing.T) {
	caps := ffmpeg.Caps{Found: true, H264Encoder: "libx264", ToneMap: true, SetParams: true, SideData: ffmpeg.SideDataHDR10}
	r := Request{Path: "x.mkv", Video: "h264", AudioCodec: "copy", Audio: -1, HDR: "hlg"}
	if err := r.Validate(); err != nil {
		t.Fatal(err)
	}
	a := Args(r, caps)
	vf := videoFilterArg(a)
	if !strings.Contains(vf, "color_trc=arib-std-b67") || !strings.Contains(vf, "tonemap") || !strings.HasSuffix(vf, ffmpeg.SideDataHDR10) {
		t.Errorf("hlg filter %q", vf)
	}
	if !has(a, ffmpeg.SDROutputArgs...) {
		t.Errorf("no sdr tags: %v", a)
	}
	// Dolby Vision with an SDR base layer isn't tone mapped.
	r.HDR = "dolby_vision"
	r.DV = probe.DolbyVision{Profile: 8, Compat: 2, BL: true, RPU: true}
	a = Args(r, caps)
	vf = videoFilterArg(a)
	if strings.Contains(vf, "tonemap") || has(a, ffmpeg.SDROutputArgs...) {
		t.Errorf("sdr base: %q %v", vf, a)
	}
}

func TestAudioDownmixAndResample(t *testing.T) {
	r := Request{Path: "x.mkv", Video: "copy", AudioCodec: "aac", Audio: -1, AudioChannels: 6, MaxAudioChannels: 2}
	if err := r.Validate(); err != nil {
		t.Fatal(err)
	}
	a := Args(r, ffmpeg.Caps{Found: true})
	af := a[indexOf(a, "-af")+1]
	if !strings.Contains(af, "aformat=channel_layouts=stereo") || !strings.Contains(af, "alimiter") || indexOf(a, "-ac") >= 0 {
		t.Errorf("downmix %v", a)
	}
	r.MaxAudioChannels = 0
	a = Args(r, ffmpeg.Caps{Found: true})
	if !has(a, "-af", "aresample=async=1") {
		t.Errorf("resample %v", a)
	}
}
