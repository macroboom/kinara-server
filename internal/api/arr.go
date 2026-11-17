package api

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"kinara/internal/arr"
	"kinara/internal/store"
)

// Radarr and Sonarr, built in: an approved request (by an owner, or from a
// trusted device) goes straight to Radarr (movies) or Sonarr (shows), which
// add it, or monitor it when they have it already, and search for it. Their
// download queues are read every little while, so the apps show the title
// coming with its progress, and once a download is imported the library is
// scanned and the request becomes available.
//
// The webhooks still fire too, for anything else the owner runs.

const (
	// arrEvery is how often the queues are read.
	arrEvery = 20 * time.Second
	// arrSourceRadarr and arrSourceSonarr name the statuses they report.
	arrSourceRadarr = "arr:radarr"
	arrSourceSonarr = "arr:sonarr"
)

// arrState is what the queue reader remembers between reads.
type arrState struct {
	mu sync.Mutex
	// seen are the downloads reported last time, by status ident.
	seen map[string]arrSeen
	// lastErr and lastRead are per service, for the settings page.
	lastErr  map[string]string
	lastRead map[string]time.Time
	// health is each service's own warnings; indexers Prowlarr's count
	// (-1 unknown); qbitVersion qBittorrent's, when it answered.
	health      map[string][]arr.HealthItem
	indexers    int
	qbitVersion string
	kick        chan struct{}
	// explained is when each waiting title was last looked at (see
	// arr_explain.go).
	explained map[string]time.Time
}

// arrSeen is a download reported last time: what to ask when it's gone.
type arrSeen struct {
	kind      string // radarr or sonarr
	movieID   int
	episodeID int
	upd       statusUpdate
}

func (s *Server) arrRoutes(mux *http.ServeMux) {
	mux.Handle("GET /api/v1/arr", s.admin(s.getArr))
	mux.Handle("PUT /api/v1/arr", s.admin(s.putArr))
	mux.Handle("POST /api/v1/arr/{kind}/test", s.admin(s.testArr))
	mux.Handle("POST /api/v1/requests/{id}/send", s.admin(s.resendRequest))
	s.stackRoutes(mux)
	s.detectRoutes(mux)
}

func (s *Server) arrSettings(ctx context.Context) store.ArrSettings {
	a, err := s.Store.GetArrSettings(ctx)
	if err != nil {
		return store.DefaultArrSettings
	}
	return a
}

func arrClient(kind string, a store.ArrServer) *arr.Client {
	return &arr.Client{Kind: kind, BaseURL: a.URL, APIKey: a.APIKey}
}

// --- Settings ---------------------------------------------------------------------

// arrServerView is a connection as the settings page shows it: the API key
// only as its last four characters.
type arrServerView struct {
	store.ArrServer
	APIKey    string `json:"apiKey"`
	HasAPIKey bool   `json:"hasApiKey"`
	// Ready: requests are sent to it.
	Ready bool `json:"ready"`
	// LastError is from the last queue read ("" when it worked).
	LastError string `json:"lastError,omitempty"`
	LastRead  int64  `json:"lastRead,omitempty"`
	// Health is the service's own warnings.
	Health []arr.HealthItem `json:"health"`
}

type prowlarrView struct {
	URL       string `json:"url"`
	APIKey    string `json:"apiKey"`
	HasAPIKey bool   `json:"hasApiKey"`
	// Indexers is how many are turned on (-1: unknown).
	Indexers  int    `json:"indexers"`
	LastError string `json:"lastError,omitempty"`
}

type qbitView struct {
	URL         string `json:"url"`
	Username    string `json:"username"`
	HasPassword bool   `json:"hasPassword"`
	Version     string `json:"version,omitempty"`
	LastError   string `json:"lastError,omitempty"`
}

