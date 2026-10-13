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
	"net"
	"net/http"
	"strings"
	"time"

	"kinora/internal/auth"
	"kinora/internal/images"
	"kinora/internal/jobs"
	"kinora/internal/match"
	"kinora/internal/scanner"
	"kinora/internal/store"
	"kinora/internal/tmdb"
	"kinora/internal/transcode"
	"kinora/internal/trickplay"
	"kinora/internal/watch"
)

//go:embed templates/*.html
var templatesFS embed.FS

//go:embed openapi.yaml
var openapiSpec []byte

const (
	cookieName     = "kinora_session"
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
	// Trickplay serves preview thumbnails (nil: off).
	Trickplay *trickplay.Maker
	// Jobs makes previews and finds intros in the background (may be nil).
	Jobs *jobs.Worker
	// TMDB is used for the Trending page (nil without a key).
	TMDB *tmdb.Client
	// FFprobe is the ffprobe executable ("" for the one on PATH).
	FFprobe string

	limiter  *auth.Limiter
	tmpl     *template.Template
	trending trendingCache
}

// Handler builds the routes.
func (s *Server) Handler() http.Handler {
	s.limiter = &auth.Limiter{Max: 10, Window: 10 * time.Minute}
	s.tmpl = template.Must(template.New("").Funcs(templateFuncs).ParseFS(templatesFS, "templates/*.html"))

	mux := http.NewServeMux()

	// Public.
	mux.HandleFunc("GET /api/v1/health", s.health)
	mux.HandleFunc("POST /api/v1/auth/login", s.login)
	mux.HandleFunc("GET /openapi.yaml", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/yaml")
		w.Write(openapiSpec)
	})

	// Signed in.
	mux.Handle("POST /api/v1/auth/logout", s.user(s.logout))
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
	mux.Handle("GET /api/v1/cast/{id}/{ord}", s.userQuery(s.castPhoto))
	mux.Handle("GET /api/v1/transcode/{id}", s.userQuery(s.transcodeStream))
	mux.Handle("GET /api/v1/transcode/{id}/start", s.user(s.transcodeStart))
	mux.Handle("GET /api/v1/speedtest", s.userQuery(s.speedTest))
	mux.Handle("GET /api/v1/trickplay/{id}", s.userQuery(s.trickplayManifest))
	mux.Handle("GET /api/v1/trickplay/{id}/{sheet}", s.userQuery(s.trickplaySheet))
	mux.Handle("GET /api/v1/tmdb-image/{file}", s.userQuery(s.tmdbImage))
	mux.Handle("GET /api/v1/genres", s.user(s.listGenres))
	mux.Handle("GET /api/v1/trending", s.user(s.trendingHandler))

	// Admin.
	mux.Handle("POST /api/v1/libraries", s.admin(s.createLibrary))
	mux.Handle("DELETE /api/v1/libraries/{id}", s.admin(s.deleteLibrary))
	mux.Handle("POST /api/v1/libraries/{id}/scan", s.admin(s.scanLibrary))
	mux.Handle("POST /api/v1/libraries/{id}/rematch", s.admin(s.rematchLibrary))
	mux.Handle("POST /api/v1/items/{id}/match", s.admin(s.fixMatch))

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
	if c, err := r.Cookie(cookieName); err == nil && c.Value != "" {
		return c.Value
	}
	if allowQuery {
		return r.URL.Query().Get("token")
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
	return authed{user: u, tokenHash: hash}, true
}

func (s *Server) authenticate(h userHandler, adminOnly, allowQuery bool) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
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
	return io.Copy(w.ResponseWriter, r)
}

func (s *Server) logRequests(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		sw := &statusWriter{ResponseWriter: w}
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
	level := slog.LevelDebug
	if sw.status >= 500 {
		level = slog.LevelError
	} else if strings.HasPrefix(r.URL.Path, "/api/") && !strings.HasPrefix(r.URL.Path, "/api/v1/images/") {
		level = slog.LevelInfo
	}
	s.Log.Log(r.Context(), level, "request", "method", r.Method, "path", r.URL.Path,
		"status", sw.status, "ms", time.Since(start).Milliseconds())
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

func clientIP(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}
