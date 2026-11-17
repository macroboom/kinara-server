package ffmpeg

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"time"
)

// Graphics card backends, the value of Caps.HW.
const (
	HWNone         = ""
	HWNVENC        = "nvenc"        // NVIDIA (Windows, Linux)
	HWQSV          = "qsv"          // Intel Quick Sync (Windows, Linux)
	HWVAAPI        = "vaapi"        // Intel or AMD on Linux
	HWAMF          = "amf"          // AMD on Windows
	HWVideoToolbox = "videotoolbox" // Apple
)

// hwChoice is a forced backend from KINARA_HWACCEL ("auto" or "" picks).
func (t *Tool) hwChoice() string {
	return strings.ToLower(strings.TrimSpace(t.HWAccel))
}

// detectHW finds the first graphics card encoder that encodes a test
// frame, in this order: NVIDIA, Intel Quick Sync, then VAAPI (Linux) or AMF
// (Windows), then Apple's. The tests are what Silo and Jellyfin run; an
// encoder only listed by ffmpeg isn't enough (it also needs the card and
// its driver).
func (t *Tool) detectHW(ctx context.Context, encoders string, c *Caps, choice string) {
	if choice == "none" || choice == "software" {
		return
	}
	want := func(hw string) bool { return choice == "" || choice == "auto" || choice == hw }
	listed := func(enc string) bool { return strings.Contains(encoders, " "+enc+" ") }

	if want(HWNVENC) && listed("h264_nvenc") && t.smoke(ctx, smokeInput("format=yuv420p", "h264_nvenc")...) {
		c.HW, c.H264Encoder = HWNVENC, "h264_nvenc"
		return
	}
	switch runtime.GOOS {
	case "windows":
		if want(HWQSV) && listed("h264_qsv") {
			// The Intel adapter by vendor ID, so a PC whose Intel graphics
			// are switched off fails here cleanly.
			init := []string{"-init_hw_device", "d3d11va=dx11:,vendor=0x8086", "-init_hw_device", "qsv=qs@dx11", "-filter_hw_device", "qs"}
			if t.smoke(ctx, append(init, smokeInput("format=nv12,hwupload=extra_hw_frames=64", "h264_qsv")...)...) {
				c.HW, c.H264Encoder, c.HWInit = HWQSV, "h264_qsv", init
				return
			}
		}
		if want(HWAMF) && listed("h264_amf") && t.smoke(ctx, smokeInput("format=nv12", "h264_amf")...) {
			c.HW, c.H264Encoder = HWAMF, "h264_amf"
			return
		}
	case "linux":
		nodes := renderNodes(t.Device)
		if want(HWQSV) && listed("h264_qsv") {
			for _, n := range nodes {
				if n.vendor != "0x8086" {
					continue
				}
				// Decoding runs on VAAPI and is handed to Quick Sync; filters
				// run on the VAAPI device. The test uses the same chain the
				// conversions do.
				init := []string{"-init_hw_device", "vaapi=va:" + n.path + ",driver=iHD", "-init_hw_device", "qsv=qs@va", "-filter_hw_device", "va"}
				if t.smoke(ctx, append(init, smokeInput("format=nv12,hwupload=extra_hw_frames=64,hwmap=derive_device=qsv,format=qsv", "h264_qsv")...)...) {
					c.HW, c.H264Encoder, c.HWDevice, c.HWInit = HWQSV, "h264_qsv", n.path, init
					return
				}
			}
		}
		if want(HWVAAPI) && listed("h264_vaapi") {
			for _, n := range nodes {
				if n.vendor == "0x10de" {
					continue // NVIDIA's VAAPI is a translation layer that doesn't encode
				}
				init := []string{"-init_hw_device", "vaapi=hw:" + n.path, "-filter_hw_device", "hw"}
				base := append(append([]string{}, init...), smokeInput("format=nv12,hwupload", "h264_vaapi")...)
				if !t.smoke(ctx, base...) {
					continue
				}
				// With a bitrate cap the rate control mode must be set:
				// left to itself the driver may pick one that ignores the
				// cap. Variable first, constant if the driver lacks it.
				c.HW, c.H264Encoder, c.HWDevice, c.HWInit = HWVAAPI, "h264_vaapi", n.path, init
				for _, mode := range []string{"VBR", "CBR"} {
					if t.smoke(ctx, append(base[:len(base)-3:len(base)-3], "-rc_mode", mode, "-b:v", "1800k", "-maxrate", "2000k", "-f", "null", "-")...) {
						c.VAAPIRateControl = mode
						break
					}
				}
				// Neither: the driver's own mode (it may not keep to the cap).
				return
			}
		}
	case "darwin":
		if want(HWVideoToolbox) && listed("h264_videotoolbox") && t.smoke(ctx, smokeInput("format=yuv420p", "h264_videotoolbox")...) {
			c.HW, c.H264Encoder = HWVideoToolbox, "h264_videotoolbox"
			return
		}
	}
}

