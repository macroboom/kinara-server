package api

import (
	"fmt"
	"html/template"
	"net/http"
	"net/url"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"

	"kinara/internal/auth"
	"kinara/internal/parse"
	"kinara/internal/store"
	"kinara/internal/watch"
)

// The built-in web page is for checking that libraries load and match
// correctly. It is plain server-rendered HTML; the apps use the JSON API.

var templateFuncs = template.FuncMap{
	"duration": func(sec float64) string {
		if sec <= 0 {
			return "-"
		}
		d := time.Duration(sec) * time.Second
		h, m := int(d.Hours()), int(d.Minutes())%60
		if h > 0 {
			return fmt.Sprintf("%dh %02dm", h, m)
		}
		return fmt.Sprintf("%dm", m)
	},
	"size": func(b int64) string {
		const gb, mb = 1 << 30, 1 << 20
		if b >= gb {
			return fmt.Sprintf("%.1f GB", float64(b)/gb)
		}
		return fmt.Sprintf("%.0f MB", float64(b)/mb)
	},
	"date": func(unix int64) string {
		if unix <= 0 {
			return "never"
		}
		return time.Unix(unix, 0).Format("Jan 2, 15:04")
	},
	"resolution": func(w, h int) string {
		switch {
		case w >= 3200 || h >= 2000:
			return "4K"
		case w >= 1800 || h >= 1000:
			return "1080p"
		case w >= 1200 || h >= 700:
			return "720p"
		case h > 0:
			return fmt.Sprintf("%dp", h)
		}
		return "-"
	},
	"percent": func(f float64) string { return fmt.Sprintf("%.0f%%", f*100) },
	"divide":  func(bps int64) float64 { return float64(bps) / 1e6 },
	"add":     func(a, b int) int { return a + b },
	"sub":     func(a, b int) int { return a - b },
	"code": func(season, episode int) string {
		return watch.Code(store.EpisodeRef{Season: season, Episode: episode})
	},
}

type page struct {
	Title    string
	User     store.User
	Message  string
	Error    string
	Refresh  bool
	Warnings []string
	Data     any
}

func (s *Server) render(w http.ResponseWriter, name string, p page) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("X-Frame-Options", "DENY")
	if err := s.tmpl.ExecuteTemplate(w, name, p); err != nil {
		s.Log.Error("template", "name", name, "err", err)
	}
}

func redirect(w http.ResponseWriter, r *http.Request, to, msg, errMsg string) {
	q := url.Values{}
	if msg != "" {
		q.Set("msg", msg)
	}
	if errMsg != "" {
		q.Set("err", errMsg)
	}
	if len(q) > 0 {
		to += "?" + q.Encode()
	}
	http.Redirect(w, r, to, http.StatusSeeOther)
}

func setSessionCookie(w http.ResponseWriter, token string) {
	http.SetCookie(w, &http.Cookie{
		Name: cookieName, Value: token, Path: "/", HttpOnly: true,
		SameSite: http.SameSiteLaxMode, MaxAge: int(sessionTTL.Seconds()),
	})
}

func (s *Server) uiRoutes(mux *http.ServeMux) {
	mux.HandleFunc("GET /{$}", s.uiHome)
	mux.HandleFunc("POST /setup", s.uiSetup)
	mux.HandleFunc("GET /login", s.uiLoginPage)
	mux.HandleFunc("POST /login", s.uiLogin)
	mux.HandleFunc("POST /logout", s.uiLogout)
	mux.HandleFunc("POST /ui/libraries", s.uiPage(s.uiAddLibrary, true))
	mux.HandleFunc("POST /ui/libraries/{id}/scan", s.uiPage(s.uiScan, true))
	mux.HandleFunc("POST /ui/libraries/{id}/rematch", s.uiPage(s.uiScan, true))
	mux.HandleFunc("POST /ui/libraries/{id}/delete", s.uiPage(s.uiDeleteLibrary, true))
	mux.HandleFunc("GET /ui/libraries/{id}", s.uiPage(s.uiLibrary, false))
	mux.HandleFunc("GET /ui/items/{id}", s.uiPage(s.uiItem, false))
	mux.HandleFunc("POST /ui/items/{id}/match", s.uiPage(s.uiFixMatch, true))
	mux.HandleFunc("POST /ui/items/{id}/watched", s.uiPage(s.uiWatched, false))
}

