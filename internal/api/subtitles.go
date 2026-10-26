package api

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"kinara/internal/auth"
	"kinara/internal/store"
	"kinara/internal/subs"
)

// Subtitle files outside the video: ones next to it (found by the
// scanner) and ones downloaded from OpenSubtitles. Apps list them apart
// from the file's own tracks (VersionView.Subtitles) and fetch them as
// WebVTT from the same address as a track, with an index from
// ExternalSubtitleBase up, so a player that draws text subtitles itself
// needs nothing new to show them.

// ExternalSubtitleBase is added to a subtitle file's ID to make its index.
const ExternalSubtitleBase = 1_000_000

// ExternalSubtitle is a subtitle file as apps see it.
type ExternalSubtitle struct {
	Index           int    `json:"index"`
	Language        string `json:"language,omitempty"`
	Title           string `json:"title,omitempty"`
	Format          string `json:"format"`
	Forced          bool   `json:"forced"`
	HearingImpaired bool   `json:"hearingImpaired"`
	// Source is sidecar (next to the video) or download.
	Source string `json:"source"`
	// URL is the WebVTT.
	URL string `json:"url"`
	// Removable: a download this account may delete.
	Removable bool `json:"removable,omitempty"`
}

func externalOf(versionID string, f store.SubtitleFile, u store.User) ExternalSubtitle {
	idx := ExternalSubtitleBase + int(f.ID)
	return ExternalSubtitle{Index: idx, Language: f.Language, Title: f.Title, Format: f.Format, Forced: f.Forced,
		HearingImpaired: f.HearingImpaired, Source: f.Source,
		URL:       fmt.Sprintf("/api/v1/subtitles/%s/%d.vtt", versionID, idx),
		Removable: f.Source == store.SubtitleDownload && (u.IsAdmin || f.AddedBy == u.ID)}
}

func (s *Server) externalSubtitles(ctx context.Context, versionID string, u store.User) []ExternalSubtitle {
	files, err := s.Store.SubtitleFiles(ctx, versionID)
	if err != nil {
		return nil
	}
	out := make([]ExternalSubtitle, 0, len(files))
	for _, f := range files {
		out = append(out, externalOf(versionID, f, u))
	}
	return out
}

func (s *Server) subtitleRoutes(mux *http.ServeMux) {
	mux.Handle("GET /api/v1/subtitles/{id}/search", s.user(s.searchSubtitles))
	mux.Handle("POST /api/v1/subtitles/{id}/download", s.user(s.downloadSubtitle))
	mux.Handle("DELETE /api/v1/subtitles/{id}/files/{file}", s.user(s.deleteSubtitleFile))
	mux.Handle("GET /api/v1/subtitles/settings", s.admin(s.getSubtitleSettings))
	mux.Handle("PUT /api/v1/subtitles/settings", s.admin(s.putSubtitleSettings))
}

// serveExternalVTT answers /subtitles/{id}/{index}.vtt for a subtitle file.
func (s *Server) serveExternalVTT(w http.ResponseWriter, r *http.Request, v store.Version, index int) {
	f, err := s.Store.SubtitleFile(r.Context(), int64(index-ExternalSubtitleBase))
	if err != nil || f.VersionID != v.ID {
		writeErr(w, http.StatusNotFound, "no subtitle file with that index")
		return
	}
	fi, err := os.Stat(f.Path)
	if err != nil {
		writeErr(w, http.StatusNotFound, "the subtitle file isn't there any more")
		return
	}
	vtt, err := s.Subtitles.VTTFile(r.Context(), fmt.Sprintf("%s-x%d", v.ID, f.ID), f.Path, fi.ModTime().Unix())
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	w.Header().Set("Content-Type", "text/vtt; charset=utf-8")
	w.Header().Set("Cache-Control", "private, max-age=3600")
	w.Write(vtt)
}

// --- Settings ---------------------------------------------------------------------

// SubtitleSettings are the owner's choices (GET and PUT /subtitles/settings).
type SubtitleSettings struct {
	// Everyone lets every account search and download subtitles; off,
	// only owners can.
	Everyone bool `json:"everyone"`
	// CleanDVD redraws DVD subtitles in Kinara's style (white, outlined,
	// smooth) instead of the disc's colours and blocky pixels.
	CleanDVD bool `json:"cleanDvd"`
	// Username is the owner's OpenSubtitles account, for more downloads a
	// day; Password is only ever sent in, never back (HasPassword says
	// one is saved).
	Username    string `json:"username"`
	Password    string `json:"password,omitempty"`
	HasPassword bool   `json:"hasPassword"`
	// APIKey is the owner's own OpenSubtitles API key ("" for Kinara's).
	// Only its last 4 characters come back.
	APIKey string `json:"apiKey,omitempty"`
	// Available says whether searching works at all (there's a key).
	Available bool `json:"available"`
	// Account says how the account check went (on PUT with an account).
	Account string `json:"account,omitempty"`
}