type arrView struct {
	Radarr   arrServerView `json:"radarr"`
	Sonarr   arrServerView `json:"sonarr"`
	Prowlarr prowlarrView  `json:"prowlarr"`
	Qbit     qbitView      `json:"qbit"`
	// Stack: Kinara made a setup script (its folders are in GET /arr/stack).
	Stack bool `json:"stack"`
}

func keyEnd(k string) string {
	if len(k) > 4 {
		return "…" + k[len(k)-4:]
	}
	return ""
}

func (s *Server) arrView(ctx context.Context) arrView {
	a := s.arrSettings(ctx)
	view := func(kind string, srv store.ArrServer) arrServerView {
		v := arrServerView{ArrServer: srv, HasAPIKey: srv.APIKey != "", Ready: srv.Ready(), APIKey: keyEnd(srv.APIKey)}
		if v.Tags == nil {
			v.Tags = []int{}
		}
		s.arr.mu.Lock()
		v.LastError = s.arr.lastErr[kind]
		if t := s.arr.lastRead[kind]; !t.IsZero() {
			v.LastRead = t.Unix()
		}
		v.Health = s.arr.health[kind]
		s.arr.mu.Unlock()
		if v.Health == nil {
			v.Health = []arr.HealthItem{}
		}
		return v
	}
	out := arrView{Radarr: view(arr.Radarr, a.Radarr), Sonarr: view(arr.Sonarr, a.Sonarr), Stack: a.Stack != nil,
		Prowlarr: prowlarrView{URL: a.Prowlarr.URL, APIKey: keyEnd(a.Prowlarr.APIKey), HasAPIKey: a.Prowlarr.APIKey != "", Indexers: -1},
		Qbit:     qbitView{URL: a.Qbit.URL, Username: a.Qbit.Username, HasPassword: a.Qbit.Password != ""}}
	s.arr.mu.Lock()
	if a.Prowlarr.URL != "" {
		out.Prowlarr.LastError = s.arr.lastErr[arr.Prowlarr]
		if out.Prowlarr.LastError == "" && s.arr.lastRead[arr.Prowlarr].After(time.Time{}) {
			out.Prowlarr.Indexers = s.arr.indexers
		}
	}
	if a.Qbit.URL != "" {
		out.Qbit.LastError, out.Qbit.Version = s.arr.lastErr["qbit"], s.arr.qbitVersion
	}
	s.arr.mu.Unlock()
	return out
}

func (s *Server) getArr(w http.ResponseWriter, r *http.Request, u store.User) {
	writeJSON(w, http.StatusOK, s.arrView(r.Context()))
}

// arrInput is a change to one connection; fields left out keep their values.
type arrInput struct {
	Enabled             *bool   `json:"enabled"`
	URL                 *string `json:"url"`
	APIKey              *string `json:"apiKey"`
	ProfileID           *int    `json:"profileId"`
	RootFolder          *string `json:"rootFolder"`
	Tags                *[]int  `json:"tags"`
	Search              *bool   `json:"search"`
	MinimumAvailability *string `json:"minimumAvailability"`
	SeasonFolder        *bool   `json:"seasonFolder"`
	SeriesType          *string `json:"seriesType"`
}

func (in *arrInput) apply(a *store.ArrServer) error {
	if in == nil {
		return nil
	}
	if in.Enabled != nil {
		a.Enabled = *in.Enabled
	}
	if in.URL != nil {
		a.URL = cleanURL(*in.URL)
	}
	// The page shows only the key's end: sending that back keeps the key.
	if in.APIKey != nil && !strings.HasPrefix(*in.APIKey, "…") {
		a.APIKey = strings.TrimSpace(*in.APIKey)
	}
	if in.ProfileID != nil {
		a.ProfileID = *in.ProfileID
	}
	if in.RootFolder != nil {
		a.RootFolder = strings.TrimSpace(*in.RootFolder)
	}
	if in.Tags != nil {
		a.Tags = *in.Tags
	}
	if in.Search != nil {
		a.Search = *in.Search
	}
	if in.MinimumAvailability != nil {
		switch *in.MinimumAvailability {
		case "announced", "inCinemas", "released":
			a.MinimumAvailability = *in.MinimumAvailability
		default:
			return errors.New("minimum availability must be announced, inCinemas or released")
		}
	}
	if in.SeasonFolder != nil {
		a.SeasonFolder = *in.SeasonFolder
	}
	if in.SeriesType != nil {
		switch *in.SeriesType {
		case "standard", "daily", "anime":
			a.SeriesType = *in.SeriesType
		default:
			return errors.New("series type must be standard, daily or anime")
		}
	}
	return nil
}

