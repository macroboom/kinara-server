//go:build !nosqlite

package scanner

import (
	"context"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"testing"
	"time"

	"kinora/internal/probe"
	"kinora/internal/store"
)

// A library with two folders scans both, each file relative to its own
// folder, and a folder that can't be read fails the scan rather than
// leaving its files looking deleted.
func TestScanTwoFolders(t *testing.T) {
	for _, bin := range []string{"ffmpeg", "ffprobe"} {
		if _, err := exec.LookPath(bin); err != nil {
			t.Skip(bin + " not installed")
		}
	}
	ctx := context.Background()
	a, b := t.TempDir(), t.TempDir()
	for _, path := range []string{filepath.Join(a, "First.mp4"), filepath.Join(b, "more", "Second.mp4")} {
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		cmd := exec.Command("ffmpeg", "-loglevel", "error", "-f", "lavfi", "-i", "testsrc2=size=160x90:rate=10:duration=1",
			"-c:v", "libx264", "-pix_fmt", "yuv420p", path)
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Skipf("can't make a test video: %v %s", err, out)
		}
		old := time.Now().Add(-time.Hour)
		_ = os.Chtimes(path, old, old)
	}
	st, err := store.Open(filepath.Join(t.TempDir(), "db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	lib, err := st.CreateLibrary(ctx, "Home videos", a, store.KindOther)
	if err != nil {
		t.Fatal(err)
	}
	if err := st.AddLibraryFolder(ctx, lib.ID, b); err != nil {
		t.Fatal(err)
	}
	sc := &Scanner{Store: st, Prober: &probe.Prober{}, Log: slog.New(slog.NewTextHandler(io.Discard, nil)), Workers: 2, StableAge: time.Nanosecond}
	if _, err := sc.Run(ctx, lib.ID); err != nil {
		t.Fatal(err)
	}
	known, err := st.LibraryVersions(ctx, lib.ID)
	if err != nil {
		t.Fatal(err)
	}
	var rels []string
	for _, v := range known {
		rels = append(rels, filepath.ToSlash(v.RelPath))
	}
	sort.Strings(rels)
	if len(rels) != 2 || rels[0] != "First.mp4" || rels[1] != "more/Second.mp4" {
		t.Fatalf("files %v", rels)
	}

	// The second folder goes away (an unplugged drive): the scan fails and
	// nothing is marked missing.
	if err := os.RemoveAll(b); err != nil {
		t.Fatal(err)
	}
	if _, err := sc.Run(ctx, lib.ID); err == nil {
		t.Fatal("scan with a missing folder should fail")
	}
	items, _ := st.ListItems(ctx, store.ItemFilter{LibraryID: lib.ID, Limit: 50})
	if len(items) != 2 {
		t.Fatalf("items after failed scan %d, want 2", len(items))
	}
}