const (
	keySubsEveryone = "subtitles_everyone"
	keySubsCleanDVD = "subtitles_clean_dvd"
	keySubsUser     = "opensubtitles_username"
	keySubsPass     = "opensubtitles_password"
	keySubsAPIKey   = "opensubtitles_api_key"
)

func (s *Server) subtitleSettings(ctx context.Context) SubtitleSettings {
	get := func(k string) string { v, _ := s.Store.Value(ctx, k); return v }
	st := SubtitleSettings{Everyone: get(keySubsEveryone) != "false", CleanDVD: get(keySubsCleanDVD) != "false",
		Username: get(keySubsUser), HasPassword: get(keySubsPass) != ""}
	if k := get(keySubsAPIKey); k != "" {
		st.APIKey = "…" + k[max(0, len(k)-4):]
	}
	st.Available = s.subs.client != nil && s.subs.client.APIKey != ""
	return st
}

// ApplySubtitleSettings sets up the OpenSubtitles client and the DVD look
// from the saved settings. Call it once at start; saving calls it again.
// builtinKey is Kinara's own API key ("" for none).
func (s *Server) ApplySubtitleSettings(ctx context.Context, builtinKey string) {
	s.subs.mu.Lock()
	defer s.subs.mu.Unlock()
	if builtinKey != "" {
		s.subs.builtinKey = builtinKey
	}
	get := func(k string) string { v, _ := s.Store.Value(ctx, k); return v }
	key := get(keySubsAPIKey)
	if key == "" {
		key = s.subs.builtinKey
	}
	if s.subs.client == nil {
		s.subs.client = &subs.OpenSubtitles{UserAgent: "Kinara v" + strings.TrimSuffix(s.Version, "-dev")}
	}
	s.subs.client.APIKey = key
	s.subs.client.SetAccount(get(keySubsUser), get(keySubsPass))
	if s.PGS != nil {
		s.PGS.SetOriginalDVD(get(keySubsCleanDVD) == "false")
	}
}

func (s *Server) getSubtitleSettings(w http.ResponseWriter, r *http.Request, u store.User) {
	writeJSON(w, http.StatusOK, s.subtitleSettings(r.Context()))
}

func (s *Server) putSubtitleSettings(w http.ResponseWriter, r *http.Request, u store.User) {
	var in struct {
		Everyone *bool   `json:"everyone"`
		CleanDVD *bool   `json:"cleanDvd"`
		Username *string `json:"username"`
		Password *string `json:"password"`
		APIKey   *string `json:"apiKey"`
	}
	if err := readJSON(r, &in); err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	ctx := r.Context()
	set := func(k, v string) bool {
		if err := s.Store.SetValue(ctx, k, v); err != nil {
			writeErr(w, http.StatusInternalServerError, err.Error())
			return false
		}
		return true
	}
	if in.Everyone != nil && !set(keySubsEveryone, strconv.FormatBool(*in.Everyone)) {
		return
	}
	if in.CleanDVD != nil && !set(keySubsCleanDVD, strconv.FormatBool(*in.CleanDVD)) {
		return
	}
	if in.Username != nil {
		user := strings.TrimSpace(*in.Username)
		if !set(keySubsUser, user) {
			return
		}
		if user == "" && !set(keySubsPass, "") {
			return
		}
	}
	if in.Password != nil && *in.Password != "" && !set(keySubsPass, *in.Password) {
		return
	}
	if in.APIKey != nil && !set(keySubsAPIKey, strings.TrimSpace(*in.APIKey)) {
		return
	}
	s.ApplySubtitleSettings(ctx, "")
	st := s.subtitleSettings(ctx)
	// A changed account is tried straight away, so the owner knows.
	if (in.Username != nil || in.Password != nil) && st.Username != "" && st.HasPassword && s.subs.client.APIKey != "" {
		cctx, cancel := context.WithTimeout(ctx, 20*time.Second)
		n, err := s.subs.client.CheckAccount(cctx)
		cancel()
		if err != nil {
			st.Account = err.Error()
		} else {
			st.Account = fmt.Sprintf("Signed in: %d downloads a day.", n)
		}
	}
	writeJSON(w, http.StatusOK, st)
}

// --- Search and download ------------------------------------------------------------------

// subsState holds the OpenSubtitles client, recent searches and the
// per-account download limit.
type subsState struct {
	mu         sync.Mutex
	client     *subs.OpenSubtitles
	builtinKey string
	cache      map[string]subsCacheEntry
	limiter    *auth.Limiter
}

