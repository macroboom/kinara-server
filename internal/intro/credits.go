package intro

import (
	"bufio"
	"bytes"
	"context"
	"fmt"
	"os/exec"
	"regexp"
	"strconv"
	"strings"
	"time"

	"kinara/internal/ffmpeg"
)

// End credits are found two ways:
//
//   - By audio, like intros: the end of each episode is compared with the
//     ends of its neighbours, and music they share (an ending theme, the
//     credits score) marks the credits. Callers decode the end with
//     DecodeFrom and run DetectIn with CreditsBounds.
//   - By picture, for episodes whose credits share no audio with the others
//     (a different song each week, or credits over the last scene): credits
//     are text on a black screen, so the last long stretch of mostly black
//     frames near the end is taken as the credits. BlackCredits does this.

// CreditsTail is how much of the end of an episode is searched for credits:
// at most this, and at most 40% of the episode.
const CreditsTail = 7 * time.Minute

// TailStart returns where the credits search starts in an episode of the
// given length, and ok=false for episodes too short to search.
func TailStart(duration time.Duration) (time.Duration, bool) {
	if duration < 5*time.Minute {
		return 0, false
	}
	tail := min(CreditsTail, duration*2/5)
	return duration - tail, true
}

const (
	// blackShare is the share of a frame's pixels, in percent, that must be
	// near black for the frame to look like credits (text on black).
	blackShare = 88
	// blackLevel is the brightest pixel value (of 255) counted as black.
	blackLevel = 32
	// minBlackCredits is the shortest stretch of credit-like frames taken as
	// credits; dark scenes rarely stay that dark for this long.
	minBlackCredits = 30.0
	// maxBlackGap is how long the credits may show something else (a still,
	// a logo) and still count as one stretch.
	maxBlackGap = 6.0
)

// BlackCredits looks for credits in the picture of path between from and
// from+span, checking only keyframes (fast: nothing else is decoded). It
// returns the credits' range in seconds from the start of the file.
func BlackCredits(ctx context.Context, ffmpegBin, path string, from, span time.Duration) (Range, bool, error) {
	if ffmpegBin == "" {
		ffmpegBin = "ffmpeg"
	}
	args := []string{"-nostdin", "-hide_banner", "-nostats", "-loglevel", "info",
		"-skip_frame", "nokey",
		"-ss", strconv.FormatFloat(from.Seconds(), 'f', 3, 64),
		"-t", strconv.FormatFloat(span.Seconds(), 'f', 3, 64),
		"-i", path,
		"-map", "0:v:0", "-an", "-sn", "-dn",
		// Every frame is reported (amount=0) with its share of black pixels.
		"-vf", fmt.Sprintf("scale=320:-2:flags=fast_bilinear,format=gray,blackframe=amount=0:threshold=%d", blackLevel),
		"-fps_mode", "passthrough",
		"-f", "null", "-",
	}
	cmd := exec.CommandContext(ctx, ffmpegBin, args...)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	err := ffmpeg.StartLow(cmd)
	if err == nil {
		err = cmd.Wait()
	}
	if err != nil {
		if ctx.Err() != nil {
			return Range{}, false, ctx.Err()
		}
		msg := stderr.String()
		if len(msg) > 500 {
			msg = msg[len(msg)-500:]
		}
		return Range{}, false, fmt.Errorf("intro: credits picture %s: %w: %s", path, err, strings.TrimSpace(msg))
	}
	frames := parseBlackframe(stderr.Bytes())
	r, ok := blackRun(frames)
	if !ok {
		return Range{}, false, nil
	}
	off := from.Seconds()
	return Range{Start: r.Start + off, End: r.End + off}, true, nil
}

// blackFrame is one checked frame: its time and share of black pixels.
type blackFrame struct {
	t     float64
	black int
}

var reBlackframe = regexp.MustCompile(`pblack:(\d+)\s.*?\bt:([0-9.]+)`)

// parseBlackframe reads ffmpeg's blackframe lines:
// "[Parsed_blackframe_2 @ 0x..] frame:3 pblack:97 pts:12012 t:12.012000 type:I last_keyframe:3".
func parseBlackframe(out []byte) []blackFrame {
	var frames []blackFrame
	sc := bufio.NewScanner(bytes.NewReader(out))
	sc.Buffer(make([]byte, 64<<10), 1<<20)
	for sc.Scan() {
		m := reBlackframe.FindSubmatch(sc.Bytes())
		if m == nil {
			continue
		}
		b, err1 := strconv.Atoi(string(m[1]))
		t, err2 := strconv.ParseFloat(string(m[2]), 64)
		if err1 != nil || err2 != nil {
			continue
		}
		frames = append(frames, blackFrame{t, b})
	}
	return frames
}

// blackRun returns the last stretch of credit-like frames lasting at least
// minBlackCredits, where non-credit frames may interrupt it for up to
// maxBlackGap. The last one is taken because credits close an episode
// (though a scene or a preview may follow them).
func blackRun(frames []blackFrame) (Range, bool) {
	var best Range
	found := false
	start, last := -1.0, -1.0
	finish := func(end float64) {
		if start >= 0 && end-start >= minBlackCredits {
			best = Range{Start: start, End: end}
			found = true
		}
	}
	for _, f := range frames {
		if f.black < blackShare {
			continue
		}
		if start >= 0 && f.t-last > maxBlackGap {
			finish(endOf(frames, last))
			start = -1
		}
		if start < 0 {
			start = f.t
		}
		last = f.t
	}
	if start >= 0 {
		finish(endOf(frames, last))
	}
	return best, found
}

// endOf is where the stretch whose last credit-like frame is at t ends: at
// the next checked frame (keyframes can be seconds apart), or at t itself
// when it was the last frame checked.
func endOf(frames []blackFrame, t float64) float64 {
	for _, f := range frames {
		if f.t > t {
			return f.t
		}
	}
	return t
}
