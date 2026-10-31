//go:build !nosqlite

package api

import (
	"archive/zip"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"math/rand/v2"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"kinara/internal/auth"
	"kinara/internal/ffmpeg"
	"kinara/internal/images"
	"kinara/internal/store"
	"kinara/internal/subs"
	"kinara/internal/subsync"
	"kinara/internal/transcode"
	"kinara/internal/watch"
)

// dialogFile writes an audio track with tone bursts where people "speak"
// and returns SRT text for those lines moved by shift seconds.
func dialogFile(t *testing.T, path string, runtime float64, seed uint64) func(shift float64) string {
	t.Helper()
	r := rand.New(rand.NewPCG(seed, seed^0x9e3779b9))
	type line struct{ a, b float64 }
	var lines []line
	var on []string
	for at := 2.0; at < runtime-5; {
		d := 0.8 + r.Float64()*3
		lines = append(lines, line{at, at + d})
		on = append(on, fmt.Sprintf("between(t,%.2f,%.2f)", at, at+d))
		at += d + 0.5 + r.Float64()*4
	}
	if path != "" {
		filter := fmt.Sprintf("[0:a]volume=enable='not(%s)':volume=0[s];[s][1:a]amix=inputs=2:duration=first[a]", strings.Join(on, "+"))
		cmd := exec.Command("ffmpeg", "-hide_banner", "-loglevel", "error",
			"-f", "lavfi", "-i", fmt.Sprintf("sine=frequency=700:sample_rate=48000:duration=%.0f", runtime),
			"-f", "lavfi", "-i", fmt.Sprintf("anoisesrc=amplitude=0.002:sample_rate=48000:duration=%.0f", runtime),
			"-filter_complex", filter, "-map", "[a]", "-c:a", "flac", path)
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Skipf("can't make test audio: %v %s", err, out)
		}
	}
	stamp := func(s float64) string {
		ms := int(s * 1000)
		return fmt.Sprintf("%02d:%02d:%02d,%03d", ms/3600000, ms/60000%60, ms/1000%60, ms%1000)
	}
	return func(shift float64) string {
		var b strings.Builder
		n := 0
		for _, l := range lines {
			if l.a+shift < 0 {
				continue
			}
			n++
			fmt.Fprintf(&b, "%d\n%s --> %s\nSomething said\n\n", n, stamp(l.a+shift), stamp(l.b+shift))
		}
		return b.String()
	}
}

func zipped(t *testing.T, name, body string) []byte {
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	w, _ := zw.Create(name)
	w.Write([]byte(body))
	zw.Close()
	return buf.Bytes()
}

