//go:build !nosqlite

package store

import (
	"context"
	"fmt"
	"path/filepath"
	"strings"
	"testing"
)

func openTest(t *testing.T) *Store {
	t.Helper()
	path := filepath.Join(t.TempDir(), "test.db")
	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	// Opening again must not re-run migrations.
	s2, err := Open(path)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	s2.Close()
	return s
}

func TestMergeShows(t *testing.T) {
	ctx := context.Background()
	s := openTest(t)
	lib, err := s.CreateLibrary(ctx, "TV", "/tv", KindTV)
	if err != nil {
		t.Fatal(err)
	}
	mk := func(kind, parent string, season, episode int) string {
		it := Item{LibraryID: lib.ID, Kind: kind, ParentID: parent, Title: kind, SortTitle: kind,
			SeasonNumber: season, EpisodeNumber: episode}
		if err := s.CreateItem(ctx, &it); err != nil {
			t.Fatal(err)
		}
		return it.ID
	}
	// Two folders for one show: A has S1E1 and S1E2, B has S1E2 and S2E1.
	a := mk(ItemShow, "", -1, -1)
	a1 := mk(ItemSeason, a, 1, -1)
	a11 := mk(ItemEpisode, a1, 1, 1)
	a12 := mk(ItemEpisode, a1, 1, 2)
	b := mk(ItemShow, "", -1, -1)
	b1 := mk(ItemSeason, b, 1, -1)
	b12 := mk(ItemEpisode, b1, 1, 2)
	b2 := mk(ItemSeason, b, 2, -1)
	mk(ItemEpisode, b2, 2, 1)

	v := Version{LibraryID: lib.ID, ItemID: b12, Path: "/tv/b/s01e02.mkv", RelPath: "b/s01e02.mkv", Size: 1}
	if err := s.SaveVersion(ctx, &v, []Stream{{Index: 0, Type: "video", Codec: "h264"}}); err != nil {
		t.Fatal(err)
	}
	u, err := s.CreateUser(ctx, "alex", "x", true)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.PutWatch(ctx, u.ProfileID, WatchState{ItemID: b12, Completed: true, PlayCount: 1}); err != nil {
		t.Fatal(err)
	}

	if err := s.MergeItem(ctx, b, a); err != nil {
		t.Fatal(err)
	}
	if _, err := s.GetItem(ctx, b); err != ErrNotFound {
		t.Fatalf("show B should be gone: %v", err)
	}
	seasons, _ := s.Children(ctx, a)
	if len(seasons) != 2 {
		t.Fatalf("want 2 seasons, got %d", len(seasons))
	}
	eps, _ := s.Children(ctx, a1)
	if len(eps) != 2 || eps[0].ID != a11 || eps[1].ID != a12 {
		t.Fatalf("season 1 episodes: %+v", eps)
	}
	vs, _ := s.VersionsForItem(ctx, a12)
	if len(vs) != 1 {
		t.Fatalf("version should move to the surviving episode, got %d", len(vs))
	}
	w, _ := s.GetWatch(ctx, u.ProfileID, a12)
	if !w.Completed {
		t.Fatal("watch state should follow the merge")
	}
	st, _ := s.Streams(ctx, v.ID)
	if len(st) != 1 {
		t.Fatalf("streams: %+v", st)
	}
}

func TestListItemsHidesMissing(t *testing.T) {
	ctx := context.Background()
	s := openTest(t)
	lib, _ := s.CreateLibrary(ctx, "Movies", "/m", KindMovies)
	it := Item{LibraryID: lib.ID, Kind: ItemMovie, Title: "Gone", SortTitle: "Gone", SeasonNumber: -1, EpisodeNumber: -1}
	if err := s.CreateItem(ctx, &it); err != nil {
		t.Fatal(err)
	}
	v := Version{LibraryID: lib.ID, ItemID: it.ID, Path: "/m/gone.mkv", RelPath: "gone.mkv", Size: 1}
	if err := s.SaveVersion(ctx, &v, nil); err != nil {
		t.Fatal(err)
	}
	items, _ := s.ListItems(ctx, ItemFilter{LibraryID: lib.ID})
	if len(items) != 1 {
		t.Fatalf("want 1 item, got %d", len(items))
	}
	if _, err := s.MarkMissing(ctx, lib.ID, now()+10); err != nil {
		t.Fatal(err)
	}
	items, _ = s.ListItems(ctx, ItemFilter{LibraryID: lib.ID})
	if len(items) != 0 {
		t.Fatalf("missing items should be hidden, got %d", len(items))
	}
	items, _ = s.ListItems(ctx, ItemFilter{LibraryID: lib.ID, IncludeMissing: true})
	if len(items) != 1 {
		t.Fatal("IncludeMissing should show it")
	}
}

func TestSessions(t *testing.T) {
	ctx := context.Background()
	s := openTest(t)
	u, _ := s.CreateUser(ctx, "Alex", "hash", true)
	if _, err := s.UserByName(ctx, "alex"); err != nil {
		t.Fatal("usernames should be case-insensitive")
	}
	if err := s.CreateSession(ctx, "h1", u.ID, "tv", now()+100); err != nil {
		t.Fatal(err)
	}
	got, exp, _, err := s.SessionUser(ctx, "h1")
	if err != nil || got.ID != u.ID || exp != now()+100 && exp != now()+99 {
		t.Fatalf("session: %+v %d %v", got, exp, err)
	}
	if err := s.SetPassword(ctx, u.ID, "new"); err != nil {
		t.Fatal(err)
	}
	if _, _, _, err := s.SessionUser(ctx, "h1"); err != ErrNotFound {
		t.Fatal("password change should end sessions")
	}
}