// uiPage wraps a handler that needs a signed-in user (via cookie).
func (s *Server) uiPage(h userHandler, adminOnly bool) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		a, ok := s.lookup(r, false)
		if !ok {
			http.Redirect(w, r, "/login", http.StatusSeeOther)
			return
		}
		if adminOnly && !a.user.IsAdmin {
			http.Error(w, "admin only", http.StatusForbidden)
			return
		}
		h(w, r, a.user)
	}
}

func (s *Server) basePage(r *http.Request, title string, u store.User) page {
	return page{Title: title, User: u, Message: r.URL.Query().Get("msg"),
		Error: r.URL.Query().Get("err"), Warnings: s.Warnings}
}

func (s *Server) uiHome(w http.ResponseWriter, r *http.Request) {
	// With the web app installed it is the front door, first-run setup
	// included; these built-in pages are the fallback for servers without it.
	if s.WebDir != "" {
		http.Redirect(w, r, "/app/", http.StatusFound)
		return
	}
	n, err := s.Store.CountUsers(r.Context())
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	if n == 0 {
		if fromInternet(r) {
			http.Error(w, "This server isn't set up yet. Set it up on its home network first.", http.StatusForbidden)
			return
		}
		s.render(w, "setup.html", page{Title: "Set up Kinara", Error: r.URL.Query().Get("err")})
		return
	}
	a, ok := s.lookup(r, false)
	if !ok {
		http.Redirect(w, r, "/login", http.StatusSeeOther)
		return
	}
	libs, err := s.libraryViews(r)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	recent, err := s.recentlyAdded(r.Context(), 12)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	cont, err := s.Watch.Continue(r.Context(), a.user.ID, 12)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	p := s.basePage(r, "Kinara", a.user)
	for _, l := range libs {
		if l.Scan != nil && l.Scan.Running {
			p.Refresh = true
		}
	}
	p.Data = map[string]any{"Libraries": libs, "Recent": recent, "Continue": cont}
	s.render(w, "dashboard.html", p)
}

// uiSetup creates the first admin account. It only works while there are
// no users at all.
func (s *Server) uiSetup(w http.ResponseWriter, r *http.Request) {
	n, err := s.Store.CountUsers(r.Context())
	if err != nil || n > 0 {
		http.Redirect(w, r, "/", http.StatusSeeOther)
		return
	}
	// Never from outside: whoever makes the first account owns the server.
	if fromInternet(r) {
		http.Error(w, "Set the server up on its home network.", http.StatusForbidden)
		return
	}
	user := strings.TrimSpace(r.FormValue("username"))
	pass := r.FormValue("password")
	if user == "" {
		redirect(w, r, "/", "", "Choose a username.")
		return
	}
	if pass != r.FormValue("confirm") {
		redirect(w, r, "/", "", "The passwords don't match.")
		return
	}
	hash, err := auth.HashPassword(pass)
	if err != nil {
		redirect(w, r, "/", "", err.Error())
		return
	}
	if _, err := s.Store.CreateUser(r.Context(), user, hash, true); err != nil {
		redirect(w, r, "/", "", err.Error())
		return
	}
	token, _, err := s.signIn(r, user, pass, "Web browser")
	if err != nil {
		redirect(w, r, "/login", "", err.Error())
		return
	}
	setSessionCookie(w, token)
	redirect(w, r, "/", "Admin account created. Add your first library below.", "")
}

func (s *Server) uiLoginPage(w http.ResponseWriter, r *http.Request) {
	s.render(w, "login.html", page{Title: "Sign in", Error: r.URL.Query().Get("err")})
}

func (s *Server) uiLogin(w http.ResponseWriter, r *http.Request) {
	token, _, err := s.signIn(r, strings.TrimSpace(r.FormValue("username")), r.FormValue("password"), "Web browser")
	if err != nil {
		redirect(w, r, "/login", "", err.Error())
		return
	}
	setSessionCookie(w, token)
	http.Redirect(w, r, "/", http.StatusSeeOther)
}

func (s *Server) uiLogout(w http.ResponseWriter, r *http.Request) {
	if a, ok := s.lookup(r, false); ok {
		_ = s.Store.DeleteSession(r.Context(), a.tokenHash)
	}
	http.SetCookie(w, &http.Cookie{Name: cookieName, Value: "", Path: "/", MaxAge: -1})
	http.Redirect(w, r, "/login", http.StatusSeeOther)
}

