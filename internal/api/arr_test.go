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
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"kinara/internal/auth"
	"kinara/internal/images"
	"kinara/internal/store"
	"kinara/internal/tmdb"
	"kinara/internal/watch"
)

// fakeRadarr answers what Kinara asks Radarr: adding a movie, its queue,
// and whether a movie has its file.
type fakeRadarr struct {
	mu      sync.Mutex
	added   []map[string]any
	queue   []map[string]any
	hasFile bool
	// movie is what GET movie?tmdbId= finds (nil: none).
	movie map[string]any
}

func (f *fakeRadarr) handler(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if r.Header.Get("X-Api-Key") != "secret-key-1234" {
		w.WriteHeader(401)
		return
	}
	send := func(v any) { json.NewEncoder(w).Encode(v) }
	switch p := strings.TrimPrefix(r.URL.Path, "/api/v3/"); {
	case p == "system/status":
		send(map[string]any{"appName": "Radarr", "version": "5.14"})
	case p == "qualityprofile":
		send([]map[string]any{{"id": 4, "name": "HD-1080p"}})
	case p == "rootfolder":
		send([]map[string]any{{"id": 1, "path": "/data/movies"}})
	case p == "tag":
		send([]any{})
	case p == "movie" && r.Method == "GET":
		if f.movie != nil && r.URL.Query().Get("tmdbId") == fmt.Sprint(f.movie["tmdbId"]) {
			send([]any{f.movie})
			return
		}
		send([]any{})
	case p == "history/movie":
		send([]any{})
	case p == "movie/lookup/tmdb":
		send(map[string]any{"title": "Far Shore", "year": 2026, "tmdbId": 3})
	case p == "movie" && r.Method == "POST":
		var body map[string]any
		json.NewDecoder(r.Body).Decode(&body)
		f.added = append(f.added, body)
		send(map[string]any{"id": 31})
	case p == "queue":
		send(map[string]any{"records": f.queue})
	case p == "movie/31":
		send(map[string]any{"id": 31, "hasFile": f.hasFile})
	default:
		w.WriteHeader(404)
	}
}

