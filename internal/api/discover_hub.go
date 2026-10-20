package api

import (
	"context"
	"math"
	"math/rand/v2"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"kinara/internal/store"
	"kinara/internal/tmdb"
)

// The Discover page: TMDB's lists (trending, new, popular, classics...) in
// sections, by genre, and a search for titles outside the library. Titles
// the library has come with their item; titles someone asked for come with
// their request.

// hubTTL is how long the Discover page's sections are kept. The lists move
// daily at most, and one refresh an hour keeps TMDB calls low.
const hubTTL = time.Hour

// genreTTL is how long TMDB's genre lists are kept: they almost never change.
const genreTTL = 24 * time.Hour

// discoverSectionDef is one row of the Discover page and how to fetch it.
type discoverSectionDef struct {
	ID    string
	Title string
	fetch func(s *Server, ctx context.Context, page int) (tmdb.Page, error)
}

// classicsBefore is the last release date counted as a classic.
const classicsBefore = "1999-12-31"

func listFetch(media, list string) func(*Server, context.Context, int) (tmdb.Page, error) {
	return func(s *Server, ctx context.Context, page int) (tmdb.Page, error) {
		return s.TMDB.List(ctx, media, list, page)
	}
}

// discoverSections are the Discover page's rows, in order.
var discoverSections = []discoverSectionDef{
	{ID: "trending", Title: "Trending this week", fetch: func(s *Server, ctx context.Context, page int) (tmdb.Page, error) {
		return s.TMDB.TrendingPage(ctx, "all", "week", page)
	}},
	{ID: "new-movies", Title: "New in cinemas", fetch: listFetch("movie", tmdb.ListNowPlaying)},
	{ID: "new-episodes", Title: "New episodes this week", fetch: listFetch("tv", tmdb.ListOnTheAir)},
	{ID: "popular-movies", Title: "Popular movies", fetch: listFetch("movie", tmdb.ListPopular)},
	{ID: "popular-shows", Title: "Popular shows", fetch: listFetch("tv", tmdb.ListPopular)},
	{ID: "coming-soon", Title: "Coming soon", fetch: listFetch("movie", tmdb.ListUpcoming)},
	// Top rated films from before 2000. TMDB's top rated list can't be
	// limited by date, so this is discover sorted by rating, with enough
	// votes that the rating means something.
	{ID: "classics", Title: "Classics", fetch: func(s *Server, ctx context.Context, page int) (tmdb.Page, error) {
		return s.TMDB.Discover(ctx, "movie", tmdb.DiscoverOptions{SortBy: "vote_average.desc", ReleasedBefore: classicsBefore, MinVotes: 1000, Page: page})
	}},
	{ID: "acclaimed-shows", Title: "Acclaimed series", fetch: listFetch("tv", tmdb.ListTopRated)},
}

func findSection(id string) (discoverSectionDef, bool) {
	for _, d := range discoverSections {
		if d.ID == id {
			return d, true
		}
	}
	return discoverSectionDef{}, false
}

type hubCache struct {
	mu       sync.Mutex
	at       time.Time
	sections []hubSection
}

type hubSection struct {
	id, title string
	results   []tmdb.TrendingResult
}

type genreCache struct {
	mu        sync.Mutex
	at        time.Time
	movie, tv []tmdb.Genre
}

// genreView is a genre the Discover page can browse.
type genreView struct {
	ID   int    `json:"id"`
	Name string `json:"name"`
	Kind string `json:"kind"` // movie or show
}

// discoverSectionView is one row of the hub.
type discoverSectionView struct {
	ID    string         `json:"id"`
	Title string         `json:"title"`
	Items []trendingView `json:"items"`
}

// pagedTitles is one page of a section, genre or search.
type pagedTitles struct {
	Items      []trendingView `json:"items"`
	Page       int            `json:"page"`
	TotalPages int            `json:"totalPages"`
}

// genreLists returns TMDB's genres for movies and tv, kept for a day. A
// failure leaves titles without genre names rather than failing the page.
func (s *Server) genreLists(ctx context.Context) (movie, tv []tmdb.Genre) {
	c := &s.reqs.genres
	c.mu.Lock()
	defer c.mu.Unlock()
	if time.Since(c.at) < genreTTL && c.movie != nil {
		return c.movie, c.tv
	}
	m, err1 := s.TMDB.Genres(ctx, "movie")
	t, err2 := s.TMDB.Genres(ctx, "tv")
	if err1 != nil || err2 != nil {
		s.Log.Warn("couldn't get genre lists from TMDB", "movieErr", err1, "tvErr", err2)
		return m, t
	}
	c.at, c.movie, c.tv = time.Now(), m, t
	return m, t
}

func (s *Server) genreNames(ctx context.Context) map[int]string {
	movie, tv := s.genreLists(ctx)
	names := map[int]string{}
	for _, list := range [][]tmdb.Genre{movie, tv} {
		for _, g := range list {
			names[g.ID] = g.Name
		}
	}
	return names
}

