// Package ffmpeg finds ffmpeg, checks what it can do (hardware encoders,
// tone mapping), and runs it at a priority that doesn't disturb playback.
package ffmpeg

import (
	"context"
	"fmt"
	"log/slog"
	"os/exec"
	"strings"
	"sync"
	"time"
)

// Tool is one ffmpeg install.
type Tool struct {
	// Binary is the ffmpeg executable; "ffmpeg" from PATH if empty.
	Binary string
	// HWAccel forces a graphics card backend (KINORA_HWACCEL): "nvenc",
	// "qsv", "vaapi", "amf", "videotoolbox", or "none" for the CPU only.
	// "" or "auto" uses the first that works.
	HWAccel string
	// Device is the Linux render node to use (KINORA_HW_DEVICE, e.g.
	// /dev/dri/renderD129); "" tries each.
	Device string
	Log    *slog.Logger

	once sync.Once
	caps Caps
}

// Caps is what this ffmpeg can do.
type Caps struct {
	Found bool `json:"found"`
	// H264Encoder is the best working H.264 encoder: a hardware one
	// (h264_nvenc, h264_qsv, h264_vaapi, h264_amf, h264_videotoolbox) or
	// libx264.
	H264Encoder string `json:"h264Encoder"`
	// HW is the graphics card backend of H264Encoder (HWNVENC and the
	// others in hw.go), "" for libx264.
	HW string `json:"hw"`
	// HWInit are the arguments that open the card for Quick Sync and VAAPI
	// (and name the filter device).
	HWInit []string `json:"-"`
	// HWDevice is the Linux render node in use, for the log.
	HWDevice string `json:"hwDevice,omitempty"`
	// VAAPIRateControl is the rate control mode the VAAPI driver accepted
	// with a bitrate cap ("VBR" or "CBR").
	VAAPIRateControl string `json:"vaapiRateControl,omitempty"`
	// Software is true when libx264 works, the last resort when the
	// graphics card fails on a file.
	Software bool `json:"software"`
	// ToneMap is true when the zscale and tonemap filters exist, so HDR can
	// be converted to SDR when re-encoding.
	ToneMap bool `json:"toneMap"`
	// Placebo is true when the libplacebo filter works here (it needs
	// Vulkan). It tone maps better than zscale, and it is the only way to
	// get right colours from Dolby Vision profile 5, which zscale turns
	// pink and green.
	Placebo bool `json:"placebo"`
	// CUDA is true when an NVIDIA card can decode, scale and encode without
	// the picture leaving the GPU (checked with a test clip). Otherwise the
	// GPU only encodes, and frames are scaled on the CPU in between.
	CUDA bool `json:"cuda"`
	// DoviStrip is true when the dovi_rpu and filter_units bitstream
	// filters exist (FFmpeg 7.1 and later): copied Dolby Vision can then be
	// turned into plain HDR10 without re-encoding.
	DoviStrip bool `json:"doviStrip"`
	// SideData is a filter chain that deletes HDR metadata left on frames
	// after tone mapping (so players don't treat SDR as HDR), or "" when
	// this ffmpeg's sidedata filter can't (checked with a test clip).
	SideData string `json:"sideData,omitempty"`
	// SetParams is true when the setparams filter can declare colour
	// properties, so the zscale tone map reads mislabelled sources right.
	SetParams bool `json:"setParams"`
}

// HDRToSDR is the filter that converts HDR (and Dolby Vision) to SDR
// BT.709, or "" when this ffmpeg can't.
func (c Caps) HDRToSDR() string {
	switch {
	case c.Placebo:
		return PlaceboFilter
	case c.ToneMap:
		return ToneMapFilter
	}
	return ""
}

// PlaceboFilter: libplacebo applies Dolby Vision's own reshaping first,
// then tone maps to SDR, and hands back ordinary frames.
const PlaceboFilter = "libplacebo=colorspace=bt709:color_primaries=bt709:color_trc=bt709:range=tv:" +
	"tonemapping=bt.2390:apply_dolbyvision=1:format=yuv420p"

// Path returns the executable to run.
func (t *Tool) Path() string {
	if t == nil || t.Binary == "" {
		return "ffmpeg"
	}
	return t.Binary
}

// Available reports whether ffmpeg can be run.
func (t *Tool) Available() error {
	_, err := exec.LookPath(t.Path())
	return err
}

// Caps checks the encoders and filters once (the first call takes a few
// seconds while hardware encoders are tried) and remembers the answer.
func (t *Tool) Caps() Caps {
	t.once.Do(func() { t.caps = t.detect() })
	return t.caps
}

