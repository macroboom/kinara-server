// Package api serves the JSON API used by the apps, media streams, artwork,
// and a small built-in web page for checking libraries.
package api

import (
	"context"
	"embed"
	"encoding/json"
	"errors"
	"html/template"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"kinara/internal/auth"
	"kinara/internal/cloud"
	"kinara/internal/images"
	"kinara/internal/jobs"
	"kinara/internal/match"
	"kinara/internal/metrics"
	"kinara/internal/pgs"
	"kinara/internal/quick"
	"kinara/internal/remote"
	"kinara/internal/scanner"
	"kinara/internal/store"
	"kinara/internal/tmdb"
	"kinara/internal/transcode"
	"kinara/internal/trickplay"
	"kinara/internal/watch"
)

//go:embed templates/*.html
var templatesFS embed.FS

//go:embed openapi.yaml
var openapiSpec []byte

const (
	cookieName     = "kinara_session"
	sessionTTL     = 90 * 24 * time.Hour
	sessionRefresh = time.Hour
)

// Server holds everything the HTTP handlers need.
type Server struct {
	Store   *store.Store
	Scanner *scanner.Scanner
	Matcher *match.Matcher // nil without a TMDB key
	Images  *images.Cache
	Watch   *watch.Service
	Log     *slog.Logger
	Version string
	// BaseCtx outlives requests; background scans run under it.
	BaseCtx context.Context
	// Warnings are shown on the web page (missing ffprobe, no TMDB key).
	Warnings []string
	// Transcoder converts files the player can't play (nil: off).
	Transcoder *transcode.Transcoder
	// HLS converts for iPhones and Safari (nil turns it off).
	HLS *transcode.HLS
	// Subtitles converts text subtitle tracks to WebVTT (nil without ffmpeg).
	Subtitles *transcode.Subtitles
	// PGS reads Blu-ray picture subtitles into timed PNGs for apps to draw
	// (nil without ffmpeg).
	PGS *pgs.Extractor

	// playing is what each device last reported playing.
	playing nowPlaying
	// LANPort is the plain HTTP port the server listens on, for addresses a
	// Chromecast on the home network can reach (see cast.go).
	LANPort int
	// libraryRev changes when a scan or a match changes the library (see pulse.go).
	libraryRev atomic.Int64
	// party runs watch parties (see party.go).
	party partyState

	// Metrics samples the computer and traffic (nil: off).
	Metrics *metrics.Collector
	// Logs keeps recent warnings and errors for the metrics page.
	Logs *metrics.LogRing
	// DataDir is the server's data folder, for storage numbers.
	DataDir string
	// CacheDir is the cache folder (DATA/cache unless set apart), for storage numbers.
	CacheDir string

	httpStats metrics.HTTPStats
	storage   storageCache
	// WebDir holds the built web app, served at /app/ ("" when not installed).
	WebDir string
	// Trickplay serves preview thumbnails (nil: off).
	Trickplay *trickplay.Maker
	// Jobs makes previews and finds intros in the background (may be nil).
	Jobs *jobs.Worker
	// TMDB is used for the Trending page (nil without a key).
	TMDB *tmdb.Client
	// FFprobe is the ffprobe executable ("" for the one on PATH).
	FFprobe string

	// Remote runs remote access through kinara-iroh (nil: not available).
	Remote *remote.Sidecar
	// Cloud links the server to a Kinara account for remote play (nil: off).
	Cloud *cloud.Manager

	limiter *auth.Limiter
	// tunnelLimiter counts failed sign-ins from all remote devices
	// together: a remote device's "address" is its node ID, which costs
	// nothing to change, so a per-address limit alone could be dodged.
	tunnelLimiter *auth.Limiter
	// quick holds Quick Connect codes (quickconnect.go); quickStarts limits
	// how many each address asks for, quickWrong wrong codes entered.
	quick       *quick.Codes
	quickStarts *auth.Limiter
	quickWrong  *auth.Limiter
	// nodeSeen remembers which node ID each session last used through the
	// tunnel, so the database is written only when it changes.
	nodeSeen sync.Map
	tmpl     *template.Template
	trending trendingCache
	// external keeps TMDB titles opened from outside the library.
	external externalCache
	// integ runs outgoing webhooks, incoming hooks and API keys (integrations*.go).
	integ integrationsState
	// reqs holds requests and the discover cache (requests*.go, discover*.go).
	reqs requestsState
}

