//go:build !nosqlite

package jobs

import (
	"context"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"kinora/internal/ffmpeg"
	"kinora/internal/store"
)

// A picture is made for an "Other videos" title and becomes its backdrop.
func TestMakeThumb(t *testing.T) {
	if _, err := exec.LookPath("ffmpeg"); err != nil {
		t.Skip("ffmpeg not installed")
	}
	ctx := context.Background()
	dir := t.TempDir()
	video := filepath.Join(dir, "clip.mp4")
	if out, err := exec.Command("ffmpeg", "-loglevel", "error", "-f", "lavfi", "-i", "testsrc2=size=320x180:rate=10:duration=3",
		"-c:v", "libx264", "-pix_fmt", "yuv420p", video).CombinedOutput(); err != nil {
		t.Skipf("can't make a test video: %v %s", err, out)
	}
	st, err := store.Open(filepath.Join(dir, "db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	lib, _ := st.CreateLibrary(ctx, "Clips", dir, store.KindOther)
	it := store.Item{LibraryID: lib.ID, Kind: store.ItemMovie, Title: "clip", SortTitle: "clip", MatchState: store.MatchManual,
		SeasonNumber: -1, EpisodeNumber: -1}
	if err := st.CreateItem(ctx, &it); err != nil {
		t.Fatal(err)
	}
	v := store.Version{LibraryID: lib.ID, ItemID: it.ID, Path: video, RelPath: "clip.mp4", Size: 1, DurationSec: 3}
	if err := st.SaveVersion(ctx, &v, nil); err != nil {
		t.Fatal(err)
	}
	w := &Worker{Store: st, Tool: &ffmpeg.Tool{}, Log: slog.New(slog.NewTextHandler(io.Discard, nil)), ThumbDir: filepath.Join(dir, "thumbs")}
	target, ok := w.nextThumb(ctx)
	if !ok || target.ItemID != it.ID {
		t.Fatalf("queue = %+v %v", target, ok)
	}
	w.makeThumb(ctx, target)
	url, err := st.ImageURL(ctx, it.ID, "backdrop")
	if err != nil || !strings.HasPrefix(url, "file:") {
		t.Fatalf("backdrop = %q, %v", url, err)
	}
	if fi, err := os.Stat(strings.TrimPrefix(url, "file:")); err != nil || fi.Size() == 0 {
		t.Fatalf("picture file: %v", err)
	}
	if _, ok := w.nextThumb(ctx); ok {
		t.Fatal("still queued after its picture was made")
	}
	st2 := w.Status()
	if st2.Current != nil || len(st2.Recent) != 1 || st2.Recent[0].Kind != "picture" || st2.Recent[0].Name != "clip.mp4" || st2.Recent[0].Failed {
		t.Fatalf("log = %+v", st2)
	}
}

// A movie and a show that didn't match get a frame as their backdrop and
// poster; a matched movie gets nothing.
func TestMakeThumbUnmatched(t *testing.T) {
	if _, err := exec.LookPath("ffmpeg"); err != nil {
		t.Skip("ffmpeg not installed")
	}
	ctx := context.Background()
	dir := t.TempDir()
	video := filepath.Join(dir, "clip.mp4")
	if out, err := exec.Command("ffmpeg", "-loglevel", "error", "-f", "lavfi", "-i", "testsrc2=size=320x180:rate=10:duration=3",
		"-c:v", "libx264", "-pix_fmt", "yuv420p", video).CombinedOutput(); err != nil {
		t.Skipf("can't make a test video: %v %s", err, out)
	}
	st, err := store.Open(filepath.Join(dir, "db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	movies, _ := st.CreateLibrary(ctx, "Movies", filepath.Join(dir, "m"), store.KindMovies)
	tv, _ := st.CreateLibrary(ctx, "TV", filepath.Join(dir, "t"), store.KindTV)

	add := func(it *store.Item, lib int64, rel string) {
		t.Helper()
		if err := st.CreateItem(ctx, it); err != nil {
			t.Fatal(err)
		}
		if rel != "" {
			// Each file needs its own path.
			own := filepath.Join(dir, strings.ReplaceAll(rel, "/", "_"))
			if err := os.Link(video, own); err != nil {
				t.Fatal(err)
			}
			v := store.Version{LibraryID: lib, ItemID: it.ID, Path: own, RelPath: rel, Size: 1, DurationSec: 3}
			if err := st.SaveVersion(ctx, &v, nil); err != nil {
				t.Fatal(err)
			}
		}
	}
	lost := store.Item{LibraryID: movies.ID, Kind: store.ItemMovie, Title: "Lost", SortTitle: "Lost", MatchState: store.MatchUnmatched, SeasonNumber: -1, EpisodeNumber: -1}
	add(&lost, movies.ID, "lost.mp4")
	found := store.Item{LibraryID: movies.ID, Kind: store.ItemMovie, Title: "Found", SortTitle: "Found", MatchState: store.MatchMatched, SeasonNumber: -1, EpisodeNumber: -1}
	add(&found, movies.ID, "found.mp4")
	show := store.Item{LibraryID: tv.ID, Kind: store.ItemShow, Title: "Mystery Show", SortTitle: "Mystery Show", MatchState: store.MatchUnmatched, SeasonNumber: -1, EpisodeNumber: -1}
	add(&show, 0, "")
	season := store.Item{LibraryID: tv.ID, Kind: store.ItemSeason, ParentID: show.ID, Title: "Season 1", SortTitle: "Season 1", SeasonNumber: 1, EpisodeNumber: -1}
	add(&season, 0, "")
	ep := store.Item{LibraryID: tv.ID, Kind: store.ItemEpisode, ParentID: season.ID, Title: "One", SortTitle: "One", SeasonNumber: 1, EpisodeNumber: 1}
	add(&ep, tv.ID, "Mystery Show/S01E01.mp4")

	w := &Worker{Store: st, Tool: &ffmpeg.Tool{}, Log: slog.New(slog.NewTextHandler(io.Discard, nil)), ThumbDir: filepath.Join(dir, "thumbs")}
	done := map[string]bool{}
	for range 4 {
		target, ok := w.nextThumb(ctx)
		if !ok {
			break
		}
		w.makeThumb(ctx, target)
		done[target.ItemID] = true
	}
	if !done[lost.ID] || !done[show.ID] || done[found.ID] || len(done) != 2 {
		t.Fatalf("pictures made for %v", done)
	}
	for _, id := range []string{lost.ID, show.ID} {
		if url, _ := st.ImageURL(ctx, id, "backdrop"); !strings.HasPrefix(url, "file:") {
			t.Errorf("%s backdrop = %q", id, url)
		}
		if url, _ := st.ImageURL(ctx, id, "poster"); !strings.HasPrefix(url, "file:") {
			t.Errorf("%s poster = %q", id, url)
		}
	}
}
