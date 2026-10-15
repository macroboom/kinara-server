//go:build !nosqlite

package store

import (
	"context"
	"errors"
	"testing"
)

// movieWithFile makes a movie in lib with a present file, so lists show it.
func movieWithFile(t *testing.T, s *Store, lib Library, title string) string {
	t.Helper()
	ctx := context.Background()
	it := Item{LibraryID: lib.ID, Kind: ItemMovie, Title: title, SortTitle: title, SeasonNumber: -1, EpisodeNumber: -1}
	if err := s.CreateItem(ctx, &it); err != nil {
		t.Fatal(err)
	}
	v := Version{LibraryID: lib.ID, ItemID: it.ID, Path: lib.Path + "/" + title + ".mkv", RelPath: title + ".mkv", Size: 1}
	if err := s.SaveVersion(ctx, &v, []Stream{{Index: 0, Type: "video", Codec: "h264"}}); err != nil {
		t.Fatal(err)
	}
	return it.ID
}

func TestLists(t *testing.T) {
	ctx := context.Background()
	s := openTest(t)
	lib, err := s.CreateLibrary(ctx, "Movies", "/m", KindMovies)
	if err != nil {
		t.Fatal(err)
	}
	a := movieWithFile(t, s, lib, "Alpha")
	b := movieWithFile(t, s, lib, "Beta")
	alex, err := s.CreateUser(ctx, "alex", "x", true)
	if err != nil {
		t.Fatal(err)
	}
	sam, err := s.CreateUser(ctx, "sam", "x", false)
	if err != nil {
		t.Fatal(err)
	}

	// My List is made on first use, once.
	lists, err := s.Lists(ctx, alex.ProfileID)
	if err != nil || len(lists) != 1 || !lists[0].IsDefault || lists[0].Name != DefaultListName {
		t.Fatalf("lists = %+v, %v", lists, err)
	}
	mine := lists[0].ID
	if again, _ := s.DefaultList(ctx, alex.ProfileID); again != mine {
		t.Fatalf("default list changed: %d then %d", mine, again)
	}

	// Adding twice is fine; newest first.
	for _, id := range []string{a, b, a} {
		if err := s.AddToList(ctx, alex.ProfileID, mine, id); err != nil {
			t.Fatal(err)
		}
	}
	items, err := s.ItemsInList(ctx, alex.ProfileID, mine)
	if err != nil || len(items) != 2 {
		t.Fatalf("items = %v, %v", items, err)
	}
	if with, _ := s.ListsWith(ctx, alex.ProfileID, a); len(with) != 1 || with[0] != mine {
		t.Fatalf("lists with a = %v", with)
	}

	// Someone else's profile can't see or change it.
	if _, err := s.ItemsInList(ctx, sam.ProfileID, mine); !errors.Is(err, ErrNotFound) {
		t.Fatalf("other profile read: %v", err)
	}
	if err := s.AddToList(ctx, sam.ProfileID, mine, b); !errors.Is(err, ErrNotFound) {
		t.Fatalf("other profile add: %v", err)
	}

	// Named lists: make, rename, delete. My List can't be deleted.
	weekend, err := s.CreateList(ctx, alex.ProfileID, "  Weekend  ")
	if err != nil || weekend.Name != "Weekend" {
		t.Fatalf("create = %+v, %v", weekend, err)
	}
	if _, err := s.CreateList(ctx, alex.ProfileID, "   "); !errors.Is(err, ErrBadInput) {
		t.Fatalf("blank name: %v", err)
	}
	if err := s.RenameList(ctx, alex.ProfileID, weekend.ID, "Saturday"); err != nil {
		t.Fatal(err)
	}
	if err := s.DeleteList(ctx, alex.ProfileID, mine); !errors.Is(err, ErrBadInput) {
		t.Fatalf("delete My List: %v", err)
	}
	if err := s.AddToList(ctx, alex.ProfileID, weekend.ID, b); err != nil {
		t.Fatal(err)
	}
	lists, _ = s.Lists(ctx, alex.ProfileID)
	if len(lists) != 2 || lists[1].Name != "Saturday" || lists[1].Count != 1 || lists[0].Count != 2 {
		t.Fatalf("lists = %+v", lists)
	}
	if err := s.DeleteList(ctx, alex.ProfileID, weekend.ID); err != nil {
		t.Fatal(err)
	}

	// Episodes and seasons can't go on a list.
	show := Item{LibraryID: lib.ID, Kind: ItemSeason, Title: "S1", SortTitle: "S1", SeasonNumber: 1, EpisodeNumber: -1}
	if err := s.CreateItem(ctx, &show); err != nil {
		t.Fatal(err)
	}
	if err := s.AddToList(ctx, alex.ProfileID, mine, show.ID); !errors.Is(err, ErrBadInput) {
		t.Fatalf("season on a list: %v", err)
	}

	// Remove, and a title whose file is missing drops out of the list.
	if err := s.RemoveFromList(ctx, alex.ProfileID, mine, b); err != nil {
		t.Fatal(err)
	}
	if _, err := s.MarkMissing(ctx, lib.ID, now()+10); err != nil {
		t.Fatal(err)
	}
	if items, _ := s.ItemsInList(ctx, alex.ProfileID, mine); len(items) != 0 {
		t.Fatalf("missing file still listed: %v", items)
	}
}

