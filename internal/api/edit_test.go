//go:build !nosqlite

package api

import (
	"bytes"
	"context"
	"image"
	"image/color"
	"image/png"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"kinara/internal/auth"
	"kinara/internal/images"
	"kinara/internal/store"
	"kinara/internal/tmdb"
	"kinara/internal/watch"
)

func TestEditTitle(t *testing.T) {
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
	lib, _ := st.CreateLibrary(ctx, "Movies", "/m", store.KindMovies)
	movieID := addMovie(t, st, lib, "Heat")
	st.SetExternalID(ctx, movieID, store.ProviderTMDBMovie, "949")
	st.SetImage(ctx, movieID, "poster", "https://image.tmdb.org/t/p/original/old.jpg")

	fake := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/movie/949/images" || !strings.Contains(r.URL.RawQuery, "include_image_language") {
			http.NotFound(w, r)
			return
		}
		w.Write([]byte(`{"posters":[{"file_path":"/lowres.jpg","width":500,"height":750,"vote_average":2},{"file_path":"/best.jpg","width":2000,"height":3000,"iso_639_1":"en","vote_average":5.5}],"backdrops":[]}`))
	}))
	defer fake.Close()
	tm := tmdb.New("key")
	tm.BaseURL = fake.URL

	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	srv := &Server{Store: st, Images: images.New(filepath.Join(data, "img")), Watch: &watch.Service{Store: st}, Log: log,
		Version: "0.3.0", BaseCtx: ctx, DataDir: data, TMDB: tm}
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

	if resp := sam.do("PATCH", "/items/"+movieID, map[string]any{"title": "Nope"}, nil); resp.StatusCode != http.StatusForbidden {
		t.Fatalf("non-owner edit: %d", resp.StatusCode)
	}
	var v editView
	if resp := owner.do("PATCH", "/items/"+movieID, map[string]any{"title": "The Heat", "year": 1995, "genres": []string{"Crime", "Drama"}}, &v); resp.StatusCode != 200 {
		t.Fatalf("edit: %d", resp.StatusCode)
	}
	if v.Title != "The Heat" || v.SortTitle != "Heat" || len(v.Genres) != 2 || !v.CanPick {
		t.Fatalf("after edit %+v", v)
	}
	if resp := owner.do("PATCH", "/items/"+movieID, map[string]any{"title": "  "}, nil); resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("empty title: %d", resp.StatusCode)
	}
	if resp := owner.do("PATCH", "/items/"+movieID, map[string]any{"airDate": "soon"}, nil); resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("bad date: %d", resp.StatusCode)
	}

	// TMDB's posters, best first; picking one locks it.
	var choices struct{ Choices []artChoice }
	owner.do("GET", "/items/"+movieID+"/artwork/poster", nil, &choices)
	if len(choices.Choices) != 2 || choices.Choices[0].Path != "/best.jpg" || choices.Choices[0].URL != "/api/v1/tmdb-image/best.jpg" {
		t.Fatalf("choices %+v", choices.Choices)
	}
	owner.do("PUT", "/items/"+movieID+"/images/poster", map[string]string{"path": "/best.jpg"}, &v)
	if u, _ := st.ImageURL(ctx, movieID, "poster"); u != tm.Image("/best.jpg") {
		t.Fatalf("poster %q", u)
	}
	if resp := owner.do("PUT", "/items/"+movieID+"/images/still", map[string]string{"path": "/best.jpg"}, nil); resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("still on a movie: %d", resp.StatusCode)
	}

	// An upload.
	img := image.NewNRGBA(image.Rect(0, 0, 200, 300))
	img.Set(5, 5, color.White)
	var buf bytes.Buffer
	png.Encode(&buf, img)
	req, _ := http.NewRequest("POST", hs.URL+"/api/v1/items/"+movieID+"/images/poster/upload", bytes.NewReader(buf.Bytes()))
	req.Header.Set("Authorization", "Bearer "+owner.token)
	resp, _ := http.DefaultClient.Do(req)
	if resp.StatusCode != 200 {
		t.Fatalf("upload %d", resp.StatusCode)
	}
	u, _ := st.ImageURL(ctx, movieID, "poster")
	if !strings.HasPrefix(u, dataPrefix+"artwork/") || !strings.HasSuffix(u, ".png") {
		t.Fatalf("uploaded poster %q", u)
	}
	req, _ = http.NewRequest("POST", hs.URL+"/api/v1/items/"+movieID+"/images/poster/upload", strings.NewReader("not a picture"))
	req.Header.Set("Authorization", "Bearer "+owner.token)
	if resp, _ := http.DefaultClient.Do(req); resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("junk upload %d", resp.StatusCode)
	}

	// The item's artwork address changes, and the server still serves it.
	var details struct{ Item ItemView }
	owner.do("GET", "/items/"+movieID, nil, &details)
	if !strings.Contains(details.Item.PosterURL, "~") {
		t.Fatalf("poster url %q", details.Item.PosterURL)
	}
	resp, err = http.Get(hs.URL + details.Item.PosterURL + "?w=160&token=" + owner.token)
	if err != nil || resp.StatusCode != 200 {
		t.Fatalf("poster fetch %v %d", err, resp.StatusCode)
	}

	owner.do("POST", "/items/"+movieID+"/unlock", map[string]any{"fields": []string{"all"}}, &v)
	if len(v.Locked) != 0 {
		t.Fatalf("locks left %v", v.Locked)
	}
}
