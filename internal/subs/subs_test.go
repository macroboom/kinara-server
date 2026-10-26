package subs

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func TestLanguage(t *testing.T) {
	for in, want := range map[string]string{"en": "eng", "ENG": "eng", "English": "eng", "pt-BR": "por", "fra": "fre", "fre": "fre",
		"deutsch": "ger", "zh-cn": "chi", "hi": "hin", "xx": "", "": "", "1080p": ""} {
		if got := Language(in); got != want {
			t.Errorf("Language(%q) = %q, want %q", in, got, want)
		}
	}
	if Code2("ger") != "de" || Code2("eng") != "en" || Code2("zzz") != "" {
		t.Error("Code2")
	}
}

func TestFind(t *testing.T) {
	sep := string(filepath.Separator)
	dir := sep + filepath.Join("media", "Movies", "Heat (1995)")
	video := filepath.Join(dir, "Heat (1995).mkv")
	files := []string{
		filepath.Join(dir, "Heat (1995).srt"),
		filepath.Join(dir, "Heat (1995).en.forced.srt"),
		filepath.Join(dir, "Heat (1995).eng.sdh.srt"),
		filepath.Join(dir, "Heat (1995).Spanish.ass"),
		filepath.Join(dir, "Heat (1995).en.hi.srt"),
		filepath.Join(dir, "Heat (1995).hi.srt"),
		filepath.Join(dir, "Heat (1995).Commentary.en.srt"),
		filepath.Join(dir, "Other Movie.en.srt"),
		filepath.Join(dir, "Heat (1995).en.sub"),
		filepath.Join(dir, "Subs", "2_English.srt"),
		filepath.Join(dir, "Subs", "Heat (1995)", "3_French.srt"),
	}
	got := Find(video, files, 1)
	type row struct {
		name, lang, title string
		forced, hi        bool
	}
	var rows []row
	for _, s := range got {
		rel, _ := filepath.Rel(dir, s.Path)
		rows = append(rows, row{filepath.ToSlash(rel), s.Language, s.Title, s.Forced, s.HearingImpaired})
	}
	want := []row{
		{"Heat (1995).Commentary.en.srt", "eng", "Commentary", false, false},
		{"Heat (1995).Spanish.ass", "spa", "", false, false},
		{"Heat (1995).en.forced.srt", "eng", "", true, false},
		{"Heat (1995).en.hi.srt", "eng", "", false, true},
		{"Heat (1995).eng.sdh.srt", "eng", "", false, true},
		{"Heat (1995).hi.srt", "hin", "", false, false},
		{"Heat (1995).srt", "", "", false, false},
		{"Subs/2_English.srt", "eng", "", false, false},
		{"Subs/Heat (1995)/3_French.srt", "fre", "", false, false},
	}
	if !reflect.DeepEqual(rows, want) {
		t.Fatalf("got\n%v\nwant\n%v", rows, want)
	}
	// Two videos in the folder: loose Subs files belong to neither.
	got = Find(video, files, 2)
	for _, s := range got {
		if strings.HasSuffix(s.Path, "2_English.srt") {
			t.Fatal("loose Subs file given to one of two videos")
		}
	}
}

func TestHash(t *testing.T) {
	p := filepath.Join(t.TempDir(), "v.bin")
	b := make([]byte, 200_000)
	for i := range b {
		b[i] = byte(i * 7)
	}
	os.WriteFile(p, b, 0o644)
	h, err := Hash(p)
	if err != nil || len(h) != 16 {
		t.Fatalf("%q %v", h, err)
	}
	h2, _ := Hash(p)
	if h != h2 {
		t.Fatal("not stable")
	}
}

func TestUTF8(t *testing.T) {
	if got := string(UTF8([]byte("\xEF\xBB\xBFhello"))); got != "hello" {
		t.Errorf("BOM: %q", got)
	}
	if got := string(UTF8([]byte("caf\xe9 \x93hi\x94"))); got != "café “hi”" {
		t.Errorf("cp1252: %q", got)
	}
}

func TestOpenSubtitlesSearchAndDownload(t *testing.T) {
	var gotQuery, gotKey, gotAuth string
	var srv *httptest.Server
	srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/v1/subtitles":
			gotQuery, gotKey = r.URL.RawQuery, r.Header.Get("Api-Key")
			w.Write([]byte(`{"data":[
				{"id":"1","attributes":{"language":"en","download_count":10,"release":"Heat.1995.BluRay","files":[{"file_id":11,"file_name":"a.srt"}]}},
				{"id":"2","attributes":{"language":"en","download_count":500,"machine_translated":true,"files":[{"file_id":22,"file_name":"b.srt"}]}},
				{"id":"3","attributes":{"language":"en","download_count":5,"moviehash_match":true,"files":[{"file_id":33,"file_name":"c.srt"}]}}]}`))
		case "/api/v1/login":
			w.Write([]byte(`{"token":"tok","base_url":"api.opensubtitles.com"}`))
		case "/api/v1/download":
			gotAuth = r.Header.Get("Authorization")
			var body map[string]any
			json.NewDecoder(r.Body).Decode(&body)
			if body["file_id"].(float64) != 33 {
				t.Errorf("file id %v", body["file_id"])
			}
			w.Write([]byte(`{"link":"` + srv.URL + `/file/c.srt","remaining":4}`))
		case "/file/c.srt":
			w.Write([]byte("1\n00:00:01,000 --> 00:00:02,000\nHello\n"))
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()
	o := &OpenSubtitles{APIKey: "k", UserAgent: "Kinara v0", Base: srv.URL + "/api/v1"}
	res, err := o.Search(context.Background(), Query{TMDBID: 949, Languages: []string{"es", "en"}, Hash: "abc"})
	if err != nil {
		t.Fatal(err)
	}
	if gotKey != "k" || gotQuery != "languages=en%2Ces&moviehash=abc&tmdb_id=949&type=movie" {
		t.Fatalf("query %q key %q", gotQuery, gotKey)
	}
	if len(res) != 3 || res[0].FileID != 33 || res[1].FileID != 11 || res[2].FileID != 22 || res[0].Language != "eng" {
		t.Fatalf("order %+v", res)
	}
	o.SetAccount("me", "pw")
	b, err := o.Download(context.Background(), 33)
	if err != nil || !strings.Contains(string(b), "Hello") || gotAuth != "Bearer tok" {
		t.Fatalf("download %q %v auth %q", b, err, gotAuth)
	}
	if _, err := (&OpenSubtitles{}).Search(context.Background(), Query{}); err != ErrNoKey {
		t.Fatalf("no key: %v", err)
	}
}
