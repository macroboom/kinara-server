//go:build !nosqlite

package api

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"

	"kinara/internal/auth"
	"kinara/internal/store"
)

// sharedSetup is a server with a library shared with sam and two that
// aren't, with something of everything in the hidden ones.
type sharedSetup struct {
	st     *store.Store
	owner  *client
	friend *client
	open   store.Library
	movies store.Library // hidden
	tv     store.Library // hidden
	// Hidden things, by what they are.
	movie, show, season, episode, version, epVersion string
	openMovie                                        string
	listID                                           int64
}

// hiddenMarker is in the title, path and bytes of everything hidden.
const hiddenMarker = "Zqx"

func newSharedSetup(t *testing.T) *sharedSetup {
	t.Helper()
	ctx := context.Background()
	st, owner, signIn := featureServer(t)
	dir := t.TempDir()
	mk := func(name, kind string) store.Library {
		p := filepath.Join(dir, name)
		os.MkdirAll(p, 0o755)
		l, err := st.CreateLibrary(ctx, name, p, kind)
		if err != nil {
			t.Fatal(err)
		}
		return l
	}
	x := &sharedSetup{st: st, owner: owner}
	x.open = mk("Family", store.KindMovies)
	x.movies = mk("Private"+hiddenMarker, store.KindMovies)
	x.tv = mk("PrivateTV"+hiddenMarker, store.KindTV)

	file := func(lib store.Library, itemID, name string) string {
		p := filepath.Join(lib.Path, name)
		if err := os.WriteFile(p, []byte(strings.Repeat(hiddenMarker, 400)), 0o644); err != nil {
			t.Fatal(err)
		}
		old := time.Now().Add(-time.Hour)
		os.Chtimes(p, old, old)
		v := store.Version{LibraryID: lib.ID, ItemID: itemID, Path: p, RelPath: name, Size: 1200, Container: "matroska",
			DurationSec: 60, Width: 1920, Height: 1080, VideoCodec: "h264"}
		streams := []store.Stream{{Index: 0, Type: "video", Codec: "h264"}, {Index: 1, Type: "audio", Codec: "aac", Channels: 2},
			{Index: 2, Type: "subtitle", Codec: "subrip", Language: "eng"}}
		if err := st.SaveVersion(ctx, &v, streams); err != nil {
			t.Fatal(err)
		}
		return v.ID
	}
	item := func(lib store.Library, kind, title, parent string, season, ep int) string {
		it := store.Item{LibraryID: lib.ID, Kind: kind, Title: title, SortTitle: title, ParentID: parent,
			SeasonNumber: season, EpisodeNumber: ep, MatchState: store.MatchMatched}
		if err := st.CreateItem(ctx, &it); err != nil {
			t.Fatal(err)
		}
		return it.ID
	}
	x.openMovie = item(x.open, store.ItemMovie, "Open Door", "", -1, -1)
	file(x.open, x.openMovie, "Open Door (2020).mkv")
	x.movie = item(x.movies, store.ItemMovie, "Secret "+hiddenMarker, "", -1, -1)
	x.version = file(x.movies, x.movie, "Secret "+hiddenMarker+".mkv")
	x.show = item(x.tv, store.ItemShow, "Hidden Show "+hiddenMarker, "", -1, -1)
	x.season = item(x.tv, store.ItemSeason, "Season 1", x.show, 1, -1)
	x.episode = item(x.tv, store.ItemEpisode, "Pilot "+hiddenMarker, x.season, 1, 1)
	x.epVersion = file(x.tv, x.episode, "Hidden Show "+hiddenMarker+" S01E01.mkv")

	// Things that point at hidden titles from the friend's side, as if
	// the owner took a library away after sam watched and saved from it.
	for _, id := range []string{x.openMovie, x.movie} {
		st.SetGenres(ctx, id, []string{"Mystery"})
	}
	st.SetExternalID(ctx, x.movie, "tmdb_movie", "4242")
	for _, id := range []string{x.movie, x.show, x.episode} {
		st.SetImage(ctx, id, "poster", "http://127.0.0.1:1/"+hiddenMarker+".jpg")
		st.SetImage(ctx, id, "still", "http://127.0.0.1:1/"+hiddenMarker+".jpg")
	}
	st.SetExternalID(ctx, x.show, "tmdb_tv", "4343")
	coll := &store.Collection{ID: 77, Name: "Saga " + hiddenMarker}
	st.SetCollection(ctx, x.movie, coll)
	st.SetCollection(ctx, x.openMovie, &store.Collection{ID: 77, Name: "Saga " + hiddenMarker})

	pw, _ := auth.HashPassword("password456")
	sam, err := st.CreateUser(ctx, "sam", pw, false)
	if err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{x.movie, x.episode} {
		st.PutWatch(ctx, sam.ProfileID, store.WatchState{ItemID: id, PositionSec: 20, LastPlayedAt: time.Now().Unix()})
	}
	x.listID, _ = st.DefaultList(ctx, sam.ProfileID)
	st.AddToList(ctx, sam.ProfileID, x.listID, x.movie)
	st.AddToList(ctx, sam.ProfileID, x.listID, x.openMovie)

	// The owner shares only Family with sam.
	if resp := owner.do("PATCH", "/users/"+strconv.FormatInt(sam.ID, 10),
		map[string]any{"libraries": map[string]any{"all": false, "libraries": []int64{x.open.ID}}}, nil); resp.StatusCode != 204 {
		t.Fatalf("share: %d", resp.StatusCode)
	}
	x.friend = signIn("sam", "password456")
	return x
}

