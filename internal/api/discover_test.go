//go:build !nosqlite

package api

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"kinora/internal/auth"
	"kinora/internal/images"
	"kinora/internal/probe"
	"kinora/internal/scanner"
	"kinora/internal/store"
	"kinora/internal/tmdb"
	"kinora/internal/watch"
)

// A fake TMDB with one movie (1, also in the library) and one show (2).
func fakeTMDBServer(t *testing.T, calls *atomic.Int32) *httptest.Server {
	t.Helper()
	pages := map[string]string{
		"/movie/1":                `{"id":1,"title":"Masterplan","overview":"A heist.","release_date":"2026-09-01","runtime":112,"vote_average":7.46,"poster_path":"/pMaster1.jpg","backdrop_path":"/bMaster1.jpg","genres":[{"id":35,"name":"Comedy"}]}`,
		"/movie/1/credits":        `{"cast":[{"name":"Ann Lee","character":"Mara","profile_path":"/aAnnLee.jpg","order":0}]}`,
		"/tv/2":                   `{"id":2,"name":"Tulsa King","overview":"A mob boss.","first_air_date":"2022-11-13","vote_average":8.2,"poster_path":"/pTulsa22.jpg","episode_run_time":[45],"status":"Returning Series","genres":[{"id":80,"name":"Crime"}],"seasons":[{"season_number":0,"name":"Specials","episode_count":2,"air_date":"2023-01-01"},{"season_number":1,"name":"Season 1","episode_count":9,"air_date":"2022-11-13","poster_path":"/sSeason1.jpg"},{"season_number":3,"name":"Season 3","episode_count":0}]}`,
		"/tv/2/aggregate_credits": `{"cast":[{"name":"Sly","profile_path":"/sSlyStal.jpg","order":0,"roles":[{"character":"Dwight"}]}]}`,
		"/tv/2/season/1":          `{"season_number":1,"name":"Season 1","episodes":[{"episode_number":1,"name":"Go West","overview":"He's sent west.","air_date":"2022-11-13","runtime":44,"still_path":"/eGoWest1.jpg","vote_average":7.9}]}`,
	}
	hs := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		body, ok := pages[r.URL.Path]
		if !ok {
			w.WriteHeader(http.StatusNotFound)
			_, _ = w.Write([]byte(`{"status_message":"not found"}`))
			return
		}
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(hs.Close)
	return hs
}

func TestExternalTitles(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	st, err := store.Open(filepath.Join(dir, "kinora.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	hash, _ := auth.HashPassword("password123")
	if _, err := st.CreateUser(ctx, "alex", hash, true); err != nil {
		t.Fatal(err)
	}
	lib, err := st.CreateLibrary(ctx, "Movies", "/movies", store.KindMovies)
	if err != nil {
		t.Fatal(err)
	}
	owned := addMovie(t, st, lib, "Masterplan")
	if err := st.SetExternalID(ctx, owned, store.ProviderTMDBMovie, "1"); err != nil {
		t.Fatal(err)
	}

	var calls atomic.Int32
	fake := fakeTMDBServer(t, &calls)
	srv := &Server{Store: st, Scanner: &scanner.Scanner{Store: st, Prober: &probe.Prober{}, Log: log},
		Images: images.New(filepath.Join(dir, "img")), Watch: &watch.Service{Store: st}, Log: log, Version: "test", BaseCtx: ctx,
		TMDB: &tmdb.Client{Key: "k", BaseURL: fake.URL, HTTP: fake.Client()}}
	hs := httptest.NewServer(srv.Handler())
	t.Cleanup(hs.Close)
	c := &client{t: t, base: hs.URL + "/api/v1"}
	var login struct{ Token string }
	c.do("POST", "/auth/login", map[string]string{"username": "alex", "password": "password123"}, &login)
	c.token = login.Token

	var movie externalTitleView
	if resp := c.do("GET", "/tmdb/movie/1", nil, &movie); resp.StatusCode != 200 {
		t.Fatalf("movie: %d", resp.StatusCode)
	}
	if movie.Title != "Masterplan" || movie.Year != 2026 || movie.RuntimeMin != 112 || movie.Rating != 7.5 ||
		len(movie.Genres) != 1 || len(movie.Cast) != 1 || movie.Cast[0].Role != "Mara" ||
		!strings.HasPrefix(movie.PosterURL, "/api/v1/tmdb-image/") {
		t.Fatalf("movie: %+v", movie)
	}
	if movie.Item == nil || movie.Item.ID != owned {
		t.Fatalf("the library's copy comes with it: %+v", movie.Item)
	}

	var show externalTitleView
	c.do("GET", "/tmdb/show/2", nil, &show)
	if show.Title != "Tulsa King" || show.Status != "Returning Series" || show.RuntimeMin != 45 || show.Item != nil {
		t.Fatalf("show: %+v", show)
	}
	// Empty seasons dropped, specials last.
	if len(show.Seasons) != 2 || show.Seasons[0].Number != 1 || show.Seasons[1].Number != 0 || show.Seasons[0].EpisodeCount != 9 {
		t.Fatalf("seasons: %+v", show.Seasons)
	}

	var season externalSeasonView
	c.do("GET", "/tmdb/show/2/season/1", nil, &season)
	if len(season.Episodes) != 1 || season.Episodes[0].Title != "Go West" || season.Episodes[0].RuntimeMin != 44 || season.Episodes[0].StillURL == "" {
		t.Fatalf("season: %+v", season)
	}

	// Asked again: answered from the cache.
	before := calls.Load()
	c.do("GET", "/tmdb/show/2", nil, &show)
	c.do("GET", "/tmdb/show/2/season/1", nil, &season)
	if calls.Load() != before {
		t.Fatalf("expected cached answers, TMDB was called %d more times", calls.Load()-before)
	}

	if resp := c.do("GET", "/tmdb/movie/99", nil, nil); resp.StatusCode != http.StatusNotFound {
		t.Fatalf("unknown title: %d", resp.StatusCode)
	}
	if resp := c.do("GET", "/tmdb/person/1", nil, nil); resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("bad kind: %d", resp.StatusCode)
	}
}
