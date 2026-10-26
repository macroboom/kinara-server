//go:build !nosqlite

package api

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"kinara/internal/auth"
	"kinara/internal/ffmpeg"
	"kinara/internal/images"
	"kinara/internal/store"
	"kinara/internal/subs"
	"kinara/internal/transcode"
	"kinara/internal/watch"
)

func TestSubtitleFilesAndDownloads(t *testing.T) {
	ctx := context.Background()
	data := t.TempDir()
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
	st.SetExternalID(ctx, it.ID, store.ProviderTMDBMovie, "949")
	videoPath := filepath.Join(data, "Heat.mkv")
	os.WriteFile(videoPath, make([]byte, 300_000), 0o644)
	v := store.Version{LibraryID: lib.ID, ItemID: it.ID, Path: videoPath, RelPath: "Heat.mkv", Size: 300_000}
	st.SaveVersion(ctx, &v, nil)
	// A sidecar, as the scanner would have found it (Windows-1252 text).
	side := filepath.Join(data, "Heat.en.srt")
	os.WriteFile(side, []byte("1\n00:00:01,000 --> 00:00:02,000\nCaf\xe9\n"), 0o644)
	st.SyncSidecars(ctx, v.ID, []store.SubtitleFile{{Path: side, Language: "eng", Format: "subrip"}})

	var downloads int
	os1 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/v1/subtitles":
			if r.URL.Query().Get("tmdb_id") != "949" || r.URL.Query().Get("moviehash") == "" {
				t.Errorf("search query %s", r.URL.RawQuery)
			}
			w.Write([]byte(`{"data":[{"id":"1","attributes":{"language":"es","download_count":9,"release":"Heat.1995.1080p","files":[{"file_id":77,"file_name":"x.srt"}]}}]}`))
		case "/api/v1/download":
			downloads++
			w.Write([]byte(`{"link":"http://` + r.Host + `/f.srt","remaining":9}`))
		case "/f.srt":
			w.Write([]byte("1\n00:00:01,000 --> 00:00:02,000\nHola\n"))
		}
	}))
	defer os1.Close()

	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	srv := &Server{Store: st, Images: images.New(filepath.Join(data, "img")), Watch: &watch.Service{Store: st}, Log: log,
		Version: "0.3.0", BaseCtx: ctx, DataDir: data,
		Subtitles: &transcode.Subtitles{Tool: &ffmpeg.Tool{}, Dir: filepath.Join(data, "subs-cache")}}
	srv.ApplySubtitleSettings(ctx, "builtin-key")
	srv.subs.client.Base = os1.URL + "/api/v1"
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

	// The item lists the sidecar apart from the file's tracks.
	var details struct {
		Versions []VersionView
	}
	sam.do("GET", "/items/"+it.ID, nil, &details)
	if len(details.Versions) != 1 || len(details.Versions[0].Subtitles) != 1 {
		t.Fatalf("versions %+v", details.Versions)
	}
	ext := details.Versions[0].Subtitles[0]
	if ext.Index < ExternalSubtitleBase || ext.Language != "eng" || ext.Source != "sidecar" || ext.Removable {
		t.Fatalf("sidecar %+v", ext)
	}
	if _, err := exec.LookPath("ffmpeg"); err == nil {
		resp, err := http.Get(hs.URL + ext.URL + "?token=" + sam.token)
		if err != nil {
			t.Fatal(err)
		}
		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		if resp.StatusCode != 200 || !strings.Contains(string(body), "WEBVTT") || !strings.Contains(string(body), "Café") {
			t.Fatalf("vtt %d %q", resp.StatusCode, body)
		}
	}

	// Search and download.
	var found struct{ Results []subs.Result }
	if resp := sam.do("GET", "/subtitles/"+v.ID+"/search?languages=spa,en", nil, &found); resp.StatusCode != 200 {
		t.Fatalf("search %d", resp.StatusCode)
	}
	if len(found.Results) != 1 || found.Results[0].FileID != 77 || found.Results[0].Language != "spa" {
		t.Fatalf("results %+v", found.Results)
	}
	var got ExternalSubtitle
	if resp := sam.do("POST", "/subtitles/"+v.ID+"/download", map[string]any{"fileId": 77, "language": "spa", "release": "Heat.1995.1080p"}, &got); resp.StatusCode != http.StatusCreated {
		t.Fatalf("download %d", resp.StatusCode)
	}
	if got.Source != "download" || got.Language != "spa" || !got.Removable || got.Title != "Heat.1995.1080p" {
		t.Fatalf("downloaded %+v", got)
	}
	// The same file again: no second download.
	sam.do("POST", "/subtitles/"+v.ID+"/download", map[string]any{"fileId": 77, "language": "spa"}, &got)
	if downloads != 1 {
		t.Fatalf("downloaded %d times", downloads)
	}
	// Someone else can't remove it; sidecars can't be removed at all.
	if resp := signIn("alex").do("DELETE", "/subtitles/"+v.ID+"/files/"+strconv.Itoa(ext.Index), nil, nil); resp.StatusCode != http.StatusConflict {
		t.Fatalf("delete sidecar %d", resp.StatusCode)
	}
	st.CreateUser(ctx, "kim", hash, false)
	if resp := signIn("kim").do("DELETE", "/subtitles/"+v.ID+"/files/"+strconv.Itoa(got.Index), nil, nil); resp.StatusCode != http.StatusForbidden {
		t.Fatalf("delete by another %d", resp.StatusCode)
	}

	// Settings: the password never comes back; turning downloads off for
	// others stops them.
	var set SubtitleSettings
	owner.do("PUT", "/subtitles/settings", map[string]any{"everyone": false, "username": "", "cleanDvd": true}, &set)
	if set.Everyone || !set.Available || set.HasPassword {
		t.Fatalf("settings %+v", set)
	}
	raw, _ := json.Marshal(set)
	if strings.Contains(string(raw), `"password"`) {
		t.Fatal("password in the answer")
	}
	if resp := sam.do("GET", "/subtitles/"+v.ID+"/search", nil, nil); resp.StatusCode != http.StatusForbidden {
		t.Fatalf("search when off %d", resp.StatusCode)
	}
	if resp := sam.do("DELETE", "/subtitles/"+v.ID+"/files/"+strconv.Itoa(got.Index), nil, nil); resp.StatusCode != http.StatusNoContent {
		t.Fatalf("delete own %d", resp.StatusCode)
	}
}