// Handler builds the routes.
func (s *Server) Handler() http.Handler {
	s.limiter = &auth.Limiter{Max: 10, Window: 10 * time.Minute}
	s.tunnelLimiter = &auth.Limiter{Max: 30, Window: 10 * time.Minute}
	s.quick = quick.New()
	s.quickStarts = &auth.Limiter{Max: 30, Window: 10 * time.Minute}
	s.quickWrong = &auth.Limiter{Max: 10, Window: 10 * time.Minute}
	s.tmpl = template.Must(template.New("").Funcs(templateFuncs).ParseFS(templatesFS, "templates/*.html"))

	mux := http.NewServeMux()

	// Public.
	mux.HandleFunc("GET /api/v1/health", s.health)
	mux.HandleFunc("POST /api/v1/auth/login", s.login)
	mux.HandleFunc("POST /api/v1/auth/cloud", s.cloudSignIn)
	mux.HandleFunc("GET /api/v1/auth/accounts", s.accounts)
	mux.HandleFunc("POST /api/v1/auth/signup", s.signup)
	mux.HandleFunc("POST /api/v1/auth/quick/start", s.quickStart)
	mux.HandleFunc("POST /api/v1/auth/quick/poll", s.quickPollHandler)
	mux.HandleFunc("POST /api/v1/auth/quick/cancel", s.quickCancel)
	mux.HandleFunc("POST /api/v1/setup", s.setup)
	mux.HandleFunc("GET /openapi.yaml", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/yaml")
		w.Write(openapiSpec)
	})

	// Signed in.
	mux.Handle("POST /api/v1/auth/logout", s.user(s.logout))
	mux.Handle("GET /api/v1/auth/quick/{code}", s.user(s.quickInfoHandler))
	mux.Handle("POST /api/v1/auth/quick/approve", s.user(s.quickApprove))
	mux.Handle("GET /api/v1/me", s.user(s.me))
	mux.Handle("GET /api/v1/libraries", s.user(s.listLibraries))
	mux.Handle("GET /api/v1/items", s.user(s.listItems))
	mux.Handle("GET /api/v1/items/letters", s.user(s.itemLetters))
	mux.Handle("GET /api/v1/items/{id}", s.user(s.getItem))
	mux.Handle("PUT /api/v1/items/{id}/prefs", s.user(s.putPrefs))
	mux.Handle("GET /api/v1/search", s.user(s.search))
	mux.Handle("GET /api/v1/profiles", s.user(s.listProfiles))
	mux.Handle("POST /api/v1/profiles", s.user(s.createProfile))
	mux.Handle("PATCH /api/v1/profiles/{id}", s.user(s.updateProfile))
	mux.Handle("DELETE /api/v1/profiles/{id}", s.user(s.deleteProfile))
	mux.Handle("POST /api/v1/profiles/{id}/select", s.user(s.selectProfile))
	mux.Handle("GET /api/v1/items/{id}/children", s.user(s.itemChildren))
	mux.Handle("POST /api/v1/items/{id}/watched", s.user(s.setWatched))
	mux.Handle("GET /api/v1/home", s.user(s.home))
	mux.Handle("PUT /api/v1/progress/{id}", s.user(s.reportProgress))
	// Media URLs also accept ?token= so players like VLC can open them.
	mux.Handle("GET /api/v1/stream/{id}", s.userQuery(s.stream))
	mux.Handle("GET /api/v1/images/{id}/{kind}", s.userQuery(s.image))
	mux.Handle("GET /api/v1/images/{id}/{kind}/color", s.user(s.imageColor))
	mux.Handle("GET /api/v1/cast/{id}/{ord}", s.userQuery(s.castPhoto))
	mux.Handle("GET /api/v1/transcode/{id}", s.userQuery(s.transcodeStream))
	mux.Handle("GET /api/v1/transcode/{id}/start", s.user(s.transcodeStart))
	mux.Handle("POST /api/v1/hls/{id}", s.user(s.hlsStart))
	mux.Handle("GET /api/v1/subtitles/{id}/{index}", s.userQuery(s.subtitleVTT))
	mux.Handle("GET /api/v1/subtitles/{id}/{index}/pgs", s.userQuery(s.subtitlePGS))
	mux.Handle("GET /api/v1/subtitles/{id}/{index}/pgs/{n}", s.userQuery(s.subtitlePGSImage))
	mux.HandleFunc("GET /api/v1/hls/session/{session}/{file}", s.hlsFile)
	mux.Handle("DELETE /api/v1/hls/session/{session}", s.user(s.hlsStop))
	mux.Handle("GET /api/v1/speedtest", s.userQuery(s.speedTest))
	mux.Handle("GET /api/v1/trickplay/{id}", s.userQuery(s.trickplayManifest))
	mux.Handle("GET /api/v1/trickplay/{id}/{sheet}", s.userQuery(s.trickplaySheet))
	mux.Handle("GET /api/v1/tmdb-image/{file}", s.userQuery(s.tmdbImage))
	mux.Handle("GET /api/v1/genres", s.user(s.listGenres))
	mux.Handle("GET /api/v1/trending", s.user(s.trendingHandler))
	mux.Handle("GET /api/v1/tmdb/{kind}/{id}", s.user(s.externalTitle))
	mux.Handle("GET /api/v1/tmdb/show/{id}/season/{n}", s.user(s.externalSeason))
	mux.Handle("GET /api/v1/tmdb/{kind}/{id}/similar", s.user(s.externalSimilar))
	mux.Handle("GET /api/v1/items/{id}/similar", s.user(s.itemSimilar))
	mux.Handle("GET /api/v1/lists", s.user(s.getLists))
	mux.Handle("POST /api/v1/lists", s.user(s.createList))
	mux.Handle("PATCH /api/v1/lists/{id}", s.user(s.renameList))
	mux.Handle("DELETE /api/v1/lists/{id}", s.user(s.deleteList))
	mux.Handle("GET /api/v1/lists/{id}/items", s.user(s.listItemsHandler))
	mux.Handle("PUT /api/v1/lists/{id}/items/{item}", s.user(s.addToList))
	mux.Handle("DELETE /api/v1/lists/{id}/items/{item}", s.user(s.removeFromList))

	// Admin.
	mux.Handle("POST /api/v1/libraries", s.admin(s.createLibrary))
	mux.Handle("DELETE /api/v1/libraries/{id}", s.admin(s.deleteLibrary))
	mux.Handle("PATCH /api/v1/libraries/{id}", s.admin(s.updateLibrary))
	mux.Handle("POST /api/v1/libraries/{id}/folders", s.admin(s.addLibraryFolder))
	mux.Handle("DELETE /api/v1/libraries/{id}/folders", s.admin(s.removeLibraryFolder))
	mux.Handle("GET /api/v1/fs/folders", s.admin(s.listFolders))
	mux.Handle("POST /api/v1/libraries/{id}/scan", s.admin(s.scanLibrary))
	mux.Handle("POST /api/v1/libraries/{id}/rematch", s.admin(s.rematchLibrary))
	mux.Handle("POST /api/v1/items/{id}/match", s.admin(s.fixMatch))
	mux.Handle("GET /api/v1/users", s.admin(s.listUsers))
	mux.Handle("POST /api/v1/users", s.admin(s.createUserHandler))
	mux.Handle("PATCH /api/v1/users/{id}", s.admin(s.updateUser))
	mux.Handle("DELETE /api/v1/users/{id}", s.admin(s.deleteUser))
	mux.Handle("GET /api/v1/libraries/{id}/problems", s.admin(s.libraryProblems))
	mux.Handle("GET /api/v1/status", s.admin(s.status))
	mux.Handle("GET /api/v1/cloud", s.admin(s.cloudStatus))
	mux.Handle("POST /api/v1/cloud/link", s.admin(s.cloudLink))
	mux.Handle("DELETE /api/v1/cloud/link", s.admin(s.cloudCancelLink))
	mux.Handle("POST /api/v1/cloud/unlink", s.admin(s.cloudUnlink))
	mux.Handle("GET /api/v1/playback/advice", s.user(s.playbackAdvice))
	mux.Handle("GET /api/v1/playback", s.admin(s.nowPlayingList))
	mux.HandleFunc("GET /api/v1/time", s.serverTime)
	mux.Handle("GET /api/v1/parties/mine", s.user(s.partyMine))
	mux.Handle("GET /api/v1/parties/devices", s.user(s.partyDevices))
	mux.Handle("POST /api/v1/parties", s.user(s.partyCreate))
	mux.Handle("POST /api/v1/parties/join", s.user(s.partyJoin))
	mux.Handle("POST /api/v1/parties/{id}/leave", s.user(s.partyLeave))
	mux.Handle("POST /api/v1/parties/{id}/decline", s.user(s.partyDecline))
	mux.Handle("POST /api/v1/parties/{id}/invite", s.user(s.partyInvite))
	mux.Handle("POST /api/v1/parties/{id}/control", s.user(s.partyControl))
	mux.Handle("POST /api/v1/parties/{id}/report", s.user(s.partyReport))
	mux.Handle("GET /api/v1/parties/{id}/wait", s.user(s.partyWait))
	mux.Handle("PUT /api/v1/me/device/name", s.user(s.renameMyDevice))
	mux.Handle("GET /api/v1/devices", s.admin(s.devices))
	mux.Handle("DELETE /api/v1/devices/{id}", s.admin(s.signOutDevice))
	mux.Handle("GET /api/v1/metrics/live", s.admin(s.metricsLive))
	mux.Handle("GET /api/v1/metrics/storage", s.admin(s.metricsStorage))
	mux.Handle("GET /api/v1/metrics/usage", s.admin(s.metricsUsage))
	mux.Handle("GET /api/v1/metrics/http", s.admin(s.metricsHTTP))
	mux.Handle("GET /api/v1/metrics/logs", s.admin(s.metricsLogs))
	mux.Handle("GET /api/v1/remote", s.admin(s.remoteInfo))
	mux.Handle("PUT /api/v1/remote/settings", s.admin(s.putRemoteSettings))
	mux.Handle("POST /api/v1/remote/block", s.admin(s.remoteBlock))
	mux.Handle("POST /api/v1/remote/unblock", s.admin(s.remoteUnblock))
	mux.Handle("GET /api/v1/settings", s.admin(s.getSettings))
	mux.Handle("PUT /api/v1/settings", s.admin(s.putSettings))

	// Integrations (webhooks, incoming hooks, API keys) and requests.
	s.integrationRoutes(mux)
	s.requestRoutes(mux)

	// The web player and manager.
	mux.Handle("GET /app/", s.webApp())
	mux.Handle("GET /app", http.RedirectHandler("/app/", http.StatusFound))

	// Built-in web page.
	s.uiRoutes(mux)

	return s.logRequests(mux)
}

