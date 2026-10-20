package api

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"kinara/internal/store"
)

// Incoming hooks: secret addresses the owner's tools post title statuses
// to, in the Kinara format or their own JSON mapped field by field.

// Kinara's states for a title on its way.
const (
	stateQueued     = "queued"
	stateInProgress = "in_progress"
	stateDone       = "done"
	stateFailed     = "failed"
)

func validState(s string) bool {
	return s == stateQueued || s == stateInProgress || s == stateDone || s == stateFailed
}

// mappingFields are the values a custom hook can map.
var mappingFields = map[string]bool{"tmdbId": true, "tvdbId": true, "imdbId": true, "title": true, "year": true,
	"kind": true, "season": true, "episode": true, "state": true, "progress": true, "message": true, "eta": true}

type incomingView struct {
	ID       int64                `json:"id"`
	Name     string               `json:"name"`
	Token    string               `json:"token"`
	Enabled  bool                 `json:"enabled"`
	Format   string               `json:"format"`
	Mapping  map[string]string    `json:"mapping"`
	StateMap []store.StateMapping `json:"stateMap"`
	// ProgressScale is auto, fraction (0 to 1) or percent (0 to 100).
	ProgressScale  string `json:"progressScale"`
	OnDone         string `json:"onDone"`
	ExpireHours    int    `json:"expireHours"`
	Visibility     string `json:"visibility"`
	LastReceivedAt int64  `json:"lastReceivedAt"`
	// Path is where tools post to (after the server's address).
	Path      string `json:"path"`
	CreatedAt int64  `json:"createdAt"`
}

func incomingViewOf(h store.IncomingHook) incomingView {
	return incomingView{ID: h.ID, Name: h.Name, Token: h.Token, Enabled: h.Enabled, Format: h.Format, Mapping: h.Mapping,
		StateMap: h.StateMap, ProgressScale: h.ProgressScale, OnDone: h.OnDone, ExpireHours: h.ExpireHours,
		Visibility: h.Visibility, LastReceivedAt: h.LastReceivedAt, Path: "/api/v1/hooks/in/" + h.Token, CreatedAt: h.CreatedAt}
}

type incomingInput struct {
	Name          *string               `json:"name"`
	Enabled       *bool                 `json:"enabled"`
	Format        *string               `json:"format"`
	Mapping       map[string]string     `json:"mapping"`
	StateMap      *[]store.StateMapping `json:"stateMap"`
	ProgressScale *string               `json:"progressScale"`
	OnDone        *string               `json:"onDone"`
	ExpireHours   *int                  `json:"expireHours"`
	Visibility    *string               `json:"visibility"`
}

func (in incomingInput) apply(h *store.IncomingHook) error {
	if in.Name != nil {
		h.Name = strings.TrimSpace(*in.Name)
	}
	if in.Enabled != nil {
		h.Enabled = *in.Enabled
	}
	if in.Format != nil {
		h.Format = *in.Format
	}
	if in.Mapping != nil {
		h.Mapping = map[string]string{}
		for k, v := range in.Mapping {
			if v = strings.TrimSpace(v); v != "" {
				h.Mapping[k] = v
			}
		}
	}
	if in.StateMap != nil {
		h.StateMap = *in.StateMap
	}
	if in.ProgressScale != nil {
		h.ProgressScale = *in.ProgressScale
	}
	if in.OnDone != nil {
		h.OnDone = *in.OnDone
	}
	if in.ExpireHours != nil {
		h.ExpireHours = *in.ExpireHours
	}
	if in.Visibility != nil {
		h.Visibility = *in.Visibility
	}
	return checkIncoming(h)
}

