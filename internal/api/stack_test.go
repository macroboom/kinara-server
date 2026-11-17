//go:build !nosqlite

package api

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"kinara/internal/arr"
	"kinara/internal/auth"
	"kinara/internal/images"
	"kinara/internal/store"
	"kinara/internal/watch"
)

// fakeService is a Radarr, Sonarr or Prowlarr that remembers what's posted.
type fakeService struct {
	mu     sync.Mutex
	app    string
	key    string
	have   map[string][]map[string]any
	posted []string
}

func (f *fakeService) handler(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if r.Header.Get("X-Api-Key") != f.key {
		w.WriteHeader(http.StatusUnauthorized)
		return
	}
	p := strings.SplitN(strings.TrimPrefix(r.URL.Path, "/api/"), "/", 2)[1]
	if r.Method == http.MethodPut {
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		base := strings.SplitN(p, "/", 2)[0]
		for i, have := range f.have[base] {
			if fmt.Sprint(have["id"]) == fmt.Sprint(body["id"]) {
				f.have[base][i] = body
			}
		}
		f.posted = append(f.posted, "PUT "+base)
		_ = json.NewEncoder(w).Encode(body)
		return
	}
	if r.Method == http.MethodPost {
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		f.have[p] = append(f.have[p], body)
		f.posted = append(f.posted, p)
		_ = json.NewEncoder(w).Encode(body)
		return
	}
	switch p {
	case "system/status":
		_ = json.NewEncoder(w).Encode(map[string]string{"appName": f.app, "version": "1.0"})
	case "qualityprofile":
		_ = json.NewEncoder(w).Encode([]map[string]any{{"id": 1, "name": "Any"}, {"id": 4, "name": "HD-1080p"}})
	case "downloadclient/schema":
		_ = json.NewEncoder(w).Encode([]map[string]any{schemaOf("QBittorrent", "host", "port", "username", "password", "movieCategory", "tvCategory")})
	case "indexerProxy/schema":
		_ = json.NewEncoder(w).Encode([]map[string]any{schemaOf("FlareSolverr", "host", "requestTimeout")})
	case "applications/schema":
		_ = json.NewEncoder(w).Encode([]map[string]any{schemaOf("Radarr", "prowlarrUrl", "baseUrl", "apiKey"), schemaOf("Sonarr", "prowlarrUrl", "baseUrl", "apiKey")})
	default:
		list := f.have[p]
		if list == nil {
			list = []map[string]any{}
		}
		_ = json.NewEncoder(w).Encode(list)
	}
}

func schemaOf(impl string, fields ...string) map[string]any {
	var fs []any
	for _, f := range fields {
		fs = append(fs, map[string]any{"name": f})
	}
	return map[string]any{"implementation": impl, "fields": fs}
}

