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
	"kinara/internal/subsync"
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
	// Provider is the service a download came from; Auto: the server
	// found it by itself; Sync says how checking it against the speech went
	// (synced: its timing was corrected; already: it was right; unchecked;
	// no_match: picked by hand and it doesn't seem to line up).
	Provider string `json:"provider,omitempty"`
	Auto     bool   `json:"auto,omitempty"`
	Sync     string `json:"sync,omitempty"`
	// Wrongable: it can be reported as wrong (downloads).
	Wrongable bool `json:"wrongable,omitempty"`
}

func externalOf(versionID string, f store.SubtitleFile, u store.User) ExternalSubtitle {
	idx := ExternalSubtitleBase + int(f.ID)
	return ExternalSubtitle{Index: idx, Language: f.Language, Title: f.Title, Format: f.Format, Forced: f.Forced,
		HearingImpaired: f.HearingImpaired, Source: f.Source,
		URL:       fmt.Sprintf("/api/v1/subtitles/%s/%d.vtt", versionID, idx),
		Removable: f.Source == store.SubtitleDownload && (u.IsAdmin || f.AddedBy == u.ID),
		Provider:  f.Provider, Auto: f.Auto, Sync: f.SyncState, Wrongable: f.Source == store.SubtitleDownload}
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
	s.autoSubsRoutes(mux)
}

// serveExternalVTT answers /subtitles/{id}/{index}.vtt for a subtitle file.
func (s *Server) serveExternalVTT(w http.ResponseWriter, r *http.Request, v store.Version, index int) {
	f, err := s.Store.SubtitleFile(r.Context(), int64(index-ExternalSubtitleBase))
	if err != nil || f.VersionID != v.ID {
		writeErr(w, http.StatusNotFound, "no subtitle file with that index")
		return
	}
	fi, err := os.Stat(s.dataPath(f.Path))
	if err != nil {
		writeErr(w, http.StatusNotFound, "the subtitle file isn't there any more")
		return
	}
	vtt, err := s.Subtitles.VTTFile(r.Context(), fmt.Sprintf("%s-x%d", v.ID, f.ID), s.dataPath(f.Path), fi.ModTime().Unix())
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	w.Header().Set("Content-Type", "text/vtt; charset=utf-8")
	if t := subtitleTiming(f); !t.IsIdentity() {
		// Lined up with the speech (autosubs.go): the file stays as it came,
		// the correction is applied here. It can change, so no long caching.
		vtt = subsync.RetimeVTT(vtt, t)
		w.Header().Set("Cache-Control", "private, no-cache")
	} else {
		w.Header().Set("Cache-Control", "private, max-age=300")
	}
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

	// Auto finds subtitles by itself in the profiles' languages
	// (autosubs.go); Budget is how many a day.
	Auto   bool `json:"auto"`
	Budget int  `json:"budget"`
	// Providers are the subtitle services, with their keys and today's use.
	Providers []ProviderView `json:"providers"`
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
	st.Auto, st.Budget = get(keyAutoSubs) != "false", s.autoSubsBudget(ctx)
	st.Providers = s.providerViews(ctx)
	return st
}

// SetBuiltinSubDLKey sets Kinara's own SubDL key (built into release builds).
func (s *Server) SetBuiltinSubDLKey(key string) {
	s.subs.mu.Lock()
	s.subs.builtinSubDL = key
	s.subs.mu.Unlock()
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
		Auto     *bool   `json:"auto"`
		Budget   *int    `json:"budget"`
		// SubDLKey and JimakuKey are the owner's keys ("" removes one).
		SubDLKey  *string `json:"subdlKey"`
		JimakuKey *string `json:"jimakuKey"`
		// Providers turns services on or off by name.
		Providers map[string]bool `json:"providers"`
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
	if in.Auto != nil && !set(keyAutoSubs, strconv.FormatBool(*in.Auto)) {
		return
	}
	if in.Budget != nil {
		if *in.Budget < 0 || *in.Budget > 1000 {
			writeErr(w, http.StatusBadRequest, "the daily number must be between 0 and 1000")
			return
		}
		if !set(keyAutoSubsBudget, strconv.Itoa(*in.Budget)) {
			return
		}
	}
	if in.SubDLKey != nil && !set(keySubDLKey, strings.TrimSpace(*in.SubDLKey)) {
		return
	}
	if in.JimakuKey != nil && !set(keyJimakuKey, strings.TrimSpace(*in.JimakuKey)) {
		return
	}
	for name, on := range in.Providers {
		switch name {
		case subs.ProviderOpenSubtitles, subs.ProviderSubDL, subs.ProviderSubvault, subs.ProviderJimaku:
		default:
			continue
		}
		if !set(keyProviderOff+name, strconv.FormatBool(!on)) {
			return
		}
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
			s.subs.mu.Lock()
			s.subs.osAllowed = n
			s.subs.mu.Unlock()
			st.Providers = s.providerViews(ctx)
		}
	}
	// New services or settings: plan and look again.
	go func() {
		s.planAutoSubs(s.BaseCtx)
		s.kickAutoSubs()
	}()
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
	// osAllowed is the owner's OpenSubtitles account's daily downloads (0
	// when unknown or no account).
	osAllowed int
	// builtinSubDL is Kinara's own SubDL key ("" for none).
	builtinSubDL string
	// Service addresses ("" for the real ones; tests point them elsewhere).
	subdlBase, subdlDLBase, subvaultBase, jimakuBase string
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