// hubSections fetches (or reuses) the Discover page's rows. A row TMDB
// fails to send is left out; if every row fails, that's an error.
func (s *Server) hubSections(ctx context.Context) ([]hubSection, time.Time, error) {
	c := &s.reqs.hub
	c.mu.Lock()
	defer c.mu.Unlock()
	if time.Since(c.at) < hubTTL && c.sections != nil {
		return c.sections, c.at, nil
	}
	var out []hubSection
	var lastErr error
	for _, d := range discoverSections {
		page := 1
		if d.ID == "classics" {
			// A different handful of classics each hour.
			page = 1 + rand.IntN(5)
		}
		p, err := d.fetch(s, ctx, page)
		if err != nil {
			lastErr = err
			s.Log.Warn("couldn't get a discover row from TMDB", "section", d.ID, "err", err)
			continue
		}
		if d.ID == "classics" {
			rand.Shuffle(len(p.Results), func(i, j int) { p.Results[i], p.Results[j] = p.Results[j], p.Results[i] })
		}
		out = append(out, hubSection{id: d.ID, title: d.Title, results: p.Results})
	}
	if len(out) == 0 && lastErr != nil {
		return nil, time.Time{}, lastErr
	}
	c.at, c.sections = time.Now(), out
	return out, c.at, nil
}

// titleViews turns TMDB results into title items: people dropped, repeats
// dropped, genres named. ranked numbers them (trending lists).
func titleViews(results []tmdb.TrendingResult, genres map[int]string, ranked bool) []trendingView {
	out := make([]trendingView, 0, len(results))
	seen := map[string]bool{}
	for _, t := range results {
		if t.MediaType != "movie" && t.MediaType != "tv" {
			continue
		}
		key := t.MediaType + strconv.Itoa(t.ID)
		if seen[key] {
			continue
		}
		seen[key] = true
		v := trendingView{TMDBID: t.ID, Overview: t.Overview, Rating: math.Round(t.VoteAverage*10) / 10,
			PosterURL: tmdbImageURL(t.PosterPath), BackdropURL: tmdbImageURL(t.BackdropPath), Genres: []string{}}
		if t.MediaType == "movie" {
			v.Kind, v.Title, v.Year = "movie", t.Title, tmdb.YearOf(t.ReleaseDate)
		} else {
			v.Kind, v.Title, v.Year = "show", t.Name, tmdb.YearOf(t.FirstAirDate)
		}
		for _, g := range t.GenreIDs {
			if name := genres[g]; name != "" {
				v.Genres = append(v.Genres, name)
			}
		}
		if ranked {
			v.Rank = len(out) + 1
		}
		out = append(out, v)
	}
	return out
}

// markTitles fills in, in bulk, the library item for titles the library has
// and the request marker for titles someone asked for.
func (s *Server) markTitles(ctx context.Context, u store.User, lists ...[]trendingView) error {
	ids := map[string][]string{}
	for _, list := range lists {
		for _, v := range list {
			p := store.ProviderTMDBMovie
			if v.Kind == "show" {
				p = store.ProviderTMDBTV
			}
			ids[p] = append(ids[p], strconv.Itoa(v.TMDBID))
		}
	}
	owned := map[string]map[string]store.Item{}
	var ownedItems []store.Item
	for p, vals := range ids {
		m, err := s.Store.ItemsByExternalIDs(ctx, p, vals)
		if err != nil {
			return err
		}
		owned[p] = m
		for _, it := range m {
			ownedItems = append(ownedItems, it)
		}
	}
	views, err := s.views(ctx, u.ProfileID, ownedItems)
	if err != nil {
		return err
	}
	viewByID := map[string]*ItemView{}
	for i := range views {
		viewByID[views[i].ID] = &views[i]
	}
	for _, list := range lists {
		for i := range list {
			p := store.ProviderTMDBMovie
			if list[i].Kind == "show" {
				p = store.ProviderTMDBTV
			}
			if it, ok := owned[p][strconv.Itoa(list[i].TMDBID)]; ok {
				list[i].Item = viewByID[it.ID]
			}
		}
	}
	s.markRequests(ctx, u, lists...)
	return nil
}

func (s *Server) needTMDB(w http.ResponseWriter) bool {
	if s.TMDB == nil {
		writeErr(w, http.StatusNotImplemented, "set a TMDB key on the server to discover titles")
		return false
	}
	return true
}

// discoverHub answers GET /api/v1/discover: every row and the genres.
func (s *Server) discoverHub(w http.ResponseWriter, r *http.Request, u store.User) {
	if !s.needTMDB(w) {
		return
	}
	ctx := r.Context()
	sections, at, err := s.hubSections(ctx)
	if err != nil {
		writeErr(w, http.StatusBadGateway, "couldn't reach TMDB: "+err.Error())
		return
	}
	movieGenres, tvGenres := s.genreLists(ctx)
	names := s.genreNames(ctx)
	out := make([]discoverSectionView, 0, len(sections))
	lists := make([][]trendingView, 0, len(sections))
	for _, sec := range sections {
		items := titleViews(sec.results, names, sec.id == "trending")
		if len(items) == 0 {
			continue
		}
		out = append(out, discoverSectionView{ID: sec.id, Title: sec.title, Items: items})
		lists = append(lists, items)
	}
	if err := s.markTitles(ctx, u, lists...); err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	genres := []genreView{}
	for _, g := range movieGenres {
		genres = append(genres, genreView{ID: g.ID, Name: g.Name, Kind: "movie"})
	}
	for _, g := range tvGenres {
		genres = append(genres, genreView{ID: g.ID, Name: g.Name, Kind: "show"})
	}
	writeJSON(w, http.StatusOK, map[string]any{"sections": out, "genres": genres, "updatedAt": at.Unix()})
}

