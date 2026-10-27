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
	"strconv"
	"testing"
	"time"

	"kinara/internal/auth"
	"kinara/internal/ffmpeg"
	"kinara/internal/images"
	"kinara/internal/store"
	"kinara/internal/transcode"
	"kinara/internal/watch"
)

func TestDownloads(t *testing.T) {
	if _, err := exec.LookPath("ffmpeg"); err != nil {
		t.Skip("ffmpeg not installed")
	}
	ctx, stop := context.WithCancel(context.Background())
	defer stop()
	data := t.TempDir()

	// A 10 second film with 5.1 sound and a subtitle track.
	src := filepath.Join(data, "Heat.mkv")
	srt := filepath.Join(data, "in.srt")
	os.WriteFile(srt, []byte("1\n00:00:01,000 --> 00:00:03,000\nHello\n"), 0o644)
	gen := exec.Command("ffmpeg", "-hide_banner", "-loglevel", "error",
		"-f", "lavfi", "-i", "testsrc2=size=1280x720:rate=24:duration=10",
		"-f", "lavfi", "-i", "sine=frequency=440:duration=10", "-i", srt,
		"-filter_complex", "[1:a]pan=5.1|c0=c0|c1=c0|c2=c0|c3=c0|c4=c0|c5=c0[a]",
		"-map", "0:v", "-map", "[a]", "-map", "2:s", "-c:v", "libx264", "-g", "24", "-c:a", "ac3", "-c:s", "srt", src)
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
	it := store.Item{LibraryID: lib.ID, Kind: store.ItemMovie, Title: "Heat", SortTitle: "Heat", Year: 1995, SeasonNumber: -1, EpisodeNumber: -1}
	st.CreateItem(ctx, &it)
	v := store.Version{LibraryID: lib.ID, ItemID: it.ID, Path: src, RelPath: "Heat.mkv", Size: fi.Size(), Mtime: fi.ModTime().Unix(),
		Container: "mkv", DurationSec: 10, Bitrate: 20_000_000, Width: 1280, Height: 720, VideoCodec: "h264"}
	streams := []store.Stream{{Index: 0, Type: "video", Codec: "h264"}, {Index: 1, Type: "audio", Codec: "ac3", Channels: 6, Language: "eng", Default: true},
		{Index: 2, Type: "subtitle", Codec: "subrip", Language: "eng"}}
	if err := st.SaveVersion(ctx, &v, streams); err != nil {
		t.Fatal(err)
	}

	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	srv := &Server{Store: st, Images: images.New(filepath.Join(data, "img")), Watch: &watch.Service{Store: st}, Log: log,
		Version: "test", BaseCtx: ctx, DataDir: data, DownloadDir: filepath.Join(data, "cache", "downloads"),
		Transcoder: &transcode.Transcoder{Tool: &ffmpeg.Tool{}, Log: log}}
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

	var opts struct {
		Enabled   bool
		Count     int
		Default   string
		Qualities []qualityOption
	}
	sam.do("GET", "/items/"+it.ID+"/download", nil, &opts)
	if !opts.Enabled || opts.Count != 1 || opts.Default != "medium" || len(opts.Qualities) != 4 || opts.Qualities[0].Estimate != fi.Size() {
		t.Fatalf("options %+v", opts)
	}

	type made struct {
		Downloads []downloadView
		Error     string
	}
	var m made
	if resp := sam.do("POST", "/downloads", map[string]any{"itemId": it.ID, "quality": "low"}, &m); resp.StatusCode != http.StatusCreated {
		t.Fatalf("create %d %s", resp.StatusCode, m.Error)
	}
	if len(m.Downloads) != 1 || m.Downloads[0].Quality != "low" || !m.Downloads[0].ThisDevice || m.Downloads[0].Item.Title != "Heat" {
		t.Fatalf("made %+v", m.Downloads)
	}
	id := strconv.FormatInt(m.Downloads[0].ID, 10)
	// The owner downloads it too: one copy, shared.
	var m2 made
	owner.do("POST", "/downloads", map[string]any{"itemId": it.ID, "quality": "low"}, &m2)
	uses, _ := st.DownloadFileUses(ctx)
	if len(uses) != 1 || uses[0].Downloads != 2 {
		t.Fatalf("copies %+v", uses)
	}

	var d downloadView
	deadline := time.Now().Add(90 * time.Second)
	for {
		var list struct{ Downloads []downloadView }
		sam.do("GET", "/downloads", nil, &list)
		if len(list.Downloads) != 1 {
			t.Fatalf("sam sees %d downloads", len(list.Downloads))
		}
		d = list.Downloads[0]
		if d.State == store.DownloadReady || d.State == store.DownloadFailed || time.Now().After(deadline) {
			break
		}
		time.Sleep(300 * time.Millisecond)
	}
	if d.State != store.DownloadReady || d.FileURL == "" || d.Size == 0 {
		t.Fatalf("not ready: %+v", d)
	}

	var detail downloadDetail
	sam.do("GET", "/downloads/"+id, nil, &detail)
	if detail.Audio == nil || detail.Audio.Index != 1 || len(detail.Subtitles) != 1 || detail.Subtitles[0].URL != "/api/v1/subtitles/"+v.ID+"/2" {
		t.Fatalf("detail %+v", detail)
	}

	// Ranges, so a download can pick up where it stopped.
	req, _ := http.NewRequest("GET", hs.URL+d.FileURL+"?token="+sam.token, nil)
	req.Header.Set("Range", "bytes=0-99")
	resp, err := http.DefaultClient.Do(req)
	if err != nil || resp.StatusCode != http.StatusPartialContent || resp.ContentLength != 100 {
		t.Fatalf("range %v %+v", err, resp)
	}
	resp.Body.Close()
	resp, _ = http.Get(hs.URL + d.FileURL + "?token=" + sam.token)
	out := filepath.Join(data, "got.mp4")
	f, _ := os.Create(out)
	io.Copy(f, resp.Body)
	f.Close()
	resp.Body.Close()
	if cd := resp.Header.Get("Content-Disposition"); cd == "" || resp.Header.Get("Content-Type") != "video/mp4" {
		t.Fatalf("headers %v", resp.Header)
	}
	probe, err := exec.Command("ffprobe", "-v", "error", "-show_entries", "stream=codec_name,height,channels", "-of", "csv=p=0", out).Output()
	if err != nil || string(probe) != "h264,480\naac,2\n" {
		t.Fatalf("probe %q %v", probe, err)
	}

	// Someone else's download is out of reach.
	if resp := sam.do("GET", "/downloads/"+strconv.FormatInt(m2.Downloads[0].ID, 10), nil, nil); resp.StatusCode != http.StatusNotFound {
		t.Fatalf("sam read the owner's download: %d", resp.StatusCode)
	}
	if resp := sam.do("POST", "/downloads/"+id+"/saved", nil, nil); resp.StatusCode != http.StatusNoContent {
		t.Fatalf("saved %d", resp.StatusCode)
	}

	// Removed to save room, then asked for again: prepared again.
	cp, _ := st.GetDownloadFile(ctx, uses[0].ID)
	srv.expireCopy(ctx, cp)
	if resp := sam.do("GET", "/downloads/"+id+"/file", nil, nil); resp.StatusCode != http.StatusConflict {
		t.Fatalf("expired fetch %d", resp.StatusCode)
	}
	if cp, _ = st.GetDownloadFile(ctx, cp.ID); cp.State != store.DownloadQueued && cp.State != store.DownloadPreparing && cp.State != store.DownloadReady {
		t.Fatalf("not requeued: %s", cp.State)
	}

	// Original: ready at once, the file as it is.
	var orig made
	owner.do("POST", "/downloads", map[string]any{"itemId": it.ID, "quality": "original"}, &orig)
	if len(orig.Downloads) != 1 || orig.Downloads[0].State != store.DownloadReady || orig.Downloads[0].Size != fi.Size() {
		t.Fatalf("original %+v", orig.Downloads)
	}
	resp, _ = http.Get(hs.URL + orig.Downloads[0].FileURL + "?token=" + owner.token)
	n, _ := io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	if n != fi.Size() || resp.Header.Get("Content-Type") != "video/x-matroska" {
		t.Fatalf("original fetch %d bytes, %s", n, resp.Header.Get("Content-Type"))
	}

	// Removing every download of a copy removes the copy.
	waitIdle := time.Now().Add(60 * time.Second)
	for {
		cp, _ = st.GetDownloadFile(ctx, cp.ID)
		if cp.State == store.DownloadReady || time.Now().After(waitIdle) {
			break
		}
		time.Sleep(200 * time.Millisecond)
	}
	sam.do("DELETE", "/downloads/"+id, nil, nil)
	if _, err := st.GetDownloadFile(ctx, cp.ID); err == nil {
		t.Fatal("copy kept with nobody wanting it")
	}
	entries, _ := os.ReadDir(srv.DownloadDir)
	if len(entries) != 0 {
		t.Fatalf("files left: %v", entries)
	}

	// The owner's view, and turning downloads off.
	var all downloadsAdminView
	owner.do("GET", "/downloads/all", nil, &all)
	if !all.CanConvert || len(all.Downloads) != 1 || all.Downloads[0].Username != "alex" {
		t.Fatalf("all %+v", all)
	}
	if resp := sam.do("PUT", "/downloads/settings", map[string]any{"enabled": false}, nil); resp.StatusCode != http.StatusForbidden {
		t.Fatalf("non-owner settings %d", resp.StatusCode)
	}
	owner.do("PUT", "/downloads/settings", map[string]any{"enabled": false, "maxGB": 50, "perUser": 1}, nil)
	if resp := sam.do("POST", "/downloads", map[string]any{"itemId": it.ID}, nil); resp.StatusCode != http.StatusForbidden {
		t.Fatalf("while off %d", resp.StatusCode)
	}
	owner.do("PUT", "/downloads/settings", map[string]any{"enabled": true, "maxGB": 50, "perUser": 1}, nil)
	sam.do("POST", "/downloads", map[string]any{"itemId": it.ID, "quality": "original"}, nil)
	it2 := store.Item{LibraryID: lib.ID, Kind: store.ItemMovie, Title: "Alien", SortTitle: "Alien", SeasonNumber: -1, EpisodeNumber: -1}
	st.CreateItem(ctx, &it2)
	v2 := v
	v2.ID, v2.ItemID, v2.Path, v2.RelPath = "", it2.ID, src+".copy", "Alien.mkv"
	st.SaveVersion(ctx, &v2, streams)
	if resp := sam.do("POST", "/downloads", map[string]any{"itemId": it2.ID, "quality": "original"}, nil); resp.StatusCode != http.StatusConflict {
		t.Fatalf("over the limit %d", resp.StatusCode)
	}
}

func TestPickAudio(t *testing.T) {
	streams := []store.Stream{{Index: 0, Type: "video"}, {Index: 1, Type: "audio", Language: "fre"},
		{Index: 2, Type: "audio", Language: "eng", Default: true}, {Index: 3, Type: "audio", Language: "jpn"}}
	if got := pickAudio(streams, "jpn"); got != 3 {
		t.Fatalf("language: %d", got)
	}
	if got := pickAudio(streams, ""); got != 2 {
		t.Fatalf("default: %d", got)
	}
	if got := pickAudio(streams[:2], "deu"); got != 1 {
		t.Fatalf("first: %d", got)
	}
	if got := pickAudio(streams[:1], ""); got != -1 {
		t.Fatalf("none: %d", got)
	}
}
