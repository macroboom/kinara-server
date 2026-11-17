package api

import (
	"context"
	"fmt"
	"net/http"
	"runtime"
	"slices"
	"strings"
	"sync"
	"time"

	"kinara/internal/ffmpeg"
	"kinara/internal/store"
	"kinara/internal/transcode"
)

// Manage, Transcoding: the owner's transcoding choices. They used to be
// environment variables only (KINARA_HWACCEL, KINARA_HW_DEVICE,
// KINARA_MAX_TRANSCODES); those still set the defaults, and what the owner
// picks here wins. Everything applies without a restart, to conversions
// that start afterwards.

// transcodeEnv is the graphics card choice from the environment, read
// before any setting changes it.
type transcodeEnv struct {
	once    sync.Once
	encoder string
	device  string
}

func (s *Server) envChoice() (encoder, device string) {
	s.tcEnv.once.Do(func() {
		// Read straight from the fields: nothing has changed them yet, and
		// Choice would wait for a graphics card check that may be running.
		if tool := s.transcoderTool(); tool != nil {
			s.tcEnv.encoder, s.tcEnv.device = tool.HWAccel, tool.Device
		}
		if s.tcEnv.encoder == "" {
			s.tcEnv.encoder = "auto"
		}
	})
	return s.tcEnv.encoder, s.tcEnv.device
}

func (s *Server) transcoderTool() *ffmpeg.Tool {
	if s.Transcoder == nil {
		return nil
	}
	return s.Transcoder.Tool
}

// prefsOf turns the settings into the transcoder's choices.
func prefsOf(t store.TranscodeSettings) transcode.Prefs {
	return transcode.Prefs{Speed: t.Speed, MaxJobs: t.MaxJobs, ToneMap: t.ToneMap, ToneMapCurve: t.ToneMapCurve,
		AudioChannels: t.AudioChannels, StereoKbps: t.StereoKbps, SurroundKbps: t.SurroundKbps, NoDialogueBoost: !t.DialogueBoost}
}

// applyTranscode puts the settings to work: the graphics card choice (checked
// again when it changes) and the transcoder's choices.
func (s *Server) applyTranscode(t store.TranscodeSettings) {
	if s.Transcoder == nil {
		return
	}
	envEnc, envDev := s.envChoice()
	enc, dev := t.Encoder, t.Device
	if enc == "" {
		enc = envEnc
	}
	if dev == "" {
		dev = envDev
	}
	if s.Transcoder.Tool != nil {
		s.Transcoder.Tool.Reconfigure(enc, dev)
	}
	s.Transcoder.SetPrefs(prefsOf(t))
}

// ApplyTranscodeSettings reads the saved choices at start.
func (s *Server) ApplyTranscodeSettings(ctx context.Context) {
	s.envChoice()
	t, err := s.Store.GetTranscodeSettings(ctx)
	if err != nil {
		s.Log.Warn("transcoding settings", "err", err)
		return
	}
	s.applyTranscode(t)
}

// transcodeSettings reads the choices (defaults when the store can't say).
func (s *Server) transcodeSettings(ctx context.Context) store.TranscodeSettings {
	t, err := s.Store.GetTranscodeSettings(ctx)
	if err != nil {
		return store.DefaultTranscodeSettings
	}
	return t
}

// qualityLimits holds a conversion to the owner's limits: the default
// bitrate when the player didn't ask, and the most at home or away. A copied
// picture over the limit is re-encoded down to it.
func (s *Server) qualityLimits(r *http.Request, req *transcode.Request, v store.Version) {
	t := s.transcodeSettings(r.Context())
	if req.MaxBitrate <= 0 && t.DefaultKbps > 0 {
		req.MaxBitrate = t.DefaultKbps
	}
	kbps, height := t.HomeMaxKbps, t.HomeMaxHeight
	if fromInternet(r) {
		kbps, height = t.AwayMaxKbps, t.AwayMaxHeight
	}
	if kbps > 0 {
		clampToCap(req, kbps, v.Bitrate)
	}
	if height > 0 {
		if req.MaxHeight <= 0 || req.MaxHeight > height {
			req.MaxHeight = height
		}
		if req.Video == "copy" && v.Height > height {
			req.Video = "h264"
		}
	}
}

// connectionLimit is the most a conversion may use on this connection in
// kbit/s (0: none): the relay's cap and the owner's home or away limit.
func (s *Server) connectionLimit(r *http.Request) int {
	t := s.transcodeSettings(r.Context())
	limit := t.HomeMaxKbps
	if fromInternet(r) {
		limit = t.AwayMaxKbps
	}
	if c := s.relayCap(r); c > 0 && (limit == 0 || c < limit) {
		limit = c
	}
	return limit
}

// transcodingView is GET /settings/transcoding.
type transcodingView struct {
	Settings store.TranscodeSettings `json:"settings"`
	// Available: this server converts files (ffmpeg, KINARA_TRANSCODE on).
	Available bool `json:"available"`
	// Checking: the graphics card is being checked now (after a change).
	Checking bool `json:"checking"`
	// Detected is what's in use: the encoder, its card, tone mapping.
	Detected *ffmpeg.Caps `json:"detected,omitempty"`
	// Platform is the server's operating system: which encoders apply.
	Platform string `json:"platform"`
	// Backends are the graphics card encoders for this system; Listed and
	// Works are known after POST /settings/transcoding/test.
	Backends []ffmpeg.Backend `json:"backends"`
	// Devices are the Linux render nodes (empty elsewhere).
	Devices []ffmpeg.RenderNode `json:"devices"`
	// EnvEncoder and EnvDevice are the defaults from the environment.
	EnvEncoder string `json:"envEncoder"`
	EnvDevice  string `json:"envDevice,omitempty"`
	// MaxJobs is how many streams convert at once now.
	MaxJobs int `json:"maxJobs"`
}

