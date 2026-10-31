package api

import (
	"context"
	"errors"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"kinara/internal/store"
	"kinara/internal/tmdb"
)

// Requests: anyone signed in can ask for a movie or show that isn't on the
// server. Owners approve or decline them; a trusted device's requests are
// approved straight away. Approving emits request.approved, which the
// owner's own scripts and tools can act on through a webhook.

// requestsState holds what requests and discovery need at run time.
type requestsState struct {
	// mu makes "is there one already?" and "make one" a single step, so a
	// double tap doesn't make two requests.
	mu sync.Mutex
	// hub is the Discover page's sections (discover_hub.go).
	hub hubCache
	// genres are TMDB's genre lists, for naming genres on titles.
	genres genreCache
	// pages keeps paged discover and search answers for an hour.
	pages externalCache
}

// requestRoutes registers the requests, devices and discover endpoints.
func (s *Server) requestRoutes(mux *http.ServeMux) {
	mux.Handle("POST /api/v1/requests", s.user(s.createRequest))
	mux.Handle("GET /api/v1/requests", s.user(s.listRequests))
	mux.Handle("POST /api/v1/requests/{id}/approve", s.admin(s.approveRequest))
	mux.Handle("POST /api/v1/requests/{id}/decline", s.admin(s.declineRequest))
	mux.Handle("DELETE /api/v1/requests/{id}", s.user(s.deleteRequest))
	mux.Handle("PUT /api/v1/clients/{clientId}", s.admin(s.updateClient))
	mux.Handle("PUT /api/v1/me/device", s.user(s.reportDevice))
	mux.Handle("GET /api/v1/pulse", s.user(s.pulse))

	mux.Handle("GET /api/v1/discover", s.user(s.discoverHub))
	mux.Handle("GET /api/v1/discover/{section}", s.user(s.discoverSection))
	mux.Handle("GET /api/v1/discover/genre/{kind}/{genreId}", s.user(s.discoverGenre))
	mux.Handle("GET /api/v1/search/external", s.user(s.searchExternal))
}

// --- Views ---

type requestUserView struct {
	ID   int64  `json:"id"`
	Name string `json:"name"`
}

type requestDeviceView struct {
	ID   string `json:"id"`
	Name string `json:"name"`
	Icon string `json:"icon"`
}

// requestView is a request as the apps show it.
type requestView struct {
	ID            int64             `json:"id"`
	Kind          string            `json:"kind"`
	TMDBID        int               `json:"tmdbId"`
	Title         string            `json:"title"`
	Year          int               `json:"year"`
	PosterURL     string            `json:"posterUrl"`
	Mode          string            `json:"mode"`
	Seasons       []int             `json:"seasons"`
	Status        string            `json:"status"`
	Auto          bool              `json:"auto"`
	Note          string            `json:"note"`
	DeclineReason string            `json:"declineReason"`
	RequestedBy   requestUserView   `json:"requestedBy"`
	Device        requestDeviceView `json:"device"`
	CreatedAt     int64             `json:"createdAt"`
	DecidedAt     int64             `json:"decidedAt"`
}

func newRequestView(r store.Request) requestView {
	return requestView{
		ID: r.ID, Kind: r.Kind, TMDBID: r.TMDBID, Title: r.Title, Year: r.Year, PosterURL: tmdbImageURL(r.PosterPath),
		Mode: r.Mode, Seasons: r.Seasons, Status: r.Status, Auto: r.Auto, Note: r.Note, DeclineReason: r.DeclineReason,
		RequestedBy: requestUserView{ID: r.UserID, Name: r.Username},
		Device:      requestDeviceView{ID: r.ClientID, Name: r.DeviceName, Icon: r.DeviceIcon},
		CreatedAt:   r.CreatedAt, DecidedAt: r.DecidedAt,
	}
}