func (s *Server) putArr(w http.ResponseWriter, r *http.Request, u store.User) {
	var in struct {
		Radarr   *arrInput `json:"radarr"`
		Sonarr   *arrInput `json:"sonarr"`
		Prowlarr *struct {
			URL    *string `json:"url"`
			APIKey *string `json:"apiKey"`
		} `json:"prowlarr"`
		Qbit *struct {
			URL      *string `json:"url"`
			Username *string `json:"username"`
			Password *string `json:"password"`
		} `json:"qbit"`
	}
	if err := readJSON(r, &in); err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	a := s.arrSettings(r.Context())
	if err := in.Radarr.apply(&a.Radarr); err != nil {
		writeErr(w, http.StatusBadRequest, "Radarr: "+err.Error())
		return
	}
	if err := in.Sonarr.apply(&a.Sonarr); err != nil {
		writeErr(w, http.StatusBadRequest, "Sonarr: "+err.Error())
		return
	}
	if p := in.Prowlarr; p != nil {
		if p.URL != nil {
			a.Prowlarr.URL = cleanURL(*p.URL)
		}
		if p.APIKey != nil && !strings.HasPrefix(*p.APIKey, "…") {
			a.Prowlarr.APIKey = strings.TrimSpace(*p.APIKey)
		}
	}
	if q := in.Qbit; q != nil {
		if q.URL != nil {
			a.Qbit.URL = cleanURL(*q.URL)
		}
		if q.Username != nil {
			a.Qbit.Username = strings.TrimSpace(*q.Username)
		}
		// An empty password keeps the saved one (the page never shows it).
		if q.Password != nil && *q.Password != "" {
			a.Qbit.Password = *q.Password
		}
	}
	// Every save of qBittorrent's card is passed on to Radarr and Sonarr,
	// changed or not: they may still point at an old one (saving the same
	// details again is how the owner fixes that).
	qbitSaved := in.Qbit != nil
	// A qBittorrent of the owner's own, saved here, is the one from now on:
	// the Docker setup stops installing (and switching back to) its own.
	if qbitSaved && a.Stack != nil {
		a.Stack.OwnQbit = a.Qbit.URL != "" && !isStackQbit(a.Qbit)
	}
	if err := s.Store.PutArrSettings(r.Context(), a); err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	s.Log.Info("Radarr and Sonarr settings saved", "by", u.Username, "radarr", a.Radarr.Ready(), "sonarr", a.Sonarr.Ready())
	// Radarr and Sonarr send their downloads to the qBittorrent saved here,
	// so a new one is passed on to them now.
	var linked steps
	if qbitSaved && a.Qbit.URL != "" && (a.Radarr.URL != "" || a.Sonarr.URL != "") {
		ctx, cancel := context.WithTimeout(r.Context(), 45*time.Second)
		linked = s.linkApps(ctx, linkInput{})
		cancel()
	}
	s.arrKick()
	view := s.arrView(r.Context())
	if linked == nil {
		writeJSON(w, http.StatusOK, view)
		return
	}
	writeJSON(w, http.StatusOK, struct {
		arrView
		Linked steps `json:"linked"`
	}{view, linked})
}

// isStackQbit: the connection is to the Docker setup's own qBittorrent.
func isStackQbit(q store.QbitLink) bool {
	return strings.HasSuffix(q.URL, ":"+strconv.Itoa(arr.PortQbit)) && q.Username == "kinara"
}

