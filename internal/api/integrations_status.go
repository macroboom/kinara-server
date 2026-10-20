package api

import (
	"context"
	"errors"
	"fmt"
	"math"
	"net/http"
	"strconv"
	"strings"
	"time"

	"kinara/internal/store"
	"kinara/internal/tmdb"
)

// Titles on their way: statuses from incoming updates, the title they are
// about, and what the apps show.

// defaultExpireHours is how long a status sent with an API key lasts
// without updates.
const defaultExpireHours = 6

// titleMatch is the title an update is about.
type titleMatch struct {
	Kind   string
	TMDBID int
	Title  string
	Year   int
	// Item is the library's copy (a movie or show), nil when not on the
	// server yet.
	Item *store.Item
}

// cachedMatch remembers a TMDB lookup (m nil: TMDB has no such title), so
// a tool reporting every few seconds doesn't ask TMDB every time.
type cachedMatch struct {
	m  *titleMatch
	at time.Time
}

const matchCacheFor = 6 * time.Hour

var errKindNeeded = errors.New(`say whether tmdbId is a movie or a show: add "kind": "movie" or "show"`)

func (s *Server) cachedTitle(key string) (*titleMatch, bool) {
	s.integ.mu.Lock()
	defer s.integ.mu.Unlock()
	c, ok := s.integ.titles[key]
	if !ok || time.Since(c.at) > matchCacheFor {
		return nil, false
	}
	return c.m, true
}

func (s *Server) cacheTitle(key string, m *titleMatch) {
	s.integ.mu.Lock()
	defer s.integ.mu.Unlock()
	if s.integ.titles == nil || len(s.integ.titles) > 5000 {
		s.integ.titles = map[string]cachedMatch{}
	}
	s.integ.titles[key] = cachedMatch{m: m, at: time.Now()}
}

func tmdbProvider(kind string) string {
	if kind == "show" {
		return store.ProviderTMDBTV
	}
	return store.ProviderTMDBMovie
}

// libraryTitle is the library's movie or show with this TMDB ID, or nil.
func (s *Server) libraryTitle(ctx context.Context, kind string, tmdbID int) *store.Item {
	items, err := s.Store.ItemsByExternalIDs(ctx, tmdbProvider(kind), []string{strconv.Itoa(tmdbID)})
	if err != nil {
		return nil
	}
	if it, ok := items[strconv.Itoa(tmdbID)]; ok {
		return &it
	}
	return nil
}

// matchFromItem describes a library movie or show as a match.
func (s *Server) matchFromItem(ctx context.Context, it store.Item) *titleMatch {
	kind := "movie"
	if it.Kind == store.ItemShow {
		kind = "show"
	}
	m := &titleMatch{Kind: kind, Title: it.Title, Year: it.Year, Item: &it}
	if v, err := s.Store.ExternalID(ctx, it.ID, tmdbProvider(kind)); err == nil {
		m.TMDBID, _ = strconv.Atoi(v)
	}
	return m
}

// matchTMDB matches a TMDB ID: the library's copy, else TMDB itself. TMDB
// saying it has no such title is the only "no match"; without TMDB (no key,
// or it can't be reached) the sender's word is taken.
func (s *Server) matchTMDB(ctx context.Context, kind string, id int, title string, year int) *titleMatch {
	if it := s.libraryTitle(ctx, kind, id); it != nil {
		return s.matchFromItem(ctx, *it)
	}
	key := fmt.Sprintf("%s:%d", kind, id)
	if m, ok := s.cachedTitle(key); ok {
		return m
	}
	given := &titleMatch{Kind: kind, TMDBID: id, Title: title, Year: year}
	if s.TMDB == nil {
		return given
	}
	var m *titleMatch
	var err error
	if kind == "show" {
		var d tmdb.TVDetails
		if d, err = s.TMDB.TV(ctx, id); err == nil {
			m = &titleMatch{Kind: kind, TMDBID: id, Title: d.Name, Year: tmdb.YearOf(d.FirstAirDate)}
		}
	} else {
		var d tmdb.MovieDetails
		if d, err = s.TMDB.Movie(ctx, id); err == nil {
			m = &titleMatch{Kind: kind, TMDBID: id, Title: d.Title, Year: tmdb.YearOf(d.ReleaseDate)}
		}
	}
	var se *tmdb.StatusError
	switch {
	case err == nil:
	case errors.As(err, &se) && se.Code == http.StatusNotFound:
		m = nil
	default:
		s.Log.Debug("integrations: TMDB lookup", "kind", kind, "id", id, "err", err)
		return given
	}
	s.cacheTitle(key, m)
	return m
}