func pageFrom(r *http.Request) int {
	n, err := strconv.Atoi(r.URL.Query().Get("page"))
	if err != nil || n < 1 {
		return 1
	}
	return min(n, tmdb.MaxPages)
}

// pagedAnswer fetches one TMDB page through the hour-long cache and
// answers with it, marked for this viewer.
func (s *Server) pagedAnswer(w http.ResponseWriter, r *http.Request, u store.User, key string, ranked bool, fetch func(ctx context.Context) (tmdb.Page, error)) {
	ctx := r.Context()
	v, err := s.reqs.pages.cachedFor(key, hubTTL, func() (any, error) { return fetch(ctx) })
	if err != nil {
		writeErr(w, tmdbStatus(err), "couldn't reach TMDB: "+err.Error())
		return
	}
	p := v.(tmdb.Page)
	items := titleViews(p.Results, s.genreNames(ctx), ranked)
	if ranked && p.Page > 1 {
		// Ranks carry on from earlier pages.
		for i := range items {
			items[i].Rank += (p.Page - 1) * 20
		}
	}
	if err := s.markTitles(ctx, u, items); err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, pagedTitles{Items: items, Page: p.Page, TotalPages: p.TotalPages})
}

// discoverSection answers GET /api/v1/discover/{section}?page=N.
func (s *Server) discoverSection(w http.ResponseWriter, r *http.Request, u store.User) {
	if !s.needTMDB(w) {
		return
	}
	d, ok := findSection(r.PathValue("section"))
	if !ok {
		writeErr(w, http.StatusNotFound, "no such discover section")
		return
	}
	page := pageFrom(r)
	s.pagedAnswer(w, r, u, "section/"+d.ID+"/"+strconv.Itoa(page), d.ID == "trending", func(ctx context.Context) (tmdb.Page, error) {
		return d.fetch(s, ctx, page)
	})
}

// genreSorts maps the sort names the apps use to TMDB discover options.
var genreSorts = map[string]tmdb.DiscoverOptions{
	"popular": {SortBy: "popularity.desc"},
	// Enough votes that the rating means something.
	"rating": {SortBy: "vote_average.desc", MinVotes: 300},
	// Newest released first: nothing from the future, and not titles no
	// one has seen yet.
	"new": {SortBy: "release.desc", MinVotes: 20},
}

// discoverGenre answers GET /api/v1/discover/genre/{kind}/{genreId}.
func (s *Server) discoverGenre(w http.ResponseWriter, r *http.Request, u store.User) {
	if !s.needTMDB(w) {
		return
	}
	kind := r.PathValue("kind")
	genre, err := strconv.Atoi(r.PathValue("genreId"))
	if (kind != "movie" && kind != "show") || err != nil || genre <= 0 {
		writeErr(w, http.StatusBadRequest, "expected /discover/genre/movie/{id} or /discover/genre/show/{id}")
		return
	}
	sort := r.URL.Query().Get("sort")
	if sort == "" {
		sort = "popular"
	}
	opts, ok := genreSorts[sort]
	if !ok {
		writeErr(w, http.StatusBadRequest, "sort is popular, rating or new")
		return
	}
	media := "movie"
	if kind == "show" {
		media = "tv"
	}
	if opts.SortBy == "release.desc" {
		opts.SortBy = "primary_release_date.desc"
		if media == "tv" {
			opts.SortBy = "first_air_date.desc"
		}
		opts.ReleasedBefore = time.Now().Format("2006-01-02")
	}
	opts.GenreID, opts.Page = genre, pageFrom(r)
	key := "genre/" + media + "/" + strconv.Itoa(genre) + "/" + sort + "/" + strconv.Itoa(opts.Page)
	s.pagedAnswer(w, r, u, key, false, func(ctx context.Context) (tmdb.Page, error) {
		return s.TMDB.Discover(ctx, media, opts)
	})
}

// searchExternal answers GET /api/v1/search/external?q=: movies and shows
// on TMDB, whether or not the library has them.
func (s *Server) searchExternal(w http.ResponseWriter, r *http.Request, u store.User) {
	if !s.needTMDB(w) {
		return
	}
	q := strings.TrimSpace(r.URL.Query().Get("q"))
	if q == "" {
		writeErr(w, http.StatusBadRequest, "type something to search for")
		return
	}
	if len(q) > 200 {
		q = q[:200]
	}
	page := pageFrom(r)
	key := "search/" + strings.ToLower(q) + "/" + strconv.Itoa(page)
	s.pagedAnswer(w, r, u, key, false, func(ctx context.Context) (tmdb.Page, error) {
		return s.TMDB.SearchMulti(ctx, q, page)
	})
}