// requestMarker goes on titles shown outside the library (title pages,
// discover, search, trending) when someone has an open request for them.
type requestMarker struct {
	ID     int64  `json:"id"`
	Status string `json:"status"`
	// Mine is true when the person looking made the request.
	Mine    bool   `json:"mine"`
	Mode    string `json:"mode"`
	Seasons []int  `json:"seasons"`
}

// requestEventData is the "data" of request.* webhook events.
type requestEventData struct {
	Request requestEvent `json:"request"`
}

type requestEvent struct {
	ID          int64              `json:"id"`
	Kind        string             `json:"kind"`
	Title       string             `json:"title"`
	Year        int                `json:"year"`
	IDs         requestEventIDs    `json:"ids"`
	Mode        string             `json:"mode"`
	Seasons     []int              `json:"seasons"`
	Note        string             `json:"note"`
	Auto        bool               `json:"auto"`
	RequestedBy requestUserView    `json:"requestedBy"`
	Device      requestEventDevice `json:"device"`
	// Reason is the owner's, on request.declined.
	Reason string `json:"reason,omitempty"`
}

type requestEventIDs struct {
	TMDB int    `json:"tmdb"`
	TVDB int    `json:"tvdb,omitempty"`
	IMDB string `json:"imdb,omitempty"`
}

type requestEventDevice struct {
	ID       string `json:"id"`
	Name     string `json:"name"`
	Platform string `json:"platform"`
}

// emitRequest tells the webhooks about a request. Only webhooks open to
// the requester's account get it.
func (s *Server) emitRequest(event string, r store.Request) {
	data := requestEventData{Request: requestEvent{
		ID: r.ID, Kind: r.Kind, Title: r.Title, Year: r.Year,
		IDs:  requestEventIDs{TMDB: r.TMDBID, TVDB: r.TVDBID, IMDB: r.IMDBID},
		Mode: r.Mode, Seasons: r.Seasons, Note: r.Note, Auto: r.Auto,
		RequestedBy: requestUserView{ID: r.UserID, Name: r.Username},
		Device:      requestEventDevice{ID: r.ClientID, Name: r.DeviceName, Platform: r.DevicePlatform},
	}}
	if event == "request.declined" {
		data.Request.Reason = r.DeclineReason
	}
	s.emit(event, eventScope{UserID: r.UserID}, data)
}

// --- Create ---

// noteMax is how long a request's note (and a decline reason) may be.
const noteMax = 280

type createRequestBody struct {
	Kind    string `json:"kind"`
	TMDBID  int    `json:"tmdbId"`
	Mode    string `json:"mode"`
	Seasons []int  `json:"seasons"`
	Note    string `json:"note"`
}

type createRequestResponse struct {
	requestView
	// Sent is true when it went straight to the webhooks (trusted device).
	Sent bool `json:"sent"`
	// Duplicate is true when an identical open request was returned
	// instead of making another.
	Duplicate bool `json:"duplicate,omitempty"`
	// OnTheWay is true when the title was already on its way: the request is
	// recorded as approved, and nothing is sent to the webhooks.
	OnTheWay bool `json:"onTheWay,omitempty"`
}

