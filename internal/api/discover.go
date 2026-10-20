package api

import (
	"context"
	"errors"
	"math"
	"net/http"
	"strconv"
	"sync"
	"time"

	"kinara/internal/store"
	"kinara/internal/tmdb"
)

// Titles from TMDB that may not be in the library (trending, later
// requests): their details, cast and seasons, so the apps can show a page
// for them. A title that is in the library comes with its item, and apps
// open the library page instead.

type externalTitleView struct {
	TMDBID      int      `json:"tmdbId"`
	Kind        string   `json:"kind"` // movie or show
	Title       string   `json:"title"`
	Year        int      `json:"year,omitempty"`
	Overview    string   `json:"overview"`
	Rating      float64  `json:"rating,omitempty"`
	Genres      []string `json:"genres"`
	RuntimeMin  int      `json:"runtimeMin,omitempty"`
	PosterURL   string   `json:"posterUrl,omitempty"`
	BackdropURL string   `json:"backdropUrl,omitempty"`
	// Status is a show's ("Returning Series", "Ended").
	Status  string              `json:"status,omitempty"`
	Cast    []externalCastView  `json:"cast"`
	Seasons []externalSeasonRef `json:"seasons,omitempty"`
	// Item is the library's copy, when there is one.
	Item *ItemView `json:"item,omitempty"`
	// Request is an open request for the title (requests.go).
	Request *requestMarker `json:"request,omitempty"`
	// OnTheWay is the newest news from the owner's tools about the title
	// being on its way (integrations).
	OnTheWay *titleStatusView `json:"onTheWay,omitempty"`
}

type externalCastView struct {
	Name     string `json:"name"`
	Role     string `json:"role,omitempty"`
	PhotoURL string `json:"photoUrl,omitempty"`
}

type externalSeasonRef struct {
	Number       int    `json:"number"`
	Name         string `json:"name"`
	EpisodeCount int    `json:"episodeCount"`
	Year         int    `json:"year,omitempty"`
	PosterURL    string `json:"posterUrl,omitempty"`
}

type externalSeasonView struct {
	Number   int                   `json:"number"`
	Name     string                `json:"name"`
	Overview string                `json:"overview,omitempty"`
	Episodes []externalEpisodeView `json:"episodes"`
}

type externalEpisodeView struct {
	Number     int     `json:"number"`
	Title      string  `json:"title"`
	Overview   string  `json:"overview,omitempty"`
	AirDate    string  `json:"airDate,omitempty"`
	RuntimeMin int     `json:"runtimeMin,omitempty"`
	Rating     float64 `json:"rating,omitempty"`
	StillURL   string  `json:"stillUrl,omitempty"`
}

// externalCast is how many cast members a title page shows.
const externalCast = 15

// externalTTL is how long TMDB answers are kept: titles change slowly, and
// the same few trending titles get opened by everyone.
const externalTTL = 6 * time.Hour

type externalCache struct {
	mu sync.Mutex
	m  map[string]externalEntry
}

type externalEntry struct {
	at time.Time
	v  any
}

// cached returns a remembered answer for key, or computes and keeps it.
func (c *externalCache) cached(key string, fetch func() (any, error)) (any, error) {
	return c.cachedFor(key, externalTTL, fetch)
}

// cachedFor is cached with answers kept for ttl.
func (c *externalCache) cachedFor(key string, ttl time.Duration, fetch func() (any, error)) (any, error) {
	c.mu.Lock()
	if c.m == nil {
		c.m = map[string]externalEntry{}
	}
	if e, ok := c.m[key]; ok && time.Since(e.at) < ttl {
		c.mu.Unlock()
		return e.v, nil
	}
	c.mu.Unlock()
	v, err := fetch()
	if err != nil {
		return nil, err
	}
	c.mu.Lock()
	// A small cache: past a few hundred titles, start over.
	if len(c.m) > 500 {
		c.m = map[string]externalEntry{}
	}
	c.m[key] = externalEntry{at: time.Now(), v: v}
	c.mu.Unlock()
	return v, nil
}

func tmdbStatus(err error) int {
	var se *tmdb.StatusError
	if errors.As(err, &se) && se.Code == http.StatusNotFound {
		return http.StatusNotFound
	}
	return http.StatusBadGateway
}

// externalTitle answers GET /api/v1/tmdb/{kind}/{id}: a movie or show from
// TMDB, with the library's copy when there is one.
func (s *Server) externalTitle(w http.ResponseWriter, r *http.Request, u store.User) {
	if s.TMDB == nil {
		writeErr(w, http.StatusNotImplemented, "set a TMDB key on the server to see titles that aren't in the library")
		return
	}
	kind := r.PathValue("kind")
	id, err := strconv.Atoi(r.PathValue("id"))
	if err != nil || id <= 0 || (kind != "movie" && kind != "show") {
		writeErr(w, http.StatusBadRequest, "expected /tmdb/movie/{id} or /tmdb/show/{id}")
		return
	}
	v, err := s.external.cached(kind+"/"+strconv.Itoa(id), func() (any, error) {
		return s.fetchExternalTitle(r.Context(), kind, id)
	})
	if err != nil {
		writeErr(w, tmdbStatus(err), "couldn't get that title from TMDB: "+err.Error())
		return
	}
	// A copy: the library match is per profile and per moment, not cached.
	out := v.(externalTitleView)
	provider := store.ProviderTMDBMovie
	if kind == "show" {
		provider = store.ProviderTMDBTV
	}
	if owned, err := s.Store.ItemsByExternalIDs(r.Context(), provider, []string{strconv.Itoa(id)}); err == nil {
		if it, ok := owned[strconv.Itoa(id)]; ok {
			if views, err := s.views(r.Context(), u.ProfileID, []store.Item{it}); err == nil && len(views) == 1 {
				out.Item = &views[0]
			}
			// It arrived: requests for it are done.
			s.markTitleAvailable(r.Context(), kind, id)
		}
	}
	s.releaseIfFailed(r.Context(), kind, id)
	if m := s.requestMarkers(r.Context(), u, kind, []int{id}); m[id] != nil {
		out.Request = m[id]
	}
	out.OnTheWay = s.titleStatus(kind, id, u)
	writeJSON(w, http.StatusOK, out)
}

