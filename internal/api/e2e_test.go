//go:build !nosqlite

package api

import (
	"bytes"
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
	"time"

	"kinora/internal/auth"
	"kinora/internal/images"
	"kinora/internal/match"
	"kinora/internal/probe"
	"kinora/internal/scanner"
	"kinora/internal/store"
	"kinora/internal/tmdb"
	"kinora/internal/watch"
)

// fakeTMDB answers like TMDB for two titles.
type fakeTMDB struct{}

func (fakeTMDB) SearchMovie(ctx context.Context, q string, year int) ([]tmdb.MovieResult, error) {
	return []tmdb.MovieResult{{ID: 101, Title: "Night Shift", ReleaseDate: "2023-05-01"}}, nil
}
func (fakeTMDB) SearchTV(ctx context.Context, q string, year int) ([]tmdb.TVResult, error) {
	return []tmdb.TVResult{{ID: 202, Name: "Small Hours", FirstAirDate: "2019-01-01"}}, nil
}
func (fakeTMDB) Movie(ctx context.Context, id int) (tmdb.MovieDetails, error) {
	return tmdb.MovieDetails{ID: id, Title: "Night Shift", Overview: "A long night.", ReleaseDate: "2023-05-01", Runtime: 1, VoteAverage: 7.5}, nil
}
func (fakeTMDB) TV(ctx context.Context, id int) (tmdb.TVDetails, error) {
	return tmdb.TVDetails{ID: id, Name: "Small Hours", FirstAirDate: "2019-01-01"}, nil
}
func (fakeTMDB) Season(ctx context.Context, tvID, n int) (tmdb.SeasonDetails, error) {
	d := tmdb.SeasonDetails{SeasonNumber: n, Name: "Season 1"}
	d.Episodes = append(d.Episodes, struct {
		ID            int     `json:"id"`
		EpisodeNumber int     `json:"episode_number"`
		Name          string  `json:"name"`
		Overview      string  `json:"overview"`
		AirDate       string  `json:"air_date"`
		Runtime       int     `json:"runtime"`
		StillPath     string  `json:"still_path"`
		VoteAverage   float64 `json:"vote_average"`
	}{ID: 1, EpisodeNumber: 1, Name: "Pilot"})
	d.Episodes = append(d.Episodes, d.Episodes[0])
	d.Episodes[1].ID, d.Episodes[1].EpisodeNumber, d.Episodes[1].Name = 2, 2, "Second Night"
	return d, nil
}
func (fakeTMDB) FindIMDB(ctx context.Context, id string) (int, int, error) { return 0, 0, nil }
func (fakeTMDB) Image(p string) string                                     { return "" }
func (fakeTMDB) MovieCast(ctx context.Context, id int) ([]tmdb.CastMember, error) {
	return []tmdb.CastMember{{Name: "Ada Park", Character: "Mara"}}, nil
}
func (fakeTMDB) TVCast(ctx context.Context, id int) ([]tmdb.CastMember, error) {
	return []tmdb.CastMember{{Name: "Tom Reyes", Character: "Tom"}}, nil
}

func makeClip(t *testing.T, path string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command("ffmpeg", "-v", "error", "-y", "-f", "lavfi", "-i", "testsrc=size=320x180:rate=24:duration=3",
		"-f", "lavfi", "-i", "sine=duration=3", "-c:v", "libx264", "-c:a", "aac", path)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Skipf("ffmpeg could not make a clip: %v %s", err, out)
	}
	old := time.Now().Add(-time.Hour)
	os.Chtimes(path, old, old)
}

type client struct {
	t     *testing.T
	base  string
	token string
	// http is the client to use (http.DefaultClient when nil).
	http *http.Client
}

