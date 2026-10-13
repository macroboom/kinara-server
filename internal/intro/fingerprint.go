// Package intro detects TV show intros (the theme song shared by episodes of
// a season) by comparing audio fingerprints of neighbouring episodes.
//
// The fingerprint is a Philips (Haitsma and Kalker) style robust hash: audio
// is cut into overlapping Hann-windowed frames, the spectral energy of each
// frame is measured in log-spaced bands, and every frame is reduced to a
// 32-bit word whose bits record the sign of the energy difference between
// adjacent bands, differentiated over time. Two recordings of the same music
// produce hash sequences with a low bit error rate even after re-encoding,
// gain changes and mild noise.
package intro

import (
	"math"
	"math/bits"
)

// SampleRate is the rate audio must be decoded at (mono, signed 16-bit
// little-endian). Callers pass it to ffmpeg as -ar.
const SampleRate = 5512

const (
	// frameSize is the FFT length in samples (about 0.37 s at SampleRate).
	frameSize = 2048
	// hopSize is how far consecutive frames advance (about 0.093 s).
	hopSize = 512
	// numBands is the number of energy bands; adjacent pairs yield 32 bits.
	numBands = 33
	// minFreq and maxFreq bound the analysed spectrum in Hz.
	minFreq = 300.0
	maxFreq = 2000.0
	// silenceRMS is the frame RMS (in int16 units) below which a frame is
	// treated as silence, roughly -70 dBFS.
	silenceRMS = 10.0
)

// silentHash marks a frame that carries no usable audio. Real hashes that
// happen to be zero are remapped so that zero is reserved for silence.
const silentHash uint32 = 0

// FrameSeconds returns how many seconds each fingerprint frame advances (the
// hop between consecutive hashes).
func FrameSeconds() float64 {
	return float64(hopSize) / SampleRate
}

// frameDuration is the length of the audio window behind one hash, in seconds.
func frameDuration() float64 {
	return float64(frameSize) / SampleRate
}

// hashTime returns the start time in seconds of the window behind hash k.
// Hash k is derived from frames k and k+1, and describes frame k+1.
func hashTime(k int) float64 {
	return float64(k+1) * FrameSeconds()
}

// Fingerprint turns mono PCM samples (at SampleRate) into a sequence of frame
// hashes, one per FrameSeconds. A hash of zero marks a silent frame; such
// frames never count as matches in Compare. Fewer than two frames of audio
// yield nil.
func Fingerprint(samples []int16) []uint32 {
	if len(samples) < frameSize+hopSize {
		return nil
	}
	nFrames := (len(samples)-frameSize)/hopSize + 1

	window := hannWindow(frameSize)
	fft := newFFT(frameSize)
	edges := bandEdges()

	re := make([]float64, frameSize)
	im := make([]float64, frameSize)
	prev := make([]float64, numBands)
	cur := make([]float64, numBands)
	out := make([]uint32, 0, nFrames-1)

	for n := 0; n < nFrames; n++ {
		frame := samples[n*hopSize : n*hopSize+frameSize]
		var sumSq float64
		for i, s := range frame {
			v := float64(s)
			sumSq += v * v
			re[i] = v * window[i]
			im[i] = 0
		}
		silent := math.Sqrt(sumSq/frameSize) < silenceRMS

		fft.transform(re, im)
		for m := 0; m < numBands; m++ {
			var e float64
			for k := edges[m]; k < edges[m+1]; k++ {
				e += re[k]*re[k] + im[k]*im[k]
			}
			cur[m] = e
		}

		if n > 0 {
			var h uint32
			if silent {
				h = silentHash
			} else {
				for m := 0; m < numBands-1; m++ {
					d := (cur[m] - cur[m+1]) - (prev[m] - prev[m+1])
					if d > 0 {
						h |= 1 << uint(m)
					}
				}
				if h == silentHash {
					h = 1
				}
			}
			out = append(out, h)
		}
		prev, cur = cur, prev
	}
	return out
}

// bandEdges returns numBands+1 FFT bin indices splitting [minFreq, maxFreq]
// into log-spaced bands. Every band spans at least one bin.
func bandEdges() []int {
	edges := make([]int, numBands+1)
	ratio := maxFreq / minFreq
	binHz := float64(SampleRate) / frameSize
	for k := 0; k <= numBands; k++ {
		f := minFreq * math.Pow(ratio, float64(k)/numBands)
		edges[k] = int(math.Round(f / binHz))
		if k > 0 && edges[k] <= edges[k-1] {
			edges[k] = edges[k-1] + 1
		}
	}
	return edges
}

func hannWindow(n int) []float64 {
	w := make([]float64, n)
	for i := range w {
		w[i] = 0.5 - 0.5*math.Cos(2*math.Pi*float64(i)/float64(n))
	}
	return w
}

// fftPlan holds precomputed tables for an in-place iterative radix-2 FFT.
type fftPlan struct {
	n       int
	rev     []int
	cos     []float64
	sin     []float64
	log2len uint
}

func newFFT(n int) *fftPlan {
	if n <= 0 || n&(n-1) != 0 {
		panic("intro: FFT size must be a power of two")
	}
	lg := uint(bits.TrailingZeros(uint(n)))
	p := &fftPlan{n: n, log2len: lg, rev: make([]int, n), cos: make([]float64, n/2), sin: make([]float64, n/2)}
	for i := 0; i < n; i++ {
		p.rev[i] = int(bits.Reverse(uint(i)) >> (bits.UintSize - lg))
	}
	for i := 0; i < n/2; i++ {
		a := -2 * math.Pi * float64(i) / float64(n)
		p.cos[i] = math.Cos(a)
		p.sin[i] = math.Sin(a)
	}
	return p
}

// transform computes the forward DFT of (re, im) in place.
func (p *fftPlan) transform(re, im []float64) {
	n := p.n
	for i := 0; i < n; i++ {
		if j := p.rev[i]; j > i {
			re[i], re[j] = re[j], re[i]
			im[i], im[j] = im[j], im[i]
		}
	}
	for size := 2; size <= n; size <<= 1 {
		half := size >> 1
		step := n / size
		for start := 0; start < n; start += size {
			for k := 0; k < half; k++ {
				wr, wi := p.cos[k*step], p.sin[k*step]
				a, b := start+k, start+k+half
				tr := wr*re[b] - wi*im[b]
				ti := wr*im[b] + wi*re[b]
				re[b] = re[a] - tr
				im[b] = im[a] - ti
				re[a] += tr
				im[a] += ti
			}
		}
	}
}
