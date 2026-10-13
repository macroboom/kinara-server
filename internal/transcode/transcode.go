// Package transcode converts a file on the fly for players that can't play
// it as is: an audio format the TV can't decode (DTS, TrueHD), a video
// format it can't decode, or a file too big for the connection.
//
// Output is a Matroska stream written as ffmpeg produces it. It has no
// index, so the player can't seek inside it; the app seeks by asking for a
// new stream that starts at the new time.
package transcode

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os/exec"
	"sort"
	"strconv"
	"strings"
	"time"

	"kinora/internal/ffmpeg"
	"kinora/internal/store"
)

// Request says what to make.
type Request struct {
	Path string
	// Start is where to begin, in seconds.
	Start float64
	// Video is "copy" (keep as is) or "h264" (re-encode).
	Video string
	// MaxBitrate caps the re-encoded video, in kbit/s (0 = 8000).
	MaxBitrate int
	// MaxHeight caps the re-encoded picture height (0 = 1080).
	MaxHeight int
	// HDR is the source's HDR kind ("" for SDR); re-encoded HDR is tone
	// mapped to SDR when ffmpeg can.
	HDR string
	// Audio is the source stream index of the audio track to keep (-1 for
	// the first audio track).
	Audio int
	// AudioCodec is "copy" or "aac".
	AudioCodec string
	// AudioChannels is the source track's channel count, if known.
	AudioChannels int
	// Subtitles are the file's subtitle tracks; they're carried over so
	// subtitles keep working.
	Subtitles []store.Stream
}

// Transcoder runs ffmpeg for streams.
type Transcoder struct {
	Tool *ffmpeg.Tool
	Log  *slog.Logger
	// MaxJobs is how many streams can be converted at once (default 3).
	MaxJobs int

	sem chan struct{}
}

// ErrBusy means too many streams are being converted.
var ErrBusy = errors.New("the server is converting as many streams as it can; try again shortly")

func (t *Transcoder) slots() chan struct{} {
	if t.sem == nil {
		n := t.MaxJobs
		if n <= 0 {
			n = 3
		}
		t.sem = make(chan struct{}, n)
	}
	return t.sem
}

// Init sets up the job limit. Call once before serving.
func (t *Transcoder) Init() { t.slots() }

// Validate checks a request's options.
func (r *Request) Validate() error {
	switch r.Video {
	case "copy", "h264":
	default:
		return errors.New(`video must be "copy" or "h264"`)
	}
	switch r.AudioCodec {
	case "copy", "aac":
	default:
		return errors.New(`audio codec must be "copy" or "aac"`)
	}
	if r.Start < 0 {
		r.Start = 0
	}
	if r.MaxBitrate <= 0 {
		r.MaxBitrate = 8000
	}
	r.MaxBitrate = min(max(r.MaxBitrate, 500), 80000)
	if r.MaxHeight <= 0 {
		r.MaxHeight = 1080
	}
	r.MaxHeight = min(max(r.MaxHeight, 240), 2160)
	return nil
}

// subtitleCodec says how to carry a subtitle track into Matroska, or "" to
// drop it.
func subtitleCodec(codec string) string {
	switch codec {
	case "subrip", "srt", "ass", "ssa", "webvtt", "hdmv_pgs_subtitle", "dvd_subtitle", "dvb_subtitle":
		return "copy"
	case "mov_text", "text":
		return "srt"
	}
	return ""
}

