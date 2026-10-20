//go:build !nosqlite

package api

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"sync"
	"testing"

	"kinara/internal/auth"
	"kinara/internal/images"
	"kinara/internal/probe"
	"kinara/internal/scanner"
	"kinara/internal/store"
	"kinara/internal/tmdb"
	"kinara/internal/watch"
)

// discoverTMDB is a fake TMDB for requests and discovery: movie 1 (also in
// the library), movie 3 and 5 (not), show 2 with seasons 1 and 2. queries
// keeps the last query string asked of each path.
type discoverTMDB struct {
	mu      sync.Mutex
	queries map[string]url.Values
}

func (f *discoverTMDB) query(path string) url.Values {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.queries[path]
}

func newDiscoverTMDB(t *testing.T) (*discoverTMDB, *httptest.Server) {
	t.Helper()
	f := &discoverTMDB{queries: map[string]url.Values{}}
	movies := `{"page":1,"total_pages":3,"results":[
		{"id":1,"title":"Masterplan","release_date":"2026-09-01","vote_average":7.46,"poster_path":"/pMaster1.jpg","genre_ids":[35]},
		{"id":3,"title":"Far Shore","release_date":"2026-10-01","vote_average":6.9,"genre_ids":[18]}]}`
	shows := `{"page":1,"total_pages":2,"results":[{"id":2,"name":"Northern Lights","first_air_date":"2024-01-05","vote_average":8.2,"genre_ids":[10765]}]}`
	pages := map[string]string{
		"/movie/1":              `{"id":1,"title":"Masterplan","release_date":"2026-09-01","poster_path":"/pMaster1.jpg","genres":[]}`,
		"/movie/3":              `{"id":3,"title":"Far Shore","release_date":"2026-10-01","poster_path":"/pFarShore.jpg","genres":[]}`,
		"/movie/5":              `{"id":5,"title":"Quiet Hours","release_date":"2025-02-01","genres":[]}`,
		"/movie/3/external_ids": `{"imdb_id":"tt0000003","tvdb_id":null}`,
		"/tv/2":                 `{"id":2,"name":"Northern Lights","first_air_date":"2024-01-05","genres":[],"seasons":[{"season_number":1,"name":"Season 1","episode_count":8},{"season_number":2,"name":"Season 2","episode_count":8}]}`,
		"/tv/2/external_ids":    `{"imdb_id":"tt0944947","tvdb_id":121361}`,
		"/trending/all/week": `{"page":1,"total_pages":10,"results":[
			{"id":2,"media_type":"tv","name":"Northern Lights","first_air_date":"2024-01-05"},
			{"id":9,"media_type":"person","name":"Someone"},
			{"id":3,"media_type":"movie","title":"Far Shore","release_date":"2026-10-01"}]}`,
		"/movie/now_playing": movies,
		"/movie/popular":     movies,
		"/discover/movie":    movies,
		"/tv/on_the_air":     shows,
		"/tv/popular":        shows,
		"/tv/top_rated":      shows,
		"/discover/tv":       shows,
		// /movie/upcoming is missing: that row is left out.
		"/genre/movie/list": `{"genres":[{"id":35,"name":"Comedy"},{"id":18,"name":"Drama"}]}`,
		"/genre/tv/list":    `{"genres":[{"id":10765,"name":"Sci-Fi & Fantasy"}]}`,
		"/search/multi": `{"page":1,"total_pages":1,"results":[
			{"id":1,"media_type":"movie","title":"Masterplan","release_date":"2026-09-01"},
			{"id":7,"media_type":"person","name":"Ann Lee"},
			{"id":2,"media_type":"tv","name":"Northern Lights","first_air_date":"2024-01-05"}]}`,
	}
	hs := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		f.queries[r.URL.Path] = r.URL.Query()
		f.mu.Unlock()
		body, ok := pages[r.URL.Path]
		if !ok {
			w.WriteHeader(http.StatusNotFound)
			_, _ = w.Write([]byte(`{"status_message":"not found"}`))
			return
		}
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(hs.Close)
	return f, hs
}

