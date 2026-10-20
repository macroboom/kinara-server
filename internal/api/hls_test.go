//go:build !nosqlite

package api

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"kinara/internal/auth"
	"kinara/internal/ffmpeg"
	"kinara/internal/images"
	"kinara/internal/probe"
	"kinara/internal/scanner"
	"kinara/internal/store"
	"kinara/internal/transcode"
	"kinara/internal/watch"
)

// An iPhone or Safari asks for HLS: the playlist and segments come without
// the token (the session ID is the key), and stopping ends it.
func TestHLSAPI(t *testing.T) {
	for _, bin := range []string{"ffmpeg", "ffprobe"} {
		if _, err := exec.LookPath(bin); err != nil {
			t.Skip(bin + " not installed")
		}
	}
	ctx := context.Background()
	dir := t.TempDir()
	clip := filepath.Join(dir, "Clip.mkv")
	makeClip(t, clip)
	st, err := store.Open(filepath.Join(dir, "kinara.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	lib, _ := st.CreateLibrary(ctx, "Movies", dir, store.KindMovies)
	it := store.Item{LibraryID: lib.ID, Kind: store.ItemMovie, Title: "Clip", SortTitle: "Clip", SeasonNumber: -1, EpisodeNumber: -1}
	st.CreateItem(ctx, &it)
	res, err := (&probe.Prober{}).Probe(ctx, clip)
	if err != nil {
		t.Fatal(err)
	}
	v := store.Version{LibraryID: lib.ID, ItemID: it.ID, Path: clip, RelPath: "Clip.mkv", Size: 1, VideoCodec: res.VideoCodec, DurationSec: res.DurationSec}
	if err := st.SaveVersion(ctx, &v, res.Streams); err != nil {
		t.Fatal(err)
	}
	hash, _ := auth.HashPassword("password123")
	st.CreateUser(ctx, "alex", hash, true)

	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	tr := &transcode.Transcoder{Tool: &ffmpeg.Tool{}, Log: log}
	hls := &transcode.HLS{T: tr, Dir: filepath.Join(dir, "hls")}
	defer hls.Close()
	srv := &Server{Store: st, Scanner: &scanner.Scanner{Store: st, Prober: &probe.Prober{}, Log: log},
		Images: images.New(filepath.Join(dir, "img")), Watch: &watch.Service{Store: st}, Log: log, Version: "test", BaseCtx: ctx,
		Transcoder: tr, HLS: hls}
	hs := httptest.NewServer(srv.Handler())
	defer hs.Close()
	c := &client{t: t, base: hs.URL + "/api/v1"}
	var login struct{ Token string }
	c.do("POST", "/auth/login", map[string]string{"username": "alex", "password": "password123"}, &login)
	c.token = login.Token

	anon := &client{t: t, base: hs.URL + "/api/v1"}
	if resp := anon.do("POST", "/hls/"+v.ID+"?video=copy&audioCodec=aac", nil, nil); resp.StatusCode != 401 {
		t.Fatalf("start without signing in: %d", resp.StatusCode)
	}
	var started struct {
		Session  string
		Playlist string
	}
	if resp := c.do("POST", "/hls/"+v.ID+"?video=copy&audioCodec=aac&start=1", nil, &started); resp.StatusCode != 200 {
		t.Fatalf("start: %d", resp.StatusCode)
	}
	if !strings.HasPrefix(started.Playlist, "/api/v1/hls/session/"+started.Session) {
		t.Fatalf("playlist url %q", started.Playlist)
	}
	resp, err := http.Get(hs.URL + started.Playlist)
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != 200 || !strings.Contains(string(body), "#EXT-X-START") || resp.Header.Get("Content-Type") != "application/vnd.apple.mpegurl" {
		t.Fatalf("playlist %d %q %s", resp.StatusCode, resp.Header.Get("Content-Type"), body)
	}
	if resp, _ := http.Get(hs.URL + "/api/v1/hls/session/" + started.Session + "/init.mp4"); resp.StatusCode != 200 {
		t.Fatalf("init segment: %d", resp.StatusCode)
	}
	if resp := c.do("DELETE", "/hls/session/"+started.Session, nil, nil); resp.StatusCode != 204 {
		t.Fatalf("stop: %d", resp.StatusCode)
	}
	if resp, _ := http.Get(hs.URL + started.Playlist); resp.StatusCode != 404 {
		t.Fatalf("after stop: %d", resp.StatusCode)
	}
	if resp := c.do("POST", "/hls/nope?video=copy&audioCodec=aac", nil, nil); resp.StatusCode != 404 {
		t.Fatalf("unknown file: %d", resp.StatusCode)
	}
	if resp := c.do("POST", "/hls/"+v.ID+"?video=vp9&audioCodec=aac", nil, nil); resp.StatusCode != 400 {
		t.Fatalf("bad video: %d", resp.StatusCode)
	}
}

// Text subtitles come out as WebVTT; picture ones are refused clearly.
func TestSubtitlesAPI(t *testing.T) {
	for _, bin := range []string{"ffmpeg", "ffprobe"} {
		if _, err := exec.LookPath(bin); err != nil {
			t.Skip(bin + " not installed")
		}
	}
	ctx := context.Background()
	dir := t.TempDir()
	srt := filepath.Join(dir, "s.srt")
	if err := writeFile(srt, "1\n00:00:00,500 --> 00:00:01,500\nFirst words\n"); err != nil {
		t.Fatal(err)
	}
	clip := filepath.Join(dir, "Sub.mkv")
	if out, err := exec.Command("ffmpeg", "-v", "error", "-f", "lavfi", "-i", "testsrc=size=160x90:rate=10:duration=2", "-i", srt,
		"-map", "0:v", "-map", "1:s", "-c:v", "libx264", "-c:s", "srt", clip).CombinedOutput(); err != nil {
		t.Skipf("can't make clip: %v %s", err, out)
	}
	st, err := store.Open(filepath.Join(dir, "kinara.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	lib, _ := st.CreateLibrary(ctx, "Movies", dir, store.KindMovies)
	res, err := (&probe.Prober{}).Probe(ctx, clip)
	if err != nil {
		t.Fatal(err)
	}
	v := store.Version{LibraryID: lib.ID, Path: clip, RelPath: "Sub.mkv", Size: 1, Mtime: 5}
	if err := st.SaveVersion(ctx, &v, res.Streams); err != nil {
		t.Fatal(err)
	}
	hash, _ := auth.HashPassword("password123")
	st.CreateUser(ctx, "alex", hash, true)
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	srv := &Server{Store: st, Scanner: &scanner.Scanner{Store: st, Prober: &probe.Prober{}, Log: log},
		Images: images.New(filepath.Join(dir, "img")), Watch: &watch.Service{Store: st}, Log: log, Version: "test", BaseCtx: ctx,
		Subtitles: &transcode.Subtitles{Tool: &ffmpeg.Tool{}, Dir: filepath.Join(dir, "subs")}}
	hs := httptest.NewServer(srv.Handler())
	defer hs.Close()
	c := &client{t: t, base: hs.URL + "/api/v1"}
	var login struct{ Token string }
	c.do("POST", "/auth/login", map[string]string{"username": "alex", "password": "password123"}, &login)

	resp, err := http.Get(hs.URL + "/api/v1/subtitles/" + v.ID + "/1.vtt?token=" + login.Token)
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != 200 || !strings.Contains(string(body), "First words") || !strings.HasPrefix(resp.Header.Get("Content-Type"), "text/vtt") {
		t.Fatalf("vtt %d %q %s", resp.StatusCode, resp.Header.Get("Content-Type"), body)
	}
	if resp, _ := http.Get(hs.URL + "/api/v1/subtitles/" + v.ID + "/0.vtt?token=" + login.Token); resp.StatusCode != 404 {
		t.Fatalf("video stream as subtitles: %d", resp.StatusCode)
	}
	if resp, _ := http.Get(hs.URL + "/api/v1/subtitles/" + v.ID + "/1.vtt"); resp.StatusCode != 401 {
		t.Fatalf("without token: %d", resp.StatusCode)
	}
}

func writeFile(path, text string) error { return os.WriteFile(path, []byte(text), 0o644) }
