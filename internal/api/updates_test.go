//go:build !nosqlite

package api

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"

	"kinara/internal/auth"
	"kinara/internal/images"
	"kinara/internal/store"
	"kinara/internal/update"
	"kinara/internal/watch"
)

func TestUpdateEndpoints(t *testing.T) {
	ctx := context.Background()
	site := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(update.Release{Version: "9.1.0", Date: "2026-10-20", Notes: "https://example.com/notes",
			Files: map[string]update.File{"linux-amd64-deb": {Name: "k.deb", URL: "http://x/k.deb", Size: 1, SHA256: "00"}}})
	}))
	defer site.Close()

	dir := t.TempDir()
	st, err := store.Open(filepath.Join(dir, "kinara.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	hash, _ := auth.HashPassword("password123")
	st.CreateUser(ctx, "alex", hash, true)
	st.CreateUser(ctx, "sam", hash, false)
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	srv := &Server{Store: st, Images: images.New(filepath.Join(dir, "img")), Watch: &watch.Service{Store: st}, Log: log,
		Version: "0.3.0", BaseCtx: ctx,
		Updater: &update.Updater{URL: site.URL, Current: "0.3.0", Dir: filepath.Join(dir, "updates"), Kind: update.KindDeb}}
	hs := httptest.NewServer(srv.Handler())
	defer hs.Close()
	signIn := func(name string) *client {
		c := &client{t: t, base: hs.URL + "/api/v1"}
		var login struct{ Token string }
		c.do("POST", "/auth/login", map[string]string{"username": name, "password": "password123"}, &login)
		c.token = login.Token
		return c
	}
	owner, other := signIn("alex"), signIn("sam")

	if resp := other.do("GET", "/update", nil, nil); resp.StatusCode != http.StatusForbidden {
		t.Fatalf("non-owner got %d", resp.StatusCode)
	}
	var s update.Status
	owner.do("GET", "/update", nil, &s)
	if s.Available || s.CheckedAt != 0 {
		t.Fatalf("before a check: %+v", s)
	}
	owner.do("POST", "/update/check", nil, &s)
	if !s.Available || s.Latest != "9.1.0" || s.CanInstall || s.How == "" {
		t.Fatalf("after a check: %+v", s)
	}
	// Remembered, so the webhook fires once per version.
	if v, _ := st.Value(ctx, "update_notified"); v != "9.1.0" {
		t.Fatalf("notified %q", v)
	}
	if resp := owner.do("POST", "/update/install", nil, nil); resp.StatusCode != http.StatusConflict {
		t.Fatalf("install on a deb: %d", resp.StatusCode)
	}

	var health map[string]any
	(&client{t: t, base: hs.URL + "/api/v1"}).do("GET", "/health", nil, &health)
	if health["update"] != "9.1.0" {
		t.Fatalf("health: %v", health)
	}

	// The owner can turn checking off; it's on by default.
	var set store.Settings
	owner.do("GET", "/settings", nil, &set)
	if !set.UpdateCheck {
		t.Fatal("update checks should be on by default")
	}
	owner.do("PUT", "/settings", map[string]any{"updateCheck": false}, &set)
	if set.UpdateCheck {
		t.Fatal("update checks didn't turn off")
	}
}
