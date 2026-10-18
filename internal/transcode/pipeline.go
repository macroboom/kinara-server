package transcode

import (
	"fmt"
	"runtime"
	"strings"
	"sync"
	"time"

	"kinora/internal/ffmpeg"
)

// Where a conversion's video work happens. Each stage is slower but more
// likely to work than the one before; a conversion that fails before
// anything reached the player is tried again at the next stage (what Silo
// and Jellyfin do):
//
//	StageGPU       decode, scale and encode on the graphics card (frames
//	               stay there when the backend allows it)
//	StageCPUDecode decode and scale on the CPU, encode on the card
//	StageCPU       everything on the CPU (libx264)
//
// The stage that worked for a file is remembered for 15 minutes, so the
// next play of it (or the next seek) starts there.
type Stage string

const (
	StageGPU       Stage = "gpu"
	StageCPUDecode Stage = "cpu-decode"
	StageCPU       Stage = "cpu"
)

// stagesFor lists the stages to try for r, best first.
func (t *Transcoder) stagesFor(r Request, caps ffmpeg.Caps) []Stage {
	if r.Video != "h264" {
		return []Stage{""} // copying video: no stages
	}
	var all []Stage
	if caps.HW == ffmpeg.HWNone {
		all = []Stage{StageGPU, StageCPU} // GPU here is "-hwaccel auto" decoding only
	} else {
		all = []Stage{StageGPU, StageCPUDecode}
		if caps.Software {
			all = append(all, StageCPU)
		}
	}
	start := StageGPU
	if r.SoftwareDecode || needsCPUDecode(r, caps) {
		start = StageCPUDecode
	}
	if s := t.preferredStage(r); s != "" {
		start = s
	}
	for i, s := range all {
		if s == start {
			return all[i:]
		}
	}
	return all
}

// needsCPUDecode: sources the card's decoder can't take. Intel's and AMD's
// decoders have no H.264 High 10 (and the older MPEG-4 Part 2 fails often).
func needsCPUDecode(r Request, caps ffmpeg.Caps) bool {
	if caps.HW != ffmpeg.HWQSV && caps.HW != ffmpeg.HWVAAPI {
		return false
	}
	codec := strings.ToLower(r.SourceCodec)
	p := strings.NewReplacer(" ", "", "-", "", "_", "").Replace(strings.ToLower(r.SourceProfile))
	if codec == "h264" && (p == "high10" || p == "high10intra" || p == "hi10p") {
		return true
	}
	return codec == "mpeg4" || codec == "msmpeg4v3"
}

// videoPlan is how a conversion's video is decoded, filtered and encoded.
type videoPlan struct {
	input   []string // before -i: devices and decoding
	vf      string
	encoder string
	// gpuFrames: the encoder gets frames in graphics card memory, where
	// output colour options (-colorspace) would ask ffmpeg for a
	// conversion it can't do; the colours are tagged in the chain instead.
	gpuFrames bool
}

