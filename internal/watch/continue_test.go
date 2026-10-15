//go:build !nosqlite

package watch

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"kinora/internal/store"
)

// Movies show in Continue Watching once started (not only after two
// minutes), and again when rewatched after being finished.
func TestContinueMovies(t *testing.T) {
	ctx := context.Background()
	st, err := store.Open(filepath.Join(t.TempDir(), "db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	lib, _ := st.CreateLibrary(ctx, "Movies", "/m", store.KindMovies)
	u, err := st.CreateUser(ctx, "alex", "x", true)
	if err != nil {
		t.Fatal(err)
	}
	profile := u.ProfileID
	now := time.Unix(1_800_000_000, 0)
	clock := now
	svc := &Service{Store: st, Now: func() time.Time { return clock }}
	movie := func(title string) store.Item {
		it := store.Item{LibraryID: lib.ID, Kind: store.ItemMovie, Title: title, SortTitle: title, SeasonNumber: -1, EpisodeNumber: -1}
		if err := st.CreateItem(ctx, &it); err != nil {
			t.Fatal(err)
		}
		v := store.Version{LibraryID: lib.ID, ItemID: it.ID, Path: "/m/" + title + ".mkv", RelPath: title + ".mkv", Size: 1, DurationSec: 6000}
		if err := st.SaveVersion(ctx, &v, nil); err != nil {
			t.Fatal(err)
		}
		return it
	}
	report := func(it store.Item, pos float64) {
		t.Helper()
		clock = clock.Add(time.Minute)
		if _, err := svc.Report(ctx, profile, it.ID, pos, 6000, clock.Unix()); err != nil {
			t.Fatal(err)
		}
	}
	started := movie("Started")
	peeked := movie("Peeked")
	finished := movie("Finished")
	rewatch := movie("Rewatch")
	report(started, 45)
	report(peeked, 10)
	report(finished, 5900)
	report(rewatch, 5900)
	report(rewatch, 900) // watching it again

	got, err := svc.Continue(ctx, profile, 20)
	if err != nil {
		t.Fatal(err)
	}
	var titles []string
	for _, e := range got {
		titles = append(titles, e.Item.Title)
	}
	if len(titles) != 2 || titles[0] != "Rewatch" || titles[1] != "Started" {
		t.Fatalf("continue = %v, want [Rewatch Started]", titles)
	}
	if got[0].PositionSec != 900 || !got[0].InProgress {
		t.Errorf("rewatch entry %+v", got[0])
	}
	if w, _ := st.GetWatch(ctx, profile, rewatch.ID); !w.Completed {
		t.Error("a rewatch should keep the movie marked watched")
	}
}
