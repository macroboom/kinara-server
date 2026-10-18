// Package tmdb is a small client for The Movie Database API (v3).
//
// The base URL is configurable so the server can later go through a Kinora
// Cloud proxy that holds one commercial key and caches responses.
package tmdb

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"
)

const (
	DefaultBaseURL  = "https://api.themoviedb.org/3"
	DefaultImageURL = "https://image.tmdb.org/t/p/original"
)

// Client talks to TMDB. Key is either a v3 API key or a v4 read access
// token (the long one starting "eyJ").
type Client struct {
	Key      string
	BaseURL  string
	ImageURL string
	Language string
	HTTP     *http.Client

	mu   sync.Mutex
	last time.Time
}

// New returns a client with sensible defaults.
func New(key string) *Client {
	return &Client{
		Key:      key,
		BaseURL:  DefaultBaseURL,
		ImageURL: DefaultImageURL,
		Language: "en-US",
		HTTP:     &http.Client{Timeout: 20 * time.Second},
	}
}

// Image returns the full URL for an image path such as "/abc.jpg".
func (c *Client) Image(path string) string {
	if path == "" {
		return ""
	}
	return strings.TrimRight(c.ImageURL, "/") + path
}

// minGap keeps requests under TMDB's rate limit (about 50 per second).
const minGap = 25 * time.Millisecond

