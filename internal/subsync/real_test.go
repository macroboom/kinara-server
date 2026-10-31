package subsync

import (
	"context"
	"fmt"
	"math/rand/v2"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// makeDialog writes an audio file with tone bursts where people "speak",
// and returns those lines as cues on the audio's own clock.
func makeDialog(t *testing.T, dir string, runtime float64, seed uint64) (string, []Cue) {
	t.Helper()
	if _, err := exec.LookPath("ffmpeg"); err != nil {
		t.Skip("ffmpeg not installed")
	}
	r := rand.New(rand.NewPCG(seed, seed^0x9e3779b9))
	var cues []Cue
	var on []string
	at := 2.0
	for at < runtime-5 {
		d := 0.8 + r.Float64()*3
		cues = append(cues, Cue{Start: dur(at), End: dur(at + d), Lines: []string{"line"}})
		on = append(on, fmt.Sprintf("between(t,%.2f,%.2f)", at, at+d))
		at += d + 0.5 + r.Float64()*4
	}
	path := filepath.Join(dir, fmt.Sprintf("dialog-%d.mka", seed))
	// Speech-band tone, quiet noise between lines (a real track is never silent).
	filter := fmt.Sprintf("[0:a]volume=enable='not(%s)':volume=0[s];[s][1:a]amix=inputs=2:duration=first[a]", strings.Join(on, "+"))
	cmd := exec.Command("ffmpeg", "-hide_banner", "-loglevel", "error",
		"-f", "lavfi", "-i", fmt.Sprintf("sine=frequency=700:sample_rate=48000:duration=%.0f", runtime),
		"-f", "lavfi", "-i", fmt.Sprintf("anoisesrc=amplitude=0.002:sample_rate=48000:duration=%.0f", runtime),
		"-filter_complex", filter, "-map", "[a]", "-c:a", "flac", path)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Skipf("can't make test audio: %v %s", err, out)
	}
	return path, cues
}

func dur(s float64) time.Duration { return time.Duration(s * float64(time.Second)) }

func shift(cues []Cue, by time.Duration) []Cue {
	out := make([]Cue, len(cues))
	for i, c := range cues {
		out[i] = Cue{Start: c.Start + by, End: c.End + by, Lines: c.Lines}
	}
	return out
}

func TestSyncRealAudio(t *testing.T) {
	dir := t.TempDir()
	const runtime = 600
	audio, cues := makeDialog(t, dir, runtime, 7)
	plan, err := PlanSpeech(runtime, []AudioTrack{{Ordinal: 0, Language: "eng", Channels: 2, Default: true}}, "en")
	if err != nil {
		t.Fatal(err)
	}
	levels, err := Decode(context.Background(), "ffmpeg", audio, plan, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(levels) != len(plan.Windows) {
		t.Fatalf("%d windows, want %d", len(levels), len(plan.Windows))
	}

	// Subtitles cut for another release: 3.2 s early.
	early := shift(cues, -3200*time.Millisecond)
	a, err := Align(levels, early)
	if err != nil {
		t.Fatal(err)
	}
	if got := Decide(a, Timing{}, dur(runtime)); got != StatusSynced {
		t.Fatalf("status %s, alignment %+v", got, a)
	}
	if off := a.Timing.Apply(early[10].Start) - cues[10].Start; off > 200*time.Millisecond || off < -200*time.Millisecond {
		t.Fatalf("corrected cue off by %v (timing %+v)", off, a.Timing)
	}

	// Already right.
	if a, _ := Align(levels, cues); Decide(a, Timing{}, dur(runtime)) != StatusAlreadySynced {
		t.Fatalf("right subtitles not taken as right: %+v", a)
	}

	// Another title's subtitles don't line up.
	_, other := makeDialog(t, dir, runtime, 99)
	if a, _ := Align(levels, other); Decide(a, Timing{}, dur(runtime)) != StatusNoMatch {
		t.Fatalf("another title's subtitles matched: %+v", a)
	}
}

func TestRetimeVTT(t *testing.T) {
	in := "WEBVTT\n\n00:00:01.000 --> 00:00:02.500 line:90%\nHello\n\n00:01:00.000 --> 00:01:02.000\nLater\n"
	out := string(RetimeVTT([]byte(in), Timing{OffsetMS: -1500}))
	if !strings.Contains(out, "00:00:00.000 --> 00:00:01.000 line:90%") || !strings.Contains(out, "00:00:58.500 --> 00:01:00.500") {
		t.Fatalf("retimed:\n%s", out)
	}
	// A cue moved before the start is dropped.
	out = string(RetimeVTT([]byte(in), Timing{OffsetMS: -3000}))
	if strings.Contains(out, "Hello") || !strings.Contains(out, "Later") || !strings.HasPrefix(out, "WEBVTT") {
		t.Fatalf("retimed:\n%s", out)
	}
	if string(RetimeVTT([]byte(in), Timing{})) != in {
		t.Fatal("identity changed the file")
	}
	cues, err := ParseCues([]byte(in))
	if err != nil || len(cues) != 2 || cues[1].Start != time.Minute {
		t.Fatalf("cues %+v %v", cues, err)
	}
}
