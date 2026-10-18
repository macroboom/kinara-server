package api

import (
	"testing"
	"time"
)

func TestProgressWorthSending(t *testing.T) {
	s := &Server{}
	at := time.Unix(1_800_000_000, 0)
	p := func(v float64) *float64 { return &v }
	steps := []struct {
		after    time.Duration
		newState bool
		progress *float64
		want     bool
		why      string
	}{
		{0, true, p(0.10), true, "first update"},
		{2 * time.Second, false, p(0.30), false, "too soon"},
		{12 * time.Second, false, p(0.12), false, "barely moved"},
		{13 * time.Second, false, p(0.20), true, "moved and waited"},
		{14 * time.Second, true, p(0.20), true, "a new state always goes"},
		{40 * time.Second, false, nil, false, "no progress, same state"},
	}
	for _, st := range steps {
		if got := s.progressWorthSending("k", st.newState, st.progress, at.Add(st.after)); got != st.want {
			t.Fatalf("%s: got %v", st.why, got)
		}
	}
	s.forgetProgress("k")
	if !s.progressWorthSending("k", false, p(0.5), at.Add(time.Minute)) {
		t.Fatal("after forgetting, the next update goes out")
	}
}

func TestPulseRevisionsMove(t *testing.T) {
	s := &Server{}
	a := s.libraryRevision()
	if a == 0 || s.libraryRevision() != a {
		t.Fatal("library revision should start from the clock and hold")
	}
	s.libraryChanged()
	if s.libraryRevision() == a {
		t.Fatal("library revision should move")
	}
}
