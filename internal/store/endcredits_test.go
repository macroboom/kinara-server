//go:build !nosqlite

package store

import (
	"context"
	"testing"
)

// A season is queued until every episode is checked for both an intro and
// end credits, and a replaced file is checked again.
func TestEndCreditsQueue(t *testing.T) {
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
	show := mk(ItemShow, "", -1, -1)
	season := mk(ItemSeason, show, 1, -1)
	var versions []Version
	for ep := 1; ep <= 2; ep++ {
		id := mk(ItemEpisode, season, 1, ep)
		v := Version{LibraryID: lib.ID, ItemID: id, Path: "/tv/s01e0" + string(rune('0'+ep)) + ".mkv",
			RelPath: "x", Size: 1, Mtime: 100, DurationSec: 1320}
		if err := s.SaveVersion(ctx, &v, []Stream{{Index: 0, Type: "video", Codec: "h264"}}); err != nil {
			t.Fatal(err)
		}
		versions = append(versions, v)
	}
	queued := func() bool {
		ids, err := s.SeasonsNeedingIntros(ctx, 5)
		if err != nil {
			t.Fatal(err)
		}
		return len(ids) == 1 && ids[0] == season
	}
	if !queued() {
		t.Fatal("new season should be queued")
	}
	for _, v := range versions {
		if err := s.SetIntro(ctx, v.ID, Intro{Start: 30, End: 60}, v.Mtime); err != nil {
			t.Fatal(err)
		}
	}
	if !queued() {
		t.Fatal("season with intros but no end credits check should stay queued")
	}
	files, err := s.SeasonIntroFiles(ctx, season)
	if err != nil {
		t.Fatal(err)
	}
	if len(files) != 2 || !files[0].Checked || files[0].CreditsChecked || files[0].Duration != 1320 {
		t.Fatalf("files: %+v", files)
	}
	if err := s.SetEndCredits(ctx, versions[0].ID, EndCredits{Start: 1250, End: 1310, Source: "audio"}, 100); err != nil {
		t.Fatal(err)
	}
	if err := s.SetEndCredits(ctx, versions[1].ID, EndCredits{}, 100); err != nil {
		t.Fatal(err)
	}
	if queued() {
		t.Fatal("fully checked season should not be queued")
	}
	c, err := s.EndCreditsFor(ctx, versions[0].ID)
	if err != nil || c == nil || c.Start != 1250 || c.Source != "audio" {
		t.Fatalf("end credits: %+v %v", c, err)
	}
	if c, err := s.EndCreditsFor(ctx, versions[1].ID); err != nil || c != nil {
		t.Fatalf("none found should read as nil: %+v %v", c, err)
	}

	// The file is replaced: its old end credits no longer apply.
	v := versions[0]
	v.Mtime = 200
	if err := s.SaveVersion(ctx, &v, []Stream{{Index: 0, Type: "video", Codec: "h264"}}); err != nil {
		t.Fatal(err)
	}
	if c, _ := s.EndCreditsFor(ctx, v.ID); c != nil {
		t.Fatalf("stale end credits returned: %+v", c)
	}
	if !queued() {
		t.Fatal("replaced file should queue the season again")
	}
}
