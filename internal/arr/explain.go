package arr

import (
	"context"
	"fmt"
	"net/http"
	"time"
)

// Why a title the service has isn't downloading: what Radarr and Sonarr
// know about it, for Kinara to say in plain words on the request.

// MovieInfo is what Radarr knows about a movie it has.
type MovieInfo struct {
	ID                  int       `json:"id"`
	TMDBID              int       `json:"tmdbId"`
	Monitored           bool      `json:"monitored"`
	HasFile             bool      `json:"hasFile"`
	IsAvailable         bool      `json:"isAvailable"`
	Status              string    `json:"status"` // tba, announced, inCinemas, released, deleted
	MinimumAvailability string    `json:"minimumAvailability"`
	InCinemas           time.Time `json:"inCinemas"`
	DigitalRelease      time.Time `json:"digitalRelease"`
	PhysicalRelease     time.Time `json:"physicalRelease"`
	Added               time.Time `json:"added"`
}

// MovieByTMDB finds a movie in Radarr by its TMDB ID (nil: not there).
func (c *Client) MovieByTMDB(ctx context.Context, tmdbID int) (*MovieInfo, error) {
	var list []MovieInfo
	if err := c.do(ctx, http.MethodGet, fmt.Sprintf("movie?tmdbId=%d", tmdbID), nil, &list); err != nil {
		return nil, err
	}
	for i := range list {
		if list[i].TMDBID == tmdbID || len(list) == 1 {
			return &list[i], nil
		}
	}
	return nil, nil
}

// SeriesInfo is what Sonarr knows about a show it has.
type SeriesInfo struct {
	ID        int       `json:"id"`
	TVDBID    int       `json:"tvdbId"`
	TMDBID    int       `json:"tmdbId"`
	Title     string    `json:"title"`
	Monitored bool      `json:"monitored"`
	Added     time.Time `json:"added"`
}

// AllSeries lists Sonarr's shows.
func (c *Client) AllSeries(ctx context.Context) ([]SeriesInfo, error) {
	var list []SeriesInfo
	return list, c.do(ctx, http.MethodGet, "series", nil, &list)
}

// EpisodeInfo is one episode of a show in Sonarr.
type EpisodeInfo struct {
	SeasonNumber  int       `json:"seasonNumber"`
	EpisodeNumber int       `json:"episodeNumber"`
	Monitored     bool      `json:"monitored"`
	HasFile       bool      `json:"hasFile"`
	AirDateUTC    time.Time `json:"airDateUtc"`
}

// Episodes lists a show's episodes.
func (c *Client) Episodes(ctx context.Context, seriesID int) ([]EpisodeInfo, error) {
	var list []EpisodeInfo
	return list, c.do(ctx, http.MethodGet, fmt.Sprintf("episode?seriesId=%d", seriesID), nil, &list)
}

// HistoryEvent is one thing that happened to a title's downloads.
type HistoryEvent struct {
	EventType string    `json:"eventType"` // grabbed, downloadFailed, downloadFolderImported...
	Date      time.Time `json:"date"`
}

// History is what happened to a movie's (Radarr) or show's (Sonarr)
// downloads, newest first.
func (c *Client) History(ctx context.Context, id int) ([]HistoryEvent, error) {
	path := fmt.Sprintf("history/movie?movieId=%d", id)
	if c.Kind == Sonarr {
		path = fmt.Sprintf("history/series?seriesId=%d", id)
	}
	var list []HistoryEvent
	if err := c.do(ctx, http.MethodGet, path, nil, &list); err != nil {
		return nil, err
	}
	// Newest first, whatever order the service used.
	for i := 1; i < len(list); i++ {
		for j := i; j > 0 && list[j].Date.After(list[j-1].Date); j-- {
			list[j], list[j-1] = list[j-1], list[j]
		}
	}
	return list, nil
}