// cleanURL tidies an address: no trailing slash, http:// when none given.
func cleanURL(v string) string {
	v = strings.TrimRight(strings.TrimSpace(v), "/")
	if v != "" && !strings.HasPrefix(v, "http://") && !strings.HasPrefix(v, "https://") {
		v = "http://" + v
	}
	return v
}

// testArr connects with the address and key given (the saved key when none
// is) and answers with the choices to pick from. Prowlarr answers with its
// version and indexers, qBittorrent with its version.
func (s *Server) testArr(w http.ResponseWriter, r *http.Request, u store.User) {
	kind := r.PathValue("kind")
	if kind == arr.Prowlarr || kind == "qbit" {
		s.testOther(w, r, kind)
		return
	}
	if kind != arr.Radarr && kind != arr.Sonarr {
		writeErr(w, http.StatusNotFound, "radarr, sonarr, prowlarr or qbit")
		return
	}
	var in struct {
		URL    string `json:"url"`
		APIKey string `json:"apiKey"`
	}
	if r.ContentLength != 0 {
		if err := readJSON(r, &in); err != nil {
			writeErr(w, http.StatusBadRequest, err.Error())
			return
		}
	}
	a := s.arrSettings(r.Context())
	saved := a.Radarr
	if kind == arr.Sonarr {
		saved = a.Sonarr
	}
	if in.URL == "" {
		in.URL = saved.URL
	}
	if in.APIKey == "" || strings.HasPrefix(in.APIKey, "…") {
		in.APIKey = saved.APIKey
	}
	ctx, cancel := context.WithTimeout(r.Context(), 20*time.Second)
	defer cancel()
	opts, err := (&arr.Client{Kind: kind, BaseURL: in.URL, APIKey: in.APIKey}).Check(ctx)
	if err != nil {
		writeErr(w, http.StatusBadGateway, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, opts)
}

// testOther tests Prowlarr or qBittorrent with what's given (saved values
// for anything left out).
func (s *Server) testOther(w http.ResponseWriter, r *http.Request, kind string) {
	var in struct {
		URL      string `json:"url"`
		APIKey   string `json:"apiKey"`
		Username string `json:"username"`
		Password string `json:"password"`
	}
	if r.ContentLength != 0 {
		if err := readJSON(r, &in); err != nil {
			writeErr(w, http.StatusBadRequest, err.Error())
			return
		}
	}
	a := s.arrSettings(r.Context())
	ctx, cancel := context.WithTimeout(r.Context(), 20*time.Second)
	defer cancel()
	if kind == arr.Prowlarr {
		if in.URL == "" {
			in.URL = a.Prowlarr.URL
		}
		if in.APIKey == "" || strings.HasPrefix(in.APIKey, "…") {
			in.APIKey = a.Prowlarr.APIKey
		}
		st, indexers, err := (&arr.Client{Kind: arr.Prowlarr, BaseURL: in.URL, APIKey: in.APIKey}).ProwlarrCheck(ctx)
		if err != nil {
			writeErr(w, http.StatusBadGateway, err.Error())
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"status": st, "indexers": indexers})
		return
	}
	if in.URL == "" {
		in.URL = a.Qbit.URL
	}
	if in.Username == "" {
		in.Username = a.Qbit.Username
	}
	if in.Password == "" {
		in.Password = a.Qbit.Password
	}
	v, err := (&arr.Qbit{BaseURL: in.URL, Username: in.Username, Password: in.Password}).Version(ctx)
	if err != nil {
		writeErr(w, http.StatusBadGateway, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"version": v})
}

// --- Sending requests -------------------------------------------------------------

// arrFor is the service a request goes to, if one is ready.
func (s *Server) arrFor(ctx context.Context, kind string) (string, store.ArrServer, bool) {
	a := s.arrSettings(ctx)
	if kind == "show" {
		return arr.Sonarr, a.Sonarr, a.Sonarr.Ready()
	}
	return arr.Radarr, a.Radarr, a.Radarr.Ready()
}