// Args builds the ffmpeg arguments (without the binary name).
func Args(r Request, caps ffmpeg.Caps) []string {
	encode := r.Video == "h264"
	a := []string{"-nostdin", "-hide_banner", "-loglevel", "error"}
	if encode {
		// Decode on the GPU when there is one; ffmpeg falls back to the CPU.
		a = append(a, "-hwaccel", "auto")
	}
	if r.Start > 0 {
		a = append(a, "-ss", strconv.FormatFloat(r.Start, 'f', 3, 64))
	}
	a = append(a, "-i", r.Path, "-map", "0:v:0")
	if r.Audio >= 0 {
		a = append(a, "-map", "0:"+strconv.Itoa(r.Audio))
	} else {
		a = append(a, "-map", "0:a:0?")
	}
	var subCodecs []string
	for _, s := range r.Subtitles {
		c := subtitleCodec(s.Codec)
		if c == "" {
			continue
		}
		a = append(a, "-map", "0:"+strconv.Itoa(s.Index))
		subCodecs = append(subCodecs, c)
	}
	a = append(a, "-map_chapters", "-1")

	if encode {
		var filters []string
		filters = append(filters, fmt.Sprintf("scale=-2:'min(ih,%d)'", r.MaxHeight))
		if r.HDR != "" && caps.ToneMap {
			filters = append(filters, ffmpeg.ToneMapFilter)
		} else {
			filters = append(filters, "format=yuv420p")
		}
		a = append(a, "-vf", strings.Join(filters, ","))
		a = append(a, videoEncoderArgs(caps.H264Encoder, r.MaxBitrate)...)
	} else {
		a = append(a, "-c:v", "copy")
	}

	if r.AudioCodec == "aac" {
		ch := r.AudioChannels
		bitrate := "192k"
		if ch > 2 {
			bitrate = "384k"
		}
		a = append(a, "-c:a", "aac", "-b:a", bitrate)
		if ch > 6 {
			a = append(a, "-ac", "6")
		}
	} else {
		a = append(a, "-c:a", "copy")
	}
	for i, c := range subCodecs {
		a = append(a, "-c:s:"+strconv.Itoa(i), c)
	}
	a = append(a,
		"-max_muxing_queue_size", "4096",
		"-avoid_negative_ts", "make_zero",
		"-f", "matroska", "pipe:1")
	return a
}

func videoEncoderArgs(enc string, kbps int) []string {
	rate := strconv.Itoa(kbps) + "k"
	buf := strconv.Itoa(kbps*2) + "k"
	switch enc {
	case "h264_nvenc":
		return []string{"-c:v", "h264_nvenc", "-preset", "p4", "-tune", "ll", "-rc", "vbr",
			"-b:v", rate, "-maxrate", rate, "-bufsize", buf, "-profile:v", "high", "-g", "96"}
	case "h264_qsv":
		return []string{"-c:v", "h264_qsv", "-preset", "faster",
			"-b:v", rate, "-maxrate", rate, "-bufsize", buf, "-profile:v", "high", "-g", "96"}
	case "h264_amf":
		return []string{"-c:v", "h264_amf", "-quality", "speed", "-rc", "vbr_peak",
			"-b:v", rate, "-maxrate", rate, "-bufsize", buf, "-profile:v", "high", "-g", "96"}
	case "h264_videotoolbox":
		return []string{"-c:v", "h264_videotoolbox", "-realtime", "1",
			"-b:v", rate, "-maxrate", rate, "-bufsize", buf, "-profile:v", "high", "-g", "96"}
	default:
		return []string{"-c:v", "libx264", "-preset", "veryfast", "-crf", "21",
			"-maxrate", rate, "-bufsize", buf, "-profile:v", "high", "-g", "96"}
	}
}

// Stream converts r and writes the result to w until it ends, fails, or ctx
// is cancelled (the player went away). It waits up to 5 seconds for a free
// slot, which covers the moment a seek replaces one stream with another.
func (t *Transcoder) Stream(ctx context.Context, w io.Writer, r Request) error {
	if err := r.Validate(); err != nil {
		return err
	}
	caps := t.Tool.Caps()
	if !caps.Found {
		return errors.New("ffmpeg was not found; install ffmpeg to convert files the TV can't play")
	}
	if r.Video == "h264" && caps.H264Encoder == "" {
		return errors.New("this ffmpeg has no H.264 encoder")
	}
	select {
	case t.slots() <- struct{}{}:
	case <-time.After(5 * time.Second):
		return ErrBusy
	case <-ctx.Done():
		return ctx.Err()
	}
	defer func() { <-t.slots() }()

	args := Args(r, caps)
	cmd := exec.CommandContext(ctx, t.Tool.Path(), args...)
	ffmpeg.HideWindow(cmd)
	cmd.Stdout = w
	tail := &ffmpeg.Tail{N: 2048}
	cmd.Stderr = tail
	start := time.Now()
	t.Log.Info("transcode start", "file", r.Path, "from", r.Start, "video", r.Video,
		"encoder", caps.H264Encoder, "audio", r.AudioCodec)
	err := cmd.Run()
	took := time.Since(start).Round(time.Second)
	if ctx.Err() != nil {
		// The player closed the stream (stopped, or seeked): not an error.
		t.Log.Info("transcode stopped", "file", r.Path, "after", took)
		return nil
	}
	if err != nil {
		msg := tail.String()
		t.Log.Warn("transcode failed", "file", r.Path, "err", err, "ffmpeg", msg)
		if msg != "" {
			return fmt.Errorf("ffmpeg: %s", msg)
		}
		return fmt.Errorf("ffmpeg: %w", err)
	}
	t.Log.Info("transcode finished", "file", r.Path, "took", took)
	return nil
}

