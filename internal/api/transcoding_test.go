//go:build !nosqlite

package api

import (
	"context"
	"net/http/httptest"
	"path/filepath"
	"testing"

	"kinara/internal/ffmpeg"
	"kinara/internal/store"
	"kinara/internal/transcode"
)

func TestCheckTranscode(t *testing.T) {
	ok := store.TranscodeSettings{Encoder: "NVENC", MaxJobs: 4, HomeMaxHeight: 1080, AwayMaxKbps: 6000}
	if err := checkTranscode(&ok); err != nil || ok.Encoder != "nvenc" || ok.Speed != "balanced" || ok.ToneMap != "auto" {
		t.Fatalf("good settings: %+v %v", ok, err)
	}
	for name, bad := range map[string]store.TranscodeSettings{
		"encoder":  {Encoder: "cuda"},
		"device":   {Device: "/etc/passwd"},
		"speed":    {Speed: "ludicrous"},
		"jobs":     {MaxJobs: 99},
		"bitrate":  {AwayMaxKbps: 100},
		"height":   {HomeMaxHeight: 900},
		"channels": {AudioChannels: 6},
		"curve":    {ToneMapCurve: "aces"},
	} {
		if err := checkTranscode(&bad); err == nil {
			t.Errorf("%s accepted", name)
		}
	}
}

func TestQualityLimits(t *testing.T) {
	ctx := context.Background()
	st, err := store.Open(filepath.Join(t.TempDir(), "kinara.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	tr := &transcode.Transcoder{Tool: ffmpeg.WithCaps("ffmpeg", ffmpeg.Caps{Found: true, H264Encoder: "libx264"})}
	srv := &Server{Store: st, Transcoder: tr}
	set := store.DefaultTranscodeSettings
	set.DefaultKbps, set.HomeMaxHeight, set.AwayMaxKbps, set.AwayMaxHeight, set.MaxJobs = 10000, 2160, 3000, 720, 4
	st.PutTranscodeSettings(ctx, set)
	srv.ApplyTranscodeSettings(ctx)
	if tr.JobLimit() != 4 {
		t.Errorf("conversions at once: %d", tr.JobLimit())
	}

	film := store.Version{Bitrate: 20_000_000, Height: 2160}
	home := httptest.NewRequest("GET", "/api/v1/transcode/v1", nil)
	home.RemoteAddr = "192.168.1.20:5000"
	away := httptest.NewRequest("GET", "/api/v1/transcode/v1", nil)
	away.RemoteAddr = "203.0.113.7:5000"

	req := transcode.Request{Video: "h264"}
	srv.qualityLimits(home, &req, film)
	if req.MaxBitrate != 10000 || req.MaxHeight != 2160 {
		t.Errorf("home, nothing asked: %d %d", req.MaxBitrate, req.MaxHeight)
	}
	req = transcode.Request{Video: "copy"}
	srv.qualityLimits(away, &req, film)
	if req.Video != "h264" || req.MaxBitrate != 3000 || req.MaxHeight != 720 {
		t.Errorf("away: %s %d %d", req.Video, req.MaxBitrate, req.MaxHeight)
	}
	if got := srv.connectionLimit(away); got != 3000 {
		t.Errorf("away connection limit: %d", got)
	}
	if got := srv.connectionLimit(home); got != 0 {
		t.Errorf("home connection limit: %d", got)
	}
	// Changing the encoder checks again, using the new choice.
	set.Encoder = "none"
	srv.applyTranscode(set)
	if enc, _ := tr.Tool.Choice(); enc != "none" {
		t.Errorf("encoder choice: %q", enc)
	}
	set.Encoder = ""
	srv.applyTranscode(set)
	if enc, _ := tr.Tool.Choice(); enc != "auto" {
		t.Errorf("back to the environment's choice: %q", enc)
	}
}
