package intro

import (
	"context"
	"encoding/binary"
	"math"
	"math/rand"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"
)

// tolerance is how far, in seconds, a detected boundary may be from the truth.
const tolerance = 1.5

// theme synthesises secs seconds of "music": a melody of notes with
// harmonics, a new pitch every ~0.3 s, each note with an attack and decay
// envelope, over a quieter sustained bass note that changes every bar.
func theme(rng *rand.Rand, secs float64) []float64 {
	n := int(secs * SampleRate)
	out := make([]float64, n)
	scale := []float64{0, 2, 4, 5, 7, 9, 11, 12, 14, 16}
	pos := 0
	for pos < n {
		noteLen := int((0.22 + 0.16*rng.Float64()) * SampleRate)
		semi := scale[rng.Intn(len(scale))]
		f0 := 330 * math.Pow(2, semi/12)
		bass := 165 * math.Pow(2, scale[rng.Intn(5)]/12)
		for k := 0; k < noteLen && pos+k < n; k++ {
			t := float64(k) / SampleRate
			env := math.Min(1, t/0.02) * math.Exp(-3*t)
			v := 0.0
			for h, amp := range []float64{1, 0.5, 0.3, 0.15} {
				v += amp * math.Sin(2*math.Pi*f0*float64(h+1)*t)
			}
			v = env*v + 0.3*math.Sin(2*math.Pi*bass*2*t)
			out[pos+k] = v
		}
		pos += noteLen
	}
	return normalize(out, 0.25)
}

// speech synthesises secs seconds of speech-like filler: white noise through
// a resonator whose centre frequency and loudness change every syllable,
// with short pauses between some syllables.
func speech(rng *rand.Rand, secs float64) []float64 {
	n := int(secs * SampleRate)
	out := make([]float64, n)
	var y1, y2 float64
	pos := 0
	for pos < n {
		sylLen := int((0.12 + 0.3*rng.Float64()) * SampleRate)
		fc := 300 + 1700*rng.Float64()
		amp := 0.3 + 0.7*rng.Float64()
		if rng.Float64() < 0.15 {
			amp = 0.02
		}
		r := 0.97
		c1 := 2 * r * math.Cos(2*math.Pi*fc/SampleRate)
		c2 := -r * r
		for k := 0; k < sylLen && pos+k < n; k++ {
			x := rng.NormFloat64()
			y := x + c1*y1 + c2*y2
			y2, y1 = y1, y
			env := math.Sin(math.Pi * float64(k) / float64(sylLen))
			out[pos+k] = amp * env * y
		}
		pos += sylLen
	}
	return normalize(out, 0.2)
}

func normalize(x []float64, targetRMS float64) []float64 {
	var ss float64
	for _, v := range x {
		ss += v * v
	}
	rms := math.Sqrt(ss / float64(len(x)))
	if rms == 0 {
		return x
	}
	for i := range x {
		x[i] *= targetRMS / rms
	}
	return x
}

// episode concatenates parts, applies gain, adds white noise at snrDB below
// the signal RMS (skipping digitally silent stretches), and converts to int16.
func episode(rng *rand.Rand, gain, snrDB float64, parts ...[]float64) []int16 {
	var sig []float64
	for _, p := range parts {
		sig = append(sig, p...)
	}
	var ss float64
	for _, v := range sig {
		ss += v * v
	}
	noiseStd := math.Sqrt(ss/float64(len(sig))) / math.Pow(10, snrDB/20)
	out := make([]int16, len(sig))
	for i, v := range sig {
		if v != 0 {
			v += noiseStd * rng.NormFloat64()
		}
		s := math.Round(v * gain * 20000)
		out[i] = int16(math.Max(math.MinInt16, math.Min(math.MaxInt16, s)))
	}
	return out
}

func silence(secs float64) []float64 { return make([]float64, int(secs*SampleRate)) }

func near(got, want float64) bool { return math.Abs(got-want) <= tolerance }

