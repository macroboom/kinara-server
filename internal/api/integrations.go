package api

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"math/big"
	"net/http"
	"os"
	"slices"
	"sync"
	"time"

	"kinara/internal/store"
)

// Integrations: outgoing webhooks, incoming hooks and API keys. See the
// shared contract in the integrations spec. Owned by the integrations work;
// other code only calls emit and titleStatus.
//
//	integrations.go           state, routes, emit and the event catalogue
//	integrations_webhooks.go  webhook endpoints
//	integrations_delivery.go  the delivery worker, signing and sending
//	integrations_incoming.go  incoming hooks and reading what they're sent
//	integrations_status.go    titles on their way, and title matching
//	integrations_keys.go      API keys and signing in with one
//	integrations_events.go    building event payloads where things happen

// integrationsState holds what the integrations need at run time.
type integrationsState struct {
	mu sync.Mutex
	// hooks are the turned-on webhooks, so emit never reads the database
	// to find out nobody is listening.
	hooks []store.Webhook
	// server names this server in payloads.
	server serverRef

	// The delivery worker: running while any webhook is on.
	running bool
	wake    chan struct{}
	// pending are new deliveries for the worker to queue. emit only adds
	// to it, so callers never wait for the database.
	pending chan store.QueuedDelivery
	// busy marks webhooks with a delivery in flight (one at a time each,
	// so they arrive in order).
	busy map[int64]bool
	// wg tracks the worker and its sends, so tests can wait for them.
	wg sync.WaitGroup

	limits  slidingLimiter
	plays   map[string]*playEntry
	watched map[string]time.Time
	keyUsed map[int64]int64
	titles  map[string]cachedMatch
	// progressSent is when status.updated last went out for a title, and at
	// what progress, so progress reaches webhooks without flooding them.
	progressSent map[string]progressMark
	// remote is the last remote access state seen ("" before the first look).
	remote string
}

// serverRef is the "server" object of every payload.
type serverRef struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}

// pendingBuffer is how many new deliveries can wait for the worker before
// more are dropped (with a warning).
const pendingBuffer = 1024

// integrationRoutes registers the integrations endpoints.
func (s *Server) integrationRoutes(mux *http.ServeMux) {
	s.integ.wake = make(chan struct{}, 1)
	s.integ.pending = make(chan store.QueuedDelivery, pendingBuffer)
	// Old delivery logs and expired statuses go even on a server with no
	// webhook (incoming hooks and status keys write them too).
	go s.pruneIntegrations()

	mux.Handle("GET /api/v1/integrations/events", s.admin(s.integrationEvents))

	mux.Handle("GET /api/v1/integrations/webhooks", s.admin(s.listWebhooks))
	mux.Handle("POST /api/v1/integrations/webhooks", s.admin(s.createWebhook))
	mux.Handle("PUT /api/v1/integrations/webhooks/{id}", s.admin(s.updateWebhook))
	mux.Handle("DELETE /api/v1/integrations/webhooks/{id}", s.admin(s.deleteWebhook))
	mux.Handle("POST /api/v1/integrations/webhooks/{id}/test", s.admin(s.testWebhook))
	mux.Handle("POST /api/v1/integrations/webhooks/{id}/secret", s.admin(s.rotateWebhookSecret))
	mux.Handle("GET /api/v1/integrations/webhooks/{id}/deliveries", s.admin(s.webhookDeliveries))
	mux.Handle("POST /api/v1/integrations/webhooks/{id}/deliveries/{delivery}/redeliver", s.admin(s.redeliver))

	mux.Handle("GET /api/v1/integrations/incoming", s.admin(s.listIncoming))
	mux.Handle("POST /api/v1/integrations/incoming", s.admin(s.createIncoming))
	mux.Handle("PUT /api/v1/integrations/incoming/{id}", s.admin(s.updateIncoming))
	mux.Handle("DELETE /api/v1/integrations/incoming/{id}", s.admin(s.deleteIncoming))
	mux.Handle("POST /api/v1/integrations/incoming/{id}/token", s.admin(s.rotateIncomingToken))
	mux.Handle("GET /api/v1/integrations/incoming/{id}/received", s.admin(s.incomingReceived))
	mux.HandleFunc("POST /api/v1/hooks/in/{token}", s.hookIn)

	// GET /api/v1/status is the server's own status page, so the list of
	// titles on their way has a path of its own.
	mux.Handle("GET /api/v1/ontheway", s.user(s.listStatuses))
	mux.Handle("DELETE /api/v1/ontheway/{id}", s.admin(s.clearStatus))
	mux.Handle("POST /api/v1/status", s.admin(s.postStatus))

	mux.Handle("GET /api/v1/integrations/keys", s.admin(s.listKeys))
	mux.Handle("POST /api/v1/integrations/keys", s.admin(s.createKey))
	mux.Handle("DELETE /api/v1/integrations/keys/{id}", s.admin(s.deleteKey))

	if s.Scanner != nil {
		s.Scanner.SetOnChanges(s.scanChanged)
	}
	if s.Store == nil {
		return // a page-only server in tests
	}
	ctx := s.baseCtx()
	s.reloadHooks(ctx)
	s.integ.mu.Lock()
	s.integ.server = s.serverIdentity(ctx)
	s.integ.mu.Unlock()
	s.noteServerStarted(ctx)
}