// pickFound chooses among TMDB's finds, preferring the kind the sender said.
func (s *Server) pickFound(ctx context.Context, found []tmdb.FoundTitle, kind string) *titleMatch {
	for _, f := range found {
		if kind == "" || f.Kind == kind {
			if it := s.libraryTitle(ctx, f.Kind, f.ID); it != nil {
				return s.matchFromItem(ctx, *it)
			}
			return &titleMatch{Kind: f.Kind, TMDBID: f.ID, Title: f.Title, Year: f.Year}
		}
	}
	return nil
}

// matchTitle works out which title an update is about: by TMDB ID, IMDb or
// TVDB ID (through TMDB), or title and year (the library first, then a
// TMDB search). nil when nothing matched.
func (s *Server) matchTitle(ctx context.Context, upd statusUpdate) (*titleMatch, error) {
	if upd.TMDBID > 0 {
		kind := upd.Kind
		if kind == "" {
			// The library may know which it is.
			mv, sh := s.libraryTitle(ctx, "movie", upd.TMDBID), s.libraryTitle(ctx, "show", upd.TMDBID)
			switch {
			case mv != nil && sh == nil:
				kind = "movie"
			case sh != nil && mv == nil:
				kind = "show"
			default:
				return nil, errKindNeeded
			}
		}
		return s.matchTMDB(ctx, kind, upd.TMDBID, upd.Title, upd.Year), nil
	}
	if upd.IMDBID != "" {
		items, err := s.Store.ItemsByExternalIDs(ctx, store.ProviderIMDB, []string{upd.IMDBID})
		if it, ok := items[upd.IMDBID]; err == nil && ok && (it.Kind == store.ItemMovie || it.Kind == store.ItemShow) {
			return s.matchFromItem(ctx, it), nil
		}
		if m := s.findExternal(ctx, "imdb_id", upd.IMDBID, upd.Kind); m != nil {
			return m, nil
		}
	}
	if upd.TVDBID > 0 {
		kind := upd.Kind
		if kind == "" {
			kind = "show"
		}
		if m := s.findExternal(ctx, "tvdb_id", strconv.Itoa(upd.TVDBID), kind); m != nil {
			return m, nil
		}
	}
	if upd.Title != "" {
		return s.matchByName(ctx, upd.Kind, upd.Title, upd.Year), nil
	}
	return nil, nil
}

// findExternal asks TMDB which title an IMDb or TVDB ID is.
func (s *Server) findExternal(ctx context.Context, source, id, kind string) *titleMatch {
	key := "find:" + source + ":" + id + ":" + kind
	if m, ok := s.cachedTitle(key); ok {
		return m
	}
	if s.TMDB == nil {
		return nil
	}
	found, err := s.TMDB.FindExternal(ctx, source, id)
	if err != nil {
		s.Log.Debug("integrations: TMDB find", "source", source, "id", id, "err", err)
		return nil
	}
	m := s.pickFound(ctx, found, kind)
	s.cacheTitle(key, m)
	return m
}

// matchByName matches a title and year: the library's copy, else the top
// TMDB search result.
func (s *Server) matchByName(ctx context.Context, kind, title string, year int) *titleMatch {
	kinds := []string{"movie", "show"}
	if kind != "" {
		kinds = []string{kind}
	}
	for _, k := range kinds {
		itemKind := store.ItemMovie
		if k == "show" {
			itemKind = store.ItemShow
		}
		if it, err := s.Store.FindTitle(ctx, itemKind, title, year); err == nil {
			return s.matchFromItem(ctx, it)
		}
	}
	key := fmt.Sprintf("search:%s:%s:%d", kind, strings.ToLower(title), year)
	if m, ok := s.cachedTitle(key); ok {
		return m
	}
	if s.TMDB == nil {
		return nil
	}
	var found []tmdb.FoundTitle
	for _, k := range kinds {
		if k == "show" {
			res, err := s.TMDB.SearchTV(ctx, title, year)
			if err == nil && len(res) > 0 {
				found = append(found, tmdb.FoundTitle{ID: res[0].ID, Kind: "show", Title: res[0].Name, Year: tmdb.YearOf(res[0].FirstAirDate)})
			}
		} else {
			res, err := s.TMDB.SearchMovie(ctx, title, year)
			if err == nil && len(res) > 0 {
				found = append(found, tmdb.FoundTitle{ID: res[0].ID, Kind: "movie", Title: res[0].Title, Year: tmdb.YearOf(res[0].ReleaseDate)})
			}
		}
		if len(found) > 0 {
			break
		}
	}
	m := s.pickFound(ctx, found, kind)
	s.cacheTitle(key, m)
	return m
}