func checkIncoming(h *store.IncomingHook) error {
	if h.Name == "" || len(h.Name) > 80 {
		return errors.New("give the hook a name (up to 80 characters)")
	}
	if h.Format != "kinara" && h.Format != "custom" {
		return errors.New(`format is "kinara" or "custom"`)
	}
	for k, v := range h.Mapping {
		if !mappingFields[k] {
			return fmt.Errorf("%q isn't a field Kinara reads", k)
		}
		if len(v) > 200 {
			return fmt.Errorf("the path for %s is too long", k)
		}
	}
	if h.Format == "custom" && h.Mapping["state"] == "" && h.Mapping["progress"] == "" {
		return errors.New("map the state or the progress, so Kinara knows where the title is")
	}
	stateMap := []store.StateMapping{}
	for _, m := range h.StateMap {
		m.From = strings.TrimSpace(m.From)
		if m.From == "" {
			continue
		}
		if !validState(m.To) {
			return fmt.Errorf("%q isn't one of Kinara's states (queued, in_progress, done, failed)", m.To)
		}
		stateMap = append(stateMap, m)
	}
	h.StateMap = stateMap
	switch h.ProgressScale {
	case "auto", "fraction", "percent":
	default:
		return errors.New(`progressScale is "auto", "fraction" or "percent"`)
	}
	if h.OnDone != "scan" && h.OnDone != "nothing" {
		return errors.New(`onDone is "scan" or "nothing"`)
	}
	if h.ExpireHours < 1 || h.ExpireHours > 168 {
		return errors.New("expireHours is 1 to 168")
	}
	if h.Visibility != "owners" && h.Visibility != "everyone" {
		return errors.New(`visibility is "owners" or "everyone"`)
	}
	return nil
}

func newHookToken() string { return randomToken("in_", 24) }

func (s *Server) listIncoming(w http.ResponseWriter, r *http.Request, u store.User) {
	hooks, err := s.Store.ListIncoming(r.Context())
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	out := make([]incomingView, 0, len(hooks))
	for _, h := range hooks {
		out = append(out, incomingViewOf(h))
	}
	writeJSON(w, http.StatusOK, map[string]any{"items": out})
}

func (s *Server) createIncoming(w http.ResponseWriter, r *http.Request, u store.User) {
	var in incomingInput
	if err := readJSONLoose(r, &in); err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	h := store.IncomingHook{Token: newHookToken(), Enabled: true, Format: "kinara", Mapping: map[string]string{},
		StateMap: []store.StateMapping{}, ProgressScale: "auto", OnDone: "scan", ExpireHours: 6, Visibility: "owners"}
	if err := in.apply(&h); err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	if err := s.Store.CreateIncoming(r.Context(), &h); err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusCreated, incomingViewOf(h))
}

func (s *Server) incomingFrom(w http.ResponseWriter, r *http.Request) (store.IncomingHook, bool) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		writeErr(w, http.StatusNotFound, "no such incoming hook")
		return store.IncomingHook{}, false
	}
	h, err := s.Store.GetIncoming(r.Context(), id)
	if err != nil {
		writeErr(w, http.StatusNotFound, "no such incoming hook")
		return h, false
	}
	return h, true
}

func (s *Server) updateIncoming(w http.ResponseWriter, r *http.Request, u store.User) {
	h, ok := s.incomingFrom(w, r)
	if !ok {
		return
	}
	var in incomingInput
	if err := readJSONLoose(r, &in); err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	if err := in.apply(&h); err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	if err := s.Store.UpdateIncoming(r.Context(), h); err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, incomingViewOf(h))
}

func (s *Server) deleteIncoming(w http.ResponseWriter, r *http.Request, u store.User) {
	h, ok := s.incomingFrom(w, r)
	if !ok {
		return
	}
	if err := s.Store.DeleteIncoming(r.Context(), h.ID); err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// rotateIncomingToken gives a hook a new address; the old one stops now.
func (s *Server) rotateIncomingToken(w http.ResponseWriter, r *http.Request, u store.User) {
	h, ok := s.incomingFrom(w, r)
	if !ok {
		return
	}
	h.Token = newHookToken()
	if err := s.Store.SetIncomingToken(r.Context(), h.ID, h.Token); err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, incomingViewOf(h))
}

func (s *Server) incomingReceived(w http.ResponseWriter, r *http.Request, u store.User) {
	h, ok := s.incomingFrom(w, r)
	if !ok {
		return
	}
	list, err := s.Store.ReceivedFor(r.Context(), h.ID, queryLimit(r, 50, 200))
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"items": list})
}

// --- Receiving updates ---

// maxIncomingBody caps what a tool may post.
const maxIncomingBody = 64 << 10

// maxLoggedBody caps what the received log keeps of each update.
const maxLoggedBody = 8 << 10

// Rate limits for updates, as the manager's Reference documents them.
const (
	perTitleLimit  = 2
	perTitleWindow = time.Second
	perHookLimit   = 60
	perHookWindow  = time.Minute
)