// baseCtx is BaseCtx, or a context that never ends when it's not set.
func (s *Server) baseCtx() context.Context {
	if s.BaseCtx != nil {
		return s.BaseCtx
	}
	return context.Background()
}

// serverIdentity: the cloud's ID for this server when it's linked (the
// apps know it by that), else one made for the integrations; and the
// computer's name.
func (s *Server) serverIdentity(ctx context.Context) serverRef {
	ref := serverRef{}
	if s.Cloud != nil {
		if st := s.Cloud.State(); st != nil && st.ServerID != "" {
			ref.ID = st.ServerID
		}
	}
	if ref.ID == "" {
		ref.ID, _ = s.Store.IntegrationsServerID(ctx)
	}
	ref.Name, _ = os.Hostname()
	if ref.Name == "" {
		ref.Name = "Kinara"
	}
	return ref
}

// reloadHooks refreshes the turned-on webhooks emit checks, and starts the
// delivery worker when there are any.
func (s *Server) reloadHooks(ctx context.Context) {
	all, err := s.Store.ListWebhooks(ctx)
	if err != nil {
		s.Log.Warn("integrations: reading webhooks", "err", err)
		return
	}
	on := []store.Webhook{}
	for _, h := range all {
		if h.Enabled {
			on = append(on, h)
		}
	}
	s.integ.mu.Lock()
	s.integ.hooks = on
	s.integ.mu.Unlock()
	s.ensureWorker()
}

// eventScope narrows which webhooks get an event: those limited to some
// libraries or accounts only get events for those. Zero means no limit on
// that dimension.
type eventScope struct {
	LibraryID int64
	UserID    int64
}

// wantsEvent says whether a webhook should get an event.
func wantsEvent(h store.Webhook, event string, scope eventScope) bool {
	if !slices.Contains(h.Events, event) {
		return false
	}
	if scope.LibraryID != 0 && h.Libraries != nil && !slices.Contains(h.Libraries, scope.LibraryID) {
		return false
	}
	if scope.UserID != 0 && h.Accounts != nil && !slices.Contains(h.Accounts, scope.UserID) {
		return false
	}
	return true
}

// listeners are the turned-on webhooks that want an event.
func (s *Server) listeners(event string, scope eventScope) []store.Webhook {
	s.integ.mu.Lock()
	defer s.integ.mu.Unlock()
	var out []store.Webhook
	for _, h := range s.integ.hooks {
		if wantsEvent(h, event, scope) {
			out = append(out, h)
		}
	}
	return out
}

// wants says whether any webhook wants an event of this kind at all, so
// callers can skip building a payload nobody gets.
func (s *Server) wants(events ...string) bool {
	s.integ.mu.Lock()
	defer s.integ.mu.Unlock()
	for _, h := range s.integ.hooks {
		for _, e := range events {
			if slices.Contains(h.Events, e) {
				return true
			}
		}
	}
	return false
}

// envelope is the body of every delivery.
type envelope struct {
	ID         string          `json:"id"`
	Event      string          `json:"event"`
	OccurredAt string          `json:"occurredAt"`
	Server     serverRef       `json:"server"`
	Data       json.RawMessage `json:"data"`
}

// maxDeliveryBody caps a delivery's size: a payload bigger than this is
// a bug, not something to send.
const maxDeliveryBody = 256 << 10

// emit tells the webhooks that want it about an event. data becomes the
// envelope's "data" object.
func (s *Server) emit(event string, scope eventScope, data any) {
	s.emitWith(event, scope, func(bool) any { return data })
}

// emitWith is emit with the payload built only if someone wants it. build
// gets whether the webhook asked for file paths. It never blocks on the
// network or fails the caller: problems are logged.
func (s *Server) emitWith(event string, scope eventScope, build func(paths bool) any) {
	targets := s.listeners(event, scope)
	if len(targets) == 0 || s.integ.pending == nil {
		return
	}
	s.integ.mu.Lock()
	server := s.integ.server
	s.integ.mu.Unlock()
	at := time.Now()
	var built [2]json.RawMessage
	for _, h := range targets {
		i := 0
		if h.IncludePaths {
			i = 1
		}
		if built[i] == nil {
			b, err := json.Marshal(build(h.IncludePaths))
			if err != nil {
				s.Log.Warn("integrations: payload", "event", event, "err", err)
				return
			}
			built[i] = b
		}
		id := newDeliveryID()
		body, err := marshalEnvelope(envelope{ID: id, Event: event, OccurredAt: at.UTC().Format(time.RFC3339),
			Server: server, Data: built[i]})
		if err != nil || len(body) > maxDeliveryBody {
			s.Log.Warn("integrations: payload too big or broken; not sent", "event", event, "bytes", len(body), "err", err)
			continue
		}
		d := store.QueuedDelivery{DeliveryID: id, WebhookID: h.ID, Event: event, Body: string(body),
			NextAt: at.Unix(), CreatedAt: at.Unix()}
		select {
		case s.integ.pending <- d:
		default:
			s.Log.Warn("integrations: too many deliveries waiting; one was dropped", "event", event, "webhook", h.Name)
		}
	}
	s.ensureWorker()
	s.wakeWorker()
}

