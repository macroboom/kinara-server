package intro

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"os/exec"
	"strconv"
	"strings"
	"time"

	"kinora/internal/ffmpeg"
)

const (
	// defaultDecodeLimit is used when Decode is given a non-positive limit.
	defaultDecodeLimit = 12 * time.Minute
	// maxDecodeLimit caps how much audio Decode will ever read.
	maxDecodeLimit = 30 * time.Minute
	// stderrCap bounds how much ffmpeg diagnostic output is kept.
	stderrCap = 4 << 10
)

// Decode runs ffmpeg to read the first limit of a file's audio (first audio
// stream, downmixed to mono, resampled to SampleRate, s16le on stdout) and
// returns the samples. ffmpegBin "" means "ffmpeg" from PATH. A non-positive
// limit means 12 minutes, and limits above 30 minutes are clamped.
//
// ffmpeg is run without a shell, with path passed as a single argument after
// -i. Output beyond what limit implies (plus a little slack) is discarded.
func Decode(ctx context.Context, ffmpegBin, path string, limit time.Duration) ([]int16, error) {
	return DecodeFrom(ctx, ffmpegBin, path, 0, limit)
}

// DecodeFrom is Decode starting at from into the file (the end of an
// episode, for credits). Sample 0 is the audio at from: the seek is exact.
func DecodeFrom(ctx context.Context, ffmpegBin, path string, from, limit time.Duration) ([]int16, error) {
	if ffmpegBin == "" {
		ffmpegBin = "ffmpeg"
	}
	if limit <= 0 {
		limit = defaultDecodeLimit
	}
	limit = min(limit, maxDecodeLimit)

	secs := strconv.FormatFloat(limit.Seconds(), 'f', 3, 64)
	args := []string{"-nostdin", "-hide_banner", "-loglevel", "error"}
	if from > 0 {
		// Before -i: a fast seek, made exact by dropping the audio before it.
		args = append(args, "-ss", strconv.FormatFloat(from.Seconds(), 'f', 3, 64))
	}
	args = append(args,
		"-t", secs,
		"-i", path,
		"-map", "0:a:0",
		"-vn", "-sn", "-dn",
		"-ac", "1",
		"-ar", strconv.Itoa(SampleRate),
		"-f", "s16le",
		"-acodec", "pcm_s16le",
		"pipe:1",
	)
	cmd := exec.CommandContext(ctx, ffmpegBin, args...)

	// One extra second of slack covers resampler and container rounding.
	maxBytes := (int(limit.Seconds()) + 1) * SampleRate * 2
	stdout := &capWriter{max: maxBytes}
	stderr := &capWriter{max: stderrCap}
	cmd.Stdout = stdout
	cmd.Stderr = stderr

	// Background work: low priority, and no console window on Windows.
	err := ffmpeg.StartLow(cmd)
	if err == nil {
		err = cmd.Wait()
	}
	if err != nil {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return nil, fmt.Errorf("intro: decode %s: %w", path, ctxErr)
		}
		msg := strings.TrimSpace(string(stderr.buf))
		if msg != "" {
			return nil, fmt.Errorf("intro: decode %s: %w: %s", path, err, msg)
		}
		return nil, fmt.Errorf("intro: decode %s: %w", path, err)
	}

	raw := stdout.buf
	if len(raw) < 2 {
		return nil, errors.New("intro: decode " + path + ": no audio")
	}
	samples := make([]int16, len(raw)/2)
	for i := range samples {
		samples[i] = int16(binary.LittleEndian.Uint16(raw[2*i:]))
	}
	return samples, nil
}

// capWriter keeps at most max bytes and silently drops the rest, so a
// misbehaving child process can neither block on a full pipe nor exhaust
// memory.
type capWriter struct {
	buf []byte
	max int
}

func (w *capWriter) Write(p []byte) (int, error) {
	if room := w.max - len(w.buf); room > 0 {
		w.buf = append(w.buf, p[:min(room, len(p))]...)
	}
	return len(p), nil
}
