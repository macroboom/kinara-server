package watcher

import (
	"bytes"
	"context"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

type harness struct {
	t     *testing.T
	w     *Watcher
	clock time.Time
	fired []int64
	roots []Root
	logs  *bytes.Buffer
}

func newHarness(t *testing.T, roots ...Root) *harness {
	t.Helper()
	h := &harness{t: t, clock: time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC), roots: roots, logs: &bytes.Buffer{}}
	h.w = &Watcher{
		Roots:    func(context.Context) ([]Root, error) { return h.roots, nil },
		OnChange: func(id int64) { h.fired = append(h.fired, id) },
		Interval: time.Minute,
		Settle:   2 * time.Minute,
		FollowUp: 90 * time.Second,
		Log:      slog.New(slog.NewTextHandler(h.logs, &slog.HandlerOptions{Level: slog.LevelDebug})),
		now:      func() time.Time { return h.clock },
	}
	return h
}

// poll advances the fake clock by d and polls once.
func (h *harness) poll(d time.Duration) {
	h.t.Helper()
	h.clock = h.clock.Add(d)
	h.w.pollOnce(context.Background())
}

func (h *harness) wantFired(n int) {
	h.t.Helper()
	if len(h.fired) != n {
		h.t.Fatalf("fired %d times (%v), want %d", len(h.fired), h.fired, n)
	}
}

// write creates or replaces a file with size bytes and the given mtime.
func write(t *testing.T, path string, size int, mod time.Time) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, make([]byte, size), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(path, mod, mod); err != nil {
		t.Fatal(err)
	}
}

func oneRoot(t *testing.T) (*harness, string) {
	dir := t.TempDir()
	h := newHarness(t, Root{LibraryID: 1, Path: dir})
	old := h.clock.Add(-time.Hour)
	write(t, filepath.Join(dir, "Movies", "a.mkv"), 10, old)
	h.poll(0) // baseline
	return h, dir
}

func TestBaselineNoChange(t *testing.T) {
	h, _ := oneRoot(t)
	for i := 0; i < 5; i++ {
		h.poll(time.Minute)
	}
	h.wantFired(0)
}

func TestNewNestedFileFiresOnceAfterSettle(t *testing.T) {
	h, dir := oneRoot(t)
	write(t, filepath.Join(dir, "TV", "Show", "S01", "e1.mp4"), 10, h.clock.Add(-time.Hour))
	h.poll(time.Minute) // detected
	h.wantFired(0)
	h.poll(time.Minute) // 1m quiet
	h.wantFired(0)
	h.poll(time.Minute) // 2m quiet
	h.wantFired(1)
	for i := 0; i < 5; i++ {
		h.poll(time.Minute)
	}
	h.wantFired(1)
}

func TestGrowingFileDelaysFire(t *testing.T) {
	h, dir := oneRoot(t)
	p := filepath.Join(dir, "Movies", "big.mkv")
	for i := 1; i <= 6; i++ {
		h.clock = h.clock.Add(time.Minute)
		write(t, p, i*100, h.clock)
		h.w.pollOnce(context.Background())
		h.wantFired(0)
	}
	h.poll(time.Minute)
	h.wantFired(0)
	h.poll(time.Minute)
	h.wantFired(1)
}

func TestMaxWaitCap(t *testing.T) {
	h, dir := oneRoot(t)
	p := filepath.Join(dir, "Movies", "endless.mkv")
	for i := 1; i <= 29; i++ {
		h.clock = h.clock.Add(time.Minute)
		write(t, p, i*100, h.clock)
		h.w.pollOnce(context.Background())
	}
	h.wantFired(0)
	for i := 30; i <= 32; i++ {
		h.clock = h.clock.Add(time.Minute)
		write(t, p, i*100, h.clock)
		h.w.pollOnce(context.Background())
	}
	if len(h.fired) == 0 {
		t.Fatal("expected fire after max wait")
	}
}

func TestNonVideoIgnored(t *testing.T) {
	h, dir := oneRoot(t)
	old := h.clock.Add(-time.Hour)
	write(t, filepath.Join(dir, "Movies", "a.nfo"), 5, old)
	write(t, filepath.Join(dir, "Movies", "poster.jpg"), 5, old)
	write(t, filepath.Join(dir, ".hidden", "x.mkv"), 5, old)
	write(t, filepath.Join(dir, "@eaDir", "x.mkv"), 5, old)
	for i := 0; i < 5; i++ {
		h.poll(time.Minute)
	}
	h.wantFired(0)
}

func TestRemoveFires(t *testing.T) {
	h, dir := oneRoot(t)
	if err := os.Remove(filepath.Join(dir, "Movies", "a.mkv")); err != nil {
		t.Fatal(err)
	}
	h.poll(time.Minute)
	h.poll(time.Minute)
	h.poll(time.Minute)
	h.wantFired(1)
}

func TestMissingRootThenReturn(t *testing.T) {
	h, dir := oneRoot(t)
	gone := dir + ".gone"
	if err := os.Rename(dir, gone); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 4; i++ {
		h.poll(time.Minute)
	}
	h.wantFired(0)
	if n := strings.Count(h.logs.String(), "library root unavailable"); n != 1 {
		t.Fatalf("unavailable warning logged %d times, want 1", n)
	}
	if err := os.Rename(gone, dir); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 4; i++ {
		h.poll(time.Minute)
	}
	h.wantFired(0)
}

func TestFollowUp(t *testing.T) {
	h, dir := oneRoot(t)
	h.w.Settle = 30 * time.Second
	h.clock = h.clock.Add(time.Minute)
	write(t, filepath.Join(dir, "Movies", "new.mkv"), 10, h.clock)
	h.w.pollOnce(context.Background()) // detected
	h.wantFired(0)
	h.poll(time.Minute) // settled, file is 1m old: fires and schedules follow-up
	h.wantFired(1)
	h.poll(time.Minute) // follow-up due at +90s
	h.wantFired(1)
	h.poll(time.Minute)
	h.wantFired(2)
	for i := 0; i < 4; i++ {
		h.poll(time.Minute)
	}
	h.wantFired(2)
}

func TestTwoRoots(t *testing.T) {
	d1, d2 := t.TempDir(), t.TempDir()
	h := newHarness(t, Root{1, d1}, Root{2, d2})
	old := h.clock.Add(-time.Hour)
	write(t, filepath.Join(d1, "a.mkv"), 1, old)
	write(t, filepath.Join(d2, "b.mkv"), 1, old)
	h.poll(0)
	write(t, filepath.Join(d2, "c.mkv"), 1, old)
	for i := 0; i < 4; i++ {
		h.poll(time.Minute)
	}
	if len(h.fired) != 1 || h.fired[0] != 2 {
		t.Fatalf("fired %v, want [2]", h.fired)
	}
}

func TestRootAddedAndRemoved(t *testing.T) {
	h, _ := oneRoot(t)
	d2 := t.TempDir()
	write(t, filepath.Join(d2, "x.mkv"), 1, h.clock.Add(-time.Hour))
	h.roots = append(h.roots, Root{2, d2})
	for i := 0; i < 4; i++ {
		h.poll(time.Minute)
	}
	h.wantFired(0) // new root only baselines
	h.roots = h.roots[:1]
	h.poll(time.Minute)
	if _, ok := h.w.states[2]; ok {
		t.Fatal("removed root state not dropped")
	}
}
