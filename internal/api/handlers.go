package api

import (
	"errors"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"time"

	"kinora/internal/auth"
	"kinora/internal/images"
	"kinora/internal/scanner"
	"kinora/internal/store"
)

func (s *Server) health(w http.ResponseWriter, r *http.Request) {
	n, err := s.Store.CountUsers(r.Context())
	writeJSON(w, http.StatusOK, map[string]any{
		"ok":        err == nil,
		"version":   s.Version,
		"setupDone": n > 0,
		"metadata":  s.Matcher != nil,
	})
}

// --- Auth ---

type loginRequest struct {
	Username   string `json:"username"`
	Password   string `json:"password"`
	DeviceName string `json:"deviceName"`
}

func (s *Server) login(w http.ResponseWriter, r *http.Request) {
	var req loginRequest
	if err := readJSON(r, &req); err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	device := req.DeviceName
	if device == "" {
		device = r.UserAgent()
	}
	token, u, err := s.signIn(r, req.Username, req.Password, device)
	if err != nil {
		status := http.StatusUnauthorized
		if errors.Is(err, auth.ErrTooManyAttempts) {
			status = http.StatusTooManyRequests
		}
		writeErr(w, status, err.Error())
		return
	}
	profiles, _ := s.Store.Profiles(r.Context(), u.ID)
	if profiles == nil {
		profiles = []store.Profile{}
	}
	writeJSON(w, http.StatusOK, map[string]any{"token": token, "user": u, "profiles": profiles})
}

var errBadLogin = errors.New("wrong username or password")

// signIn checks a password and creates a device session.
func (s *Server) signIn(r *http.Request, username, password, device string) (string, store.User, error) {
	keys := []string{"ip:" + clientIP(r), "user:" + strings.ToLower(username)}
	if err := s.limiter.Allow(keys...); err != nil {
		return "", store.User{}, err
	}
	u, err := s.Store.UserByName(r.Context(), username)
	if err != nil || !auth.CheckPassword(u.PasswordHash, password) {
		s.limiter.Fail(keys...)
		return "", store.User{}, errBadLogin
	}
	s.limiter.Reset(keys...)
	token, hash, err := auth.NewToken()
	if err != nil {
		return "", store.User{}, err
	}
	if len(device) > 120 {
		device = device[:120]
	}
	if err := s.Store.CreateSession(r.Context(), hash, u.ID, device, time.Now().Add(sessionTTL).Unix()); err != nil {
		return "", store.User{}, err
	}
	return token, u, nil
}

func (s *Server) logout(w http.ResponseWriter, r *http.Request, u store.User) {
	_ = s.Store.DeleteSession(r.Context(), sessionFrom(r).tokenHash)
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) me(w http.ResponseWriter, r *http.Request, u store.User) {
	writeJSON(w, http.StatusOK, u)
}

// --- Libraries ---

type libraryView struct {
	store.Library
	Stats store.LibraryStats `json:"stats"`
	Scan  *scanner.Progress  `json:"scan,omitempty"`
}

func (s *Server) libraryViews(r *http.Request) ([]libraryView, error) {
	libs, err := s.Store.ListLibraries(r.Context())
	if err != nil {
		return nil, err
	}
	out := make([]libraryView, 0, len(libs))
	for _, l := range libs {
		st, err := s.Store.LibraryStats(r.Context(), l.ID)
		if err != nil {
			return nil, err
		}
		v := libraryView{Library: l, Stats: st}
		if p, ok := s.Scanner.Progress(l.ID); ok {
			v.Scan = &p
		}
		out = append(out, v)
	}
	return out, nil
}

func (s *Server) listLibraries(w http.ResponseWriter, r *http.Request, u store.User) {
	views, err := s.libraryViews(r)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, views)
}

type createLibraryRequest struct {
	Name string `json:"name"`
	Path string `json:"path"`
	Kind string `json:"kind"`
}