func TestFingerprintBasics(t *testing.T) {
	if got := Fingerprint(make([]int16, 100)); got != nil {
		t.Fatalf("short input: got %d hashes, want nil", len(got))
	}
	fp := Fingerprint(make([]int16, 10*SampleRate))
	for i, h := range fp {
		if h != silentHash {
			t.Fatalf("silent input: hash %d = %#x, want silent", i, h)
		}
	}
	want := (10*SampleRate-frameSize)/hopSize + 1 - 1
	if len(fp) != want {
		t.Fatalf("len = %d, want %d", len(fp), want)
	}
}

func TestFFTMatchesDFT(t *testing.T) {
	const n = 64
	rng := rand.New(rand.NewSource(7))
	re, im := make([]float64, n), make([]float64, n)
	in := make([]float64, n)
	for i := range in {
		in[i] = rng.NormFloat64()
		re[i] = in[i]
	}
	newFFT(n).transform(re, im)
	for k := 0; k < n; k++ {
		var wr, wi float64
		for j, x := range in {
			a := -2 * math.Pi * float64(j*k) / n
			wr += x * math.Cos(a)
			wi += x * math.Sin(a)
		}
		if math.Abs(wr-re[k]) > 1e-9 || math.Abs(wi-im[k]) > 1e-9 {
			t.Fatalf("bin %d: got (%g,%g), want (%g,%g)", k, re[k], im[k], wr, wi)
		}
	}
}

func TestCompareFindsSharedTheme(t *testing.T) {
	rng := rand.New(rand.NewSource(1))
	th := theme(rng, 40)
	a := episode(rand.New(rand.NewSource(11)), 1.0, 25,
		speech(rand.New(rand.NewSource(2)), 70), th, speech(rand.New(rand.NewSource(3)), 200))
	b := episode(rand.New(rand.NewSource(12)), 0.8, 25,
		speech(rand.New(rand.NewSource(4)), 20), th, speech(rand.New(rand.NewSource(5)), 200))

	ra, rb, ok := Compare(Fingerprint(a), Fingerprint(b))
	if !ok {
		t.Fatal("no intro found")
	}
	t.Logf("ra=%+v rb=%+v", ra, rb)
	if !near(ra.Start, 70) || !near(ra.End, 110) {
		t.Errorf("ra = %+v, want about [70,110]", ra)
	}
	if !near(rb.Start, 20) || !near(rb.End, 60) {
		t.Errorf("rb = %+v, want about [20,60]", rb)
	}

	// Compare must be symmetric up to swapping the results.
	rb2, ra2, ok := Compare(Fingerprint(b), Fingerprint(a))
	if !ok || !near(ra2.Start, ra.Start) || !near(rb2.Start, rb.Start) {
		t.Errorf("swapped: ok=%v ra=%+v rb=%+v", ok, ra2, rb2)
	}
}

func TestCompareNoSharedContent(t *testing.T) {
	a := episode(rand.New(rand.NewSource(21)), 1.0, 25,
		speech(rand.New(rand.NewSource(22)), 60), theme(rand.New(rand.NewSource(23)), 40), speech(rand.New(rand.NewSource(24)), 100))
	b := episode(rand.New(rand.NewSource(31)), 0.8, 25,
		speech(rand.New(rand.NewSource(32)), 20), theme(rand.New(rand.NewSource(33)), 40), speech(rand.New(rand.NewSource(34)), 140))
	if ra, rb, ok := Compare(Fingerprint(a), Fingerprint(b)); ok {
		t.Fatalf("unexpected match ra=%+v rb=%+v", ra, rb)
	}
}

func TestCompareIgnoresSilence(t *testing.T) {
	a := episode(rand.New(rand.NewSource(41)), 1.0, 25,
		speech(rand.New(rand.NewSource(42)), 50), silence(30), speech(rand.New(rand.NewSource(43)), 100))
	b := episode(rand.New(rand.NewSource(51)), 0.8, 25,
		speech(rand.New(rand.NewSource(52)), 50), silence(30), speech(rand.New(rand.NewSource(53)), 100))
	if ra, rb, ok := Compare(Fingerprint(a), Fingerprint(b)); ok {
		t.Fatalf("silence matched: ra=%+v rb=%+v", ra, rb)
	}
}