func (s *Server) createRequest(w http.ResponseWriter, r *http.Request, u store.User) {
	var body createRequestBody
	if err := readJSON(r, &body); err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	if (body.Kind != "movie" && body.Kind != "show") || body.TMDBID <= 0 {
		writeErr(w, http.StatusBadRequest, `say which title: "kind" is movie or show, with its "tmdbId"`)
		return
	}
	body.Note = strings.TrimSpace(body.Note)
	if utf8.RuneCountInString(body.Note) > noteMax {
		writeErr(w, http.StatusBadRequest, "keep the note to 280 characters")
		return
	}
	if body.Kind == "movie" {
		body.Mode, body.Seasons = store.RequestModeAll, nil
	}
	switch body.Mode {
	case "":
		body.Mode = store.RequestModeAll
	case store.RequestModeAll, store.RequestModePilot:
		body.Seasons = nil
	case store.RequestModeSeasons:
		if len(body.Seasons) == 0 {
			writeErr(w, http.StatusBadRequest, "pick at least one season")
			return
		}
	default:
		writeErr(w, http.StatusBadRequest, `"mode" is all, seasons or pilot`)
		return
	}
	if s.TMDB == nil {
		writeErr(w, http.StatusNotImplemented, "set a TMDB key on the server to request titles")
		return
	}
	ctx := r.Context()
	v, err := s.external.cached(body.Kind+"/"+strconv.Itoa(body.TMDBID), func() (any, error) {
		return s.fetchExternalTitle(ctx, body.Kind, body.TMDBID)
	})
	if err != nil {
		writeErr(w, tmdbStatus(err), "couldn't get that title from TMDB: "+err.Error())
		return
	}
	title := v.(externalTitleView)
	if body.Mode == store.RequestModeSeasons {
		for _, n := range body.Seasons {
			if !slices.ContainsFunc(title.Seasons, func(sn externalSeasonRef) bool { return sn.Number == n }) {
				writeErr(w, http.StatusBadRequest, "this show has no season "+strconv.Itoa(n))
				return
			}
		}
		body.Seasons = parseSeasonsKey(store.SeasonsKey(body.Seasons))
	}

	owned, err := s.titleOwned(ctx, body.Kind, body.TMDBID, body.Mode, body.Seasons)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	if owned {
		s.markTitleAvailable(ctx, body.Kind, body.TMDBID)
		what := "That movie is"
		if body.Kind == "show" {
			what = "That show is"
			if body.Mode == store.RequestModeSeasons {
				what = "Those seasons are"
			}
		}
		writeErr(w, http.StatusConflict, what+" already on the server")
		return
	}

	// What may take a while (TMDB's other IDs) before the lock, so one
	// slow answer doesn't hold up everyone else's requests.
	client, err := s.Store.SessionClient(ctx, sessionFrom(r).tokenHash)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	ids := s.titleIDs(ctx, body.Kind, body.TMDBID)

	s.reqs.mu.Lock()
	defer s.reqs.mu.Unlock()
	if dup, err := s.Store.FindOpenRequest(ctx, body.Kind, body.TMDBID, body.Mode, body.Seasons); err == nil {
		view := newRequestView(dup)
		if dup.UserID != u.ID && !u.IsAdmin {
			// Someone else's: that it exists, not their note or device.
			view.Note, view.RequestedBy, view.Device = "", requestUserView{}, requestDeviceView{}
		}
		writeJSON(w, http.StatusOK, createRequestResponse{requestView: view, Duplicate: true})
		return
	} else if !errors.Is(err, store.ErrNotFound) {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}

	req := store.Request{
		Kind: body.Kind, TMDBID: body.TMDBID, TVDBID: ids.TVDBID, IMDBID: ids.IMDBID,
		Title: title.Title, Year: title.Year, PosterPath: tmdbPath(title.PosterURL),
		Mode: body.Mode, Seasons: body.Seasons, Status: store.RequestPending, Note: body.Note,
		UserID: u.ID, Username: u.Username,
		ClientID: client.ID, DeviceName: client.Name, DeviceIcon: client.Icon, DevicePlatform: client.Platform,
	}
	if req.Seasons == nil {
		req.Seasons = []int{}
	}
	req.CreatedAt = time.Now().Unix()
	// Already coming: approve it without bothering the owner or the
	// webhooks, so the tools aren't asked to add it a second time.
	if s.alreadyOnTheWay(ctx, req.Kind, req.TMDBID, req.Mode) {
		req.Status, req.DecidedAt = store.RequestApproved, req.CreatedAt
		if err := s.Store.CreateRequest(ctx, &req); err != nil {
			writeErr(w, http.StatusInternalServerError, err.Error())
			return
		}
		writeJSON(w, http.StatusCreated, createRequestResponse{requestView: newRequestView(req), OnTheWay: true})
		return
	}
	if client.Trusted && s.trustedFor(ctx, client.ID, u.ID) {
		req.Status, req.Auto, req.DecidedAt = store.RequestApproved, true, req.CreatedAt
	}
	if err := s.Store.CreateRequest(ctx, &req); err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	s.emitRequest("request.created", req)
	if req.Auto {
		s.emitRequest("request.approved", req)
	}
	writeJSON(w, http.StatusCreated, createRequestResponse{requestView: newRequestView(req), Sent: req.Auto})
}

