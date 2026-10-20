//go:build !nosqlite

package api

import (
	"context"
	"io"
	"log/slog"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"kinara/internal/auth"
	"kinara/internal/images"
	"kinara/internal/probe"
	"kinara/internal/scanner"
	"kinara/internal/store"
	"kinara/internal/watch"
)

// featureServer is a server over an empty database with one owner, alex.
func featureServer(t *testing.T) (*store.Store, *client, func(user, pass string) *client) {
	t.Helper()
	ctx := context.Background()
	dir := t.TempDir()
	st, err := store.Open(filepath.Join(dir, "kinara.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	hash, _ := auth.HashPassword("password123")
	if _, err := st.CreateUser(ctx, "alex", hash, true); err != nil {
		t.Fatal(err)
	}
	srv := &Server{Store: st, Scanner: &scanner.Scanner{Store: st, Prober: &probe.Prober{}, Log: log},
		Images: images.New(filepath.Join(dir, "img")), Watch: &watch.Service{Store: st}, Log: log, Version: "test", BaseCtx: ctx}
	hs := httptest.NewServer(srv.Handler())
	t.Cleanup(hs.Close)
	signIn := func(user, pass string) *client {
		c := &client{t: t, base: hs.URL + "/api/v1"}
		var login struct{ Token string }
		if resp := c.do("POST", "/auth/login", map[string]string{"username": user, "password": pass}, &login); resp.StatusCode != 200 {
			t.Fatalf("sign in %s: %d", user, resp.StatusCode)
		}
		c.token = login.Token
		return c
	}
	return st, signIn("alex", "password123"), signIn
}

func addMovie(t *testing.T, st *store.Store, lib store.Library, title string) string {
	t.Helper()
	ctx := context.Background()
	it := store.Item{LibraryID: lib.ID, Kind: store.ItemMovie, Title: title, SortTitle: title, SeasonNumber: -1, EpisodeNumber: -1}
	if err := st.CreateItem(ctx, &it); err != nil {
		t.Fatal(err)
	}
	v := store.Version{LibraryID: lib.ID, ItemID: it.ID, Path: lib.Path + "/" + title + ".mkv", RelPath: title + ".mkv", Size: 1}
	if err := st.SaveVersion(ctx, &v, nil); err != nil {
		t.Fatal(err)
	}
	return it.ID
}

func TestAccountsAndSignup(t *testing.T) {
	_, owner, signIn := featureServer(t)
	anon := &client{t: t, base: owner.base}

	var acc struct {
		Accounts    []store.AccountCard
		AllowSignup bool
	}
	anon.do("GET", "/auth/accounts", nil, &acc)
	if len(acc.Accounts) != 1 || acc.Accounts[0].Username != "alex" || acc.AllowSignup {
		t.Fatalf("accounts = %+v", acc)
	}
	if resp := anon.do("POST", "/auth/signup", map[string]string{"username": "sam", "password": "longenough"}, nil); resp.StatusCode != 403 {
		t.Fatalf("sign-up while off: %d", resp.StatusCode)
	}

	// The owner turns sign-up on and hides the account list.
	if resp := owner.do("PUT", "/settings", map[string]bool{"allowSignup": true, "listAccounts": false}, nil); resp.StatusCode != 200 {
		t.Fatalf("settings: %d", resp.StatusCode)
	}
	anon.do("GET", "/auth/accounts", nil, &acc)
	if len(acc.Accounts) != 0 || !acc.AllowSignup {
		t.Fatalf("accounts after settings = %+v", acc)
	}
	var made struct {
		Token    string
		User     store.User
		Profiles []store.Profile
	}
	if resp := anon.do("POST", "/auth/signup", map[string]string{"username": "sam", "password": "longenough", "deviceName": "TV"}, &made); resp.StatusCode != 201 {
		t.Fatalf("sign-up: %d", resp.StatusCode)
	}
	if made.Token == "" || made.User.IsAdmin || len(made.Profiles) != 1 {
		t.Fatalf("sign-up answer = %+v", made)
	}
	if resp := anon.do("POST", "/auth/signup", map[string]string{"username": "SAM", "password": "longenough"}, nil); resp.StatusCode != 409 {
		t.Fatalf("taken name: %d", resp.StatusCode)
	}
	if resp := anon.do("POST", "/auth/signup", map[string]string{"username": "kim", "password": "short"}, nil); resp.StatusCode != 400 {
		t.Fatalf("short password: %d", resp.StatusCode)
	}
	if resp := anon.do("POST", "/auth/signup", map[string]string{"username": "../x", "password": "longenough"}, nil); resp.StatusCode != 400 {
		t.Fatalf("bad name: %d", resp.StatusCode)
	}

	// An ordinary account can't manage accounts or settings.
	sam := signIn("sam", "longenough")
	if resp := sam.do("GET", "/users", nil, nil); resp.StatusCode != 403 {
		t.Fatalf("non-owner users: %d", resp.StatusCode)
	}
	if resp := sam.do("PUT", "/settings", map[string]bool{"allowSignup": false}, nil); resp.StatusCode != 403 {
		t.Fatalf("non-owner settings: %d", resp.StatusCode)
	}
}

func TestOwnerManagesAccounts(t *testing.T) {
	_, owner, signIn := featureServer(t)
	var made store.User
	if resp := owner.do("POST", "/users", map[string]any{"username": "kim", "password": "password456"}, &made); resp.StatusCode != 201 || made.IsAdmin {
		t.Fatalf("create: %d %+v", resp.StatusCode, made)
	}
	var users []store.User
	owner.do("GET", "/users", nil, &users)
	if len(users) != 2 {
		t.Fatalf("users = %+v", users)
	}
	alexID := users[0].ID
	if resp := owner.do("PATCH", "/users/"+itoa(alexID), map[string]bool{"isAdmin": false}, nil); resp.StatusCode != 409 {
		t.Fatalf("demote the last owner: %d", resp.StatusCode)
	}
	if resp := owner.do("DELETE", "/users/"+itoa(alexID), nil, nil); resp.StatusCode != 409 {
		t.Fatalf("delete yourself: %d", resp.StatusCode)
	}
	// A new password signs kim out everywhere and works at once.
	kim := signIn("kim", "password456")
	if resp := owner.do("PATCH", "/users/"+itoa(made.ID), map[string]any{"password": "newpassword9"}, nil); resp.StatusCode != 204 {
		t.Fatalf("reset password: %d", resp.StatusCode)
	}
	if resp := kim.do("GET", "/me", nil, nil); resp.StatusCode != 401 {
		t.Fatalf("old session still works: %d", resp.StatusCode)
	}
	signIn("kim", "newpassword9")
	if resp := owner.do("DELETE", "/users/"+itoa(made.ID), nil, nil); resp.StatusCode != 204 {
		t.Fatalf("delete: %d", resp.StatusCode)
	}
}

func TestListsAPI(t *testing.T) {
	st, owner, _ := featureServer(t)
	ctx := context.Background()
	lib, _ := st.CreateLibrary(ctx, "Movies", "/m", store.KindMovies)
	film := addMovie(t, st, lib, "Film")

	var lists []store.List
	owner.do("GET", "/lists", nil, &lists)
	if len(lists) != 1 || !lists[0].IsDefault {
		t.Fatalf("lists = %+v", lists)
	}
	if resp := owner.do("PUT", "/lists/default/items/"+film, nil, nil); resp.StatusCode != 204 {
		t.Fatalf("add: %d", resp.StatusCode)
	}
	var item struct{ Lists []int64 }
	owner.do("GET", "/items/"+film, nil, &item)
	if len(item.Lists) != 1 || item.Lists[0] != lists[0].ID {
		t.Fatalf("item lists = %v", item.Lists)
	}
	var home struct{ MyList []ItemView }
	owner.do("GET", "/home", nil, &home)
	if len(home.MyList) != 1 || home.MyList[0].ID != film {
		t.Fatalf("home my list = %+v", home.MyList)
	}

	var weekend store.List
	if resp := owner.do("POST", "/lists", map[string]string{"name": "Weekend"}, &weekend); resp.StatusCode != 201 {
		t.Fatalf("create: %d", resp.StatusCode)
	}
	owner.do("PUT", "/lists/"+itoa(weekend.ID)+"/items/"+film, nil, nil)
	var in struct{ Items []ItemView }
	owner.do("GET", "/lists/"+itoa(weekend.ID)+"/items", nil, &in)
	if len(in.Items) != 1 {
		t.Fatalf("weekend = %+v", in.Items)
	}
	if resp := owner.do("PATCH", "/lists/"+itoa(weekend.ID), map[string]string{"name": ""}, nil); resp.StatusCode != 400 {
		t.Fatalf("blank rename: %d", resp.StatusCode)
	}
	if resp := owner.do("DELETE", "/lists/"+itoa(lists[0].ID), nil, nil); resp.StatusCode != 400 {
		t.Fatalf("delete My List: %d", resp.StatusCode)
	}
	if resp := owner.do("DELETE", "/lists/default/items/"+film, nil, nil); resp.StatusCode != 204 {
		t.Fatalf("remove: %d", resp.StatusCode)
	}
	if resp := owner.do("GET", "/lists/99999/items", nil, nil); resp.StatusCode != 404 {
		t.Fatalf("someone else's list: %d", resp.StatusCode)
	}
}

func TestOtherVideosAPI(t *testing.T) {
	st, owner, _ := featureServer(t)
	ctx := context.Background()
	movies, _ := st.CreateLibrary(ctx, "Movies", "/m", store.KindMovies)
	other, _ := st.CreateLibrary(ctx, "Home videos", "/o", store.KindOther)
	addMovie(t, st, movies, "Film")
	addMovie(t, st, other, "Birthday")

	var libs []struct {
		ID   int64
		Kind string
	}
	owner.do("GET", "/libraries", nil, &libs)
	kinds := map[int64]string{}
	for _, l := range libs {
		kinds[l.ID] = l.Kind
	}
	if kinds[other.ID] != "other" || kinds[movies.ID] != "movies" {
		t.Fatalf("libraries = %+v", libs)
	}
	var page struct{ Items []ItemView }
	owner.do("GET", "/items?kind=movie", nil, &page)
	if len(page.Items) != 1 || page.Items[0].Title != "Film" {
		t.Fatalf("movies = %+v", page.Items)
	}
	owner.do("GET", "/items?other=only", nil, &page)
	if len(page.Items) != 1 || page.Items[0].Title != "Birthday" {
		t.Fatalf("other = %+v", page.Items)
	}
	var found struct{ Items []ItemView }
	owner.do("GET", "/search?q=birth", nil, &found)
	if len(found.Items) != 1 {
		t.Fatalf("search = %+v", found.Items)
	}
}

func TestManagerEndpoints(t *testing.T) {
	st, owner, signIn := featureServer(t)
	ctx := context.Background()
	lib, _ := st.CreateLibrary(ctx, "Movies", "/m", store.KindMovies)
	v := store.Version{LibraryID: lib.ID, Path: "/m/broken.mkv", RelPath: "broken.mkv", Size: 1, ProbeError: "invalid data"}
	if err := st.SaveVersion(ctx, &v, nil); err != nil {
		t.Fatal(err)
	}
	var problems struct {
		Unreadable []problemFile
		Unplaced   []problemFile
		Unmatched  []ItemView
	}
	if resp := owner.do("GET", "/libraries/"+itoa(lib.ID)+"/problems", nil, &problems); resp.StatusCode != 200 {
		t.Fatalf("problems: %d", resp.StatusCode)
	}
	if len(problems.Unreadable) != 1 || problems.Unreadable[0].Reason != "invalid data" || problems.Unmatched == nil {
		t.Fatalf("problems = %+v", problems)
	}
	var status struct {
		Version  string
		Warnings []string
		Web      bool
	}
	if resp := owner.do("GET", "/status", nil, &status); resp.StatusCode != 200 || status.Version != "test" || status.Warnings == nil {
		t.Fatalf("status: %d %+v", resp.StatusCode, status)
	}
	owner.do("POST", "/users", map[string]any{"username": "kim", "password": "password456"}, nil)
	kim := signIn("kim", "password456")
	if resp := kim.do("GET", "/status", nil, nil); resp.StatusCode != 403 {
		t.Fatalf("non-owner status: %d", resp.StatusCode)
	}
}

// The manager edits a library: rename it, add a folder, and take one out.
func TestEditLibrary(t *testing.T) {
	st, owner, _ := featureServer(t)
	ctx := context.Background()
	root := t.TempDir()
	a, b, nested := filepath.Join(root, "a"), filepath.Join(root, "b"), filepath.Join(root, "a", "inner")
	for _, d := range []string{a, b, nested} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	lib, err := st.CreateLibrary(ctx, "Movies", a, store.KindMovies)
	if err != nil {
		t.Fatal(err)
	}
	base := "/libraries/" + itoa(lib.ID)

	var view struct {
		Name    string
		Path    string
		Folders []string
	}
	if resp := owner.do("PATCH", base, map[string]string{"name": "  Films  "}, &view); resp.StatusCode != 200 || view.Name != "Films" {
		t.Fatalf("rename: %d %+v", resp.StatusCode, view)
	}
	if resp := owner.do("PATCH", base, map[string]string{"name": " "}, nil); resp.StatusCode != 400 {
		t.Fatalf("empty name: %d", resp.StatusCode)
	}
	if resp := owner.do("PATCH", "/libraries/999", map[string]string{"name": "x"}, nil); resp.StatusCode != 404 {
		t.Fatalf("rename missing: %d", resp.StatusCode)
	}

	// Overlaps are refused: the same folder, one inside, one around.
	for _, p := range []string{a, nested, root, "relative/path", filepath.Join(root, "nope")} {
		if resp := owner.do("POST", base+"/folders", map[string]string{"path": p}, nil); resp.StatusCode != 400 {
			t.Fatalf("add %s: %d", p, resp.StatusCode)
		}
	}
	// A new library can't overlap either.
	if resp := owner.do("POST", "/libraries", map[string]string{"path": nested, "kind": "movies"}, nil); resp.StatusCode != 400 {
		t.Fatalf("nested library: %d", resp.StatusCode)
	}

	if resp := owner.do("POST", base+"/folders", map[string]string{"path": b}, &view); resp.StatusCode != 201 {
		t.Fatalf("add folder: %d", resp.StatusCode)
	}
	if len(view.Folders) != 2 || view.Folders[1] != b {
		t.Fatalf("folders %v", view.Folders)
	}
	waitScan(t, owner, lib.ID)

	if resp := owner.do("DELETE", base+"/folders?path="+url.QueryEscape(nested), nil, nil); resp.StatusCode != 404 {
		t.Fatalf("remove unknown: %d", resp.StatusCode)
	}
	var removed struct {
		Path         string
		Folders      []string
		RemovedFiles int64
	}
	if resp := owner.do("DELETE", base+"/folders?path="+url.QueryEscape(a), nil, &removed); resp.StatusCode != 200 {
		t.Fatalf("remove: %d", resp.StatusCode)
	}
	if removed.Path != b || len(removed.Folders) != 1 {
		t.Fatalf("after remove %+v", removed)
	}
	if resp := owner.do("DELETE", base+"/folders?path="+url.QueryEscape(b), nil, nil); resp.StatusCode != 400 {
		t.Fatalf("remove last: %d", resp.StatusCode)
	}
}

func waitScan(t *testing.T, c *client, id int64) {
	t.Helper()
	for range 100 {
		var libs []struct {
			ID   int64
			Scan *struct{ Running bool }
		}
		c.do("GET", "/libraries", nil, &libs)
		busy := false
		for _, l := range libs {
			if l.ID == id && l.Scan != nil && l.Scan.Running {
				busy = true
			}
		}
		if !busy {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatal("scan never finished")
}

// The folder picker lists folders on the server, marks ones already in a
// library, and is for the owner only.
func TestFolderPicker(t *testing.T) {
	st, owner, signIn := featureServer(t)
	ctx := context.Background()
	root := t.TempDir()
	for _, d := range []string{"Movies", "TV", ".hidden", "Movies/Inner"} {
		if err := os.MkdirAll(filepath.Join(root, d), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	_ = os.WriteFile(filepath.Join(root, "clip.mkv"), []byte("x"), 0o644)
	_ = os.WriteFile(filepath.Join(root, "notes.txt"), []byte("x"), 0o644)
	if _, err := st.CreateLibrary(ctx, "Films", filepath.Join(root, "Movies"), store.KindMovies); err != nil {
		t.Fatal(err)
	}

	var top struct{ Folders []struct{ Name, Path string } }
	if resp := owner.do("GET", "/fs/folders", nil, &top); resp.StatusCode != 200 || len(top.Folders) == 0 {
		t.Fatalf("places: %d %+v", resp.StatusCode, top)
	}

	var got struct {
		Path, Parent string
		Videos       int
		Folders      []struct{ Name, Path, Library string }
	}
	if resp := owner.do("GET", "/fs/folders?path="+url.QueryEscape(root), nil, &got); resp.StatusCode != 200 {
		t.Fatalf("list: %d", resp.StatusCode)
	}
	if got.Path != root || got.Parent != filepath.Dir(root) || got.Videos != 1 {
		t.Fatalf("listing %+v", got)
	}
	if len(got.Folders) != 2 || got.Folders[0].Name != "Movies" || got.Folders[0].Library != "Films" || got.Folders[1].Name != "TV" || got.Folders[1].Library != "" {
		t.Fatalf("folders %+v", got.Folders)
	}
	var inner struct{ Library string }
	owner.do("GET", "/fs/folders?path="+url.QueryEscape(filepath.Join(root, "Movies", "Inner")), nil, &inner)
	if inner.Library != "Films" {
		t.Fatalf("inside a library folder: %+v", inner)
	}
	if resp := owner.do("GET", "/fs/folders?path=relative", nil, nil); resp.StatusCode != 400 {
		t.Fatalf("relative: %d", resp.StatusCode)
	}
	if resp := owner.do("GET", "/fs/folders?path="+url.QueryEscape(filepath.Join(root, "nope")), nil, nil); resp.StatusCode != 404 {
		t.Fatalf("missing: %d", resp.StatusCode)
	}

	// Only the owner can look around the server's disks.
	hash, _ := auth.HashPassword("password456")
	if _, err := st.CreateUser(ctx, "sam", hash, false); err != nil {
		t.Fatal(err)
	}
	if resp := signIn("sam", "password456").do("GET", "/fs/folders?path="+url.QueryEscape(root), nil, nil); resp.StatusCode != 403 {
		t.Fatalf("non-owner: %d", resp.StatusCode)
	}
}

// An unmatched title in a Movies library whose files look like episodes
// comes with a hint to move it.
func TestProblemsHintShowInMovies(t *testing.T) {
	st, owner, _ := featureServer(t)
	ctx := context.Background()
	lib, _ := st.CreateLibrary(ctx, "Movies", "/m", store.KindMovies)
	it := store.Item{LibraryID: lib.ID, Kind: store.ItemMovie, Title: "Book 1 - Air", SortTitle: "Book 1 - Air", Year: 2012, SeasonNumber: -1, EpisodeNumber: -1}
	if err := st.CreateItem(ctx, &it); err != nil {
		t.Fatal(err)
	}
	for i := 1; i <= 3; i++ {
		rel := "Legend of Korra/Book 1 - Air (2012)/Legend of Korra S01E0" + itoa(int64(i)) + ".mkv"
		v := store.Version{LibraryID: lib.ID, ItemID: it.ID, Path: "/m/" + rel, RelPath: rel, Size: 1}
		if err := st.SaveVersion(ctx, &v, nil); err != nil {
			t.Fatal(err)
		}
	}
	if err := st.SetMatchState(ctx, it.ID, store.MatchUnmatched, 0); err != nil {
		t.Fatal(err)
	}
	var problems struct {
		Notes map[string]unmatchedNote
	}
	owner.do("GET", "/libraries/"+itoa(lib.ID)+"/problems", nil, &problems)
	n, ok := problems.Notes[it.ID]
	if !ok || n.Files != 3 || !strings.Contains(n.Hint, "TV shows library") || !strings.HasPrefix(n.Path, "Legend of Korra/") {
		t.Fatalf("note %+v", problems.Notes)
	}
}

// The manager sees signed-in devices and what's playing, picks a priority
// device without clearing other settings, and can sign a device out.
func TestDevicesAndNowPlaying(t *testing.T) {
	st, owner, signIn := featureServer(t)
	ctx := context.Background()
	hash, _ := auth.HashPassword("password456")
	if _, err := st.CreateUser(ctx, "sam", hash, false); err != nil {
		t.Fatal(err)
	}
	sam := signIn("sam", "password456")
	lib, _ := st.CreateLibrary(ctx, "Movies", "/m", store.KindMovies)
	movie := addMovie(t, st, lib, "Night Shift")

	var devices []store.Device
	if resp := owner.do("GET", "/devices", nil, &devices); resp.StatusCode != 200 || len(devices) != 2 {
		t.Fatalf("devices: %d %+v", resp.StatusCode, devices)
	}
	var samDevice string
	for _, d := range devices {
		if d.Username == "sam" {
			samDevice = d.ID
		}
	}
	if len(samDevice) != store.DeviceIDLen {
		t.Fatalf("device id %q", samDevice)
	}
	if resp := sam.do("GET", "/devices", nil, nil); resp.StatusCode != 403 {
		t.Fatalf("non-owner devices: %d", resp.StatusCode)
	}

	// Priority device: set it, and the sign-in settings stay as they were.
	owner.do("PUT", "/settings", map[string]any{"allowSignup": true}, nil)
	var set store.Settings
	owner.do("PUT", "/settings", map[string]any{"priorityDevice": samDevice}, &set)
	if set.PriorityDevice != samDevice || !set.AllowSignup {
		t.Fatalf("settings %+v", set)
	}

	// Sam plays something: it shows as playing, marked priority.
	sam.do("PUT", "/progress/"+movie, map[string]any{"positionSec": 300}, nil)
	var now []struct {
		Device, User, Title string
		PositionSec         float64
		Priority            bool
	}
	owner.do("GET", "/playback", nil, &now)
	if len(now) != 1 || now[0].User != "sam" || now[0].Title != "Night Shift" || now[0].PositionSec != 300 || !now[0].Priority {
		t.Fatalf("now playing %+v", now)
	}
	// Nothing converting for sam: carry on.
	var adv struct{ Action string }
	sam.do("GET", "/playback/advice?version=x", nil, &adv)
	if adv.Action != "ok" {
		t.Fatalf("advice %+v", adv)
	}

	// Signing sam's device out ends its session and its priority.
	if resp := owner.do("DELETE", "/devices/"+samDevice, nil, nil); resp.StatusCode != 204 {
		t.Fatalf("sign out: %d", resp.StatusCode)
	}
	if resp := sam.do("GET", "/home", nil, nil); resp.StatusCode != 401 {
		t.Fatalf("signed-out device still works: %d", resp.StatusCode)
	}
	owner.do("GET", "/settings", nil, &set)
	if set.PriorityDevice != "" {
		t.Fatalf("priority kept for a signed-out device: %+v", set)
	}
	if resp := owner.do("DELETE", "/devices/"+samDevice, nil, nil); resp.StatusCode != 404 {
		t.Fatalf("second sign out: %d", resp.StatusCode)
	}
}

// The metrics endpoints: plays become usage, storage adds up, routes are
// counted by pattern, and only the owner can see any of it.
func TestMetricsEndpoints(t *testing.T) {
	st, owner, signIn := featureServer(t)
	ctx := context.Background()
	lib, _ := st.CreateLibrary(ctx, "Movies", t.TempDir(), store.KindMovies)
	movie := addMovie(t, st, lib, "Night Shift")

	// A play: started, then 60 s watched in two reports; a skip ahead
	// counts only the time that passed.
	owner.do("PUT", "/progress/"+movie, map[string]any{"positionSec": 100}, nil)
	owner.do("PUT", "/progress/"+movie, map[string]any{"positionSec": 103}, nil)
	owner.do("PUT", "/progress/"+movie, map[string]any{"positionSec": 2000}, nil)
	var usage struct {
		Plays int
		Hours float64
		Days  []struct {
			Day   string
			Plays int
		}
		Titles []struct {
			Label string
			Plays int
		}
		Users []struct{ Label string }
	}
	if resp := owner.do("GET", "/metrics/usage?days=7", nil, &usage); resp.StatusCode != 200 {
		t.Fatalf("usage: %d", resp.StatusCode)
	}
	if usage.Plays != 1 || len(usage.Days) != 7 || usage.Days[6].Plays != 1 || len(usage.Titles) != 1 || usage.Titles[0].Label != "Night Shift" || usage.Users[0].Label != "alex" {
		t.Fatalf("usage %+v", usage)
	}
	plays, _ := st.PlaysSince(ctx, 0)
	if len(plays) != 1 || plays[0].WatchedSec < 3 || plays[0].WatchedSec > 20 || plays[0].Method != "direct" {
		t.Fatalf("play log %+v", plays)
	}

	var storage struct {
		Libraries []struct {
			Name  string
			Files int
			Bytes int64
		}
		Volumes     []struct{ Total, Free uint64 }
		Composition struct{ Resolution []struct{ Label string } }
	}
	if resp := owner.do("GET", "/metrics/storage", nil, &storage); resp.StatusCode != 200 {
		t.Fatalf("storage: %d", resp.StatusCode)
	}
	if len(storage.Libraries) != 1 || storage.Libraries[0].Files != 1 || len(storage.Volumes) == 0 || len(storage.Composition.Resolution) != 1 {
		t.Fatalf("storage %+v", storage)
	}

	var httpStats struct {
		Routes []struct {
			Route string
			Count int
		}
	}
	owner.do("GET", "/metrics/http", nil, &httpStats)
	found := false
	for _, r := range httpStats.Routes {
		if r.Route == "PUT /api/v1/progress/{id}" && r.Count == 3 {
			found = true
		}
	}
	if !found {
		t.Fatalf("routes %+v", httpStats.Routes)
	}

	hash, _ := auth.HashPassword("password456")
	if _, err := st.CreateUser(ctx, "sam", hash, false); err != nil {
		t.Fatal(err)
	}
	sam := signIn("sam", "password456")
	for _, p := range []string{"/metrics/usage", "/metrics/storage", "/metrics/http", "/metrics/logs"} {
		if resp := sam.do("GET", p, nil, nil); resp.StatusCode != 403 {
			t.Fatalf("non-owner %s: %d", p, resp.StatusCode)
		}
	}
}
