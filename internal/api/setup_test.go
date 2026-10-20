//go:build !nosqlite

package api

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"

	"kinara/internal/store"
)

// A fresh server: the first account is made through /setup and is the
// owner; after that /setup refuses.
func TestFirstRunSetup(t *testing.T) {
	st, err := store.Open(filepath.Join(t.TempDir(), "kinara.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	srv := &Server{Store: st, Log: log, Version: "test", BaseCtx: context.Background(), WebDir: t.TempDir()}
	hs := httptest.NewServer(srv.Handler())
	t.Cleanup(hs.Close)
	anon := &client{t: t, base: hs.URL + "/api/v1"}

	var health struct{ SetupDone bool }
	anon.do("GET", "/health", nil, &health)
	if health.SetupDone {
		t.Fatal("a fresh server isn't set up")
	}
	if resp := anon.do("POST", "/setup", map[string]string{"username": "alex", "password": "short"}, nil); resp.StatusCode != 400 {
		t.Fatalf("short password: %d", resp.StatusCode)
	}
	var made struct {
		Token    string
		User     store.User
		Profiles []store.Profile
	}
	if resp := anon.do("POST", "/setup", map[string]string{"username": "alex", "password": "password123", "deviceName": "Web browser"}, &made); resp.StatusCode != 201 {
		t.Fatalf("setup: %d", resp.StatusCode)
	}
	if made.Token == "" || !made.User.IsAdmin || made.User.Username != "alex" {
		t.Fatalf("setup answer = %+v", made)
	}
	anon.do("GET", "/health", nil, &health)
	if !health.SetupDone {
		t.Fatal("set up now")
	}
	if resp := anon.do("POST", "/setup", map[string]string{"username": "mallory", "password": "password123"}, nil); resp.StatusCode != 409 {
		t.Fatalf("a second owner: %d", resp.StatusCode)
	}
	owner := &client{t: t, base: anon.base, token: made.Token}
	if resp := owner.do("GET", "/libraries", nil, nil); resp.StatusCode != 200 {
		t.Fatalf("the new owner is signed in: %d", resp.StatusCode)
	}

	// With the web app installed, / goes to it.
	noFollow := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	resp, err := noFollow.Get(hs.URL + "/")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusFound || resp.Header.Get("Location") != "/app/" {
		t.Fatalf("/ = %d %q", resp.StatusCode, resp.Header.Get("Location"))
	}
}
