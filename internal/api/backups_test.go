//go:build !nosqlite

package api

import (
	"bytes"
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"kinara/internal/auth"
	"kinara/internal/backup"
	"kinara/internal/images"
	"kinara/internal/store"
	"kinara/internal/watch"
)

func TestBackupEndpoints(t *testing.T) {
	ctx := context.Background()
	data := t.TempDir()
	st, err := store.Open(filepath.Join(data, "kinara.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	hash, _ := auth.HashPassword("password123")
	st.CreateUser(ctx, "alex", hash, true)
	st.CreateUser(ctx, "sam", hash, false)
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	restarts := make(chan string, 1)
	srv := &Server{Store: st, Images: images.New(filepath.Join(data, "img")), Watch: &watch.Service{Store: st}, Log: log,
		Version: "0.3.0", BaseCtx: ctx, DataDir: data,
		Backups: &backup.Manager{Store: st, DataDir: data, Version: "0.3.0"},
		Restart: func(reason string) { restarts <- reason }}
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

	if resp := other.do("GET", "/backups", nil, nil); resp.StatusCode != http.StatusForbidden {
		t.Fatalf("non-owner: %d", resp.StatusCode)
	}
	var view backupsView
	owner.do("GET", "/backups", nil, &view)
	if !view.Settings.Auto || view.Settings.Keep != 7 || len(view.Backups) != 0 || view.NextAt == 0 {
		t.Fatalf("defaults %+v", view)
	}

	var made backup.Info
	if resp := owner.do("POST", "/backups", nil, &made); resp.StatusCode != http.StatusCreated {
		t.Fatalf("create: %d", resp.StatusCode)
	}
	owner.do("GET", "/backups", nil, &view)
	if len(view.Backups) != 1 || view.Backups[0].Name != made.Name {
		t.Fatalf("list %+v", view.Backups)
	}

	// Download with ?token=, as a browser link does.
	resp, err := http.Get(hs.URL + "/api/v1/backups/" + made.Name + "?token=" + owner.token)
	if err != nil {
		t.Fatal(err)
	}
	zipBytes, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != 200 || len(zipBytes) != int(made.Size) {
		t.Fatalf("download: %d, %d bytes", resp.StatusCode, len(zipBytes))
	}
	if resp, _ := http.Get(hs.URL + "/api/v1/backups/" + made.Name + "?token=" + other.token); resp.StatusCode != http.StatusForbidden {
		t.Fatalf("non-owner download: %d", resp.StatusCode)
	}
	if resp := owner.do("GET", "/backups/..%2fkinara.db", nil, nil); resp.StatusCode != http.StatusNotFound {
		t.Fatalf("path escape: %d", resp.StatusCode)
	}

	// Settings: a folder elsewhere, checked for writing.
	elsewhere := filepath.Join(t.TempDir(), "nas")
	var set store.BackupSettings
	if resp := owner.do("PUT", "/backups/settings", map[string]any{"dir": elsewhere, "keep": 3, "auto": true}, &set); resp.StatusCode != 200 || set.Dir != elsewhere {
		t.Fatalf("settings: %d %+v", resp.StatusCode, set)
	}
	if resp := owner.do("PUT", "/backups/settings", map[string]any{"dir": "relative/path"}, nil); resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("relative folder accepted: %d", resp.StatusCode)
	}
	if resp := owner.do("PUT", "/backups/settings", map[string]any{"keep": 0}, nil); resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("keep 0 accepted: %d", resp.StatusCode)
	}

	// Restore an upload: checked, a backup of now is made, staged, restart.
	req, _ := http.NewRequest("POST", hs.URL+"/api/v1/backups/restore", bytes.NewReader([]byte("not a zip")))
	req.Header.Set("Authorization", "Bearer "+owner.token)
	if resp, _ := http.DefaultClient.Do(req); resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("junk upload: %d", resp.StatusCode)
	}
	req, _ = http.NewRequest("POST", hs.URL+"/api/v1/backups/restore", bytes.NewReader(zipBytes))
	req.Header.Set("Authorization", "Bearer "+owner.token)
	resp, _ = http.DefaultClient.Do(req)
	if resp.StatusCode != http.StatusAccepted {
		b, _ := io.ReadAll(resp.Body)
		t.Fatalf("restore: %d %s", resp.StatusCode, b)
	}
	select {
	case <-restarts:
	case <-time.After(3 * time.Second):
		t.Fatal("no restart")
	}
	if !backup.Pending(data) {
		t.Fatal("restore not staged")
	}
	list, _ := backup.List(elsewhere)
	if len(list) != 1 || list[0].Reason != backup.ReasonBeforeRestore {
		t.Fatalf("no before-restore backup: %+v", list)
	}
	// One at a time.
	if resp := owner.do("POST", "/backups/"+list[0].Name+"/restore", nil, nil); resp.StatusCode != http.StatusConflict {
		t.Fatalf("second restore: %d", resp.StatusCode)
	}
	os.Remove(filepath.Join(data, "restore-pending.zip"))
}