func (t *Tool) detect() Caps {
	var c Caps
	if t.Available() != nil {
		return c
	}
	c.Found = true
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()

	listCmd := exec.CommandContext(ctx, t.Path(), "-hide_banner", "-encoders")
	HideWindow(listCmd)
	list, _ := listCmd.Output()
	encoders := string(list)
	t.detectHW(ctx, encoders, &c)
	c.Software = strings.Contains(encoders, " libx264 ")
	if c.H264Encoder == "" && c.Software {
		c.H264Encoder = "libx264"
	}
	if c.H264Encoder == "h264_nvenc" {
		// Upload a test clip, scale it on the card and encode it there: the
		// same filters a real stream uses.
		test := exec.CommandContext(ctx, t.Path(), "-hide_banner", "-loglevel", "error", "-nostdin",
			"-init_hw_device", "cuda=gpu", "-filter_hw_device", "gpu",
			"-f", "lavfi", "-i", "testsrc2=size=1280x720:rate=24:duration=1",
			"-vf", "format=nv12,hwupload_cuda,"+CUDAScale(480), "-c:v", "h264_nvenc", "-f", "null", "-")
		HideWindow(test)
		c.CUDA = test.Run() == nil
	}
	filterCmd := exec.CommandContext(ctx, t.Path(), "-hide_banner", "-filters")
	HideWindow(filterCmd)
	filters, _ := filterCmd.Output()
	f := string(filters)
	c.ToneMap = strings.Contains(f, " zscale ") && strings.Contains(f, " tonemap ")
	if strings.Contains(f, " libplacebo ") {
		// Listed isn't enough: it needs a working Vulkan driver.
		test := exec.CommandContext(ctx, t.Path(), "-hide_banner", "-loglevel", "error", "-nostdin",
			"-f", "lavfi", "-i", "testsrc2=size=320x180:rate=1:duration=1",
			"-vf", "format=yuv420p10le,"+PlaceboFilter, "-f", "null", "-")
		HideWindow(test)
		c.Placebo = test.Run() == nil
	}
	c.SetParams = strings.Contains(f, " setparams ")
	if strings.Contains(f, " sidedata ") {
		// Newer ffmpeg knows the Dolby Vision side data types too; older ones
		// reject the whole filter over an unknown type, so try both.
		for _, chain := range []string{SideDataAll, SideDataHDR10} {
			test := exec.CommandContext(ctx, t.Path(), "-hide_banner", "-loglevel", "error", "-nostdin",
				"-f", "lavfi", "-i", "testsrc2=size=320x180:rate=1:duration=1",
				"-vf", "format=yuv420p,"+chain, "-f", "null", "-")
			HideWindow(test)
			if test.Run() == nil {
				c.SideData = chain
				break
			}
		}
	}
	bsfCmd := exec.CommandContext(ctx, t.Path(), "-hide_banner", "-bsfs")
	HideWindow(bsfCmd)
	bsfs, _ := bsfCmd.Output()
	c.DoviStrip = hasLine(string(bsfs), "dovi_rpu") && hasLine(string(bsfs), "filter_units")
	if t.Log != nil {
		t.Log.Info("ffmpeg checked", "h264_encoder", c.H264Encoder, "gpu", c.HW, "gpu_device", c.HWDevice,
			"vaapi_rate_control", c.VAAPIRateControl, "gpu_pipeline", c.CUDA, "hdr_tone_mapping", c.ToneMap,
			"libplacebo", c.Placebo, "dolby_vision_strip", c.DoviStrip, "hdr_metadata_cleanup", c.SideData != "")
	}
	return c
}

// hasLine reports whether a list printed one name per line contains name.
func hasLine(list, name string) bool {
	for _, l := range strings.Split(list, "\n") {
		if strings.TrimSpace(l) == name {
			return true
		}
	}
	return false
}

// SideDataHDR10 deletes HDR10's static metadata from frames.
const SideDataHDR10 = "sidedata=mode=delete:type=MASTERING_DISPLAY_METADATA,sidedata=mode=delete:type=CONTENT_LIGHT_LEVEL"

// SideDataAll also deletes HDR10+ and Dolby Vision metadata.
const SideDataAll = SideDataHDR10 + ",sidedata=mode=delete:type=DYNAMIC_HDR_PLUS," +
	"sidedata=mode=delete:type=DOVI_RPU_BUFFER,sidedata=mode=delete:type=DOVI_METADATA"

// DoviStripBSF turns Dolby Vision (profile 7, or 8.1) into its HDR10 base
// layer while copying: dovi_rpu drops the Dolby Vision metadata and
// filter_units the profile 7 enhancement layer (NAL type 63).
const DoviStripBSF = "dovi_rpu=strip=1,filter_units=remove_types=63"

// CUDAScale scales frames already on an NVIDIA card down to a height (never
// up) and to 8-bit 4:2:0, which NVENC's H.264 encoder needs.
func CUDAScale(height int) string {
	return fmt.Sprintf("scale_cuda=w=-2:h='min(ih,%d)':format=yuv420p", height)
}

// NVDEC lists the source codecs NVIDIA cards decode (RTX 20 series and
// later; AV1 from the 30 series). Others decode on the CPU.
var NVDEC = map[string]bool{
	"h264": true, "hevc": true, "av1": true, "vp9": true, "vp8": true,
	"mpeg2video": true, "mpeg1video": true, "vc1": true, "mpeg4": true,
}

// ToneMapFilter converts HDR (PQ or HLG) to SDR BT.709 for players that
// can't show HDR. It needs the zscale filter (ffmpeg built with zimg).
const ToneMapFilter = "zscale=t=linear:npl=100,format=gbrpf32le,zscale=p=bt709," +
	"tonemap=tonemap=hable:desat=0,zscale=t=bt709:m=bt709:r=tv,format=yuv420p"

// SourceParams declares what the source is before the zscale tone map, so a
// file with missing or wrong colour tags still converts right. transfer is
// "pq" or "hlg".
func SourceParams(transfer string) string {
	trc := "smpte2084"
	if transfer == "hlg" {
		trc = "arib-std-b67"
	}
	return "setparams=range=tv:color_primaries=bt2020:color_trc=" + trc + ":colorspace=bt2020nc"
}

// SDROutputArgs tag a tone mapped stream as SDR BT.709, so players don't
// guess from leftovers of the source.
var SDROutputArgs = []string{"-color_range", "tv", "-color_primaries", "bt709", "-color_trc", "bt709", "-colorspace", "bt709"}

// Tail keeps the last n bytes written to it, for error messages.
type Tail struct {
	N   int
	buf []byte
}

func (t *Tail) Write(p []byte) (int, error) {
	t.buf = append(t.buf, p...)
	if n := t.N; n > 0 && len(t.buf) > n {
		t.buf = t.buf[len(t.buf)-n:]
	}
	return len(p), nil
}

func (t *Tail) String() string { return strings.TrimSpace(string(t.buf)) }