// arrSendLater sends an approved request in the background.
func (s *Server) arrSendLater(req store.Request) {
	if _, _, ok := s.arrFor(s.baseCtx(), req.Kind); !ok {
		return
	}
	go func() {
		ctx, cancel := context.WithTimeout(s.baseCtx(), 2*time.Minute)
		defer cancel()
		s.arrSend(ctx, req)
	}()
}

// arrSend sends a request to Radarr or Sonarr and records what came of it.
func (s *Server) arrSend(ctx context.Context, req store.Request) (state, note string) {
	kind, srv, ok := s.arrFor(ctx, req.Kind)
	if !ok {
		return "", ""
	}
	name := "Radarr"
	if kind == arr.Sonarr {
		name = "Sonarr"
	}
	c := arrClient(kind, srv)
	o := arr.AddOptions{ProfileID: srv.ProfileID, RootFolder: srv.RootFolder, Tags: srv.Tags, Search: srv.Search,
		MinimumAvailability: srv.MinimumAvailability, SeasonFolder: srv.SeasonFolder, SeriesType: srv.SeriesType}
	var res arr.Result
	var err error
	if kind == arr.Radarr {
		res, err = c.AddMovie(ctx, req.TMDBID, o)
	} else {
		tvdb, term := req.TVDBID, ""
		if tvdb == 0 {
			// TMDB didn't know its TVDB ID when it was asked for: try again,
			// then fall back to the IMDb ID or the name.
			ids := s.titleIDs(ctx, "show", req.TMDBID)
			tvdb = ids.TVDBID
			switch {
			case tvdb > 0:
			case ids.IMDBID != "":
				term = "imdb:" + ids.IMDBID
			default:
				term = req.Title
			}
		}
		res, err = c.AddSeries(ctx, tvdb, term, req.Mode, req.Seasons, o)
	}
	switch {
	case err != nil:
		state, note = "error", name+": "+err.Error()
		s.Log.Warn("couldn't send a request", "to", name, "title", req.Title, "err", err)
	case res.HasFile:
		state, note = "sent", name+" has it already"
	case res.Added && srv.Search:
		state, note = "sent", "Added to "+name+", searching"
	case res.Added:
		state, note = "sent", "Added to "+name
	case srv.Search:
		state, note = "sent", "In "+name+", searching"
	default:
		state, note = "sent", "Monitored in "+name
	}
	if len(note) > 280 {
		note = note[:280]
	}
	if err == nil {
		s.Log.Info("request sent", "to", name, "title", req.Title, "note", note)
		if res.HasFile {
			// Nothing to fetch: look for it in the library now.
			s.scanFor(ctx, req.Kind)
		} else if srv.Search {
			s.arrQueued(ctx, kind, req)
		}
	}
	_ = s.Store.SetRequestArr(ctx, req.ID, state, note)
	if err == nil {
		s.arrKick()
	}
	return state, note
}

// arrQueued shows a title as on its way (searching) until its download
// shows up in the queue.
func (s *Server) arrQueued(ctx context.Context, kind string, req store.Request) {
	src := statusSource{key: arrSourceRadarr, name: "Radarr"}
	if kind == arr.Sonarr {
		src = statusSource{key: arrSourceSonarr, name: "Sonarr"}
	}
	upd := statusUpdate{Kind: req.Kind, TMDBID: req.TMDBID, Title: req.Title, Year: req.Year, Season: -1,
		State: stateQueued, Message: "Looking for a release"}
	r, _ := http.NewRequestWithContext(ctx, http.MethodGet, "/", nil)
	if _, _, err := s.applyStatus(r, src, upd); err != nil {
		s.Log.Info("couldn't show a request as on its way", "title", req.Title, "err", err)
	}
}