func (c *Client) wait(ctx context.Context) error {
	c.mu.Lock()
	next := c.last.Add(minGap)
	nowT := time.Now()
	if next.Before(nowT) {
		next = nowT
	}
	c.last = next
	c.mu.Unlock()
	select {
	case <-time.After(time.Until(next)):
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// StatusError is returned for non-2xx responses.
type StatusError struct {
	Code int
	Body string
}

func (e *StatusError) Error() string { return fmt.Sprintf("tmdb: HTTP %d: %s", e.Code, e.Body) }

func (c *Client) get(ctx context.Context, path string, q url.Values, out any) error {
	if q == nil {
		q = url.Values{}
	}
	if c.Language != "" {
		q.Set("language", c.Language)
	}
	bearer := strings.HasPrefix(c.Key, "eyJ")
	if !bearer && c.Key != "" {
		q.Set("api_key", c.Key)
	}
	u := strings.TrimRight(c.BaseURL, "/") + path + "?" + q.Encode()

	for attempt := 0; ; attempt++ {
		if err := c.wait(ctx); err != nil {
			return err
		}
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
		if err != nil {
			return err
		}
		req.Header.Set("Accept", "application/json")
		if bearer {
			req.Header.Set("Authorization", "Bearer "+c.Key)
		}
		resp, err := c.HTTP.Do(req)
		if err != nil {
			if attempt < 2 {
				continue
			}
			return err
		}
		body, err := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
		resp.Body.Close()
		if err != nil {
			return err
		}
		switch {
		case resp.StatusCode == http.StatusTooManyRequests && attempt < 3:
			delay := 2 * time.Second
			if s, err := strconv.Atoi(resp.Header.Get("Retry-After")); err == nil && s > 0 {
				delay = time.Duration(s) * time.Second
			}
			select {
			case <-time.After(delay):
			case <-ctx.Done():
				return ctx.Err()
			}
			continue
		case resp.StatusCode >= 500 && attempt < 1:
			continue
		case resp.StatusCode < 200 || resp.StatusCode > 299:
			msg := string(body)
			if len(msg) > 200 {
				msg = msg[:200]
			}
			return &StatusError{Code: resp.StatusCode, Body: msg}
		}
		return json.Unmarshal(body, out)
	}
}

// MovieResult is one movie search hit.
type MovieResult struct {
	ID            int     `json:"id"`
	Title         string  `json:"title"`
	OriginalTitle string  `json:"original_title"`
	ReleaseDate   string  `json:"release_date"`
	Popularity    float64 `json:"popularity"`
}

// TVResult is one show search hit.
type TVResult struct {
	ID            int      `json:"id"`
	Name          string   `json:"name"`
	OriginalName  string   `json:"original_name"`
	FirstAirDate  string   `json:"first_air_date"`
	OriginCountry []string `json:"origin_country"`
	Popularity    float64  `json:"popularity"`
}

type MovieDetails struct {
	ID           int     `json:"id"`
	Title        string  `json:"title"`
	Overview     string  `json:"overview"`
	ReleaseDate  string  `json:"release_date"`
	Runtime      int     `json:"runtime"`
	VoteAverage  float64 `json:"vote_average"`
	PosterPath   string  `json:"poster_path"`
	BackdropPath string  `json:"backdrop_path"`
	IMDBID       string  `json:"imdb_id"`
	Genres       []Genre `json:"genres"`
}

// Genre is a TMDB genre such as {35, "Comedy"}.
type Genre struct {
	ID   int    `json:"id"`
	Name string `json:"name"`
}

// GenreNames lists genre names in TMDB's order.
func GenreNames(gs []Genre) []string {
	out := make([]string, 0, len(gs))
	for _, g := range gs {
		out = append(out, g.Name)
	}
	return out
}

type TVDetails struct {
	ID             int     `json:"id"`
	Name           string  `json:"name"`
	Overview       string  `json:"overview"`
	FirstAirDate   string  `json:"first_air_date"`
	VoteAverage    float64 `json:"vote_average"`
	PosterPath     string  `json:"poster_path"`
	BackdropPath   string  `json:"backdrop_path"`
	EpisodeRunTime []int   `json:"episode_run_time"`
	Genres         []Genre `json:"genres"`
	Seasons        []struct {
		SeasonNumber int    `json:"season_number"`
		Name         string `json:"name"`
		EpisodeCount int    `json:"episode_count"`
		AirDate      string `json:"air_date"`
		PosterPath   string `json:"poster_path"`
	} `json:"seasons"`
	// Status is "Returning Series", "Ended", "Canceled" and the like.
	Status string `json:"status"`
}

type SeasonDetails struct {
	SeasonNumber int    `json:"season_number"`
	Name         string `json:"name"`
	Overview     string `json:"overview"`
	AirDate      string `json:"air_date"`
	PosterPath   string `json:"poster_path"`
	Episodes     []struct {
		ID            int     `json:"id"`
		EpisodeNumber int     `json:"episode_number"`
		Name          string  `json:"name"`
		Overview      string  `json:"overview"`
		AirDate       string  `json:"air_date"`
		Runtime       int     `json:"runtime"`
		StillPath     string  `json:"still_path"`
		VoteAverage   float64 `json:"vote_average"`
	} `json:"episodes"`
}

func yearParam(q url.Values, key string, year int) {
	if year > 0 {
		q.Set(key, strconv.Itoa(year))
	}
}

func (c *Client) SearchMovie(ctx context.Context, query string, year int) ([]MovieResult, error) {
	q := url.Values{"query": {query}, "include_adult": {"false"}}
	yearParam(q, "year", year)
	var res struct {
		Results []MovieResult `json:"results"`
	}
	err := c.get(ctx, "/search/movie", q, &res)
	return res.Results, err
}

func (c *Client) SearchTV(ctx context.Context, query string, year int) ([]TVResult, error) {
	q := url.Values{"query": {query}, "include_adult": {"false"}}
	yearParam(q, "first_air_date_year", year)
	var res struct {
		Results []TVResult `json:"results"`
	}
	err := c.get(ctx, "/search/tv", q, &res)
	return res.Results, err
}

func (c *Client) Movie(ctx context.Context, id int) (MovieDetails, error) {
	var d MovieDetails
	err := c.get(ctx, "/movie/"+strconv.Itoa(id), nil, &d)
	return d, err
}

func (c *Client) TV(ctx context.Context, id int) (TVDetails, error) {
	var d TVDetails
	err := c.get(ctx, "/tv/"+strconv.Itoa(id), nil, &d)
	return d, err
}

func (c *Client) Season(ctx context.Context, tvID, season int) (SeasonDetails, error) {
	var d SeasonDetails
	err := c.get(ctx, fmt.Sprintf("/tv/%d/season/%d", tvID, season), nil, &d)
	return d, err
}

// FindIMDB looks up a movie or show by IMDb ID. Either result may be 0.
func (c *Client) FindIMDB(ctx context.Context, imdbID string) (movieID, tvID int, err error) {
	var res struct {
		Movies []struct {
			ID int `json:"id"`
		} `json:"movie_results"`
		TV []struct {
			ID int `json:"id"`
		} `json:"tv_results"`
	}
	err = c.get(ctx, "/find/"+url.PathEscape(imdbID), url.Values{"external_source": {"imdb_id"}}, &res)
	if len(res.Movies) > 0 {
		movieID = res.Movies[0].ID
	}
	if len(res.TV) > 0 {
		tvID = res.TV[0].ID
	}
	return movieID, tvID, err
}

// YearOf returns the year from a date like "2024-03-01", or 0.
func YearOf(date string) int {
	if len(date) < 4 {
		return 0
	}
	y, _ := strconv.Atoi(date[:4])
	return y
}

// CastMember is one billed actor.
type CastMember struct {
	Name        string `json:"name"`
	Character   string `json:"character"`
	ProfilePath string `json:"profile_path"`
	Order       int    `json:"order"`
}

// MovieCast returns a movie's cast, top-billed first.
func (c *Client) MovieCast(ctx context.Context, id int) ([]CastMember, error) {
	var res struct {
		Cast []CastMember `json:"cast"`
	}
	err := c.get(ctx, "/movie/"+strconv.Itoa(id)+"/credits", nil, &res)
	return res.Cast, err
}

// TVCast returns a show's cast across all seasons, regulars first.
func (c *Client) TVCast(ctx context.Context, id int) ([]CastMember, error) {
	var res struct {
		Cast []struct {
			Name        string `json:"name"`
			ProfilePath string `json:"profile_path"`
			Order       int    `json:"order"`
			Roles       []struct {
				Character string `json:"character"`
			} `json:"roles"`
		} `json:"cast"`
	}
	if err := c.get(ctx, "/tv/"+strconv.Itoa(id)+"/aggregate_credits", nil, &res); err != nil {
		return nil, err
	}
	out := make([]CastMember, 0, len(res.Cast))
	for _, m := range res.Cast {
		cm := CastMember{Name: m.Name, ProfilePath: m.ProfilePath, Order: m.Order}
		if len(m.Roles) > 0 {
			cm.Character = m.Roles[0].Character
		}
		out = append(out, cm)
	}
	return out, nil
}

// TrendingResult is one title from TMDB's trending lists.
type TrendingResult struct {
	ID           int     `json:"id"`
	MediaType    string  `json:"media_type"` // movie or tv
	Title        string  `json:"title"`      // movies
	Name         string  `json:"name"`       // shows
	Overview     string  `json:"overview"`
	ReleaseDate  string  `json:"release_date"`
	FirstAirDate string  `json:"first_air_date"`
	VoteAverage  float64 `json:"vote_average"`
	Popularity   float64 `json:"popularity"`
	PosterPath   string  `json:"poster_path"`
	BackdropPath string  `json:"backdrop_path"`
	GenreIDs     []int   `json:"genre_ids"`
}

// Trending lists what's trending: mediaType is movie, tv or all; window is
// day or week. page starts at 1.
func (c *Client) Trending(ctx context.Context, mediaType, window string, page int) ([]TrendingResult, error) {
	q := url.Values{}
	if page > 1 {
		q.Set("page", strconv.Itoa(page))
	}
	var res struct {
		Results []TrendingResult `json:"results"`
	}
	err := c.get(ctx, "/trending/"+url.PathEscape(mediaType)+"/"+url.PathEscape(window), q, &res)
	for i := range res.Results {
		if res.Results[i].MediaType == "" {
			res.Results[i].MediaType = mediaType
		}
	}
	return res.Results, err
}

// GenreList returns TMDB's genre names by ID for movies or tv.
func (c *Client) GenreList(ctx context.Context, mediaType string) (map[int]string, error) {
	var res struct {
		Genres []Genre `json:"genres"`
	}
	if err := c.get(ctx, "/genre/"+url.PathEscape(mediaType)+"/list", nil, &res); err != nil {
		return nil, err
	}
	out := make(map[int]string, len(res.Genres))
	for _, g := range res.Genres {
		out[g.ID] = g.Name
	}
	return out, nil
}