func (x *sharedSetup) hiddenIDs() []string {
	return []string{x.movie, x.show, x.season, x.episode, x.version, x.epVersion,
		strconv.FormatInt(x.movies.ID, 10), strconv.FormatInt(x.tv.ID, 10)}
}

// get fetches a path as c and returns the status and body.
func fetch(c *client, method, path string, body any) (int, string) {
	c.t.Helper()
	hc := &http.Client{Timeout: 4 * time.Second}
	cc := *c
	cc.http = hc
	var status int
	var text string
	func() {
		defer func() {
			if r := recover(); r != nil {
				status, text = -1, fmt.Sprint(r)
			}
		}()
		var r io.Reader
		if body != nil {
			j, _ := json.Marshal(body)
			r = bytes.NewReader(j)
		}
		req, _ := http.NewRequest(method, cc.base+path, r)
		req.Header.Set("Authorization", "Bearer "+cc.token)
		resp, err := hc.Do(req)
		if err != nil {
			resp = nil
		}
		if resp == nil {
			status = -1
			return
		}
		defer resp.Body.Close()
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
		status, text = resp.StatusCode, string(b)
	}()
	return status, text
}

// TestSharingAccounts: the owner sees and sets each account's libraries.
func TestSharingAccounts(t *testing.T) {
	x := newSharedSetup(t)
	var users []struct {
		Username  string
		IsAdmin   bool
		Libraries store.LibraryAccess
	}
	x.owner.do("GET", "/users", nil, &users)
	got := map[string]store.LibraryAccess{}
	for _, u := range users {
		got[u.Username] = u.Libraries
	}
	if !got["alex"].All {
		t.Fatalf("owner access = %+v", got["alex"])
	}
	if s := got["sam"]; s.All || len(s.Libraries) != 1 || s.Libraries[0] != x.open.ID {
		t.Fatalf("sam access = %+v", s)
	}
	var libs []store.Library
	x.friend.do("GET", "/libraries", nil, &libs)
	if len(libs) != 1 || libs[0].ID != x.open.ID {
		t.Fatalf("sam's libraries = %+v", libs)
	}
	// A deleted library drops out of the grant.
	x.owner.do("DELETE", "/libraries/"+strconv.FormatInt(x.open.ID, 10), nil, nil)
	acc, _ := x.st.UserLibraries(context.Background(), x.friendID(t))
	if acc.All || len(acc.Libraries) != 0 {
		t.Fatalf("after delete: %+v", acc)
	}
	// Giving everything back.
	x.owner.do("PATCH", "/users/"+strconv.FormatInt(x.friendID(t), 10), map[string]any{"libraries": map[string]any{"all": true}}, nil)
	x.friend.do("GET", "/libraries", nil, &libs)
	if len(libs) != 2 {
		t.Fatalf("all again: %d libraries", len(libs))
	}
}