// requestServer is a server with an owner (alex), a member (sam), a movie
// library holding TMDB movie 1, and the fake TMDB.
func requestServer(t *testing.T) (*store.Store, store.Library, *discoverTMDB, func(user string, device map[string]string) *client) {
	t.Helper()
	ctx := context.Background()
	dir := t.TempDir()
	st, err := store.Open(filepath.Join(dir, "kinara.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	hash, _ := auth.HashPassword("password123")
	if _, err := st.CreateUser(ctx, "alex", hash, true); err != nil {
		t.Fatal(err)
	}
	if _, err := st.CreateUser(ctx, "sam", hash, false); err != nil {
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
	fake, fakeHS := newDiscoverTMDB(t)
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	srv := &Server{Store: st, Scanner: &scanner.Scanner{Store: st, Prober: &probe.Prober{}, Log: log},
		Images: images.New(filepath.Join(dir, "img")), Watch: &watch.Service{Store: st}, Log: log, Version: "test", BaseCtx: ctx,
		TMDB: &tmdb.Client{Key: "k", BaseURL: fakeHS.URL, HTTP: fakeHS.Client()}}
	hs := httptest.NewServer(srv.Handler())
	t.Cleanup(hs.Close)
	signIn := func(user string, device map[string]string) *client {
		c := &client{t: t, base: hs.URL + "/api/v1"}
		body := map[string]any{"username": user, "password": "password123"}
		if device != nil {
			body["device"] = device
		}
		var login struct{ Token string }
		if resp := c.do("POST", "/auth/login", body, &login); resp.StatusCode != 200 {
			t.Fatalf("sign in %s: %d", user, resp.StatusCode)
		}
		c.token = login.Token
		return c
	}
	return st, lib, fake, signIn
}

var samPhone = map[string]string{"id": "sam-phone-0001", "name": "Sam's Fold", "platform": "android",
	"brand": "samsung", "model": "SM-F956U1", "os": "Android 15", "app": "Kinara 0.9.4", "extraField": "ignored"}

type requestResp struct {
	requestView
	Sent      bool
	Duplicate bool
	OnTheWay  bool
}

func TestDevicesIdentity(t *testing.T) {
	_, _, _, signIn := requestServer(t)
	owner := signIn("alex", map[string]string{"id": "living-room-shield", "name": "Living room TV",
		"platform": "android-tv", "brand": "NVIDIA", "model": "SHIELD Android TV", "os": "Android 11"})
	sam := signIn("sam", samPhone)
	oldApp := signIn("sam", nil) // an app that sends no device

	var devices []store.Device
	owner.do("GET", "/devices", nil, &devices)
	if len(devices) != 3 {
		t.Fatalf("devices: %+v", devices)
	}
	byName := map[string]store.Device{}
	for _, d := range devices {
		byName[d.Name] = d
	}
	tv := byName["Living room TV"]
	if tv.ClientID != "living-room-shield" || tv.Icon != "shield" || tv.Label != "NVIDIA SHIELD Android TV · Android 11" || !tv.Current || tv.Trusted {
		t.Fatalf("tv: %+v", tv)
	}
	phone := byName["Sam's Fold"]
	if phone.ClientID != "sam-phone-0001" || phone.Icon != "android-phone" || phone.Label != "Samsung SM-F956U1 · Android 15" || phone.App != "Kinara 0.9.4" {
		t.Fatalf("phone: %+v", phone)
	}
	var derived store.Device
	for _, d := range devices {
		if d.ClientID != "living-room-shield" && d.ClientID != "sam-phone-0001" {
			derived = d
		}
	}
	if derived.ClientID != "s-"+derived.ID {
		t.Fatalf("a session without a client gets one from its session: %+v", derived)
	}

	// Trust and rename; only owners may.
	if resp := sam.do("PUT", "/clients/sam-phone-0001", map[string]any{"trusted": true}, nil); resp.StatusCode != 403 {
		t.Fatalf("member trusting: %d", resp.StatusCode)
	}
	var got store.Device
	if resp := owner.do("PUT", "/clients/sam-phone-0001", map[string]any{"trusted": true, "name": "Sam's phone"}, &got); resp.StatusCode != 200 ||
		!got.Trusted || got.Name != "Sam's phone" || got.Icon != "android-phone" {
		t.Fatalf("trust: %d %+v", resp.StatusCode, got)
	}
	if resp := owner.do("PUT", "/clients/"+derived.ClientID, map[string]any{"trusted": true}, &got); resp.StatusCode != 200 || !got.Trusted || got.ClientID != derived.ClientID {
		t.Fatalf("trust a derived client: %d %+v", resp.StatusCode, got)
	}
	if resp := owner.do("PUT", "/clients/no-such-client", map[string]any{"trusted": true}, nil); resp.StatusCode != 404 {
		t.Fatalf("unknown client: %d", resp.StatusCode)
	}

	// Signing out and back in on the same install keeps its trust.
	sam.do("POST", "/auth/logout", nil, nil)
	again := map[string]string{}
	for k, v := range samPhone {
		again[k] = v
	}
	again["name"] = "" // the owner's name stays when the app sends none
	signIn("sam", again)
	owner.do("GET", "/devices", nil, &devices)
	found := false
	for _, d := range devices {
		if d.ClientID == "sam-phone-0001" {
			found = true
			if !d.Trusted || d.Name != "Sam's phone" {
				t.Fatalf("after signing in again: %+v", d)
			}
		}
	}
	if !found {
		t.Fatal("the phone is missing after signing in again")
	}

	// An app signed in before it sent a device describes itself when it starts.
	tvBox := map[string]string{"id": "bedroom-roku-01", "name": "Bedroom", "platform": "roku", "brand": "TCL", "model": "Roku TV 55S455"}
	if resp := oldApp.do("PUT", "/me/device", tvBox, nil); resp.StatusCode != 204 {
		t.Fatalf("report device: %d", resp.StatusCode)
	}
	if resp := oldApp.do("PUT", "/me/device", map[string]string{"id": "x"}, nil); resp.StatusCode != 400 {
		t.Fatalf("a bad device id: %d", resp.StatusCode)
	}
	// A name the owner gave stays when the app reports its own again.
	if resp := sam.do("PUT", "/me/device", samPhone, nil); resp.StatusCode != 401 {
		t.Fatalf("signed out session: %d", resp.StatusCode)
	}
	samAgain := signIn("sam", nil)
	samAgain.do("PUT", "/me/device", samPhone, nil)
	owner.do("GET", "/devices", nil, &devices)
	names := map[string]store.Device{}
	for _, d := range devices {
		names[d.ClientID] = d
	}
	// It was trusted under the client made from its session: that carries over.
	if d := names["bedroom-roku-01"]; d.Name != "Bedroom" || d.Icon != "roku" || d.Label != "Roku · TCL TV 55S455" || !d.Trusted {
		t.Fatalf("reported device: %+v", d)
	}
	if d := names["sam-phone-0001"]; d.Name != "Sam's phone" || !d.Trusted {
		t.Fatalf("owner's name kept: %+v", d)
	}
}

func TestRequestsFlow(t *testing.T) {
	st, lib, _, signIn := requestServer(t)
	owner := signIn("alex", nil)
	sam := signIn("sam", samPhone)

	// A pending request.
	var made requestResp
	if resp := sam.do("POST", "/requests", map[string]any{"kind": "movie", "tmdbId": 3, "note": "please"}, &made); resp.StatusCode != 201 {
		t.Fatalf("create: %d", resp.StatusCode)
	}
	if made.Status != "pending" || made.Sent || made.Auto || made.Title != "Far Shore" || made.Year != 2026 ||
		made.PosterURL != "/api/v1/tmdb-image/pFarShore.jpg" || made.RequestedBy.Name != "sam" || made.Note != "please" ||
		made.Device.ID != "sam-phone-0001" || made.Device.Icon != "android-phone" || made.Device.Name != "Sam's Fold" || made.Mode != "all" {
		t.Fatalf("made: %+v", made)
	}
	stored, _ := st.GetRequest(context.Background(), made.ID)
	if stored.IMDBID != "tt0000003" {
		t.Fatalf("IMDb ID from TMDB: %+v", stored)
	}

	// Asking again returns the same request.
	var dup requestResp
	if resp := sam.do("POST", "/requests", map[string]any{"kind": "movie", "tmdbId": 3}, &dup); resp.StatusCode != 200 || !dup.Duplicate || dup.ID != made.ID {
		t.Fatalf("duplicate: %d %+v", resp.StatusCode, dup)
	}
	// Already on the server.
	if resp := sam.do("POST", "/requests", map[string]any{"kind": "movie", "tmdbId": 1}, nil); resp.StatusCode != 409 {
		t.Fatalf("owned: %d", resp.StatusCode)
	}
	// Seasons: sorted; one that doesn't exist is refused.
	var seasons requestResp
	sam.do("POST", "/requests", map[string]any{"kind": "show", "tmdbId": 2, "mode": "seasons", "seasons": []int{2, 1}}, &seasons)
	if seasons.Mode != "seasons" || len(seasons.Seasons) != 2 || seasons.Seasons[0] != 1 {
		t.Fatalf("seasons: %+v", seasons)
	}
	if resp := sam.do("POST", "/requests", map[string]any{"kind": "show", "tmdbId": 2, "mode": "seasons", "seasons": []int{9}}, nil); resp.StatusCode != 400 {
		t.Fatalf("no such season: %d", resp.StatusCode)
	}
	for _, bad := range []map[string]any{{"kind": "person", "tmdbId": 1}, {"kind": "show", "tmdbId": 2, "mode": "some"}, {"kind": "show", "tmdbId": 2, "mode": "seasons"}} {
		if resp := sam.do("POST", "/requests", bad, nil); resp.StatusCode != 400 {
			t.Fatalf("bad %v: %d", bad, resp.StatusCode)
		}
	}
	// The owner's own request.
	var mine requestResp
	owner.do("POST", "/requests", map[string]any{"kind": "show", "tmdbId": 2, "mode": "pilot"}, &mine)

	// Listing: members see their own, owners everyone's.
	var list struct {
		Items   []requestView
		Pending int
	}
	sam.do("GET", "/requests", nil, &list)
	if len(list.Items) != 2 || list.Pending != 2 || list.Items[0].ID != seasons.ID {
		t.Fatalf("sam's list: %+v", list)
	}
	owner.do("GET", "/requests?status=pending", nil, &list)
	if len(list.Items) != 3 || list.Pending != 3 {
		t.Fatalf("owner's list: %+v", list)
	}
	if resp := owner.do("GET", "/requests?status=maybe", nil, nil); resp.StatusCode != 400 {
		t.Fatalf("bad status: %d", resp.StatusCode)
	}

	// Markers on the title page.
	var page externalTitleView
	sam.do("GET", "/tmdb/movie/3", nil, &page)
	if page.Request == nil || page.Request.ID != made.ID || !page.Request.Mine || page.Request.Status != "pending" {
		t.Fatalf("marker for sam: %+v", page.Request)
	}
	page = externalTitleView{}
	owner.do("GET", "/tmdb/movie/3", nil, &page)
	if page.Request == nil || page.Request.Mine {
		t.Fatalf("marker for the owner: %+v", page.Request)
	}
	// Two open requests for show 2: each viewer sees their own.
	owner.do("GET", "/tmdb/show/2", nil, &page)
	if page.Request == nil || page.Request.ID != mine.ID || page.Request.Mode != "pilot" {
		t.Fatalf("owner's show marker: %+v", page.Request)
	}

	// Deciding.
	if resp := sam.do("POST", "/requests/"+itoa(made.ID)+"/approve", nil, nil); resp.StatusCode != 403 {
		t.Fatalf("member approving: %d", resp.StatusCode)
	}
	var decided requestView
	if resp := owner.do("POST", "/requests/"+itoa(made.ID)+"/approve", nil, &decided); resp.StatusCode != 200 || decided.Status != "approved" || decided.DecidedAt == 0 {
		t.Fatalf("approve: %d %+v", resp.StatusCode, decided)
	}
	if resp := owner.do("POST", "/requests/"+itoa(made.ID)+"/approve", nil, nil); resp.StatusCode != 409 {
		t.Fatalf("approve twice: %d", resp.StatusCode)
	}
	if resp := owner.do("POST", "/requests/"+itoa(mine.ID)+"/decline", map[string]any{"reason": "not this year"}, &decided); resp.StatusCode != 200 ||
		decided.Status != "declined" || decided.DeclineReason != "not this year" {
		t.Fatalf("decline: %d %+v", resp.StatusCode, decided)
	}
	if resp := owner.do("POST", "/requests/999/approve", nil, nil); resp.StatusCode != 404 {
		t.Fatalf("approve missing: %d", resp.StatusCode)
	}

	// Cancelling: only your own, only while pending; owners remove any.
	if resp := sam.do("DELETE", "/requests/"+itoa(made.ID), nil, nil); resp.StatusCode != 409 {
		t.Fatalf("cancel approved: %d", resp.StatusCode)
	}
	if resp := sam.do("DELETE", "/requests/"+itoa(mine.ID), nil, nil); resp.StatusCode != 404 {
		t.Fatalf("cancel someone else's: %d", resp.StatusCode)
	}
	if resp := sam.do("DELETE", "/requests/"+itoa(seasons.ID), nil, nil); resp.StatusCode != 204 {
		t.Fatalf("cancel own pending: %d", resp.StatusCode)
	}
	if resp := owner.do("DELETE", "/requests/"+itoa(mine.ID), nil, nil); resp.StatusCode != 204 {
		t.Fatalf("owner removes: %d", resp.StatusCode)
	}

	// A trusted device's requests are approved straight away.
	owner.do("PUT", "/clients/sam-phone-0001", map[string]any{"trusted": true}, nil)
	var auto requestResp
	if resp := sam.do("POST", "/requests", map[string]any{"kind": "movie", "tmdbId": 5}, &auto); resp.StatusCode != 201 ||
		auto.Status != "approved" || !auto.Auto || !auto.Sent || auto.DecidedAt == 0 {
		t.Fatalf("trusted: %d %+v", resp.StatusCode, auto)
	}

	// The approved movie arrives: its request becomes available.
	arrived := addMovie(t, st, lib, "Far Shore")
	if err := st.SetExternalID(context.Background(), arrived, store.ProviderTMDBMovie, "3"); err != nil {
		t.Fatal(err)
	}
	sam.do("GET", "/requests?status=available", nil, &list)
	if len(list.Items) != 1 || list.Items[0].ID != made.ID {
		t.Fatalf("available: %+v", list)
	}
	// And it no longer carries a marker.
	page = externalTitleView{}
	sam.do("GET", "/tmdb/movie/3", nil, &page)
	if page.Request != nil || page.Item == nil {
		t.Fatalf("arrived title: %+v %+v", page.Request, page.Item)
	}
}

func TestDiscover(t *testing.T) {
	_, _, fake, signIn := requestServer(t)
	sam := signIn("sam", samPhone)
	var req requestResp
	sam.do("POST", "/requests", map[string]any{"kind": "movie", "tmdbId": 3}, &req)

	var hub struct {
		Sections []struct {
			ID, Title string
			Items     []trendingView
		}
		Genres    []genreView
		UpdatedAt int64
	}
	if resp := sam.do("GET", "/discover", nil, &hub); resp.StatusCode != 200 {
		t.Fatalf("hub: %d", resp.StatusCode)
	}
	ids := []string{}
	for _, s := range hub.Sections {
		ids = append(ids, s.ID)
	}
	want := []string{"trending", "new-movies", "new-episodes", "popular-movies", "popular-shows", "classics", "acclaimed-shows"}
	if len(ids) != len(want) {
		t.Fatalf("sections %v, want %v (coming-soon failed and is left out)", ids, want)
	}
	for i := range want {
		if ids[i] != want[i] {
			t.Fatalf("sections %v, want %v", ids, want)
		}
	}
	trending := hub.Sections[0].Items
	if len(trending) != 2 || trending[0].Kind != "show" || trending[0].Rank != 1 || trending[1].Request == nil || !trending[1].Request.Mine {
		t.Fatalf("trending: %+v", trending)
	}
	newMovies := hub.Sections[1].Items
	if newMovies[0].TMDBID != 1 || newMovies[0].Item == nil || newMovies[0].Genres[0] != "Comedy" || newMovies[0].Rank != 0 || newMovies[1].Request == nil {
		t.Fatalf("new movies: %+v", newMovies)
	}
	if len(hub.Genres) != 3 || hub.Genres[2].Kind != "show" || hub.Genres[2].Name != "Sci-Fi & Fantasy" || hub.UpdatedAt == 0 {
		t.Fatalf("genres: %+v", hub.Genres)
	}
	q := fake.query("/discover/movie")
	if q.Get("primary_release_date.lte") != "1999-12-31" || q.Get("sort_by") != "vote_average.desc" {
		t.Fatalf("classics query: %v", q)
	}

	// One section, paged.
	var paged pagedTitles
	if resp := sam.do("GET", "/discover/popular-shows?page=2", nil, &paged); resp.StatusCode != 200 || paged.TotalPages != 2 || len(paged.Items) != 1 {
		t.Fatalf("section: %d %+v", resp.StatusCode, paged)
	}
	if fake.query("/tv/popular").Get("page") != "2" {
		t.Fatalf("page not passed: %v", fake.query("/tv/popular"))
	}
	if resp := sam.do("GET", "/discover/nothing", nil, nil); resp.StatusCode != 404 {
		t.Fatalf("unknown section: %d", resp.StatusCode)
	}

	// A genre.
	if resp := sam.do("GET", "/discover/genre/show/10765?sort=rating", nil, &paged); resp.StatusCode != 200 || len(paged.Items) != 1 || paged.Items[0].Kind != "show" {
		t.Fatalf("genre: %d %+v", resp.StatusCode, paged)
	}
	if q := fake.query("/discover/tv"); q.Get("with_genres") != "10765" || q.Get("sort_by") != "vote_average.desc" {
		t.Fatalf("genre query: %v", q)
	}
	sam.do("GET", "/discover/genre/movie/18?sort=new", nil, &paged)
	if q := fake.query("/discover/movie"); q.Get("sort_by") != "primary_release_date.desc" || q.Get("primary_release_date.lte") == "" {
		t.Fatalf("new query: %v", q)
	}
	if resp := sam.do("GET", "/discover/genre/movie/18?sort=odd", nil, nil); resp.StatusCode != 400 {
		t.Fatalf("bad sort: %d", resp.StatusCode)
	}
	if resp := sam.do("GET", "/discover/genre/person/18", nil, nil); resp.StatusCode != 400 {
		t.Fatalf("bad kind: %d", resp.StatusCode)
	}

	// Search outside the library: people dropped, owned titles marked.
	if resp := sam.do("GET", "/search/external?q=master", nil, &paged); resp.StatusCode != 200 || len(paged.Items) != 2 || paged.Items[0].Item == nil || paged.Items[1].Kind != "show" {
		t.Fatalf("search: %d %+v", resp.StatusCode, paged)
	}
	if fake.query("/search/multi").Get("query") != "master" {
		t.Fatalf("search query: %v", fake.query("/search/multi"))
	}
	if resp := sam.do("GET", "/search/external?q=", nil, nil); resp.StatusCode != 400 {
		t.Fatalf("empty search: %d", resp.StatusCode)
	}
}

func TestRequestAlreadyOnTheWay(t *testing.T) {
	_, _, _, signIn := requestServer(t)
	owner := signIn("alex", nil)
	sam := signIn("sam", samPhone)

	// The owner's tools report Far Shore and Northern Lights downloading.
	for _, body := range []map[string]any{
		{"tmdbId": 3, "kind": "movie", "state": "in_progress", "progress": 0.4},
		{"tmdbId": 2, "kind": "show", "state": "in_progress", "progress": 0.1},
	} {
		if resp := owner.do("POST", "/status", body, nil); resp.StatusCode != 202 && resp.StatusCode != 200 {
			t.Fatalf("status: %d", resp.StatusCode)
		}
	}

	// Cards and the title page say so.
	var hub struct {
		Sections []struct {
			ID    string         `json:"id"`
			Items []trendingView `json:"items"`
		} `json:"sections"`
	}
	sam.do("GET", "/discover", nil, &hub)
	found := false
	for _, sec := range hub.Sections {
		for _, it := range sec.Items {
			if it.Kind == "movie" && it.TMDBID == 3 {
				found = true
				if it.OnTheWay == nil || it.OnTheWay.State != "in_progress" || it.OnTheWay.Progress == nil || *it.OnTheWay.Progress != 0.4 {
					t.Fatalf("card: %+v", it.OnTheWay)
				}
			}
		}
	}
	if !found {
		t.Fatal("Far Shore isn't in the hub")
	}
	var page externalTitleView
	sam.do("GET", "/tmdb/movie/3", nil, &page)
	if page.OnTheWay == nil || page.OnTheWay.State != "in_progress" {
		t.Fatalf("page: %+v", page.OnTheWay)
	}

	// Asking for it anyway: approved on the spot, and nothing goes out.
	var made requestResp
	if resp := sam.do("POST", "/requests", map[string]any{"kind": "movie", "tmdbId": 3}, &made); resp.StatusCode != 201 {
		t.Fatalf("create: %d", resp.StatusCode)
	}
	if made.Status != "approved" || made.Sent || made.Auto || !made.OnTheWay {
		t.Fatalf("on the way: %+v", made)
	}
	// Only some seasons of a show that's on its way: the tools may be fetching
	// others, so it goes to the owner as usual.
	made = requestResp{}
	if resp := sam.do("POST", "/requests", map[string]any{"kind": "show", "tmdbId": 2, "mode": "seasons", "seasons": []int{2}}, &made); resp.StatusCode != 201 || made.Status != "pending" || made.OnTheWay {
		t.Fatalf("some seasons: %d %+v", resp.StatusCode, made)
	}
	// The whole show is covered by the download.
	made = requestResp{}
	if resp := sam.do("POST", "/requests", map[string]any{"kind": "show", "tmdbId": 2, "mode": "all"}, &made); resp.StatusCode != 201 || made.Status != "approved" {
		t.Fatalf("whole show: %d %+v", resp.StatusCode, made)
	}

	// Removing the movie's request clears its "on the way" too.
	var list struct{ Items []requestView }
	owner.do("GET", "/requests?status=approved", nil, &list)
	for _, rq := range list.Items {
		if rq.Kind == "movie" && rq.TMDBID == 3 {
			if resp := owner.do("DELETE", fmt.Sprintf("/requests/%d", rq.ID), nil, nil); resp.StatusCode != 204 {
				t.Fatalf("delete: %d", resp.StatusCode)
			}
		}
	}
	page = externalTitleView{}
	sam.do("GET", "/tmdb/movie/3", nil, &page)
	if page.OnTheWay != nil {
		t.Fatalf("still on the way after its request was removed: %+v", page.OnTheWay)
	}
	// The show still has open requests: its status stays.
	page = externalTitleView{}
	sam.do("GET", "/tmdb/show/2", nil, &page)
	if page.OnTheWay == nil {
		t.Fatal("the show's status went with someone else's request")
	}

	// The show's download fails: its requests close, and it can be asked for again.
	owner.do("POST", "/status", map[string]any{"tmdbId": 2, "kind": "show", "state": "failed", "message": "Removed from the download queue"}, nil)
	page = externalTitleView{}
	sam.do("GET", "/tmdb/show/2", nil, &page)
	if page.Request != nil || page.OnTheWay == nil || page.OnTheWay.State != "failed" {
		t.Fatalf("after a failure: request %+v, status %+v", page.Request, page.OnTheWay)
	}
	var declined struct{ Items []requestView }
	owner.do("GET", "/requests?status=declined", nil, &declined)
	if len(declined.Items) != 2 || declined.Items[0].DeclineReason != "The download failed: Removed from the download queue" {
		t.Fatalf("declined: %+v", declined.Items)
	}
	made = requestResp{}
	if resp := sam.do("POST", "/requests", map[string]any{"kind": "show", "tmdbId": 2, "mode": "all"}, &made); resp.StatusCode != 201 || made.Duplicate || made.OnTheWay {
		t.Fatalf("asking again: %d %+v", resp.StatusCode, made)
	}
}