// trustedFor: a trusted device's requests are approved straight away only
// while it's signed in to just this account. A device's ID is made up by
// the app and sent in the clear, so someone who learns a trusted device's
// ID and signs in to their own account with it gets no free approvals.
func (s *Server) trustedFor(ctx context.Context, clientID string, userID int64) bool {
	if store.SessionOfDerivedClient(clientID) != "" {
		return true // made from this very session (an app that sends no ID)
	}
	users, err := s.Store.ClientUsers(ctx, clientID)
	return err == nil && len(users) == 1 && users[0] == userID
}

func parseSeasonsKey(key string) []int {
	out := []int{}
	for _, p := range strings.Split(key, ",") {
		if n, err := strconv.Atoi(p); err == nil {
			out = append(out, n)
		}
	}
	return out
}

// tmdbPath turns our image URL back into TMDB's path ("/abc.jpg").
func tmdbPath(imageURL string) string {
	file, ok := strings.CutPrefix(imageURL, "/api/v1/tmdb-image/")
	if !ok || file == "" {
		return ""
	}
	return "/" + file
}

// titleIDs looks up a title's TVDB and IMDb IDs on TMDB. Missing IDs don't
// stop a request: the owner's tools can still go by the TMDB ID.
func (s *Server) titleIDs(ctx context.Context, kind string, tmdbID int) tmdb.ExternalIDs {
	media := "movie"
	if kind == "show" {
		media = "tv"
	}
	v, err := s.external.cached("ids/"+kind+"/"+strconv.Itoa(tmdbID), func() (any, error) {
		return s.TMDB.ExternalIDs(ctx, media, tmdbID)
	})
	if err != nil {
		s.Log.Warn("couldn't get a title's other IDs from TMDB", "kind", kind, "tmdbId", tmdbID, "err", err)
		return tmdb.ExternalIDs{}
	}
	return v.(tmdb.ExternalIDs)
}

// titleOwned reports whether the library already has what a request asks
// for: the movie, the show (all or pilot), or every season asked for.
func (s *Server) titleOwned(ctx context.Context, kind string, tmdbID int, mode string, seasons []int) (bool, error) {
	provider := store.ProviderTMDBMovie
	if kind == "show" {
		provider = store.ProviderTMDBTV
	}
	key := strconv.Itoa(tmdbID)
	owned, err := s.Store.ItemsByExternalIDs(ctx, provider, []string{key})
	if err != nil {
		return false, err
	}
	it, ok := owned[key]
	if !ok {
		return false, nil
	}
	if kind == "movie" || mode != store.RequestModeSeasons {
		return true, nil
	}
	return s.hasSeasons(ctx, it.ID, seasons)
}

func (s *Server) hasSeasons(ctx context.Context, showID string, seasons []int) (bool, error) {
	children, err := s.Store.Children(ctx, showID)
	if err != nil {
		return false, err
	}
	have := map[int]bool{}
	for _, c := range children {
		if c.Kind == store.ItemSeason {
			have[c.SeasonNumber] = true
		}
	}
	for _, n := range seasons {
		if !have[n] {
			return false, nil
		}
	}
	return true, nil
}

// --- Available ---

