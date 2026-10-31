// Speech levels adapted from Silo (https://github.com/Silo-Server/silo-server),
// internal/mediasample/speech.go and internal/subtitles/subsync/speech.go at
// commit 01e01b0417d2937ed577ce996ce1ef5ae108311e. Copyright (C) the Silo
// authors, licensed under the GNU Affero General Public License v3.0.
// Changes for Kinara: ffmpeg runs here directly (Silo's media sampler and
// transcode nodes aren't part of Kinara), and the window plan takes
// Kinara's stream list. Modifications Copyright (C) the Kinara authors,
// under the same license.

package subsync

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"math"
	"os/exec"
	"strconv"
	"strings"
	"time"
)

// Speech output: the loudness of the speech band, frame by frame. ffmpeg
// band-limits one audio stream to 200-3400 Hz, resamples it to 8 kHz mono
// and writes raw 16-bit samples; they're reduced to one level per 10 ms
// frame as they stream, so a long window never sits in memory as PCM.
const (
	speechSampleRate = 8000
	// SpeechFrameSeconds is the duration of one level.
	SpeechFrameSeconds = 0.01
	speechFrameSamples = speechSampleRate / 100
	// speechFloorDB is the level of digital silence; levels count whole dB above it.
	speechFloorDB = -100
)

// Window plan: long files are sampled, so a big remux isn't read end to end.
const (
	windowSeconds     = 120
	sampledWindows    = 12
	sampleFromSeconds = 45 * 60
	// windowTimeout bounds one window's decode.
	windowTimeout = 5 * time.Minute
)

// SpeechLevels is one window's speech.
type SpeechLevels struct {
	// StartSeconds is the media time of the first level.
	StartSeconds float64 `json:"startSeconds"`
	FrameSeconds float64 `json:"frameSeconds"`
	// Levels holds each frame's speech-band energy in whole dB above
	// -100 dBFS: 0 is silence, 100 full scale.
	Levels []byte `json:"levels"`
}

// Window is a stretch of a file to read.
type Window struct {
	Start, Duration float64
}

// AudioTrack is what the plan needs to know about an audio stream.
type AudioTrack struct {
	// Ordinal is the stream's place among the file's audio streams (ffmpeg's 0:a:N).
	Ordinal  int
	Language string
	Channels int
	Default  bool
}

// Plan is what to decode for one file.
type Plan struct {
	AudioOrdinal  int
	CenterChannel bool
	Windows       []Window
	Runtime       float64
}

// PlanSpeech picks the audio and windows for a file: the first audio track
// in the subtitle's language (else the default track), its centre channel
// when it's surround, and windows spread over the runtime.
func PlanSpeech(runtime float64, tracks []AudioTrack, language string) (Plan, error) {
	if runtime <= 0 {
		return Plan{}, errors.New("the file's length isn't known")
	}
	if len(tracks) == 0 {
		return Plan{}, ErrNoSpeech
	}
	pick := tracks[0]
	for _, t := range tracks {
		if t.Default {
			pick = t
			break
		}
	}
	if language != "" {
		for _, t := range tracks {
			if SameLanguage(t.Language, language) {
				pick = t
				break
			}
		}
	}
	p := Plan{AudioOrdinal: pick.Ordinal, CenterChannel: pick.Channels >= 6, Runtime: runtime}
	if runtime < sampleFromSeconds {
		for start := 0.0; start < runtime; start += windowSeconds {
			p.Windows = append(p.Windows, Window{Start: start, Duration: min(windowSeconds, runtime-start)})
		}
		return p, nil
	}
	for i := range sampledWindows {
		start := (runtime - windowSeconds) * (float64(i) + 0.5) / sampledWindows
		p.Windows = append(p.Windows, Window{Start: math.Round(start), Duration: windowSeconds})
	}
	return p, nil
}

// filter is the audio chain. first_pts=0 makes aresample pad a stream that
// starts after the window start, keeping samples on the media clock.
func (p Plan) filter(center bool) string {
	chain := ""
	if center {
		chain = "pan=mono|c0=FC,"
	}
	return chain + "highpass=f=200,lowpass=f=3400,aresample=8000:async=1:first_pts=0"
}

// Decode reads every window's speech levels with ffmpeg. A surround track
// whose centre channel can't be read is read again as a downmix.
// onWindow is told after each window (for progress).
func Decode(ctx context.Context, ffmpegPath, input string, p Plan, start func(*exec.Cmd) error, onWindow func(done, total int)) ([]SpeechLevels, error) {
	out := make([]SpeechLevels, 0, len(p.Windows))
	center := p.CenterChannel
	for i, w := range p.Windows {
		lv, err := decodeWindow(ctx, ffmpegPath, input, p, w, center, start)
		if err != nil && center {
			center = false
			lv, err = decodeWindow(ctx, ffmpegPath, input, p, w, false, start)
		}
		if err != nil {
			if ctx.Err() != nil {
				return nil, ctx.Err()
			}
			return nil, err
		}
		out = append(out, lv)
		if onWindow != nil {
			onWindow(i+1, len(p.Windows))
		}
	}
	return out, nil
}