type ctxKey struct{}

type authed struct {
	user      store.User
	tokenHash string
}

type userHandler func(w http.ResponseWriter, r *http.Request, u store.User)

func (s *Server) user(h userHandler) http.Handler  { return s.authenticate(h, false, false) }
func (s *Server) admin(h userHandler) http.Handler { return s.authenticate(h, true, false) }
func (s *Server) userQuery(h userHandler) http.Handler {
	return s.authenticate(h, false, true)
}

func tokenFrom(r *http.Request, allowQuery bool) string {
	if h := r.Header.Get("Authorization"); strings.HasPrefix(h, "Bearer ") {
		return strings.TrimSpace(strings.TrimPrefix(h, "Bearer "))
	}
	// A token in the URL (images and video, which can't send headers) is
	// what the page asked with, so it beats the cookie. The cookie belongs to
	// the built-in pages and can outlive its session: a reinstall with a
	// fresh database left a dead one that turned every poster into a 401.
	if allowQuery {
		if t := r.URL.Query().Get("token"); t != "" {
			return t
		}
	}
	if c, err := r.Cookie(cookieName); err == nil && c.Value != "" {
		return c.Value
	}
	return ""
}

// lookup resolves a request's token to a user, sliding the session's expiry.
func (s *Server) lookup(r *http.Request, allowQuery bool) (authed, bool) {
	tok := tokenFrom(r, allowQuery)
	if tok == "" {
		return authed{}, false
	}
	hash := auth.HashToken(tok)
	u, expires, lastSeen, err := s.Store.SessionUser(r.Context(), hash)
	nowT := time.Now()
	if err != nil || expires < nowT.Unix() {
		return authed{}, false
	}
	if nowT.Unix()-lastSeen > int64(sessionRefresh.Seconds()) {
		_ = s.Store.TouchSession(r.Context(), hash, nowT.Add(sessionTTL).Unix())
	}
	s.noteNode(r, hash)
	return authed{user: u, tokenHash: hash}, true
}