func TestRequestsToRadarr(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	st, err := store.Open(filepath.Join(dir, "kinara.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	hash, _ := auth.HashPassword("password123")
	st.CreateUser(ctx, "alex", hash, true)
	st.CreateUser(ctx, "sam", hash, false)
	_, tmdbHS := newDiscoverTMDB(t)
	radarr := &fakeRadarr{}
	rs := httptest.NewServer(http.HandlerFunc(radarr.handler))
	defer rs.Close()
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	srv := &Server{Store: st, Images: images.New(filepath.Join(dir, "img")), Watch: &watch.Service{Store: st}, Log: log,
		Version: "test", BaseCtx: ctx, TMDB: &tmdb.Client{Key: "k", BaseURL: tmdbHS.URL, HTTP: tmdbHS.Client()}}
	hs := httptest.NewServer(srv.Handler())
	defer hs.Close()
	signIn := func(user string) *client {
		c := &client{t: t, base: hs.URL + "/api/v1"}
		var login struct{ Token string }
		c.do("POST", "/auth/login", map[string]any{"username": user, "password": "password123"}, &login)
		c.token = login.Token
		return c
	}
	owner, sam := signIn("alex"), signIn("sam")

	// Setting it up: the connection test lists the choices.
	var opts struct {
		Profiles    []struct{ ID int }
		RootFolders []struct{ Path string }
	}
	if resp := owner.do("POST", "/arr/radarr/test", map[string]string{"url": rs.URL, "apiKey": "secret-key-1234"}, &opts); resp.StatusCode != 200 || len(opts.Profiles) != 1 {
		t.Fatalf("test: %d %+v", resp.StatusCode, opts)
	}
	if resp := owner.do("POST", "/arr/radarr/test", map[string]string{"url": rs.URL, "apiKey": "wrong"}, nil); resp.StatusCode != 502 {
		t.Errorf("wrong key: %d", resp.StatusCode)
	}
	var view arrView
	owner.do("PUT", "/arr", map[string]any{"radarr": map[string]any{"enabled": true, "url": rs.URL, "apiKey": "secret-key-1234",
		"profileId": 4, "rootFolder": "/data/movies"}}, &view)
	if !view.Radarr.Ready || view.Radarr.APIKey != "…1234" || view.Sonarr.Ready {
		t.Fatalf("saved: %+v", view)
	}
	// Sending the key's end back keeps the key.
	owner.do("PUT", "/arr", map[string]any{"radarr": map[string]any{"apiKey": "…1234", "search": true}}, &view)
	if a, _ := st.GetArrSettings(ctx); a.Radarr.APIKey != "secret-key-1234" {
		t.Fatalf("key lost: %+v", a.Radarr)
	}
	if resp := sam.do("GET", "/arr", nil, nil); resp.StatusCode != 403 {
		t.Errorf("members can read the settings: %d", resp.StatusCode)
	}

	// A request, approved: Radarr adds it and searches; the apps see it coming.
	var made requestView
	sam.do("POST", "/requests", map[string]any{"kind": "movie", "tmdbId": 3}, &made)
	owner.do("POST", "/requests/"+itoa(made.ID)+"/approve", nil, nil)
	var req store.Request
	for i := 0; i < 100; i++ {
		if req, _ = st.GetRequest(ctx, made.ID); req.ArrState != "" {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if req.ArrState != "sent" || req.ArrNote != "Added to Radarr, searching" || len(radarr.added) != 1 || radarr.added[0]["tmdbId"] != float64(3) {
		t.Fatalf("sent: %+v added %v", req, radarr.added)
	}
	statuses, _ := st.Statuses(ctx, true)
	if len(statuses) != 1 || statuses[0].TMDBID != 3 || statuses[0].State != stateQueued {
		t.Fatalf("searching: %+v", statuses)
	}

	// Nothing in the queue: Radarr says why, in the request's own words.
	soon := time.Now().AddDate(0, 0, 20)
	radarr.mu.Lock()
	radarr.movie = map[string]any{"id": 31, "tmdbId": 3, "monitored": true, "isAvailable": false, "minimumAvailability": "released",
		"inCinemas": time.Now().AddDate(0, 0, -10).Format(time.RFC3339), "digitalRelease": soon.Format(time.RFC3339), "added": time.Now().Format(time.RFC3339)}
	radarr.mu.Unlock()
	a, _ := st.GetArrSettings(ctx)
	srv.arrExplain(ctx, a, nil)
	statuses, _ = st.Statuses(ctx, true)
	if len(statuses) != 1 || statuses[0].Message != "In cinemas now; out on digital "+shortDate(soon, time.Now()) || statuses[0].State != stateQueued {
		t.Fatalf("explained: %+v", statuses)
	}

	// Downloading: one status with its progress, the searching one gone.
	radarr.mu.Lock()
	radarr.queue = []map[string]any{{"id": 1, "downloadId": "ABC", "movieId": 31, "size": 1000, "sizeleft": 400, "timeleft": "00:05:00",
		"status": "downloading", "trackedDownloadState": "downloading", "movie": map[string]any{"title": "Far Shore", "year": 2026, "tmdbId": 3}}}
	radarr.mu.Unlock()
	srv.arrReadQueues(ctx)
	statuses, _ = st.Statuses(ctx, true)
	if len(statuses) != 1 || statuses[0].State != stateInProgress || statuses[0].Progress == nil || *statuses[0].Progress != 0.6 || statuses[0].EtaSec != 300 {
		t.Fatalf("downloading: %+v", statuses)
	}

	// Imported: the status goes (done), and the request page says it was sent.
	radarr.mu.Lock()
	radarr.queue, radarr.hasFile = nil, true
	radarr.mu.Unlock()
	srv.arrReadQueues(ctx)
	if statuses, _ = st.Statuses(ctx, true); len(statuses) != 0 {
		t.Fatalf("after import: %+v", statuses)
	}
	var list struct{ Items []requestView }
	owner.do("GET", "/requests?status=approved", nil, &list)
	if len(list.Items) != 1 || list.Items[0].Arr == nil || list.Items[0].Arr.State != "sent" {
		t.Fatalf("list: %+v", list.Items)
	}

	// A broken connection: the request says so, and Send again works once fixed.
	owner.do("PUT", "/arr", map[string]any{"radarr": map[string]any{"apiKey": "wrong-key"}}, nil)
	var second requestView
	sam.do("POST", "/requests", map[string]any{"kind": "movie", "tmdbId": 5}, &second)
	var after requestView
	owner.do("POST", "/requests/"+itoa(second.ID)+"/approve", nil, nil)
	for i := 0; i < 100; i++ {
		if req, _ = st.GetRequest(ctx, second.ID); req.ArrState != "" {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if req.ArrState != "error" || !strings.Contains(req.ArrNote, "API key") {
		t.Fatalf("broken: %+v", req)
	}
	owner.do("PUT", "/arr", map[string]any{"radarr": map[string]any{"apiKey": "secret-key-1234"}}, nil)
	if resp := owner.do("POST", "/requests/"+itoa(second.ID)+"/send", nil, &after); resp.StatusCode != 200 || after.Arr == nil || after.Arr.State != "sent" {
		t.Fatalf("send again: %d %+v", resp.StatusCode, after.Arr)
	}
}
