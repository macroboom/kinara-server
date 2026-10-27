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
	"time"

	"kinara/internal/auth"
	"kinara/internal/ffmpeg"
	"kinara/internal/images"
	"kinara/internal/store"
	"kinara/internal/transcode"
	"kinara/internal/watch"
)

func TestOptimizedVersions(t *testing.T) {
	if _, err := exec.LookPath("ffmpeg"); err != nil {
		t.Skip("ffmpeg not installed")
	}
	ctx, stop := context.WithCancel(context.Background())
	defer stop()
	data := t.TempDir()
	src := filepath.Join(data, "Heat.mkv")
	srt := filepath.Join(data, "in.srt")
	os.WriteFile(srt, []byte("1\n00:00:01,000 --> 00:00:03,000\nHello\n"), 0o644)
	gen := exec.Command("ffmpeg", "-hide_banner", "-loglevel", "error",
		"-f", "lavfi", "-i", "testsrc2=size=1280x720:rate=24:duration=8",
		"-f", "lavfi", "-i", "sine=frequency=440:duration=8", "-i", srt,
		"-map", "0:v", "-map", "1:a", "-map", "2:s", "-c:v", "libx264", "-g", "24", "-c:a", "ac3", "-c:s", "srt", src)
	if out, err := gen.CombinedOutput(); err != nil {
		t.Skipf("can't make test clip: %v %s", err, out)
	}
	fi, _ := os.Stat(src)

	st, err := store.Open(filepath.Join(data, "kinara.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	hash, _ := auth.HashPassword("password123")
	st.CreateUser(ctx, "alex", hash, true)
	st.CreateUser(ctx, "sam", hash, false)
	lib, _ := st.CreateLibrary(ctx, "Movies", data, store.KindMovies)
	it := store.Item{LibraryID: lib.ID, Kind: store.ItemMovie, Title: "Heat", SortTitle: "Heat", SeasonNumber: -1, EpisodeNumber: -1}
	st.CreateItem(ctx, &it)
	// A big file (as far as the server knows): 20 Mbps, so a 720p copy at 4 Mbps is worth making.
	v := store.Version{LibraryID: lib.ID, ItemID: it.ID, Path: src, RelPath: "Heat.mkv", Size: fi.Size(), Mtime: fi.ModTime().Unix(),
		Container: "mkv", DurationSec: 8, Bitrate: 20_000_000, Width: 1280, Height: 720, VideoCodec: "h264"}
	streams := []store.Stream{{Index: 0, Type: "video", Codec: "h264"}, {Index: 1, Type: "audio", Codec: "ac3", Channels: 2, Language: "eng", Default: true},
		{Index: 2, Type: "subtitle", Codec: "subrip", Language: "eng"}}
	st.SaveVersion(ctx, &v, streams)

	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	srv := &Server{Store: st, Images: images.New(filepath.Join(data, "img")), Watch: &watch.Service{Store: st}, Log: log,
		Version: "test", BaseCtx: ctx, DataDir: data, DownloadDir: filepath.Join(data, "cache", "downloads"),
		Transcoder: &transcode.Transcoder{Tool: &ffmpeg.Tool{}, Log: log},
		Subtitles:  &transcode.Subtitles{Tool: &ffmpeg.Tool{}, Dir: filepath.Join(data, "cache", "subtitles")}}
	hs := httptest.NewServer(srv.Handler())
	defer hs.Close()
	signIn := func(name string) *client {
		c := &client{t: t, base: hs.URL + "/api/v1"}
		var login struct{ Token string }
		c.do("POST", "/auth/login", map[string]string{"username": name, "password": "password123"}, &login)
		c.token = login.Token
		return c
	}
	owner, sam := signIn("alex"), signIn("sam")
	go srv.RunDownloads(ctx)

	if resp := sam.do("POST", "/optimize", map[string]any{"libraryId": lib.ID}, nil); resp.StatusCode != http.StatusForbidden {
		t.Fatalf("non-owner rule: %d", resp.StatusCode)
	}
	if resp := owner.do("POST", "/optimize", map[string]any{"libraryId": lib.ID, "quality": "original"}, nil); resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("original rule: %d", resp.StatusCode)
	}
	var rule store.OptimizeRule
	if resp := owner.do("POST", "/optimize", map[string]any{"libraryId": lib.ID, "quality": "medium"}, &rule); resp.StatusCode != http.StatusCreated {
		t.Fatalf("rule: %d", resp.StatusCode)
	}

	var view optimizeView
	deadline := time.Now().Add(90 * time.Second)
	for {
		owner.do("GET", "/optimize", nil, &view)
		if len(view.Rules) == 1 && (view.Rules[0].Ready == 1 || view.Rules[0].Failed > 0) || time.Now().After(deadline) {
			break
		}
		time.Sleep(300 * time.Millisecond)
	}
	if len(view.Rules) != 1 || view.Rules[0].Ready != 1 || view.Rules[0].Title != "Movies" || view.Rules[0].Kind != "library" || view.UsedBytes == 0 {
		t.Fatalf("rules %+v", view.Rules)
	}

	// The copy is a version of the title, after the original.
	var details struct{ Versions []VersionView }
	sam.do("GET", "/items/"+it.ID, nil, &details)
	if len(details.Versions) != 2 || details.Versions[0].ID != v.ID {
		t.Fatalf("versions %+v", details.Versions)
	}
	o := details.Versions[1]
	if !strings.HasPrefix(o.ID, "o") || o.Optimized != "medium" || o.Container != "mp4" || o.VideoCodec != "h264" || o.Height != 720 ||
		o.Bitrate <= 0 || o.Bitrate >= v.Bitrate || len(o.Streams) != 3 || o.Streams[1].Codec != "aac" {
		t.Fatalf("copy version %+v", o)
	}

	// It plays as it is, with ranges; its subtitles are the original's.
	req, _ := http.NewRequest("GET", hs.URL+o.StreamURL+"?token="+sam.token, nil)
	req.Header.Set("Range", "bytes=0-9")
	resp, err := http.DefaultClient.Do(req)
	if err != nil || resp.StatusCode != http.StatusPartialContent {
		t.Fatalf("stream %v %+v", err, resp)
	}
	resp.Body.Close()
	resp, _ = http.Get(hs.URL + "/api/v1/subtitles/" + o.ID + "/2.vtt?token=" + sam.token)
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != 200 || !strings.Contains(string(body), "Hello") {
		t.Fatalf("subtitles %d %q", resp.StatusCode, body)
	}
	// Watching it counts on the original.
	if resp := sam.do("PUT", "/progress/"+it.ID, map[string]any{"positionSec": 4, "versionId": o.ID}, nil); resp.StatusCode != 200 {
		t.Fatalf("progress %d", resp.StatusCode)
	}
	// Converting from it still works (a player that needs another format).
	resp, _ = http.Get(hs.URL + "/api/v1/transcode/" + o.ID + "?video=copy&audioCodec=aac&container=mp4&token=" + sam.token)
	n, _ := io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	if resp.StatusCode != 200 || n < 1000 {
		t.Fatalf("transcode from copy: %d, %d bytes", resp.StatusCode, n)
	}

	// The title's page in the manager.
	var info struct {
		LibraryRule *store.OptimizeRule
		Copies      []struct{ Quality, State string }
	}
	owner.do("GET", "/items/"+it.ID+"/optimize", nil, &info)
	if info.LibraryRule == nil || len(info.Copies) != 1 || info.Copies[0].State != store.DownloadReady {
		t.Fatalf("item optimize %+v", info)
	}

	// A download at the same quality and audio track shares the copy.
	var made struct{ Downloads []downloadView }
	sam.do("POST", "/downloads", map[string]any{"itemId": it.ID, "quality": "medium"}, &made)
	if len(made.Downloads) != 1 || made.Downloads[0].State != store.DownloadReady {
		t.Fatalf("download from the copy %+v", made.Downloads)
	}

	// Removing the rule: the copy stays while the download wants it, then goes.
	owner.do("DELETE", "/optimize/"+itoa(rule.ID), nil, nil)
	sam.do("GET", "/items/"+it.ID, nil, &details)
	if len(details.Versions) != 1 {
		t.Fatalf("copy still listed: %d versions", len(details.Versions))
	}
	uses, _ := st.DownloadFileUses(ctx)
	if len(uses) != 1 || uses[0].Optimized {
		t.Fatalf("copies after the rule went %+v", uses)
	}
	sam.do("DELETE", "/downloads/"+itoa(made.Downloads[0].ID), nil, nil)
	if uses, _ := st.DownloadFileUses(ctx); len(uses) != 0 {
		t.Fatalf("copy kept: %+v", uses)
	}
	if entries, _ := os.ReadDir(srv.DownloadDir); len(entries) != 0 {
		t.Fatalf("files left %v", entries)
	}
}