// searchSubtitles: GET /subtitles/{id}/search?languages=en,es. Every
// subtitle service that's on is asked at once; results are listed with
// the service they came from, the likeliest first.
func (s *Server) searchSubtitles(w http.ResponseWriter, r *http.Request, u store.User) {
	if !s.mayUseSubtitles(r.Context(), u) {
		writeErr(w, http.StatusForbidden, "the server's owner has turned subtitle downloads off for other accounts")
		return
	}
	providers := s.autoProviders(r.Context())
	if len(providers) == 0 {
		writeErr(w, http.StatusNotImplemented, subs.ErrNoKey.Error())
		return
	}
	v, err := s.sourceVersion(r.Context(), r.PathValue("id"))
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
	type answer struct {
		i   int
		res []subs.Result
		err error
	}
	var asked []subs.Provider
	for _, p := range providers {
		if p.Covers(q) {
			asked = append(asked, p)
		}
	}
	ch := make(chan answer, len(asked))
	for i, p := range asked {
		go func() {
			res, err := p.Search(ctx, q)
			ch <- answer{i, res, err}
		}()
	}
	var cands []candidate
	var firstErr error
	for range asked {
		a := <-ch
		if a.err != nil && firstErr == nil {
			firstErr = a.err
		}
		if asked[a.i].Name() == subs.ProviderSubDL {
			_, _ = s.Store.CountSubtitleUse(r.Context(), subs.ProviderSubDL, today())
		}
		for _, res := range a.res {
			cands = append(cands, candidate{Result: res, provider: asked[a.i], order: a.i})
		}
	}
	if len(cands) == 0 && firstErr != nil {
		writeErr(w, http.StatusBadGateway, firstErr.Error())
		return
	}
	rankCandidates(cands, v.RelPath)
	results := make([]subs.Result, 0, len(cands))
	for _, c := range cands {
		results = append(results, c.Result)
	}
	if len(results) > 80 {
		results = results[:80]
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
	// Provider and Key say which result (as search gave them); FileID
	// alone means OpenSubtitles (older apps).
	Provider        string `json:"provider"`
	Key             string `json:"key"`
	FileID          int    `json:"fileId"`
	Language        string `json:"language"`
	Release         string `json:"release"`
	HearingImpaired bool   `json:"hearingImpaired"`
	Forced          bool   `json:"forced"`
}

// downloadSubtitle: POST /subtitles/{id}/download, a result from search.
// The file is kept in the data folder and listed with the video for
// everyone; the same file asked for again isn't downloaded twice. It's
// then checked against the speech in the background, and its timing
// corrected when it's off.
func (s *Server) downloadSubtitle(w http.ResponseWriter, r *http.Request, u store.User) {
	if !s.mayUseSubtitles(r.Context(), u) {
		writeErr(w, http.StatusForbidden, "the server's owner has turned subtitle downloads off for other accounts")
		return
	}
	v, err := s.sourceVersion(r.Context(), r.PathValue("id"))
	if err != nil || v.Missing {
		writeErr(w, http.StatusNotFound, "file not found")
		return
	}
	var req subtitleDownload
	if err := readJSON(r, &req); err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	if req.Provider == "" {
		req.Provider = subs.ProviderOpenSubtitles
	}
	if req.Key == "" && req.FileID > 0 {
		req.Key = strconv.Itoa(req.FileID)
	}
	if req.Key == "" {
		writeErr(w, http.StatusBadRequest, "a result from search is needed")
		return
	}
	var provider subs.Provider
	for _, p := range s.autoProviders(r.Context()) {
		if p.Name() == req.Provider {
			provider = p
		}
	}
	if provider == nil {
		writeErr(w, http.StatusBadRequest, "that subtitle service isn't on")
		return
	}
	have, _ := s.Store.SubtitleFiles(r.Context(), v.ID)
	for _, f := range have {
		if f.Source == store.SubtitleDownload && f.ProviderID == req.Key && firstNonEmpty(f.Provider, subs.ProviderOpenSubtitles) == req.Provider {
			if _, err := os.Stat(s.dataPath(f.Path)); err == nil {
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
	res := subs.Result{Provider: req.Provider, Key: req.Key, FileID: req.FileID, Language: req.Language, Release: req.Release,
		HearingImpaired: req.HearingImpaired, Forced: req.Forced}
	body, err := provider.Fetch(ctx, res)
	_, _ = s.Store.CountSubtitleUse(r.Context(), req.Provider, today())
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
	f, _, err := s.saveSubtitle(r.Context(), v, res, body, u.ID, false)
	if err != nil {
		writeErr(w, http.StatusBadGateway, "the subtitle file couldn't be read: "+err.Error())
		return
	}
	s.Log.Info("subtitle downloaded", "by", u.Username, "version", v.ID, "language", f.Language, "service", req.Provider)
	go s.syncSubtitleFile(v.ID, f.ID)
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
	_ = os.Remove(s.dataPath(f.Path))
	w.WriteHeader(http.StatusNoContent)
}

// dataPrefix marks a path inside the data folder ("kinara-data:artwork/x.png"):
// files Kinara keeps itself are recorded this way, not by full path, so a
// backup restored on another computer (or a moved data folder) still finds
// them.
const dataPrefix = "kinara-data:"

// dataPath turns a recorded path into one on disk.
func (s *Server) dataPath(p string) string {
	if rel, ok := strings.CutPrefix(p, dataPrefix); ok {
		return filepath.Join(s.DataDir, filepath.FromSlash(rel))
	}
	return p
}

// imageSrc turns an artwork address kept in the database into one the
// image cache reads ("file:" for files in the data folder).
func (s *Server) imageSrc(u string) string {
	if strings.HasPrefix(u, dataPrefix) {
		return "file:" + s.dataPath(u)
	}
	return u
}