func (s *Server) fetchExternalTitle(ctx context.Context, kind string, id int) (externalTitleView, error) {
	out := externalTitleView{TMDBID: id, Kind: kind, Genres: []string{}, Cast: []externalCastView{}}
	var cast []tmdb.CastMember
	if kind == "movie" {
		m, err := s.TMDB.Movie(ctx, id)
		if err != nil {
			return out, err
		}
		out.Title, out.Year, out.Overview = m.Title, tmdb.YearOf(m.ReleaseDate), m.Overview
		out.Rating = math.Round(m.VoteAverage*10) / 10
		out.Genres = tmdb.GenreNames(m.Genres)
		out.RuntimeMin = m.Runtime
		out.PosterURL, out.BackdropURL = tmdbImageURL(m.PosterPath), tmdbImageURL(m.BackdropPath)
		cast, _ = s.TMDB.MovieCast(ctx, id) // a page without cast is still a page
	} else {
		t, err := s.TMDB.TV(ctx, id)
		if err != nil {
			return out, err
		}
		out.Title, out.Year, out.Overview = t.Name, tmdb.YearOf(t.FirstAirDate), t.Overview
		out.Rating = math.Round(t.VoteAverage*10) / 10
		out.Genres = tmdb.GenreNames(t.Genres)
		if len(t.EpisodeRunTime) > 0 {
			out.RuntimeMin = t.EpisodeRunTime[0]
		}
		out.PosterURL, out.BackdropURL = tmdbImageURL(t.PosterPath), tmdbImageURL(t.BackdropPath)
		out.Status = t.Status
		out.Seasons = []externalSeasonRef{}
		for _, sn := range t.Seasons {
			if sn.EpisodeCount == 0 {
				continue
			}
			out.Seasons = append(out.Seasons, externalSeasonRef{
				Number: sn.SeasonNumber, Name: sn.Name, EpisodeCount: sn.EpisodeCount,
				Year: tmdb.YearOf(sn.AirDate), PosterURL: tmdbImageURL(sn.PosterPath),
			})
		}
		// Specials last, as on library shows.
		if len(out.Seasons) > 1 && out.Seasons[0].Number == 0 {
			out.Seasons = append(out.Seasons[1:], out.Seasons[0])
		}
		cast, _ = s.TMDB.TVCast(ctx, id)
	}
	for _, c := range cast {
		if len(out.Cast) == externalCast {
			break
		}
		out.Cast = append(out.Cast, externalCastView{Name: c.Name, Role: c.Character, PhotoURL: tmdbImageURL(c.ProfilePath)})
	}
	return out, nil
}

// externalSeason answers GET /api/v1/tmdb/show/{id}/season/{n}: a season's
// episodes from TMDB.
func (s *Server) externalSeason(w http.ResponseWriter, r *http.Request, u store.User) {
	if s.TMDB == nil {
		writeErr(w, http.StatusNotImplemented, "set a TMDB key on the server to see titles that aren't in the library")
		return
	}
	id, err1 := strconv.Atoi(r.PathValue("id"))
	n, err2 := strconv.Atoi(r.PathValue("n"))
	if err1 != nil || err2 != nil || id <= 0 || n < 0 {
		writeErr(w, http.StatusBadRequest, "expected /tmdb/show/{id}/season/{number}")
		return
	}
	v, err := s.external.cached("show/"+strconv.Itoa(id)+"/season/"+strconv.Itoa(n), func() (any, error) {
		sd, err := s.TMDB.Season(r.Context(), id, n)
		if err != nil {
			return nil, err
		}
		out := externalSeasonView{Number: sd.SeasonNumber, Name: sd.Name, Overview: sd.Overview, Episodes: []externalEpisodeView{}}
		for _, e := range sd.Episodes {
			out.Episodes = append(out.Episodes, externalEpisodeView{
				Number: e.EpisodeNumber, Title: e.Name, Overview: e.Overview, AirDate: e.AirDate,
				RuntimeMin: e.Runtime, Rating: math.Round(e.VoteAverage*10) / 10, StillURL: tmdbImageURL(e.StillPath),
			})
		}
		return out, nil
	})
	if err != nil {
		writeErr(w, tmdbStatus(err), "couldn't get that season from TMDB: "+err.Error())
		return
	}
	writeJSON(w, http.StatusOK, v)
}
