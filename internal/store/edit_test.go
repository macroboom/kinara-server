//go:build !nosqlite

package store

import (
	"context"
	"reflect"
	"testing"
)

// Fields edited by hand survive matching until they're unlocked.
func TestEditLocks(t *testing.T) {
	ctx := context.Background()
	s := openTest(t)
	lib, _ := s.CreateLibrary(ctx, "Movies", "/m", KindMovies)
	it := Item{LibraryID: lib.ID, Kind: ItemMovie, Title: "Heat", SortTitle: "Heat", Year: 1995, SeasonNumber: -1, EpisodeNumber: -1}
	if err := s.CreateItem(ctx, &it); err != nil {
		t.Fatal(err)
	}
	s.SetImage(ctx, it.ID, "poster", "https://tmdb/a.jpg")
	s.SetGenres(ctx, it.ID, []string{"Crime"})

	title, overview, genres := "The Heat (Director's Cut)", "My notes.", []string{"Crime", "Drama"}
	if err := s.EditItem(ctx, it.ID, ItemEdit{Title: &title, Overview: &overview, Genres: &genres}); err != nil {
		t.Fatal(err)
	}
	if err := s.SetImageByHand(ctx, it.ID, "poster", "file:/data/artwork/p.jpg"); err != nil {
		t.Fatal(err)
	}
	locks, _ := s.ItemLocks(ctx, it.ID)
	want := []string{LockTitle, LockSortTitle, LockOverview, LockGenres, LockPoster}
	if !reflect.DeepEqual(locks, want) {
		t.Fatalf("locks %v", locks)
	}

	// A refresh from TMDB.
	fresh, _ := s.GetItem(ctx, it.ID)
	fresh.Title, fresh.SortTitle, fresh.Overview, fresh.Year = "Heat", "Heat", "TMDB text", 1996
	if err := s.UpdateItemMetadata(ctx, fresh); err != nil {
		t.Fatal(err)
	}
	s.SetImage(ctx, it.ID, "poster", "https://tmdb/b.jpg")
	s.SetImage(ctx, it.ID, "backdrop", "https://tmdb/bd.jpg")
	s.SetGenres(ctx, it.ID, []string{"Action"})

	got, _ := s.GetItem(ctx, it.ID)
	if got.Title != title || got.SortTitle != "Heat (Director's Cut)" || got.Overview != overview || got.Year != 1996 {
		t.Fatalf("after refresh: %+v", got)
	}
	if u, _ := s.ImageURL(ctx, it.ID, "poster"); u != "file:/data/artwork/p.jpg" {
		t.Fatalf("poster %q", u)
	}
	if u, _ := s.ImageURL(ctx, it.ID, "backdrop"); u != "https://tmdb/bd.jpg" {
		t.Fatalf("backdrop %q", u)
	}
	if g, _ := s.ItemGenres(ctx, it.ID); !reflect.DeepEqual(g, genres) {
		t.Fatalf("genres %v", g)
	}

	// Unlocked, the next refresh wins again.
	if err := s.Unlock(ctx, it.ID, []string{LockTitle, LockSortTitle}); err != nil {
		t.Fatal(err)
	}
	s.UpdateItemMetadata(ctx, fresh)
	got, _ = s.GetItem(ctx, it.ID)
	if got.Title != "Heat" || got.Overview != overview {
		t.Fatalf("after unlocking the title: %+v", got)
	}
	s.Unlock(ctx, it.ID, []string{"all"})
	if l, _ := s.ItemLocks(ctx, it.ID); len(l) != 0 {
		t.Fatalf("locks left %v", l)
	}
	s.SetImage(ctx, it.ID, "poster", "https://tmdb/c.jpg")
	if u, _ := s.ImageURL(ctx, it.ID, "poster"); u != "https://tmdb/c.jpg" {
		t.Fatalf("poster after unlock %q", u)
	}
}