// hookIn receives an update at an incoming hook's address. No sign-in: the
// address is the secret.
func (s *Server) hookIn(w http.ResponseWriter, r *http.Request) {
	h, err := s.Store.IncomingByToken(r.Context(), r.PathValue("token"))
	if err != nil {
		writeErr(w, http.StatusNotFound, "no such hook: the address is wrong or was replaced")
		return
	}
	if !h.Enabled {
		writeErr(w, http.StatusForbidden, "this hook is turned off on the server")
		return
	}
	src := statusSource{key: "hook:" + strconv.FormatInt(h.ID, 10), name: h.Name, hook: &h}
	s.receiveStatus(w, r, src)
}

// postStatus is an update sent with an API key (scope status:write) or by
// an owner: the Kinara format, as from an incoming hook.
func (s *Server) postStatus(w http.ResponseWriter, r *http.Request, u store.User) {
	src := statusSource{key: "user:" + strconv.FormatInt(u.ID, 10), name: u.Username}
	if k := keyFrom(r); k != nil {
		src = statusSource{key: "key:" + strconv.FormatInt(k.ID, 10), name: k.Name}
	}
	s.receiveStatus(w, r, src)
}

// statusSource is what sent an update: an incoming hook, or an API key.
type statusSource struct {
	key  string
	name string
	// hook is nil for API keys (Kinara format, ready scans, everyone sees it).
	hook *store.IncomingHook
}

func (src statusSource) format() string {
	if src.hook == nil {
		return "kinara"
	}
	return src.hook.Format
}

func (src statusSource) logReceived(s *Server, r *http.Request, ok bool, summary string, body []byte) {
	if src.hook == nil {
		return
	}
	text := strings.ToValidUTF8(string(body), "")
	if len(text) > maxLoggedBody {
		text = strings.ToValidUTF8(text[:maxLoggedBody], "") + "..."
	}
	if err := s.Store.AddReceived(r.Context(), src.hook.ID, store.Received{OK: ok, Summary: summary, Body: text}); err != nil {
		s.Log.Warn("integrations: logging an update", "err", err)
	}
}

// tooMany answers 429 with when to try again.
func tooMany(w http.ResponseWriter, wait time.Duration, msg string) {
	sec := int(math.Ceil(wait.Seconds()))
	w.Header().Set("Retry-After", strconv.Itoa(max(sec, 1)))
	writeErr(w, http.StatusTooManyRequests, msg)
}

func (s *Server) receiveStatus(w http.ResponseWriter, r *http.Request, src statusSource) {
	if ok, wait := s.integ.limits.allow(src.key, perHookLimit, perHookWindow, time.Now()); !ok {
		tooMany(w, wait, "too many updates: at most 60 a minute for one hook")
		return
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, maxIncomingBody+1))
	if err != nil {
		writeErr(w, http.StatusBadRequest, "couldn't read the update")
		return
	}
	if len(body) > maxIncomingBody {
		writeErr(w, http.StatusRequestEntityTooLarge, "the update is too big (64 KB at most)")
		return
	}
	var doc any
	dec := json.NewDecoder(bytes.NewReader(body))
	dec.UseNumber()
	if err := dec.Decode(&doc); err != nil {
		msg := "the body isn't valid JSON: " + err.Error()
		src.logReceived(s, r, false, msg, body)
		writeErr(w, http.StatusBadRequest, msg)
		return
	}
	var upd statusUpdate
	if src.format() == "custom" {
		upd, err = readMapped(doc, *src.hook)
	} else {
		scale := "auto"
		if src.hook != nil {
			scale = src.hook.ProgressScale
		}
		upd, err = readKinara(doc, scale)
	}
	if err != nil {
		src.logReceived(s, r, false, err.Error(), body)
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	res, status, err := s.applyStatus(r, src, upd)
	if err != nil {
		if status == http.StatusTooManyRequests {
			tooMany(w, res.retryAfter, err.Error())
			return
		}
		src.logReceived(s, r, false, err.Error(), body)
		writeErr(w, status, err.Error())
		return
	}
	src.logReceived(s, r, res.matched != nil, res.summary, body)
	writeJSON(w, status, res.view())
}

// --- Reading updates ---

// statusUpdate is what an update says, whatever format it came in.
type statusUpdate struct {
	TMDBID  int
	TVDBID  int
	IMDBID  string
	Title   string
	Year    int
	Kind    string // movie, show, or "" when not said
	Season  int    // -1 when not said
	Episode int
	State   string
	// Progress is 0 to 1 (nil when not said).
	Progress *float64
	Message  string
	EtaSec   int
	// Ref is the sender's own reference: updates with the same one replace
	// each other.
	Ref string
}