// planVideo builds the video side of a conversion for r.Stage.
func planVideo(r Request, caps ffmpeg.Caps) videoPlan {
	height := r.MaxHeight
	if height <= 0 {
		height = 1080
	}
	stage := r.Stage
	if stage == "" {
		stage = StageGPU
		if r.SoftwareDecode {
			stage = StageCPUDecode
		}
	}
	cpu := cpuFilter(r, caps, height)
	// CPU frames going up to the card: tag tone mapped ones as SDR first.
	up := cpu
	if toneMapped(r, caps) && caps.SetParams {
		up += ",setparams=range=tv:color_primaries=bt709:color_trc=bt709:colorspace=bt709"
	}

	if stage == StageCPU || caps.HW == ffmpeg.HWNone {
		p := videoPlan{vf: cpu, encoder: "libx264"}
		if caps.HW == ffmpeg.HWNone {
			p.encoder = caps.H264Encoder
			if stage == StageGPU {
				// Decode on whatever graphics card there is; ffmpeg falls
				// back to the CPU by itself.
				p.input = []string{"-hwaccel", "auto"}
			}
		}
		return p
	}

	enc := caps.H264Encoder
	sdr := r.HDR == ""
	switch caps.HW {
	case ffmpeg.HWNVENC:
		p := videoPlan{vf: cpu, encoder: enc}
		gpuDecode, onGPU := gpuPipeline(r, caps)
		switch {
		case onGPU:
			// Frames stay on the card from decoder to encoder.
			p.input = []string{"-hwaccel", "cuda", "-hwaccel_output_format", "cuda"}
			p.vf = ffmpeg.CUDAScale(height)
		case gpuDecode:
			p.input = []string{"-hwaccel", "cuda"}
		case stage == StageGPU:
			p.input = []string{"-hwaccel", "auto"}
		}
		return p

	case ffmpeg.HWQSV:
		upload := up + ",format=nv12,hwupload=extra_hw_frames=64,format=qsv"
		p := videoPlan{input: append([]string{}, caps.HWInit...), vf: upload, encoder: enc, gpuFrames: true}
		if runtime.GOOS == "windows" {
			// Windows opens the Quick Sync device as the filter device.
			if stage == StageGPU {
				if sdr {
					p.input = append(p.input, "-hwaccel", "d3d11va", "-hwaccel_output_format", "d3d11", "-noautorotate")
					fw, fh := ffmpeg.Fit(height)
					p.vf = fmt.Sprintf("hwmap=derive_device=qsv,format=qsv,scale_qsv=w=%s:h=%s:format=nv12", fw, fh)
				} else {
					// HDR is tone mapped on the CPU; decoding stays on the card.
					p.input = append(p.input, "-hwaccel", "d3d11va")
				}
			}
			return p
		}
		// Linux: decoding runs on VAAPI and is mapped to Quick Sync.
		if stage == StageGPU {
			if sdr {
				p.input = append(p.input, "-hwaccel", "vaapi", "-hwaccel_output_format", "vaapi", "-noautorotate")
				fw, fh := ffmpeg.Fit(height)
				p.vf = fmt.Sprintf("scale_vaapi=w=%s:h=%s:format=nv12,hwmap=derive_device=qsv,format=qsv", fw, fh)
			} else {
				p.input = append(p.input, "-hwaccel", "vaapi")
			}
		}
		// Software frames: scaled on the CPU first, then uploaded to VAAPI
		// and mapped to Quick Sync (uploading straight to Quick Sync needs
		// the QSV device as the filter device, which this init doesn't name).
		if p.vf == upload {
			p.vf = up + ",format=nv12,hwupload=extra_hw_frames=64,hwmap=derive_device=qsv,format=qsv"
		}
		return p

	case ffmpeg.HWVAAPI:
		p := videoPlan{input: append([]string{}, caps.HWInit...), vf: up + ",format=nv12,hwupload", encoder: enc, gpuFrames: true}
		if stage == StageGPU {
			if sdr {
				p.input = append(p.input, "-hwaccel", "vaapi", "-hwaccel_output_format", "vaapi", "-noautorotate")
				fw, fh := ffmpeg.Fit(height)
				p.vf = fmt.Sprintf("scale_vaapi=w=%s:h=%s:format=nv12", fw, fh)
			} else {
				p.input = append(p.input, "-hwaccel", "vaapi")
			}
		}
		return p

	case ffmpeg.HWAMF:
		// AMF takes frames from system memory; the card decodes and the
		// frames come back for scaling and tone mapping.
		p := videoPlan{vf: cpu, encoder: enc}
		if stage == StageGPU {
			p.input = []string{"-hwaccel", "d3d11va"}
		}
		return p

	case ffmpeg.HWVideoToolbox:
		p := videoPlan{vf: cpu, encoder: enc}
		if stage == StageGPU {
			p.input = []string{"-hwaccel", "videotoolbox"}
		}
		return p
	}
	return videoPlan{vf: cpu, encoder: enc}
}

// --- Remembering what worked ---------------------------------------------------------

const stageMemory = 15 * time.Minute

type stagePref struct {
	stage Stage
	until time.Time
}

type stageMemo struct {
	mu sync.Mutex
	m  map[string]stagePref
}

func (t *Transcoder) stages() *stageMemo {
	t.stageOnce.Do(func() { t.stageMem = &stageMemo{m: map[string]stagePref{}} })
	return t.stageMem
}

// stageKey: the same file converted the same way. One file that fails on
// the card never moves other files off it.
func stageKey(r Request) string {
	k, ok := keyFor(r.Path)
	if !ok {
		return ""
	}
	return fmt.Sprintf("%s\x00%d\x00%d\x00%s\x00%d", k.path, k.size, k.mtime, r.HDR, r.MaxHeight)
}

func (t *Transcoder) preferredStage(r Request) Stage {
	key := stageKey(r)
	if key == "" {
		return ""
	}
	m := t.stages()
	m.mu.Lock()
	defer m.mu.Unlock()
	p, ok := m.m[key]
	if !ok || time.Now().After(p.until) {
		delete(m.m, key)
		return ""
	}
	return p.stage
}

// rememberStage notes the stage that worked after a fallback (or forgets,
// when the first choice worked). Starting at a remembered stage doesn't
// extend it, so a card that recovers is tried again after a while.
func (t *Transcoder) rememberStage(r Request, worked Stage, fellBack bool) {
	key := stageKey(r)
	if key == "" {
		return
	}
	m := t.stages()
	m.mu.Lock()
	defer m.mu.Unlock()
	switch {
	case worked == StageGPU:
		delete(m.m, key)
	case fellBack:
		if len(m.m) >= 256 {
			// Full: drop the one closest to expiring.
			var oldest string
			var at time.Time
			for k, p := range m.m {
				if oldest == "" || p.until.Before(at) {
					oldest, at = k, p.until
				}
			}
			delete(m.m, oldest)
		}
		m.m[key] = stagePref{stage: worked, until: time.Now().Add(stageMemory)}
	}
}