// addLibrary validates and creates a library, then starts its first scan.
func (s *Server) addLibrary(r *http.Request, req createLibraryRequest) (store.Library, error) {
	req.Path = filepath.Clean(strings.TrimSpace(req.Path))
	req.Name = strings.TrimSpace(req.Name)
	if req.Kind != store.KindMovies && req.Kind != store.KindTV {
		return store.Library{}, errors.New(`kind must be "movies" or "tv"`)
	}
	if !filepath.IsAbs(req.Path) {
		return store.Library{}, errors.New("path must be an absolute folder path")
	}
	if fi, err := os.Stat(req.Path); err != nil || !fi.IsDir() {
		return store.Library{}, errors.New("folder not found or not readable: " + req.Path)
	}
	if req.Name == "" {
		req.Name = filepath.Base(req.Path)
	}
	lib, err := s.Store.CreateLibrary(r.Context(), req.Name, req.Path, req.Kind)
	if err != nil {
		if strings.Contains(err.Error(), "UNIQUE") {
			return lib, errors.New("that folder is already a library")
		}
		return lib, err
	}
	_ = s.Scanner.Start(s.BaseCtx, lib.ID)
	return lib, nil
}

func (s *Server) createLibrary(w http.ResponseWriter, r *http.Request, u store.User) {
	var req createLibraryRequest
	if err := readJSON(r, &req); err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	lib, err := s.addLibrary(r, req)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(w, http.StatusCreated, lib)
}

func pathID(r *http.Request) (int64, bool) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	return id, err == nil
}

func (s *Server) deleteLibrary(w http.ResponseWriter, r *http.Request, u store.User) {
	id, ok := pathID(r)
	if !ok {
		writeErr(w, http.StatusBadRequest, "bad library id")
		return
	}
	if p, _ := s.Scanner.Progress(id); p.Running {
		writeErr(w, http.StatusConflict, "wait for the scan to finish")
		return
	}
	if err := s.Store.DeleteLibrary(r.Context(), id); err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) startScan(w http.ResponseWriter, r *http.Request, rematch bool) {
	id, ok := pathID(r)
	if !ok {
		writeErr(w, http.StatusBadRequest, "bad library id")
		return
	}
	if _, err := s.Store.GetLibrary(r.Context(), id); err != nil {
		writeErr(w, http.StatusNotFound, "library not found")
		return
	}
	if rematch {
		if err := s.Store.ResetUnmatched(r.Context(), id); err != nil {
			writeErr(w, http.StatusInternalServerError, err.Error())
			return
		}
	}
	if err := s.Scanner.Start(s.BaseCtx, id); err != nil {
		if errors.Is(err, scanner.ErrBusy) {
			writeErr(w, http.StatusConflict, err.Error())
			return
		}
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	p, _ := s.Scanner.Progress(id)
	writeJSON(w, http.StatusAccepted, p)
}

func (s *Server) scanLibrary(w http.ResponseWriter, r *http.Request, u store.User) {
	s.startScan(w, r, false)
}

func (s *Server) rematchLibrary(w http.ResponseWriter, r *http.Request, u store.User) {
	s.startScan(w, r, true)
}

// --- Items ---

func (s *Server) listItems(w http.ResponseWriter, r *http.Request, u store.User) {
	f := itemFilterFrom(r, u)
	items, err := s.Store.ListItems(r.Context(), f)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	views, err := s.views(r.Context(), u.ProfileID, items)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	resp := map[string]any{"items": views}
	if len(items) == f.Limit {
		resp["nextOffset"] = f.Offset + f.Limit
	}
	writeJSON(w, http.StatusOK, resp)
}

type nextUpView struct {
	Episode    *ItemView `json:"episode,omitempty"`
	InProgress bool      `json:"inProgress"`
	Reason     string    `json:"reason"`
	Gaps       []gapView `json:"gaps"`
}

type gapView struct {
	ID      string `json:"id"`
	Season  int    `json:"season"`
	Episode int    `json:"episode"`
}

func (s *Server) nextUp(r *http.Request, profileID int64, showID string) (*nextUpView, error) {
	nu, err := s.Watch.ShowNextUp(r.Context(), profileID, showID)
	if err != nil {
		return nil, err
	}
	out := &nextUpView{InProgress: nu.InProgress, Reason: nu.Reason, Gaps: []gapView{}}
	for _, g := range nu.Gaps {
		out.Gaps = append(out.Gaps, gapView{ID: g.ID, Season: g.Season, Episode: g.Episode})
	}
	if nu.Next != nil {
		it, err := s.Store.GetItem(r.Context(), nu.Next.ID)
		if err != nil {
			return nil, err
		}
		views, err := s.views(r.Context(), profileID, []store.Item{it})
		if err != nil {
			return nil, err
		}
		out.Episode = &views[0]
	}
	return out, nil
}

func (s *Server) getItem(w http.ResponseWriter, r *http.Request, u store.User) {
	it, err := s.Store.GetItem(r.Context(), r.PathValue("id"))
	if err != nil {
		writeErr(w, http.StatusNotFound, "item not found")
		return
	}
	views, err := s.views(r.Context(), u.ProfileID, []store.Item{it})
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	resp := map[string]any{"item": views[0]}

	switch it.Kind {
	case store.ItemMovie, store.ItemEpisode:
		versions, err := s.versionViews(r.Context(), it.ID)
		if err != nil {
			writeErr(w, http.StatusInternalServerError, err.Error())
			return
		}
		resp["versions"] = versions
	}
	if it.Kind == store.ItemEpisode {
		if season, err := s.Store.GetItem(r.Context(), it.ParentID); err == nil {
			resp["season"] = viewOf(season, nil)
			if show, err := s.Store.GetItem(r.Context(), season.ParentID); err == nil {
				resp["show"] = viewOf(show, nil)
			}
		}
	}
	if it.Kind != store.ItemSeason {
		top := s.topLevelID(r, it)
		if g, err := s.Store.Genres(r.Context(), []string{top}); err == nil {
			resp["genres"] = append([]string{}, g[top]...)
		}
		resp["cast"] = s.castFor(r, top)
		resp["trackPrefs"] = s.effectivePrefs(r, u.ProfileID, top)
	}
	if it.Kind == store.ItemShow {
		nu, err := s.nextUp(r, u.ProfileID, it.ID)
		if err != nil {
			writeErr(w, http.StatusInternalServerError, err.Error())
			return
		}
		resp["nextUp"] = nu
	}
	writeJSON(w, http.StatusOK, resp)
}

func (s *Server) itemChildren(w http.ResponseWriter, r *http.Request, u store.User) {
	items, err := s.Store.ListItems(r.Context(), store.ItemFilter{ParentID: r.PathValue("id"), Limit: 500})
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	// Children are listed in season and episode order.
	sortChildren(items)
	views, err := s.views(r.Context(), u.ProfileID, items)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"items": views})
}

