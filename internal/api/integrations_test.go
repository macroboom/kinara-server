package api

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"kinara/internal/store"
)

func TestSignature(t *testing.T) {
	body := []byte(`{"id":"dlv_1","event":"test"}`)
	mac := hmac.New(sha256.New, []byte("whsec_abc"))
	mac.Write([]byte("1792430464." + string(body)))
	want := "sha256=" + hex.EncodeToString(mac.Sum(nil))
	if got := signature("whsec_abc", 1792430464, body); got != want {
		t.Fatalf("signature = %s, want %s", got, want)
	}
	if signature("other", 1792430464, body) == want || signature("whsec_abc", 1792430465, body) == want {
		t.Fatal("the secret and timestamp must both count")
	}
}

func parseDoc(t *testing.T, s string) any {
	t.Helper()
	dec := json.NewDecoder(strings.NewReader(s))
	dec.UseNumber()
	var v any
	if err := dec.Decode(&v); err != nil {
		t.Fatal(err)
	}
	return v
}

func TestReadPath(t *testing.T) {
	doc := parseDoc(t, `{"item":{"name":"Masterplan","ids":{"tmdb":912649},"parts":[{"season":2,"episode":5}]},"x":null}`)
	cases := []struct {
		path string
		want string
		ok   bool
	}{
		{"item.name", "Masterplan", true},
		{"item.ids.tmdb", "912649", true},
		{"item.parts[0].season", "2", true},
		{" item.parts[0].episode ", "5", true},
		{"item.parts[1].season", "", false},
		{"item.missing", "", false},
		{"item.name.deeper", "", false},
		{"x", "", false},
		{"", "", false},
	}
	for _, c := range cases {
		v, ok := readPath(doc, c.path)
		if ok != c.ok || asString(v) != c.want {
			t.Errorf("readPath(%q) = %v, %v; want %q, %v", c.path, v, ok, c.want, c.ok)
		}
	}
}

func TestStateAndProgress(t *testing.T) {
	m := []store.StateMapping{{From: "running", To: stateInProgress}, {From: "Finished", To: stateDone}}
	for raw, want := range map[string]string{"RUNNING": stateInProgress, "finished": stateDone, "queued": stateQueued,
		"FAILED": stateFailed, "weird": ""} {
		if got := mapState(raw, m); got != want {
			t.Errorf("mapState(%q) = %q, want %q", raw, got, want)
		}
	}
	for _, c := range []struct {
		p     float64
		scale string
		want  float64
	}{
		{0.45, "auto", 0.45}, {72, "auto", 0.72}, {1, "auto", 1}, {50, "fraction", 1},
		{0.5, "percent", 0.005}, {150, "percent", 1}, {-3, "auto", 0},
	} {
		if got := scaleProgress(c.p, c.scale); got != c.want {
			t.Errorf("scaleProgress(%v, %s) = %v, want %v", c.p, c.scale, got, c.want)
		}
	}
}