func (s *Server) uiAddLibrary(w http.ResponseWriter, r *http.Request, u store.User) {
	lib, err := s.addLibrary(r, createLibraryRequest{
		Name: r.FormValue("name"), Path: r.FormValue("path"), Kind: r.FormValue("kind"),
	})
	if err != nil {
		redirect(w, r, "/", "", err.Error())
		return
	}
	redirect(w, r, "/", fmt.Sprintf("Added %s. The first scan has started.", lib.Name), "")
}

func (s *Server) uiScan(w http.ResponseWriter, r *http.Request, u store.User) {
	id, ok := pathID(r)
	if !ok {
		redirect(w, r, "/", "", "Bad library id.")
		return
	}
	if strings.HasSuffix(r.URL.Path, "/rematch") {
		if err := s.Store.ResetUnmatched(r.Context(), id); err != nil {
			redirect(w, r, "/", "", err.Error())
			return
		}
	}
	if err := s.Scanner.Start(s.BaseCtx, id); err != nil {
		redirect(w, r, "/", "", err.Error())
		return
	}
	redirect(w, r, "/", "Scan started.", "")
}

func (s *Server) uiDeleteLibrary(w http.ResponseWriter, r *http.Request, u store.User) {
	id, ok := pathID(r)
	if !ok {
		redirect(w, r, "/", "", "Bad library id.")
		return
	}
	if p, _ := s.Scanner.Progress(id); p.Running {
		redirect(w, r, "/", "", "Wait for the scan to finish.")
		return
	}
	if err := s.Store.DeleteLibrary(r.Context(), id); err != nil {
		redirect(w, r, "/", "", err.Error())
		return
	}
	redirect(w, r, "/", "Library removed. Your files were not touched.", "")
}

const uiPageSize = 60

func (s *Server) uiLibrary(w http.ResponseWriter, r *http.Request, u store.User) {
	id, ok := pathID(r)
	if !ok {
		http.NotFound(w, r)
		return
	}
	lib, err := s.Store.GetLibrary(r.Context(), id)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	stats, err := s.Store.LibraryStats(r.Context(), id)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	tab := r.URL.Query().Get("tab")
	pageNum, _ := strconv.Atoi(r.URL.Query().Get("page"))
	pageNum = max(pageNum, 0)
	data := map[string]any{"Library": lib, "Stats": stats, "Tab": tab, "Page": pageNum}

	switch tab {
	case "review":
		items, err := s.Store.UnmatchedItems(r.Context(), id)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		data["Unmatched"] = items
	case "files":
		unplaced, err := s.Store.UnlinkedVersions(r.Context(), id, 500)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		probeErrs, err := s.Store.ProbeErrors(r.Context(), id, 500)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		rows := make([]unplacedRow, len(unplaced))
		for i, v := range unplaced {
			rows[i] = unplacedRow{Version: v, Reason: unplacedReason(lib.Kind, v.RelPath)}
		}
		data["Unplaced"], data["ProbeErrors"] = rows, probeErrs
	default:
		kind := store.ItemMovie
		if lib.Kind == store.KindTV {
			kind = store.ItemShow
		}
		sort := r.URL.Query().Get("sort")
		items, err := s.Store.ListItems(r.Context(), store.ItemFilter{
			LibraryID: id, Kind: kind, Sort: sort, Limit: uiPageSize + 1, Offset: pageNum * uiPageSize,
			// The owner's check page lists everything, matched or not.
			IncludeUnmatched: true,
		})
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		data["HasNext"] = len(items) > uiPageSize
		if len(items) > uiPageSize {
			items = items[:uiPageSize]
		}
		data["Items"], data["Sort"] = items, sort
	}
	p := s.basePage(r, lib.Name, u)
	if prog, ok := s.Scanner.Progress(id); ok {
		data["Scan"] = prog
		p.Refresh = prog.Running
	}
	p.Data = data
	s.render(w, "library.html", p)
}

type unplacedRow struct {
	store.Version
	Reason string
}

var reSeasonOnly = regexp.MustCompile(`(?i)(?:^|[ ._\-])s\d{1,2}(?:[ ._\-]|$)`)