// statusIdent says which thing an update is about, from what it says (not
// what it matched, so it is known before any lookup): the sender's own
// reference, else the title's IDs or name, and the season and episode.
func statusIdent(upd statusUpdate) string {
	var id string
	switch {
	case upd.Ref != "":
		return "ref:" + upd.Ref
	case upd.TMDBID > 0:
		id = fmt.Sprintf("tmdb:%s:%d", upd.Kind, upd.TMDBID)
	case upd.IMDBID != "":
		id = "imdb:" + upd.IMDBID
	case upd.TVDBID > 0:
		id = fmt.Sprintf("tvdb:%d", upd.TVDBID)
	default:
		id = fmt.Sprintf("title:%s:%s:%d", upd.Kind, strings.ToLower(upd.Title), upd.Year)
	}
	return fmt.Sprintf("%s:%d:%d", id, upd.Season, upd.Episode)
}

// statusResult is what came of an update.
type statusResult struct {
	matched    *titleMatch
	status     store.TitleStatus
	summary    string
	scan       string
	retryAfter time.Duration
}

// view is the answer to the sender (see the manager's Reference tab).
func (res statusResult) view() map[string]any {
	out := map[string]any{"state": res.status.State}
	if res.status.ID > 0 && res.status.State != stateDone {
		out["id"] = res.status.ID
	}
	if res.scan != "" {
		out["scan"] = res.scan
	}
	if m := res.matched; m != nil {
		matched := map[string]any{"kind": m.Kind, "title": m.Title}
		if m.Year > 0 {
			matched["year"] = m.Year
		}
		if m.TMDBID > 0 {
			matched["tmdbId"] = m.TMDBID
		}
		if m.Item != nil {
			matched["item"] = map[string]string{"id": m.Item.ID}
		}
		out["matched"] = matched
		return out
	}
	out["matched"] = nil
	if res.status.State == stateDone {
		out["message"] = "No title matched; the status was cleared."
	} else {
		out["kept"] = true
		out["message"] = "No title matched yet. The status is kept and shows once the title is known."
	}
	return out
}

var stateWords = map[string]string{stateQueued: "queued", stateInProgress: "in progress", stateDone: "ready", stateFailed: "failed"}

// describeStatus is a one-line summary for the received log.
func describeStatus(st store.TitleStatus) string {
	var b strings.Builder
	b.WriteString(st.Title)
	if st.Year > 0 && st.Season < 0 {
		fmt.Fprintf(&b, " (%d)", st.Year)
	}
	if st.Season >= 0 {
		fmt.Fprintf(&b, " S%02d", st.Season)
		if st.Episode >= 0 {
			fmt.Fprintf(&b, "E%02d", st.Episode)
		}
	}
	b.WriteString(" · " + stateWords[st.State])
	if st.Progress != nil && st.State != stateDone {
		fmt.Fprintf(&b, " %d%%", int(*st.Progress*100+0.5))
	}
	return b.String()
}

// unmatchedSummary says what was looked for when nothing matched.
func unmatchedSummary(upd statusUpdate) string {
	switch {
	case upd.TMDBID > 0:
		return fmt.Sprintf("No title matched tmdbId %d", upd.TMDBID)
	case upd.IMDBID != "":
		return "No title matched imdbId " + upd.IMDBID
	case upd.TVDBID > 0:
		return fmt.Sprintf("No title matched tvdbId %d", upd.TVDBID)
	case upd.Year > 0:
		return fmt.Sprintf("No title matched %q (%d)", upd.Title, upd.Year)
	}
	return fmt.Sprintf("No title matched %q", upd.Title)
}

// idLabel names a title by the ID it was sent with, when no name is known.
func idLabel(upd statusUpdate) string {
	switch {
	case upd.TMDBID > 0:
		return fmt.Sprintf("TMDB %d", upd.TMDBID)
	case upd.IMDBID != "":
		return "IMDb " + upd.IMDBID
	case upd.TVDBID > 0:
		return fmt.Sprintf("TVDB %d", upd.TVDBID)
	}
	return "Unknown title"
}

