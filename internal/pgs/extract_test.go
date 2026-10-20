package pgs

import (
	"image/png"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"kinara/internal/ffmpeg"
)

// A PGS track muxed into an MKV comes back out as a timeline and PNGs, and
// a second request reads the finished timeline from disk.
func TestExtractFromMKV(t *testing.T) {
	if _, err := exec.LookPath("ffmpeg"); err != nil {
		t.Skip("ffmpeg not installed")
	}
	dir := t.TempDir()
	sup := filepath.Join(dir, "subs.sup")
	if err := os.WriteFile(sup, sampleSup(), 0o644); err != nil {
		t.Fatal(err)
	}
	mkv := filepath.Join(dir, "movie.mkv")
	out, err := exec.Command("ffmpeg", "-loglevel", "error", "-f", "lavfi", "-i", "testsrc2=size=320x180:rate=10:duration=12",
		"-i", sup, "-map", "0:v", "-map", "1:s", "-c:v", "libx264", "-pix_fmt", "yuv420p", "-c:s", "copy",
		// Keep the subtitle times as written (ffmpeg would start the .sup at 0).
		"-copyts", mkv).CombinedOutput()
	if err != nil {
		t.Skipf("can't make a test file with PGS: %v %s", err, out)
	}

	x := &Extractor{Tool: &ffmpeg.Tool{}, Dir: filepath.Join(dir, "cache")}
	var tl Timeline
	var events []TimelineEvent
	deadline := time.Now().Add(20 * time.Second)
	for {
		tl, err = x.Timeline("v1", mkv, 42, 1, "hdmv_pgs_subtitle", len(events))
		if err != nil {
			t.Fatal(err)
		}
		events = append(events, tl.Events...)
		if tl.Done || time.Now().After(deadline) {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	if !tl.Done || tl.Error != "" {
		t.Fatalf("not done: %+v", tl)
	}
	if tl.Width != 1920 || tl.Height != 1080 || len(events) != 2 || tl.Total != 2 {
		t.Fatalf("timeline %+v, events %+v", tl, events)
	}
	if e := events[0]; e.Start != 1 || e.End != 3 || len(e.Images) != 1 || e.Images[0].X != 900 || e.Images[0].W != 100 {
		t.Fatalf("first event %+v", e)
	}
	if e := events[1]; e.Start != 5 || !e.Images[0].Forced {
		t.Fatalf("second event %+v", e)
	}
	f, err := os.Open(x.ImagePath("v1", 42, 1, events[1].Images[0].N))
	if err != nil {
		t.Fatal(err)
	}
	img, err := png.Decode(f)
	f.Close()
	if err != nil || img.Bounds().Dx() != 120 {
		t.Fatalf("picture: %v %v", err, img.Bounds())
	}

	// A fresh extractor (a restarted server) answers from disk, from any event.
	y := &Extractor{Tool: &ffmpeg.Tool{}, Dir: filepath.Join(dir, "cache")}
	again, err := y.Timeline("v1", mkv, 42, 1, "hdmv_pgs_subtitle", 1)
	if err != nil || !again.Done || len(again.Events) != 1 || again.Total != 2 || again.Events[0].Start != 5 {
		t.Fatalf("from disk: %+v %v", again, err)
	}
}

// The same pictures restated at the next composition (as PGS does at each
// acquisition point) extend one event instead of making two.
func TestRepeatExtends(t *testing.T) {
	x := &Extractor{Dir: t.TempDir()}
	j := &job{keys: map[string]TimelineImage{}}
	var got []Event
	_ = Decode(bytesReader(sampleSup()), func(ev Event) error { got = append(got, ev); return nil })
	first := got[0]
	repeat := first
	repeat.Start, repeat.End = first.End, first.End+2
	for _, ev := range []Event{first, repeat} {
		if err := x.add(j, x.Dir, ev); err != nil {
			t.Fatal(err)
		}
	}
	if len(j.tl.Events) != 1 || j.tl.Events[0].End != 5 || j.images != 1 {
		t.Fatalf("events %+v, pictures %d", j.tl.Events, j.images)
	}
}
