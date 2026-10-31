package subs

import (
	"archive/zip"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"path"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
)

// Subtitle services. Each one searches its catalogue for a movie or an
// episode and fetches a subtitle file. Only services with a proper API are
// here: sites that would need scraping break whenever the page changes.

// Provider names.
const (
	ProviderOpenSubtitles = "opensubtitles"
	ProviderSubDL         = "subdl"
	ProviderSubvault      = "subvault"
	ProviderJimaku        = "jimaku"
)

// Provider is a subtitle service.
type Provider interface {
	Name() string
	// Covers says whether it could have subtitles for q (a TV-only
	// service has none for movies; one for Japanese only for other languages).
	Covers(q Query) bool
	Search(ctx context.Context, q Query) ([]Result, error)
	// Fetch gets a result's subtitle file (unpacked from an archive when
	// the service sends one).
	Fetch(ctx context.Context, r Result) ([]byte, error)
}

// ErrTryLater means the service is busy or refreshing; ask again later.
var ErrTryLater = errors.New("the subtitle service asked to try again later")

// --- OpenSubtitles ---------------------------------------------------------------

// Name implements Provider.
func (o *OpenSubtitles) Name() string { return ProviderOpenSubtitles }

// Covers implements Provider.
func (o *OpenSubtitles) Covers(q Query) bool { return o.APIKey != "" }

// Fetch implements Provider.
func (o *OpenSubtitles) Fetch(ctx context.Context, r Result) ([]byte, error) {
	id := r.FileID
	if id == 0 {
		id, _ = strconv.Atoi(r.Key)
	}
	if id <= 0 {
		return nil, errors.New("OpenSubtitles: no file ID")
	}
	return o.Download(ctx, id)
}

// --- SubDL ------------------------------------------------------------------------

// SubDL is subdl.com: movies and TV in many languages, with a free API key.
type SubDL struct {
	APIKey    string
	UserAgent string
	// Base and DownloadBase are the API and file addresses (the defaults
	// when empty).
	Base, DownloadBase string
	Client             *http.Client
}

func (s *SubDL) client() *http.Client {
	if s.Client != nil {
		return s.Client
	}
	return &http.Client{Timeout: 30 * time.Second}
}

// Name implements Provider.
func (s *SubDL) Name() string { return ProviderSubDL }

// Covers implements Provider.
func (s *SubDL) Covers(q Query) bool { return s.APIKey != "" }