// scanFor scans the libraries of a kind (a title the service already had).
func (s *Server) scanFor(ctx context.Context, kind string) {
	if s.Scanner == nil {
		return
	}
	libs, err := s.Store.ListLibraries(ctx)
	if err != nil {
		return
	}
	for _, l := range libs {
		if (kind == "movie" && l.Kind == store.KindMovies) || (kind == "show" && l.Kind == store.KindTV) {
			_ = s.Scanner.Queue(ctx, l.ID)
		}
	}
}

// resendRequest sends an approved request again (after fixing a setting).
func (s *Server) resendRequest(w http.ResponseWriter, r *http.Request, u store.User) {
	req, ok := s.requestFromPath(w, r)
	if !ok {
		return
	}
	if req.Status != store.RequestApproved {
		writeErr(w, http.StatusConflict, "only approved requests are sent")
		return
	}
	if _, _, ready := s.arrFor(r.Context(), req.Kind); !ready {
		svc := "Radarr"
		if req.Kind == "show" {
			svc = "Sonarr"
		}
		writeErr(w, http.StatusConflict, svc+" isn't set up (Manage, Requests)")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), time.Minute)
	defer cancel()
	s.arrSend(ctx, req)
	req, err := s.Store.GetRequest(r.Context(), req.ID)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, newRequestView(req))
}

// --- Reading the queues -----------------------------------------------------------

func (s *Server) arrKick() {
	s.arr.mu.Lock()
	if s.arr.kick == nil {
		s.arr.kick = make(chan struct{}, 1)
	}
	k := s.arr.kick
	s.arr.mu.Unlock()
	select {
	case k <- struct{}{}:
	default:
	}
}

// RunArr reads Radarr's and Sonarr's queues until ctx ends.
func (s *Server) RunArr(ctx context.Context) {
	s.arrKick()
	s.arr.mu.Lock()
	kick := s.arr.kick
	s.arr.mu.Unlock()
	t := time.NewTicker(arrEvery)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		case <-kick:
		}
		s.arrReadQueues(ctx)
	}
}

// arrReadQueues reports every download as a title on its way, and a
// download that's gone as done (imported: scan) or forgets it (removed).
func (s *Server) arrReadQueues(ctx context.Context) {
	a := s.arrSettings(ctx)
	now := map[string]arrSeen{}
	read := map[string]bool{}
	for _, svc := range []struct {
		kind string
		srv  store.ArrServer
	}{{arr.Radarr, a.Radarr}, {arr.Sonarr, a.Sonarr}} {
		if !svc.srv.Enabled || svc.srv.URL == "" || svc.srv.APIKey == "" {
			continue
		}
		rctx, cancel := context.WithTimeout(ctx, 20*time.Second)
		items, err := arrClient(svc.kind, svc.srv).Queue(rctx)
		cancel()
		s.arr.mu.Lock()
		if s.arr.lastErr == nil {
			s.arr.lastErr, s.arr.lastRead = map[string]string{}, map[string]time.Time{}
		}
		if err != nil {
			s.arr.lastErr[svc.kind] = err.Error()
		} else {
			delete(s.arr.lastErr, svc.kind)
			s.arr.lastRead[svc.kind] = time.Now()
		}
		s.arr.mu.Unlock()
		if err != nil {
			s.Log.Debug("couldn't read the queue", "service", svc.kind, "err", err)
			continue
		}
		read[svc.kind] = true
		for ident, seen := range arrDownloads(svc.kind, items) {
			now[ident] = seen
		}
		hctx, hcancel := context.WithTimeout(ctx, 10*time.Second)
		health, herr := arrClient(svc.kind, svc.srv).Health(hctx)
		hcancel()
		if herr == nil {
			s.arr.mu.Lock()
			if s.arr.health == nil {
				s.arr.health = map[string][]arr.HealthItem{}
			}
			s.arr.health[svc.kind] = health
			s.arr.mu.Unlock()
		}
	}
	s.readHelpers(ctx, a)
	// Titles with a download in the queue need no explaining.
	queued := map[string]bool{}
	for _, seen := range now {
		queued[seen.upd.Kind+strconv.Itoa(seen.upd.TMDBID)] = true
	}
	defer s.arrExplain(ctx, a, queued)

	for ident, seen := range now {
		src := arrSource(seen.kind)
		r, _ := http.NewRequestWithContext(ctx, http.MethodGet, "/", nil)
		if _, _, err := s.applyStatus(r, src, seen.upd); err == nil && seen.upd.TMDBID > 0 {
			// Its "looking for a release" status gives way to the download.
			if ident != statusIdent(searchingUpdate(seen.upd)) {
				_ = s.Store.DeleteStatusFor(ctx, src.key, statusIdent(searchingUpdate(seen.upd)))
			}
		}
	}

	s.arr.mu.Lock()
	before := s.arr.seen
	s.arr.seen = now
	s.arr.mu.Unlock()
	for ident, seen := range before {
		if _, still := now[ident]; still || !read[seen.kind] {
			continue
		}
		s.arrFinished(ctx, ident, seen, a)
	}
}