func (s *Server) authenticate(h userHandler, adminOnly, allowQuery bool) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// API keys reach only the endpoints listed in integrations_keys.go.
		if s.serveAPIKey(w, r, h) {
			return
		}
		a, ok := s.lookup(r, allowQuery)
		if !ok {
			writeErr(w, http.StatusUnauthorized, "sign in required")
			return
		}
		if adminOnly && !a.user.IsAdmin {
			writeErr(w, http.StatusForbidden, "admin only")
			return
		}
		r = r.WithContext(context.WithValue(r.Context(), ctxKey{}, a))
		h(w, r, a.user)
	})
}

func sessionFrom(r *http.Request) authed {
	a, _ := r.Context().Value(ctxKey{}).(authed)
	return a
}

type statusWriter struct {
	http.ResponseWriter
	status int
	bytes  int64
	// count, if set, gets bytes as they're sent (for live traffic rates).
	count func(n int64)
}

func (w *statusWriter) Write(p []byte) (int, error) {
	if w.status == 0 {
		w.status = http.StatusOK
	}
	n, err := w.ResponseWriter.Write(p)
	w.sent(int64(n))
	return n, err
}

func (w *statusWriter) sent(n int64) {
	w.bytes += n
	if w.count != nil {
		w.count(n)
	}
}

