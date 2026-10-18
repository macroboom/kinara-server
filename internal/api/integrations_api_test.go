//go:build !nosqlite

package api

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"kinora/internal/auth"
	"kinora/internal/images"
	"kinora/internal/probe"
	"kinora/internal/scanner"
	"kinora/internal/store"
	"kinora/internal/tmdb"
	"kinora/internal/watch"
)

type integEnv struct {
	srv   *Server
	st    *store.Store
	owner *client
	base  string
	lib   store.Library
	movie string // the library's Masterplan (TMDB movie 1)
	h     http.Handler
}

// integServer is a server with a Movies library holding Masterplan, a fake
// TMDB, and an owner signed in. Its worker stops when the test ends.
func integServer(t *testing.T) *integEnv {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	dir := t.TempDir()
	st, err := store.Open(filepath.Join(dir, "kinora.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	hash, _ := auth.HashPassword("password123")
	if _, err := st.CreateUser(ctx, "alex", hash, true); err != nil {
		t.Fatal(err)
	}
	lib, err := st.CreateLibrary(ctx, "Movies", filepath.Join(dir, "movies"), store.KindMovies)
	if err != nil {
		t.Fatal(err)
	}
	movie := addMovie(t, st, lib, "Masterplan")
	if err := st.SetExternalID(ctx, movie, store.ProviderTMDBMovie, "1"); err != nil {
		t.Fatal(err)
	}
	var calls atomic.Int32
	fake := fakeTMDBServer(t, &calls)
	srv := &Server{Store: st, Scanner: &scanner.Scanner{Store: st, Prober: &probe.Prober{}, Log: log},
		Images: images.New(filepath.Join(dir, "img")), Watch: &watch.Service{Store: st}, Log: log, Version: "test", BaseCtx: ctx,
		TMDB: &tmdb.Client{Key: "k", BaseURL: fake.URL, HTTP: fake.Client()}}
	h := srv.Handler()
	hs := httptest.NewServer(h)
	t.Cleanup(func() {
		hs.Close()
		cancel()
		srv.integ.wg.Wait()
		// Let scans started by "ready" updates end before the database closes.
		for range 50 {
			if p, _ := srv.Scanner.Progress(lib.ID); !p.Running {
				break
			}
			time.Sleep(20 * time.Millisecond)
		}
	})
	c := &client{t: t, base: hs.URL + "/api/v1"}
	var login struct{ Token string }
	c.do("POST", "/auth/login", map[string]string{"username": "alex", "password": "password123"}, &login)
	c.token = login.Token
	return &integEnv{srv: srv, st: st, owner: c, base: hs.URL + "/api/v1", lib: lib, movie: movie, h: h}
}

// receiver is a webhook endpoint that records what it gets.
type receiver struct {
	mu   sync.Mutex
	got  []*http.Request
	body [][]byte
	// fail answers 500 to this many requests first.
	fail int
}

func (rc *receiver) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	b, _ := io.ReadAll(r.Body)
	rc.mu.Lock()
	defer rc.mu.Unlock()
	rc.got = append(rc.got, r)
	rc.body = append(rc.body, b)
	if rc.fail > 0 {
		rc.fail--
		w.WriteHeader(http.StatusInternalServerError)
		return
	}
	w.Write([]byte(`{"ok":true}`))
}

// wait returns the first request for an event, waiting up to 5 s.
func (rc *receiver) wait(t *testing.T, event string) (*http.Request, []byte) {
	t.Helper()
	for range 250 {
		rc.mu.Lock()
		for i, r := range rc.got {
			if r.Header.Get("X-Kinora-Event") == event {
				rc.mu.Unlock()
				return r, rc.body[i]
			}
		}
		rc.mu.Unlock()
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("no %s delivery arrived", event)
	return nil, nil
}

type webhookResp struct {
	ID     int64
	Secret string
	Health struct{ State string }
	Events []string
}

func TestWebhookDelivery(t *testing.T) {
	e := integServer(t)
	rc := &receiver{}
	rs := httptest.NewServer(rc)
	t.Cleanup(rs.Close)

	// Checks first.
	if resp := e.owner.do("POST", "/integrations/webhooks", map[string]any{"name": "x", "url": "ftp://a", "events": []string{"user.signed_in"}}, nil); resp.StatusCode != 400 {
		t.Fatalf("ftp url: %d", resp.StatusCode)
	}
	if resp := e.owner.do("POST", "/integrations/webhooks", map[string]any{"name": "x", "url": rs.URL, "events": []string{"nope"}}, nil); resp.StatusCode != 400 {
		t.Fatalf("unknown event: %d", resp.StatusCode)
	}

	var wh webhookResp
	resp := e.owner.do("POST", "/integrations/webhooks", map[string]any{"name": "Script", "url": rs.URL + "/hook",
		"events": []string{"user.signed_in", "device.new", "item.watched"}, "headers": []map[string]string{{"name": "X-Extra", "value": "yes"}}}, &wh)
	if resp.StatusCode != 201 || !strings.HasPrefix(wh.Secret, "whsec_") || wh.Health.State != "never" {
		t.Fatalf("create: %d %+v", resp.StatusCode, wh)
	}

	// A new sign-in goes out, signed.
	other := &client{t: t, base: e.base}
	other.do("POST", "/auth/login", map[string]string{"username": "alex", "password": "password123", "deviceName": "Living room TV"}, nil)
	r, body := rc.wait(t, "user.signed_in")
	ts, _ := strconv.ParseInt(r.Header.Get("X-Kinora-Timestamp"), 10, 64)
	if r.Header.Get("X-Kinora-Signature") != signature(wh.Secret, ts, body) {
		t.Fatal("bad signature")
	}
	if r.Header.Get("X-Extra") != "yes" || r.Header.Get("Content-Type") != "application/json" ||
		!strings.HasPrefix(r.Header.Get("X-Kinora-Delivery"), "dlv_") {
		t.Fatalf("headers: %v", r.Header)
	}
	var env struct {
		ID, Event string
		Server    struct{ ID, Name string }
		Data      struct {
			Account struct{ Name string }
			Device  struct{ Name string }
		}
	}
	json.Unmarshal(body, &env)
	if env.ID != r.Header.Get("X-Kinora-Delivery") || env.Server.ID == "" || env.Data.Account.Name != "alex" || env.Data.Device.Name != "Living room TV" {
		t.Fatalf("envelope: %s", body)
	}
	// It was a device never seen before.
	rc.wait(t, "device.new")

	// Marked watched by hand.
	e.owner.do("POST", "/items/"+e.movie+"/watched", map[string]bool{"watched": true}, nil)
	_, body = rc.wait(t, "item.watched")
	var watched struct {
		Data struct {
			Title struct {
				Kind  string
				Title string
				IDs   struct{ TMDB int }
				Path  string
			}
			ByHand bool
		}
	}
	json.Unmarshal(body, &watched)
	if watched.Data.Title.Title != "Masterplan" || watched.Data.Title.IDs.TMDB != 1 || !watched.Data.ByHand || watched.Data.Title.Path != "" {
		t.Fatalf("watched: %s", body)
	}

	// The log and health.
	var list struct{ Items []webhookResp }
	for range 50 {
		e.owner.do("GET", "/integrations/webhooks", nil, &list)
		if list.Items[0].Health.State == "ok" {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if list.Items[0].Health.State != "ok" {
		t.Fatalf("health: %+v", list.Items[0])
	}
	var dels struct {
		Items []struct {
			ID      int64
			Event   string
			Status  *int
			Request string
		}
	}
	e.owner.do("GET", "/integrations/webhooks/"+strconv.FormatInt(wh.ID, 10)+"/deliveries", nil, &dels)
	if len(dels.Items) < 3 || dels.Items[0].Status == nil || *dels.Items[0].Status != 200 || dels.Items[0].Request == "" {
		t.Fatalf("deliveries: %+v", dels)
	}

	// Test and send again answer with the result.
	var d struct {
		Event    string
		Status   *int
		Response *string
	}
	e.owner.do("POST", "/integrations/webhooks/"+strconv.FormatInt(wh.ID, 10)+"/test", nil, &d)
	if d.Event != "test" || d.Status == nil || *d.Status != 200 || d.Response == nil || *d.Response != `{"ok":true}` {
		t.Fatalf("test: %+v", d)
	}
	e.owner.do("POST", "/integrations/webhooks/"+strconv.FormatInt(wh.ID, 10)+"/deliveries/"+strconv.FormatInt(dels.Items[0].ID, 10)+"/redeliver", nil, &d)
	if d.Status == nil || *d.Status != 200 {
		t.Fatalf("redeliver: %+v", d)
	}

	// A new secret.
	var sec struct{ Secret string }
	e.owner.do("POST", "/integrations/webhooks/"+strconv.FormatInt(wh.ID, 10)+"/secret", nil, &sec)
	if sec.Secret == wh.Secret || !strings.HasPrefix(sec.Secret, "whsec_") {
		t.Fatalf("secret: %+v", sec)
	}

	// Pausing is a partial update.
	var paused webhookResp
	e.owner.do("PUT", "/integrations/webhooks/"+strconv.FormatInt(wh.ID, 10), map[string]any{"enabled": false}, &paused)
	if paused.Health.State != "paused" || len(paused.Events) != 3 {
		t.Fatalf("paused: %+v", paused)
	}
	if resp := e.owner.do("DELETE", "/integrations/webhooks/"+strconv.FormatInt(wh.ID, 10), nil, nil); resp.StatusCode != 204 {
		t.Fatalf("delete: %d", resp.StatusCode)
	}
}

func TestWebhookRetryAndRedirect(t *testing.T) {
	e := integServer(t)
	rc := &receiver{fail: 1}
	rs := httptest.NewServer(rc)
	t.Cleanup(rs.Close)
	var wh webhookResp
	e.owner.do("POST", "/integrations/webhooks", map[string]any{"name": "Flaky", "url": rs.URL, "events": []string{"server.started", "user.signed_in"}, "retries": 3}, &wh)
	e.srv.emit("user.signed_in", eventScope{}, map[string]string{"n": "1"})
	rc.wait(t, "user.signed_in")
	// The failure stays at the head of the queue for a retry in a minute.
	var heads []store.QueuedDelivery
	for range 100 {
		heads, _ = e.st.QueueHeads(context.Background())
		if len(heads) == 1 && heads[0].Attempts == 1 {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if len(heads) != 1 || heads[0].Attempts != 1 || heads[0].NextAt < time.Now().Add(50*time.Second).Unix() {
		t.Fatalf("queue after a failure: %+v", heads)
	}
	// Later events wait behind it, in order.
	e.srv.emit("user.signed_in", eventScope{}, map[string]string{"n": "2"})
	time.Sleep(200 * time.Millisecond)
	rc.mu.Lock()
	n := len(rc.got)
	rc.mu.Unlock()
	if n != 1 {
		t.Fatalf("sent %d, want 1 (the second waits behind the retry)", n)
	}

	// Redirects to another host are not followed.
	other := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Error("followed a redirect to another host")
	}))
	t.Cleanup(other.Close)
	redir := httptest.NewServer(http.RedirectHandler(other.URL+"/x", http.StatusTemporaryRedirect))
	t.Cleanup(redir.Close)
	var wh2 webhookResp
	e.owner.do("POST", "/integrations/webhooks", map[string]any{"name": "Moved", "url": redir.URL, "events": []string{"user.signed_in"}}, &wh2)
	var d struct {
		Status *int
		Error  string
	}
	e.owner.do("POST", "/integrations/webhooks/"+strconv.FormatInt(wh2.ID, 10)+"/test", nil, &d)
	if d.Status == nil || *d.Status != 307 || !strings.Contains(d.Error, "Redirected") {
		t.Fatalf("redirect: %+v", d)
	}
}

type hookResp struct {
	ID    int64
	Token string
	Path  string
}

type statusList struct {
	Items []struct {
		ID       int64
		Kind     string
		TMDBID   int `json:"tmdbId"`
		Title    string
		State    string
		Progress *float64
		Source   string
		Item     *struct{ ID string }
		Season   *int
	}
}

func TestIncomingHooks(t *testing.T) {
	e := integServer(t)
	rc := &receiver{}
	rs := httptest.NewServer(rc)
	t.Cleanup(rs.Close)
	e.owner.do("POST", "/integrations/webhooks", map[string]any{"name": "Watcher", "url": rs.URL, "events": []string{"status.updated", "status.done"}}, nil)

	var hook hookResp
	e.owner.do("POST", "/integrations/incoming", map[string]any{"name": "NAS sync", "visibility": "everyone"}, &hook)
	if !strings.HasPrefix(hook.Token, "in_") || hook.Path != "/api/v1/hooks/in/"+hook.Token {
		t.Fatalf("hook: %+v", hook)
	}
	anon := &client{t: t, base: e.base}
	var res map[string]any
	resp := anon.do("POST", "/hooks/in/"+hook.Token, map[string]any{"tmdbId": 1, "kind": "movie", "state": "in_progress", "progress": 0.45, "message": "Copying"}, &res)
	if resp.StatusCode != 202 || res["matched"].(map[string]any)["title"] != "Masterplan" {
		t.Fatalf("matched: %d %v", resp.StatusCode, res)
	}
	_, body := rc.wait(t, "status.updated")
	if !strings.Contains(string(body), `"progress":0.45`) || !strings.Contains(string(body), `"hook":"NAS sync"`) {
		t.Fatalf("status.updated: %s", body)
	}

	// Unknown to TMDB: kept, not matched.
	resp = anon.do("POST", "/hooks/in/"+hook.Token, map[string]any{"tmdbId": 99999999, "kind": "movie", "state": "queued"}, &res)
	if resp.StatusCode != 200 || res["matched"] != nil || res["kept"] != true {
		t.Fatalf("unmatched: %d %v", resp.StatusCode, res)
	}
	// A show found through TMDB.
	resp = anon.do("POST", "/hooks/in/"+hook.Token, map[string]any{"tmdbId": 2, "kind": "show", "season": 1, "episode": 1, "state": "queued"}, &res)
	if resp.StatusCode != 202 || res["matched"].(map[string]any)["title"] != "Tulsa King" {
		t.Fatalf("show: %d %v", resp.StatusCode, res)
	}

	// Errors.
	if resp := anon.do("POST", "/hooks/in/in_nope", map[string]any{"tmdbId": 1}, nil); resp.StatusCode != 404 {
		t.Fatalf("bad token: %d", resp.StatusCode)
	}
	req, _ := http.NewRequest("POST", e.base+"/hooks/in/"+hook.Token, strings.NewReader("{oops"))
	if resp, _ := http.DefaultClient.Do(req); resp.StatusCode != 400 {
		t.Fatalf("bad json: %d", resp.StatusCode)
	}
	if resp := anon.do("POST", "/hooks/in/"+hook.Token, map[string]any{"tmdbId": 77, "state": "queued"}, nil); resp.StatusCode != 400 {
		t.Fatalf("kind missing for an unknown title: %d", resp.StatusCode)
	}

	// On the way, with the library's copy.
	var list statusList
	e.owner.do("GET", "/ontheway", nil, &list)
	if len(list.Items) != 3 {
		t.Fatalf("on the way: %+v", list)
	}
	var mp *float64
	for _, it := range list.Items {
		if it.TMDBID == 1 {
			mp = it.Progress
			if it.Item == nil || it.Item.ID != e.movie || it.Source != "NAS sync" {
				t.Fatalf("masterplan: %+v", it)
			}
		}
	}
	if mp == nil || *mp != 0.45 {
		t.Fatal("progress missing")
	}
	alex, _ := e.st.UserByName(context.Background(), "alex")
	if ts := e.srv.titleStatus("movie", 1, alex); ts == nil || ts.State != "in_progress" {
		t.Fatalf("titleStatus: %+v", ts)
	}

	// Too many updates for one title.
	got429 := false
	for range 3 {
		resp := anon.do("POST", "/hooks/in/"+hook.Token, map[string]any{"tmdbId": 1, "kind": "movie", "state": "in_progress", "progress": 0.5}, nil)
		if resp.StatusCode == 429 {
			got429 = resp.Header.Get("Retry-After") != ""
		}
	}
	if !got429 {
		t.Fatal("no 429 with Retry-After")
	}
	time.Sleep(1100 * time.Millisecond)

	// Ready: cleared, status.done sent, its library scanned.
	resp = anon.do("POST", "/hooks/in/"+hook.Token, map[string]any{"tmdbId": 1, "kind": "movie", "state": "done"}, &res)
	if resp.StatusCode != 202 || res["scan"] != "started" {
		t.Fatalf("done: %d %v", resp.StatusCode, res)
	}
	_, body = rc.wait(t, "status.done")
	if !strings.Contains(string(body), `"scan":"started"`) {
		t.Fatalf("status.done: %s", body)
	}
	if ts := e.srv.titleStatus("movie", 1, alex); ts != nil {
		t.Fatal("done clears the status")
	}

	// The received log.
	var rec struct {
		Items []struct {
			OK      bool
			Summary string
		}
	}
	e.owner.do("GET", "/integrations/incoming/"+strconv.FormatInt(hook.ID, 10)+"/received", nil, &rec)
	if len(rec.Items) < 5 || !rec.Items[0].OK || rec.Items[len(rec.Items)-1].Summary != "Masterplan · in progress 45%" {
		t.Fatalf("received: %+v", rec.Items)
	}

	// A tool's own JSON.
	var custom hookResp
	resp = e.owner.do("POST", "/integrations/incoming", map[string]any{"name": "Transfer jobs", "format": "custom", "progressScale": "percent",
		"mapping":  map[string]string{"tmdbId": "item.ids.tmdb", "kind": "item.type", "state": "job.state", "progress": "job.percent"},
		"stateMap": []map[string]string{{"from": "running", "to": "in_progress"}}}, &custom)
	if resp.StatusCode != 201 {
		t.Fatalf("custom hook: %d", resp.StatusCode)
	}
	resp = anon.do("POST", "/hooks/in/"+custom.Token, map[string]any{"job": map[string]any{"state": "running", "percent": 72},
		"item": map[string]any{"type": "tv", "ids": map[string]any{"tmdb": 2}}}, &res)
	if resp.StatusCode != 202 || res["matched"].(map[string]any)["title"] != "Tulsa King" {
		t.Fatalf("custom: %d %v", resp.StatusCode, res)
	}

	// Owners-only statuses are hidden from others.
	e.owner.do("PUT", "/integrations/incoming/"+strconv.FormatInt(hook.ID, 10), map[string]any{"visibility": "owners"}, nil)
	sam := store.User{ID: 99}
	if ts := e.srv.titleStatus("show", 2, sam); ts != nil {
		t.Fatalf("owners-only status shown to others: %+v", ts)
	}
	if ts := e.srv.titleStatus("show", 2, alex); ts == nil {
		t.Fatal("owners see it")
	}

	// Expired statuses go.
	past := store.TitleStatus{Source: "hook:1", SourceName: "x", Ident: "old", Kind: "movie", TMDBID: 5, Title: "Old", State: "queued",
		Season: -1, Episode: -1, UpdatedAt: 1, ExpiresAt: time.Now().Add(-time.Minute).Unix()}
	if err := e.st.PutStatus(context.Background(), &past); err != nil {
		t.Fatal(err)
	}
	e.owner.do("GET", "/ontheway", nil, &list)
	for _, it := range list.Items {
		if it.Title == "Old" {
			t.Fatal("an expired status is listed")
		}
		if resp := e.owner.do("DELETE", "/ontheway/"+strconv.FormatInt(it.ID, 10), nil, nil); resp.StatusCode != 204 {
			t.Fatalf("clear: %d", resp.StatusCode)
		}
	}
	e.owner.do("GET", "/ontheway", nil, &list)
	if len(list.Items) != 0 {
		t.Fatalf("cleared: %+v", list.Items)
	}

	// A new address; the old one stops working.
	var rotated hookResp
	e.owner.do("POST", "/integrations/incoming/"+strconv.FormatInt(hook.ID, 10)+"/token", nil, &rotated)
	if resp := anon.do("POST", "/hooks/in/"+hook.Token, map[string]any{"tmdbId": 1, "kind": "movie", "state": "queued"}, nil); resp.StatusCode != 404 || rotated.Token == hook.Token {
		t.Fatalf("old address: %d", resp.StatusCode)
	}
}

func TestAPIKeys(t *testing.T) {
	e := integServer(t)
	var key struct {
		ID     int64
		Key    string
		Prefix string
	}
	resp := e.owner.do("POST", "/integrations/keys", map[string]any{"name": "Dashboard", "scopes": []string{"library:read", "status:write"}, "network": "local"}, &key)
	if resp.StatusCode != 201 || !strings.HasPrefix(key.Key, "kn_live_") || len(key.Key) != 40 || key.Prefix != key.Key[:12] {
		t.Fatalf("create: %d %+v", resp.StatusCode, key)
	}
	k := &client{t: t, base: e.base, token: key.Key}
	if resp := k.do("GET", "/items", nil, nil); resp.StatusCode != 200 {
		t.Fatalf("items with a key: %d", resp.StatusCode)
	}
	for _, c := range []struct{ method, path string }{
		{"POST", "/libraries/" + strconv.FormatInt(e.lib.ID, 10) + "/scan"}, // no library:scan
		{"GET", "/users"}, {"GET", "/settings"}, {"PATCH", "/libraries/" + strconv.FormatInt(e.lib.ID, 10)},
		{"GET", "/integrations/keys"}, {"GET", "/me"},
	} {
		if resp := k.do(c.method, c.path, map[string]any{}, nil); resp.StatusCode != 403 {
			t.Errorf("%s %s with a key: %d, want 403", c.method, c.path, resp.StatusCode)
		}
	}
	var res map[string]any
	if resp := k.do("POST", "/status", map[string]any{"title": "Masterplan", "year": 0, "state": "queued"}, &res); resp.StatusCode != 202 {
		t.Fatalf("status with a key: %d %v", resp.StatusCode, res)
	}

	// A home network key, from outside.
	req := httptest.NewRequest("GET", "/api/v1/items", nil)
	req.RemoteAddr = "203.0.113.9:5000"
	req.Header.Set("Authorization", "Bearer "+key.Key)
	w := httptest.NewRecorder()
	e.h.ServeHTTP(w, req)
	if w.Code != 403 {
		t.Fatalf("local key from outside: %d", w.Code)
	}
	var anyKey struct{ Key string }
	e.owner.do("POST", "/integrations/keys", map[string]any{"name": "Scan", "scopes": []string{"library:scan"}, "network": "any", "expiresDays": 30}, &anyKey)
	req = httptest.NewRequest("POST", "/api/v1/libraries/"+strconv.FormatInt(e.lib.ID, 10)+"/scan?path=../../etc", nil)
	req.RemoteAddr = "203.0.113.9:5000"
	req.Header.Set("Authorization", "Bearer "+anyKey.Key)
	w = httptest.NewRecorder()
	e.h.ServeHTTP(w, req)
	if w.Code != 400 {
		t.Fatalf("scan outside the library: %d %s", w.Code, w.Body)
	}

	var list struct {
		Items []struct {
			Prefix     string
			Key        string
			LastUsedAt int64
			ExpiresAt  int64
		}
	}
	e.owner.do("GET", "/integrations/keys", nil, &list)
	if len(list.Items) != 2 || list.Items[0].Key != "" || list.Items[0].LastUsedAt == 0 || list.Items[1].ExpiresAt == 0 {
		t.Fatalf("list: %+v", list)
	}
	e.owner.do("DELETE", "/integrations/keys/"+strconv.FormatInt(key.ID, 10), nil, nil)
	if resp := k.do("GET", "/items", nil, nil); resp.StatusCode != 401 {
		t.Fatalf("revoked key: %d", resp.StatusCode)
	}
	if resp := k.do("GET", "/integrations/events", nil, nil); resp.StatusCode != 401 {
		t.Fatalf("revoked key: %d", resp.StatusCode)
	}
}

func TestFolderScanAndLibraryEvents(t *testing.T) {
	e := integServer(t)
	rc := &receiver{}
	rs := httptest.NewServer(rc)
	t.Cleanup(rs.Close)
	e.owner.do("POST", "/integrations/webhooks", map[string]any{"name": "Lib", "url": rs.URL,
		"events": []string{"library.scan_finished", "library.item_removed"}, "includePaths": true}, nil)
	// The library folder exists but is empty: Masterplan's file is gone.
	dir := e.lib.Path
	if err := os.MkdirAll(filepath.Join(dir, "Sub"), 0o755); err != nil {
		t.Fatal(err)
	}
	resp := e.owner.do("POST", "/libraries/"+strconv.FormatInt(e.lib.ID, 10)+"/scan?path=Sub", nil, nil)
	if resp.StatusCode != 202 {
		t.Fatalf("folder scan: %d", resp.StatusCode)
	}
	_, body := rc.wait(t, "library.scan_finished")
	if !strings.Contains(string(body), `"folder":`) || !strings.Contains(string(body), `"removed":0`) {
		t.Fatalf("folder scan finished: %s", body)
	}
	for range 50 {
		if p, _ := e.srv.Scanner.Progress(e.lib.ID); !p.Running {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	// A whole scan notices the missing file (seen in an earlier second).
	time.Sleep(1100 * time.Millisecond)
	e.owner.do("POST", "/libraries/"+strconv.FormatInt(e.lib.ID, 10)+"/scan", nil, nil)
	_, body = rc.wait(t, "library.item_removed")
	if !strings.Contains(string(body), `"title":"Masterplan"`) || !strings.Contains(string(body), `"path":`) {
		t.Fatalf("removed: %s", body)
	}
}

func TestPlaybackEvents(t *testing.T) {
	e := integServer(t)
	rc := &receiver{}
	rs := httptest.NewServer(rc)
	t.Cleanup(rs.Close)
	e.owner.do("POST", "/integrations/webhooks", map[string]any{"name": "Lights", "url": rs.URL,
		"events": []string{"playback.started", "playback.stopped"}}, nil)
	e.owner.do("PUT", "/progress/"+e.movie, map[string]any{"positionSec": 10}, nil)
	_, body := rc.wait(t, "playback.started")
	if !strings.Contains(string(body), `"title":"Masterplan"`) || !strings.Contains(string(body), `"account":{"id":1,"name":"alex"`) {
		t.Fatalf("started: %s", body)
	}
	// No report for a while: stopped.
	e.srv.sweepPlays(time.Now().Add(time.Minute))
	_, body = rc.wait(t, "playback.stopped")
	if !strings.Contains(string(body), `"positionSec":10`) {
		t.Fatalf("stopped: %s", body)
	}
}