// marshalEnvelope writes a delivery's body: the bytes that are signed and
// sent, and kept for the log and for sending again.
func marshalEnvelope(e envelope) ([]byte, error) {
	return json.Marshal(e)
}

const tokenChars = "ABCDEFGHJKLMNPQRSTUVWXYZabcdefghijkmnopqrstuvwxyz23456789"

// randomToken is prefix plus n random characters that are easy to read.
func randomToken(prefix string, n int) string {
	out := make([]byte, n)
	max := big.NewInt(int64(len(tokenChars)))
	for i := range out {
		v, err := rand.Int(rand.Reader, max)
		if err != nil {
			panic(err)
		}
		out[i] = tokenChars[v.Int64()]
	}
	return prefix + string(out)
}

func newDeliveryID() string { return randomToken("dlv_", 16) }

// --- The event catalogue ---

type eventDef struct {
	ID          string `json:"id"`
	Group       string `json:"group"`
	Label       string `json:"label"`
	Description string `json:"description"`
	// Soon marks events the server doesn't send yet.
	Soon bool `json:"soon,omitempty"`
}

var eventCatalogue = []eventDef{
	{"library.item_added", "Library", "Title added", "A movie, show, season or episode appears after a scan.", false},
	{"library.item_removed", "Library", "Title removed", "A title's files are gone from disk.", false},
	{"library.scan_finished", "Library", "Scan finished", "A library scan completed, with what it found.", false},
	{"library.match_needed", "Library", "Needs matching", "A file couldn't be matched to a title on its own.", false},
	{"playback.started", "Playback", "Started", "Someone starts playing a title.", false},
	{"playback.paused", "Playback", "Paused", "Playback is paused for more than a few seconds.", false},
	{"playback.resumed", "Playback", "Resumed", "Paused playback carries on.", false},
	{"playback.stopped", "Playback", "Stopped", "Playback ends, with how far it got.", false},
	{"item.watched", "Playback", "Watched", "A title is finished, or marked watched by hand.", false},
	{"user.signed_in", "Accounts", "Signed in", "An account signs in on a device.", false},
	{"user.created", "Accounts", "Account created", "A new account is made on this server.", false},
	{"device.new", "Accounts", "New device", "A device this server has not seen before connects.", false},
	{"request.created", "Requests", "Requested", "Someone asks for a title that is not on the server.", false},
	{"request.approved", "Requests", "Approved", "The owner approves a request.", false},
	{"request.declined", "Requests", "Declined", "The owner declines a request.", false},
	{"status.updated", "Incoming updates", "Status changed", "A title's status changed: a new state, or progress moved on.", false},
	{"status.done", "Incoming updates", "Ready", "An incoming update said a title is ready.", false},
	{"server.started", "Server", "Started", "The server starts or restarts.", false},
	{"server.update_available", "Server", "Update available", "A newer version of Kinara is out.", true},
	{"conversion.failed", "Server", "Conversion failed", "A stream couldn't be converted for a device.", false},
	{"remote.state_changed", "Server", "Remote access changed", "Remote access connects, disconnects or falls back to the relay.", false},
}

func knownEvent(id string) bool {
	for _, e := range eventCatalogue {
		if e.ID == id {
			return true
		}
	}
	return false
}

type scopeDef struct {
	ID          string `json:"id"`
	Label       string `json:"label"`
	Description string `json:"description"`
}

var scopeCatalogue = []scopeDef{
	{scopeLibraryRead, "Read the library", "Titles, seasons, episodes and their files."},
	{scopeLibraryScan, "Start scans", "Scan a whole library or one folder in it."},
	{scopeStatusWrite, "Send status updates", "The same as an incoming hook, with this key instead of a hook URL."},
	{scopeActivityRead, "Read activity", "What's playing now."},
	{scopeWebhooksManage, "Manage integrations", "Create, change and remove webhooks and incoming hooks."},
}

// integrationEvents lists every event and API key scope, for the manager.
func (s *Server) integrationEvents(w http.ResponseWriter, r *http.Request, u store.User) {
	writeJSON(w, http.StatusOK, map[string]any{"items": eventCatalogue, "scopes": scopeCatalogue})
}

// pruneIntegrations forgets old delivery attempts and expired statuses
// every hour, whether or not the delivery worker runs.
func (s *Server) pruneIntegrations() {
	ctx := s.baseCtx()
	tick := time.NewTicker(time.Hour)
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
			_ = s.Store.PruneAttempts(ctx, time.Now().Add(-keepDeliveries).Unix())
			_ = s.Store.PruneStatuses(ctx)
		}
	}
}