func (x *sharedSetup) friendID(t *testing.T) int64 {
	u, err := x.st.UserByName(context.Background(), "sam")
	if err != nil {
		t.Fatal(err)
	}
	return u.ID
}

// routeRE finds the routes the server registers.
var routeRE = regexp.MustCompile(`mux\.Handle(?:Func)?\("([A-Z]+) (/[^"]*)"`)

func registeredRoutes(t *testing.T) [][2]string {
	t.Helper()
	files, _ := filepath.Glob("*.go")
	seen := map[string]bool{}
	var out [][2]string
	for _, f := range files {
		if strings.HasSuffix(f, "_test.go") {
			continue
		}
		b, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		for _, m := range routeRE.FindAllStringSubmatch(string(b), -1) {
			k := m[1] + " " + m[2]
			if !seen[k] {
				seen[k] = true
				out = append(out, [2]string{m[1], m[2]})
			}
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i][1]+out[i][0] < out[j][1]+out[j][0] })
	if len(out) < 150 {
		t.Fatalf("found only %d routes", len(out))
	}
	return out
}

// Routes the walk leaves alone: they end the session or change the server
// in ways that would spoil the rest of the walk (all owner-only anyway,
// apart from signing out).
var walkSkip = map[string]bool{
	"POST /api/v1/auth/logout": true, "POST /logout": true, "POST /api/v1/setup": true, "POST /setup": true,
	"POST /api/v1/update/install": true, "POST /api/v1/backups/restore": true, "POST /api/v1/backups/{name}/restore": true,
	"GET /api/v1/speedtest": true, "PUT /api/v1/me/device": true, "PUT /api/v1/me/device/name": true,
	"DELETE /api/v1/devices/{id}": true,
}

