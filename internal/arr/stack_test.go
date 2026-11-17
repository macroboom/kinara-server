package arr

import (
	"context"
	"crypto/pbkdf2"
	"crypto/sha512"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"regexp"
	"strings"
	"sync"
	"testing"
)

func testStack(goos string) Stack {
	s := Stack{OS: goos, MediaDir: "/srv/media/", MoviesDir: "movies", TVDir: "tv", DownloadsDir: "downloads", PUID: 1001, PGID: 1002, TZ: "Europe/London"}
	s.NewSecrets()
	return s
}

func TestStackCompose(t *testing.T) {
	s := testStack("linux")
	c := s.Compose()
	for _, want := range []string{
		"RADARR__AUTH__APIKEY=" + s.RadarrKey, "SONARR__AUTH__APIKEY=" + s.SonarrKey, "PROWLARR__AUTH__APIKEY=" + s.ProwlarrKey,
		"source: \"/srv/media\"\n        target: /data", `"8090:8090"`, "flaresolverr/flaresolverr", `"8191:8191"`, "WEBUI_PORT=8090", "TORRENTING_PORT=6881", `"6881:6881/udp"`, "PUID=1001", "TZ=Europe/London",
	} {
		if !strings.Contains(c, want) {
			t.Errorf("compose is missing %q", want)
		}
	}
	if s.MoviesPath() != "/data/movies" || s.DownloadsPath() != "/data/downloads" {
		t.Fatal(s.MoviesPath(), s.DownloadsPath())
	}
	w := testStack("windows")
	w.MediaDir = `D:\Media`
	if !strings.Contains(w.Compose(), `source: "D:/Media"`) {
		t.Fatal("windows path not turned to slashes")
	}
	// A whole drive: "E:" alone is a relative path, and the short volume
	// form can't take its colon.
	for _, m := range []string{`E:\`, `E:`, `E:/`} {
		w.MediaDir = m
		if c := w.Compose(); !strings.Contains(c, `source: "E:/"`) || strings.Contains(c, "E::") {
			t.Fatalf("%s: %s", m, c)
		}
	}
	w.MediaDir = `E:\`
	if _, ps := w.Script(); !strings.Contains(ps, `'E:\movies'`) || !strings.Contains(ps, "$LASTEXITCODE") {
		t.Fatal("drive root folders, or exit code check")
	}
}

func TestStackScripts(t *testing.T) {
	s := testStack("linux")
	name, sh := s.Script()
	if name != "kinara-stack.sh" || !strings.HasPrefix(sh, "#!/bin/sh") {
		t.Fatal(name)
	}
	for _, want := range []string{"'/srv/media/movies'", "'/srv/media/downloads/incomplete'", "'/srv/media/downloads/radarr'", "docker compose up -d",
		"<<'KINARA_COMPOSE'", "\nKINARA_COMPOSE\n", "\nKINARA_QBIT\nelse\n", "docker stop qbittorrent", "ENVIRON[\"KQ_PASS\"]", "Password_PBKDF2"} {
		if !strings.Contains(sh, want) {
			t.Errorf("shell script is missing %q", want)
		}
	}
	w := testStack("windows")
	w.MediaDir = `D:\Media`
	name, ps := w.Script()
	if name != "kinara-stack.ps1" {
		t.Fatal(name)
	}
	for _, want := range []string{`'D:\Media\movies'`, `'D:\Media\downloads\incomplete'`, "\n'@, $Utf8)", "UTF8Encoding $false", "docker stop qbittorrent", "docker compose up -d"} {
		if !strings.Contains(ps, want) {
			t.Errorf("powershell script is missing %q", want)
		}
	}
	for _, goos := range []string{"linux", "darwin", "windows"} {
		if len(DockerInstall(goos)) == 0 || DockerInstall(goos)[0].Command == "" {
			t.Fatal(goos)
		}
	}
}

// The password hash is the one qBittorrent checks: PBKDF2-SHA512, 100000
// rounds, salt and key in base64.
func TestQbitConfPassword(t *testing.T) {
	s := testStack("linux")
	m := regexp.MustCompile(`Password_PBKDF2="@ByteArray\(([^:]+):([^)]+)\)"`).FindStringSubmatch(s.QbitConf())
	if m == nil {
		t.Fatal("no password line")
	}
	salt, _ := base64.StdEncoding.DecodeString(m[1])
	key, _ := pbkdf2.Key(sha512.New, s.QbitPass, salt, 100000, 64)
	if base64.StdEncoding.EncodeToString(key) != m[2] || len(salt) != 16 {
		t.Fatal("hash doesn't match the password")
	}
	if !strings.Contains(s.QbitConf(), `Session\DefaultSavePath=/data/downloads/`) {
		t.Fatal("save path")
	}
}

// fakeQbit is enough of qBittorrent's API.
type fakeQbit struct {
	mu         sync.Mutex
	modern     bool
	prefs      string
	categories map[string]string
}

func (f *fakeQbit) handler(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	// Like qBittorrent behind Docker's "8090:8080": it listens on 8080, so a
	// Host header naming another port is turned away, and so is a Referer
	// that isn't the Host.
	if h := r.Host; strings.Contains(h, ":") && !strings.HasSuffix(h, ":8080") {
		w.WriteHeader(http.StatusUnauthorized)
		return
	}
	if ref := r.Header.Get("Referer"); ref != "" && !strings.Contains(ref, "//"+r.Host) {
		w.WriteHeader(http.StatusUnauthorized)
		return
	}
	if r.URL.Path == "/api/v2/auth/login" {
		_ = r.ParseForm()
		if r.Form.Get("username") != "kinara" || r.Form.Get("password") != "pw" {
			_, _ = io.WriteString(w, "Fails.")
			return
		}
		http.SetCookie(w, &http.Cookie{Name: "SID", Value: "s1", Path: "/"})
		// qBittorrent 5.2 and later: 204, nothing in it (older: "Ok.").
		if f.modern {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		_, _ = io.WriteString(w, "Ok.")
		return
	}
	if c, err := r.Cookie("SID"); err != nil || c.Value != "s1" {
		w.WriteHeader(http.StatusForbidden)
		return
	}
	_ = r.ParseForm()
	switch r.URL.Path {
	case "/api/v2/app/version":
		_, _ = io.WriteString(w, "v5.0.2")
	case "/api/v2/app/setPreferences":
		f.prefs = r.Form.Get("json")
	case "/api/v2/torrents/createCategory":
		cat := r.Form.Get("category")
		if _, ok := f.categories[cat]; ok {
			w.WriteHeader(http.StatusConflict)
			return
		}
		f.categories[cat] = r.Form.Get("savePath")
	default:
		w.WriteHeader(http.StatusNotFound)
	}
}

func TestQbit(t *testing.T) {
	f := &fakeQbit{categories: map[string]string{"sonarr": "/x"}}
	ts := httptest.NewServer(http.HandlerFunc(f.handler))
	defer ts.Close()
	ctx := context.Background()
	if _, err := (&Qbit{BaseURL: ts.URL, Username: "kinara", Password: "nope"}).Version(ctx); err == nil || !errors.Is(err, ErrQbitRefused) {
		t.Fatal(err)
	}
	q := &Qbit{BaseURL: ts.URL, Username: "kinara", Password: "pw"}
	v, err := q.Version(ctx)
	if err != nil || v != "v5.0.2" {
		t.Fatal(v, err)
	}
	f.modern = true
	if v, err := (&Qbit{BaseURL: ts.URL, Username: "kinara", Password: "pw"}).Version(ctx); err != nil || v != "v5.0.2" {
		t.Fatal("204 sign-in:", v, err)
	}
	if err := q.Prepare(ctx, "/data/downloads"); err != nil {
		t.Fatal(err)
	}
	if f.prefs != `{"save_path":"/data/downloads"}` || f.categories["radarr"] != "/data/downloads/radarr" || f.categories["sonarr"] != "/x" {
		t.Fatal(f.prefs, f.categories)
	}
}

// fakeWiring is the parts of Radarr, Sonarr and Prowlarr that wiring uses.
type fakeWiring struct {
	mu      sync.Mutex
	posted  map[string][]map[string]any
	have    map[string][]map[string]any
	schemas map[string][]map[string]any
}

func (f *fakeWiring) handler(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if r.Header.Get("X-Api-Key") != "key" {
		w.WriteHeader(http.StatusUnauthorized)
		return
	}
	p := r.URL.Path
	p = p[strings.Index(p[1:], "/")+2:] // past /api
	p = p[strings.Index(p, "/")+1:]     // past v3 or v1
	if r.Method == http.MethodPost {
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		f.posted[p] = append(f.posted[p], body)
		f.have[p] = append(f.have[p], body)
		_ = json.NewEncoder(w).Encode(body)
		return
	}
	if s, ok := f.schemas[p]; ok {
		_ = json.NewEncoder(w).Encode(s)
		return
	}
	list := f.have[p]
	if list == nil {
		list = []map[string]any{}
	}
	_ = json.NewEncoder(w).Encode(list)
}

func schema(impl string, fields ...string) map[string]any {
	var fs []any
	for _, f := range fields {
		fs = append(fs, map[string]any{"name": f, "value": nil})
	}
	return map[string]any{"id": 0, "implementation": impl, "fields": fs}
}

func field(obj map[string]any, name string) any {
	for _, f := range obj["fields"].([]any) {
		if m := f.(map[string]any); m["name"] == name {
			return m["value"]
		}
	}
	return nil
}

func TestWiring(t *testing.T) {
	f := &fakeWiring{posted: map[string][]map[string]any{}, have: map[string][]map[string]any{
		"rootfolder": {{"path": "/data/tv/"}},
		"indexer":    {{"enable": true}, {"enable": false}},
	}, schemas: map[string][]map[string]any{
		"downloadclient/schema": {schema("Transmission", "host"), schema("QBittorrent", "host", "port", "useSsl", "username", "password", "movieCategory", "tvCategory")},
		"applications/schema":   {schema("Lidarr", "baseUrl"), schema("Radarr", "prowlarrUrl", "baseUrl", "apiKey"), schema("Sonarr", "prowlarrUrl", "baseUrl", "apiKey")},
		"indexerProxy/schema":   {schema("Http", "host"), schema("FlareSolverr", "host", "requestTimeout")},
	}}
	ts := httptest.NewServer(http.HandlerFunc(f.handler))
	defer ts.Close()
	ctx := context.Background()
	radarr := &Client{Kind: Radarr, BaseURL: ts.URL, APIKey: "key"}
	sonarr := &Client{Kind: Sonarr, BaseURL: ts.URL, APIKey: "key"}
	prowlarr := &Client{Kind: Prowlarr, BaseURL: ts.URL, APIKey: "key"}

	if added, err := sonarr.EnsureRootFolder(ctx, "/data/tv"); added || err != nil {
		t.Fatal("tv folder is there already", added, err)
	}
	if added, err := radarr.EnsureRootFolder(ctx, "/data/movies"); !added || err != nil {
		t.Fatal(added, err)
	}
	q := QbitClient{Host: "qbittorrent", Port: 8080, Username: "kinara", Password: "pw", Category: "radarr"}
	if added, err := radarr.EnsureQbittorrent(ctx, q); !added || err != nil {
		t.Fatal(added, err)
	}
	dc := f.posted["downloadclient"][0]
	if dc["implementation"] != "QBittorrent" || field(dc, "host") != "qbittorrent" || field(dc, "movieCategory") != "radarr" || field(dc, "port") != float64(8080) {
		t.Fatal(dc)
	}
	if _, ok := dc["id"]; ok {
		t.Fatal("id sent")
	}
	// A second time: there already.
	if added, err := sonarr.EnsureQbittorrent(ctx, q); added || err != nil {
		t.Fatal(added, err)
	}
	if added, err := prowlarr.EnsureApp(ctx, ProwlarrApp{Kind: Sonarr, BaseURL: "http://sonarr:8989", ProwlarrURL: "http://prowlarr:9696", APIKey: "sk"}); !added || err != nil {
		t.Fatal(added, err)
	}
	app := f.posted["applications"][0]
	if app["implementation"] != "Sonarr" || app["syncLevel"] != "fullSync" || field(app, "apiKey") != "sk" || field(app, "baseUrl") != "http://sonarr:8989" {
		t.Fatal(app)
	}
	if added, _ := prowlarr.EnsureApp(ctx, ProwlarrApp{Kind: Sonarr}); added {
		t.Fatal("sonarr added twice")
	}
	if added, err := prowlarr.EnsureFlareSolverr(ctx, "http://flaresolverr:8191"); !added || err != nil {
		t.Fatal(added, err)
	}
	fs := f.posted["indexerProxy"][0]
	if field(fs, "host") != "http://flaresolverr:8191/" || len(f.posted["tag"]) != 1 || f.posted["tag"][0]["label"] != "flaresolverr" {
		t.Fatal(fs, f.posted["tag"])
	}
	if added, _ := prowlarr.EnsureFlareSolverr(ctx, "x"); added {
		t.Fatal("proxy added twice")
	}
	total, enabled, err := prowlarr.Indexers(ctx)
	if total != 2 || enabled != 1 || err != nil {
		t.Fatal(total, enabled, err)
	}
	// Prowlarr's paths are v1.
	if u, _ := url.Parse(prowlarr.BaseURL); u == nil {
		t.Fatal()
	}
	if _, err := (&Client{Kind: Radarr, BaseURL: ts.URL, APIKey: "bad"}).Health(ctx); err == nil {
		t.Fatal("bad key accepted")
	}
}
