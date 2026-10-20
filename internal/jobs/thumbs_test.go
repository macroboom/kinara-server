package jobs

import (
	"strings"
	"testing"

	"kinara/internal/ffmpeg"
)

func TestThumbTime(t *testing.T) {
	cases := []struct{ dur, want float64 }{
		{0, 1},     // unknown length
		{600, 60},  // a tenth in
		{5, 1},     // at least a second
		{1.2, 0.6}, // a clip shorter than that: the middle
	}
	for _, c := range cases {
		if got := ThumbTime(c.dur); got != c.want {
			t.Errorf("ThumbTime(%v) = %v, want %v", c.dur, got, c.want)
		}
	}
}

func TestThumbArgs(t *testing.T) {
	sdr := strings.Join(ThumbArgs("in.mkv", 12.5, false, ffmpeg.Caps{Found: true}, "out.jpg"), " ")
	if !strings.Contains(sdr, "-ss 12.50 -i in.mkv") || !strings.Contains(sdr, "-frames:v 1") || strings.Contains(sdr, "tonemap") {
		t.Errorf("sdr args: %s", sdr)
	}
	hdr := strings.Join(ThumbArgs("in.mkv", 1, true, ffmpeg.Caps{Found: true, ToneMap: true}, "out.jpg"), " ")
	if !strings.Contains(hdr, "tonemap") {
		t.Errorf("hdr args should tone map: %s", hdr)
	}
	placebo := strings.Join(ThumbArgs("in.mkv", 1, true, ffmpeg.Caps{Found: true, ToneMap: true, Placebo: true}, "out.jpg"), " ")
	if !strings.Contains(placebo, "libplacebo") {
		t.Errorf("libplacebo preferred: %s", placebo)
	}
}
