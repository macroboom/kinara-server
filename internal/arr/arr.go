// Package arr talks to Radarr and Sonarr (API v3): it checks a connection,
// reads the choices an owner picks from (quality profiles, root folders),
// adds a requested movie or show (or monitors and searches one already
// there), and reads the download queue so the apps can show what's coming.
package arr

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// Kinds of service.
const (
	Radarr   = "radarr"
	Sonarr   = "sonarr"
	Prowlarr = "prowlarr" // API v1
)

// Client is one Radarr or Sonarr.
type Client struct {
	// Kind is Radarr or Sonarr.
	Kind string
	// BaseURL is where it answers, with any URL base ("http://nas:7878/radarr").
	BaseURL string
	APIKey  string
	HTTP    *http.Client
}

// Error is an answer from the service that isn't a success.
type Error struct {
	Status  int
	Message string
}

func (e *Error) Error() string {
	switch e.Status {
	case http.StatusUnauthorized:
		return "the API key was turned down (Settings, General, API Key)"
	case http.StatusNotFound:
		return "not found: is the address right, including any URL base?"
	}
	if e.Message != "" {
		return fmt.Sprintf("%d: %s", e.Status, e.Message)
	}
	return fmt.Sprintf("answered %d", e.Status)
}

func (c *Client) http() *http.Client {
	if c.HTTP != nil {
		return c.HTTP
	}
	return &http.Client{Timeout: 20 * time.Second}
}

// do calls /api/v3/<path> and decodes the answer into out (if not nil).
func (c *Client) do(ctx context.Context, method, path string, body, out any) error {
	base := strings.TrimRight(strings.TrimSpace(c.BaseURL), "/")
	if base == "" {
		return errors.New("no address set")
	}
	if !strings.HasPrefix(base, "http://") && !strings.HasPrefix(base, "https://") {
		base = "http://" + base
	}
	var rd io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return err
		}
		rd = bytes.NewReader(b)
	}
	version := "/api/v3/"
	if c.Kind == Prowlarr {
		version = "/api/v1/"
	}
	req, err := http.NewRequestWithContext(ctx, method, base+version+strings.TrimLeft(path, "/"), rd)
	if err != nil {
		return err
	}
	req.Header.Set("X-Api-Key", strings.TrimSpace(c.APIKey))
	req.Header.Set("Accept", "application/json")
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := c.http().Do(req)
	if err != nil {
		// Without Go's "Get <address>:" in front: the address is on the page.
		var ue *url.Error
		if errors.As(err, &ue) {
			err = ue.Err
		}
		return fmt.Errorf("couldn't reach it: %w", err)
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(io.LimitReader(resp.Body, 16<<20))
	if resp.StatusCode >= 300 {
		return &Error{Status: resp.StatusCode, Message: errorText(data)}
	}
	if out == nil || len(data) == 0 {
		return nil
	}
	if err := json.Unmarshal(data, out); err != nil {
		return fmt.Errorf("unreadable answer (is this %s?): %w", c.label(), err)
	}
	return nil
}

func (c *Client) label() string {
	switch c.Kind {
	case Sonarr:
		return "Sonarr"
	case Prowlarr:
		return "Prowlarr"
	}
	return "Radarr"
}

// errorText pulls a readable message out of an error answer: a message
// field, a list of validation errors, or the text itself.
func errorText(data []byte) string {
	var one struct {
		Message string `json:"message"`
	}
	if json.Unmarshal(data, &one) == nil && one.Message != "" {
		return one.Message
	}
	var many []struct {
		ErrorMessage string `json:"errorMessage"`
	}
	if json.Unmarshal(data, &many) == nil && len(many) > 0 {
		var msgs []string
		for _, m := range many {
			if m.ErrorMessage != "" {
				msgs = append(msgs, m.ErrorMessage)
			}
		}
		return strings.Join(msgs, "; ")
	}
	s := strings.TrimSpace(string(data))
	if len(s) > 200 || strings.HasPrefix(s, "<") {
		return ""
	}
	return s
}