func TestDownloadStack(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	st, err := store.Open(filepath.Join(dir, "kinara.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	hash, _ := auth.HashPassword("password123")
	st.CreateUser(ctx, "alex", hash, true)
	media := filepath.Join(dir, "media")
	srv := &Server{Store: st, Images: images.New(filepath.Join(dir, "img")), Watch: &watch.Service{Store: st},
		Log: slog.New(slog.NewTextHandler(io.Discard, nil)), Version: "test", BaseCtx: ctx}
	hs := httptest.NewServer(srv.Handler())
	defer hs.Close()
	owner := &client{t: t, base: hs.URL + "/api/v1"}
	var login struct{ Token string }
	owner.do("POST", "/auth/login", map[string]any{"username": "alex", "password": "password123"}, &login)
	owner.token = login.Token

	var info stackInfo
	if resp := owner.do("GET", "/arr/stack", nil, &info); resp.StatusCode != 200 || len(info.Install) == 0 || info.Ports["qbittorrent"] != 8090 {
		t.Fatalf("info: %d %+v", resp.StatusCode, info)
	}
	if resp := owner.do("POST", "/arr/stack/connect", nil, nil); resp.StatusCode != http.StatusConflict {
		t.Fatalf("connect before a script: %d", resp.StatusCode)
	}
	if resp := owner.do("POST", "/arr/stack", map[string]any{"mediaDir": "media"}, nil); resp.StatusCode != 400 {
		t.Fatalf("relative folder: %d", resp.StatusCode)
	}
	if resp := owner.do("POST", "/arr/stack", map[string]any{"mediaDir": media, "tvDir": "../etc"}, nil); resp.StatusCode != 400 {
		t.Fatalf("folder outside: %d", resp.StatusCode)
	}
	// A qBittorrent already on the computer holds 6881: the stack takes the next.
	oldFree := portFree
	portFree = func(p int) bool { return p != 6881 }
	defer func() { portFree = oldFree }()
	var made struct{ Name, Script, Compose, Run string }
	owner.do("POST", "/arr/stack", map[string]any{"mediaDir": media}, &made)
	if !strings.Contains(made.Compose, `"6882:6882/udp"`) || !strings.Contains(made.Compose, "TORRENTING_PORT=6882") {
		t.Fatalf("peer port: %s", made.Compose)
	}
	// Made again while the stack runs (6882 busy now): it keeps its port.
	portFree = func(p int) bool { return p != 6881 && p != 6882 }
	owner.do("POST", "/arr/stack", map[string]any{"mediaDir": media}, &made)
	if !strings.Contains(made.Compose, "TORRENTING_PORT=6882") {
		t.Fatal("peer port changed")
	}
	a, _ := st.GetArrSettings(ctx)
	if a.Stack == nil || made.Name != "kinara-stack.sh" || !strings.Contains(made.Compose, "RADARR__AUTH__APIKEY="+a.Stack.RadarrKey) {
		t.Fatalf("made: %+v", made)
	}
	// Made again: the same keys, so running containers still match.
	owner.do("POST", "/arr/stack", map[string]any{"mediaDir": media}, &made)
	if b, _ := st.GetArrSettings(ctx); b.Stack.RadarrKey != a.Stack.RadarrKey {
		t.Fatal("keys changed")
	}
	owner.do("POST", "/arr/stack", map[string]any{"mediaDir": media, "newSecrets": true}, &made)
	if b, _ := st.GetArrSettings(ctx); b.Stack.RadarrKey == a.Stack.RadarrKey {
		t.Fatal("new keys asked for")
	}
	a, _ = st.GetArrSettings(ctx)
	k := a.Stack

	// The script would make these.
	for _, d := range []string{"movies", "tv"} {
		os.MkdirAll(filepath.Join(media, d), 0o755)
	}
	radarr := &fakeService{app: "Radarr", key: k.RadarrKey, have: map[string][]map[string]any{}}
	sonarr := &fakeService{app: "Sonarr", key: k.SonarrKey, have: map[string][]map[string]any{"rootfolder": {{"path": "/data/tv"}}}}
	prowlarr := &fakeService{app: "Prowlarr", key: k.ProwlarrKey, have: map[string][]map[string]any{"indexer": {{"enable": true}}}}
	qb := &fakeQbitAny{user: k.QbitUser, pass: k.QbitPass}
	urls := map[int]string{}
	for port, h := range map[int]http.HandlerFunc{arr.PortRadarr: radarr.handler, arr.PortSonarr: sonarr.handler, arr.PortProwlarr: prowlarr.handler, arr.PortQbit: qb.handler} {
		ts := httptest.NewServer(h)
		defer ts.Close()
		urls[port] = ts.URL
	}
	old := stackURL
	stackURL = func(port int) string { return urls[port] }
	defer func() { stackURL = old }()

	var res struct {
		Steps    []stepView
		OK       bool
		Indexers int
	}
	owner.do("POST", "/arr/stack/connect", nil, &res)
	if !res.OK || res.Indexers != 1 {
		t.Fatalf("connect: %+v", res)
	}
	if !strings.Contains(strings.Join(radarr.posted, ","), "rootfolder") || strings.Contains(strings.Join(sonarr.posted, ","), "rootfolder") {
		t.Fatalf("root folders: %v %v", radarr.posted, sonarr.posted)
	}
	if len(radarr.have["downloadclient"]) != 1 || len(sonarr.have["downloadclient"]) != 1 || len(prowlarr.have["applications"]) != 2 {
		t.Fatal("not wired")
	}
	a, _ = st.GetArrSettings(ctx)
	if !a.Radarr.Ready() || a.Radarr.ProfileID != 4 || a.Sonarr.RootFolder != "/data/tv" || a.Qbit.Password != k.QbitPass || a.Prowlarr.APIKey != k.ProwlarrKey {
		t.Fatalf("kinara's settings: %+v", a)
	}
	if qb.categories != 2 || qb.savePath != "/data/downloads" {
		t.Fatalf("qbit: %+v", qb)
	}
	libs, _ := st.ListLibraries(ctx)
	if len(libs) != 2 {
		t.Fatalf("libraries: %d", len(libs))
	}
	// Twice, as the page asks: progress line by line, then the result.
	req, _ := http.NewRequest("POST", owner.base+"/arr/stack/connect", nil)
	req.Header.Set("Authorization", "Bearer "+owner.token)
	req.Header.Set("Accept", "application/x-ndjson")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	var events []map[string]any
	dec := json.NewDecoder(resp.Body)
	for dec.More() {
		var ev map[string]any
		if err := dec.Decode(&ev); err != nil {
			t.Fatal(err)
		}
		events = append(events, ev)
	}
	resp.Body.Close()
	last := events[len(events)-1]
	if last["event"] != "done" || last["progress"] != 1.0 || events[0]["event"] != "status" {
		t.Fatalf("stream: %v", events)
	}
	statuses, stepsSeen := 0, 0
	prev := 0.0
	for _, ev := range events {
		if p := ev["progress"].(float64); p < prev {
			t.Fatalf("progress went back: %v", events)
		} else {
			prev = p
		}
		switch ev["event"] {
		case "status":
			statuses++
		case "step":
			stepsSeen++
		}
	}
	if stepsSeen != connectSteps || statuses < connectSteps {
		t.Fatalf("%d steps, %d statuses", stepsSeen, statuses)
	}
	res.Steps = nil
	b, _ := json.Marshal(last["result"])
	json.Unmarshal(b, &res)
	for _, s := range res.Steps {
		if s.State == "failed" {
			t.Fatalf("second connect: %+v", s)
		}
	}
	if len(prowlarr.have["applications"]) != 2 || len(radarr.have["downloadclient"]) != 1 || len(prowlarr.have["indexerProxy"]) != 1 {
		t.Fatal("added twice")
	}

	// The settings page sees the helpers, secrets masked.
	srv.readHelpers(ctx, a)
	var view arrView
	owner.do("GET", "/arr", nil, &view)
	if view.Prowlarr.Indexers != 1 || !view.Qbit.HasPassword || view.Qbit.Version != "v5.0.2" || !view.Stack || view.Prowlarr.APIKey == k.ProwlarrKey {
		t.Fatalf("view: %+v", view)
	}
	var tested struct{ Version string }
	if resp := owner.do("POST", "/arr/qbit/test", map[string]any{}, &tested); resp.StatusCode != 200 || tested.Version != "v5.0.2" {
		t.Fatalf("qbit test: %d %+v", resp.StatusCode, tested)
	}
	var link struct{ Steps []stepView }
	owner.do("POST", "/arr/link", map[string]any{"qbitFromApps": "http://qbittorrent:8080"}, &link)
	if len(link.Steps) != 4 {
		t.Fatalf("link: %+v", link)
	}

	// The owner switches to their own qBittorrent in its card: it's kept
	// as theirs, passed on to Radarr and Sonarr at once, and connecting
	// again doesn't switch back.
	mine := &fakeQbitAny{user: "me", pass: "vpn"}
	ms := httptest.NewServer(http.HandlerFunc(mine.handler))
	defer ms.Close()
	var saved struct {
		Linked []stepView
	}
	owner.do("PUT", "/arr", map[string]any{"qbit": map[string]any{"url": ms.URL, "username": "me", "password": "vpn"}}, &saved)
	if len(saved.Linked) == 0 || fieldOf(radarr.have["downloadclient"][0], "host") != "host.docker.internal" {
		t.Fatalf("not passed on: %+v %+v", saved.Linked, radarr.have["downloadclient"])
	}
	if a, _ := st.GetArrSettings(ctx); !a.Stack.OwnQbit {
		t.Fatal("not kept as the owner's")
	}
	// Saving the same details again passes them on again (an app pointing
	// elsewhere gets fixed that way).
	radarr.have["downloadclient"][0] = map[string]any{"id": 1, "name": "qBittorrent", "implementation": "QBittorrent",
		"fields": []any{map[string]any{"name": "host", "value": "qbittorrent"}, map[string]any{"name": "port", "value": 8090}}}
	saved.Linked = nil
	owner.do("PUT", "/arr", map[string]any{"qbit": map[string]any{"url": ms.URL, "username": "me"}}, &saved)
	if len(saved.Linked) == 0 || fieldOf(radarr.have["downloadclient"][0], "host") != "host.docker.internal" {
		t.Fatalf("same details not passed on: %+v", saved.Linked)
	}
	owner.do("POST", "/arr/stack/connect", nil, &res)
	if a, _ := st.GetArrSettings(ctx); a.Qbit.URL != ms.URL || a.Qbit.Username != "me" {
		t.Fatalf("switched back: %+v", a.Qbit)
	}
}

type fakeQbitAny struct {
	mu         sync.Mutex
	user, pass string
	categories int
	savePath   string
}

func (f *fakeQbitAny) handler(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	_ = r.ParseForm()
	switch r.URL.Path {
	case "/api/v2/auth/login":
		if r.Form.Get("username") == f.user && r.Form.Get("password") == f.pass {
			http.SetCookie(w, &http.Cookie{Name: "SID", Value: "x", Path: "/"})
			io.WriteString(w, "Ok.")
			return
		}
		io.WriteString(w, "Fails.")
	case "/api/v2/app/version":
		io.WriteString(w, "v5.0.2")
	case "/api/v2/app/setPreferences":
		var p struct {
			SavePath string `json:"save_path"`
		}
		json.Unmarshal([]byte(r.Form.Get("json")), &p)
		f.savePath = p.SavePath
	case "/api/v2/torrents/createCategory":
		if f.categories >= 2 {
			w.WriteHeader(http.StatusConflict)
			return
		}
		f.categories++
	}
}

// The owner's own qBittorrent (with their VPN, say): the stack has none,
// Radarr and Sonarr reach it on the host, and map its folder.
func TestDownloadStackOwnQbit(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	st, err := store.Open(filepath.Join(dir, "kinara.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	hash, _ := auth.HashPassword("password123")
	st.CreateUser(ctx, "alex", hash, true)
	srv := &Server{Store: st, Images: images.New(filepath.Join(dir, "img")), Watch: &watch.Service{Store: st},
		Log: slog.New(slog.NewTextHandler(io.Discard, nil)), Version: "test", BaseCtx: ctx}
	hs := httptest.NewServer(srv.Handler())
	defer hs.Close()
	owner := &client{t: t, base: hs.URL + "/api/v1"}
	var login struct{ Token string }
	owner.do("POST", "/auth/login", map[string]any{"username": "alex", "password": "password123"}, &login)
	owner.token = login.Token

	media := filepath.Join(dir, "media")
	if resp := owner.do("POST", "/arr/stack", map[string]any{"mediaDir": media, "ownQbit": true}, nil); resp.StatusCode != 400 {
		t.Fatalf("own qBittorrent before connecting it: %d", resp.StatusCode)
	}
	mine := &fakeQbitAny{user: "me", pass: "vpn"}
	qs := httptest.NewServer(http.HandlerFunc(mine.handler))
	defer qs.Close()
	owner.do("PUT", "/arr", map[string]any{"qbit": map[string]any{"url": qs.URL, "username": "me", "password": "vpn"}}, nil)
	var made struct{ Name, Script, Compose string }
	owner.do("POST", "/arr/stack", map[string]any{"mediaDir": media, "ownQbit": true}, &made)
	if strings.Contains(made.Compose, "linuxserver/qbittorrent") || !strings.Contains(made.Compose, "host.docker.internal:host-gateway") ||
		strings.Contains(made.Script, "qBittorrent.conf") || !strings.Contains(made.Script, "--remove-orphans") || !strings.Contains(made.Script, "qBittorrent: yours") {
		t.Fatalf("compose or script: %s\n%s", made.Compose, made.Script)
	}
	a, _ := st.GetArrSettings(ctx)
	k := a.Stack
	os.MkdirAll(filepath.Join(media, "movies"), 0o755)
	os.MkdirAll(filepath.Join(media, "tv"), 0o755)
	radarr := &fakeService{app: "Radarr", key: k.RadarrKey, have: map[string][]map[string]any{}}
	// Sonarr still has the stack's own qBittorrent from before: it's repointed.
	sonarr := &fakeService{app: "Sonarr", key: k.SonarrKey, have: map[string][]map[string]any{"downloadclient": {{"id": 1, "name": "qBittorrent", "implementation": "QBittorrent",
		"fields": []any{map[string]any{"name": "host", "value": "qbittorrent"}, map[string]any{"name": "port", "value": 8090}}}}}}
	prowlarr := &fakeService{app: "Prowlarr", key: k.ProwlarrKey, have: map[string][]map[string]any{}}
	urls := map[int]string{}
	for port, h := range map[int]http.HandlerFunc{arr.PortRadarr: radarr.handler, arr.PortSonarr: sonarr.handler, arr.PortProwlarr: prowlarr.handler} {
		ts := httptest.NewServer(h)
		defer ts.Close()
		urls[port] = ts.URL
	}
	old := stackURL
	stackURL = func(port int) string { return urls[port] }
	defer func() { stackURL = old }()

	var res struct {
		Steps []stepView
		OK    bool
	}
	owner.do("POST", "/arr/stack/connect", nil, &res)
	if !res.OK {
		t.Fatalf("connect: %+v", res.Steps)
	}
	dc := radarr.have["downloadclient"]
	if len(dc) != 1 || fieldOf(dc[0], "host") != "host.docker.internal" || fmt.Sprint(fieldOf(dc[0], "port")) != strings.Split(qs.URL, ":")[2] {
		t.Fatalf("radarr's client: %+v", dc)
	}
	if len(sonarr.have["downloadclient"]) != 1 || fieldOf(sonarr.have["downloadclient"][0], "host") != "host.docker.internal" {
		t.Fatalf("sonarr's client not repointed: %+v", sonarr.have["downloadclient"])
	}
	pm := radarr.have["remotepathmapping"]
	want := filepath.Join(media, "downloads") + string(filepath.Separator)
	if len(pm) != 1 || pm[0]["remotePath"] != want || pm[0]["localPath"] != "/data/downloads/" || pm[0]["host"] != "host.docker.internal" {
		t.Fatalf("path mapping: %+v (want %s)", pm, want)
	}
	if mine.categories != 2 || mine.savePath != "" {
		t.Fatalf("your qBittorrent: categories %d, default folder changed to %q", mine.categories, mine.savePath)
	}
	// Link with the qBittorrent card's 127.0.0.1 address: still host.docker.internal
	// to the apps in Docker, and nothing new (it's all there).
	var link struct{ Steps []stepView }
	owner.do("POST", "/arr/link", nil, &link)
	for _, st := range link.Steps {
		if st.State != "skipped" {
			t.Fatalf("link: %+v", link.Steps)
		}
	}
	if len(link.Steps) != 6 || fieldOf(radarr.have["downloadclient"][0], "host") != "host.docker.internal" {
		t.Fatalf("link: %+v", link.Steps)
	}
	if a, _ := st.GetArrSettings(ctx); a.Qbit.URL != qs.URL || a.Qbit.Username != "me" {
		t.Fatalf("your qBittorrent's connection was replaced: %+v", a.Qbit)
	}
}

func fieldOf(obj map[string]any, name string) any {
	for _, f := range obj["fields"].([]any) {
		if m := f.(map[string]any); m["name"] == name {
			return m["value"]
		}
	}
	return nil
}
