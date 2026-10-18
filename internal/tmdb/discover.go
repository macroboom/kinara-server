package tmdb

import (
	"context"
	"net/url"
	"strconv"
)

// The calls behind the Discover page and requests: TMDB's lists (now
// playing, popular and so on), genre discovery, multi search and a title's
// IDs on other sites.

// MaxPages is the last page TMDB will hand out for any list.
const MaxPages = 500

// Page is one page of titles. Results reuse TrendingResult: every TMDB list
// returns the same fields.
type Page struct {
	Page       int              `json:"page"`
	TotalPages int              `json:"total_pages"`
	Results    []TrendingResult `json:"results"`
}

// TMDB's lists by name. Not every list exists for both kinds: now_playing
// and upcoming are movies only, on_the_air and airing_today are tv only.
const (
	ListNowPlaying = "now_playing"
	ListUpcoming   = "upcoming"
	ListOnTheAir   = "on_the_air"
	ListPopular    = "popular"
	ListTopRated   = "top_rated"
)

func pageParam(q url.Values, page int) {
	if page > 1 {
		q.Set("page", strconv.Itoa(min(page, MaxPages)))
	}
}

// getPage fetches a paged list and marks each result with mediaType when
// TMDB leaves it out (only mixed lists say which each title is).
func (c *Client) getPage(ctx context.Context, path, mediaType string, q url.Values) (Page, error) {
	var p Page
	if err := c.get(ctx, path, q, &p); err != nil {
		return p, err
	}
	for i := range p.Results {
		if p.Results[i].MediaType == "" {
			p.Results[i].MediaType = mediaType
		}
	}
	if p.Page == 0 {
		p.Page = 1
	}
	p.TotalPages = min(p.TotalPages, MaxPages)
	return p, nil
}

// List fetches one of TMDB's lists (ListPopular and so on) for movie or tv.
func (c *Client) List(ctx context.Context, mediaType, list string, page int) (Page, error) {
	q := url.Values{}
	pageParam(q, page)
	return c.getPage(ctx, "/"+url.PathEscape(mediaType)+"/"+url.PathEscape(list), mediaType, q)
}

// TrendingPage is Trending with the page count, for paging through it.
func (c *Client) TrendingPage(ctx context.Context, mediaType, window string, page int) (Page, error) {
	q := url.Values{}
	pageParam(q, page)
	return c.getPage(ctx, "/trending/"+url.PathEscape(mediaType)+"/"+url.PathEscape(window), mediaType, q)
}

// DiscoverOptions narrows Discover.
type DiscoverOptions struct {
	// GenreID limits to one genre (0: any).
	GenreID int
	// SortBy is TMDB's sort, such as "popularity.desc" or
	// "vote_average.desc".
	SortBy string
	// ReleasedBefore and ReleasedAfter are dates like "1999-12-31" (release
	// date for movies, first air date for tv).
	ReleasedBefore string
	ReleasedAfter  string
	// MinVotes leaves out titles too few people rated, so a top rated list
	// isn't led by a film three people saw.
	MinVotes int
	Page     int
}

// Discover lists movies or tv by genre, date and sort.
func (c *Client) Discover(ctx context.Context, mediaType string, o DiscoverOptions) (Page, error) {
	q := url.Values{"include_adult": {"false"}}
	if o.GenreID > 0 {
		q.Set("with_genres", strconv.Itoa(o.GenreID))
	}
	if o.SortBy != "" {
		q.Set("sort_by", o.SortBy)
	}
	dateKey := "primary_release_date"
	if mediaType == "tv" {
		dateKey = "first_air_date"
	}
	if o.ReleasedBefore != "" {
		q.Set(dateKey+".lte", o.ReleasedBefore)
	}
	if o.ReleasedAfter != "" {
		q.Set(dateKey+".gte", o.ReleasedAfter)
	}
	if o.MinVotes > 0 {
		q.Set("vote_count.gte", strconv.Itoa(o.MinVotes))
	}
	pageParam(q, o.Page)
	return c.getPage(ctx, "/discover/"+url.PathEscape(mediaType), mediaType, q)
}

// SearchMulti searches movies, shows and people at once. Results say which
// each is in MediaType ("movie", "tv" or "person").
func (c *Client) SearchMulti(ctx context.Context, query string, page int) (Page, error) {
	q := url.Values{"query": {query}, "include_adult": {"false"}}
	pageParam(q, page)
	return c.getPage(ctx, "/search/multi", "", q)
}

// Genres lists TMDB's genres for movie or tv in TMDB's order.
func (c *Client) Genres(ctx context.Context, mediaType string) ([]Genre, error) {
	var res struct {
		Genres []Genre `json:"genres"`
	}
	err := c.get(ctx, "/genre/"+url.PathEscape(mediaType)+"/list", nil, &res)
	return res.Genres, err
}

// ExternalIDs are a title's IDs on other sites (zero or "" when unknown).
type ExternalIDs struct {
	IMDBID string `json:"imdb_id"`
	TVDBID int    `json:"tvdb_id"`
}

// ExternalIDs returns a movie's or show's IMDb and TVDB IDs. mediaType is
// movie or tv.
func (c *Client) ExternalIDs(ctx context.Context, mediaType string, id int) (ExternalIDs, error) {
	var res struct {
		IMDBID *string `json:"imdb_id"`
		TVDBID *int    `json:"tvdb_id"`
	}
	err := c.get(ctx, "/"+url.PathEscape(mediaType)+"/"+strconv.Itoa(id)+"/external_ids", nil, &res)
	var out ExternalIDs
	if res.IMDBID != nil {
		out.IMDBID = *res.IMDBID
	}
	if res.TVDBID != nil {
		out.TVDBID = *res.TVDBID
	}
	return out, err
}

// Recommendations is TMDB's "more like this" for a title (mediaType "movie"
// or "tv"): its recommendations, or its similar titles when there are none
// (recommendations come from viewing habits, so lesser-known titles have
// few).
func (c *Client) Recommendations(ctx context.Context, mediaType string, id, page int) (Page, error) {
	q := url.Values{}
	pageParam(q, page)
	base := "/" + url.PathEscape(mediaType) + "/" + strconv.Itoa(id)
	p, err := c.getPage(ctx, base+"/recommendations", mediaType, q)
	if err != nil || len(p.Results) > 0 {
		return p, err
	}
	return c.getPage(ctx, base+"/similar", mediaType, q)
}
