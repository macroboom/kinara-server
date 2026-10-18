// Package transcode converts a file on the fly for players that can't play
// it as is: an audio format the TV can't decode (DTS, TrueHD), a video
// format it can't decode, or a file too big for the connection.
//
// Output is a Matroska stream written as ffmpeg produces it. It has no
// index, so the player can't seek inside it; the app seeks by asking for a
// new stream that starts at the new time.
package transcode

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"kinora/internal/ffmpeg"
	"kinora/internal/probe"
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
	// SourceCodec is the file's video codec ("hevc"), for MP4 tagging.
	SourceCodec string
	// Audio is the source stream index of the audio track to keep (-1 for
	// the first audio track).
	Audio int
	// AudioCodec is "copy" or "aac".
	AudioCodec string
	// AudioChannels is the source track's channel count, if known.
	AudioChannels int
	// Subtitles are the file's subtitle tracks; they're carried over so
	// subtitles keep working (Matroska only).
	Subtitles []store.Stream
	// Container is "mkv" (the TV app; the default) or "mp4", a fragmented
	// MP4 a web browser can play as it arrives, or "hls" (set by HLS.Start:
	// segments written to OutDir for iPhones and Safari). MP4 and HLS carry
	// no subtitles.
	Container string
	// OutDir is where HLS writes its playlist and segments.
	OutDir string
	// Segments is the HLS segment kind: "fmp4" (the default; Apple players
	// and browsers) or "ts" (MPEG-TS, what Roku players handle best).
	Segments string
	// MaxAudioChannels caps converted audio (0: up to 6). A player that
	// can't decode 5.1 AAC asks for 2.
	MaxAudioChannels int
	// Owner is who the stream is for.
	Owner Owner
	// SoftwareDecode starts at the CPU decoding stage (see pipeline.go).
	SoftwareDecode bool
	// Stage is where the video work happens; set by Stream and HLS.Start
	// as they work down the stages ("" is the first).
	Stage Stage
	// SourceProfile is the video's codec profile ("High 10"), for sources
	// the graphics card can't decode.
	SourceProfile string
	// DV is the file's Dolby Vision configuration; read from the file when
	// HDR is "dolby_vision" and this is empty.
	DV probe.DolbyVision
	// ClientDV is "1" when the player shows Dolby Vision, "0" when it
	// doesn't, "" when it didn't say (see dovi.go).
	ClientDV string

	// dvRoute is how copied Dolby Vision goes out, set by prepare.
	dvRoute string
}

// UsesGPUDecode reports whether a request would decode on an NVIDIA card,
// so a caller can try again without it when that fails.
func (t *Transcoder) UsesGPUDecode(r Request) bool {
	d, _ := gpuPipeline(r, t.Tool.Caps())
	return d
}

// gpuPipeline says whether a re-encode runs entirely on an NVIDIA card:
// decoded there, scaled there, encoded there. HDR keeps GPU decoding but
// goes through system memory for tone mapping (libplacebo or zscale).
func gpuPipeline(r Request, caps ffmpeg.Caps) (decode, keepOnGPU bool) {
	if r.Video != "h264" || r.SoftwareDecode || (r.Stage != "" && r.Stage != StageGPU) || !caps.CUDA || caps.H264Encoder != "h264_nvenc" {
		return false, false
	}
	if !ffmpeg.NVDEC[strings.ToLower(r.SourceCodec)] {
		return false, false
	}
	return true, r.HDR == ""
}

// Transcoder runs ffmpeg for streams.
type Transcoder struct {
	Tool *ffmpeg.Tool
	// FFprobe is the ffprobe executable ("" for the one on PATH).
	FFprobe string
	Log     *slog.Logger
	// MaxJobs is how many streams can be converted at once (default: 6
	// with a hardware encoder, 3 without).
	MaxJobs int

	semOnce sync.Once
	sem     chan struct{}
	reg     registry
	dvOnce  sync.Once
	dv      *dvCache

	stageOnce sync.Once
	stageMem  *stageMemo
}

// ErrBusy means too many streams are being converted.
var ErrBusy = errors.New("the server is converting as many streams as it can; try again shortly")

