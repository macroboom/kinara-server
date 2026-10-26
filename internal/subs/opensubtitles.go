package subs

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode/utf8"
)

// OpenSubtitles talks to the OpenSubtitles.com REST API.
//
// Every request carries an API key (Kinara's own, built into release
// builds, or the owner's) and Kinara's name as the User-Agent. Searching
// needs nothing else. Downloads are counted per day: without an account by
// the server's address, with the owner's OpenSubtitles account (optional)
// by that account, which allows more.
type OpenSubtitles struct {
	APIKey    string
	UserAgent string
	// Username and Password are the owner's OpenSubtitles account ("" for
	// none).
	Username, Password string
	// Base is the API's address (DefaultBase when empty); a signed-in
	// account may be sent to another.
	Base   string
	Client *http.Client

	mu       sync.Mutex
	token    string
	tokenFor string
	tokenAt  time.Time
	userBase string
}

// DefaultBase is the OpenSubtitles REST API.
const DefaultBase = "https://api.opensubtitles.com/api/v1"

// ErrNoKey means there's no API key to ask with.
var ErrNoKey = errors.New("subtitle search isn't set up on this server (no OpenSubtitles API key)")

// Query is what to look for.
type Query struct {
	// TMDBID is the movie's, or for an episode the show's (with Season and
	// Episode).
	TMDBID          int
	IMDBID          string
	Season, Episode int
	Episodic        bool
	// Title and Year are used when there's no ID.
	Title string
	Year  int
	// Languages are two-letter codes ("en", "es").
	Languages []string
	// Hash is the file's OpenSubtitles hash (see Hash), for exact matches.
	Hash string
}

// Result is one subtitle file found.
type Result struct {
	FileID   int    `json:"fileId"`
	Language string `json:"language"` // three-letter code, as tracks use
	// Release is the release the subtitle was timed for.
	Release         string  `json:"release"`
	FileName        string  `json:"fileName"`
	Downloads       int     `json:"downloads"`
	Rating          float64 `json:"rating"`
	HearingImpaired bool    `json:"hearingImpaired"`
	Forced          bool    `json:"forced"`
	// AI and Machine mark translations not made by a person.
	AI      bool    `json:"ai"`
	Machine bool    `json:"machine"`
	Trusted bool    `json:"trusted"`
	FPS     float64 `json:"fps,omitempty"`
	// HashMatch: made for this exact file, so the timing fits.
	HashMatch bool   `json:"hashMatch"`
	Uploader  string `json:"uploader,omitempty"`
}

func (o *OpenSubtitles) client() *http.Client {
	if o.Client != nil {
		return o.Client
	}
	return &http.Client{Timeout: 30 * time.Second}
}

func (o *OpenSubtitles) base() string {
	o.mu.Lock()
	defer o.mu.Unlock()
	if o.userBase != "" {
		return o.userBase
	}
	if o.Base != "" {
		return o.Base
	}
	return DefaultBase
}

// apiError is how the API says what went wrong.
type apiError struct {
	Message string   `json:"message"`
	Errors  []string `json:"errors"`
	Status  int      `json:"status"`
}

func (o *OpenSubtitles) do(ctx context.Context, method, path string, q url.Values, body any, auth bool, out any) error {
	if o.APIKey == "" {
		return ErrNoKey
	}
	// The API wants its query lower case and in alphabetical order (it
	// redirects otherwise); url.Values encodes sorted.
	u := o.base() + path
	if len(q) > 0 {
		u += "?" + q.Encode()
	}
	var rd io.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		rd = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, u, rd)
	if err != nil {
		return err
	}
	req.Header.Set("Api-Key", o.APIKey)
	req.Header.Set("User-Agent", o.UserAgent)
	req.Header.Set("Accept", "application/json")
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if auth {
		if tok, err := o.login(ctx); err != nil {
			return err
		} else if tok != "" {
			req.Header.Set("Authorization", "Bearer "+tok)
		}
	}
	resp, err := o.client().Do(req)
	if err != nil {
		return fmt.Errorf("OpenSubtitles: %w", err)
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if resp.StatusCode >= 300 {
		var e apiError
		_ = json.Unmarshal(data, &e)
		msg := e.Message
		if msg == "" && len(e.Errors) > 0 {
			msg = strings.Join(e.Errors, "; ")
		}
		if msg == "" {
			msg = resp.Status
		}
		if resp.StatusCode == http.StatusUnauthorized && auth {
			o.mu.Lock()
			o.token = ""
			o.mu.Unlock()
		}
		return &StatusError{Status: resp.StatusCode, Message: msg}
	}
	if out != nil {
		if err := json.Unmarshal(data, out); err != nil {
			return fmt.Errorf("OpenSubtitles: unreadable answer: %w", err)
		}
	}
	return nil
}