// --- What the owner picks from ----------------------------------------------------

// Status is the service's own description.
type Status struct {
	AppName string `json:"appName"`
	Version string `json:"version"`
}

// Profile is a quality profile.
type Profile struct {
	ID   int    `json:"id"`
	Name string `json:"name"`
}

// RootFolder is where the service keeps a library.
type RootFolder struct {
	ID   int    `json:"id"`
	Path string `json:"path"`
	// FreeSpace is in bytes (a float: some versions write it as one).
	FreeSpace float64 `json:"freeSpace"`
}

// Tag is a label for titles.
type Tag struct {
	ID    int    `json:"id"`
	Label string `json:"label"`
}

// Options is everything the settings page shows after a connection test.
type Options struct {
	Status      Status       `json:"status"`
	Profiles    []Profile    `json:"profiles"`
	RootFolders []RootFolder `json:"rootFolders"`
	Tags        []Tag        `json:"tags"`
}

// Check connects and reads the choices; it also makes sure the service is
// the kind it should be (a Sonarr address in the Radarr box is a mistake).
func (c *Client) Check(ctx context.Context) (Options, error) {
	var o Options
	if err := c.do(ctx, http.MethodGet, "system/status", nil, &o.Status); err != nil {
		return o, err
	}
	if app := strings.ToLower(o.Status.AppName); app != "" && app != c.Kind {
		return o, fmt.Errorf("that address is %s, not %s", o.Status.AppName, c.label())
	}
	if err := c.do(ctx, http.MethodGet, "qualityprofile", nil, &o.Profiles); err != nil {
		return o, err
	}
	if err := c.do(ctx, http.MethodGet, "rootfolder", nil, &o.RootFolders); err != nil {
		return o, err
	}
	_ = c.do(ctx, http.MethodGet, "tag", nil, &o.Tags)
	if o.Tags == nil {
		o.Tags = []Tag{}
	}
	return o, nil
}

// --- Adding -----------------------------------------------------------------------

// AddOptions are how a title is added.
type AddOptions struct {
	ProfileID  int
	RootFolder string
	Tags       []int
	// Search starts looking for it straight away.
	Search bool
	// MinimumAvailability (Radarr): announced, inCinemas or released.
	MinimumAvailability string
	// SeasonFolder (Sonarr) puts each season in its own folder.
	SeasonFolder bool
	// SeriesType (Sonarr): standard, daily or anime.
	SeriesType string
}

// Result says what adding did.
type Result struct {
	// Added: it's new to the service. Otherwise it was there already and
	// is now monitored (and searched for, with Search).
	Added bool
	// HasFile: the service already has it, so there's nothing to fetch.
	HasFile bool
	// ID is the service's own ID for it.
	ID int
}

