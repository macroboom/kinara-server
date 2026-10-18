package api

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"

	"kinora/internal/store"
)

// Outgoing webhook endpoints (owners, or a key with webhooks:manage).

type webhookHealth struct {
	// State is ok, failing, never (nothing sent yet) or paused.
	State      string `json:"state"`
	LastAt     int64  `json:"lastAt,omitempty"`
	LastStatus *int   `json:"lastStatus"`
	LastError  string `json:"lastError,omitempty"`
}

type webhookView struct {
	ID           int64                 `json:"id"`
	Name         string                `json:"name"`
	URL          string                `json:"url"`
	Enabled      bool                  `json:"enabled"`
	Secret       string                `json:"secret"`
	Events       []string              `json:"events"`
	Libraries    any                   `json:"libraries"`
	Accounts     any                   `json:"accounts"`
	Headers      []store.WebhookHeader `json:"headers"`
	Retries      int                   `json:"retries"`
	TimeoutSec   int                   `json:"timeoutSec"`
	IncludePaths bool                  `json:"includePaths"`
	Health       webhookHealth         `json:"health"`
	CreatedAt    int64                 `json:"createdAt"`
}

// idsOrAll is "all" for nil, else the list.
func idsOrAll(ids []int64) any {
	if ids == nil {
		return "all"
	}
	return ids
}

func webhookViewOf(h store.Webhook, last *store.Attempt) webhookView {
	v := webhookView{ID: h.ID, Name: h.Name, URL: h.URL, Enabled: h.Enabled, Secret: h.Secret, Events: h.Events,
		Libraries: idsOrAll(h.Libraries), Accounts: idsOrAll(h.Accounts), Headers: h.Headers, Retries: h.Retries,
		TimeoutSec: h.TimeoutSec, IncludePaths: h.IncludePaths, CreatedAt: h.CreatedAt}
	switch {
	case !h.Enabled:
		v.Health.State = "paused"
	case last == nil:
		v.Health.State = "never"
	case delivered(*last):
		v.Health.State = "ok"
	default:
		v.Health.State = "failing"
	}
	if last != nil {
		v.Health.LastAt, v.Health.LastStatus, v.Health.LastError = last.At, last.Status, last.Error
	}
	return v
}

// deliveryView is one try at a delivery, for the log.
type deliveryView struct {
	ID int64 `json:"id"`
	// DeliveryID is the X-Kinora-Delivery header (the same for every try).
	DeliveryID string  `json:"deliveryId"`
	At         int64   `json:"at"`
	Event      string  `json:"event"`
	Status     *int    `json:"status"`
	Ms         int64   `json:"ms"`
	Attempt    int     `json:"attempt"`
	Error      string  `json:"error,omitempty"`
	Request    string  `json:"request"`
	Response   *string `json:"response,omitempty"`
}

func deliveryViewOf(a store.Attempt) deliveryView {
	return deliveryView{ID: a.ID, DeliveryID: a.DeliveryID, At: a.At, Event: a.Event, Status: a.Status, Ms: a.Ms,
		Attempt: a.Attempt, Error: a.Error, Request: a.Request, Response: a.Response}
}

// webhookInput is a webhook as sent by the manager. Fields left out keep
// their value (on create: their default).
type webhookInput struct {
	Name         *string                `json:"name"`
	URL          *string                `json:"url"`
	Enabled      *bool                  `json:"enabled"`
	Secret       *string                `json:"secret"`
	Events       []string               `json:"events"`
	Libraries    json.RawMessage        `json:"libraries"`
	Accounts     json.RawMessage        `json:"accounts"`
	Headers      *[]store.WebhookHeader `json:"headers"`
	Retries      *int                   `json:"retries"`
	TimeoutSec   *int                   `json:"timeoutSec"`
	IncludePaths *bool                  `json:"includePaths"`
}

// readJSONLoose is readJSON that ignores fields it doesn't know, so the
// manager can send back a whole object it read (with id and health).
func readJSONLoose(r *http.Request, v any) error {
	if err := json.NewDecoder(io.LimitReader(r.Body, 1<<20)).Decode(v); err != nil {
		return errors.New("invalid JSON body: " + err.Error())
	}
	return nil
}

var reHeaderName = regexp.MustCompile(`^[A-Za-z0-9!#$%&'*+.^_|~-]{1,64}$`)

// reservedHeaders are set by Kinora on every delivery.
var reservedHeaders = map[string]bool{"content-type": true, "content-length": true, "host": true,
	"user-agent": true, "transfer-encoding": true, "connection": true}

// checkWebhookURL accepts full http and https addresses.
func checkWebhookURL(raw string) (string, error) {
	raw = strings.TrimSpace(raw)
	u, err := url.Parse(raw)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" || len(raw) > 2000 {
		return "", errors.New("enter a full address starting with http:// or https://")
	}
	if u.User != nil {
		return "", errors.New("put a user name and password in an Authorization header instead of the address")
	}
	return raw, nil
}