// smokeInput is one generated frame encoded with enc and thrown away.
func smokeInput(vf, enc string) []string {
	return []string{"-f", "lavfi", "-i", "testsrc2=size=640x360:rate=1", "-vf", vf, "-frames:v", "1", "-an", "-c:v", enc, "-f", "null", "-"}
}

// smoke runs ffmpeg with args and reports whether it succeeded.
func (t *Tool) smoke(ctx context.Context, args ...string) bool {
	// A driver that hangs counts as not working.
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	full := append([]string{"-hide_banner", "-loglevel", "error", "-nostdin"}, args...)
	cmd := exec.CommandContext(ctx, t.Path(), full...)
	HideWindow(cmd)
	return cmd.Run() == nil
}

// RenderNode is a Linux graphics card render node.
type RenderNode struct {
	Path string `json:"path"`
	// Vendor is the card's maker: Intel, AMD, NVIDIA, or its PCI ID.
	Vendor string `json:"vendor"`
}

// RenderNodes lists the Linux render nodes this process can open.
func RenderNodes() []RenderNode {
	var out []RenderNode
	for _, n := range renderNodes("") {
		v := n.vendor
		switch v {
		case "0x8086":
			v = "Intel"
		case "0x1002":
			v = "AMD"
		case "0x10de":
			v = "NVIDIA"
		}
		out = append(out, RenderNode{Path: n.path, Vendor: v})
	}
	return out
}

type renderNode struct {
	path   string
	vendor string // PCI vendor ID: 0x8086 Intel, 0x1002 AMD, 0x10de NVIDIA
}

// renderNodes lists the Linux GPU render nodes this process can open,
// with their vendors (or just dev, when KINARA_HW_DEVICE names one).
func renderNodes(dev string) []renderNode {
	paths := []string{dev}
	if dev == "" {
		paths, _ = filepath.Glob("/dev/dri/renderD*")
		sort.Strings(paths)
	}
	var out []renderNode
	for _, p := range paths {
		f, err := os.Open(p)
		if err != nil {
			continue // no permission: the container lacks the device or the group
		}
		f.Close()
		v, _ := os.ReadFile(filepath.Join("/sys/class/drm", filepath.Base(p), "device", "vendor"))
		out = append(out, renderNode{path: p, vendor: strings.TrimSpace(string(v))})
	}
	return out
}

// WithCaps is a Tool whose capabilities are given rather than detected
// (tests, and checking how a conversion would run on other hardware).
func WithCaps(binary string, c Caps) *Tool {
	return &Tool{Binary: binary, detected: true, caps: c}
}

// Backend is one graphics card encoder this system might have.
type Backend struct {
	// ID is the KINARA_HWACCEL value: nvenc, qsv, vaapi, amf, videotoolbox.
	ID    string `json:"id"`
	Label string `json:"label"`
	// Listed: this ffmpeg was built with the encoder.
	Listed bool `json:"listed"`
	// Works: it encoded a test frame here (the card and its driver work).
	Works bool `json:"works"`
	// Device is the Linux render node it worked on.
	Device string `json:"device,omitempty"`
}

// Backends lists the graphics card encoders for this operating system.
func Backends() []Backend {
	nv := Backend{ID: HWNVENC, Label: "NVIDIA (NVENC)"}
	qsv := Backend{ID: HWQSV, Label: "Intel Quick Sync"}
	switch runtime.GOOS {
	case "windows":
		return []Backend{nv, qsv, {ID: HWAMF, Label: "AMD (AMF)"}}
	case "linux":
		return []Backend{nv, qsv, {ID: HWVAAPI, Label: "VAAPI (Intel or AMD)"}}
	case "darwin":
		return []Backend{{ID: HWVideoToolbox, Label: "Apple VideoToolbox"}}
	}
	return nil
}

// Survey tries every graphics card encoder for this system with a test
// frame, whatever is chosen now: what the owner can pick from. It takes a
// few seconds per card.
func (t *Tool) Survey(ctx context.Context) []Backend {
	out := Backends()
	if t.Available() != nil {
		return out
	}
	listCmd := exec.CommandContext(ctx, t.Path(), "-hide_banner", "-encoders")
	HideWindow(listCmd)
	list, _ := listCmd.Output()
	encoders := string(list)
	_, device := t.Choice()
	for i, b := range out {
		out[i].Listed = strings.Contains(encoders, " h264_"+b.ID+" ")
		if !out[i].Listed {
			continue
		}
		probe := &Tool{Binary: t.Binary, Device: device}
		var c Caps
		probe.detectHW(ctx, encoders, &c, b.ID)
		out[i].Works, out[i].Device = c.HW == b.ID, c.HWDevice
	}
	return out
}
