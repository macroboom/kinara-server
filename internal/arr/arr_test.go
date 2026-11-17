package arr

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
)

// fake is a tiny Radarr or Sonarr: it remembers what was posted.
type fake struct {
	mu       sync.Mutex
	app      string
	movies   []map[string]any
	series   []map[string]any
	posted   []map[string]any
	put      []map[string]any
	commands []map[string]any
	monitor  []map[string]any
}

func (f *fake) server(t *testing.T) *httptest.Server {
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		if r.Header.Get("X-Api-Key") != "key" {
			w.WriteHeader(401)
			return
		}
		var body map[string]any
		if r.Body != nil {
			b, _ := io.ReadAll(r.Body)
			json.Unmarshal(b, &body)
		}
		send := func(v any) { json.NewEncoder(w).Encode(v) }
		p := strings.TrimPrefix(r.URL.Path, "/base/api/v3/")
		switch {
		case p == "system/status":
			send(map[string]any{"appName": f.app, "version": "5.0"})
		case p == "qualityprofile":
			send([]map[string]any{{"id": 4, "name": "HD-1080p"}})
		case p == "rootfolder":
			send([]map[string]any{{"id": 1, "path": "/media/movies", "freeSpace": 1 << 40}})
		case p == "tag", p == "languageprofile":
			send([]any{})
		case p == "movie" && r.Method == "GET":
			out := []map[string]any{}
			for _, m := range f.movies {
				if json.Number(r.URL.Query().Get("tmdbId")).String() == jsonInt(m["tmdbId"]) {
					out = append(out, m)
				}
			}
			send(out)
		case p == "movie/lookup/tmdb":
			send(map[string]any{"title": "Heat", "year": 1995, "tmdbId": 949, "images": []any{}})
		case p == "movie" && r.Method == "POST":
			body["id"] = 77
			f.posted = append(f.posted, body)
			send(body)
		case strings.HasPrefix(p, "movie/") && r.Method == "PUT", strings.HasPrefix(p, "series/") && r.Method == "PUT":
			f.put = append(f.put, body)
			send(body)
		case p == "command":
			f.commands = append(f.commands, body)
			send(map[string]any{"id": 1})
		case p == "series" && r.Method == "GET":
			send(f.series)
		case p == "series/lookup":
			send([]map[string]any{{"title": "Severance", "tvdbId": 371980, "seasons": []any{
				map[string]any{"seasonNumber": 0}, map[string]any{"seasonNumber": 1}, map[string]any{"seasonNumber": 2}}}})
		case p == "series" && r.Method == "POST":
			body["id"] = 12
			f.posted = append(f.posted, body)
			send(body)
		case p == "episode" && r.Method == "GET":
			send([]map[string]any{{"id": 501, "seasonNumber": 1, "episodeNumber": 1, "hasFile": false}})
		case p == "episode/monitor":
			f.monitor = append(f.monitor, body)
			send([]any{})
		default:
			t.Errorf("unexpected %s %s", r.Method, r.URL)
			w.WriteHeader(404)
		}
	}))
}

func jsonInt(v any) string {
	b, _ := json.Marshal(v)
	return string(b)
}

