//go:build !nosqlite

package scanner

import (
	"context"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"kinora/internal/probe"
	"kinora/internal/store"
)

// An "Other videos" library: every file is its own title, named from the
// file, never matched, even names that look like samples or episodes.
func TestScanOtherLibrary(t *testing.T) {
	if _, err := exec.LookPath("ffmpeg"); err != nil {
		t.Skip("ffmpeg not installed")
	}
	if _, err := exec.LookPath("ffprobe"); err != nil {
		t.Skip("ffprobe not installed")
	}
	ctx := context.Background()
	dir := t.TempDir()
	for _, name := range []string{"Birthday_Party.mp4", "trips/Show.S01E01.mp4", "trips/sample.mp4"} {
		path := filepath.Join(dir, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		cmd := exec.Command("ffmpeg", "-loglevel", "error", "-f", "lavfi", "-i", "testsrc2=size=160x90:rate=10:duration=1",
			"-c:v", "libx264", "-pix_fmt", "yuv420p", path)
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Skipf("can't make a test video: %v %s", err, out)
		}
		// Files must look settled (not still being copied) to be scanned.
		old := time.Now().Add(-time.Hour)
		_ = os.Chtimes(path, old, old)
	}
	st, err := store.Open(filepath.Join(t.TempDir(), "db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	lib, err := st.CreateLibrary(ctx, "Home videos", dir, store.KindOther)
	if err != nil {
		t.Fatal(err)
	}
	sc := &Scanner{Store: st, Prober: &probe.Prober{}, Log: slog.New(slog.NewTextHandler(io.Discard, nil)), Workers: 2, StableAge: time.Nanosecond}
	if _, err := sc.Run(ctx, lib.ID); err != nil {
		t.Fatal(err)
	}
	items, err := st.ListItems(ctx, store.ItemFilter{LibraryID: lib.ID, Limit: 50})
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]string{}
	for _, it := range items {
		got[it.Title] = it.MatchState
		if it.Kind != store.ItemMovie {
			t.Errorf("%s is a %s", it.Title, it.Kind)
		}
	}
	for _, want := range []string{"Birthday Party", "Show S01E01", "sample"} {
		if state, ok := got[want]; !ok {
			t.Errorf("missing %q in %v", want, got)
		} else if state != store.MatchManual {
			t.Errorf("%q match state %q", want, state)
		}
	}
	// A second scan finds the same titles rather than making new ones.
	if _, err := sc.Run(ctx, lib.ID); err != nil {
		t.Fatal(err)
	}
	again, _ := st.ListItems(ctx, store.ItemFilter{LibraryID: lib.ID, Limit: 50})
	if len(again) != len(items) {
		t.Errorf("rescan made %d titles, had %d", len(again), len(items))
	}
}