// applyStatus matches an update's title and saves (or, when done, clears)
// its status. It answers the HTTP status to send.
func (s *Server) applyStatus(r *http.Request, src statusSource, upd statusUpdate) (statusResult, int, error) {
	ctx := r.Context()
	ident := statusIdent(upd)
	if ok, wait := s.integ.limits.allow(src.key+"|"+ident, perTitleLimit, perTitleWindow, time.Now()); !ok {
		return statusResult{retryAfter: wait}, http.StatusTooManyRequests,
			errors.New("too many updates for this title: at most 2 a second")
	}
	m, err := s.matchTitle(ctx, upd)
	if err != nil {
		return statusResult{}, http.StatusBadRequest, err
	}
	now := time.Now().Unix()
	expire, visibility := defaultExpireHours, "everyone"
	st := store.TitleStatus{Source: src.key, SourceName: src.name, Ident: ident, Kind: upd.Kind, Title: upd.Title,
		Year: upd.Year, Season: upd.Season, Episode: upd.Episode, State: upd.State, Progress: upd.Progress,
		Message: upd.Message, EtaSec: upd.EtaSec, UpdatedAt: now}
	if src.hook != nil {
		st.HookID = src.hook.ID
		expire, visibility = src.hook.ExpireHours, src.hook.Visibility
	}
	st.ExpiresAt, st.Visibility = now+int64(expire)*3600, visibility
	if m != nil {
		st.Kind, st.TMDBID, st.Title, st.Year = m.Kind, m.TMDBID, m.Title, m.Year
		if m.Item != nil {
			st.ItemID = m.Item.ID
		}
	}
	if st.Title == "" {
		st.Title = idLabel(upd)
	}
	res := statusResult{matched: m, status: st, summary: describeStatus(st)}
	if m == nil {
		res.summary = unmatchedSummary(upd)
	}

	prev, prevErr := s.Store.GetStatusFor(ctx, src.key, ident)
	if st.State == stateDone {
		if err := s.Store.DeleteStatusFor(ctx, src.key, ident); err != nil {
			return res, http.StatusInternalServerError, err
		}
		res.scan = s.readyScan(src, m, st.Kind)
		s.forgetProgress(src.key + "|" + ident)
		s.noteStatus("status.done", src, st, m, res.scan)
	} else {
		if err := s.Store.PutStatus(ctx, &st); err != nil {
			return res, http.StatusInternalServerError, err
		}
		res.status = st
		newState := prevErr != nil || prev.State != st.State
		if newState && st.State == stateFailed && st.TMDBID > 0 {
			// Its requests are done for: free the title to be asked for again.
			s.releaseFailed(ctx, st.Kind, st.TMDBID, st.Message)
		}
		if s.progressWorthSending(src.key+"|"+ident, newState, st.Progress, time.Now()) {
			s.noteStatus("status.updated", src, st, m, "")
		}
	}
	if m == nil {
		return res, http.StatusOK, nil
	}
	return res, http.StatusAccepted, nil
}

// readyScan scans for a title that is ready, if its sender wants that: the
// library it is in, or every library of its kind when it isn't in one yet.
// It says what it did: started, off, or none (no library to scan).
func (s *Server) readyScan(src statusSource, m *titleMatch, kind string) string {
	if src.hook != nil && src.hook.OnDone != "scan" {
		return "off"
	}
	if s.Scanner == nil {
		return "none"
	}
	ctx := s.baseCtx()
	var ids []int64
	if m != nil && m.Item != nil {
		ids = []int64{m.Item.LibraryID}
	} else if libs, err := s.Store.ListLibraries(ctx); err == nil {
		for _, l := range libs {
			if (kind != "show" && l.Kind == store.KindMovies) || (kind != "movie" && l.Kind == store.KindTV) {
				ids = append(ids, l.ID)
			}
		}
	}
	started := 0
	for _, id := range ids {
		if err := s.Scanner.Queue(ctx, id); err == nil {
			started++
		}
	}
	if started == 0 {
		return "none"
	}
	return "started"
}

// statusView is a title on its way, as the apps and the manager show it.
type statusView struct {
	ID        int64    `json:"id"`
	Kind      string   `json:"kind"`
	TMDBID    int      `json:"tmdbId"`
	Title     string   `json:"title"`
	Year      int      `json:"year,omitempty"`
	Season    *int     `json:"season,omitempty"`
	Episode   *int     `json:"episode,omitempty"`
	State     string   `json:"state"`
	Progress  *float64 `json:"progress,omitempty"`
	Message   string   `json:"message,omitempty"`
	EtaSec    int      `json:"etaSec,omitempty"`
	UpdatedAt int64    `json:"updatedAt"`
	// Source is the name of the hook or key that sent it.
	Source string `json:"source"`
	// Item is the library's copy, once the title is on the server.
	Item *itemRef `json:"item,omitempty"`
}

