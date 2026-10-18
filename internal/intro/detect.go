package intro

// Detect is the high-level helper: given fingerprints of all episodes of one
// season in episode order, it returns an intro Range per episode index. A
// missing key means no intro was found for that episode.
//
// Each episode is compared with the next episode, the previous episode and
// the one after next (pair results are computed once and shared by both
// episodes). When several comparisons find an intro, the candidate that the
// most other candidates agree with (overlapping by at least half) wins, with
// ties going to the earliest start. Agreement favours the theme song over a
// recap or a cue that two episodes happen to share.
func Detect(prints [][]uint32) map[int]Range {
	return DetectIn(prints, IntroBounds)
}

// DetectIn is Detect with other length bounds (CreditsBounds for the ends
// of episodes).
func DetectIn(prints [][]uint32, bounds Bounds) map[int]Range {
	type pairResult struct {
		ra, rb Range
		ok     bool
	}
	cache := make(map[[2]int]pairResult)
	compare := func(i, j int) (Range, bool) {
		lo, hi := min(i, j), max(i, j)
		key := [2]int{lo, hi}
		res, seen := cache[key]
		if !seen {
			res.ra, res.rb, res.ok = CompareIn(prints[lo], prints[hi], bounds)
			cache[key] = res
		}
		if !res.ok {
			return Range{}, false
		}
		if i == lo {
			return res.ra, true
		}
		return res.rb, true
	}

	out := make(map[int]Range)
	for i := range prints {
		var cands []Range
		for _, j := range []int{i + 1, i - 1, i + 2} {
			if j < 0 || j >= len(prints) {
				continue
			}
			if r, ok := compare(i, j); ok {
				cands = append(cands, r)
			}
		}
		if r, ok := pickConsistent(cands); ok {
			out[i] = r
		}
	}
	return out
}

// pickConsistent chooses the candidate that agrees with the most others,
// breaking ties by the earliest start.
func pickConsistent(cands []Range) (Range, bool) {
	if len(cands) == 0 {
		return Range{}, false
	}
	best, bestScore := 0, -1
	for x, rx := range cands {
		score := 0
		for y, ry := range cands {
			if x != y && agree(rx, ry) {
				score++
			}
		}
		if score > bestScore || (score == bestScore && rx.Start < cands[best].Start) {
			best, bestScore = x, score
		}
	}
	return cands[best], true
}

// agree reports whether two ranges overlap by at least half of the shorter.
func agree(a, b Range) bool {
	overlap := min(a.End, b.End) - max(a.Start, b.Start)
	shorter := min(a.Duration(), b.Duration())
	return shorter > 0 && overlap >= shorter/2
}
