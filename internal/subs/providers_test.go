package subs

import (
	"archive/zip"
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestSubvault(t *testing.T) {
	fake := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/shows/search/The Bear":
			json.NewEncoder(w).Encode(map[string]any{"shows": []map[string]any{{"id": "x1", "name": "The Bear Hunt"}, {"id": "b42", "name": "The Bear"}}})
		case r.URL.Path == "/subtitles/get/b42/2/3/English":
			json.NewEncoder(w).Encode(map[string]any{"matchingSubtitles": []map[string]any{
				{"subtitleId": "s1", "version": "WEB.H264-SuccessfulCrab", "completed": true, "downloadUri": "/subtitles/download/s1", "downloadCount": 40},
				{"subtitleId": "s2", "version": "PARTIAL", "completed": false, "downloadUri": "/subtitles/download/s2"},
			}})
		case r.URL.Path == "/subtitles/get/b42/2/3/French":
			w.WriteHeader(http.StatusLocked) // refreshing the show
		case r.URL.Path == "/subtitles/download/s1":
			w.Write([]byte("1\n00:00:01,000 --> 00:00:02,000\nYes chef\n"))
		default:
			http.NotFound(w, r)
		}
	}))
	defer fake.Close()
	g := &Subvault{Base: fake.URL, UserAgent: "test"}
	q := Query{Episodic: true, Title: "The Bear", Season: 2, Episode: 3, Languages: []string{"en"}}
	if !g.Covers(q) || g.Covers(Query{Title: "Heat", Languages: []string{"en"}}) {
		t.Fatal("covers")
	}
	res, err := g.Search(context.Background(), q)
	if err != nil || len(res) != 1 || res[0].Language != "eng" || res[0].Provider != ProviderSubvault || res[0].Downloads != 40 {
		t.Fatalf("results %+v %v", res, err)
	}
	body, err := g.Fetch(context.Background(), res[0])
	if err != nil || !strings.Contains(string(body), "Yes chef") {
		t.Fatalf("fetch %q %v", body, err)
	}
	q.Languages = []string{"fr"}
	if _, err := g.Search(context.Background(), q); err != ErrTryLater {
		t.Fatalf("refreshing: %v", err)
	}
}

func TestSubDLEpisodes(t *testing.T) {
	var zbuf bytes.Buffer
	zw := zip.NewWriter(&zbuf)
	for _, n := range []string{"Show.S01E01.srt", "Show.S01E02.srt", "readme.txt"} {
		f, _ := zw.Create(n)
		f.Write([]byte("1\n00:00:01,000 --> 00:00:02,000\n" + n + "\n"))
	}
	zw.Close()
	fake := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/v1/subtitles":
			if r.URL.Query().Get("type") != "tv" || r.URL.Query().Get("season_number") != "1" || r.URL.Query().Get("tmdb_id") != "1399" {
				t.Errorf("query %s", r.URL.RawQuery)
			}
			ep := 2
			other := 5
			json.NewEncoder(w).Encode(map[string]any{"status": true, "subtitles": []map[string]any{
				{"release_name": "Season pack", "language": "EN", "url": "/subtitle/pack.zip", "full_season": true},
				{"release_name": "Other episode", "language": "EN", "url": "/subtitle/o.zip", "episode": other},
				{"release_name": "S01E02 WEB", "language": "EN", "url": "/subtitle/e2.zip", "episode": ep, "hi": true},
			}})
		case "/subtitle/e2.zip":
			w.Write(zbuf.Bytes())
		default:
			http.NotFound(w, r)
		}
	}))
	defer fake.Close()
	s := &SubDL{APIKey: "k", Base: fake.URL + "/api/v1", DownloadBase: fake.URL}
	res, err := s.Search(context.Background(), Query{Episodic: true, TMDBID: 1399, Season: 1, Episode: 2, Languages: []string{"en"}})
	if err != nil || len(res) != 1 || !res[0].HearingImpaired || res[0].Language != "eng" {
		t.Fatalf("results %+v %v", res, err)
	}
	body, err := s.Fetch(context.Background(), res[0])
	if err != nil || !strings.Contains(string(body), "srt") {
		t.Fatalf("fetch %q %v", body, err)
	}
	// From a pack, the episode's own file.
	body, err = unpack(zbuf.Bytes(), 2)
	if err != nil || !strings.Contains(string(body), "S01E02") {
		t.Fatalf("unpack %q %v", body, err)
	}
}

func TestJimaku(t *testing.T) {
	fake := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "jk" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		switch r.URL.Path {
		case "/entries/search":
			json.NewEncoder(w).Encode([]map[string]any{{"id": 7, "name": "Frieren"}})
		case "/entries/7/files":
			if r.URL.Query().Get("episode") != "4" {
				t.Errorf("episode %s", r.URL.RawQuery)
			}
			json.NewEncoder(w).Encode([]map[string]any{
				{"url": "http://" + r.Host + "/f/ep4.ass", "name": "Frieren 04.ass", "size": 9000},
				{"url": "http://" + r.Host + "/f/all.7z", "name": "all.7z", "size": 90000},
			})
		case "/f/ep4.ass":
			w.Write([]byte("[Script Info]\nTitle: x\n"))
		default:
			http.NotFound(w, r)
		}
	}))
	defer fake.Close()
	j := &Jimaku{APIKey: "jk", Base: fake.URL}
	q := Query{Episodic: true, Title: "Frieren", Season: 1, Episode: 4, Languages: []string{"ja"}}
	if !j.Covers(q) || j.Covers(Query{Title: "Frieren", Languages: []string{"en"}}) {
		t.Fatal("covers")
	}
	res, err := j.Search(context.Background(), q)
	if err != nil || len(res) != 1 || res[0].Language != "jpn" {
		t.Fatalf("results %+v %v", res, err)
	}
	body, err := j.Fetch(context.Background(), res[0])
	if err != nil || !strings.HasPrefix(string(body), "[Script Info]") {
		t.Fatalf("fetch %q %v", body, err)
	}
}

// A failed connection's error doesn't carry SubDL's key.
func TestSubDLErrorHidesKey(t *testing.T) {
	fake := httptest.NewServer(http.NotFoundHandler())
	base := fake.URL
	fake.Close()
	s := &SubDL{APIKey: "secret-subdl", Base: base + "/api/v1", DownloadBase: base}
	_, err := s.Search(context.Background(), Query{TMDBID: 1, Title: "Heat", Languages: []string{"en"}})
	if err == nil || strings.Contains(err.Error(), "secret-subdl") {
		t.Fatalf("error %v", err)
	}
}
