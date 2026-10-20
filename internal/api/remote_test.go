//go:build !nosqlite

package api

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"kinara/internal/auth"
	"kinara/internal/images"
	"kinara/internal/probe"
	"kinara/internal/remote"
	"kinara/internal/scanner"
	"kinara/internal/store"
	"kinara/internal/watch"
)

// tunnelServer serves one handler on the home network (an httptest server)
// and on a tunnel listener, the way kinara-server does.
func tunnelServer(t *testing.T, users bool) (st *store.Store, lan string, viaTunnel func(node string) *client) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	dir := t.TempDir()
	st, err := store.Open(filepath.Join(dir, "kinara.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	if users {
		hash, _ := auth.HashPassword("password123")
		if _, err := st.CreateUser(ctx, "alex", hash, true); err != nil {
			t.Fatal(err)
		}
	}
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	srv := &Server{Store: st, Scanner: &scanner.Scanner{Store: st, Prober: &probe.Prober{}, Log: log},
		Images: images.New(filepath.Join(dir, "img")), Watch: &watch.Service{Store: st}, Log: log, Version: "test", BaseCtx: ctx}
	h := srv.Handler()
	hs := httptest.NewServer(h)
	t.Cleanup(hs.Close)

	ln, _ := net.Listen("tcp", "127.0.0.1:0")
	addr := ln.Addr().String()
	ln.Close()
	go remote.Serve(ctx, addr, "s3cret", h, log)
	for i := 0; i < 100; i++ {
		if c, err := net.Dial("tcp", addr); err == nil {
			c.Close()
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	viaTunnel = func(node string) *client {
		tr := &http.Transport{DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			c, err := (&net.Dialer{}).DialContext(ctx, "tcp", addr)
			if err != nil {
				return nil, err
			}
			_, err = c.Write(remote.RelayedHeader(node, "s3cret"))
			return c, err
		}}
		return &client{t: t, base: "http://tunnel/api/v1", http: &http.Client{Transport: tr}}
	}
	return st, hs.URL, viaTunnel
}

func node(n int) string { return fmt.Sprintf("%064x", n) }

func TestTunnelRules(t *testing.T) {
	ctx := context.Background()
	st, lan, viaTunnel := tunnelServer(t, true)
	if err := st.PutSettings(ctx, store.Settings{ListAccounts: true, AllowSignup: true}); err != nil {
		t.Fatal(err)
	}
	home := &client{t: t, base: lan + "/api/v1"}

	// The account picker and sign-up stay on the home network.
	var acc struct {
		Accounts    []any `json:"accounts"`
		AllowSignup bool  `json:"allowSignup"`
	}
	home.do("GET", "/auth/accounts", nil, &acc)
	if len(acc.Accounts) != 1 || !acc.AllowSignup {
		t.Fatalf("home accounts %+v", acc)
	}
	far := viaTunnel(node(1))
	far.do("GET", "/auth/accounts", nil, &acc)
	if len(acc.Accounts) != 0 || acc.AllowSignup {
		t.Fatalf("tunnel accounts %+v", acc)
	}
	if resp := far.do("POST", "/auth/signup", map[string]string{"username": "mallory", "password": "password123"}, nil); resp.StatusCode != 403 {
		t.Fatalf("tunnel signup: %d", resp.StatusCode)
	}

	// Signing in through the tunnel records the device's node ID.
	var login struct{ Token string }
	if resp := far.do("POST", "/auth/login", map[string]string{"username": "alex", "password": "password123", "deviceName": "Phone"}, &login); resp.StatusCode != 200 {
		t.Fatalf("tunnel sign in: %d", resp.StatusCode)
	}
	far.token = login.Token
	var me map[string]any
	if resp := far.do("GET", "/me", nil, &me); resp.StatusCode != 200 {
		t.Fatalf("me over tunnel: %d", resp.StatusCode)
	}
	home.do("POST", "/auth/login", map[string]string{"username": "alex", "password": "password123", "deviceName": "PC"}, &login)
	home.token = login.Token
	var devs []store.Device
	home.do("GET", "/devices", nil, &devs)
	var phone store.Device
	for _, d := range devs {
		if d.Name == "Phone" {
			phone = d
		}
	}
	if phone.NodeID != node(1) {
		t.Fatalf("devices %+v", devs)
	}

	// Without remote access available, the page says so and can't turn it on.
	var info struct {
		Available bool   `json:"available"`
		State     string `json:"state"`
	}
	home.do("GET", "/remote", nil, &info)
	if info.Available || info.State != remote.StateDisabled {
		t.Fatalf("remote info %+v", info)
	}
	if resp := home.do("PUT", "/remote/settings", map[string]any{"enabled": true}, nil); resp.StatusCode != 409 {
		t.Fatalf("enable without sidecar: %d", resp.StatusCode)
	}

	// Signing a device out with block=1 blocks its key.
	if resp := home.do("DELETE", "/devices/"+phone.ID+"?block=1", nil, nil); resp.StatusCode != 204 {
		t.Fatalf("sign out and block: %d", resp.StatusCode)
	}
	bl, _ := st.BlockedNodes(ctx)
	if len(bl) != 1 || bl[0].NodeID != node(1) || !strings.Contains(bl[0].Label, "Phone") {
		t.Fatalf("blocked %+v", bl)
	}
	if resp := home.do("POST", "/remote/unblock", map[string]string{"nodeId": node(1)}, nil); resp.StatusCode != 204 {
		t.Fatalf("unblock: %d", resp.StatusCode)
	}
	if resp := home.do("POST", "/remote/block", map[string]string{"nodeId": "nope"}, nil); resp.StatusCode != 400 {
		t.Fatalf("bad node id: %d", resp.StatusCode)
	}
	if bl, _ := st.BlockedNodes(ctx); len(bl) != 0 {
		t.Fatalf("still blocked %+v", bl)
	}
}

// Fresh node IDs and user names don't buy more remote password guesses.
func TestTunnelSignInLimit(t *testing.T) {
	_, lan, viaTunnel := tunnelServer(t, true)
	limited := 0
	for i := 0; i < 35; i++ {
		resp := viaTunnel(node(100+i)).do("POST", "/auth/login", map[string]string{"username": fmt.Sprintf("guess%d", i), "password": "x"}, nil)
		if resp.StatusCode == 429 {
			limited++
		}
	}
	if limited != 5 {
		t.Fatalf("%d of 35 remote guesses limited, want 5", limited)
	}
	// The home network isn't affected.
	home := &client{t: t, base: lan + "/api/v1"}
	if resp := home.do("POST", "/auth/login", map[string]string{"username": "alex", "password": "password123"}, nil); resp.StatusCode != 200 {
		t.Fatalf("home sign in after remote guessing: %d", resp.StatusCode)
	}
}

// The first-run page that makes the owner's account is never served remotely.
func TestTunnelNoRemoteSetup(t *testing.T) {
	_, lan, viaTunnel := tunnelServer(t, false)
	far := viaTunnel(node(7))
	far.base = "http://tunnel"
	if resp := far.do("GET", "/", nil, nil); resp.StatusCode != 403 {
		t.Fatalf("setup page over tunnel: %d", resp.StatusCode)
	}
	form := strings.NewReader("username=mallory&password=password123&confirm=password123")
	req, _ := http.NewRequest("POST", "http://tunnel/setup", form)
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	resp, err := far.http.Do(req)
	if err != nil || resp.StatusCode != 403 {
		t.Fatalf("setup over tunnel: %v %v", resp, err)
	}
	if r, err := http.Get(lan + "/"); err != nil || r.StatusCode != 200 {
		t.Fatalf("setup page at home: %v %v", r, err)
	}
}