// markAvailable moves open requests whose title has reached the library to
// "available".
func (s *Server) markAvailable(ctx context.Context, reqs []store.Request) {
	ids := map[string][]string{}
	for _, r := range reqs {
		p := store.ProviderTMDBMovie
		if r.Kind == "show" {
			p = store.ProviderTMDBTV
		}
		ids[p] = append(ids[p], strconv.Itoa(r.TMDBID))
	}
	owned := map[string]map[string]store.Item{}
	for p, vals := range ids {
		m, err := s.Store.ItemsByExternalIDs(ctx, p, vals)
		if err != nil {
			s.Log.Warn("couldn't check requests against the library", "err", err)
			return
		}
		owned[p] = m
	}
	for _, r := range reqs {
		p := store.ProviderTMDBMovie
		if r.Kind == "show" {
			p = store.ProviderTMDBTV
		}
		it, ok := owned[p][strconv.Itoa(r.TMDBID)]
		if !ok {
			continue
		}
		if r.Kind == "show" && r.Mode == store.RequestModeSeasons {
			if has, err := s.hasSeasons(ctx, it.ID, r.Seasons); err != nil || !has {
				continue
			}
		}
		_ = s.Store.SetRequestStatus(ctx, r.ID, store.RequestAvailable, "", store.RequestPending, store.RequestApproved)
	}
}

// refreshAvailable checks every open request against the library. Called
// when listing, so a title that arrived since shows as available.
func (s *Server) refreshAvailable(ctx context.Context) {
	reqs, err := s.Store.OpenRequests(ctx)
	if err != nil || len(reqs) == 0 {
		return
	}
	s.markAvailable(ctx, reqs)
	// And close any whose download failed before that closed them.
	seen := map[string]bool{}
	for _, rq := range reqs {
		key := rq.Kind + ":" + strconv.Itoa(rq.TMDBID)
		if !seen[key] {
			seen[key] = true
			s.releaseIfFailed(ctx, rq.Kind, rq.TMDBID)
		}
	}
}

// markTitleAvailable checks one title's open requests, when it's seen in
// the library.
func (s *Server) markTitleAvailable(ctx context.Context, kind string, tmdbID int) {
	reqs, err := s.Store.OpenRequestsFor(ctx, kind, []int{tmdbID})
	if err != nil || len(reqs) == 0 {
		return
	}
	s.markAvailable(ctx, reqs)
}

// --- Markers ---

// requestMarkers finds the open request for each title (of one kind). When
// several people asked, the viewer's own request wins, then the newest.
func (s *Server) requestMarkers(ctx context.Context, viewer store.User, kind string, tmdbIDs []int) map[int]*requestMarker {
	out := map[int]*requestMarker{}
	reqs, err := s.Store.OpenRequestsFor(ctx, kind, tmdbIDs)
	if err != nil {
		s.Log.Warn("couldn't look up requests", "err", err)
		return out
	}
	for _, r := range reqs { // oldest first, so newer ones replace
		mine := r.UserID == viewer.ID
		if prev := out[r.TMDBID]; prev != nil && prev.Mine && !mine {
			continue
		}
		out[r.TMDBID] = &requestMarker{ID: r.ID, Status: r.Status, Mine: mine, Mode: r.Mode, Seasons: r.Seasons}
	}
	return out
}

// markRequests sets the request marker on titles in bulk.
func (s *Server) markRequests(ctx context.Context, viewer store.User, lists ...[]trendingView) {
	ids := map[string][]int{}
	for _, list := range lists {
		for _, v := range list {
			ids[v.Kind] = append(ids[v.Kind], v.TMDBID)
		}
	}
	markers := map[string]map[int]*requestMarker{}
	statuses := map[string]map[int]store.TitleStatus{}
	for kind, vals := range ids {
		markers[kind] = s.requestMarkers(ctx, viewer, kind, vals)
		if st, err := s.Store.StatusesForTitles(ctx, kind, vals, viewer.IsAdmin); err == nil {
			statuses[kind] = st
		}
	}
	for _, list := range lists {
		for i := range list {
			list[i].Request = markers[list[i].Kind][list[i].TMDBID]
			if st, ok := statuses[list[i].Kind][list[i].TMDBID]; ok {
				list[i].OnTheWay = titleStatusOf(st)
			}
		}
	}
}