func (w *statusWriter) WriteHeader(code int) {
	w.status = code
	w.ResponseWriter.WriteHeader(code)
}

// Flush passes through, so converted streams reach the player as they're made.
func (w *statusWriter) Flush() {
	if f, ok := w.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

// Unwrap lets http.ResponseController reach the real writer.
func (w *statusWriter) Unwrap() http.ResponseWriter { return w.ResponseWriter }

// ReadFrom keeps sendfile working for streams.
func (w *statusWriter) ReadFrom(r io.Reader) (int64, error) {
	if w.status == 0 {
		w.status = http.StatusOK
	}
	n, err := io.Copy(w.ResponseWriter, r)
	w.sent(n)
	return n, err
}

func (s *Server) logRequests(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		sw := &statusWriter{ResponseWriter: w}
		if s.Metrics != nil {
			sw.count = s.Metrics.AddOut
			s.Metrics.AddIn(max(r.ContentLength, 0))
		}
		// Logged in a defer so a request cut off on purpose (a conversion
		// that failed partway) still shows up.
		defer func() {
			s.logRequest(r, sw, start)
		}()
		next.ServeHTTP(sw, r)
	})
}

func (s *Server) logRequest(r *http.Request, sw *statusWriter, start time.Time) {
	if sw.status == 0 {
		sw.status = http.StatusOK
	}
	// The mux names the route it picked on the request.
	s.httpStats.Record(r.Pattern, sw.status, time.Since(start), sw.bytes)
	level := slog.LevelDebug
	if sw.status >= 500 {
		level = slog.LevelError
	} else if strings.HasPrefix(r.URL.Path, "/api/") && !strings.HasPrefix(r.URL.Path, "/api/v1/images/") &&
		!strings.HasPrefix(r.URL.Path, "/api/v1/hls/session/") && !(sw.status < 400 && routine(r)) {
		level = slog.LevelInfo
	}
	s.Log.Log(r.Context(), level, "request", "method", r.Method, "path", r.URL.Path,
		"status", sw.status, "ms", time.Since(start).Milliseconds())
}

// routine is a request apps and pages repeat every few seconds on their
// own (checking for changes, playback position and advice, live charts).
// It's only logged with -debug, unless it fails.
func routine(r *http.Request) bool {
	p := r.URL.Path
	switch {
	case r.Method == http.MethodGet && (p == "/api/v1/pulse" || p == "/api/v1/health" || p == "/api/v1/ontheway" ||
		p == "/api/v1/playback" || p == "/api/v1/playback/advice" || p == "/api/v1/requests"):
		return true
	case r.Method == http.MethodGet && (strings.HasPrefix(p, "/api/v1/metrics") || strings.HasPrefix(p, "/api/v1/integrations/") ||
		strings.HasPrefix(p, "/api/v1/trickplay/")):
		return true
	case r.Method == http.MethodPut && strings.HasPrefix(p, "/api/v1/progress/"):
		return true
	}
	return false
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	enc := json.NewEncoder(w)
	enc.SetEscapeHTML(false)
	enc.Encode(v)
}

func writeErr(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, map[string]string{"error": msg})
}

func readJSON(r *http.Request, v any) error {
	dec := json.NewDecoder(io.LimitReader(r.Body, 1<<20))
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		return errors.New("invalid JSON body: " + err.Error())
	}
	return nil
}