func (c *client) do(method, path string, body any, out any, headers ...string) *http.Response {
	c.t.Helper()
	var r io.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		r = bytes.NewReader(b)
	}
	req, _ := http.NewRequest(method, c.base+path, r)
	if c.token != "" {
		req.Header.Set("Authorization", "Bearer "+c.token)
	}
	for i := 0; i+1 < len(headers); i += 2 {
		req.Header.Set(headers[i], headers[i+1])
	}
	hc := c.http
	if hc == nil {
		hc = http.DefaultClient
	}
	resp, err := hc.Do(req)
	if err != nil {
		c.t.Fatal(err)
	}
	if out != nil {
		defer resp.Body.Close()
		if err := json.NewDecoder(resp.Body).Decode(out); err != nil {
			c.t.Fatalf("%s %s: decode: %v", method, path, err)
		}
	}
	return resp
}

// TestEndToEnd scans real (generated) files, matches them against a fake
// TMDB, and drives the API like an app would.
func TestEndToEnd(t *testing.T) {
	for _, bin := range []string{"ffmpeg", "ffprobe"} {
		if _, err := exec.LookPath(bin); err != nil {
			t.Skip(bin + " not installed")
		}
	}
	ctx := context.Background()
	dir := t.TempDir()
	moviesDir := filepath.Join(dir, "Movies")
	tvDir := filepath.Join(dir, "TV")
	makeClip(t, filepath.Join(moviesDir, "Night Shift (2023)", "Night Shift (2023).mkv"))
	makeClip(t, filepath.Join(tvDir, "Small Hours", "Season 1", "Small Hours - S01E01.mkv"))
	makeClip(t, filepath.Join(tvDir, "Small Hours", "Season 1", "Small Hours - S01E02.mkv"))
	makeClip(t, filepath.Join(moviesDir, "Night Shift (2023)", "Night.Shift.2023.sample.mkv"))

	st, err := store.Open(filepath.Join(dir, "kinora.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	m := &match.Matcher{Store: st, Src: fakeTMDB{}, Log: log}
	sc := &scanner.Scanner{Store: st, Prober: &probe.Prober{}, Matcher: m, Log: log, StableAge: time.Second}

	movies, err := st.CreateLibrary(ctx, "Movies", moviesDir, store.KindMovies)
	if err != nil {
		t.Fatal(err)
	}
	tv, err := st.CreateLibrary(ctx, "TV", tvDir, store.KindTV)
	if err != nil {
		t.Fatal(err)
	}
	for _, id := range []int64{movies.ID, tv.ID} {
		p, err := sc.Run(ctx, id)
		if err != nil {
			t.Fatalf("scan %d: %v", id, err)
		}
		if p.Errors != 0 {
			t.Fatalf("scan %d: %d errors", id, p.Errors)
		}
	}
	ms, _ := st.LibraryStats(ctx, movies.ID)
	if ms.Movies != 1 || ms.Files != 1 || ms.Unmatched != 0 || ms.Pending != 0 {
		t.Fatalf("movie stats %+v (sample should be skipped)", ms)
	}
	ts, _ := st.LibraryStats(ctx, tv.ID)
	if ts.Shows != 1 || ts.Episodes != 2 {
		t.Fatalf("tv stats %+v", ts)
	}

	// A second copy of the same movie merges into one item with two files.
	makeClip(t, filepath.Join(moviesDir, "Night.Shift.2023.2160p.mkv"))
	if _, err := sc.Run(ctx, movies.ID); err != nil {
		t.Fatal(err)
	}
	if ms, _ = st.LibraryStats(ctx, movies.ID); ms.Movies != 1 || ms.Files != 2 {
		t.Fatalf("after second copy: %+v", ms)
	}

	// Rescanning unchanged files changes nothing.
	p, err := sc.Run(ctx, movies.ID)
	if err != nil || p.New != 0 || p.Changed != 0 {
		t.Fatalf("rescan: %+v %v", p, err)
	}

	hash, _ := auth.HashPassword("password123")
	if _, err := st.CreateUser(ctx, "alex", hash, true); err != nil {
		t.Fatal(err)
	}
	srv := &Server{Store: st, Scanner: sc, Matcher: m, Images: images.New(filepath.Join(dir, "img")),
		Watch: &watch.Service{Store: st}, Log: log, Version: "test", BaseCtx: ctx}
	hs := httptest.NewServer(srv.Handler())
	defer hs.Close()
	c := &client{t: t, base: hs.URL + "/api/v1"}

	if resp := c.do("GET", "/home", nil, nil); resp.StatusCode != 401 {
		t.Fatalf("want 401 before sign-in, got %d", resp.StatusCode)
	}
	var login struct{ Token string }
	if resp := c.do("POST", "/auth/login", map[string]string{"username": "alex", "password": "wrong-pass"}, nil); resp.StatusCode != 401 {
		t.Fatalf("wrong password: %d", resp.StatusCode)
	}
	c.do("POST", "/auth/login", map[string]string{"username": "alex", "password": "password123", "deviceName": "test"}, &login)
	if login.Token == "" {
		t.Fatal("no token")
	}
	c.token = login.Token

	// Browse movies and open the movie.
	var list struct{ Items []ItemView }
	c.do("GET", "/items?kind=movie", nil, &list)
	if len(list.Items) != 1 || list.Items[0].Title != "Night Shift" || list.Items[0].MatchState != "matched" {
		t.Fatalf("movies: %+v", list.Items)
	}
	var detail struct {
		Item     ItemView
		Versions []VersionView
	}
	c.do("GET", "/items/"+list.Items[0].ID, nil, &detail)
	if len(detail.Versions) != 2 || detail.Versions[0].VideoCodec != "h264" || len(detail.Versions[0].Streams) != 2 {
		t.Fatalf("versions: %+v", detail.Versions)
	}

	// Direct play with a byte range, as players do when seeking.
	resp := c.do("GET", "/stream/"+detail.Versions[0].ID, nil, nil, "Range", "bytes=0-99")
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusPartialContent || len(body) != 100 {
		t.Fatalf("range: %d, %d bytes", resp.StatusCode, len(body))
	}
	// ?token= works for players that can't send headers.
	plain := &client{t: t, base: c.base}
	if resp := plain.do("GET", "/stream/"+detail.Versions[0].ID+"?token="+login.Token, nil, nil, "Range", "bytes=0-0"); resp.StatusCode != 206 {
		t.Fatalf("token query: %d", resp.StatusCode)
	}

	// Show, episodes, progress, next episode.
	c.do("GET", "/items?kind=show", nil, &list)
	if len(list.Items) != 1 || list.Items[0].Title != "Small Hours" {
		t.Fatalf("shows: %+v", list.Items)
	}
	showID := list.Items[0].ID
	var seasons struct{ Items []ItemView }
	c.do("GET", "/items/"+showID+"/children", nil, &seasons)
	if len(seasons.Items) != 1 {
		t.Fatalf("seasons: %+v", seasons.Items)
	}
	var episodes struct{ Items []ItemView }
	c.do("GET", "/items/"+seasons.Items[0].ID+"/children", nil, &episodes)
	if len(episodes.Items) != 2 || episodes.Items[0].Title != "Pilot" || episodes.Items[1].EpisodeNumber != 2 {
		t.Fatalf("episodes: %+v", episodes.Items)
	}
	var ws store.WatchState
	c.do("PUT", "/progress/"+episodes.Items[0].ID, map[string]any{"positionSec": 2.9}, &ws)
	if !ws.Completed || ws.PlayCount != 1 {
		t.Fatalf("progress past 92%% should complete: %+v", ws)
	}
	var home struct {
		ContinueWatching []continueView
		RecentlyAdded    []ItemView
	}
	c.do("GET", "/home", nil, &home)
	if len(home.ContinueWatching) != 1 || home.ContinueWatching[0].Item.EpisodeNumber != 2 ||
		!strings.Contains(home.ContinueWatching[0].Reason, "S01E01") {
		t.Fatalf("continue watching: %+v", home.ContinueWatching)
	}
	if len(home.RecentlyAdded) != 2 {
		t.Fatalf("recently added: %+v", home.RecentlyAdded)
	}
	var showDetail struct{ NextUp nextUpView }
	c.do("GET", "/items/"+showID, nil, &showDetail)
	if showDetail.NextUp.Episode == nil || showDetail.NextUp.Episode.Title != "Second Night" {
		t.Fatalf("next up: %+v", showDetail.NextUp)
	}

	// Cast is fetched during the scan; track choices stick to the show.
	var castDetail struct {
		Cast       []castView
		TrackPrefs store.TrackPrefs
	}
	c.do("GET", "/items/"+episodes.Items[1].ID, nil, &castDetail)
	if len(castDetail.Cast) != 1 || castDetail.Cast[0].Name != "Tom Reyes" || castDetail.TrackPrefs.SubtitleMode != "forced" {
		t.Fatalf("episode cast and prefs: %+v", castDetail)
	}
	c.do("PUT", "/items/"+episodes.Items[0].ID+"/prefs", map[string]string{"audioLang": "JPN", "subtitleMode": "always"}, nil)
	c.do("GET", "/items/"+episodes.Items[1].ID, nil, &castDetail)
	if castDetail.TrackPrefs.AudioLang != "jpn" || castDetail.TrackPrefs.SubtitleMode != "always" {
		t.Fatalf("prefs should carry to the next episode: %+v", castDetail.TrackPrefs)
	}

	// Search, filters and letters.
	c.do("GET", "/search?q=night", nil, &list)
	if len(list.Items) != 1 || list.Items[0].Title != "Night Shift" {
		t.Fatalf("search: %+v", list.Items)
	}
	c.do("GET", "/items?kind=show&watched=started", nil, &list)
	if len(list.Items) != 1 {
		t.Fatalf("started shows: %+v", list.Items)
	}
	var letters struct{ Letters []store.LetterOffset }
	c.do("GET", "/items/letters?kind=movie", nil, &letters)
	if len(letters.Letters) != 1 || letters.Letters[0].Letter != "N" {
		t.Fatalf("letters: %+v", letters.Letters)
	}

	// A second profile has its own history; a PIN guards switching to it.
	var kid store.Profile
	if resp := c.do("POST", "/profiles", map[string]any{"name": "Sam", "pin": "1234"}, &kid); resp.StatusCode != 201 || !kid.HasPin {
		t.Fatalf("create profile: %d %+v", resp.StatusCode, kid)
	}
	if resp := c.do("POST", "/profiles/"+itoa(kid.ID)+"/select", map[string]string{"pin": "0000"}, nil); resp.StatusCode != 403 {
		t.Fatalf("wrong PIN: %d", resp.StatusCode)
	}
	c.do("POST", "/profiles/"+itoa(kid.ID)+"/select", map[string]string{"pin": "1234"}, nil)
	c.do("GET", "/home", nil, &home)
	if len(home.ContinueWatching) != 0 {
		t.Fatalf("new profile sees someone else's history: %+v", home.ContinueWatching)
	}
	var profs struct {
		Profiles []store.Profile
		Current  int64
	}
	c.do("GET", "/profiles", nil, &profs)
	if len(profs.Profiles) != 2 || profs.Current != kid.ID {
		t.Fatalf("profiles: %+v", profs)
	}

	// The built-in page renders for a signed-in browser.
	req, _ := http.NewRequest("GET", hs.URL+"/ui/libraries/"+itoa(movies.ID), nil)
	req.AddCookie(&http.Cookie{Name: cookieName, Value: login.Token})
	page, err := http.DefaultClient.Do(req)
	if err != nil || page.StatusCode != 200 {
		t.Fatalf("web page: %v %d", err, page.StatusCode)
	}
	page.Body.Close()
}

func itoa(n int64) string { return strconv.FormatInt(n, 10) }