func sortChildren(items []store.Item) {
	less := func(a, b store.Item) bool {
		if a.SeasonNumber != b.SeasonNumber {
			return a.SeasonNumber < b.SeasonNumber
		}
		return a.EpisodeNumber < b.EpisodeNumber
	}
	for i := 1; i < len(items); i++ {
		for j := i; j > 0 && less(items[j], items[j-1]); j-- {
			items[j], items[j-1] = items[j-1], items[j]
		}
	}
}

type watchedRequest struct {
	Watched bool `json:"watched"`
}

func (s *Server) setWatched(w http.ResponseWriter, r *http.Request, u store.User) {
	var req watchedRequest
	if err := readJSON(r, &req); err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	it, err := s.Store.GetItem(r.Context(), r.PathValue("id"))
	if err != nil {
		writeErr(w, http.StatusNotFound, "item not found")
		return
	}
	if err := s.Watch.SetWatched(r.Context(), u.ProfileID, it, req.Watched); err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

type fixMatchRequest struct {
	TMDBID int `json:"tmdbId"`
}

// fixMatch sets the TMDB ID by hand and re-matches the item.
func (s *Server) fixMatch(w http.ResponseWriter, r *http.Request, u store.User) {
	var req fixMatchRequest
	if err := readJSON(r, &req); err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	it, err := s.applyFix(r, r.PathValue("id"), req.TMDBID)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, viewOf(it, nil))
}