type subsCacheEntry struct {
	at      time.Time
	results []subs.Result
}

func (s *Server) mayUseSubtitles(ctx context.Context, u store.User) bool {
	if u.IsAdmin {
		return true
	}
	v, _ := s.Store.Value(ctx, keySubsEveryone)
	return v != "false"
}

// subtitleQuery says what to search OpenSubtitles for, for a version.
func (s *Server) subtitleQuery(ctx context.Context, v store.Version) (subs.Query, error) {
	it, err := s.Store.GetItem(ctx, v.ItemID)
	if err != nil {
		return subs.Query{}, err
	}
	q := subs.Query{Title: it.Title, Year: it.Year}
	switch it.Kind {
	case store.ItemMovie:
		if id, _ := s.Store.ExternalID(ctx, it.ID, store.ProviderTMDBMovie); id != "" {
			q.TMDBID, _ = strconv.Atoi(id)
		}
		q.IMDBID, _ = s.Store.ExternalID(ctx, it.ID, store.ProviderIMDB)
	case store.ItemEpisode:
		q.Episodic, q.Season, q.Episode = true, it.SeasonNumber, it.EpisodeNumber
		show := it
		for i := 0; i < 2 && show.Kind != store.ItemShow && show.ParentID != ""; i++ {
			if show, err = s.Store.GetItem(ctx, show.ParentID); err != nil {
				return q, err
			}
		}
		q.Title, q.Year = show.Title, show.Year
		if id, _ := s.Store.ExternalID(ctx, show.ID, store.ProviderTMDBTV); id != "" {
			q.TMDBID, _ = strconv.Atoi(id)
		}
		q.IMDBID, _ = s.Store.ExternalID(ctx, show.ID, store.ProviderIMDB)
	default:
		return q, errors.New("subtitles can be found for movies and episodes")
	}
	if h, err := subs.Hash(v.Path); err == nil {
		q.Hash = h
	}
	return q, nil
}

// searchSubtitles: GET /subtitles/{id}/search?languages=en,es
func (s *Server) searchSubtitles(w http.ResponseWriter, r *http.Request, u store.User) {
	if !s.mayUseSubtitles(r.Context(), u) {
		writeErr(w, http.StatusForbidden, "the server's owner has turned subtitle downloads off for other accounts")
		return
	}
	c := s.subs.client
	if c == nil || c.APIKey == "" {
		writeErr(w, http.StatusNotImplemented, subs.ErrNoKey.Error())
		return
	}
	v, err := s.Store.GetVersion(r.Context(), r.PathValue("id"))
	if err != nil || v.Missing || v.ItemID == "" {
		writeErr(w, http.StatusNotFound, "file not found")
		return
	}
	var langs []string
	for _, l := range strings.Split(r.URL.Query().Get("languages"), ",") {
		if c2 := subs.Code2(subs.Language(l)); c2 != "" {
			langs = append(langs, c2)
		}
	}
	if len(langs) == 0 {
		langs = []string{"en"}
	}
	key := v.ID + "|" + strings.Join(langs, ",")
	s.subs.mu.Lock()
	if e, ok := s.subs.cache[key]; ok && time.Since(e.at) < 30*time.Minute {
		s.subs.mu.Unlock()
		writeJSON(w, http.StatusOK, map[string]any{"results": e.results})
		return
	}
	s.subs.mu.Unlock()

	q, err := s.subtitleQuery(r.Context(), v)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	q.Languages = langs
	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	defer cancel()
	results, err := c.Search(ctx, q)
	if err != nil {
		writeErr(w, http.StatusBadGateway, err.Error())
		return
	}
	if len(results) > 60 {
		results = results[:60]
	}
	if results == nil {
		results = []subs.Result{}
	}
	s.subs.mu.Lock()
	if s.subs.cache == nil || len(s.subs.cache) > 500 {
		s.subs.cache = map[string]subsCacheEntry{}
	}
	s.subs.cache[key] = subsCacheEntry{at: time.Now(), results: results}
	s.subs.mu.Unlock()
	writeJSON(w, http.StatusOK, map[string]any{"results": results})
}

type subtitleDownload struct {
	FileID          int    `json:"fileId"`
	Language        string `json:"language"`
	Release         string `json:"release"`
	HearingImpaired bool   `json:"hearingImpaired"`
	Forced          bool   `json:"forced"`
}

