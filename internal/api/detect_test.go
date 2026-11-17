//go:build !nosqlite

package api

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"kinara/internal/arr"
	"kinara/internal/auth"
	"kinara/internal/images"
	"kinara/internal/store"
	"kinara/internal/watch"
)

// titled serves the app's web page at / (what Identify reads), and the
// fake's API everywhere else.
func titled(name string, api http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/" {
			io.WriteString(w, "<!doctype html><html><head><title>"+name+"</title></head></html>")
			return
		}
		api(w, r)
	}
}

func TestDetectApps(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	st, err := store.Open(filepath.Join(dir, "kinara.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	hash, _ := auth.HashPassword("password123")
	st.CreateUser(ctx, "alex", hash, true)

	// Radarr installed here: its key is in its settings file. Sonarr's
	// isn't anywhere Kinara can read. No Prowlarr. qBittorrent lets
	// localhost in without a password.
	radarr := &fakeService{app: "Radarr", key: "rk", have: map[string][]map[string]any{"rootfolder": {{"path": "D:/Movies"}}}}
	// Sonarr is new: no folder for new titles yet, so it gets Kinara's TV library.
	sonarr := &fakeService{app: "Sonarr", key: "sk", have: map[string][]map[string]any{}}
	st.CreateLibrary(ctx, "TV Shows", "D:/TV", store.KindTV)
	qb := &fakeQbitAny{user: "admin", pass: "x"}
	cfg := filepath.Join(dir, "radarr-config.xml")
	os.WriteFile(cfg, []byte("<Config>\n  <Port>7878</Port>\n  <ApiKey>rk</ApiKey>\n</Config>"), 0o644)
	oldPaths := arr.ConfigPaths
	arr.ConfigPaths = func(kind string) []string {
		if kind == arr.Radarr {
			return []string{cfg}
		}
		return []string{filepath.Join(dir, "none.xml")}
	}
	defer func() { arr.ConfigPaths = oldPaths }()
	closed := httptest.NewServer(http.NotFoundHandler())
	closed.Close()
	urls := map[int]string{8080: closed.URL, arr.PortProwlarr: closed.URL}
	for port, h := range map[int]http.HandlerFunc{arr.PortRadarr: titled("Radarr", radarr.handler), arr.PortSonarr: titled("Sonarr", sonarr.handler), arr.PortQbit: qb.handler} {
		ts := httptest.NewServer(h)
		defer ts.Close()
		urls[port] = ts.URL
	}
	old := stackURL
	stackURL = func(port int) string { return urls[port] }
	defer func() { stackURL = old }()

	srv := &Server{Store: st, Images: images.New(filepath.Join(dir, "img")), Watch: &watch.Service{Store: st},
		Log: slog.New(slog.NewTextHandler(io.Discard, nil)), Version: "test", BaseCtx: ctx}
	hs := httptest.NewServer(srv.Handler())
	defer hs.Close()
	owner := &client{t: t, base: hs.URL + "/api/v1"}
	var login struct{ Token string }
	owner.do("POST", "/auth/login", map[string]any{"username": "alex", "password": "password123"}, &login)
	owner.token = login.Token

	var det struct {
		Apps []foundApp
		Any  bool
	}
	owner.do("GET", "/arr/detect", nil, &det)
	by := map[string]foundApp{}
	for _, a := range det.Apps {
		by[a.Kind] = a
	}
	if !det.Any || !by["radarr"].Found || !by["radarr"].KeyFound || by["radarr"].Version != "1.0" {
		t.Fatalf("radarr: %+v", by["radarr"])
	}
	if !by["sonarr"].Found || by["sonarr"].KeyFound {
		t.Fatalf("sonarr: %+v", by["sonarr"])
	}
	if by["prowlarr"].Found || !by["qbit"].Found || !by["qbit"].KeyFound || by["qbit"].URL != urls[arr.PortQbit] {
		t.Fatalf("prowlarr, qbit: %+v %+v", by["prowlarr"], by["qbit"])
	}

	// A wrong Sonarr key: that step fails, the rest connect.
	var res struct {
		Steps []stepView
		OK    bool
	}
	owner.do("POST", "/arr/detect/connect", map[string]any{"keys": map[string]string{"sonarr": "nope"}}, &res)
	if res.OK || len(res.Steps) < 3 {
		t.Fatalf("wrong key: %+v", res)
	}
	owner.do("POST", "/arr/detect/connect", map[string]any{"keys": map[string]string{"sonarr": "sk"}}, &res)
	if !res.OK {
		t.Fatalf("connect: %+v", res)
	}
	a, _ := st.GetArrSettings(ctx)
	if !a.Radarr.Ready() || a.Radarr.APIKey != "rk" || a.Sonarr.APIKey != "sk" || a.Sonarr.RootFolder != "D:/TV" || a.Qbit.URL != urls[arr.PortQbit] || a.Prowlarr.URL != "" {
		t.Fatalf("saved: %+v", a)
	}
	// Linked: qBittorrent went into both.
	if len(sonarr.have["rootfolder"]) != 1 {
		t.Fatal("root folder not added to Sonarr")
	}
	if len(radarr.have["downloadclient"]) != 1 || len(sonarr.have["downloadclient"]) != 1 {
		t.Fatal("not linked")
	}
}