func TestSettingsAndAccounts(t *testing.T) {
	ctx := context.Background()
	s := openTest(t)
	st, err := s.GetSettings(ctx)
	if err != nil || st != DefaultSettings {
		t.Fatalf("defaults = %+v, %v", st, err)
	}
	if err := s.PutSettings(ctx, Settings{ListAccounts: false, AllowSignup: true}); err != nil {
		t.Fatal(err)
	}
	if st, _ = s.GetSettings(ctx); st.ListAccounts || !st.AllowSignup {
		t.Fatalf("saved = %+v", st)
	}

	a, _ := s.CreateUser(ctx, "alex", "x", true)
	b, _ := s.CreateUser(ctx, "sam", "x", false)
	cards, err := s.AccountCards(ctx)
	if err != nil || len(cards) != 2 || cards[0].Username != "alex" || cards[0].Color == "" {
		t.Fatalf("cards = %+v, %v", cards, err)
	}
	if n, _ := s.CountAdmins(ctx); n != 1 {
		t.Fatalf("admins = %d", n)
	}
	if err := s.SetAdmin(ctx, b.ID, true); err != nil {
		t.Fatal(err)
	}
	if n, _ := s.CountAdmins(ctx); n != 2 {
		t.Fatalf("admins = %d", n)
	}
	if err := s.DeleteUser(ctx, a.ID); err != nil {
		t.Fatal(err)
	}
	users, _ := s.ListUsers(ctx)
	if len(users) != 1 || users[0].Username != "sam" {
		t.Fatalf("users = %+v", users)
	}
	if err := s.DeleteUser(ctx, a.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("delete twice: %v", err)
	}
}

func TestOtherLibrary(t *testing.T) {
	ctx := context.Background()
	s := openTest(t)
	movies, _ := s.CreateLibrary(ctx, "Movies", "/m", KindMovies)
	other, err := s.CreateLibrary(ctx, "Home videos", "/o", KindOther)
	if err != nil {
		t.Fatal(err)
	}
	got, err := s.GetLibrary(ctx, other.ID)
	if err != nil || got.Kind != KindOther {
		t.Fatalf("library = %+v, %v", got, err)
	}
	movieWithFile(t, s, movies, "Film")
	movieWithFile(t, s, other, "Birthday")

	// Other videos stay out of the movie listing unless asked for, or listed by library.
	all, _ := s.ListItems(ctx, ItemFilter{Kind: ItemMovie})
	if len(all) != 1 || all[0].Title != "Film" {
		t.Fatalf("movies = %v", all)
	}
	all, _ = s.ListItems(ctx, ItemFilter{Kind: ItemMovie, IncludeOther: true})
	if len(all) != 2 {
		t.Fatalf("with other = %v", all)
	}
	own, _ := s.ListItems(ctx, ItemFilter{LibraryID: other.ID})
	if len(own) != 1 || own[0].Title != "Birthday" {
		t.Fatalf("other library = %v", own)
	}
}