func TestCompareRejectsTooShort(t *testing.T) {
	th := theme(rand.New(rand.NewSource(61)), 6)
	a := episode(rand.New(rand.NewSource(62)), 1.0, 25,
		speech(rand.New(rand.NewSource(63)), 30), th, speech(rand.New(rand.NewSource(64)), 60))
	b := episode(rand.New(rand.NewSource(65)), 0.8, 25,
		speech(rand.New(rand.NewSource(66)), 10), th, speech(rand.New(rand.NewSource(67)), 60))
	if ra, rb, ok := Compare(Fingerprint(a), Fingerprint(b)); ok {
		t.Fatalf("6 s jingle accepted as intro: ra=%+v rb=%+v", ra, rb)
	}
}

func TestDetect(t *testing.T) {
	th := theme(rand.New(rand.NewSource(100)), 40)
	offsets := []float64{70, 20, 45}
	var prints [][]uint32
	for i, off := range offsets {
		seed := int64(200 + 10*i)
		ep := episode(rand.New(rand.NewSource(seed)), 1.0-0.1*float64(i), 25,
			speech(rand.New(rand.NewSource(seed+1)), off), th, speech(rand.New(rand.NewSource(seed+2)), 120))
		prints = append(prints, Fingerprint(ep))
	}
	got := Detect(prints)
	for i, off := range offsets {
		r, ok := got[i]
		if !ok {
			t.Errorf("episode %d: no intro", i)
			continue
		}
		t.Logf("episode %d: %+v", i, r)
		if !near(r.Start, off) || !near(r.End, off+40) {
			t.Errorf("episode %d: got %+v, want about [%g,%g]", i, r, off, off+40)
		}
	}
}

func TestDetectEmpty(t *testing.T) {
	if got := Detect(nil); len(got) != 0 {
		t.Fatalf("got %v", got)
	}
	if got := Detect([][]uint32{nil}); len(got) != 0 {
		t.Fatalf("got %v", got)
	}
}

// twelveMinutePair builds two 12 minute episodes sharing a 45 s theme.
func twelveMinutePair() (a, b []int16) {
	th := theme(rand.New(rand.NewSource(300)), 45)
	a = episode(rand.New(rand.NewSource(301)), 1.0, 25,
		speech(rand.New(rand.NewSource(302)), 90), th, speech(rand.New(rand.NewSource(303)), 720-90-45))
	b = episode(rand.New(rand.NewSource(311)), 0.8, 25,
		speech(rand.New(rand.NewSource(312)), 30), th, speech(rand.New(rand.NewSource(313)), 720-30-45))
	return a, b
}

func TestTwelveMinuteTiming(t *testing.T) {
	if testing.Short() {
		t.Skip("short mode")
	}
	a, b := twelveMinutePair()

	start := time.Now()
	fa, fb := Fingerprint(a), Fingerprint(b)
	fpDur := time.Since(start)
	cmpStart := time.Now()
	ra, rb, ok := Compare(fa, fb)
	cmpDur := time.Since(cmpStart)
	total := time.Since(start)

	t.Logf("frames %d/%d, fingerprint both %v, compare %v, total %v", len(fa), len(fb), fpDur, cmpDur, total)
	if !ok || !near(ra.Start, 90) || !near(rb.Start, 30) || !near(ra.End, 135) {
		t.Errorf("ok=%v ra=%+v rb=%+v, want about [90,135] and [30,75]", ok, ra, rb)
	}
	if total > 3*time.Second {
		t.Errorf("fingerprint + compare took %v, want < 3s", total)
	}
}

func TestDecodeMissingBinary(t *testing.T) {
	_, err := Decode(context.Background(), "/nonexistent/ffmpeg-binary", "x.mkv", time.Second)
	if err == nil {
		t.Fatal("expected error for missing ffmpeg")
	}
}