func (s *Server) transcodingView(ctx context.Context) transcodingView {
	envEnc, envDev := s.envChoice()
	out := transcodingView{Settings: s.transcodeSettings(ctx), Platform: runtime.GOOS, Backends: ffmpeg.Backends(),
		Devices: ffmpeg.RenderNodes(), EnvEncoder: envEnc, EnvDevice: envDev}
	if out.Backends == nil {
		out.Backends = []ffmpeg.Backend{}
	}
	if out.Devices == nil {
		out.Devices = []ffmpeg.RenderNode{}
	}
	if s.Transcoder == nil || s.Transcoder.Tool == nil {
		return out
	}
	caps, ok := s.Transcoder.Tool.Ready()
	out.Checking = !ok
	if ok {
		out.Available = caps.Found
		out.Detected = &caps
		out.MaxJobs = s.Transcoder.JobLimit()
	} else {
		out.Available = true
	}
	return out
}

func (s *Server) getTranscoding(w http.ResponseWriter, r *http.Request, u store.User) {
	writeJSON(w, http.StatusOK, s.transcodingView(r.Context()))
}

func (s *Server) putTranscoding(w http.ResponseWriter, r *http.Request, u store.User) {
	t := s.transcodeSettings(r.Context())
	// Fields left out keep their values.
	if err := readJSON(r, &t); err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	if err := checkTranscode(&t); err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	if err := s.Store.PutTranscodeSettings(r.Context(), t); err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	s.applyTranscode(t)
	s.Log.Info("transcoding settings saved", "by", u.Username, "encoder", t.Encoder, "speed", t.Speed, "max_jobs", t.MaxJobs)
	// A new graphics card choice is checked in the background ("checking"
	// in the answer until it's done).
	writeJSON(w, http.StatusOK, s.transcodingView(r.Context()))
}

// testEncoders tries every graphics card encoder for this system.
func (s *Server) testEncoders(w http.ResponseWriter, r *http.Request, u store.User) {
	if s.Transcoder == nil || s.Transcoder.Tool == nil {
		writeErr(w, http.StatusNotImplemented, "converting is turned off on this server")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 2*time.Minute)
	defer cancel()
	writeJSON(w, http.StatusOK, map[string]any{"backends": s.Transcoder.Tool.Survey(ctx)})
}

// checkTranscode checks the settings, filling in defaults for empty choices.
func checkTranscode(t *store.TranscodeSettings) error {
	t.Encoder = strings.ToLower(strings.TrimSpace(t.Encoder))
	encoders := []string{"", "auto", "none", ffmpeg.HWNVENC, ffmpeg.HWQSV, ffmpeg.HWVAAPI, ffmpeg.HWAMF, ffmpeg.HWVideoToolbox}
	if !slices.Contains(encoders, t.Encoder) {
		return fmt.Errorf("encoder must be one of auto, none, nvenc, qsv, vaapi, amf, videotoolbox")
	}
	t.Device = strings.TrimSpace(t.Device)
	if t.Device != "" && !strings.HasPrefix(t.Device, "/dev/dri/") {
		return fmt.Errorf("the device must be a render node under /dev/dri/")
	}
	if t.Speed == "" {
		t.Speed = "balanced"
	}
	if !slices.Contains(transcode.Speeds, t.Speed) {
		return fmt.Errorf("speed must be faster, balanced or quality")
	}
	if t.ToneMap == "" {
		t.ToneMap = "auto"
	}
	if !slices.Contains(transcode.ToneMaps, t.ToneMap) {
		return fmt.Errorf("tone mapping must be auto or zscale")
	}
	if t.ToneMapCurve == "" {
		t.ToneMapCurve = "auto"
	}
	if !slices.Contains(transcode.ToneMapCurves, t.ToneMapCurve) {
		return fmt.Errorf("the tone mapping curve must be auto, hable, mobius, reinhard or bt.2390")
	}
	if t.MaxJobs < 0 || t.MaxJobs > 32 {
		return fmt.Errorf("conversions at once must be 1 to 32, or 0 for automatic")
	}
	for _, k := range []struct {
		name string
		v    int
	}{{"the default bitrate", t.DefaultKbps}, {"the home limit", t.HomeMaxKbps}, {"the away limit", t.AwayMaxKbps}} {
		if k.v != 0 && (k.v < 500 || k.v > 80000) {
			return fmt.Errorf("%s must be 500 to 80000 kbit/s, or 0", k.name)
		}
	}
	for _, h := range []int{t.HomeMaxHeight, t.AwayMaxHeight} {
		if h != 0 && !slices.Contains([]int{360, 480, 720, 1080, 1440, 2160}, h) {
			return fmt.Errorf("a resolution limit must be 360, 480, 720, 1080, 1440 or 2160, or 0")
		}
	}
	if t.AudioChannels != 0 && t.AudioChannels != 2 {
		return fmt.Errorf("audio channels must be 0 (up to 5.1) or 2 (stereo)")
	}
	for _, k := range []int{t.StereoKbps, t.SurroundKbps} {
		if k != 0 && (k < 64 || k > 640) {
			return fmt.Errorf("audio bitrates must be 64 to 640 kbit/s, or 0")
		}
	}
	return nil
}
