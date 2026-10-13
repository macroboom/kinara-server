package api

import (
	"io"
	"log/slog"
	"net/http/httptest"
	"strings"
	"testing"

	"kinora/internal/scanner"
	"kinora/internal/store"
)

// TestTemplatesRender executes every page with sample data, so template
// mistakes fail here instead of in the browser.
func TestUnplacedReason(t *testing.T) {
	for rel, want := range map[string]string{
		`Bill Burr Drop Dead Years (2025) 2160p 4K WEB 5.1-LAMA\Bill.Burr.Drop.Dead.Years.2025.2160p.4K.WEB.x265.10bit.AAC5.1-LAMA.mkv`: "Looks like a movie",
		`Succession S02 2160p MAX WEB-DL\Succession.S02.2160p.MAX.WEB-DL.DD+5.1.Atmos.DoVi.HDR.H.265-playWEB.mkv`:                       "Has a season number",
		`Random Folder\clip.mkv`: "No season and episode",
	} {
		if got := unplacedReason(store.KindTV, rel); !strings.HasPrefix(got, want) {
			t.Errorf("%s: got %q", rel, got)
		}
	}
}

func TestTemplatesRender(t *testing.T) {
	s := &Server{Log: slog.New(slog.NewTextHandler(io.Discard, nil))}
	s.Handler() // parses the templates

	admin := store.User{ID: 1, Username: "alex", IsAdmin: true}
	movie := store.Item{ID: "m1", LibraryID: 1, Kind: "movie", Title: "Night Shift <b>", Year: 2023,
		SeasonNumber: -1, EpisodeNumber: -1, MatchState: "matched", MatchConfidence: 0.97, Rating: 7.8,
		RuntimeMin: 112, Overview: "A night.", AirDate: "2023-05-01"}
	ep := store.Item{ID: "e1", Kind: "episode", Title: "Low Tide", SeasonNumber: 2, EpisodeNumber: 5, ParentID: "s2"}
	lib := store.Library{ID: 1, Name: "Movies", Path: "/media/Movies", Kind: "movies", LastScanAt: 1760000000}
	scan := scanner.Progress{LibraryID: 1, Running: true, Phase: "walking", FilesSeen: 10, New: 3}

	pages := map[string]page{
		"setup.html": {Title: "Set up", Error: "oops"},
		"login.html": {Title: "Sign in"},
		"dashboard.html": {Title: "Kinora", User: admin, Warnings: []string{"No TMDB key"}, Message: "hi",
			Data: map[string]any{
				"Libraries": []libraryView{{Library: lib, Stats: store.LibraryStats{Movies: 5, Unmatched: 1, Files: 6}, Scan: &scan}},
				"Recent":    []store.Item{movie},
				"Continue":  nil,
			}},
		"library.html": {Title: "Movies", User: admin, Data: map[string]any{
			"Library": lib, "Stats": store.LibraryStats{Movies: 1}, "Tab": "", "Page": 1, "Sort": "added",
			"Items": []store.Item{movie}, "HasNext": true, "Scan": scan}},
		"library.html#review": {Title: "Movies", User: admin, Data: map[string]any{
			"Library": lib, "Stats": store.LibraryStats{Unmatched: 1}, "Tab": "review", "Page": 0,
			"Unmatched": []store.Item{movie}}},
		"library.html#files": {Title: "Movies", User: admin, Data: map[string]any{
			"Library": lib, "Stats": store.LibraryStats{}, "Tab": "files", "Page": 0,
			"Unplaced":    []unplacedRow{{Version: store.Version{RelPath: "x.mkv", Size: 1 << 30}, Reason: "why"}},
			"ProbeErrors": []store.Version{{RelPath: "y.mkv", ProbeError: "bad"}}}},
		"item.html": {Title: "Night Shift", User: admin, Data: map[string]any{
			"Item": movie, "Origin": "http://localhost:8420", "Token": "tok",
			"Versions": []VersionView{{Version: store.Version{ID: "v1", RelPath: "Night Shift (2023).mkv",
				Width: 3840, Height: 2160, VideoCodec: "hevc", HDR: "dolby_vision", DVProfile: 7, Container: "mkv",
				DurationSec: 6720, Size: 40 << 30, Bitrate: 58_000_000},
				Streams:   []store.Stream{{Index: 1, Type: "audio", Codec: "truehd", Channels: 8, Default: true}},
				StreamURL: "/api/v1/stream/v1"}},
			"Children": []childRow{{Item: ep, Watch: store.WatchState{Completed: true}}},
			"Watch":    store.WatchState{PositionSec: 600},
			"NextUp":   &nextUpView{Reason: "You finished S02E04 yesterday", Gaps: []gapView{{ID: "g", Season: 1, Episode: 2}}},
			"CanMatch": true}},
	}
	for name, p := range pages {
		tmpl, _, _ := strings.Cut(name, "#")
		rec := httptest.NewRecorder()
		if err := s.tmpl.ExecuteTemplate(rec, tmpl, p); err != nil {
			t.Errorf("%s: %v", name, err)
			continue
		}
		body := rec.Body.String()
		if strings.Contains(body, "<b>") {
			t.Errorf("%s: title not escaped", name)
		}
		if !strings.Contains(body, "</html>") {
			t.Errorf("%s: page incomplete", name)
		}
	}
}