func TestRadarr(t *testing.T) {
	f := &fake{app: "Radarr"}
	srv := f.server(t)
	defer srv.Close()
	ctx := context.Background()
	c := &Client{Kind: Radarr, BaseURL: srv.URL + "/base/", APIKey: "key"}
	o, err := c.Check(ctx)
	if err != nil || len(o.Profiles) != 1 || o.RootFolders[0].Path != "/media/movies" {
		t.Fatalf("check: %+v %v", o, err)
	}
	if _, err := (&Client{Kind: Sonarr, BaseURL: srv.URL + "/base", APIKey: "key"}).Check(ctx); err == nil || !strings.Contains(err.Error(), "not Sonarr") {
		t.Errorf("Radarr in the Sonarr box: %v", err)
	}
	if _, err := (&Client{Kind: Radarr, BaseURL: srv.URL + "/base", APIKey: "nope"}).Check(ctx); err == nil || !strings.Contains(err.Error(), "API key") {
		t.Errorf("bad key: %v", err)
	}

	add := AddOptions{ProfileID: 4, RootFolder: "/media/movies", Search: true}
	res, err := c.AddMovie(ctx, 949, add)
	if err != nil || !res.Added || res.ID != 77 {
		t.Fatalf("add: %+v %v", res, err)
	}
	m := f.posted[0]
	if m["qualityProfileId"] != float64(4) || m["rootFolderPath"] != "/media/movies" || m["minimumAvailability"] != "released" ||
		m["addOptions"].(map[string]any)["searchForMovie"] != true || m["title"] != "Heat" {
		t.Errorf("posted %v", m)
	}

	// Already in Radarr, not monitored: monitored and searched.
	f.movies = []map[string]any{{"id": 5, "tmdbId": 949, "monitored": false, "hasFile": false}}
	res, err = c.AddMovie(ctx, 949, add)
	if err != nil || res.Added || len(f.put) != 1 || f.put[0]["monitored"] != true || f.commands[0]["name"] != "MoviesSearch" {
		t.Fatalf("existing: %+v %v put %v commands %v", res, err, f.put, f.commands)
	}
	// Already downloaded: nothing to do.
	f.movies[0]["hasFile"] = true
	if res, err := c.AddMovie(ctx, 949, add); err != nil || !res.HasFile || len(f.commands) != 1 {
		t.Errorf("has file: %+v %v", res, err)
	}
}

func TestSonarr(t *testing.T) {
	f := &fake{app: "Sonarr"}
	srv := f.server(t)
	defer srv.Close()
	ctx := context.Background()
	c := &Client{Kind: Sonarr, BaseURL: srv.URL + "/base", APIKey: "key"}
	add := AddOptions{ProfileID: 4, RootFolder: "/media/tv", Search: true, SeasonFolder: true}

	res, err := c.AddSeries(ctx, 371980, "", ModeSeasons, []int{2}, add)
	if err != nil || !res.Added {
		t.Fatalf("add: %+v %v", res, err)
	}
	s := f.posted[0]
	seasons := s["seasons"].([]any)
	mon := func(i int) bool { return seasons[i].(map[string]any)["monitored"].(bool) }
	if mon(0) || mon(1) || !mon(2) || s["seasonFolder"] != true || s["addOptions"].(map[string]any)["searchForMissingEpisodes"] != true {
		t.Errorf("season 2 only: %v", s)
	}

	f.posted = nil
	if _, err := c.AddSeries(ctx, 371980, "", ModeAll, nil, add); err != nil {
		t.Fatal(err)
	}
	seasons = f.posted[0]["seasons"].([]any)
	if mon(0) || !mon(1) || !mon(2) {
		t.Errorf("all seasons but specials: %v", seasons)
	}

	// Sonarr has it with season 1 only: season 2 joins, and is searched.
	f.series = []map[string]any{{"id": 12, "tvdbId": 371980, "monitored": true, "seasons": []any{
		map[string]any{"seasonNumber": 1, "monitored": true}, map[string]any{"seasonNumber": 2, "monitored": false}}}}
	res, err = c.AddSeries(ctx, 371980, "", ModeSeasons, []int{2}, add)
	if err != nil || res.Added || len(f.put) != 1 {
		t.Fatalf("existing: %+v %v %v", res, err, f.put)
	}
	if len(f.commands) != 1 || f.commands[0]["name"] != "SeasonSearch" || f.commands[0]["seasonNumber"] != float64(2) {
		t.Errorf("searched %v", f.commands)
	}
	// The first episode only.
	if _, err := c.AddSeries(ctx, 371980, "", ModePilot, nil, add); err != nil {
		t.Fatal(err)
	}
	if len(f.monitor) != 1 || f.commands[len(f.commands)-1]["name"] != "EpisodeSearch" {
		t.Errorf("pilot: monitor %v commands %v", f.monitor, f.commands)
	}
}

func TestQueueItem(t *testing.T) {
	q := QueueItem{Size: 1000, SizeLeft: 250, TimeLeft: "1.02:03:04"}
	if q.Progress() != 0.75 || q.EtaSeconds() != 86400+2*3600+3*60+4 {
		t.Errorf("progress %v eta %d", q.Progress(), q.EtaSeconds())
	}
	if (QueueItem{TimeLeft: "00:10:00"}).EtaSeconds() != 600 || (QueueItem{}).Progress() != -1 {
		t.Error("short eta or unknown size")
	}
}
