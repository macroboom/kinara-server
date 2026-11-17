package transcode

import (
	"context"
	"io"
	"log/slog"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"kinara/internal/ffmpeg"
)

func TestLosslessValidateAndArgs(t *testing.T) {
	for _, codec := range []string{"alac", "flac"} {
		r := Request{Path: "/m/a.mkv", Video: "copy", Audio: 1, AudioCodec: codec, AudioChannels: 8, Container: "hls", OutDir: "/tmp/x"}
		if err := r.Validate(); err != nil {
			t.Fatalf("%s: %v", codec, err)
		}
		a := Args(r, ffmpeg.Caps{})
		// ALAC has no 7.1: eight channels go out as FLAC.
		i := slices.Index(a, "-c:a")
		if i < 0 || a[i+1] != "flac" || slices.Contains(a, "-ac") || !slices.Contains(a, "experimental") {
			t.Fatalf("%s 7.1: audio args %v", codec, a)
		}
		ts := Request{Path: "/m/a.mkv", Video: "copy", AudioCodec: codec, Container: "hls", OutDir: "/tmp/x", Segments: "ts"}
		if ts.Validate() == nil {
			t.Fatalf("%s accepted in TS segments", codec)
		}
	}
	// 5.1 stays ALAC, 24-bit.
	r := Request{Path: "/m/a.mkv", Video: "copy", Audio: 1, AudioCodec: "alac", AudioChannels: 6, Container: "hls", OutDir: "/tmp/x"}
	if err := r.Validate(); err != nil {
		t.Fatal(err)
	}
	a := Args(r, ffmpeg.Caps{})
	if i := slices.Index(a, "-c:a"); i < 0 || a[i+1] != "alac" || !slices.Contains(a, "s32p") {
		t.Fatalf("alac 5.1: %v", a)
	}
	// A player that asks for fewer channels gets them.
	r = Request{Path: "/m/a.mkv", Video: "copy", Audio: 1, AudioCodec: "alac", AudioChannels: 8, MaxAudioChannels: 6, Container: "mkv"}
	if err := r.Validate(); err != nil {
		t.Fatal(err)
	}
	a = Args(r, ffmpeg.Caps{})
	if i := slices.Index(a, "-ac"); i < 0 || a[i+1] != "6" || a[slices.Index(a, "-c:a")+1] != "alac" {
		t.Fatalf("channel limit: %v", a)
	}
}

// A 7.1 source converted to ALAC HLS keeps all 8 channels.
func TestLosslessHLSReal(t *testing.T) {
	if _, err := exec.LookPath("ffprobe"); err != nil {
		t.Skip("ffprobe not installed")
	}
	dir := t.TempDir()
	src := filepath.Join(dir, "in.mkv")
	gen := exec.Command("ffmpeg", "-hide_banner", "-loglevel", "error",
		"-f", "lavfi", "-i", "testsrc2=size=320x180:rate=24:duration=6",
		"-f", "lavfi", "-i", "sine=duration=6,pan=7.1|c0=c0|c1=c0|c2=c0|c3=c0|c4=c0|c5=c0|c6=c0|c7=c0",
		"-c:v", "libx264", "-g", "48", "-c:a", "pcm_s24le", src)
	if out, err := gen.CombinedOutput(); err != nil {
		t.Skipf("can't make clip: %v %s", err, out)
	}
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	h := &HLS{T: &Transcoder{Tool: &ffmpeg.Tool{}, Log: log}, Dir: filepath.Join(dir, "hls"), Idle: 2 * time.Second}
	defer h.Close()
	for _, codec := range []string{"alac", "flac"} {
		id, err := h.Start(context.Background(), Request{Path: src, Video: "copy", Audio: -1, AudioCodec: codec, AudioChannels: 8})
		if err != nil {
			t.Fatalf("%s: %v", codec, err)
		}
		h.mu.Lock()
		s := h.sessions[id]
		h.mu.Unlock()
		deadline := time.Now().Add(30 * time.Second)
		for finished := false; !finished; {
			select {
			case <-s.done:
				finished = true
			case <-time.After(100 * time.Millisecond):
				h.File(id, "index.m3u8")
				if time.Now().After(deadline) {
					t.Fatalf("%s: conversion didn't finish", codec)
				}
			}
		}
		out, err := exec.Command("ffprobe", "-v", "error", "-select_streams", "a", "-show_entries", "stream=codec_name,channels",
			"-of", "csv=p=0", filepath.Join(s.dir, "index.m3u8")).Output()
		// Both come out as 8-channel FLAC (ALAC has no 7.1).
		if err != nil || strings.Fields(string(out))[0] != "flac,8" {
			t.Fatalf("%s: ffprobe %q %v", codec, out, err)
		}
	}
}