// parseIDs reads "all" (nil) or a list of IDs.
func parseIDs(raw json.RawMessage, what string) ([]int64, error) {
	var all string
	if json.Unmarshal(raw, &all) == nil {
		if all == "all" {
			return nil, nil
		}
		return nil, fmt.Errorf("%s is \"all\" or a list of IDs", what)
	}
	ids := []int64{}
	if err := json.Unmarshal(raw, &ids); err != nil {
		return nil, fmt.Errorf("%s is \"all\" or a list of IDs", what)
	}
	return ids, nil
}

// apply copies the manager's changes onto h and checks the result.
func (in webhookInput) apply(h *store.Webhook) error {
	if in.Name != nil {
		h.Name = strings.TrimSpace(*in.Name)
	}
	if in.URL != nil {
		h.URL = strings.TrimSpace(*in.URL)
	}
	if in.Enabled != nil {
		h.Enabled = *in.Enabled
	}
	if in.Secret != nil && strings.TrimSpace(*in.Secret) != "" {
		h.Secret = strings.TrimSpace(*in.Secret)
	}
	if in.Events != nil {
		h.Events = in.Events
	}
	if in.Libraries != nil {
		ids, err := parseIDs(in.Libraries, "libraries")
		if err != nil {
			return err
		}
		h.Libraries = ids
	}
	if in.Accounts != nil {
		ids, err := parseIDs(in.Accounts, "accounts")
		if err != nil {
			return err
		}
		h.Accounts = ids
	}
	if in.Headers != nil {
		h.Headers = *in.Headers
	}
	if in.Retries != nil {
		h.Retries = *in.Retries
	}
	if in.TimeoutSec != nil {
		h.TimeoutSec = *in.TimeoutSec
	}
	if in.IncludePaths != nil {
		h.IncludePaths = *in.IncludePaths
	}
	return checkWebhook(h)
}

func checkWebhook(h *store.Webhook) error {
	if h.Name == "" || len(h.Name) > 80 {
		return errors.New("give the webhook a name (up to 80 characters)")
	}
	u, err := checkWebhookURL(h.URL)
	if err != nil {
		return err
	}
	h.URL = u
	if len(h.Secret) < 16 || len(h.Secret) > 128 {
		return errors.New("the secret must be 16 to 128 characters")
	}
	if len(h.Events) == 0 {
		return errors.New("choose at least one event")
	}
	seen := map[string]bool{}
	events := []string{}
	for _, e := range h.Events {
		if !knownEvent(e) {
			return fmt.Errorf("%q isn't an event Kinora sends", e)
		}
		if !seen[e] {
			seen[e] = true
			events = append(events, e)
		}
	}
	h.Events = events
	if h.Retries < 0 || h.Retries > 8 {
		return errors.New("retries is 0 to 8")
	}
	if h.TimeoutSec < 5 || h.TimeoutSec > 30 {
		return errors.New("timeoutSec is 5 to 30 seconds")
	}
	if len(h.Headers) > 20 {
		return errors.New("up to 20 extra headers")
	}
	headers := []store.WebhookHeader{}
	for _, hd := range h.Headers {
		hd.Name = strings.TrimSpace(hd.Name)
		if hd.Name == "" && hd.Value == "" {
			continue
		}
		lower := strings.ToLower(hd.Name)
		if !reHeaderName.MatchString(hd.Name) {
			return fmt.Errorf("%q can't be a header name", hd.Name)
		}
		if reservedHeaders[lower] || strings.HasPrefix(lower, "x-kinora-") {
			return fmt.Errorf("Kinora sets the %s header itself", hd.Name)
		}
		if strings.ContainsAny(hd.Value, "\r\n") || len(hd.Value) > 4096 {
			return fmt.Errorf("the value of %s must be one line, up to 4096 characters", hd.Name)
		}
		headers = append(headers, hd)
	}
	h.Headers = headers
	return nil
}

func newWebhookSecret() string { return randomToken("whsec_", 24) }

func (s *Server) listWebhooks(w http.ResponseWriter, r *http.Request, u store.User) {
	hooks, err := s.Store.ListWebhooks(r.Context())
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	last, err := s.Store.LastAttempts(r.Context())
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	out := []webhookView{}
	for _, h := range hooks {
		var a *store.Attempt
		if l, ok := last[h.ID]; ok {
			a = &l
		}
		out = append(out, webhookViewOf(h, a))
	}
	writeJSON(w, http.StatusOK, map[string]any{"items": out})
}

func (s *Server) createWebhook(w http.ResponseWriter, r *http.Request, u store.User) {
	var in webhookInput
	if err := readJSONLoose(r, &in); err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	h := store.Webhook{Enabled: true, Secret: newWebhookSecret(), Retries: 3, TimeoutSec: 10,
		Events: []string{}, Headers: []store.WebhookHeader{}}
	if err := in.apply(&h); err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	if err := s.Store.CreateWebhook(r.Context(), &h); err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	s.reloadHooks(r.Context())
	writeJSON(w, http.StatusCreated, webhookViewOf(h, nil))
}

