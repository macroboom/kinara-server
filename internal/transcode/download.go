package transcode

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"time"

	"kinara/internal/ffmpeg"
)

// Downloads: a whole title written to a file for a phone or tablet to keep
// and play without a connection (see api/downloads.go). It's background
// work: it takes a conversion slot only while another stays free for
// someone watching, and ffmpeg runs at a lower priority.

// DownloadPreset is a download quality.
type DownloadPreset struct {
	ID    string `json:"id"`
	Label string `json:"label"`
	// MaxHeight and MaxBitrate (kbit/s) cap the picture; 0 for the
	// original file as it is.
	MaxHeight  int `json:"maxHeight"`
	MaxBitrate int `json:"maxBitrate"`
}

// DownloadPresets are the qualities a download can be, best first.
var DownloadPresets = []DownloadPreset{
	{ID: "original", Label: "Original"},
	{ID: "high", Label: "High (1080p)", MaxHeight: 1080, MaxBitrate: 8000},
	{ID: "medium", Label: "Medium (720p)", MaxHeight: 720, MaxBitrate: 4000},
	{ID: "low", Label: "Small (480p)", MaxHeight: 480, MaxBitrate: 1500},
}

// FindPreset looks a quality up by ID.
func FindPreset(id string) (DownloadPreset, bool) {
	for _, p := range DownloadPresets {
		if p.ID == id {
			return p, true
		}
	}
	return DownloadPreset{}, false
}

// downloadAudioKbps is the converted audio's rate (stereo AAC).
const downloadAudioKbps = 192

// CanCopyVideo says whether a source's picture can go into a download as it
// is: H.264 in SDR, no bigger and no busier than the preset allows. (H.264
// plays on every phone; HEVC and HDR don't, so they're converted.)
func (p DownloadPreset) CanCopyVideo(codec, hdr string, height int, bitrateBps int64) bool {
	if p.MaxHeight == 0 {
		return true
	}
	if codec != "h264" || hdr != "" || height <= 0 || height > p.MaxHeight {
		return false
	}
	// The file's overall rate includes its audio; allow a little over.
	return bitrateBps > 0 && bitrateBps/1000 <= int64(p.MaxBitrate)*11/10+int64(downloadAudioKbps)
}

// Estimate guesses a download's size in bytes for a video of seconds
// length (sourceBytes when it's the original or a copy).
func (p DownloadPreset) Estimate(seconds float64, sourceBytes int64, copyVideo bool) int64 {
	if p.MaxHeight == 0 || seconds <= 0 {
		return sourceBytes
	}
	if copyVideo && sourceBytes > 0 {
		return sourceBytes
	}
	// Encoders aim under the cap; most scenes come in below it.
	kbps := float64(p.MaxBitrate)*0.8 + downloadAudioKbps
	return int64(kbps * 1000 / 8 * seconds)
}

// Convert writes r to out as a plain MP4 (index at the front, so it plays
// and seeks straight away from storage). progress gets the seconds of
// video written so far. It waits for a free slot for as long as ctx
// allows, and works down the conversion stages like Stream; nothing has
// gone anywhere yet, so every stage can be tried.
func (t *Transcoder) Convert(ctx context.Context, r Request, out string, progress func(sec float64)) error {
	part := out + ".part"
	r.Container, r.OutFile, r.Start = "file", part, 0
	if err := r.Validate(); err != nil {
		return err
	}
	caps := t.Tool.Caps()
	if !caps.Found {
		return errors.New("ffmpeg was not found")
	}
	if r.Video == "h264" && caps.H264Encoder == "" {
		return errors.New("this ffmpeg has no H.264 encoder")
	}
	if err := t.prepare(ctx, &r, caps); err != nil {
		return err
	}
	release, err := t.acquireSpare(ctx)
	if err != nil {
		return err
	}
	defer release()
	live := t.register(r, "download")
	defer t.unregister(live)
	defer os.Remove(part)

	stages := t.stagesFor(r, caps)
	for i, stage := range stages {
		r.Stage = stage
		err = t.convertOnce(ctx, r, caps, live, progress)
		if err == nil {
			break
		}
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if i == len(stages)-1 {
			return err
		}
		t.Log.Warn("download conversion failed; trying the next way", "file", filepath.Base(r.Path),
			"stage", stage, "next", stages[i+1], "err", err)
	}
	if fi, err := os.Stat(part); err != nil || fi.Size() == 0 {
		return errors.New("ffmpeg wrote nothing")
	}
	return os.Rename(part, out)
}

func (t *Transcoder) convertOnce(ctx context.Context, r Request, caps ffmpeg.Caps, live *Live, progress func(float64)) error {
	_ = os.Remove(r.OutFile)
	cmd := exec.CommandContext(ctx, t.Tool.Path(), Args(r, caps)...)
	tail := &ffmpeg.Tail{N: 2048}
	cmd.Stderr = &statsSplitter{rest: tail, onStat: func(line string) {
		live.stat(line)
		if sec, ok := parseStatTime(line); ok && progress != nil {
			progress(sec)
		}
	}}
	start := time.Now()
	t.Log.Info("download conversion start", "file", r.Path, "video", r.Video, "stage", r.Stage,
		"height", r.MaxHeight, "bitrate", r.MaxBitrate, "audio", r.AudioCodec)
	if err := ffmpeg.StartLow(cmd); err != nil {
		return err
	}
	if err := cmd.Wait(); err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if msg := tail.String(); msg != "" {
			return fmt.Errorf("ffmpeg: %s", msg)
		}
		return fmt.Errorf("ffmpeg: %w", err)
	}
	t.Log.Info("download conversion finished", "file", r.Path, "took", time.Since(start).Round(time.Second))
	return nil
}

// acquireSpare takes a conversion slot for background work, only while at
// least one more stays free for someone who starts watching (with a single
// slot, when it's free). It waits as long as ctx allows.
func (t *Transcoder) acquireSpare(ctx context.Context) (release func(), err error) {
	sem := t.slots()
	keep := 1
	if cap(sem) <= 1 {
		keep = 0
	}
	tick := time.NewTicker(3 * time.Second)
	defer tick.Stop()
	for {
		if len(sem) < cap(sem)-keep {
			select {
			case sem <- struct{}{}:
				return func() { <-sem }, nil
			default:
			}
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-tick.C:
		}
	}
}
