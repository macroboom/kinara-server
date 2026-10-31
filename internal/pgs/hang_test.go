package pgs

import (
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"testing"
	"time"

	"kinara/internal/ffmpeg"
)

// An ffprobe that stops reading the track early doesn't leave ffmpeg (and
// the reading slot) waiting on a full pipe.
func TestDVDListerStopsEarly(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("uses a shell script")
	}
	for _, bin := range []string{"ffmpeg", "ffprobe"} {
		if _, err := exec.LookPath(bin); err != nil {
			t.Skip(bin + " not installed")
		}
	}
	real, _ := exec.LookPath("ffprobe")
	dir := t.TempDir()
	// A track long enough to fill the pipe: two thousand subtitles.
	var subs []vobSub
	for i := 0; i < 2000; i++ {
		subs = append(subs, vobSub{at: float64(1 + 2*i), dur: 1})
	}
	big := writeVobSubs(t, dir, subs)
	fake := filepath.Join(dir, "ffprobe")
	script := "#!/bin/sh\nfor a in \"$@\"; do [ \"$a\" = pipe:0 ] && { head -c 1000 >/dev/null; echo '{\"packets\":[]}'; exit 0; }; done\nexec " + real + " \"$@\"\n"
	if err := os.WriteFile(fake, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	x := &Extractor{Tool: &ffmpeg.Tool{}, FFprobe: fake, Dir: filepath.Join(dir, "cache")}
	done := make(chan struct{})
	go func() {
		defer close(done)
		for {
			tl, err := x.Timeline("v1", big, 1, 0, "dvd_subtitle", 0)
			if err != nil || tl.Done {
				return
			}
			time.Sleep(50 * time.Millisecond)
		}
	}()
	select {
	case <-done:
	case <-time.After(30 * time.Second):
		t.Fatal("reading the track hung after ffprobe stopped early")
	}
}
