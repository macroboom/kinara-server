package watch

import (
	"testing"
	"time"

	"kinara/internal/store"
)

func eps() []store.EpisodeRef {
	return []store.EpisodeRef{
		{ID: "sp1", Season: 0, Episode: 1},
		{ID: "e11", Season: 1, Episode: 1},
		{ID: "e12", Season: 1, Episode: 2},
		{ID: "e13", Season: 1, Episode: 3},
		{ID: "e21", Season: 2, Episode: 1},
		{ID: "e22", Season: 2, Episode: 2},
	}
}

var now = time.Date(2026, 10, 12, 20, 0, 0, 0, time.UTC)

func done(at time.Time) store.WatchState {
	return store.WatchState{Completed: true, LastPlayedAt: at.Unix()}
}

func TestNothingWatched(t *testing.T) {
	r := Compute(eps(), nil, now)
	if r.Next == nil || r.Next.ID != "e11" || r.InProgress || len(r.Gaps) != 0 {
		t.Fatalf("got %+v", r)
	}
}

func TestNextAfterFinished(t *testing.T) {
	r := Compute(eps(), map[string]store.WatchState{
		"e11": done(now.Add(-72 * time.Hour)),
		"e12": done(now.Add(-24 * time.Hour)),
	}, now)
	if r.Next == nil || r.Next.ID != "e13" {
		t.Fatalf("next = %+v", r.Next)
	}
	if r.Reason != "You finished S01E02 yesterday" {
		t.Errorf("reason %q", r.Reason)
	}
}

func TestInProgress(t *testing.T) {
	r := Compute(eps(), map[string]store.WatchState{
		"e11": done(now),
		"e12": {PositionSec: 600},
	}, now)
	if r.Next == nil || r.Next.ID != "e12" || !r.InProgress {
		t.Fatalf("got %+v", r)
	}
}

func TestBriefSampleIgnored(t *testing.T) {
	// Opening an episode for 30 seconds doesn't move the pointer.
	r := Compute(eps(), map[string]store.WatchState{
		"e11": done(now),
		"e22": {PositionSec: 30},
	}, now)
	if r.Next == nil || r.Next.ID != "e12" {
		t.Fatalf("next = %+v", r.Next)
	}
}

func TestGapsNeverChosen(t *testing.T) {
	// Skipped S01E02: next is still after the furthest watched, and the
	// skipped one is reported as a gap.
	r := Compute(eps(), map[string]store.WatchState{
		"e11": done(now),
		"e13": done(now),
	}, now)
	if r.Next == nil || r.Next.ID != "e21" {
		t.Fatalf("next = %+v", r.Next)
	}
	if len(r.Gaps) != 1 || r.Gaps[0].ID != "e12" {
		t.Errorf("gaps = %+v", r.Gaps)
	}
}

func TestRewatchDoesNotMovePointer(t *testing.T) {
	// Rewatching S01E01 after finishing S02E01 keeps S02E02 as next.
	r := Compute(eps(), map[string]store.WatchState{
		"e11": done(now),
		"e12": done(now.Add(-48 * time.Hour)),
		"e13": done(now.Add(-48 * time.Hour)),
		"e21": done(now.Add(-24 * time.Hour)),
	}, now)
	if r.Next == nil || r.Next.ID != "e22" {
		t.Fatalf("next = %+v", r.Next)
	}
}

func TestCaughtUp(t *testing.T) {
	states := map[string]store.WatchState{}
	for _, e := range eps() {
		states[e.ID] = done(now)
	}
	r := Compute(eps(), states, now)
	if r.Next != nil || r.Reason != "You're all caught up" {
		t.Fatalf("got %+v", r)
	}
}

func TestSpecialsIgnored(t *testing.T) {
	r := Compute(eps(), map[string]store.WatchState{"sp1": done(now)}, now)
	if r.Next == nil || r.Next.ID != "e11" {
		t.Fatalf("next = %+v", r.Next)
	}
}

func TestWhen(t *testing.T) {
	for _, c := range []struct {
		ago  time.Duration
		want string
	}{
		{time.Hour, "today"},
		{24 * time.Hour, "yesterday"},
		{72 * time.Hour, "3 days ago"},
		{20 * 24 * time.Hour, "on Sep 22"},
	} {
		if got := When(now.Add(-c.ago).Unix(), now); got != c.want {
			t.Errorf("When(-%v) = %q, want %q", c.ago, got, c.want)
		}
	}
}

func TestIsWatched(t *testing.T) {
	if !IsWatched(2760, 3000) || IsWatched(2700, 3000) || IsWatched(100, 0) {
		t.Error("92% rule")
	}
}