func TestDecodeWithFFmpeg(t *testing.T) {
	if _, err := exec.LookPath("ffmpeg"); err != nil {
		t.Skip("ffmpeg not installed")
	}
	ctx := context.Background()
	if _, err := Decode(ctx, "", "/nonexistent/file.mkv", time.Second); err == nil {
		t.Fatal("expected error for missing input file")
	}

	// Round-trip two synthetic episodes through 44.1 kHz stereo WAV files
	// (with a leading dash in one name) and detect the intro from the
	// decoded audio.
	th := theme(rand.New(rand.NewSource(400)), 30)
	dir := t.TempDir()
	pa := filepath.Join(dir, "-ep1 (a).wav")
	pb := filepath.Join(dir, "ep2.wav")
	writeWAV(t, pa, episode(rand.New(rand.NewSource(401)), 1.0, 25,
		speech(rand.New(rand.NewSource(402)), 25), th, speech(rand.New(rand.NewSource(403)), 40)))
	writeWAV(t, pb, episode(rand.New(rand.NewSource(404)), 0.8, 25,
		speech(rand.New(rand.NewSource(405)), 10), th, speech(rand.New(rand.NewSource(406)), 40)))

	sa, err := Decode(ctx, "", pa, 60*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if want := 60 * SampleRate; math.Abs(float64(len(sa)-want)) > SampleRate/10 {
		t.Errorf("decoded %d samples, want about %d (limit 60 s)", len(sa), want)
	}
	sb, err := Decode(ctx, "", pb, 0)
	if err != nil {
		t.Fatal(err)
	}
	ra, rb, ok := Compare(Fingerprint(sa), Fingerprint(sb))
	t.Logf("ra=%+v rb=%+v", ra, rb)
	if !ok || !near(ra.Start, 25) || !near(ra.End, 55) || !near(rb.Start, 10) || !near(rb.End, 40) {
		t.Errorf("ok=%v ra=%+v rb=%+v, want about [25,55] and [10,40]", ok, ra, rb)
	}
}

// writeWAV stores mono samples (at SampleRate) as a 44.1 kHz stereo 16-bit
// WAV file, using linear interpolation, so Decode has to resample and
// downmix.
func writeWAV(t *testing.T, path string, mono []int16) {
	t.Helper()
	const rate, channels = 44100, 2
	frames := len(mono) * rate / SampleRate
	data := make([]byte, 0, frames*channels*2)
	for i := 0; i < frames; i++ {
		x := float64(i) * SampleRate / rate
		k := int(x)
		v := float64(mono[k])
		if k+1 < len(mono) {
			v += (x - float64(k)) * (float64(mono[k+1]) - v)
		}
		s := uint16(int16(math.Round(v)))
		for range channels {
			data = binary.LittleEndian.AppendUint16(data, s)
		}
	}
	var h []byte
	h = append(h, "RIFF"...)
	h = binary.LittleEndian.AppendUint32(h, uint32(36+len(data)))
	h = append(h, "WAVEfmt "...)
	h = binary.LittleEndian.AppendUint32(h, 16)
	h = binary.LittleEndian.AppendUint16(h, 1)
	h = binary.LittleEndian.AppendUint16(h, channels)
	h = binary.LittleEndian.AppendUint32(h, rate)
	h = binary.LittleEndian.AppendUint32(h, rate*channels*2)
	h = binary.LittleEndian.AppendUint16(h, channels*2)
	h = binary.LittleEndian.AppendUint16(h, 16)
	h = append(h, "data"...)
	h = binary.LittleEndian.AppendUint32(h, uint32(len(data)))
	if err := os.WriteFile(path, append(h, data...), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestCapWriter(t *testing.T) {
	w := &capWriter{max: 5}
	for _, s := range []string{"abc", "defg", "hij"} {
		if n, err := w.Write([]byte(s)); n != len(s) || err != nil {
			t.Fatalf("Write(%q) = %d, %v", s, n, err)
		}
	}
	if string(w.buf) != "abcde" {
		t.Fatalf("buf = %q", w.buf)
	}
}

func BenchmarkFingerprint12Min(b *testing.B) {
	a, _ := twelveMinutePair()
	b.ResetTimer()
	for range b.N {
		Fingerprint(a)
	}
}

func BenchmarkCompare12Min(b *testing.B) {
	x, y := twelveMinutePair()
	fx, fy := Fingerprint(x), Fingerprint(y)
	b.ResetTimer()
	for range b.N {
		Compare(fx, fy)
	}
}