func titleStatusOf(st store.TitleStatus) *titleStatusView {
	return &titleStatusView{State: st.State, Progress: st.Progress, Message: st.Message, EtaSec: st.EtaSec, UpdatedAt: st.UpdatedAt}
}

// alreadyOnTheWay reports whether the owner's tools are already bringing in
// what a request asks for: a movie with a live status, or a show (every
// season, or its first episode) with one. A failed status doesn't count, and
// neither does one for a show when only some seasons are asked for, since
// the tools may be fetching different ones.
func (s *Server) alreadyOnTheWay(ctx context.Context, kind string, tmdbID int, mode string) bool {
	if kind == "show" && mode == store.RequestModeSeasons {
		return false
	}
	st, err := s.Store.StatusForTitle(ctx, kind, tmdbID, true)
	return err == nil && st.State != stateFailed
}

// --- List and decide ---

func (s *Server) listRequests(w http.ResponseWriter, r *http.Request, u store.User) {
	q := r.URL.Query()
	f := store.RequestFilter{Status: q.Get("status"), Limit: 50}
	switch f.Status {
	case "", store.RequestPending, store.RequestApproved, store.RequestDeclined, store.RequestAvailable:
	default:
		writeErr(w, http.StatusBadRequest, "status is pending, approved, declined or available")
		return
	}
	if n, err := strconv.Atoi(q.Get("limit")); err == nil && n > 0 {
		f.Limit = min(n, 200)
	}
	if n, err := strconv.Atoi(q.Get("offset")); err == nil && n > 0 {
		f.Offset = n
	}
	// Owners see everyone's requests; others only their own.
	if !u.IsAdmin {
		f.UserID = u.ID
	}
	s.refreshAvailable(r.Context())
	reqs, err := s.Store.ListRequests(r.Context(), f)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	pending, err := s.Store.CountRequests(r.Context(), store.RequestFilter{UserID: f.UserID, Status: store.RequestPending})
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	items := make([]requestView, 0, len(reqs))
	for _, rq := range reqs {
		items = append(items, newRequestView(rq))
	}
	writeJSON(w, http.StatusOK, map[string]any{"items": items, "pending": pending})
}

// requestFromPath loads the request named in the URL, answering 404 itself.
func (s *Server) requestFromPath(w http.ResponseWriter, r *http.Request) (store.Request, bool) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		writeErr(w, http.StatusNotFound, "no such request")
		return store.Request{}, false
	}
	req, err := s.Store.GetRequest(r.Context(), id)
	if errors.Is(err, store.ErrNotFound) {
		writeErr(w, http.StatusNotFound, "no such request")
		return req, false
	}
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return req, false
	}
	return req, true
}

// decide moves a request to a new status and answers with it, or explains
// why it can't.
func (s *Server) decide(w http.ResponseWriter, r *http.Request, to, reason string, from ...string) (store.Request, bool) {
	req, ok := s.requestFromPath(w, r)
	if !ok {
		return req, false
	}
	err := s.Store.SetRequestStatus(r.Context(), req.ID, to, reason, from...)
	if errors.Is(err, store.ErrNotFound) {
		writeErr(w, http.StatusConflict, "this request is already "+req.Status)
		return req, false
	}
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return req, false
	}
	req, err = s.Store.GetRequest(r.Context(), req.ID)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return req, false
	}
	writeJSON(w, http.StatusOK, newRequestView(req))
	return req, true
}

func (s *Server) approveRequest(w http.ResponseWriter, r *http.Request, u store.User) {
	if req, ok := s.decide(w, r, store.RequestApproved, "", store.RequestPending); ok {
		s.emitRequest("request.approved", req)
	}
}