// AddMovie adds a movie to Radarr by its TMDB ID, or monitors and searches
// for it when Radarr has it already.
func (c *Client) AddMovie(ctx context.Context, tmdbID int, o AddOptions) (Result, error) {
	var have []map[string]any
	if err := c.do(ctx, http.MethodGet, "movie?tmdbId="+strconv.Itoa(tmdbID), nil, &have); err != nil {
		return Result{}, err
	}
	if len(have) > 0 {
		m := have[0]
		id := intOf(m["id"])
		if b, _ := m["hasFile"].(bool); b {
			return Result{HasFile: true, ID: id}, nil
		}
		if b, _ := m["monitored"].(bool); !b {
			m["monitored"] = true
			if err := c.do(ctx, http.MethodPut, "movie/"+strconv.Itoa(id), m, nil); err != nil {
				return Result{}, err
			}
		}
		if o.Search {
			if err := c.command(ctx, map[string]any{"name": "MoviesSearch", "movieIds": []int{id}}); err != nil {
				return Result{}, err
			}
		}
		return Result{ID: id}, nil
	}
	var m map[string]any
	if err := c.do(ctx, http.MethodGet, "movie/lookup/tmdb?tmdbId="+strconv.Itoa(tmdbID), nil, &m); err != nil {
		return Result{}, err
	}
	if len(m) == 0 {
		return Result{}, errors.New("Radarr couldn't find that movie")
	}
	avail := o.MinimumAvailability
	if avail == "" {
		avail = "released"
	}
	m["qualityProfileId"] = o.ProfileID
	m["rootFolderPath"] = o.RootFolder
	m["monitored"] = true
	m["minimumAvailability"] = avail
	m["tags"] = tagsOrEmpty(o.Tags)
	m["addOptions"] = map[string]any{"searchForMovie": o.Search, "monitor": "movieOnly"}
	delete(m, "id")
	var added map[string]any
	if err := c.do(ctx, http.MethodPost, "movie", m, &added); err != nil {
		return Result{}, err
	}
	return Result{Added: true, ID: intOf(added["id"])}, nil
}

// Show request modes, as Kinara's requests have them.
const (
	ModeAll     = "all"
	ModeSeasons = "seasons"
	ModePilot   = "pilot"
)

// AddSeries adds a show to Sonarr, monitoring what was asked for (every
// season, some seasons, or the first episode), or monitors that much more
// of a show Sonarr has already and searches for it. term finds the show:
// "tvdb:<id>" is the reliable one; "imdb:tt..." or the title also work.
func (c *Client) AddSeries(ctx context.Context, tvdbID int, term, mode string, seasons []int, o AddOptions) (Result, error) {
	want := map[int]bool{}
	for _, n := range seasons {
		want[n] = true
	}
	monitored := func(n int, before bool) bool {
		switch mode {
		case ModeSeasons:
			return before || want[n]
		case ModePilot:
			return before
		}
		return before || n > 0 // every season but specials
	}

	if tvdbID > 0 {
		var have []map[string]any
		if err := c.do(ctx, http.MethodGet, "series?tvdbId="+strconv.Itoa(tvdbID), nil, &have); err != nil {
			return Result{}, err
		}
		// Older Sonarr ignores the filter and lists everything.
		for _, s := range have {
			if intOf(s["tvdbId"]) == tvdbID {
				return c.monitorSeries(ctx, s, mode, monitored, o)
			}
		}
	}
	if term == "" && tvdbID > 0 {
		term = "tvdb:" + strconv.Itoa(tvdbID)
	}
	var found []map[string]any
	if err := c.do(ctx, http.MethodGet, "series/lookup?term="+url.QueryEscape(term), nil, &found); err != nil {
		return Result{}, err
	}
	if len(found) == 0 {
		return Result{}, errors.New("Sonarr couldn't find that show")
	}
	s := found[0]
	if id := intOf(s["id"]); id > 0 {
		// The lookup found one Sonarr has already (no TVDB ID to ask by).
		return c.monitorSeries(ctx, s, mode, monitored, o)
	}
	if list, ok := s["seasons"].([]any); ok {
		for _, x := range list {
			if season, ok := x.(map[string]any); ok {
				season["monitored"] = monitored(intOf(season["seasonNumber"]), false)
			}
		}
	}
	seriesType := o.SeriesType
	if seriesType == "" {
		seriesType = "standard"
	}
	s["qualityProfileId"] = o.ProfileID
	s["rootFolderPath"] = o.RootFolder
	s["monitored"] = true
	s["seasonFolder"] = o.SeasonFolder
	s["seriesType"] = seriesType
	s["tags"] = tagsOrEmpty(o.Tags)
	add := map[string]any{"searchForMissingEpisodes": o.Search, "ignoreEpisodesWithFiles": true}
	if mode == ModePilot {
		add["monitor"] = "pilot"
	}
	s["addOptions"] = add
	// Sonarr v3 also wants a language profile; v4 has none.
	if _, ok := s["languageProfileId"]; !ok || intOf(s["languageProfileId"]) == 0 {
		var langs []Profile
		if err := c.do(ctx, http.MethodGet, "languageprofile", nil, &langs); err == nil && len(langs) > 0 {
			s["languageProfileId"] = langs[0].ID
		}
	}
	var added map[string]any
	if err := c.do(ctx, http.MethodPost, "series", s, &added); err != nil {
		return Result{}, err
	}
	return Result{Added: true, ID: intOf(added["id"])}, nil
}