// readKinara reads the Kinara format (see the manager's Reference tab).
func readKinara(doc any, scale string) (statusUpdate, error) {
	obj, ok := doc.(map[string]any)
	if !ok {
		return statusUpdate{}, errors.New("send one JSON object per update")
	}
	values := map[string]any{}
	for _, f := range []string{"tmdbId", "tvdbId", "imdbId", "title", "year", "kind", "season", "episode", "state",
		"progress", "message"} {
		if v, ok := obj[f]; ok {
			values[f] = v
		}
	}
	if v, ok := obj["eta"]; ok {
		values["eta"] = v
	} else if v, ok := obj["etaSec"]; ok {
		values["eta"] = v
	}
	if raw := asString(values["state"]); raw != "" && !validState(strings.ToLower(raw)) {
		return statusUpdate{}, fmt.Errorf("state %q isn't one of queued, in_progress, done or failed", raw)
	}
	upd, err := buildUpdate(values, nil, scale)
	if err != nil {
		return upd, err
	}
	if id, ok := obj["id"]; ok {
		upd.Ref = asString(id)
	}
	return upd, checkUpdate(upd)
}

// readMapped reads a tool's own JSON with a hook's mapping.
func readMapped(doc any, h store.IncomingHook) (statusUpdate, error) {
	values := map[string]any{}
	for field, path := range h.Mapping {
		if v, ok := readPath(doc, path); ok {
			values[field] = v
		}
	}
	upd, err := buildUpdate(values, h.StateMap, h.ProgressScale)
	if err != nil {
		return upd, err
	}
	return upd, checkUpdate(upd)
}

// checkUpdate makes sure an update says which title and where it is.
func checkUpdate(upd statusUpdate) error {
	if upd.TMDBID == 0 && upd.TVDBID == 0 && upd.IMDBID == "" && upd.Title == "" {
		return errors.New("nothing says which title this is: send tmdbId, tvdbId, imdbId, or title and year")
	}
	if upd.State == "" {
		return errors.New("no state: send state (queued, in_progress, done or failed) or progress")
	}
	return nil
}

var reIndex = regexp.MustCompile(`\[(\d+)\]`)

// readPath reads "a.b[0].c" from parsed JSON. ok is false when any step is
// missing.
func readPath(doc any, path string) (any, bool) {
	path = strings.TrimSpace(path)
	if path == "" {
		return nil, false
	}
	cur := doc
	for _, part := range strings.Split(reIndex.ReplaceAllString(path, ".$1"), ".") {
		if part == "" {
			continue
		}
		switch v := cur.(type) {
		case map[string]any:
			next, ok := v[part]
			if !ok {
				return nil, false
			}
			cur = next
		case []any:
			i, err := strconv.Atoi(part)
			if err != nil || i < 0 || i >= len(v) {
				return nil, false
			}
			cur = v[i]
		default:
			return nil, false
		}
	}
	return cur, cur != nil
}

// asString turns a JSON value into text ("" for objects, lists and null).
func asString(v any) string {
	switch x := v.(type) {
	case string:
		return strings.TrimSpace(x)
	case json.Number:
		return x.String()
	case float64:
		return strconv.FormatFloat(x, 'f', -1, 64)
	case bool:
		return strconv.FormatBool(x)
	}
	return ""
}

// asNumber reads a JSON number, or a string holding one.
func asNumber(v any) (float64, bool) {
	s := asString(v)
	if s == "" {
		return 0, false
	}
	f, err := strconv.ParseFloat(s, 64)
	if err != nil || math.IsNaN(f) || math.IsInf(f, 0) {
		return 0, false
	}
	return f, true
}

func asInt(v any) (int, bool) {
	f, ok := asNumber(v)
	if !ok || f < 0 || f > math.MaxInt32 {
		return 0, false
	}
	return int(f), true
}

// normalKind turns the many ways tools name kinds into movie or show.
func normalKind(s string) string {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "movie", "movies", "film":
		return "movie"
	case "show", "shows", "tv", "series", "episode", "season":
		return "show"
	}
	return ""
}