func (s *Server) declineRequest(w http.ResponseWriter, r *http.Request, u store.User) {
	var body struct {
		Reason string `json:"reason"`
	}
	// The reason is optional, and so is the body.
	if r.ContentLength != 0 {
		if err := readJSON(r, &body); err != nil {
			writeErr(w, http.StatusBadRequest, err.Error())
			return
		}
	}
	body.Reason = strings.TrimSpace(body.Reason)
	if utf8.RuneCountInString(body.Reason) > noteMax {
		writeErr(w, http.StatusBadRequest, "keep the reason to 280 characters")
		return
	}
	if req, ok := s.decide(w, r, store.RequestDeclined, body.Reason, store.RequestPending, store.RequestApproved); ok {
		s.emitRequest("request.declined", req)
		s.clearUnwantedStatus(r.Context(), req.Kind, req.TMDBID)
	}
}

// releaseFailed closes the open requests for a title whose download failed,
// so they don't sit there as "on its way" and anyone can ask again. They
// stay in the owner's list as declined, with the reason.
func (s *Server) releaseFailed(ctx context.Context, kind string, tmdbID int, message string) {
	open, err := s.Store.OpenRequestsFor(ctx, kind, []int{tmdbID})
	if err != nil {
		return
	}
	reason := "The download failed"
	if message = strings.TrimSpace(message); message != "" {
		reason += ": " + message
	}
	if utf8.RuneCountInString(reason) > noteMax {
		reason = string([]rune(reason)[:noteMax])
	}
	for _, rq := range open {
		if err := s.Store.SetRequestStatus(ctx, rq.ID, store.RequestDeclined, reason, store.RequestPending, store.RequestApproved); err == nil {
			s.Log.Info("closed a request whose download failed", "title", rq.Title, "reason", reason)
		}
	}
}

// releaseIfFailed is releaseFailed for a title whose latest status is a
// failure (one reported before requests were closed this way).
func (s *Server) releaseIfFailed(ctx context.Context, kind string, tmdbID int) {
	if st, err := s.Store.StatusForTitle(ctx, kind, tmdbID, true); err == nil && st.State == stateFailed {
		s.releaseFailed(ctx, kind, tmdbID, st.Message)
	}
}

// clearUnwantedStatus forgets a title's "on the way" status once no open
// request for it is left, so a removed or declined request doesn't leave
// the title looking like it's coming. If the owner's tools are really
// still fetching it, their next update brings the status back.
func (s *Server) clearUnwantedStatus(ctx context.Context, kind string, tmdbID int) {
	if open, err := s.Store.OpenRequestsFor(ctx, kind, []int{tmdbID}); err != nil || len(open) > 0 {
		return
	}
	if n, err := s.Store.DeleteStatusesForTitle(ctx, kind, tmdbID); err != nil {
		s.Log.Warn("couldn't clear a title's status", "kind", kind, "tmdbId", tmdbID, "err", err)
	} else if n > 0 {
		s.Log.Info("cleared the status of a title whose request was removed", "kind", kind, "tmdbId", tmdbID)
	}
}

// deleteRequest lets people cancel their own pending requests, and owners
// remove any.
func (s *Server) deleteRequest(w http.ResponseWriter, r *http.Request, u store.User) {
	req, ok := s.requestFromPath(w, r)
	if !ok {
		return
	}
	if !u.IsAdmin {
		if req.UserID != u.ID {
			writeErr(w, http.StatusNotFound, "no such request")
			return
		}
		if req.Status != store.RequestPending {
			writeErr(w, http.StatusConflict, "only a pending request can be cancelled; this one is "+req.Status)
			return
		}
	}
	if err := s.Store.DeleteRequest(r.Context(), req.ID); err != nil && !errors.Is(err, store.ErrNotFound) {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	s.clearUnwantedStatus(r.Context(), req.Kind, req.TMDBID)
	w.WriteHeader(http.StatusNoContent)
}