// TestSharingNoLeaks walks every route as a friend shared one library,
// putting the IDs of hidden titles, files and libraries wherever a route
// takes one, and checks nothing about them comes back and nothing about
// them can be changed.
func TestSharingNoLeaks(t *testing.T) {
	x := newSharedSetup(t)
	hidden := x.hiddenIDs()
	params := map[string]string{"kind": "poster", "index": "2", "n": "0", "file": "x", "sheet": "0", "ord": "0",
		"session": "x", "section": "trending", "name": "x", "delivery": "1", "genreId": "1", "code": "x",
		"token": "x", "clientId": "x"}
	body := func(id string) map[string]any {
		return map[string]any{"itemId": id, "versionId": id, "id": id, "libraryId": id, "items": []string{id},
			"positionSec": 30, "watched": true, "completed": true, "title": "x", "kind": "movie", "tmdbId": 4242,
			"mediaId": id, "versions": []string{id}, "quality": "1080p"}
	}
	leaks := []string{}
	check := func(label string, status int, text string) {
		if status == -1 {
			return // timed out: a long poll, nothing came back
		}
		for _, h := range append([]string{hiddenMarker}, x.hiddenIDs()[:6]...) {
			if strings.Contains(text, h) {
				leaks = append(leaks, fmt.Sprintf("%s -> %d mentions %q: %.200s", label, status, h, text))
				return
			}
		}
	}
	// A hidden thing must look exactly like one that doesn't exist: the
	// same answer as for a made-up ID of the same shape.
	fakeFor := func(id string) string {
		if _, err := strconv.Atoi(id); err == nil {
			return "9999"
		}
		return strings.Repeat("f", len(id))
	}
	for _, rt := range registeredRoutes(t) {
		method, pattern := rt[0], rt[1]
		if walkSkip[method+" "+pattern] || strings.HasPrefix(pattern, "/app") {
			continue
		}
		path := strings.TrimSuffix(pattern, "{$}")
		for k, v := range params {
			path = strings.ReplaceAll(path, "{"+k+"}", v)
		}
		mutating := method != "GET"
		c := *x.friend
		if strings.HasPrefix(path, "/api/v1") {
			path = strings.TrimPrefix(path, "/api/v1")
		} else {
			c.base = strings.TrimSuffix(c.base, "/api/v1")
		}
		fill := func(id string) string {
			if strings.Contains(path, "{item}") && strings.HasPrefix(path, "/lists/") {
				return strings.ReplaceAll(strings.ReplaceAll(path, "{id}", strconv.FormatInt(x.listID, 10)), "{item}", id)
			}
			return strings.ReplaceAll(strings.ReplaceAll(path, "{id}", id), "{item}", id)
		}
		query := func(id string) string {
			return "?itemId=" + id + "&versionId=" + id + "&libraryId=" + id + "&id=" + id + "&parentId=" + id +
				"&q=" + hiddenMarker + "&search=" + hiddenMarker
		}
		var bodyOf func(string) any = func(string) any { return nil }
		if mutating {
			bodyOf = func(id string) any { return body(id) }
		}
		inPath := strings.Contains(path, "{id}") || strings.Contains(path, "{item}")
		for _, id := range hidden {
			label := method + " " + fill(id)
			// Library IDs are small numbers that other routes' own IDs
			// (your profiles, lists) share; only library routes take them.
			_, numErr := strconv.Atoi(id)
			if inPath && numErr == nil && !strings.Contains(path, "libraries") {
				continue
			}
			if inPath {
				fakeSt, _ := fetch(&c, method, fill(fakeFor(id))+query(fakeFor(id)), bodyOf(fakeFor(id)))
				st, text := fetch(&c, method, fill(id)+query(id), bodyOf(id))
				check(label, st, text)
				if st != fakeSt && st != -1 && fakeSt != -1 {
					leaks = append(leaks, fmt.Sprintf("%s -> %d, but %d for an ID that doesn't exist", label, st, fakeSt))
				}
			} else if !mutating {
				st, text := fetch(&c, method, path+query(id), nil)
				check(label+query(id), st, text)
			}
		}
		if mutating && !inPath {
			for _, id := range hidden {
				fakeSt, _ := fetch(&c, method, path, body(fakeFor(id)))
				st, text := fetch(&c, method, path, body(id))
				check(method+" "+path+" body="+id, st, text)
				if st != fakeSt && st != -1 && fakeSt != -1 {
					leaks = append(leaks, fmt.Sprintf("%s body=%s -> %d, but %d for an ID that doesn't exist", method, path, st, fakeSt))
				}
			}
		}
	}

	// The friend's own things that point at hidden titles.
	for _, p := range []string{"/home", "/lists", "/lists/" + strconv.FormatInt(x.listID, 10) + "/items",
		"/items?search=" + hiddenMarker, "/search?q=" + hiddenMarker, "/search?q=Secret", "/items?kind=movie&groupCollections=1",
		"/items?kind=movie&sort=added", "/items?kind=show", "/items/letters", "/genres", "/genres?kind=movie", "/playback",
		"/items?other=1", "/items?libraryId=" + strconv.FormatInt(x.movies.ID, 10), "/discover", "/trending",
		"/libraries/" + strconv.FormatInt(x.movies.ID, 10) + "/problems", "/me", "/ontheway", "/requests", "/downloads"} {
		st, text := fetch(x.friend, "GET", p, nil)
		check("GET "+p, st, text)
	}
	for _, l := range leaks {
		t.Error(l)
	}

	// The owner still sees it all.
	if st, text := fetch(x.owner, "GET", "/items/"+x.movie, nil); st != 200 || !strings.Contains(text, hiddenMarker) {
		t.Fatalf("owner: %d %s", st, text)
	}
	// And sam still sees what was shared.
	if st, text := fetch(x.friend, "GET", "/items/"+x.openMovie, nil); st != 200 || !strings.Contains(text, "Open Door") {
		t.Fatalf("shared title: %d %s", st, text)
	}
}