// mapState turns a tool's state word into Kinara's: through the hook's
// list first (any case), then Kinara's own names. "" when unknown.
func mapState(raw string, stateMap []store.StateMapping) string {
	raw = strings.TrimSpace(raw)
	for _, m := range stateMap {
		if m.From != "" && strings.EqualFold(m.From, raw) {
			return m.To
		}
	}
	if s := strings.ToLower(raw); validState(s) {
		return s
	}
	return ""
}

// scaleProgress turns a progress number into a share from 0 to 1. With
// "auto", anything above 1 is taken as a percentage.
func scaleProgress(p float64, scale string) float64 {
	if scale == "percent" || (scale == "auto" && p > 1) {
		p /= 100
	}
	return min(max(p, 0), 1)
}

// buildUpdate turns the values read from an update into a statusUpdate.
// Without a known state, progress decides: below 100% is in progress,
// 100% is done.
func buildUpdate(values map[string]any, stateMap []store.StateMapping, scale string) (statusUpdate, error) {
	upd := statusUpdate{Season: -1, Episode: -1}
	if v, ok := values["tmdbId"]; ok {
		n, ok := asInt(v)
		if !ok && asString(v) != "" {
			return upd, errors.New("tmdbId must be a number")
		}
		upd.TMDBID = n
	}
	if v, ok := values["tvdbId"]; ok {
		n, ok := asInt(v)
		if !ok && asString(v) != "" {
			return upd, errors.New("tvdbId must be a number")
		}
		upd.TVDBID = n
	}
	upd.IMDBID = asString(values["imdbId"])
	if upd.IMDBID != "" && !reIMDB.MatchString(upd.IMDBID) {
		return upd, errors.New(`imdbId looks like "tt0944947"`)
	}
	upd.Title = asString(values["title"])
	if len(upd.Title) > 300 {
		upd.Title = upd.Title[:300]
	}
	upd.Year, _ = asInt(values["year"])
	if k := asString(values["kind"]); k != "" {
		upd.Kind = normalKind(k)
		if upd.Kind == "" {
			return upd, fmt.Errorf(`kind %q isn't "movie" or "show"`, k)
		}
	}
	if n, ok := asInt(values["season"]); ok {
		upd.Season = n
	}
	if n, ok := asInt(values["episode"]); ok {
		upd.Episode = n
	}
	if upd.Season >= 0 && upd.Kind == "" {
		upd.Kind = "show"
	}
	if v, ok := values["progress"]; ok {
		if p, ok := asNumber(v); ok {
			p = scaleProgress(p, scale)
			upd.Progress = &p
		}
	}
	upd.State = mapState(asString(values["state"]), stateMap)
	if upd.State == "" && upd.Progress != nil {
		if *upd.Progress >= 1 {
			upd.State = stateDone
		} else {
			upd.State = stateInProgress
		}
	}
	upd.Message = asString(values["message"])
	if len(upd.Message) > 200 {
		upd.Message = strings.ToValidUTF8(upd.Message[:200], "")
	}
	if eta, ok := asNumber(values["eta"]); ok && eta > 0 {
		upd.EtaSec = int(min(eta, 365*24*3600))
	}
	return upd, nil
}

var reIMDB = regexp.MustCompile(`^tt\d{5,10}$`)

// --- Rate limits ---

// slidingLimiter allows up to n hits per key in any window of time.
type slidingLimiter struct {
	mu        sync.Mutex
	hits      map[string][]time.Time
	lastSweep time.Time
}

// allow counts a hit and says whether it's within the limit; if not, how
// long until it would be.
func (l *slidingLimiter) allow(key string, n int, window time.Duration, now time.Time) (bool, time.Duration) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.hits == nil {
		l.hits = map[string][]time.Time{}
	}
	if now.Sub(l.lastSweep) > 2*time.Minute {
		l.sweep(now)
	}
	recent := l.hits[key][:0]
	for _, t := range l.hits[key] {
		if now.Sub(t) < window {
			recent = append(recent, t)
		}
	}
	if len(recent) >= n {
		l.hits[key] = recent
		return false, recent[0].Add(window).Sub(now)
	}
	l.hits[key] = append(recent, now)
	return true, 0
}

// sweep forgets keys with no recent hits (the longest window is a minute).
func (l *slidingLimiter) sweep(now time.Time) {
	l.lastSweep = now
	for k, ts := range l.hits {
		if len(ts) == 0 || now.Sub(ts[len(ts)-1]) > perHookWindow {
			delete(l.hits, k)
		}
	}
}