// webhookFrom reads the webhook named in the path, answering 404 if none.
func (s *Server) webhookFrom(w http.ResponseWriter, r *http.Request) (store.Webhook, bool) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		writeErr(w, http.StatusNotFound, "no such webhook")
		return store.Webhook{}, false
	}
	h, err := s.Store.GetWebhook(r.Context(), id)
	if err != nil {
		writeErr(w, http.StatusNotFound, "no such webhook")
		return h, false
	}
	return h, true
}

func (s *Server) lastAttempt(r *http.Request, id int64) *store.Attempt {
	list, err := s.Store.Attempts(r.Context(), id, 1)
	if err != nil || len(list) == 0 {
		return nil
	}
	return &list[0]
}

func (s *Server) updateWebhook(w http.ResponseWriter, r *http.Request, u store.User) {
	h, ok := s.webhookFrom(w, r)
	if !ok {
		return
	}
	var in webhookInput
	if err := readJSONLoose(r, &in); err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	if err := in.apply(&h); err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	if err := s.Store.UpdateWebhook(r.Context(), h); err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	s.reloadHooks(r.Context())
	writeJSON(w, http.StatusOK, webhookViewOf(h, s.lastAttempt(r, h.ID)))
}

func (s *Server) deleteWebhook(w http.ResponseWriter, r *http.Request, u store.User) {
	h, ok := s.webhookFrom(w, r)
	if !ok {
		return
	}
	if err := s.Store.DeleteWebhook(r.Context(), h.ID); err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	s.reloadHooks(r.Context())
	w.WriteHeader(http.StatusNoContent)
}

// rotateWebhookSecret makes a new secret; the old one stops working now.
func (s *Server) rotateWebhookSecret(w http.ResponseWriter, r *http.Request, u store.User) {
	h, ok := s.webhookFrom(w, r)
	if !ok {
		return
	}
	h.Secret = newWebhookSecret()
	if err := s.Store.UpdateWebhook(r.Context(), h); err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	s.reloadHooks(r.Context())
	writeJSON(w, http.StatusOK, map[string]string{"secret": h.Secret})
}

// testWebhook sends a "test" event now and answers with what happened. It
// is logged like any delivery but never retried.
func (s *Server) testWebhook(w http.ResponseWriter, r *http.Request, u store.User) {
	h, ok := s.webhookFrom(w, r)
	if !ok {
		return
	}
	s.integ.mu.Lock()
	server := s.integ.server
	s.integ.mu.Unlock()
	data, _ := json.Marshal(map[string]any{"message": "This is a test from Kinora. If you can read this, the webhook works."})
	id := newDeliveryID()
	body, _ := marshalEnvelope(envelope{ID: id, Event: "test", OccurredAt: time.Now().UTC().Format(time.RFC3339),
		Server: server, Data: data})
	s.sendNow(w, r, h, id, "test", body)
}

// sendNow sends a delivery straight away (a test, or sending one again) and
// answers with the logged result.
func (s *Server) sendNow(w http.ResponseWriter, r *http.Request, h store.Webhook, id, event string, body []byte) {
	a := s.post(r.Context(), h, id, event, body, 1)
	if err := s.Store.AddAttempt(r.Context(), &a); err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, deliveryViewOf(a))
}

func (s *Server) webhookDeliveries(w http.ResponseWriter, r *http.Request, u store.User) {
	h, ok := s.webhookFrom(w, r)
	if !ok {
		return
	}
	limit := queryLimit(r, 50, 200)
	list, err := s.Store.Attempts(r.Context(), h.ID, limit)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	out := make([]deliveryView, 0, len(list))
	for _, a := range list {
		out = append(out, deliveryViewOf(a))
	}
	writeJSON(w, http.StatusOK, map[string]any{"items": out})
}

// queryLimit reads ?limit=, between 1 and most.
func queryLimit(r *http.Request, def, most int) int {
	n, err := strconv.Atoi(r.URL.Query().Get("limit"))
	if err != nil || n <= 0 {
		return def
	}
	return min(n, most)
}

// redeliver sends a logged delivery again now, as a new delivery (with a
// new ID, so the receiver doesn't take it for a repeat), and answers with
// the result.
func (s *Server) redeliver(w http.ResponseWriter, r *http.Request, u store.User) {
	h, ok := s.webhookFrom(w, r)
	if !ok {
		return
	}
	aid, err := strconv.ParseInt(r.PathValue("delivery"), 10, 64)
	if err != nil {
		writeErr(w, http.StatusNotFound, "no such delivery")
		return
	}
	a, err := s.Store.GetAttempt(r.Context(), h.ID, aid)
	if err != nil {
		writeErr(w, http.StatusNotFound, "no such delivery")
		return
	}
	var env envelope
	if err := json.Unmarshal([]byte(a.Request), &env); err != nil {
		writeErr(w, http.StatusConflict, "that delivery can't be sent again")
		return
	}
	env.ID = newDeliveryID()
	body, err := marshalEnvelope(env)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	s.sendNow(w, r, h, env.ID, a.Event, body)
}