// StatusError is an error answer from OpenSubtitles.
type StatusError struct {
	Status  int
	Message string
}

func (e *StatusError) Error() string {
	switch e.Status {
	case http.StatusNotAcceptable, http.StatusTooManyRequests:
		return "OpenSubtitles: the download limit for today is used up (" + e.Message + ")"
	case http.StatusUnauthorized, http.StatusForbidden:
		return "OpenSubtitles turned the request down: " + e.Message
	}
	return fmt.Sprintf("OpenSubtitles answered %d: %s", e.Status, e.Message)
}

// login signs in the owner's account, keeping the token for a day. With
// no account it returns "".
func (o *OpenSubtitles) login(ctx context.Context) (string, error) {
	o.mu.Lock()
	user, pass := o.Username, o.Password
	if user == "" || pass == "" {
		o.mu.Unlock()
		return "", nil
	}
	if o.token != "" && o.tokenFor == user && time.Since(o.tokenAt) < 23*time.Hour {
		t := o.token
		o.mu.Unlock()
		return t, nil
	}
	o.mu.Unlock()
	var out struct {
		Token   string `json:"token"`
		BaseURL string `json:"base_url"`
	}
	if err := o.do(ctx, http.MethodPost, "/login", nil, map[string]string{"username": user, "password": pass}, false, &out); err != nil {
		var se *StatusError
		if errors.As(err, &se) && (se.Status == http.StatusUnauthorized || se.Status == http.StatusBadRequest) {
			return "", errors.New("OpenSubtitles didn't accept the account's user name and password (Manage, Subtitles)")
		}
		return "", err
	}
	o.mu.Lock()
	o.token, o.tokenFor, o.tokenAt = out.Token, user, time.Now()
	// A VIP account is sent to its own address.
	if out.BaseURL != "" && o.Base == "" {
		host := strings.TrimSuffix(strings.TrimPrefix(strings.TrimPrefix(out.BaseURL, "https://"), "http://"), "/")
		if host != "" && !strings.ContainsAny(host, "/?#") {
			o.userBase = "https://" + host + "/api/v1"
		}
	}
	o.mu.Unlock()
	return out.Token, nil
}

// SetAccount changes the owner's account (signing in again on next use).
func (o *OpenSubtitles) SetAccount(user, pass string) {
	o.mu.Lock()
	o.Username, o.Password, o.token, o.userBase = user, pass, "", ""
	o.mu.Unlock()
}

// CheckAccount signs in, to tell the owner whether the account works, and
// returns how many downloads it has a day.
func (o *OpenSubtitles) CheckAccount(ctx context.Context) (allowed int, err error) {
	if _, err := o.login(ctx); err != nil {
		return 0, err
	}
	var out struct {
		Data struct {
			AllowedDownloads int    `json:"allowed_downloads"`
			Level            string `json:"level"`
		} `json:"data"`
	}
	if err := o.do(ctx, http.MethodGet, "/infos/user", nil, nil, true, &out); err != nil {
		return 0, err
	}
	return out.Data.AllowedDownloads, nil
}

type searchResponse struct {
	Data []struct {
		ID         string `json:"id"`
		Attributes struct {
			Language          string  `json:"language"`
			DownloadCount     int     `json:"download_count"`
			HearingImpaired   bool    `json:"hearing_impaired"`
			ForeignPartsOnly  bool    `json:"foreign_parts_only"`
			AITranslated      bool    `json:"ai_translated"`
			MachineTranslated bool    `json:"machine_translated"`
			FromTrusted       bool    `json:"from_trusted"`
			Ratings           float64 `json:"ratings"`
			FPS               float64 `json:"fps"`
			Release           string  `json:"release"`
			MoviehashMatch    bool    `json:"moviehash_match"`
			Uploader          struct {
				Name string `json:"name"`
			} `json:"uploader"`
			Files []struct {
				FileID   int    `json:"file_id"`
				FileName string `json:"file_name"`
			} `json:"files"`
		} `json:"attributes"`
	} `json:"data"`
}