// Start is where a stream that keeps the original video should begin.
type Start struct {
	// Keyframe is where the picture will really start, in seconds from the
	// start of the file. The player's clock counts from here.
	Keyframe float64 `json:"start"`
	// Request is the time to ask ffmpeg for. It's a little after the
	// keyframe: with -ss before -i, ffmpeg backs off slightly for files with
	// B-frames, and asking for the keyframe's exact time would land on the
	// keyframe before it.
	Request float64 `json:"request"`
}

// FindStart finds the video keyframe to start a copied stream at, near t:
// the first one at or after t when moving forward (so a skip always moves),
// else the last one at or before t. If none can be found it returns t.
// ffprobe "" means ffprobe from PATH.
func FindStart(ctx context.Context, ffprobe, path string, t float64, forward bool) (Start, error) {
	plain := Start{Keyframe: t, Request: t}
	if t <= 0 {
		return Start{}, nil
	}
	if ffprobe == "" {
		ffprobe = "ffprobe"
	}
	ctx, cancel := context.WithTimeout(ctx, 8*time.Second)
	defer cancel()
	from := max(0, t-20)
	// A window rather than "the next frame": MPEG-TS seeks don't land on
	// keyframes, so a single frame after the seek is usually not one.
	cmd := exec.CommandContext(ctx, ffprobe, "-v", "error",
		"-read_intervals", fmt.Sprintf("%.3f%%%.3f", from, t+20),
		"-skip_frame", "nokey", "-select_streams", "v:0",
		"-show_entries", "frame=pts_time,pkt_pts_time,best_effort_timestamp_time:format=start_time",
		"-of", "json", "--", path)
	ffmpeg.HideWindow(cmd)
	out, err := cmd.Output()
	if err != nil {
		return plain, fmt.Errorf("ffprobe: %w", err)
	}
	keys, err := parseKeyframes(out)
	if err != nil || len(keys) == 0 {
		return plain, errors.New("ffprobe found no keyframes near that time")
	}
	return pickStart(keys, t, forward), nil
}

// parseKeyframes reads keyframe times from ffprobe's JSON, relative to the
// start of the file (MPEG-TS times count from an arbitrary clock, and
// ffmpeg's -ss counts from the file's start).
func parseKeyframes(out []byte) ([]float64, error) {
	var res struct {
		Frames []struct {
			PTS     string `json:"pts_time"`
			PktPTS  string `json:"pkt_pts_time"`
			BestPTS string `json:"best_effort_timestamp_time"`
		} `json:"frames"`
		Format struct {
			StartTime string `json:"start_time"`
		} `json:"format"`
	}
	if err := json.Unmarshal(out, &res); err != nil {
		return nil, err
	}
	base, _ := strconv.ParseFloat(res.Format.StartTime, 64)
	var keys []float64
	for _, f := range res.Frames {
		for _, v := range []string{f.PTS, f.BestPTS, f.PktPTS} {
			if x, err := strconv.ParseFloat(v, 64); err == nil {
				keys = append(keys, x-base)
				break
			}
		}
	}
	sort.Float64s(keys)
	return keys, nil
}

// pickStart chooses the keyframe and the time to request (a quarter second
// after it, or half way to the next keyframe if that's closer).
func pickStart(keys []float64, t float64, forward bool) Start {
	const slack = 0.05
	idx := -1
	if forward {
		for i, k := range keys {
			if k >= t-slack {
				idx = i
				break
			}
		}
	}
	if idx < 0 {
		for i, k := range keys {
			if k <= t+slack {
				idx = i
			}
		}
	}
	if idx < 0 {
		idx = 0
	}
	k := max(0, keys[idx])
	nudge := 0.25
	if idx+1 < len(keys) {
		nudge = min(nudge, (keys[idx+1]-keys[idx])/2)
	}
	if k == 0 {
		return Start{}
	}
	return Start{Keyframe: k, Request: k + nudge}
}