// arrFinished handles a download that left the queue: imported (done, which
// scans the library) or removed (forgotten).
func (s *Server) arrFinished(ctx context.Context, ident string, seen arrSeen, a store.ArrSettings) {
	src := arrSource(seen.kind)
	var has bool
	var err error
	if seen.kind == arr.Radarr {
		has, err = arrClient(arr.Radarr, a.Radarr).MovieHasFile(ctx, seen.movieID)
	} else {
		has, err = arrClient(arr.Sonarr, a.Sonarr).EpisodeHasFile(ctx, seen.episodeID)
	}
	if err == nil && has {
		upd := seen.upd
		upd.State, upd.Progress, upd.Message, upd.EtaSec = stateDone, nil, "", 0
		r, _ := http.NewRequestWithContext(ctx, http.MethodGet, "/", nil)
		if _, _, err := s.applyStatus(r, src, upd); err != nil {
			s.Log.Info("couldn't mark a download done", "title", upd.Title, "err", err)
		}
		return
	}
	_ = s.Store.DeleteStatusFor(ctx, src.key, ident)
}

func arrSource(kind string) statusSource {
	if kind == arr.Sonarr {
		return statusSource{key: arrSourceSonarr, name: "Sonarr"}
	}
	return statusSource{key: arrSourceRadarr, name: "Radarr"}
}

// searchingUpdate is the "looking for a release" status a download replaces.
func searchingUpdate(u statusUpdate) statusUpdate {
	return statusUpdate{Kind: u.Kind, TMDBID: u.TMDBID, Season: -1}
}