func decodeWindow(ctx context.Context, ffmpegPath, input string, p Plan, w Window, center bool, start func(*exec.Cmd) error) (SpeechLevels, error) {
	wctx, cancel := context.WithTimeout(ctx, windowTimeout)
	defer cancel()
	sec := func(f float64) string { return strconv.FormatFloat(f, 'f', 3, 64) }
	args := []string{"-nostdin", "-hide_banner", "-loglevel", "error",
		"-ss", sec(w.Start), "-i", input, "-t", sec(w.Duration),
		"-map", "0:a:" + strconv.Itoa(p.AudioOrdinal), "-vn", "-sn", "-dn",
		"-af", p.filter(center), "-ac", "1",
		"-f", "s16le", "-acodec", "pcm_s16le", "-"}
	cmd := exec.CommandContext(wctx, ffmpegPath, args...)
	sw := newSpeechWriter(w.Duration)
	cmd.Stdout = sw
	var stderr strings.Builder
	cmd.Stderr = &limited{b: &stderr, n: 2048}
	if start == nil {
		start = func(c *exec.Cmd) error { return c.Start() }
	}
	if err := start(cmd); err != nil {
		return SpeechLevels{}, err
	}
	if err := cmd.Wait(); err != nil {
		msg := strings.TrimSpace(stderr.String())
		if msg == "" {
			msg = err.Error()
		}
		return SpeechLevels{}, fmt.Errorf("ffmpeg: %s", msg)
	}
	return sw.result(w.Start), nil
}

// limited keeps the first n bytes written.
type limited struct {
	b *strings.Builder
	n int
}

func (l *limited) Write(p []byte) (int, error) {
	if room := l.n - l.b.Len(); room > 0 {
		l.b.Write(p[:min(room, len(p))])
	}
	return len(p), nil
}

// speechWriter receives raw s16le mono samples and reduces them to frame levels.
type speechWriter struct {
	levels  []byte
	partial []byte
	sum     float64
	count   int
}

func newSpeechWriter(durationSeconds float64) *speechWriter {
	frames := int(math.Ceil(durationSeconds/SpeechFrameSeconds)) + 1
	return &speechWriter{levels: make([]byte, 0, max(frames, 0))}
}

func (w *speechWriter) Write(p []byte) (int, error) {
	n := len(p)
	if len(w.partial) == 1 && len(p) > 0 {
		w.add(int16(binary.LittleEndian.Uint16([]byte{w.partial[0], p[0]})))
		w.partial = w.partial[:0]
		p = p[1:]
	}
	for len(p) >= 2 {
		w.add(int16(binary.LittleEndian.Uint16(p)))
		p = p[2:]
	}
	if len(p) == 1 {
		w.partial = append(w.partial[:0], p[0])
	}
	return n, nil
}

func (w *speechWriter) add(sample int16) {
	v := float64(sample)
	w.sum += v * v
	w.count++
	if w.count == speechFrameSamples {
		w.flush()
	}
}

func (w *speechWriter) flush() {
	if w.count == 0 {
		return
	}
	w.levels = append(w.levels, speechLevel(w.sum/float64(w.count)))
	w.sum, w.count = 0, 0
}

func (w *speechWriter) result(start float64) SpeechLevels {
	w.flush()
	return SpeechLevels{StartSeconds: start, FrameSeconds: SpeechFrameSeconds, Levels: w.levels}
}

// speechLevel converts a frame's mean square sample to its level.
func speechLevel(meanSquare float64) byte {
	const fullScale = 32768.0 * 32768.0
	if meanSquare <= 0 {
		return 0
	}
	db := 10*math.Log10(meanSquare/fullScale) - speechFloorDB
	return byte(math.Round(min(max(db, 0), -speechFloorDB)))
}

// SameLanguage compares ISO 639 codes loosely: "en", "eng" and "English"
// match, as do the bibliographic and terminology forms ("fre", "fra").
func SameLanguage(a, b string) bool {
	return langKey(a) != "" && langKey(a) == langKey(b)
}

var langAliases = map[string]string{
	"en": "eng", "english": "eng", "fr": "fre", "fra": "fre", "french": "fre", "de": "ger", "deu": "ger", "german": "ger",
	"es": "spa", "spanish": "spa", "it": "ita", "italian": "ita", "pt": "por", "portuguese": "por", "ja": "jpn", "japanese": "jpn",
	"ko": "kor", "korean": "kor", "zh": "chi", "zho": "chi", "chinese": "chi", "hi": "hin", "hindi": "hin", "ru": "rus", "russian": "rus",
	"ar": "ara", "arabic": "ara", "nl": "dut", "nld": "dut", "dutch": "dut", "sv": "swe", "swedish": "swe", "pl": "pol", "polish": "pol",
	"tr": "tur", "turkish": "tur", "ta": "tam", "tamil": "tam", "te": "tel", "telugu": "tel", "bn": "ben", "bengali": "ben",
	"mr": "mar", "marathi": "mar", "cs": "cze", "ces": "cze", "czech": "cze", "el": "gre", "ell": "gre", "greek": "gre",
	"he": "heb", "hebrew": "heb", "da": "dan", "danish": "dan", "fi": "fin", "finnish": "fin", "no": "nor", "nb": "nor", "nob": "nor", "norwegian": "nor",
	"ro": "rum", "ron": "rum", "romanian": "rum", "hu": "hun", "hungarian": "hun", "th": "tha", "thai": "tha", "vi": "vie", "vietnamese": "vie",
	"id": "ind", "indonesian": "ind", "ms": "may", "msa": "may", "uk": "ukr", "ukrainian": "ukr",
}

func langKey(code string) string {
	c := strings.ToLower(strings.TrimSpace(code))
	if i := strings.IndexAny(c, "-_"); i > 0 {
		c = c[:i]
	}
	if a, ok := langAliases[c]; ok {
		return a
	}
	return c
}