// downloadSubtitle: POST /subtitles/{id}/download, a result from search.
// The file is kept in the data folder and listed with the video for
// everyone; the same file asked for again isn't downloaded twice.
func (s *Server) downloadSubtitle(w http.ResponseWriter, r *http.Request, u store.User) {
	if !s.mayUseSubtitles(r.Context(), u) {
		writeErr(w, http.StatusForbidden, "the server's owner has turned subtitle downloads off for other accounts")
		return
	}
	c := s.subs.client
	if c == nil || c.APIKey == "" {
		writeErr(w, http.StatusNotImplemented, subs.ErrNoKey.Error())
		return
	}
	v, err := s.Store.GetVersion(r.Context(), r.PathValue("id"))
	if err != nil || v.Missing {
		writeErr(w, http.StatusNotFound, "file not found")
		return
	}
	var req subtitleDownload
	if err := readJSON(r, &req); err != nil || req.FileID <= 0 {
		writeErr(w, http.StatusBadRequest, "fileId from a search result is needed")
		return
	}
	provider := strconv.Itoa(req.FileID)
	have, _ := s.Store.SubtitleFiles(r.Context(), v.ID)
	for _, f := range have {
		if f.Source == store.SubtitleDownload && f.ProviderID == provider {
			if _, err := os.Stat(f.Path); err == nil {
				writeJSON(w, http.StatusOK, externalOf(v.ID, f, u))
				return
			}
		}
	}
	// Each account gets a share of the day's downloads, so one can't use
	// up the server's allowance.
	s.subs.mu.Lock()
	if s.subs.limiter == nil {
		s.subs.limiter = &auth.Limiter{Max: 30, Window: 24 * time.Hour}
	}
	lim := s.subs.limiter
	s.subs.mu.Unlock()
	who := "user:" + strconv.FormatInt(u.ID, 10)
	if !u.IsAdmin && lim.Allow(who) != nil {
		writeErr(w, http.StatusTooManyRequests, "that's the most subtitles one account can download today")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 60*time.Second)
	defer cancel()
	body, err := c.Download(ctx, req.FileID)
	if err != nil {
		status := http.StatusBadGateway
		var se *subs.StatusError
		if errors.As(err, &se) && (se.Status == http.StatusNotAcceptable || se.Status == http.StatusTooManyRequests) {
			status = http.StatusTooManyRequests
		}
		writeErr(w, status, err.Error())
		return
	}
	if !u.IsAdmin {
		lim.Fail(who)
	}
	lang := subs.Language(req.Language)
	dir := filepath.Join(s.DataDir, "subtitles", v.ID)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	name := "opensubtitles-" + provider
	if lang != "" {
		name += "." + lang
	}
	path := filepath.Join(dir, name+".srt")
	if err := os.WriteFile(path, subs.UTF8(body), 0o644); err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	title := strings.TrimSpace(req.Release)
	if len(title) > 80 {
		title = title[:80]
	}
	f := store.SubtitleFile{VersionID: v.ID, Path: path, Language: lang, Title: title, Format: "subrip",
		Forced: req.Forced, HearingImpaired: req.HearingImpaired, Source: store.SubtitleDownload,
		ProviderID: provider, AddedBy: u.ID}
	if err := s.Store.AddSubtitleFile(r.Context(), &f); err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	s.Log.Info("subtitle downloaded", "by", u.Username, "version", v.ID, "language", lang, "opensubtitles", provider)
	writeJSON(w, http.StatusCreated, externalOf(v.ID, f, u))
}

// deleteSubtitleFile removes a downloaded subtitle (owners, or whoever
// downloaded it). Files next to the video are the owner's own: Kinara
// doesn't delete them.
func (s *Server) deleteSubtitleFile(w http.ResponseWriter, r *http.Request, u store.User) {
	idx, err := strconv.Atoi(r.PathValue("file"))
	if err != nil {
		writeErr(w, http.StatusBadRequest, "bad subtitle")
		return
	}
	id := int64(idx)
	if idx >= ExternalSubtitleBase {
		id = int64(idx - ExternalSubtitleBase)
	}
	f, err := s.Store.SubtitleFile(r.Context(), id)
	if err != nil || f.VersionID != r.PathValue("id") {
		writeErr(w, http.StatusNotFound, "no such subtitle file")
		return
	}
	if f.Source != store.SubtitleDownload {
		writeErr(w, http.StatusConflict, "that subtitle file is next to the video; remove it from the folder instead")
		return
	}
	if !u.IsAdmin && f.AddedBy != u.ID {
		writeErr(w, http.StatusForbidden, "only the owner or whoever downloaded it can remove it")
		return
	}
	if err := s.Store.DeleteSubtitleFile(r.Context(), f.ID); err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	_ = os.Remove(f.Path)
	w.WriteHeader(http.StatusNoContent)
}
