//go:build !nosqlite

package api

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"kinara/internal/auth"
	"kinara/internal/images"
	"kinara/internal/probe"
	"kinara/internal/scanner"
	"kinara/internal/store"
	"kinara/internal/watch"
)

// remoteServer is featureServer where a request with X-Test-Remote comes
// from that address (as the internet would), for the rules that depend on it.
func remoteServer(t *testing.T) (*client, func(user, pass string) *client) {
	t.Helper()
	ctx := context.Background()
	dir := t.TempDir()
	st, err := store.Open(filepath.Join(dir, "kinara.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	hash, _ := auth.HashPassword("password123")
	if _, err := st.CreateUser(ctx, "alex", hash, true); err != nil {
		t.Fatal(err)
	}
	srv := &Server{Store: st, Scanner: &scanner.Scanner{Store: st, Prober: &probe.Prober{}, Log: log},
		Images: images.New(filepath.Join(dir, "img")), Watch: &watch.Service{Store: st}, Log: log, Version: "test", BaseCtx: ctx}
	h := srv.Handler()
	hs := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if a := r.Header.Get("X-Test-Remote"); a != "" {
			r.RemoteAddr = a + ":40000"
		}
		h.ServeHTTP(w, r)
	}))
	t.Cleanup(hs.Close)
	signIn := func(user, pass string) *client {
		c := &client{t: t, base: hs.URL + "/api/v1"}
		var login struct{ Token string }
		if resp := c.do("POST", "/auth/login", map[string]string{"username": user, "password": pass}, &login); resp.StatusCode != 200 {
			t.Fatalf("sign in %s: %d", user, resp.StatusCode)
		}
		c.token = login.Token
		return c
	}
	return signIn("alex", "password123"), signIn
}

// M2: a PIN-protected profile that isn't in use can't be changed or
// removed on the admin account without its PIN or the account password.
func TestProtectedProfileNeedsPIN(t *testing.T) {
	_, c, _ := featureServer(t)
	var parent store.Profile
	if resp := c.do("POST", "/profiles", map[string]any{"name": "Parent", "pin": "4321"}, &parent); resp.StatusCode != 201 {
		t.Fatalf("create: %d", resp.StatusCode)
	}
	path := "/profiles/" + itoa(parent.ID)
	if resp := c.do("PATCH", path, map[string]any{"pin": ""}, nil); resp.StatusCode != 403 {
		t.Fatalf("PIN removed without it: %d", resp.StatusCode)
	}
	if resp := c.do("DELETE", path, nil, nil); resp.StatusCode != 403 {
		t.Fatalf("deleted without the PIN: %d", resp.StatusCode)
	}
	if resp := c.do("PATCH", path, map[string]any{"name": "X", "currentPin": "0000"}, nil); resp.StatusCode != 403 {
		t.Fatalf("wrong PIN accepted: %d", resp.StatusCode)
	}
	if resp := c.do("PATCH", path, map[string]any{"name": "Mum", "currentPin": "4321"}, nil); resp.StatusCode != 200 {
		t.Fatalf("right PIN refused: %d", resp.StatusCode)
	}
	if resp := c.do("PATCH", path, map[string]any{"pin": "", "password": "password123"}, nil); resp.StatusCode != 200 {
		t.Fatalf("account password refused: %d", resp.StatusCode)
	}
}

// M5: a user name's failures from the internet don't lock out sign-ins
// from the home network.
func TestLoginLockoutOnlyFromInternet(t *testing.T) {
	c, signIn := remoteServer(t)
	away := &client{t: t, base: c.base, http: &http.Client{Transport: forwardedFor("203.0.113.9")}}
	for i := 0; i < 12; i++ {
		away.do("POST", "/auth/login", map[string]string{"username": "alex", "password": "wrong"}, nil)
	}
	// From home (the test client is on 127.0.0.1): still fine.
	signIn("alex", "password123")
}

// forwardedFor pretends requests come from addr (see remoteServer).
type forwardedFor string

func (f forwardedFor) RoundTrip(r *http.Request) (*http.Response, error) {
	r.Header.Set("X-Test-Remote", string(f))
	return http.DefaultTransport.RoundTrip(r)
}

// M4: the limiter forgets old keys and doesn't grow without end.
func TestLimiterBounded(t *testing.T) {
	l := &auth.Limiter{Max: 3, Window: time.Minute}
	for i := 0; i < 50000; i++ {
		l.Fail(auth.Key("user:", strings.Repeat("x", 100)+itoa(int64(i))))
	}
	if err := l.Allow("user:fresh"); err != nil {
		t.Fatal(err)
	}
	if n := l.Len(); n > 20000 {
		t.Fatalf("limiter holds %d keys", n)
	}
}

// M3, M12: the public health answer leaves out the node ID, the computer's
// name and database errors.
func TestHealthFromInternet(t *testing.T) {
	c, _ := remoteServer(t)
	var home map[string]any
	c.do("GET", "/health", nil, &home)
	if _, ok := home["name"]; !ok {
		t.Fatalf("home answer has no name: %v", home)
	}
	away := &client{t: t, base: c.base, http: &http.Client{Transport: forwardedFor("203.0.113.9")}}
	var pub map[string]any
	away.do("GET", "/health", nil, &pub)
	if _, ok := pub["name"]; ok {
		t.Fatalf("public answer has the name: %v", pub)
	}
}

// M11: a change carried by the session cookie from another site is refused.
func TestCookieCrossSite(t *testing.T) {
	ok := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {})
	h := sameOrigin(ok)
	for _, c := range []struct {
		method, site, origin string
		cookie               bool
		want                 int
	}{
		{"POST", "cross-site", "", true, 403},
		{"POST", "", "https://evil.example", true, 403},
		{"POST", "same-origin", "", true, 200},
		{"POST", "", "", true, 200},
		{"GET", "cross-site", "", true, 200},
		{"POST", "cross-site", "", false, 200},
	} {
		r := httptest.NewRequest(c.method, "http://kinara.local/ui/libraries", nil)
		if c.site != "" {
			r.Header.Set("Sec-Fetch-Site", c.site)
		}
		if c.origin != "" {
			r.Header.Set("Origin", c.origin)
		}
		if c.cookie {
			r.AddCookie(&http.Cookie{Name: cookieName, Value: "t"})
		}
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		if w.Code != c.want {
			t.Errorf("%+v: %d", c, w.Code)
		}
	}
}

// M20: changing your own password keeps this session and signs out the rest.
func TestOwnPasswordKeepsSession(t *testing.T) {
	_, c, signIn := featureServer(t)
	other := signIn("alex", "password123")
	var me struct{ ID int64 }
	c.do("GET", "/me", nil, &me)
	if resp := c.do("PATCH", "/users/"+itoa(me.ID), map[string]any{"password": "newpassword456"}, nil); resp.StatusCode != 204 {
		t.Fatalf("change: %d", resp.StatusCode)
	}
	if resp := c.do("GET", "/me", nil, nil); resp.StatusCode != 200 {
		t.Fatalf("this session after its own change: %d", resp.StatusCode)
	}
	if resp := other.do("GET", "/me", nil, nil); resp.StatusCode != 401 {
		t.Fatalf("another session after the change: %d", resp.StatusCode)
	}
}
