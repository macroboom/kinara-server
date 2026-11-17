//go:build !nosqlite

package api

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"kinara/internal/auth"
	"kinara/internal/ffmpeg"
	"kinara/internal/images"
	"kinara/internal/store"
	"kinara/internal/subsync"
	"kinara/internal/transcode"
	"kinara/internal/watch"
)

func TestNeedsTimingCheck(t *testing.T) {
	for _, c := range []struct {
		state, key   string
		check, adopt bool
	}{
		{"", "", true, false},
		{"", "1:2", true, false},
		{"synced", "", false, true},
		{"synced", "1:2", false, false},
		{"already", "1:3", true, false},
		{"unchecked", timingRetry, true, false},
	} {
		check, adopt := needsTimingCheck(store.SubtitleFile{SyncState: c.state, SyncKey: c.key}, "1:2")
		if check != c.check || adopt != c.adopt {
			t.Errorf("state %q key %q: check %v adopt %v", c.state, c.key, check, adopt)
		}
	}
}

// A subtitle file next to the video, timed for another release, is checked
// in the background and served lined up; edited, it's checked again.
func TestSidecarTiming(t *testing.T) {
	if _, err := exec.LookPath("ffmpeg"); err != nil {
		t.Skip("ffmpeg not installed")
	}
	ctx := context.Background()
	data := t.TempDir()
	const runtime = 600
	film := filepath.Join(data, "Heat.1995.mka")
	srtOf := dialogFile(t, film, runtime, 7)
	sidecar := filepath.Join(data, "Heat.1995.en.srt")
	if err := os.WriteFile(sidecar, []byte(srtOf(-3.2)), 0o644); err != nil {
		t.Fatal(err)
	}
	fi, _ := os.Stat(film)

	st, err := store.Open(filepath.Join(data, "kinara.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	hash, _ := auth.HashPassword("password123")
	st.CreateUser(ctx, "alex", hash, true)
	lib, _ := st.CreateLibrary(ctx, "Movies", data, store.KindMovies)
	it := store.Item{LibraryID: lib.ID, Kind: store.ItemMovie, Title: "Heat", SortTitle: "Heat", Year: 1995, SeasonNumber: -1, EpisodeNumber: -1}
	st.CreateItem(ctx, &it)
	v := store.Version{LibraryID: lib.ID, ItemID: it.ID, Path: film, RelPath: filepath.Base(film), Size: fi.Size(), Mtime: fi.ModTime().Unix(),
		Container: "matroska", DurationSec: runtime, AddedAt: time.Now().Unix()}
	st.SaveVersion(ctx, &v, []store.Stream{{Index: 0, Type: "audio", Codec: "flac", Channels: 1, Language: "eng", Default: true}})
	if _, err := st.SyncSidecars(ctx, v.ID, []store.SubtitleFile{{Path: sidecar, Language: "eng", Format: "subrip"}}); err != nil {
		t.Fatal(err)
	}

	srv := &Server{Store: st, Images: images.New(filepath.Join(data, "img")), Watch: &watch.Service{Store: st},
		Log: slog.New(slog.NewTextHandler(io.Discard, nil)), Version: "test", BaseCtx: ctx, DataDir: data, CacheDir: filepath.Join(data, "cache"),
		Subtitles: &transcode.Subtitles{Tool: &ffmpeg.Tool{}, Dir: filepath.Join(data, "cache", "subtitles")}}
	hs := httptest.NewServer(srv.Handler())
	defer hs.Close()
	owner := &client{t: t, base: hs.URL + "/api/v1"}
	var login struct{ Token string }
	owner.do("POST", "/auth/login", map[string]string{"username": "alex", "password": "password123"}, &login)
	owner.token = login.Token

	// The background pass: planned, then checked one by one.
	work := func() int {
		n := 0
		for {
			id, _, ok := srv.nextTiming(ctx)
			if !ok {
				return n
			}
			if srv.checkSubtitleTiming(ctx, id) {
				n++
			}
		}
	}
	srv.planTiming(ctx)
	if n := work(); n != 1 {
		t.Fatalf("checked %d files, want 1", n)
	}
	files, _ := st.SubtitleFiles(ctx, v.ID)
	if len(files) != 1 || files[0].SyncState != "synced" || files[0].SyncKey == "" {
		t.Fatalf("after the check: %+v", files)
	}
	f := files[0]

	served := func() []subsync.Cue {
		t.Helper()
		resp, err := http.Get(fmt.Sprintf("%s/api/v1/subtitles/%s/%d.vtt?token=%s", hs.URL, v.ID, ExternalSubtitleBase+int(f.ID), owner.token))
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		b, _ := io.ReadAll(resp.Body)
		cues, err := subsync.ParseCues(b)
		if err != nil || len(cues) < 5 {
			t.Fatalf("served %q %v", b, err)
		}
		return cues
	}
	near := func(got []subsync.Cue, shift float64) bool {
		want, _ := subsync.ParseCues([]byte(srtOf(shift)))
		g, w := got[len(got)-5], want[len(want)-5]
		d := g.Start - w.Start
		return d < 250*time.Millisecond && d > -250*time.Millisecond
	}
	if !near(served(), 0) {
		t.Fatal("served sidecar isn't lined up with the speech")
	}

	// Checked: planning again finds nothing to do.
	srv.planTiming(ctx)
	if n := work(); n != 0 {
		t.Fatalf("checked %d files again", n)
	}

	// Edited (now 1.5 s late): served as it is until checked again, then lined up.
	os.WriteFile(sidecar, []byte(srtOf(1.5)), 0o644)
	later := time.Now().Add(2 * time.Second)
	os.Chtimes(sidecar, later, later)
	if !near(served(), 1.5) {
		t.Fatal("edited sidecar served with the old correction")
	}
	if n := work(); n != 1 {
		t.Fatalf("edited file: checked %d, want 1 (asked for when served)", n)
	}
	if !near(served(), 0) {
		t.Fatal("edited sidecar isn't lined up after the new check")
	}

	// Checked before keys were kept: the key is recorded, not checked again.
	st.SetSubtitleSyncKey(ctx, f.ID, "")
	srv.planTiming(ctx)
	files, _ = st.SubtitleFiles(ctx, v.ID)
	if n := work(); n != 0 || files[0].SyncKey == "" {
		t.Fatalf("adopting: checked %d, key %q", n, files[0].SyncKey)
	}

	// Turned off: the background leaves files alone; one asked for is still checked.
	st.SetValue(ctx, keySubsCheckTiming, "false")
	st.SetSubtitleTiming(ctx, f.ID, 0, 0, "", "")
	srv.planTiming(ctx)
	if n := work(); n != 0 {
		t.Fatalf("off: checked %d in the background", n)
	}
	srv.checkTimingSoon(f.ID)
	if n := work(); n != 1 {
		t.Fatalf("off: checked %d asked for, want 1", n)
	}
}
