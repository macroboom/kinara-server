package tmdb

import (
	"context"
	"net/url"
	"strconv"
)

// FoundTitle is a movie or show TMDB found for an ID from another site.
type FoundTitle struct {
	ID    int
	Kind  string // "movie" or "show"
	Title string
	Year  int
}

// FindExternal looks a title up by its ID on another site: source is
// "imdb_id" or "tvdb_id". Movies come first. Nothing found is not an error.
func (c *Client) FindExternal(ctx context.Context, source, id string) ([]FoundTitle, error) {
	var res struct {
		Movies []MovieResult `json:"movie_results"`
		TV     []TVResult    `json:"tv_results"`
	}
	if err := c.get(ctx, "/find/"+url.PathEscape(id), url.Values{"external_source": {source}}, &res); err != nil {
		return nil, err
	}
	var out []FoundTitle
	for _, m := range res.Movies {
		out = append(out, FoundTitle{ID: m.ID, Kind: "movie", Title: m.Title, Year: YearOf(m.ReleaseDate)})
	}
	for _, t := range res.TV {
		out = append(out, FoundTitle{ID: t.ID, Kind: "show", Title: t.Name, Year: YearOf(t.FirstAirDate)})
	}
	return out, nil
}

// FindTVDB looks a title up by its TVDB ID.
func (c *Client) FindTVDB(ctx context.Context, tvdbID int) ([]FoundTitle, error) {
	return c.FindExternal(ctx, "tvdb_id", strconv.Itoa(tvdbID))
}