func TestReadUpdates(t *testing.T) {
	upd, err := readKinara(parseDoc(t, `{"tmdbId":1399,"kind":"show","season":2,"episode":5,"state":"in_progress","progress":0.45,"message":"Copying","eta":600,"id":"job-7"}`), "auto")
	if err != nil || upd.TMDBID != 1399 || upd.Kind != "show" || upd.Season != 2 || upd.Episode != 5 ||
		upd.State != stateInProgress || *upd.Progress != 0.45 || upd.EtaSec != 600 || upd.Ref != "job-7" {
		t.Fatalf("kinara: %+v %v", upd, err)
	}
	// Progress decides when there's no state.
	upd, _ = readKinara(parseDoc(t, `{"title":"Masterplan","year":2026,"progress":100}`), "auto")
	if upd.State != stateDone || upd.Season != -1 {
		t.Fatalf("progress 100 is done: %+v", upd)
	}
	for _, bad := range []string{`{"tmdbId":1,"kind":"movie","state":"copying"}`, `{"state":"queued"}`,
		`{"tmdbId":1,"kind":"movie"}`, `{"tmdbId":"abc","state":"queued"}`, `[1,2]`, `{"title":"x","state":"queued","kind":"song"}`} {
		if _, err := readKinara(parseDoc(t, bad), "auto"); err == nil {
			t.Errorf("%s should be refused", bad)
		}
	}

	h := store.IncomingHook{Format: "custom", ProgressScale: "percent",
		Mapping: map[string]string{"tmdbId": "item.ids.tmdb", "title": "item.name", "kind": "item.type", "state": "job.state",
			"progress": "job.percent", "message": "job.note", "eta": "job.remaining_s"},
		StateMap: []store.StateMapping{{From: "running", To: stateInProgress}}}
	upd, err = readMapped(parseDoc(t, `{"job":{"state":"running","percent":"72","note":"Transferring","remaining_s":240},"item":{"type":"movie","name":"Masterplan","ids":{"tmdb":912649}}}`), h)
	if err != nil || upd.TMDBID != 912649 || upd.Kind != "movie" || upd.State != stateInProgress || *upd.Progress != 0.72 ||
		upd.Message != "Transferring" || upd.EtaSec != 240 {
		t.Fatalf("mapped: %+v %v", upd, err)
	}
	// A state not in the list is ignored, and progress decides.
	upd, err = readMapped(parseDoc(t, `{"job":{"state":"odd","percent":100},"item":{"type":"movie","ids":{"tmdb":1}}}`), h)
	if err != nil || upd.State != stateDone {
		t.Fatalf("unknown state: %+v %v", upd, err)
	}
}

func TestSlidingLimiter(t *testing.T) {
	var l slidingLimiter
	t0 := time.Unix(1000, 0)
	for i := range 2 {
		if ok, _ := l.allow("a", 2, time.Second, t0.Add(time.Duration(i)*100*time.Millisecond)); !ok {
			t.Fatalf("hit %d refused", i)
		}
	}
	ok, wait := l.allow("a", 2, time.Second, t0.Add(300*time.Millisecond))
	if ok || wait != 700*time.Millisecond {
		t.Fatalf("third hit: %v, wait %v", ok, wait)
	}
	if ok, _ := l.allow("b", 2, time.Second, t0.Add(300*time.Millisecond)); !ok {
		t.Fatal("keys are separate")
	}
	if ok, _ := l.allow("a", 2, time.Second, t0.Add(1100*time.Millisecond)); !ok {
		t.Fatal("the window slides")
	}
}

func TestRetryWaits(t *testing.T) {
	total := time.Duration(0)
	want := map[int]time.Duration{1: time.Minute, 3: 15 * time.Minute, 5: 2 * time.Hour, 8: 12 * time.Hour}
	for n := 1; n <= 8; n++ {
		total += retryWait(n)
		if w, ok := want[n]; ok && total != w {
			t.Errorf("%d retries take %v, want %v", n, total, w)
		}
	}
	if retryWait(20) != retryWait(8) || retryWait(0) != retryWait(1) {
		t.Error("waits are clamped")
	}
}

func TestWantsEvent(t *testing.T) {
	h := store.Webhook{Events: []string{"playback.started"}, Libraries: []int64{1}}
	if !wantsEvent(h, "playback.started", eventScope{LibraryID: 1, UserID: 9}) {
		t.Error("library 1 is wanted")
	}
	if wantsEvent(h, "playback.started", eventScope{LibraryID: 2}) || wantsEvent(h, "playback.stopped", eventScope{LibraryID: 1}) {
		t.Error("other libraries and events are not")
	}
	if !wantsEvent(h, "playback.started", eventScope{}) {
		t.Error("no library: no filter")
	}
	h.Accounts = []int64{}
	if wantsEvent(h, "playback.started", eventScope{LibraryID: 1, UserID: 3}) {
		t.Error("an empty account list wants nobody")
	}
}
