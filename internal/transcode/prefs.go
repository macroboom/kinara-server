package transcode

import (
	"strings"
	"sync/atomic"

	"kinara/internal/ffmpeg"
)

// Prefs are the owner's transcoding choices (Manage, Transcoding). They
// apply to conversions started after they're set; the zero value of each
// field is what Kinara picks by itself.
type Prefs struct {
	// Speed trades encoding speed for picture quality at the same bitrate:
	// "faster", "balanced" (the default) or "quality".
	Speed string
	// MaxJobs is how many streams convert at once (0: KINARA_MAX_TRANSCODES,
	// else 6 with a graphics card encoder and 3 without).
	MaxJobs int
	// ToneMap is how HDR becomes SDR: "auto" (libplacebo when it works,
	// else zscale) or "zscale" (lighter, no Vulkan). Dolby Vision keeps
	// libplacebo either way: zscale can't apply its reshaping.
	ToneMap string
	// ToneMapCurve is the tone mapping curve: "auto" (BT.2390 with
	// libplacebo, Hable with zscale), "hable", "mobius", "reinhard" or
	// "bt.2390" (libplacebo only; zscale uses Hable).
	ToneMapCurve string
	// AudioChannels caps converted sound: 0 for up to 5.1, 2 for stereo
	// always.
	AudioChannels int
	// StereoKbps and SurroundKbps are the AAC bitrates (0: 192 and 384).
	StereoKbps   int
	SurroundKbps int
	// NoDialogueBoost turns off lifting dialogue when surround is mixed
	// down to stereo.
	NoDialogueBoost bool
}

// Speeds and tone mapping choices, for checking a request.
var (
	Speeds        = []string{"faster", "balanced", "quality"}
	ToneMaps      = []string{"auto", "zscale"}
	ToneMapCurves = []string{"auto", "hable", "mobius", "reinhard", "bt.2390"}
)

type prefsBox struct{ p atomic.Pointer[Prefs] }

// SetPrefs sets the owner's choices for conversions from now on.
func (t *Transcoder) SetPrefs(p Prefs) {
	t.prefs.p.Store(&p)
}

// Prefs are the owner's choices now.
func (t *Transcoder) Prefs() Prefs {
	if p := t.prefs.p.Load(); p != nil {
		return *p
	}
	return Prefs{}
}

// applyPrefs copies the owner's choices into a request.
func (t *Transcoder) applyPrefs(r *Request) {
	p := t.Prefs()
	r.Speed, r.ToneMapCurve = p.Speed, p.ToneMapCurve
	r.StereoKbps, r.SurroundKbps, r.NoDialogueBoost = p.StereoKbps, p.SurroundKbps, p.NoDialogueBoost
	r.zscaleOnly = p.ToneMap == "zscale"
	if p.AudioChannels > 0 && (r.MaxAudioChannels == 0 || r.MaxAudioChannels > p.AudioChannels) {
		r.MaxAudioChannels = p.AudioChannels
	}
}

// capsFor is what a request may use: without libplacebo when the owner
// chose zscale (unless it's Dolby Vision, which needs libplacebo).
func (t *Transcoder) capsFor(r Request) ffmpeg.Caps {
	c := t.Tool.Caps()
	if r.zscaleOnly && r.HDR != "dolby_vision" && c.ToneMap {
		c.Placebo = false
	}
	return c
}

// jobLimit is how many streams may convert at once.
func (t *Transcoder) jobLimit() int {
	if n := t.Prefs().MaxJobs; n > 0 {
		return n
	}
	if t.MaxJobs > 0 {
		return t.MaxJobs
	}
	// A graphics card's encoder does the heavy lifting; NVIDIA's consumer
	// cards allow 8 encodes at once, others a similar few.
	if t.Tool != nil && hardwareEncoder(t.Tool.Caps().H264Encoder) {
		return 6
	}
	return 3
}

// JobLimit is how many streams may convert at once now.
func (t *Transcoder) JobLimit() int { return t.jobLimit() }

// placeboCurve puts a curve into libplacebo's filter.
func placeboCurve(filter, curve string) string {
	switch curve {
	case "hable", "mobius", "reinhard", "bt.2390":
		return strings.Replace(filter, "tonemapping=bt.2390", "tonemapping="+curve, 1)
	}
	return filter
}

// zscaleCurve puts a curve into the zscale tone map (which has no BT.2390).
func zscaleCurve(filter, curve string) string {
	switch curve {
	case "mobius", "reinhard":
		return strings.Replace(filter, "tonemap=tonemap=hable", "tonemap=tonemap="+curve, 1)
	}
	return filter
}

// speedPreset is the encoder setting for a speed choice.
func speedPreset(enc, speed string) []string {
	switch enc {
	case "h264_nvenc":
		switch speed {
		case "faster":
			return []string{"-preset", "p3"}
		case "quality":
			return []string{"-preset", "p7"}
		}
		return []string{"-preset", "p5"}
	case "h264_qsv":
		if speed == "quality" {
			return []string{"-preset", "medium"}
		}
		return []string{"-preset", "veryfast"}
	case "h264_amf":
		if speed == "quality" {
			return []string{"-quality", "quality"}
		}
		return []string{"-quality", "speed"}
	case "h264_videotoolbox":
		if speed == "quality" {
			return nil
		}
		return []string{"-realtime", "1"}
	case "h264_vaapi":
		// Drivers differ in what they take here; left to the driver.
		return nil
	}
	// libx264
	switch speed {
	case "faster":
		return []string{"-preset", "superfast", "-crf", "23"}
	case "quality":
		return []string{"-preset", "fast", "-crf", "20"}
	}
	return []string{"-preset", "veryfast", "-crf", "21"}
}