func (s *Server) applyFix(r *http.Request, id string, tmdbID int) (store.Item, error) {
	if s.Matcher == nil {
		return store.Item{}, errors.New("set a TMDB key to match items")
	}
	if tmdbID <= 0 {
		return store.Item{}, errors.New("tmdbId must be a positive number")
	}
	it, err := s.Store.GetItem(r.Context(), id)
	if err != nil {
		return it, errors.New("item not found")
	}
	provider := store.ProviderTMDBMovie
	switch it.Kind {
	case store.ItemShow:
		provider = store.ProviderTMDBTV
	case store.ItemMovie:
	default:
		return it, errors.New("only movies and shows can be matched by hand")
	}
	if err := s.Store.SetExternalID(r.Context(), it.ID, provider, strconv.Itoa(tmdbID)); err != nil {
		return it, err
	}
	ok, err := s.Matcher.MatchItem(r.Context(), it)
	if err != nil {
		return it, err
	}
	if !ok {
		return it, errors.New("no match for that ID")
	}
	if fresh, err := s.Store.GetItem(r.Context(), it.ID); err == nil {
		return fresh, nil
	}
	// The item was merged into another copy of the same title.
	return it, nil
}

// --- Home and progress ---

type continueView struct {
	Item        ItemView `json:"item"`
	ShowID      string   `json:"showId,omitempty"`
	ShowTitle   string   `json:"showTitle,omitempty"`
	PositionSec float64  `json:"positionSec"`
	InProgress  bool     `json:"inProgress"`
	Reason      string   `json:"reason"`
}

func (s *Server) home(w http.ResponseWriter, r *http.Request, u store.User) {
	entries, err := s.Watch.Continue(r.Context(), u.ProfileID, 20)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	cont := make([]continueView, 0, len(entries))
	for _, e := range entries {
		views, err := s.views(r.Context(), u.ProfileID, []store.Item{e.Item})
		if err != nil {
			writeErr(w, http.StatusInternalServerError, err.Error())
			return
		}
		cv := continueView{Item: views[0], ShowID: e.ShowID, PositionSec: e.PositionSec,
			InProgress: e.InProgress, Reason: e.Reason}
		if e.ShowID != "" {
			if show, err := s.Store.GetItem(r.Context(), e.ShowID); err == nil {
				cv.ShowTitle = show.Title
			}
		}
		cont = append(cont, cv)
	}
	recent, err := s.recentlyAdded(r.Context(), 20)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	recentViews, err := s.views(r.Context(), u.ProfileID, recent)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	genres, err := s.genreRows(r.Context(), u.ProfileID, 8, 20)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"continueWatching": cont, "recentlyAdded": recentViews, "genres": genres})
}

type progressRequest struct {
	PositionSec float64 `json:"positionSec"`
	VersionID   string  `json:"versionId,omitempty"`
	ClientTime  int64   `json:"clientTime,omitempty"`
}

func (s *Server) reportProgress(w http.ResponseWriter, r *http.Request, u store.User) {
	var req progressRequest
	if err := readJSON(r, &req); err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	it, err := s.Store.GetItem(r.Context(), r.PathValue("id"))
	if err != nil {
		writeErr(w, http.StatusNotFound, "item not found")
		return
	}
	if it.Kind != store.ItemMovie && it.Kind != store.ItemEpisode {
		writeErr(w, http.StatusBadRequest, "progress is reported on movies and episodes")
		return
	}
	duration := 0.0
	versions, err := s.Store.VersionsForItem(r.Context(), it.ID)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	for _, v := range versions {
		if req.VersionID == "" || v.ID == req.VersionID {
			duration = v.DurationSec
			break
		}
	}
	if duration == 0 && it.RuntimeMin > 0 {
		duration = float64(it.RuntimeMin) * 60
	}
	st, err := s.Watch.Report(r.Context(), u.ProfileID, it.ID, req.PositionSec, duration, req.ClientTime)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, st)
}

// --- Media ---

var contentTypes = map[string]string{
	"mkv": "video/x-matroska", "webm": "video/webm", "mp4": "video/mp4", "m4v": "video/mp4",
	"mov": "video/quicktime", "ts": "video/mp2t", "avi": "video/x-msvideo",
}