func (t *Transcoder) slots() chan struct{} {
	t.semOnce.Do(func() {
		n := t.MaxJobs
		if n <= 0 {
			n = 3
			// A graphics card's encoder does the heavy lifting; NVIDIA's
			// consumer cards allow 8 encodes at once, others a similar few.
			if t.Tool != nil && hardwareEncoder(t.Tool.Caps().H264Encoder) {
				n = 6
			}
		}
		t.sem = make(chan struct{}, n)
		if t.Log != nil {
			t.Log.Info("transcode slots", "max", n)
		}
	})
	return t.sem
}

func hardwareEncoder(enc string) bool {
	return enc != "" && enc != "libx264"
}

// Init sets up the job limit. Call once before serving; it waits for the
// encoder check, which decides the default.
func (t *Transcoder) Init() { go t.slots() }

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
	switch r.Container {
	case "":
		r.Container = "mkv"
	case "mkv", "mp4":
	case "hls":
		if r.OutDir == "" {
			return errors.New("hls needs an output folder")
		}
	default:
		return errors.New(`container must be "mkv" or "mp4"`)
	}
	switch r.Segments {
	case "":
		r.Segments = "fmp4"
	case "fmp4", "ts":
	default:
		return errors.New(`segments must be "fmp4" or "ts"`)
	}
	if r.MaxAudioChannels < 0 || r.MaxAudioChannels > 8 {
		return errors.New("audio channels must be 1 to 8")
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
	// -stats still prints the progress line at this log level; Stream reads
	// it to log how fast the conversion runs.
	a := []string{"-nostdin", "-hide_banner", "-loglevel", "error", "-stats", "-stats_period", "5"}
	var plan videoPlan
	if encode {
		plan = planVideo(r, caps)
		a = append(a, plan.input...)
	}
	if r.Start > 0 {
		a = append(a, "-ss", strconv.FormatFloat(r.Start, 'f', 3, 64))
	}
	a = append(a, "-i", r.Path)
	// HDR converted to SDR goes through -filter_complex rather than -vf: a
	// stream made by a filter graph doesn't inherit the source stream's side
	// data, so the H.264 out isn't labelled with the source's Dolby Vision
	// configuration or HDR mastering data. Players that see a Dolby Vision
	// label on plain H.264 (Roku) fetch it and never start.
	fresh := encode && r.HDR != ""
	if fresh {
		vf := plan.vf
		if vf == "" {
			vf = "null"
		}
		a = append(a, "-filter_complex", "[0:v:0]"+vf+"[v]", "-map", "[v]")
	} else {
		a = append(a, "-map", "0:v:0")
	}
	if r.Audio >= 0 {
		a = append(a, "-map", "0:"+strconv.Itoa(r.Audio))
	} else {
		a = append(a, "-map", "0:a:0?")
	}
	mp4 := r.Container == "mp4" || r.Container == "hls"
	hlsTS := r.Container == "hls" && r.Segments == "ts"
	var subCodecs []string
	for _, s := range r.Subtitles {
		if mp4 {
			break // browsers get subtitles separately
		}
		c := subtitleCodec(s.Codec)
		if c == "" {
			continue
		}
		a = append(a, "-map", "0:"+strconv.Itoa(s.Index))
		subCodecs = append(subCodecs, c)
	}
	a = append(a, "-map_chapters", "-1")

	if encode {
		if !fresh {
			a = append(a, "-vf", plan.vf)
		}
		a = append(a, videoEncoderArgs(plan.encoder, r.MaxBitrate, caps)...)
		if r.Container == "hls" {
			// A keyframe at every segment boundary, so each segment starts
			// cleanly and seeking lands where expected.
			a = append(a, "-force_key_frames", "expr:gte(t,n_forced*4)")
			if plan.encoder == "libx264" {
				a = append(a, "-sc_threshold", "0")
			}
		}
		if toneMapped(r, caps) && !plan.gpuFrames {
			a = append(a, ffmpeg.SDROutputArgs...)
		}
	} else {
		a = append(a, "-c:v", "copy")
		hevc := strings.EqualFold(r.SourceCodec, "hevc")
		switch {
		case r.dvRoute == dvStrip:
			// Only the HDR10 base layer: see dovi.go.
			a = append(a, "-bsf:v", ffmpeg.DoviStripBSF)
			if mp4 && !hlsTS {
				a = append(a, "-tag:v", "hvc1")
			}
		case r.dvRoute == dvTag && mp4 && !hlsTS:
			// dvh1 switches Dolby Vision on in Apple players; ffmpeg calls
			// writing it "unofficial".
			a = append(a, "-tag:v", "dvh1", "-strict", "unofficial")
		case mp4 && !hlsTS && hevc:
			a = append(a, "-tag:v", "hvc1") // what browsers and Apple players expect
		}
	}

	if r.AudioCodec == "aac" {
		// Up to 5.1, or fewer when the player asks; a track of unknown
		// size keeps its own layout unless the player set a limit.
		ch := r.AudioChannels
		limit := r.MaxAudioChannels
		asked := limit > 0
		if !asked || limit > 6 {
			limit = 6
		}
		out := ch
		if out > limit || (out <= 0 && asked) {
			out = limit
		}
		bitrate := "192k"
		if out > 2 {
			bitrate = "384k"
		}
		a = append(a, "-c:a", "aac", "-b:a", bitrate)
		if out == 2 && ch > 2 {
			// Surround to stereo: the plain downmix leaves dialogue quiet
			// (the centre channel is mixed in at -3 dB beside everything
			// else), so lift it and limit the peaks instead of clipping.
			a = append(a, "-af", "aresample=async=1,aformat=channel_layouts=stereo,"+
				"alimiter=level_in=2:limit=0.794328235:attack=5:release=50:level=false")
		} else {
			// async fills Matroska's millisecond timestamp gaps, which some
			// players otherwise play as crackle.
			a = append(a, "-af", "aresample=async=1")
			if out > 0 && out != ch {
				a = append(a, "-ac", strconv.Itoa(out))
			}
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
		// Send each packet as soon as it's muxed, and don't hold the start
		// for up to 10 s waiting on a sparse track (subtitles): the first
		// picture reaches the player sooner.
		"-flush_packets", "1",
		"-max_interleave_delta", "1000000")
	switch {
	case hlsTS:
		// MPEG-TS segments of about 4 s, listed as they're made.
		a = append(a, "-f", "hls", "-hls_time", "4", "-hls_list_size", "0",
			"-hls_playlist_type", "event", "-hls_segment_type", "mpegts",
			"-hls_flags", "independent_segments+temp_file",
			"-hls_segment_filename", filepath.Join(r.OutDir, "seg%05d.ts"),
			filepath.Join(r.OutDir, "index.m3u8"))
	case r.Container == "hls":
		// Fragmented-MP4 segments of about 4 s, listed as they're made.
		a = append(a, "-f", "hls", "-hls_time", "4", "-hls_list_size", "0",
			"-hls_playlist_type", "event", "-hls_segment_type", "fmp4",
			"-hls_flags", "independent_segments+temp_file",
			"-hls_fmp4_init_filename", "init.mp4",
			"-hls_segment_filename", filepath.Join(r.OutDir, "seg%05d.m4s"),
			filepath.Join(r.OutDir, "index.m3u8"))
	case mp4:
		// delay_moov rather than empty_moov: the header waits for the first
		// packets, which copied E-AC-3 (Atmos) needs to describe itself.
		a = append(a, "-movflags", "frag_keyframe+delay_moov+default_base_moof", "-f", "mp4", "pipe:1")
	default:
		a = append(a, "-f", "matroska", "pipe:1")
	}
	return a
}

// videoFilter scales a re-encoded picture down to the height limit and, for
// HDR, converts it to SDR. With libplacebo both happen in one step on the
// GPU: scaling 4K 10-bit video on the CPU first was the slow part.
func videoFilter(r Request, caps ffmpeg.Caps) string {
	return planVideo(r, caps).vf
}

// cpuFilter is the scaling and tone mapping done on the CPU (libplacebo
// runs on the GPU through Vulkan but takes and gives ordinary frames).
func cpuFilter(r Request, caps ffmpeg.Caps, height int) string {
	clean := ""
	if caps.SideData != "" && r.HDR != "" {
		clean = "," + caps.SideData
	}
	if r.HDR != "" && caps.Placebo {
		w, h := ffmpeg.Fit(height)
		return fmt.Sprintf("libplacebo=w=%s:h=%s:", w, h) + strings.TrimPrefix(ffmpeg.PlaceboFilter, "libplacebo=") + clean
	}
	fw, fh := ffmpeg.Fit(height)
	filters := []string{fmt.Sprintf("scale=w=%s:h=%s", fw, fh)}
	if transfer := sourceTransfer(r); transfer != "" && caps.ToneMap {
		if caps.SetParams {
			filters = append(filters, ffmpeg.SourceParams(transfer))
		}
		filters = append(filters, ffmpeg.ToneMapFilter)
	} else {
		filters = append(filters, "format=yuv420p")
	}
	return strings.Join(filters, ",") + clean
}

// sourceTransfer is what the zscale tone map converts from: "pq", "hlg",
// or "" when the source is SDR (including Dolby Vision with an SDR base).
func sourceTransfer(r Request) string {
	switch r.HDR {
	case "":
		return ""
	case "hlg":
		return "hlg"
	case "dolby_vision":
		if r.DV.Present() {
			if t := r.DV.BaseTransfer(); t != "sdr" {
				return t
			}
			return ""
		}
	}
	return "pq"
}

// toneMapped reports whether a conversion turns HDR into SDR.
func toneMapped(r Request, caps ffmpeg.Caps) bool {
	if r.Video != "h264" || r.HDR == "" {
		return false
	}
	if _, onGPU := gpuPipeline(r, caps); onGPU && caps.HW == ffmpeg.HWNVENC {
		return false
	}
	return caps.Placebo || (caps.ToneMap && sourceTransfer(r) != "")
}

func videoEncoderArgs(enc string, kbps int, caps ffmpeg.Caps) []string {
	rate := strconv.Itoa(kbps) + "k"
	buf := strconv.Itoa(kbps*2) + "k"
	// Hardware encoders aim a little under the cap: with the average equal
	// to the maximum they pad easy scenes up to it (QSV runs as constant
	// bitrate then).
	target := strconv.Itoa(kbps*9/10) + "k"
	switch enc {
	case "h264_nvenc":
		// Quality over latency: this is film, not a video call. A recent
		// NVIDIA card still runs this many times faster than real time.
		return []string{"-c:v", "h264_nvenc", "-preset", "p5", "-tune", "hq", "-rc", "vbr", "-spatial-aq", "1",
			"-b:v", target, "-maxrate", rate, "-bufsize", buf, "-profile:v", "high", "-g", "96"}
	case "h264_qsv":
		// forced_idr: forced keyframes become IDR frames, which HLS can cut
		// segments at (plain I-frames leave segments of 2.5 to 5 s).
		return []string{"-c:v", "h264_qsv", "-preset", "veryfast", "-forced_idr", "1",
			"-b:v", target, "-maxrate", rate, "-bufsize", buf, "-profile:v", "high", "-g", "96"}
	case "h264_vaapi":
		// The rate control the driver accepted with a cap (checked at
		// start): left to choose, some drivers pick a mode that ignores it.
		a := []string{"-c:v", "h264_vaapi"}
		if mode := caps.VAAPIRateControl; mode != "" {
			a = append(a, "-rc_mode", mode)
		}
		return append(a, "-b:v", target, "-maxrate", rate, "-bufsize", buf, "-profile:v", "high", "-g", "96")
	case "h264_amf":
		return []string{"-c:v", "h264_amf", "-quality", "speed", "-rc", "vbr_peak",
			"-b:v", target, "-maxrate", rate, "-bufsize", buf, "-profile:v", "high", "-g", "96"}
	case "h264_videotoolbox":
		return []string{"-c:v", "h264_videotoolbox", "-realtime", "1",
			"-b:v", target, "-maxrate", rate, "-bufsize", buf, "-profile:v", "high", "-g", "96"}
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
	if err := t.prepare(ctx, &r, caps); err != nil {
		return err
	}
	release, err := t.acquire(ctx, r.Owner.Priority)
	if err != nil {
		return err
	}
	defer release()
	live := t.register(r, "pipe")
	defer t.unregister(live)

	// Work down the stages while nothing has reached the player: a card
	// can fail on a file it can't decode (an unusual profile or chroma
	// format) or a driver can refuse a filter. Once bytes went out, trying
	// again would corrupt the stream, so the error stands.
	stages := t.stagesFor(r, caps)
	for i, stage := range stages {
		r.Stage = stage
		var sent bool
		sent, err = t.stream(ctx, w, r, caps, live)
		if err == nil || sent {
			if stage != "" {
				t.rememberStage(r, stage, i > 0)
			}
			return err
		}
		if ctx.Err() != nil || i == len(stages)-1 {
			break
		}
		t.Log.Warn("conversion failed before sending anything; trying the next way", "file", filepath.Base(r.Path),
			"stage", stage, "next", stages[i+1], "err", err)
	}
	return err
}

// stream runs ffmpeg once for Stream. sent reports whether anything reached
// the player, after which trying again would corrupt the stream.
func (t *Transcoder) stream(ctx context.Context, w io.Writer, r Request, caps ffmpeg.Caps, live *Live) (sent bool, err error) {
	args := Args(r, caps)
	cmd := exec.CommandContext(ctx, t.Tool.Path(), args...)
	ffmpeg.HideWindow(cmd)
	start := time.Now()
	out := &firstOutput{w: &timedWriter{w: w, live: live}, start: start}
	cmd.Stdout = out
	tail := &ffmpeg.Tail{N: 2048}
	stats := &statsSplitter{rest: tail, onStat: live.stat}
	cmd.Stderr = stats
	enc := ""
	if r.Video == "h264" {
		enc = planVideo(r, caps).encoder
	}
	t.Log.Info("transcode start", "file", r.Path, "from", r.Start, "video", r.Video, "stage", r.Stage,
		"encoder", enc, "gpu", caps.HW, "audio", r.AudioCodec, "hdr", r.HDR,
		"tonemap", toneMapped(r, caps), "dolby_vision", dvSummary(r))

	// While it runs: when the first bytes reached the player, then the
	// speed every 10 s (below 1.0x the player will keep buffering).
	done := make(chan struct{})
	defer close(done)
	go func() {
		tick := time.NewTicker(10 * time.Second)
		defer tick.Stop()
		logged := false
		for {
			select {
			case <-done:
				return
			case <-tick.C:
				if first := out.first(); !first.IsZero() && !logged {
					logged = true
					t.Log.Info("transcode first output", "file", filepath.Base(r.Path), "after", first.Sub(start).Round(100*time.Millisecond))
				}
				if out.first().IsZero() {
					t.Log.Warn("transcode has sent nothing yet", "file", filepath.Base(r.Path), "waited", time.Since(start).Round(time.Second), "ffmpeg", stats.last())
				} else {
					in := live.info(time.Now())
					t.Log.Info("transcode progress", "file", filepath.Base(r.Path), "rate", fmt.Sprintf("%.2fx", in.Rate),
						"waiting_on_player", in.PlayerLimited, "behind", in.Behind, "ffmpeg", stats.last())
				}
			}
		}
	}()

	err = cmd.Run()
	sent = !out.first().IsZero()
	took := time.Since(start).Round(time.Second)
	if ctx.Err() != nil {
		// The player closed the stream (stopped, or seeked): not an error.
		t.Log.Info("transcode stopped", "file", r.Path, "after", took)
		return sent, nil
	}
	if err != nil {
		msg := tail.String()
		t.Log.Warn("transcode failed", "file", r.Path, "err", err, "ffmpeg", msg)
		if msg != "" {
			return sent, fmt.Errorf("ffmpeg: %s", msg)
		}
		return sent, fmt.Errorf("ffmpeg: %w", err)
	}
	t.Log.Info("transcode finished", "file", r.Path, "took", took)
	return sent, nil
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
	window := fmt.Sprintf("%.3f%%%.3f", from, t+20)

	// First, the packets' keyframe flags: no decoding at all, so it's quick
	// even for 4K HEVC (decoding each keyframe took seconds there).
	cmd := exec.CommandContext(ctx, ffprobe, "-v", "error",
		"-read_intervals", window, "-select_streams", "v:0",
		"-show_entries", "packet=pts_time,dts_time,flags:format=start_time",
		"-of", "json", "--", path)
	ffmpeg.HideWindow(cmd)
	keys := []float64{}
	if out, err := cmd.Output(); err == nil {
		keys, _ = parseKeyPackets(out)
	}
	if len(keys) == 0 {
		// Some files don't flag keyframe packets: decode keyframes instead.
		cmd = exec.CommandContext(ctx, ffprobe, "-v", "error",
			"-read_intervals", window,
			"-skip_frame", "nokey", "-select_streams", "v:0",
			"-show_entries", "frame=pts_time,pkt_pts_time,best_effort_timestamp_time:format=start_time",
			"-of", "json", "--", path)
		ffmpeg.HideWindow(cmd)
		out, err := cmd.Output()
		if err != nil {
			return plain, fmt.Errorf("ffprobe: %w", err)
		}
		if keys, err = parseKeyframes(out); err != nil {
			return plain, err
		}
	}
	if len(keys) == 0 {
		return plain, errors.New("ffprobe found no keyframes near that time")
	}
	return pickStart(keys, t, forward), nil
}

// parseKeyframes reads keyframe times from ffprobe's JSON, relative to the
// start of the file (MPEG-TS times count from an arbitrary clock, and
// ffmpeg's -ss counts from the file's start).
// parseKeyPackets reads keyframe times from ffprobe's packet list (packets
// flagged K), relative to the file's start.
func parseKeyPackets(out []byte) ([]float64, error) {
	var res struct {
		Packets []struct {
			PTS   string `json:"pts_time"`
			DTS   string `json:"dts_time"`
			Flags string `json:"flags"`
		} `json:"packets"`
		Format struct {
			StartTime string `json:"start_time"`
		} `json:"format"`
	}
	if err := json.Unmarshal(out, &res); err != nil {
		return nil, err
	}
	base, _ := strconv.ParseFloat(res.Format.StartTime, 64)
	var keys []float64
	for _, p := range res.Packets {
		if !strings.Contains(p.Flags, "K") {
			continue
		}
		for _, v := range []string{p.PTS, p.DTS} {
			if x, err := strconv.ParseFloat(v, 64); err == nil {
				keys = append(keys, x-base)
				break
			}
		}
	}
	sort.Float64s(keys)
	return keys, nil
}

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

// firstOutput passes ffmpeg's output on, noting when the first bytes went.
type firstOutput struct {
	w     io.Writer
	start time.Time
	mu    sync.Mutex
	at    time.Time
}

func (f *firstOutput) Write(p []byte) (int, error) {
	f.mu.Lock()
	if f.at.IsZero() {
		f.at = time.Now()
	}
	f.mu.Unlock()
	return f.w.Write(p)
}

func (f *firstOutput) first() time.Time {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.at
}

// statsSplitter separates ffmpeg's progress lines ("frame= ... speed=1.4x")
// from its error messages: it keeps the latest progress line, and passes
// everything else on (to the tail kept for error reports).
type statsSplitter struct {
	rest io.Writer
	// onStat, if set, gets each progress line.
	onStat func(line string)
	mu     sync.Mutex
	buf    []byte
	stat   string
}

func (s *statsSplitter) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.buf = append(s.buf, p...)
	for {
		i := bytes.IndexAny(s.buf, "\r\n")
		if i < 0 {
			break
		}
		line := strings.TrimSpace(string(s.buf[:i]))
		s.buf = s.buf[i+1:]
		if line == "" {
			continue
		}
		if strings.Contains(line, "speed=") {
			s.stat = strings.Join(strings.Fields(line), " ")
			if s.onStat != nil {
				s.onStat(s.stat)
			}
			continue
		}
		_, _ = s.rest.Write([]byte(line + "\n"))
	}
	// A very long line with no end yet: pass it on rather than grow forever.
	if len(s.buf) > 4096 {
		_, _ = s.rest.Write(s.buf)
		s.buf = s.buf[:0]
	}
	return len(p), nil
}

func (s *statsSplitter) last() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.stat == "" {
		return "no progress reported yet"
	}
	return s.stat
}