func TestProfilesFiltersAndCast(t *testing.T) {
	ctx := context.Background()
	s := openTest(t)
	lib, err := s.CreateLibrary(ctx, "Movies", "/movies", KindMovies)
	if err != nil {
		t.Fatal(err)
	}
	movie := func(title string, runtime int, height int, hdr string) string {
		it := Item{LibraryID: lib.ID, Kind: ItemMovie, Title: title, SortTitle: title, RuntimeMin: runtime,
			SeasonNumber: -1, EpisodeNumber: -1}
		if err := s.CreateItem(ctx, &it); err != nil {
			t.Fatal(err)
		}
		// Runtimes come from matching, not from creating the item.
		if err := s.UpdateItemMetadata(ctx, it); err != nil {
			t.Fatal(err)
		}
		v := Version{LibraryID: lib.ID, ItemID: it.ID, Path: "/movies/" + title + ".mkv", RelPath: title + ".mkv",
			Size: 1, Height: height, HDR: hdr}
		if err := s.SaveVersion(ctx, &v, nil); err != nil {
			t.Fatal(err)
		}
		return it.ID
	}
	alpha := movie("Alpha", 90, 2160, "hdr10")
	beta := movie("Beta", 130, 1080, "")
	movie("9 Lives", 100, 1080, "")

	// Every account starts with one profile; a second one has its own history.
	u, err := s.CreateUser(ctx, "alex", "x", true)
	if err != nil {
		t.Fatal(err)
	}
	ps, err := s.Profiles(ctx, u.ID)
	if err != nil || len(ps) != 1 || ps[0].ID != u.ProfileID || ps[0].Name != "alex" {
		t.Fatalf("profiles = %+v, %v; want one named alex with id %d", ps, err, u.ProfileID)
	}
	kid := Profile{UserID: u.ID, Name: "Sam", Kids: true}
	if err := s.CreateProfile(ctx, &kid); err != nil {
		t.Fatal(err)
	}
	if err := s.PutWatch(ctx, u.ProfileID, WatchState{ItemID: alpha, Completed: true}); err != nil {
		t.Fatal(err)
	}
	if err := s.PutWatch(ctx, u.ProfileID, WatchState{ItemID: beta, PositionSec: 600}); err != nil {
		t.Fatal(err)
	}
	if w, _ := s.GetWatch(ctx, kid.ID, alpha); w.Completed {
		t.Error("the second profile sees the first profile's history")
	}

	titles := func(f ItemFilter) []string {
		t.Helper()
		f.LibraryID = lib.ID
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
	check := func(name string, got []string, want ...string) {
		t.Helper()
		if strings.Join(got, ",") != strings.Join(want, ",") {
			t.Errorf("%s = %v, want %v", name, got, want)
		}
	}
	check("unwatched", titles(ItemFilter{ProfileID: u.ProfileID, Watched: "unwatched"}), "9 Lives")
	check("started", titles(ItemFilter{ProfileID: u.ProfileID, Watched: "started"}), "Beta")
	check("watched", titles(ItemFilter{ProfileID: u.ProfileID, Watched: "watched"}), "Alpha")
	check("kid unwatched", titles(ItemFilter{ProfileID: kid.ID, Watched: "unwatched"}), "9 Lives", "Alpha", "Beta")
	check("under 100 min", titles(ItemFilter{MaxRuntime: 100}), "9 Lives", "Alpha")
	check("best quality", titles(ItemFilter{BestQuality: true}), "Alpha")
	check("by length", titles(ItemFilter{Sort: "length"}), "Alpha", "9 Lives", "Beta")
	check("search", titles(ItemFilter{Search: "BET"}), "Beta")

	letters, err := s.LetterOffsets(ctx, ItemFilter{LibraryID: lib.ID, Kind: ItemMovie})
	if err != nil {
		t.Fatal(err)
	}
	want := []LetterOffset{{"#", 0}, {"A", 1}, {"B", 2}}
	if fmt.Sprint(letters) != fmt.Sprint(want) {
		t.Errorf("letters = %v, want %v", letters, want)
	}

	// Track preferences and cast.
	if err := s.PutTrackPrefs(ctx, u.ProfileID, alpha, TrackPrefs{AudioLang: "jpn", SubtitleMode: "always"}); err != nil {
		t.Fatal(err)
	}
	if tp, _ := s.TrackPrefs(ctx, u.ProfileID, alpha); tp.AudioLang != "jpn" || tp.SubtitleMode != "always" {
		t.Errorf("track prefs = %+v", tp)
	}
	if err := s.SetCredits(ctx, alpha, []Credit{{Name: "Ada Park", Role: "Mara", PhotoURL: "http://x/a.jpg"}}); err != nil {
		t.Fatal(err)
	}
	if cast, _ := s.Credits(ctx, alpha); len(cast) != 1 || cast[0].Name != "Ada Park" {
		t.Errorf("cast = %+v", cast)
	}
	if photo, _ := s.CreditPhoto(ctx, alpha, 0); photo != "http://x/a.jpg" {
		t.Errorf("photo = %q", photo)
	}
}
