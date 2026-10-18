package intro

import (
	"context"
	"math"
	"math/rand"
	"os/exec"
	"path/filepath"
	"testing"
	"time"
)

func TestTailStart(t *testing.T) {
	if _, ok := TailStart(0); ok {
		t.Error("unknown length should not be searched")
	}
	if _, ok := TailStart(4 * time.Minute); ok {
		t.Error("a 4 minute file should not be searched")
	}
	// 22 minutes: 40% is 8.8 minutes, so the 7 minute cap applies.
	if got, _ := TailStart(22 * time.Minute); got != 15*time.Minute {
		t.Errorf("22 min: from %v, want 15m", got)
	}
	// 11 minutes: 40% is 4.4 minutes.
	if got, _ := TailStart(11 * time.Minute); got != 11*time.Minute-264*time.Second {
		t.Errorf("11 min: from %v, want 6m36s", got)
	}
}

// Credits that share an ending theme are found in the tails, longer than an
// intro may be.
func TestDetectCreditsInTails(t *testing.T) {
	th := theme(rand.New(rand.NewSource(700)), 170) // longer than MaxIntro
	before := []float64{200, 150, 240}
	var prints [][]uint32
	for i, b := range before {
		seed := int64(710 + 10*i)
		ep := episode(rand.New(rand.NewSource(seed)), 1.0-0.1*float64(i), 25,
			speech(rand.New(rand.NewSource(seed+1)), b), th, speech(rand.New(rand.NewSource(seed+2)), 20))
		prints = append(prints, Fingerprint(ep))
	}
	got := DetectIn(prints, CreditsBounds)
	for i, b := range before {
		r, ok := got[i]
		if !ok || !near(r.Start, b) || !near(r.End, b+170) {
			t.Errorf("episode %d: got %+v ok=%v, want about [%g,%g]", i, r, ok, b, b+170)
		}
	}
}

func TestParseBlackframe(t *testing.T) {
	out := []byte(`Input #0, matroska,webm, from 'x.mkv':
[Parsed_blackframe_2 @ 0x5587c1713c40] frame:0 pblack:16 pts:23 t:0.023000 type:I last_keyframe:0
[Parsed_blackframe_2 @ 0x5587c1713c40] frame:1 pblack:99 pts:2023 t:2.023000 type:I last_keyframe:1
garbage pblack:x t:y
`)
	got := parseBlackframe(out)
	if len(got) != 2 || got[0].black != 16 || got[1].black != 99 || math.Abs(got[1].t-2.023) > 1e-9 {
		t.Errorf("got %+v", got)
	}
}

func frames(step float64, spans ...[3]float64) []blackFrame {
	// spans are {from, to, black}; frames every step seconds.
	var out []blackFrame
	for _, s := range spans {
		for t := s[0]; t < s[1]-1e-9; t += step {
			out = append(out, blackFrame{t, int(s[2])})
		}
	}
	return out
}

func TestBlackRun(t *testing.T) {
	cases := []struct {
		name   string
		frames []blackFrame
		want   Range
		ok     bool
	}{
		{"credits to the end", frames(2, [3]float64{0, 300, 20}, [3]float64{300, 360, 99}), Range{300, 358}, true},
		{"too short", frames(2, [3]float64{0, 300, 20}, [3]float64{300, 320, 99}, [3]float64{320, 360, 20}), Range{}, false},
		{"logo inside credits", frames(2, [3]float64{0, 200, 20}, [3]float64{200, 230, 99}, [3]float64{230, 234, 10}, [3]float64{234, 260, 95}, [3]float64{260, 300, 30}),
			Range{200, 260}, true},
		{"last long run wins", frames(2, [3]float64{0, 50, 99}, [3]float64{50, 200, 20}, [3]float64{200, 250, 99}, [3]float64{250, 300, 20}),
			Range{200, 250}, true},
		{"long break splits", frames(2, [3]float64{0, 100, 20}, [3]float64{100, 140, 99}, [3]float64{140, 150, 20}, [3]float64{150, 170, 99}),
			Range{100, 140}, true},
		{"nothing", nil, Range{}, false},
	}
	for _, c := range cases {
		got, ok := blackRun(c.frames)
		if ok != c.ok || (ok && (math.Abs(got.Start-c.want.Start) > 1e-9 || math.Abs(got.End-c.want.End) > 1e-9)) {
			t.Errorf("%s: got %+v ok=%v, want %+v ok=%v", c.name, got, ok, c.want, c.ok)
		}
	}
}

// A 100 s clip: a test pattern, then 40 s of scrolling text on black.
// Both the seek and the picture check run through ffmpeg.
func TestCreditsWithFFmpeg(t *testing.T) {
	if _, err := exec.LookPath("ffmpeg"); err != nil {
		t.Skip("ffmpeg not installed")
	}
	ctx := context.Background()
	dir := t.TempDir()
	clip := filepath.Join(dir, "ep.mkv")
	cmd := exec.Command("ffmpeg", "-y", "-loglevel", "error",
		"-f", "lavfi", "-i", "testsrc2=size=640x360:rate=24:duration=60",
		"-f", "lavfi", "-i", "color=black:size=640x360:rate=24:duration=40",
		"-f", "lavfi", "-i", "sine=f=440:duration=100",
		"-filter_complex", "[1:v]drawbox=x=200:y=150+t*2:w=240:h=14:color=white:t=fill[c];[0:v][c]concat=n=2:v=1[v]",
		"-map", "[v]", "-map", "2:a", "-c:v", "libx264", "-preset", "ultrafast", "-g", "48", "-c:a", "aac", "-shortest", clip)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Skipf("can't make a test clip: %v %s", err, out)
	}
	from, ok := TailStart(100 * time.Second)
	if ok {
		t.Fatalf("a 100 s clip is under 5 minutes; got from %v", from)
	}

	r, ok, err := BlackCredits(ctx, "", clip, 30*time.Second, 70*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if !ok || math.Abs(r.Start-60) > 2.5 || r.End < 95 {
		t.Errorf("got %+v ok=%v, want credits from about 60 to the end", r, ok)
	}
	if _, ok, err := BlackCredits(ctx, "", clip, 0, 55*time.Second); err != nil || ok {
		t.Errorf("no credits in the first 55 s: ok=%v err=%v", ok, err)
	}

	// DecodeFrom starts exactly at the offset.
	samples, err := DecodeFrom(ctx, "", clip, 30*time.Second, 70*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if want := 70 * SampleRate; math.Abs(float64(len(samples)-want)) > SampleRate/5 {
		t.Errorf("decoded %d samples, want about %d", len(samples), want)
	}
}
