//go:build !nosqlite

package api

import (
	"context"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"

	"kinara/internal/store"
	"kinara/internal/transcode"
)

func TestClampToCap(t *testing.T) {
	for _, c := range []struct {
		name               string
		in                 transcode.Request
		capKbps            int
		source             int64
		video              string
		maxBitrate, height int
	}{
		{"no cap", transcode.Request{Video: "copy"}, 0, 20_000_000, "copy", 0, 0},
		{"copy over the cap", transcode.Request{Video: "copy"}, 4000, 20_000_000, "h264", 4000, 720},
		{"copy under the cap", transcode.Request{Video: "copy"}, 4000, 3_000_000, "copy", 4000, 720},
		{"copy, bitrate unknown", transcode.Request{Video: "copy"}, 4000, 0, "h264", 4000, 720},
		{"asked for more", transcode.Request{Video: "h264", MaxBitrate: 12000, MaxHeight: 2160}, 8000, 20_000_000, "h264", 8000, 1080},
		{"asked for less", transcode.Request{Video: "h264", MaxBitrate: 1500, MaxHeight: 480}, 8000, 20_000_000, "h264", 1500, 480},
	} {
		req := c.in
		clampToCap(&req, c.capKbps, c.source)
		if req.Video != c.video || req.MaxBitrate != c.maxBitrate || req.MaxHeight != c.height {
			t.Errorf("%s: %s %d %d", c.name, req.Video, req.MaxBitrate, req.MaxHeight)
		}
	}
}

func TestThrottledWriter(t *testing.T) {
	rec := httptest.NewRecorder()
	// 400 KB/s after a 100 KB burst: 300 KB more takes about 0.75 s.
	tw := &throttledWriter{ResponseWriter: rec, ctx: context.Background(), perSec: 400 << 10, burst: 100 << 10, started: time.Now()}
	start := time.Now()
	n, err := tw.Write(make([]byte, 400<<10))
	took := time.Since(start)
	if err != nil || n != 400<<10 || rec.Body.Len() != 400<<10 {
		t.Fatalf("wrote %d of %d: %v", n, rec.Body.Len(), err)
	}
	if took < 600*time.Millisecond || took > 2*time.Second {
		t.Fatalf("took %v, want about 0.75 s", took)
	}
	// A player that goes away stops it at once.
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	tw = &throttledWriter{ResponseWriter: httptest.NewRecorder(), ctx: ctx, perSec: 1 << 10, started: time.Now()}
	if _, err := tw.Write(make([]byte, 200<<10)); err == nil {
		t.Fatal("kept writing after the player left")
	}
}

func TestWebPlaySettings(t *testing.T) {
	ctx := context.Background()
	st, err := store.Open(filepath.Join(t.TempDir(), "kinara.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	srv := &Server{Store: st}
	got, _ := st.GetSettings(ctx)
	if !got.WebRemotePlay || got.RelayMaxKbps != 4000 || !srv.WebPlay() {
		t.Fatalf("defaults: %+v", got)
	}
	got.WebRemotePlay, got.RelayMaxKbps = false, 0
	if err := st.PutSettings(ctx, got); err != nil {
		t.Fatal(err)
	}
	got, _ = st.GetSettings(ctx)
	if got.WebRemotePlay || got.RelayMaxKbps != 0 || srv.WebPlay() {
		t.Fatalf("after turning off: %+v", got)
	}
}