func TestAutoSubtitles(t *testing.T) {
	if _, err := exec.LookPath("ffmpeg"); err != nil {
		t.Skip("ffmpeg not installed")
	}
	ctx := context.Background()
	data := t.TempDir()
	const runtime = 600
	film := filepath.Join(data, "Heat.1995.1080p.BluRay.x264-GRP.mka")
	srtOf := dialogFile(t, film, runtime, 7)
	otherTitle := dialogFile(t, "", runtime, 99)(0)
	right := srtOf(-3.2) // cut for another release: 3.2 s early
	fi, _ := os.Stat(film)

	// A fake SubDL: two candidates, the first for another film.
	var fetched []string
	fake := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/api/v1/subtitles":
			if r.URL.Query().Get("api_key") != "k" || r.URL.Query().Get("languages") != "EN" || r.URL.Query().Get("type") != "movie" {
				t.Errorf("search %s", r.URL.RawQuery)
			}
			json.NewEncoder(w).Encode(map[string]any{"status": true, "subtitles": []map[string]any{
				{"release_name": "Heat.1995.1080p.BluRay.x264-GRP", "lang": "english", "language": "EN", "url": "/subtitle/wrong.zip"},
				{"release_name": "Heat 1995 WEB-DL", "lang": "english", "language": "EN", "url": "/subtitle/right.zip"},
			}})
		case r.URL.Path == "/subtitle/wrong.zip":
			fetched = append(fetched, "wrong")
			w.Write(zipped(t, "Heat.srt", otherTitle))
		case r.URL.Path == "/subtitle/right.zip":
			fetched = append(fetched, "right")
			w.Write(zipped(t, "Heat.srt", right))
		default:
			http.NotFound(w, r)
		}
	}))
	defer fake.Close()

	st, err := store.Open(filepath.Join(data, "kinara.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	hash, _ := auth.HashPassword("password123")
	user, _ := st.CreateUser(ctx, "alex", hash, true)
	profiles, _ := st.Profiles(ctx, user.ID)
	p := profiles[0]
	p.SubtitleMode, p.SubtitleLang = "always", "eng"
	st.UpdateProfile(ctx, p)
	lib, _ := st.CreateLibrary(ctx, "Movies", data, store.KindMovies)
	it := store.Item{LibraryID: lib.ID, Kind: store.ItemMovie, Title: "Heat", SortTitle: "Heat", Year: 1995, SeasonNumber: -1, EpisodeNumber: -1}
	st.CreateItem(ctx, &it)
	v := store.Version{LibraryID: lib.ID, ItemID: it.ID, Path: film, RelPath: filepath.Base(film), Size: fi.Size(), Mtime: fi.ModTime().Unix(),
		Container: "matroska", DurationSec: runtime, AddedAt: time.Now().Unix()}
	st.SaveVersion(ctx, &v, []store.Stream{{Index: 0, Type: "audio", Codec: "flac", Channels: 1, Language: "eng", Default: true}})

	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	tool := &ffmpeg.Tool{}
	srv := &Server{Store: st, Images: images.New(filepath.Join(data, "img")), Watch: &watch.Service{Store: st}, Log: log,
		Version: "test", BaseCtx: ctx, DataDir: data, CacheDir: filepath.Join(data, "cache"),
		Subtitles: &transcode.Subtitles{Tool: tool, Dir: filepath.Join(data, "cache", "subtitles")}}
	srv.SetBuiltinSubDLKey("k")
	srv.subs.subdlBase, srv.subs.subdlDLBase = fake.URL+"/api/v1", fake.URL
	st.SetValue(ctx, keyProviderOff+subs.ProviderSubvault, "true")
	hs := httptest.NewServer(srv.Handler())
	defer hs.Close()
	owner := &client{t: t, base: hs.URL + "/api/v1"}
	var login struct{ Token string }
	owner.do("POST", "/auth/login", map[string]string{"username": "alex", "password": "password123"}, &login)
	owner.token = login.Token

	srv.planAutoSubs(ctx)
	if tg, err := st.AutoSubsTargets(ctx); err != nil || len(tg) != 1 {
		t.Fatalf("targets %+v %v", tg, err)
	}
	if ws, err := srv.profileWants(ctx); err != nil || len(ws) != 1 {
		t.Fatalf("profile wants %+v %v", ws, err)
	}
	wanted, _ := st.WantedFor(ctx, v.ID)
	if len(wanted) != 1 || wanted[0].Language != "eng" || wanted[0].Forced || wanted[0].Priority != prioRecent {
		t.Fatalf("wanted %+v", wanted)
	}
	if _, did := srv.autoSubsOne(ctx); !did {
		t.Fatal("nothing looked for")
	}
	wanted, _ = st.WantedFor(ctx, v.ID)
	if wanted[0].State != store.WantFound || len(fetched) != 2 || fetched[0] != "wrong" {
		t.Fatalf("after looking: %+v, fetched %v", wanted, fetched)
	}
	rejected, _ := st.Rejected(ctx, v.ID)
	if !rejected["subdl|/subtitle/wrong.zip"] || rejected["subdl|/subtitle/right.zip"] {
		t.Fatalf("rejected %v", rejected)
	}

	// The player sees it, lined up.
	var auto struct {
		Subtitles []ExternalSubtitle
		Wanted    []store.SubtitleWant
	}
	owner.do("GET", "/subtitles/"+v.ID+"/auto", nil, &auto)
	if len(auto.Subtitles) != 1 || !auto.Subtitles[0].Auto || auto.Subtitles[0].Sync != "synced" || auto.Subtitles[0].Provider != subs.ProviderSubDL {
		t.Fatalf("auto %+v", auto.Subtitles)
	}
	resp, _ := http.Get(hs.URL + auto.Subtitles[0].URL + "?token=" + owner.token)
	vtt, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	got, err := subsync.ParseCues(vtt)
	want, _ := subsync.ParseCues([]byte(srtOf(0)))
	if err != nil || len(got) == 0 {
		t.Fatalf("served %q %v", vtt, err)
	}
	// Match by line count from the end (early cues before 0 were dropped).
	g, w := got[len(got)-5], want[len(want)-5]
	if d := g.Start - w.Start; d > 250*time.Millisecond || d < -250*time.Millisecond {
		t.Fatalf("served cue at %v, speech at %v", g.Start, w.Start)
	}

	// Said to be wrong: gone, never picked again, looked for again first.
	var wrong struct{ Searching bool }
	if resp := owner.do("POST", fmt.Sprintf("/subtitles/%s/files/%d/wrong", v.ID, auto.Subtitles[0].Index), nil, &wrong); resp.StatusCode != 200 || !wrong.Searching {
		t.Fatalf("wrong %d %+v", resp.StatusCode, wrong)
	}
	files, _ := st.SubtitleFiles(ctx, v.ID)
	rejected, _ = st.Rejected(ctx, v.ID)
	wanted, _ = st.WantedFor(ctx, v.ID)
	if len(files) != 0 || !rejected["subdl|/subtitle/right.zip"] || len(wanted) != 1 || wanted[0].Priority != prioPlaying || wanted[0].State != store.WantWanted {
		t.Fatalf("after wrong: files %d, rejected %v, wanted %+v", len(files), rejected, wanted)
	}
	// Nothing left to try: missing, and retried later.
	srv.autoSubsOne(ctx)
	wanted, _ = st.WantedFor(ctx, v.ID)
	if wanted[0].State != store.WantMissing || wanted[0].NextAt <= time.Now().Unix() {
		t.Fatalf("nothing left: %+v", wanted)
	}

	// The owner's view.
	var status autoSubsStatusView
	owner.do("GET", "/subtitles/auto", nil, &status)
	if !status.Enabled || status.Today != 2 || len(status.Missing) != 1 || status.Missing[0].Title == "" {
		t.Fatalf("status %+v", status)
	}
}

func TestWantsFor(t *testing.T) {
	eng := []store.Stream{{Type: "audio", Language: "eng", Default: true}}
	jpn := []store.Stream{{Type: "audio", Language: "jpn", Default: true}}
	always := []langWant{{"eng", "always", true}}
	if w := wantsFor(always, eng, nil, 0); len(w) != 1 || w[0].Forced {
		t.Fatalf("always: %+v", w)
	}
	withSub := append(append([]store.Stream{}, eng...), store.Stream{Type: "subtitle", Language: "en"})
	if w := wantsFor(always, withSub, nil, 0); len(w) != 0 {
		t.Fatalf("already has English: %+v", w)
	}
	if w := wantsFor(always, eng, []store.SubtitleFile{{Language: "eng"}}, 0); len(w) != 0 {
		t.Fatalf("already has a file: %+v", w)
	}
	off := []langWant{{"eng", "off", true}}
	if w := wantsFor(off, eng, nil, 0); len(w) != 0 {
		t.Fatalf("off, English film: %+v", w)
	}
	if w := wantsFor(off, jpn, nil, 0); len(w) != 1 || w[0].Language != "eng" {
		t.Fatalf("off, foreign film: %+v", w)
	}
	if w := wantsFor([]langWant{{"eng", "forced", false}}, eng, nil, 2); len(w) != 0 {
		t.Fatalf("default profile, English film: %+v", w)
	}
	forced := []langWant{{"eng", "forced", true}}
	if w := wantsFor(forced, eng, nil, 2); len(w) != 1 || !w[0].Forced || w[0].Priority != 1 {
		t.Fatalf("forced: %+v", w)
	}
	if w := wantsFor(forced, jpn, nil, 2); len(w) != 1 || w[0].Forced {
		t.Fatalf("forced, foreign film: %+v", w)
	}
}
