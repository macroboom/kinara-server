package intro

import (
	"math"
	"math/bits"
	"sort"
)

// MinIntro and MaxIntro bound the length, in seconds, of a shared segment
// that Compare accepts as an intro.
const (
	MinIntro = 12.0
	MaxIntro = 150.0
)

// Bounds is the accepted length, in seconds, of a shared segment.
type Bounds struct{ Min, Max float64 }

var (
	// IntroBounds is what Compare and Detect accept: opening titles.
	IntroBounds = Bounds{MinIntro, MaxIntro}
	// CreditsBounds is for end credits, compared over the end of each
	// episode: longer than a theme song can be, but never most of the end.
	CreditsBounds = Bounds{15, 330}
)

const (
	// maxBitErrors is the largest Hamming distance (out of 32 bits),
	// averaged over smoothFrames aligned frames, at which the frames are
	// considered the same audio. Unrelated audio averages about 16; the
	// same audio misaligned by up to half a hop averages about 7.
	maxBitErrors = 11
	// smoothFrames is the width (odd, about one second) of the centred
	// window over which per-frame Hamming distances are averaged. Single
	// frames are too noisy: unrelated audio often lands within 10 bits.
	smoothFrames = 11
	// maxGapSeconds is how long a run of matching frames may be interrupted
	// by non-matching frames and still count as one segment.
	maxGapSeconds = 1.5
	// candidateShifts is how many of the best voted alignments are verified.
	candidateShifts = 10
	// minVotes is the least number of hash hits an alignment needs to be
	// verified at all.
	minVotes = 3
	// maxBucket skips hash values that occur very often in b (stationary
	// tones, hum) since they vote for every shift and carry no information.
	maxBucket = 64
)

// Range is a span in seconds.
type Range struct{ Start, End float64 }

// Duration returns the length of the range in seconds.
func (r Range) Duration() float64 { return r.End - r.Start }

// Compare finds the longest shared segment between two episodes'
// fingerprints. It returns the segment's range in a and in b, and ok=false
// if none qualifies. A qualifying segment lasts between MinIntro and
// MaxIntro seconds.
//
// Candidate alignments are found by letting every hash of a vote for the
// offsets at which the same hash (or one differing in a single bit) occurs
// in b. The best alignments are then verified frame by frame using the
// Hamming distance between aligned hashes, averaged over about one second,
// and the longest run of matching frames (allowing gaps of up to 1.5 s) wins.
// Silent frames never match.
func Compare(a, b []uint32) (ra, rb Range, ok bool) {
	return CompareIn(a, b, IntroBounds)
}

// CompareIn is Compare with other length bounds.
func CompareIn(a, b []uint32, bounds Bounds) (ra, rb Range, ok bool) {
	if len(a) == 0 || len(b) == 0 {
		return Range{}, Range{}, false
	}

	index := make(map[uint32][]int32, len(b))
	for j, h := range b {
		if h == silentHash {
			continue
		}
		index[h] = append(index[h], int32(j))
	}

	// votes is keyed by shift + len(b) so it can be a dense slice.
	votes := make([]int32, len(a)+len(b))
	vote := func(i int, h uint32) {
		pos := index[h]
		if len(pos) == 0 || len(pos) > maxBucket {
			return
		}
		for _, j := range pos {
			votes[i-int(j)+len(b)]++
		}
	}
	for i, h := range a {
		if h == silentHash {
			continue
		}
		vote(i, h)
		for bit := 0; bit < 32; bit++ {
			vote(i, h^(1<<uint(bit)))
		}
	}

	shifts := topShifts(votes, len(b))
	gap := int(math.Round(maxGapSeconds / FrameSeconds()))

	span := func(i0, i1 int) Range {
		return Range{Start: hashTime(i0), End: hashTime(i1) + frameDuration()}
	}
	qualifies := func(i0, i1 int) bool {
		d := span(i0, i1).Duration()
		return d >= bounds.Min && d <= bounds.Max
	}

	best := -1.0
	for _, s := range shifts {
		i0, i1, found := longestRun(a, b, s, gap, qualifies)
		if !found {
			continue
		}
		r := span(i0, i1)
		d := r.Duration()
		if d > best || (d == best && r.Start < ra.Start) {
			best = d
			ra = r
			rb = span(i0-s, i1-s)
			ok = true
		}
	}
	return ra, rb, ok
}

// topShifts returns up to candidateShifts shifts (i - j) with the most votes,
// highest first and ties broken by the smaller shift.
func topShifts(votes []int32, offset int) []int {
	type cand struct {
		shift int
		votes int32
	}
	var cs []cand
	for k, v := range votes {
		if v >= minVotes {
			cs = append(cs, cand{k - offset, v})
		}
	}
	sort.Slice(cs, func(x, y int) bool {
		if cs[x].votes != cs[y].votes {
			return cs[x].votes > cs[y].votes
		}
		return cs[x].shift < cs[y].shift
	})
	if len(cs) > candidateShifts {
		cs = cs[:candidateShifts]
	}
	out := make([]int, len(cs))
	for i, c := range cs {
		out[i] = c.shift
	}
	return out
}

// longestRun aligns a[i] with b[i-shift] and returns the first and last
// index in a of the longest stretch of matching frames accepted by accept,
// where up to gap consecutive non-matching frames may occur inside a stretch.
// A frame matches when the mean Hamming distance over a centred window of
// smoothFrames aligned frames is at most maxBitErrors. Silent frames never
// match and count as 32 bit errors in their neighbours' windows.
func longestRun(a, b []uint32, shift, gap int, accept func(start, end int) bool) (start, end int, found bool) {
	lo := max(0, shift)
	hi := min(len(a), len(b)+shift)
	if hi <= lo {
		return 0, 0, false
	}

	// prefix[k] is the total distance of the first k aligned frames.
	n := hi - lo
	prefix := make([]int32, n+1)
	silent := make([]bool, n)
	for k := 0; k < n; k++ {
		ha, hb := a[lo+k], b[lo+k-shift]
		d := 32
		if ha != silentHash && hb != silentHash {
			d = bits.OnesCount32(ha ^ hb)
		} else {
			silent[k] = true
		}
		prefix[k+1] = prefix[k] + int32(d)
	}
	half := smoothFrames / 2
	matches := func(k int) bool {
		if silent[k] {
			return false
		}
		w0, w1 := max(0, k-half), min(n, k+half+1)
		return int(prefix[w1]-prefix[w0]) <= maxBitErrors*(w1-w0)
	}

	runStart, lastMatch := -1, -1
	bestLen := 0
	finish := func() {
		if runStart < 0 {
			return
		}
		if l := lastMatch - runStart + 1; l > bestLen && accept(lo+runStart, lo+lastMatch) {
			bestLen = l
			start, end, found = lo+runStart, lo+lastMatch, true
		}
	}
	for k := 0; k < n; k++ {
		if !matches(k) {
			continue
		}
		if runStart >= 0 && k-lastMatch-1 > gap {
			finish()
			runStart = -1
		}
		if runStart < 0 {
			runStart = k
		}
		lastMatch = k
	}
	finish()
	return start, end, found
}