// Search finds subtitles, best first: made for this exact file, then
// made by people over machine translations, then the most downloaded.
func (o *OpenSubtitles) Search(ctx context.Context, q Query) ([]Result, error) {
	v := url.Values{}
	if len(q.Languages) > 0 {
		langs := append([]string(nil), q.Languages...)
		sort.Strings(langs)
		v.Set("languages", strings.ToLower(strings.Join(langs, ",")))
	}
	if q.Hash != "" {
		v.Set("moviehash", q.Hash)
	}
	switch {
	case q.Episodic && q.TMDBID > 0:
		v.Set("parent_tmdb_id", strconv.Itoa(q.TMDBID))
	case q.TMDBID > 0:
		v.Set("tmdb_id", strconv.Itoa(q.TMDBID))
	case q.IMDBID != "":
		key := "imdb_id"
		if q.Episodic {
			key = "parent_imdb_id"
		}
		v.Set(key, strings.TrimPrefix(strings.ToLower(q.IMDBID), "tt"))
	default:
		v.Set("query", strings.ToLower(q.Title))
		if q.Year > 0 {
			v.Set("year", strconv.Itoa(q.Year))
		}
	}
	if q.Episodic {
		v.Set("season_number", strconv.Itoa(q.Season))
		v.Set("episode_number", strconv.Itoa(q.Episode))
		v.Set("type", "episode")
	} else {
		v.Set("type", "movie")
	}
	var resp searchResponse
	if err := o.do(ctx, http.MethodGet, "/subtitles", v, nil, false, &resp); err != nil {
		return nil, err
	}
	var out []Result
	for _, d := range resp.Data {
		a := d.Attributes
		for _, f := range a.Files {
			out = append(out, Result{
				FileID: f.FileID, Language: Language(a.Language), Release: a.Release, FileName: f.FileName,
				Downloads: a.DownloadCount, Rating: a.Ratings, HearingImpaired: a.HearingImpaired,
				Forced: a.ForeignPartsOnly, AI: a.AITranslated, Machine: a.MachineTranslated,
				Trusted: a.FromTrusted, FPS: a.FPS, HashMatch: a.MoviehashMatch, Uploader: a.Uploader.Name,
			})
		}
	}
	sort.SliceStable(out, func(i, j int) bool {
		a, b := out[i], out[j]
		if a.HashMatch != b.HashMatch {
			return a.HashMatch
		}
		if (a.AI || a.Machine) != (b.AI || b.Machine) {
			return !(a.AI || a.Machine)
		}
		return a.Downloads > b.Downloads
	})
	return out, nil
}

// Download fetches a subtitle file as SubRip, UTF-8.
func (o *OpenSubtitles) Download(ctx context.Context, fileID int) ([]byte, error) {
	var out struct {
		Link      string `json:"link"`
		FileName  string `json:"file_name"`
		Remaining int    `json:"remaining"`
		Message   string `json:"message"`
	}
	if err := o.do(ctx, http.MethodPost, "/download", nil, map[string]any{"file_id": fileID, "sub_format": "srt"}, true, &out); err != nil {
		return nil, err
	}
	if out.Link == "" {
		msg := out.Message
		if msg == "" {
			msg = "no download link"
		}
		return nil, errors.New("OpenSubtitles: " + msg)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, out.Link, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", o.UserAgent)
	resp, err := o.client().Do(req)
	if err != nil {
		return nil, fmt.Errorf("OpenSubtitles download: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("OpenSubtitles download: %s", resp.Status)
	}
	return io.ReadAll(io.LimitReader(resp.Body, 16<<20))
}

// Hash is the OpenSubtitles hash of a video file: its size plus the sum
// of the first and last 64 KB read as little-endian 64-bit numbers.
func Hash(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	fi, err := f.Stat()
	if err != nil {
		return "", err
	}
	const chunk = 64 << 10
	size := fi.Size()
	if size < chunk {
		return "", errors.New("file too small to hash")
	}
	sum := uint64(size)
	buf := make([]byte, chunk)
	for _, off := range []int64{0, size - chunk} {
		if _, err := f.ReadAt(buf, off); err != nil {
			return "", err
		}
		for i := 0; i < chunk; i += 8 {
			sum += binary.LittleEndian.Uint64(buf[i:])
		}
	}
	return fmt.Sprintf("%016x", sum), nil
}

// UTF8 returns subtitle text as UTF-8 without a byte order mark. Text that
// isn't UTF-8 is taken as Windows-1252, which most older subtitle files in
// Western languages are.
func UTF8(b []byte) []byte {
	b = bytes.TrimPrefix(b, []byte{0xEF, 0xBB, 0xBF})
	if utf8.Valid(b) {
		return b
	}
	var out bytes.Buffer
	out.Grow(len(b) + len(b)/8)
	for _, c := range b {
		if c >= 0x80 && c < 0xA0 {
			if r := cp1252[c-0x80]; r != 0 {
				out.WriteRune(r)
				continue
			}
		}
		out.WriteRune(rune(c))
	}
	return out.Bytes()
}

// cp1252 is Windows-1252's 0x80 to 0x9F (0: unused).
var cp1252 = [32]rune{
	0x20AC, 0, 0x201A, 0x0192, 0x201E, 0x2026, 0x2020, 0x2021, 0x02C6, 0x2030, 0x0160, 0x2039, 0x0152, 0, 0x017D, 0,
	0, 0x2018, 0x2019, 0x201C, 0x201D, 0x2022, 0x2013, 0x2014, 0x02DC, 0x2122, 0x0161, 0x203A, 0x0153, 0, 0x017E, 0x0178,
}