// monitorSeries monitors more of a show Sonarr has and searches for it.
func (c *Client) monitorSeries(ctx context.Context, s map[string]any, mode string, monitored func(int, bool) bool, o AddOptions) (Result, error) {
	id := intOf(s["id"])
	var searchSeasons []int
	changed := false
	if b, _ := s["monitored"].(bool); !b {
		s["monitored"], changed = true, true
	}
	if list, ok := s["seasons"].([]any); ok {
		for _, x := range list {
			season, ok := x.(map[string]any)
			if !ok {
				continue
			}
			n := intOf(season["seasonNumber"])
			before, _ := season["monitored"].(bool)
			after := monitored(n, before)
			if after != before {
				season["monitored"], changed = after, true
			}
			if after && n > 0 && (mode != ModeSeasons || monitored(n, false)) {
				searchSeasons = append(searchSeasons, n)
			}
		}
	}
	if changed {
		if err := c.do(ctx, http.MethodPut, "series/"+strconv.Itoa(id), s, nil); err != nil {
			return Result{}, err
		}
	}
	if mode == ModePilot {
		return Result{ID: id}, c.pilot(ctx, id, o.Search)
	}
	if o.Search {
		for _, n := range searchSeasons {
			if err := c.command(ctx, map[string]any{"name": "SeasonSearch", "seriesId": id, "seasonNumber": n}); err != nil {
				return Result{}, err
			}
		}
	}
	return Result{ID: id}, nil
}

// pilot monitors (and searches for) a show's first episode.
func (c *Client) pilot(ctx context.Context, seriesID int, search bool) error {
	var eps []struct {
		ID            int  `json:"id"`
		SeasonNumber  int  `json:"seasonNumber"`
		EpisodeNumber int  `json:"episodeNumber"`
		HasFile       bool `json:"hasFile"`
	}
	if err := c.do(ctx, http.MethodGet, "episode?seriesId="+strconv.Itoa(seriesID), nil, &eps); err != nil {
		return err
	}
	for _, e := range eps {
		if e.SeasonNumber == 1 && e.EpisodeNumber == 1 {
			if e.HasFile {
				return nil
			}
			if err := c.do(ctx, http.MethodPut, "episode/monitor", map[string]any{"episodeIds": []int{e.ID}, "monitored": true}, nil); err != nil {
				return err
			}
			if search {
				return c.command(ctx, map[string]any{"name": "EpisodeSearch", "episodeIds": []int{e.ID}})
			}
			return nil
		}
	}
	return errors.New("Sonarr doesn't list a first episode for that show yet")
}

func (c *Client) command(ctx context.Context, body map[string]any) error {
	return c.do(ctx, http.MethodPost, "command", body, nil)
}

// --- The download queue -----------------------------------------------------------