// stream serves a file as-is with byte ranges (direct play). Seeking works
// because http.ServeContent handles Range requests.
func (s *Server) stream(w http.ResponseWriter, r *http.Request, u store.User) {
	// Timings for each step, logged once per request, so a stalled start can
	// be traced to the step that stalled (a drive waking up, a locked file).
	start := time.Now()
	step := func(name string) { s.Log.Info("stream step", "step", name, "ms", time.Since(start).Milliseconds()) }
	s.Log.Info("stream request", "version", r.PathValue("id"), "range", r.Header.Get("Range"),
		"client", clientIP(r))

	v, err := s.Store.GetVersion(r.Context(), r.PathValue("id"))
	if err != nil || v.Missing {
		writeErr(w, http.StatusNotFound, "file not found")
		return
	}
	step("found in database")
	if rg := r.Header.Get("Range"); s.Jobs != nil && (rg == "" || strings.HasPrefix(rg, "bytes=0-")) {
		// Someone started watching: make its preview thumbnails next.
		s.Jobs.Prioritize(v.ID)
	}
	f, err := os.Open(v.Path)
	if err != nil {
		s.Log.Warn("stream open failed", "path", v.Path, "err", err)
		writeErr(w, http.StatusNotFound, "file not available")
		return
	}
	defer f.Close()
	// Players give up after about 8 seconds without data. A sleeping hard
	// drive or an antivirus scan of a big file can take that long to open.
	if d := time.Since(start); d > 2*time.Second {
		s.Log.Warn("slow file open: is the drive waking up, or being scanned by antivirus?",
			"path", v.RelPath, "seconds", d.Round(100*time.Millisecond).Seconds())
	}
	step("file opened")
	fi, err := f.Stat()
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	if ct, ok := contentTypes[v.Container]; ok {
		w.Header().Set("Content-Type", ct)
	}
	w.Header().Set("Cache-Control", "private, no-transform")
	var content io.ReadSeeker = f
	if runtime.GOOS == "windows" {
		// Serving an *os.File lets Go use Windows' TransmitFile, which
		// desktop editions of Windows limit to two transfers at a time
		// across the whole PC. Hiding the file type makes Go copy through a
		// buffer instead, which has no such limit.
		content = plainReader{f}
	}
	step("sending")
	http.ServeContent(w, r, filepath.Base(v.Path), fi.ModTime(), content)
	step("finished")
}

// plainReader exposes only Read and Seek, so the HTTP server can't use
// sendfile/TransmitFile for it.
type plainReader struct{ f *os.File }

func (p plainReader) Read(b []byte) (int, error)                { return p.f.Read(b) }
func (p plainReader) Seek(off int64, whence int) (int64, error) { return p.f.Seek(off, whence) }

// imageFallbacks: a season without a poster uses its show's; an episode
// without a still uses its season's poster.
func (s *Server) imageSource(r *http.Request, id, kind string) (string, error) {
	for range 3 {
		url, err := s.Store.ImageURL(r.Context(), id, kind)
		if err != nil || url != "" {
			return url, err
		}
		it, err := s.Store.GetItem(r.Context(), id)
		if err != nil || it.ParentID == "" {
			return "", err
		}
		id = it.ParentID
		if kind == "still" {
			kind = "poster"
		}
	}
	return "", nil
}

func (s *Server) image(w http.ResponseWriter, r *http.Request, u store.User) {
	kind := r.PathValue("kind")
	if kind != "poster" && kind != "backdrop" && kind != "still" {
		writeErr(w, http.StatusBadRequest, "kind must be poster, backdrop or still")
		return
	}
	src, err := s.imageSource(r, r.PathValue("id"), kind)
	if err != nil || src == "" {
		writeErr(w, http.StatusNotFound, "no image")
		return
	}
	width, _ := strconv.Atoi(r.URL.Query().Get("w"))
	path, err := s.Images.Get(r.Context(), src, images.SnapWidth(width))
	if err != nil {
		s.Log.Warn("image", "src", src, "err", err)
		writeErr(w, http.StatusBadGateway, "image unavailable")
		return
	}
	w.Header().Set("Cache-Control", "private, max-age=604800")
	http.ServeFile(w, r, path)
}