// arrDownloads turns a queue into one status per download (a season pack is
// one download for many episodes: it's shown as the season).
func arrDownloads(kind string, items []arr.QueueItem) map[string]arrSeen {
	type agg struct {
		seen     arrSeen
		size     float64
		left     float64
		episodes map[int]bool
	}
	by := map[string]*agg{}
	var order []string
	for _, q := range items {
		key := q.DownloadID
		if key == "" {
			key = strconv.Itoa(q.ID)
		}
		g := by[key]
		if g == nil {
			upd := statusUpdate{Season: -1, State: stateInProgress}
			seen := arrSeen{kind: kind, movieID: q.MovieID, episodeID: q.EpisodeID}
			switch {
			case q.Movie != nil:
				upd.Kind, upd.TMDBID, upd.Title, upd.Year = "movie", q.Movie.TMDBID, q.Movie.Title, q.Movie.Year
			case q.Series != nil:
				upd.Kind, upd.TMDBID, upd.TVDBID, upd.IMDBID = "show", q.Series.TMDBID, q.Series.TVDBID, q.Series.IMDBID
				upd.Title, upd.Year = q.Series.Title, q.Series.Year
				if q.Episode != nil {
					upd.Season, upd.Episode = q.Episode.SeasonNumber, q.Episode.EpisodeNumber
				}
			default:
				continue // not matched to a title in the service
			}
			upd.State, upd.Message = queueState(q)
			seen.upd = upd
			g = &agg{seen: seen, episodes: map[int]bool{}}
			by[key] = g
			order = append(order, key)
		}
		if q.Episode != nil {
			g.episodes[q.Episode.EpisodeNumber] = true
		}
		// Every episode of a pack lists the whole download's size.
		if g.size == 0 {
			g.size, g.left = q.Size, q.SizeLeft
			if eta := q.EtaSeconds(); eta > 0 {
				g.seen.upd.EtaSec = eta
			}
		}
	}
	out := map[string]arrSeen{}
	for _, key := range order {
		g := by[key]
		upd := g.seen.upd
		if len(g.episodes) > 1 {
			upd.Episode = 0 // the season
		}
		if g.size > 0 {
			p := min(max(1-g.left/g.size, 0), 1)
			upd.Progress = &p
		}
		// The service's own download reference keeps each one apart.
		upd.Ref = fmt.Sprintf("%s:%s", kind, key)
		g.seen.upd = upd
		out[statusIdent(upd)] = g.seen
	}
	return out
}

// queueState is Kinara's state for a queue item, and a word on it.
func queueState(q arr.QueueItem) (string, string) {
	msg := ""
	for _, m := range q.StatusMessages {
		for _, t := range m.Messages {
			if t != "" {
				msg = t
				break
			}
		}
		if msg != "" {
			break
		}
	}
	if q.ErrorMessage != "" {
		msg = q.ErrorMessage
	}
	switch strings.ToLower(q.TrackedDownloadState) {
	case "importpending", "importing":
		return stateInProgress, "Importing"
	case "failedpending", "failed":
		// The service tries another release by itself; still on its way.
		return stateQueued, firstNonEmpty(msg, "The download failed; trying another")
	}
	switch strings.ToLower(q.Status) {
	case "queued", "delay", "paused", "downloadclientunavailable":
		return stateQueued, firstNonEmpty(msg, "Waiting to download")
	case "warning":
		return stateInProgress, msg
	}
	return stateInProgress, firstNonEmpty(msg, "Downloading")
}

// readHelpers checks Prowlarr (its indexers) and qBittorrent (it signs in),
// for the settings page.
func (s *Server) readHelpers(ctx context.Context, a store.ArrSettings) {
	note := func(key string, err error) {
		s.arr.mu.Lock()
		defer s.arr.mu.Unlock()
		if s.arr.lastErr == nil {
			s.arr.lastErr, s.arr.lastRead = map[string]string{}, map[string]time.Time{}
		}
		if err != nil {
			s.arr.lastErr[key] = err.Error()
		} else {
			delete(s.arr.lastErr, key)
			s.arr.lastRead[key] = time.Now()
		}
	}
	if a.Prowlarr.URL != "" && a.Prowlarr.APIKey != "" {
		pctx, cancel := context.WithTimeout(ctx, 10*time.Second)
		_, enabled, err := (&arr.Client{Kind: arr.Prowlarr, BaseURL: a.Prowlarr.URL, APIKey: a.Prowlarr.APIKey}).Indexers(pctx)
		cancel()
		note(arr.Prowlarr, err)
		if err == nil {
			s.arr.mu.Lock()
			s.arr.indexers = enabled
			s.arr.mu.Unlock()
		}
	}
	if a.Qbit.URL != "" {
		qctx, cancel := context.WithTimeout(ctx, 10*time.Second)
		v, err := (&arr.Qbit{BaseURL: a.Qbit.URL, Username: a.Qbit.Username, Password: a.Qbit.Password}).Version(qctx)
		cancel()
		note("qbit", err)
		s.arr.mu.Lock()
		s.arr.qbitVersion = v
		s.arr.mu.Unlock()
	}
}
