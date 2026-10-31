//go:build !nosqlite

package scanner

import (
	"context"
	"io"
	"io/fs"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"testing"
	"time"

	"kinara/internal/probe"
	"kinara/internal/store"
)

// walkFollow goes through a linked root and linked folders inside it, keeps
// the paths as they are under the root, and stops at a link loop.
func TestWalkFollowLinks(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlinks need privileges on Windows")
	}
	real := t.TempDir()
	extra := t.TempDir()
	must(t, os.MkdirAll(filepath.Join(real, "Show", "Season 1"), 0o755))
	must(t, os.WriteFile(filepath.Join(real, "Show", "Season 1", "a.mkv"), nil, 0o644))
	must(t, os.WriteFile(filepath.Join(extra, "b.mkv"), nil, 0o644))
	must(t, os.Symlink(extra, filepath.Join(real, "Show", "Season 2")))
	must(t, os.Symlink(real, filepath.Join(real, "Show", "loop")))

	root := filepath.Join(t.TempDir(), "media")
	must(t, os.Symlink(real, root))

	var files []string
	err := walkFollow(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			t.Fatalf("%s: %v", path, err)
		}
		if !d.IsDir() {
			rel, _ := filepath.Rel(root, path)
			files = append(files, filepath.ToSlash(rel))
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	sort.Strings(files)
	if len(files) != 2 || files[0] != "Show/Season 1/a.mkv" || files[1] != "Show/Season 2/b.mkv" {
		t.Fatalf("files %v", files)
	}
}

// A library whose folder is a link scans its files; a file touched a
// moment ago stays in the library instead of being reported as removed.
func TestScanLinkedRootAndFreshFile(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlinks need privileges on Windows")
	}
	for _, bin := range []string{"ffmpeg", "ffprobe"} {
		if _, err := exec.LookPath(bin); err != nil {
			t.Skip(bin + " not installed")
		}
	}
	ctx := context.Background()
	real := t.TempDir()
	var paths []string
	for _, name := range []string{"First.mp4", "Second.mp4"} {
		path := filepath.Join(real, name)
		cmd := exec.Command("ffmpeg", "-loglevel", "error", "-f", "lavfi", "-i", "testsrc2=size=160x90:rate=10:duration=1",
			"-c:v", "libx264", "-pix_fmt", "yuv420p", path)
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Skipf("can't make a test video: %v %s", err, out)
		}
		old := time.Now().Add(-time.Hour)
		_ = os.Chtimes(path, old, old)
		paths = append(paths, path)
	}
	root := filepath.Join(t.TempDir(), "videos")
	must(t, os.Symlink(real, root))

	st, err := store.Open(filepath.Join(t.TempDir(), "db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	lib, err := st.CreateLibrary(ctx, "Home videos", root, store.KindOther)
	if err != nil {
		t.Fatal(err)
	}
	sc := &Scanner{Store: st, Prober: &probe.Prober{}, Log: slog.New(slog.NewTextHandler(io.Discard, nil)), Workers: 2, StableAge: time.Minute}
	if _, err := sc.Run(ctx, lib.ID); err != nil {
		t.Fatal(err)
	}
	items, _ := st.ListItems(ctx, store.ItemFilter{LibraryID: lib.ID, Limit: 50})
	if len(items) != 2 {
		t.Fatalf("items through a linked folder: %d, want 2", len(items))
	}

	// Retagged a moment ago: still in the library after the next scan.
	now := time.Now()
	must(t, os.Chtimes(paths[0], now, now))
	if _, err := sc.Run(ctx, lib.ID); err != nil {
		t.Fatal(err)
	}
	items, _ = st.ListItems(ctx, store.ItemFilter{LibraryID: lib.ID, Limit: 50})
	if len(items) != 2 {
		t.Fatalf("items after touching one: %d, want 2", len(items))
	}
	if p, _ := sc.Progress(lib.ID); p.Missing != 0 {
		t.Fatalf("missing %d, want 0", p.Missing)
	}
}

// A folder inside the library that can't be read keeps its files.
func TestScanUnreadableFolderKeepsFiles(t *testing.T) {
	if runtime.GOOS == "windows" || os.Geteuid() == 0 {
		t.Skip("needs a non-root Unix user to make a folder unreadable")
	}
	for _, bin := range []string{"ffmpeg", "ffprobe"} {
		if _, err := exec.LookPath(bin); err != nil {
			t.Skip(bin + " not installed")
		}
	}
	ctx := context.Background()
	root := t.TempDir()
	sub := filepath.Join(root, "Season 2")
	must(t, os.MkdirAll(sub, 0o755))
	for _, path := range []string{filepath.Join(root, "A.mp4"), filepath.Join(sub, "B.mp4")} {
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
	lib, err := st.CreateLibrary(ctx, "Home videos", root, store.KindOther)
	if err != nil {
		t.Fatal(err)
	}
	sc := &Scanner{Store: st, Prober: &probe.Prober{}, Log: slog.New(slog.NewTextHandler(io.Discard, nil)), Workers: 2, StableAge: time.Nanosecond}
	if _, err := sc.Run(ctx, lib.ID); err != nil {
		t.Fatal(err)
	}
	must(t, os.Chmod(sub, 0))
	defer os.Chmod(sub, 0o755)
	if _, err := sc.Run(ctx, lib.ID); err != nil {
		t.Fatal(err)
	}
	items, _ := st.ListItems(ctx, store.ItemFilter{LibraryID: lib.ID, Limit: 50})
	if len(items) != 2 {
		t.Fatalf("items with an unreadable folder: %d, want 2", len(items))
	}
}

func must(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}