// Search implements Provider.
func (s *SubDL) Search(ctx context.Context, q Query) ([]Result, error) {
	base := s.Base
	if base == "" {
		base = "https://api.subdl.com/api/v1"
	}
	v := url.Values{"api_key": {s.APIKey}, "subs_per_page": {"30"}, "client": {"kinara"}}
	switch {
	case q.TMDBID > 0:
		v.Set("tmdb_id", strconv.Itoa(q.TMDBID))
	case q.IMDBID != "":
		v.Set("imdb_id", q.IMDBID)
	default:
		v.Set("film_name", q.Title)
		if q.Year > 0 {
			v.Set("year", strconv.Itoa(q.Year))
		}
	}
	if q.Episodic {
		v.Set("type", "tv")
		v.Set("season_number", strconv.Itoa(q.Season))
		v.Set("episode_number", strconv.Itoa(q.Episode))
	} else {
		v.Set("type", "movie")
	}
	var langs []string
	for _, l := range q.Languages {
		langs = append(langs, strings.ToUpper(l))
	}
	if len(langs) > 0 {
		v.Set("languages", strings.Join(langs, ","))
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, base+"/subtitles?"+v.Encode(), nil)
	if err != nil {
		return nil, fmt.Errorf("SubDL: %w", withoutURL(err))
	}
	req.Header.Set("User-Agent", s.UserAgent)
	resp, err := s.client().Do(req)
	if err != nil {
		return nil, fmt.Errorf("SubDL: %w", withoutURL(err))
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusTooManyRequests {
		return nil, ErrTryLater
	}
	var body struct {
		Status    bool   `json:"status"`
		Error     string `json:"error"`
		Subtitles []struct {
			ReleaseName string `json:"release_name"`
			Name        string `json:"name"`
			Lang        string `json:"lang"`
			Language    string `json:"language"`
			URL         string `json:"url"`
			Season      int    `json:"season"`
			Episode     *int   `json:"episode"`
			HI          bool   `json:"hi"`
			FullSeason  bool   `json:"full_season"`
		} `json:"subtitles"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 4<<20)).Decode(&body); err != nil {
		return nil, fmt.Errorf("SubDL: %w", err)
	}
	if !body.Status {
		if strings.Contains(strings.ToLower(body.Error), "not found") || body.Error == "" {
			return nil, nil
		}
		return nil, errors.New("SubDL: " + body.Error)
	}
	var out []Result
	for _, sub := range body.Subtitles {
		if sub.URL == "" {
			continue
		}
		// A whole-season pack, or another episode, isn't this episode's.
		if q.Episodic && (sub.FullSeason || (sub.Episode != nil && *sub.Episode != q.Episode)) {
			continue
		}
		lang := Language(sub.Language)
		if lang == "" {
			lang = Language(sub.Lang)
		}
		if lang == "" && len(q.Languages) == 1 {
			lang = Language(q.Languages[0])
		}
		out = append(out, Result{Provider: ProviderSubDL, Key: sub.URL, Language: lang, Release: firstNonEmpty(sub.ReleaseName, sub.Name),
			FileName: path.Base(sub.URL), HearingImpaired: sub.HI, Episode: q.Episode})
	}
	return out, nil
}

// Fetch implements Provider: SubDL sends a zip.
func (s *SubDL) Fetch(ctx context.Context, r Result) ([]byte, error) {
	base := s.DownloadBase
	if base == "" {
		base = "https://dl.subdl.com"
	}
	link := r.Key
	if !strings.HasPrefix(link, "http") {
		link = base + "/" + strings.TrimPrefix(link, "/")
	}
	body, err := get(ctx, s.client(), link, s.UserAgent, nil)
	if err != nil {
		return nil, fmt.Errorf("SubDL: %w", err)
	}
	return unpack(body, r.Episode)
}

// --- Subvault (formerly Gestdown) -------------------------------------------------------

// Subvault is subvault.tv (Gestdown before), an API in front of Addic7ed's
// TV subtitles. TV episodes only, found by the show's name; no key.
type Subvault struct {
	UserAgent string
	// Base is the API's address ("https://api.subvault.tv" when empty).
	Base   string
	Client *http.Client
}

func (g *Subvault) base() string {
	if g.Base != "" {
		return strings.TrimRight(g.Base, "/")
	}
	return "https://api.subvault.tv"
}

func (g *Subvault) client() *http.Client {
	if g.Client != nil {
		return g.Client
	}
	return &http.Client{Timeout: 30 * time.Second}
}

// Name implements Provider.
func (g *Subvault) Name() string { return ProviderSubvault }

// Covers implements Provider: episodes, in the languages Addic7ed has.
func (g *Subvault) Covers(q Query) bool {
	if !q.Episodic || q.Title == "" {
		return false
	}
	for _, l := range q.Languages {
		if addic7edName(Language(l)) != "" {
			return true
		}
	}
	return false
}

// Search implements Provider.
func (g *Subvault) Search(ctx context.Context, q Query) ([]Result, error) {
	var shows struct {
		Shows []struct {
			ID   string `json:"id"`
			Name string `json:"name"`
		} `json:"shows"`
	}
	if err := g.getJSON(ctx, "/shows/search/"+url.PathEscape(q.Title), &shows); err != nil {
		return nil, err
	}
	showID := ""
	for _, s := range shows.Shows {
		if strings.EqualFold(strings.TrimSpace(s.Name), strings.TrimSpace(q.Title)) {
			showID = s.ID
			break
		}
	}
	if showID == "" && len(shows.Shows) == 1 {
		showID = shows.Shows[0].ID
	}
	if showID == "" {
		return nil, nil
	}
	var out []Result
	for _, l := range q.Languages {
		lang := Language(l)
		name := addic7edName(lang)
		if name == "" {
			continue
		}
		var found struct {
			MatchingSubtitles []struct {
				SubtitleID      string `json:"subtitleId"`
				Version         string `json:"version"`
				Completed       bool   `json:"completed"`
				HearingImpaired bool   `json:"hearingImpaired"`
				DownloadURI     string `json:"downloadUri"`
				DownloadCount   int    `json:"downloadCount"`
			} `json:"matchingSubtitles"`
		}
		p := fmt.Sprintf("/subtitles/get/%s/%d/%d/%s", showID, q.Season, q.Episode, url.PathEscape(name))
		if err := g.getJSON(ctx, p, &found); err != nil {
			if errors.Is(err, ErrTryLater) {
				return out, err
			}
			continue
		}
		for _, s := range found.MatchingSubtitles {
			if !s.Completed || s.DownloadURI == "" {
				continue
			}
			out = append(out, Result{Provider: ProviderSubvault, Key: s.DownloadURI, Language: lang, Release: s.Version,
				HearingImpaired: s.HearingImpaired, Downloads: s.DownloadCount})
		}
	}
	return out, nil
}

func (g *Subvault) getJSON(ctx context.Context, p string, out any) error {
	body, err := get(ctx, g.client(), g.base()+p, g.UserAgent, nil)
	if err != nil {
		var se *HTTPError
		if errors.As(err, &se) {
			switch se.Status {
			case http.StatusNotFound:
				return nil
			case http.StatusLocked, http.StatusTooManyRequests:
				return ErrTryLater
			}
		}
		return fmt.Errorf("Subvault: %w", err)
	}
	return json.Unmarshal(body, out)
}

// Fetch implements Provider.
func (g *Subvault) Fetch(ctx context.Context, r Result) ([]byte, error) {
	link := r.Key
	if !strings.HasPrefix(link, "http") {
		link = g.base() + "/" + strings.TrimPrefix(link, "/")
	}
	body, err := get(ctx, g.client(), link, g.UserAgent, nil)
	if err != nil {
		return nil, fmt.Errorf("Subvault: %w", err)
	}
	return unpack(body, 0)
}

// addic7edName is Addic7ed's name for a language ("eng" -> "English").
func addic7edName(code3 string) string {
	return map[string]string{
		"eng": "English", "spa": "Spanish", "fre": "French", "ger": "German", "ita": "Italian", "por": "Portuguese",
		"dut": "Dutch", "pol": "Polish", "rum": "Romanian", "hun": "Hungarian", "gre": "Greek", "tur": "Turkish",
		"rus": "Russian", "swe": "Swedish", "nor": "Norwegian", "dan": "Danish", "fin": "Finnish", "cze": "Czech",
		"heb": "Hebrew", "ara": "Arabic", "chi": "Chinese (Simplified)", "jpn": "Japanese", "kor": "Korean",
		"bul": "Bulgarian", "hrv": "Croatian", "srp": "Serbian (Latin)", "slv": "Slovenian", "per": "Persian",
		"ind": "Indonesian", "may": "Malay", "tha": "Thai", "vie": "Vietnamese", "ukr": "Ukrainian", "cat": "Catalan",
		"est": "Estonian", "lit": "Lithuanian", "lav": "Latvian", "slo": "Slovak", "ice": "Icelandic", "hin": "Hindi",
	}[code3]
}

// --- Jimaku -------------------------------------------------------------------------------

// Jimaku is jimaku.cc: Japanese subtitles for anime (and some live
// action), with the owner's own API key.
type Jimaku struct {
	APIKey    string
	UserAgent string
	Base      string
	Client    *http.Client
}

func (j *Jimaku) base() string {
	if j.Base != "" {
		return strings.TrimRight(j.Base, "/")
	}
	return "https://jimaku.cc/api"
}

func (j *Jimaku) client() *http.Client {
	if j.Client != nil {
		return j.Client
	}
	return &http.Client{Timeout: 30 * time.Second}
}

// Name implements Provider.
func (j *Jimaku) Name() string { return ProviderJimaku }

// Covers implements Provider: Japanese only.
func (j *Jimaku) Covers(q Query) bool {
	if j.APIKey == "" || q.Title == "" {
		return false
	}
	for _, l := range q.Languages {
		if Language(l) == "jpn" {
			return true
		}
	}
	return false
}

// Search implements Provider.
func (j *Jimaku) Search(ctx context.Context, q Query) ([]Result, error) {
	var entries []struct {
		ID    int    `json:"id"`
		Name  string `json:"name"`
		Flags struct {
			Movie bool `json:"movie"`
		} `json:"flags"`
	}
	hdr := map[string]string{"Authorization": j.APIKey}
	search := func(anime bool) error {
		v := url.Values{"query": {q.Title}}
		if !anime {
			v.Set("anime", "false")
		}
		body, err := get(ctx, j.client(), j.base()+"/entries/search?"+v.Encode(), j.UserAgent, hdr)
		if err != nil {
			return err
		}
		return json.Unmarshal(body, &entries)
	}
	if err := search(true); err != nil {
		return nil, fmt.Errorf("Jimaku: %w", err)
	}
	if len(entries) == 0 {
		if err := search(false); err != nil {
			return nil, fmt.Errorf("Jimaku: %w", err)
		}
	}
	if len(entries) == 0 {
		return nil, nil
	}
	entry := entries[0]
	v := url.Values{}
	if q.Episodic {
		v.Set("episode", strconv.Itoa(q.Episode))
	}
	body, err := get(ctx, j.client(), j.base()+"/entries/"+strconv.Itoa(entry.ID)+"/files?"+v.Encode(), j.UserAgent, hdr)
	if err != nil {
		return nil, fmt.Errorf("Jimaku: %w", err)
	}
	var files []struct {
		URL  string `json:"url"`
		Name string `json:"name"`
		Size int    `json:"size"`
	}
	if err := json.Unmarshal(body, &files); err != nil {
		return nil, fmt.Errorf("Jimaku: %w", err)
	}
	var out []Result
	for _, f := range files {
		ext := strings.ToLower(path.Ext(f.Name))
		if f.URL == "" || ext == ".7z" || ext == ".rar" || (f.Size > 0 && f.Size < 500) {
			continue
		}
		out = append(out, Result{Provider: ProviderJimaku, Key: f.URL, Language: "jpn", Release: f.Name, FileName: f.Name})
	}
	return out, nil
}

// Fetch implements Provider.
func (j *Jimaku) Fetch(ctx context.Context, r Result) ([]byte, error) {
	body, err := get(ctx, j.client(), r.Key, j.UserAgent, map[string]string{"Authorization": j.APIKey})
	if err != nil {
		return nil, fmt.Errorf("Jimaku: %w", err)
	}
	return unpack(body, 0)
}

// --- Shared --------------------------------------------------------------------------------

// HTTPError is a service's error answer.
type HTTPError struct {
	Status  int
	Message string
}

func (e *HTTPError) Error() string { return "answered " + e.Message }

func get(ctx context.Context, c *http.Client, link, ua string, headers map[string]string) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, link, nil)
	if err != nil {
		return nil, err
	}
	if ua != "" {
		req.Header.Set("User-Agent", ua)
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	resp, err := c.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, &HTTPError{Status: resp.StatusCode, Message: resp.Status}
	}
	return io.ReadAll(io.LimitReader(resp.Body, 16<<20))
}

var subtitleExts = map[string]int{".srt": 3, ".ass": 2, ".ssa": 2, ".vtt": 1, ".sub": 0}

var episodeTag = regexp.MustCompile(`(?i)(?:s\d{1,2}e|\bep?|x)(\d{1,3})\b`)

// unpack returns a subtitle file as is, or the best one inside a zip
// archive (for an episode, the one named for it when there are several).
func unpack(body []byte, episode int) ([]byte, error) {
	if !bytes.HasPrefix(body, []byte("PK\x03\x04")) {
		return UTF8(body), nil
	}
	zr, err := zip.NewReader(bytes.NewReader(body), int64(len(body)))
	if err != nil {
		return nil, fmt.Errorf("subtitle archive: %w", err)
	}
	type cand struct {
		f    *zip.File
		rank int
	}
	var cands []cand
	for _, f := range zr.File {
		rank, ok := subtitleExts[strings.ToLower(path.Ext(f.Name))]
		if !ok || f.FileInfo().IsDir() || f.UncompressedSize64 > 8<<20 {
			continue
		}
		if episode > 0 {
			if m := episodeTag.FindStringSubmatch(path.Base(f.Name)); m != nil {
				if n, _ := strconv.Atoi(m[1]); n == episode {
					rank += 10
				} else {
					rank -= 10
				}
			}
		}
		cands = append(cands, cand{f, rank})
	}
	if len(cands) == 0 {
		return nil, errors.New("no subtitle file in the archive")
	}
	sort.SliceStable(cands, func(i, j int) bool { return cands[i].rank > cands[j].rank })
	rc, err := cands[0].f.Open()
	if err != nil {
		return nil, err
	}
	defer rc.Close()
	b, err := io.ReadAll(io.LimitReader(rc, 8<<20))
	if err != nil {
		return nil, err
	}
	return UTF8(b), nil
}

func firstNonEmpty(s ...string) string {
	for _, x := range s {
		if strings.TrimSpace(x) != "" {
			return strings.TrimSpace(x)
		}
	}
	return ""
}

// withoutURL drops the address from a failed request's error: SubDL's key
// rides in the query string, and these errors reach the owner's screen
// and the log.
func withoutURL(err error) error {
	var ue *url.Error
	if errors.As(err, &ue) {
		return ue.Err
	}
	return err
}
