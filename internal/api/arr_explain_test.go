package api

import (
	"testing"
	"time"

	"kinara/internal/arr"
)

func TestMovieReason(t *testing.T) {
	now := time.Date(2026, 11, 17, 12, 0, 0, 0, time.UTC)
	day := 24 * time.Hour
	old := now.Add(-time.Hour)
	cases := []struct {
		m       arr.MovieInfo
		history []arr.HistoryEvent
		empty   bool
		want    string
		done    bool
	}{
		{m: arr.MovieInfo{HasFile: true, Monitored: true}, done: true},
		{m: arr.MovieInfo{Monitored: false}, want: "Not monitored in Radarr, so it won't be downloaded"},
		{m: arr.MovieInfo{Monitored: true, MinimumAvailability: "released", InCinemas: now.Add(-5 * day), DigitalRelease: now.Add(30 * day)},
			want: "In cinemas now; out on digital Dec 17"},
		{m: arr.MovieInfo{Monitored: true, MinimumAvailability: "released", InCinemas: now.Add(-5 * day)},
			want: "In cinemas now; waiting for the digital release"},
		{m: arr.MovieInfo{Monitored: true, MinimumAvailability: "released", InCinemas: now.Add(60 * day), PhysicalRelease: now.Add(200 * day)},
			want: "Waiting for its release: out on disc Jun 5, 2027"},
		{m: arr.MovieInfo{Monitored: true, MinimumAvailability: "inCinemas", InCinemas: now.Add(3 * day)},
			want: "Waiting for it to reach cinemas (Nov 20)"},
		{m: arr.MovieInfo{Monitored: true, IsAvailable: true, Added: now.Add(-time.Minute)}, want: "Looking for a release"},
		{m: arr.MovieInfo{Monitored: true, IsAvailable: true, Added: old}, want: "No release found yet; Radarr keeps checking on its own"},
		{m: arr.MovieInfo{Monitored: true, IsAvailable: true, Added: old}, empty: true, want: "Prowlarr has no indexers, so Radarr has nowhere to search"},
		{m: arr.MovieInfo{Monitored: true, IsAvailable: true, Added: old}, history: []arr.HistoryEvent{{EventType: "downloadFailed"}, {EventType: "grabbed"}},
			want: "A download failed; Radarr is looking for another"},
	}
	for i, c := range cases {
		got := movieReason(c.m, c.history, c.empty, now)
		if got.done != c.done || got.message != c.want {
			t.Errorf("%d: %+v, want %q done %v", i, got, c.want, c.done)
		}
	}
}

func TestShowReason(t *testing.T) {
	now := time.Date(2026, 11, 17, 12, 0, 0, 0, time.UTC)
	day := 24 * time.Hour
	sh := arr.SeriesInfo{Monitored: true, Added: now.Add(-time.Hour)}
	ep := func(s, e int, has bool, air time.Time) arr.EpisodeInfo {
		return arr.EpisodeInfo{SeasonNumber: s, EpisodeNumber: e, Monitored: true, HasFile: has, AirDateUTC: air}
	}
	past, next := now.Add(-30*day), now.Add(4*day)
	if r := showReason(sh, []arr.EpisodeInfo{ep(1, 1, true, past), ep(1, 2, false, next)}, nil, false, now); r.message != "Waiting for the next episode: airs Nov 21" {
		t.Errorf("waiting: %+v", r)
	}
	if r := showReason(sh, []arr.EpisodeInfo{ep(1, 1, false, past), ep(1, 2, false, past), ep(1, 3, false, next)}, nil, false, now); r.message != "No release found yet for 2 episodes; Sonarr keeps checking; next episode airs Nov 21" {
		t.Errorf("missing: %+v", r)
	}
	if r := showReason(sh, []arr.EpisodeInfo{ep(1, 1, true, past), ep(0, 1, false, past)}, nil, false, now); !r.done {
		t.Errorf("specials don't count, all there: %+v", r)
	}
	if r := showReason(sh, []arr.EpisodeInfo{ep(1, 1, false, past)}, nil, true, now); r.message != "Prowlarr has no indexers, so Sonarr has nowhere to search" {
		t.Errorf("no indexers: %+v", r)
	}
	if r := showReason(arr.SeriesInfo{}, nil, nil, false, now); r.message != "Not monitored in Sonarr, so it won't be downloaded" {
		t.Errorf("unmonitored: %+v", r)
	}
}