type itemRef struct {
	ID string `json:"id"`
}

func numberOrNil(n int) *int {
	if n < 0 {
		return nil
	}
	return &n
}

// listStatuses lists titles on their way the asker may see: owners see
// all, others those their hooks share with everyone.
func (s *Server) listStatuses(w http.ResponseWriter, r *http.Request, u store.User) {
	list, err := s.Store.Statuses(r.Context(), u.IsAdmin)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	// Titles that arrived since their status was last updated.
	want := map[string][]string{}
	for _, st := range list {
		if st.TMDBID > 0 {
			want[st.Kind] = append(want[st.Kind], strconv.Itoa(st.TMDBID))
		}
	}
	owned := map[string]map[string]store.Item{}
	for kind, ids := range want {
		owned[kind], _ = s.Store.ItemsByExternalIDs(r.Context(), tmdbProvider(kind), ids)
	}
	out := make([]statusView, 0, len(list))
	for _, st := range list {
		v := statusView{ID: st.ID, Kind: st.Kind, TMDBID: st.TMDBID, Title: st.Title, Year: st.Year,
			Season: numberOrNil(st.Season), Episode: numberOrNil(st.Episode), State: st.State, Progress: st.Progress,
			Message: st.Message, EtaSec: st.EtaSec, UpdatedAt: st.UpdatedAt, Source: st.SourceName}
		if it, ok := owned[st.Kind][strconv.Itoa(st.TMDBID)]; ok && st.TMDBID > 0 {
			v.Item = &itemRef{ID: it.ID}
		}
		out = append(out, v)
	}
	writeJSON(w, http.StatusOK, map[string]any{"items": out})
}

// clearStatus removes a status by hand.
func (s *Server) clearStatus(w http.ResponseWriter, r *http.Request, u store.User) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err == nil {
		err = s.Store.DeleteStatus(r.Context(), id)
	}
	if err != nil {
		writeErr(w, http.StatusNotFound, "no such status")
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// titleStatusView is a title's newest incoming status, as the apps show it.
type titleStatusView struct {
	State     string   `json:"state"`
	Progress  *float64 `json:"progress,omitempty"`
	Message   string   `json:"message,omitempty"`
	EtaSec    int      `json:"etaSec,omitempty"`
	UpdatedAt int64    `json:"updatedAt"`
}

// titleStatus is the newest status for a title that viewer may see, or nil.
func (s *Server) titleStatus(kind string, tmdbID int, viewer store.User) *titleStatusView {
	kind = normalKind(kind)
	if kind == "" || tmdbID <= 0 {
		return nil
	}
	st, err := s.Store.StatusForTitle(s.baseCtx(), kind, tmdbID, viewer.IsAdmin)
	if err != nil {
		return nil
	}
	return titleStatusOf(st)
}

// --- Progress for webhooks ----------------------------------------------------

// status.updated goes out on every new state, and while a title stays in
// one state, when its progress has moved at least progressStep since the
// last one and progressEvery has passed. A webhook can then show a live
// progress bar without getting every update a downloader sends.
const (
	progressStep  = 0.05
	progressEvery = 10 * time.Second
)

type progressMark struct {
	at       time.Time
	progress float64
}

func (s *Server) progressWorthSending(key string, newState bool, progress *float64, now time.Time) bool {
	s.integ.mu.Lock()
	defer s.integ.mu.Unlock()
	if s.integ.progressSent == nil {
		s.integ.progressSent = map[string]progressMark{}
	}
	last, seen := s.integ.progressSent[key]
	p := -1.0
	if progress != nil {
		p = *progress
	}
	send := newState || !seen
	if !send && p >= 0 && now.Sub(last.at) >= progressEvery && math.Abs(p-last.progress) >= progressStep {
		send = true
	}
	if send {
		s.integ.progressSent[key] = progressMark{at: now, progress: p}
		// Forget titles nobody has updated for a day, so the map stays small.
		if len(s.integ.progressSent) > 500 {
			for k, v := range s.integ.progressSent {
				if now.Sub(v.at) > 24*time.Hour {
					delete(s.integ.progressSent, k)
				}
			}
		}
	}
	return send
}

func (s *Server) forgetProgress(key string) {
	s.integ.mu.Lock()
	delete(s.integ.progressSent, key)
	s.integ.mu.Unlock()
}
