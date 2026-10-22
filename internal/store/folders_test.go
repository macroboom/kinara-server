//go:build !nosqlite

package store

import (
	"context"
	"errors"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func TestLibraryFolders(t *testing.T) {
	ctx := context.Background()
	s := openTest(t)
	lib, err := s.CreateLibrary(ctx, "Movies", "/a", KindMovies)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.AddLibraryFolder(ctx, lib.ID, "/b"); err != nil {
		t.Fatal(err)
	}
	// A folder can't be in two libraries.
	other, _ := s.CreateLibrary(ctx, "Other", "/c", KindTV)
	if err := s.AddLibraryFolder(ctx, other.ID, "/b"); err == nil {
		t.Fatal("same folder added to a second library")
	}
	got, _ := s.GetLibrary(ctx, lib.ID)
	if !reflect.DeepEqual(got.Folders, []string{"/a", "/b"}) || got.Path != "/a" {
		t.Fatalf("folders %v path %q", got.Folders, got.Path)
	}

	if err := s.RenameLibrary(ctx, lib.ID, "Films"); err != nil {
		t.Fatal(err)
	}
	if err := s.RenameLibrary(ctx, 999, "x"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("rename missing: %v", err)
	}

	// Files in /a, in /b, and in /ab (a different folder sharing the prefix).
	keep := Item{LibraryID: lib.ID, Kind: ItemMovie, Title: "Keep", SortTitle: "Keep", SeasonNumber: -1, EpisodeNumber: -1}
	gone := Item{LibraryID: lib.ID, Kind: ItemMovie, Title: "Gone", SortTitle: "Gone", SeasonNumber: -1, EpisodeNumber: -1}
	for _, it := range []*Item{&keep, &gone} {
		if err := s.CreateItem(ctx, it); err != nil {
			t.Fatal(err)
		}
	}
	// File paths use this system's separator, as the scanner's do (a
	// folder's files are found by its path plus the separator).
	in := func(folder string, parts ...string) string {
		return folder + string(filepath.Separator) + strings.Join(parts, string(filepath.Separator))
	}
	for _, v := range []Version{
		{LibraryID: lib.ID, ItemID: gone.ID, Path: in("/a", "gone.mkv"), RelPath: "gone.mkv", Size: 1},
		{LibraryID: lib.ID, ItemID: gone.ID, Path: in("/a", "sub", "gone2.mkv"), RelPath: "sub/gone2.mkv", Size: 1},
		{LibraryID: lib.ID, ItemID: keep.ID, Path: in("/b", "keep.mkv"), RelPath: "keep.mkv", Size: 1},
		{LibraryID: lib.ID, ItemID: keep.ID, Path: in("/ab", "keep.mkv"), RelPath: "keep.mkv", Size: 1},
	} {
		v := v
		if err := s.SaveVersion(ctx, &v, nil); err != nil {
			t.Fatal(err)
		}
	}

	if _, err := s.RemoveLibraryFolder(ctx, lib.ID, "/nope"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("remove unknown: %v", err)
	}
	n, err := s.RemoveLibraryFolder(ctx, lib.ID, "/a")
	if err != nil {
		t.Fatal(err)
	}
	if n != 2 {
		t.Fatalf("dropped %d files, want 2", n)
	}
	got, _ = s.GetLibrary(ctx, lib.ID)
	if got.Name != "Films" || got.Path != "/b" || !reflect.DeepEqual(got.Folders, []string{"/b"}) {
		t.Fatalf("after remove: %+v", got)
	}
	if _, err := s.GetItem(ctx, gone.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("title with no files left should go: %v", err)
	}
	if _, err := s.GetItem(ctx, keep.ID); err != nil {
		t.Fatalf("kept title: %v", err)
	}
	if _, err := s.RemoveLibraryFolder(ctx, lib.ID, "/b"); !errors.Is(err, ErrLastFolder) {
		t.Fatalf("remove last: %v", err)
	}

	libs, _ := s.ListLibraries(ctx)
	if len(libs) != 2 || !reflect.DeepEqual(libs[0].Folders, []string{"/b"}) || !reflect.DeepEqual(libs[1].Folders, []string{"/c"}) {
		t.Fatalf("list: %+v", libs)
	}

	// Deleting the library frees its folders for another.
	if err := s.DeleteLibrary(ctx, lib.ID); err != nil {
		t.Fatal(err)
	}
	if err := s.AddLibraryFolder(ctx, other.ID, "/b"); err != nil {
		t.Fatalf("folder still held after delete: %v", err)
	}
}

// A title that didn't match leaves its library listing and joins the Other
// tab, alongside plain videos; search still finds it.
func TestUnmatchedGoesToOther(t *testing.T) {
	ctx := context.Background()
	s := openTest(t)
	movies, _ := s.CreateLibrary(ctx, "Movies", "/m", KindMovies)
	plain, _ := s.CreateLibrary(ctx, "Clips", "/c", KindOther)
	mk := func(lib int64, title, state, path string) Item {
		it := Item{LibraryID: lib, Kind: ItemMovie, Title: title, SortTitle: title, MatchState: state, SeasonNumber: -1, EpisodeNumber: -1}
		if err := s.CreateItem(ctx, &it); err != nil {
			t.Fatal(err)
		}
		v := Version{LibraryID: lib, ItemID: it.ID, Path: path, RelPath: path[3:], Size: 1}
		if err := s.SaveVersion(ctx, &v, nil); err != nil {
			t.Fatal(err)
		}
		return it
	}
	mk(movies.ID, "Matched", MatchMatched, "/m/a.mkv")
	mk(movies.ID, "Waiting", MatchPending, "/m/b.mkv")
	mk(movies.ID, "Lost Film", MatchUnmatched, "/m/c.mkv")
	mk(plain.ID, "Clip", MatchManual, "/c/d.mkv")

	titles := func(f ItemFilter) []string {
		items, err := s.ListItems(ctx, f)
		if err != nil {
			t.Fatal(err)
		}
		var out []string
		for _, it := range items {
			out = append(out, it.Title)
		}
		return out
	}
	if got := titles(ItemFilter{LibraryID: movies.ID}); !reflect.DeepEqual(got, []string{"Matched", "Waiting"}) {
		t.Errorf("library: %v", got)
	}
	if got := titles(ItemFilter{Kind: ItemMovie}); !reflect.DeepEqual(got, []string{"Matched", "Waiting"}) {
		t.Errorf("all movies: %v", got)
	}
	if got := titles(ItemFilter{OnlyOther: true}); !reflect.DeepEqual(got, []string{"Clip", "Lost Film"}) {
		t.Errorf("other: %v", got)
	}
	if got := titles(ItemFilter{Search: "lost", IncludeOther: true, IncludeUnmatched: true}); !reflect.DeepEqual(got, []string{"Lost Film"}) {
		t.Errorf("search: %v", got)
	}
	if got := titles(ItemFilter{LibraryID: movies.ID, MatchState: MatchUnmatched}); !reflect.DeepEqual(got, []string{"Lost Film"}) {
		t.Errorf("by state: %v", got)
	}
}