// reSpecialHint spots specials and bonus clips: "Part 1 of 3", ".Extra.", "OVA".
var reSpecialHint = regexp.MustCompile(`(?i)\bpart \d+ of \d+\b|[ ._-]extras?[ ._-]|\bspecial\b|\bova\b`)

// unplacedReason explains in plain words why a file wasn't placed.
func unplacedReason(libKind, rel string) string {
	base := filepath.Base(strings.ReplaceAll(rel, `\`, "/"))
	p := parse.Parse(strings.ReplaceAll(rel, `\`, "/"))
	switch {
	case libKind != store.KindTV:
		return "No title could be read from the name."
	case reSeasonOnly.MatchString(base):
		return "Has a season number but no episode number. It may be a whole season in one file, or an unfinished download."
	case reSpecialHint.MatchString(base):
		return "Looks like a special or bonus episode. Put it in a Specials folder and name it with S00E01, S00E02 and so on."
	case p.Year > 0:
		return "Looks like a movie or special. Move it to a Movies library."
	default:
		return "No season and episode number in the name or folders."
	}
}

type childRow struct {
	Item  store.Item
	Watch store.WatchState
}

func (s *Server) uiItem(w http.ResponseWriter, r *http.Request, u store.User) {
	it, err := s.Store.GetItem(r.Context(), r.PathValue("id"))
	if err != nil {
		http.NotFound(w, r)
		return
	}
	scheme := "http"
	if r.TLS != nil {
		scheme = "https"
	}
	data := map[string]any{"Item": it, "Origin": scheme + "://" + r.Host}
	if c, err := r.Cookie(cookieName); err == nil {
		data["Token"] = c.Value
	}
	if it.ParentID != "" {
		if parent, err := s.Store.GetItem(r.Context(), it.ParentID); err == nil {
			data["Parent"] = parent
		}
	}
	versions, err := s.versionViews(r.Context(), it.ID, store.User{})
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	data["Versions"] = versions
	children, err := s.Store.ListItems(r.Context(), store.ItemFilter{ParentID: it.ID, Limit: 500, IncludeMissing: true})
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	sortChildren(children)
	ids := make([]string, len(children))
	for i, c := range children {
		ids[i] = c.ID
	}
	states, err := s.Store.WatchStates(r.Context(), u.ProfileID, append(ids, it.ID))
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	rows := make([]childRow, len(children))
	for i, c := range children {
		rows[i] = childRow{Item: c, Watch: states[c.ID]}
	}
	data["Children"] = rows
	data["Watch"] = states[it.ID]
	if it.Kind == store.ItemShow {
		if nu, err := s.nextUp(r, u.ProfileID, it.ID); err == nil {
			data["NextUp"] = nu
		}
	}
	data["CanMatch"] = s.Matcher != nil && (it.Kind == store.ItemMovie || it.Kind == store.ItemShow)
	s.render(w, "item.html", page{Title: it.Title, User: u, Message: r.URL.Query().Get("msg"),
		Error: r.URL.Query().Get("err"), Data: data})
}

func (s *Server) uiFixMatch(w http.ResponseWriter, r *http.Request, u store.User) {
	id := r.PathValue("id")
	tmdbID, _ := strconv.Atoi(strings.TrimSpace(r.FormValue("tmdbId")))
	it, err := s.applyFix(r, id, tmdbID)
	if err != nil {
		redirect(w, r, "/ui/items/"+id, "", err.Error())
		return
	}
	target := "/ui/items/" + it.ID
	if _, err := s.Store.GetItem(r.Context(), it.ID); err != nil {
		target = "/"
	}
	redirect(w, r, target, "Matched to "+it.Title+".", "")
}

func (s *Server) uiWatched(w http.ResponseWriter, r *http.Request, u store.User) {
	id := r.PathValue("id")
	it, err := s.Store.GetItem(r.Context(), id)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	watched := r.FormValue("watched") == "1"
	if err := s.Watch.SetWatched(r.Context(), u.ProfileID, it, watched); err != nil {
		redirect(w, r, "/ui/items/"+id, "", err.Error())
		return
	}
	back := r.FormValue("back")
	if !strings.HasPrefix(back, "/ui/items/") {
		back = "/ui/items/" + id
	}
	redirect(w, r, back, "", "")
}