// QueueItem is one download the service is handling.
type QueueItem struct {
	ID         int    `json:"id"`
	DownloadID string `json:"downloadId"`
	Title      string `json:"title"`
	// Size and SizeLeft are in bytes.
	Size     float64 `json:"size"`
	SizeLeft float64 `json:"sizeleft"`
	// TimeLeft is "hh:mm:ss" or "d.hh:mm:ss".
	TimeLeft string `json:"timeleft"`
	// Status is the download client's (queued, paused, downloading,
	// completed, delay, warning, failed...).
	Status string `json:"status"`
	// TrackedDownloadState is the service's (downloading, importPending,
	// importing, imported, failedPending, failed...).
	TrackedDownloadState  string `json:"trackedDownloadState"`
	TrackedDownloadStatus string `json:"trackedDownloadStatus"`
	ErrorMessage          string `json:"errorMessage"`
	StatusMessages        []struct {
		Title    string   `json:"title"`
		Messages []string `json:"messages"`
	} `json:"statusMessages"`
	MovieID   int `json:"movieId"`
	SeriesID  int `json:"seriesId"`
	EpisodeID int `json:"episodeId"`
	Movie     *struct {
		Title  string `json:"title"`
		Year   int    `json:"year"`
		TMDBID int    `json:"tmdbId"`
	} `json:"movie"`
	Series *struct {
		Title  string `json:"title"`
		Year   int    `json:"year"`
		TVDBID int    `json:"tvdbId"`
		TMDBID int    `json:"tmdbId"`
		IMDBID string `json:"imdbId"`
	} `json:"series"`
	Episode *struct {
		SeasonNumber  int `json:"seasonNumber"`
		EpisodeNumber int `json:"episodeNumber"`
	} `json:"episode"`
}

// Progress is how much is downloaded, 0 to 1 (-1 when unknown).
func (q QueueItem) Progress() float64 {
	if q.Size <= 0 {
		return -1
	}
	p := 1 - q.SizeLeft/q.Size
	return min(max(p, 0), 1)
}

// EtaSeconds reads TimeLeft (0 when unknown).
func (q QueueItem) EtaSeconds() int {
	t := q.TimeLeft
	if t == "" {
		return 0
	}
	days := 0
	if d, rest, ok := strings.Cut(t, "."); ok && !strings.Contains(d, ":") {
		days, _ = strconv.Atoi(d)
		t = rest
	}
	parts := strings.Split(t, ":")
	if len(parts) != 3 {
		return 0
	}
	h, _ := strconv.Atoi(parts[0])
	m, _ := strconv.Atoi(parts[1])
	sec, _ := strconv.ParseFloat(parts[2], 64)
	return days*86400 + h*3600 + m*60 + int(sec)
}

// Queue lists the service's downloads.
func (c *Client) Queue(ctx context.Context) ([]QueueItem, error) {
	path := "queue?page=1&pageSize=500&includeUnknownMovieItems=false&includeMovie=true"
	if c.Kind == Sonarr {
		path = "queue?page=1&pageSize=500&includeUnknownSeriesItems=false&includeSeries=true&includeEpisode=true"
	}
	var page struct {
		Records []QueueItem `json:"records"`
	}
	if err := c.do(ctx, http.MethodGet, path, nil, &page); err != nil {
		return nil, err
	}
	return page.Records, nil
}

// MovieHasFile says whether Radarr has a movie's file.
func (c *Client) MovieHasFile(ctx context.Context, id int) (bool, error) {
	var m struct {
		HasFile bool `json:"hasFile"`
	}
	err := c.do(ctx, http.MethodGet, "movie/"+strconv.Itoa(id), nil, &m)
	return m.HasFile, err
}

// EpisodeHasFile says whether Sonarr has an episode's file.
func (c *Client) EpisodeHasFile(ctx context.Context, id int) (bool, error) {
	var e struct {
		HasFile bool `json:"hasFile"`
	}
	err := c.do(ctx, http.MethodGet, "episode/"+strconv.Itoa(id), nil, &e)
	return e.HasFile, err
}

func intOf(v any) int {
	switch n := v.(type) {
	case float64:
		return int(n)
	case int:
		return n
	case json.Number:
		i, _ := n.Int64()
		return int(i)
	}
	return 0
}

func tagsOrEmpty(t []int) []int {
	if t == nil {
		return []int{}
	}
	return t
}
