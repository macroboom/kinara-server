// Package probe reads technical details from media files with ffprobe.
package probe

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"kinora/internal/store"
)

// Result is what the server needs to know to decide how a file plays.
type Result struct {
	Container   string
	DurationSec float64
	Bitrate     int64
	Width       int
	Height      int
	VideoCodec  string
	// HDR is "", "hdr10", "hlg" or "dolby_vision".
	HDR       string
	DVProfile int
	// DV is the full Dolby Vision configuration (zero when not Dolby Vision).
	DV      DolbyVision
	Streams []store.Stream
}

// DolbyVision is a file's Dolby Vision configuration record.
type DolbyVision struct {
	Profile int `json:"profile"`
	Level   int `json:"level"`
	// BL, EL and RPU say which parts are present: the base layer every
	// player decodes, profile 7's enhancement layer, and the Dolby Vision
	// metadata.
	BL  bool `json:"bl"`
	EL  bool `json:"el"`
	RPU bool `json:"rpu"`
	// Compat is what the base layer is without Dolby Vision: 1 or 6 HDR10,
	// 2 SDR, 4 HLG, 0 nothing usable (profile 5). -1 when not stated.
	Compat int `json:"compat"`
}

// Present reports whether this is Dolby Vision at all.
func (d DolbyVision) Present() bool { return d.Profile > 0 || d.RPU }

// BaseTransfer is what a player without Dolby Vision gets from the base
// layer: "pq" (HDR10), "hlg", "sdr", or "" when the picture needs the
// Dolby Vision metadata to look right (profile 5).
func (d DolbyVision) BaseTransfer() string {
	if d.Profile == 5 {
		return ""
	}
	switch d.Compat {
	case 1, 6:
		return "pq"
	case 2, 5:
		return "sdr"
	case 3, 4:
		return "hlg"
	case 0:
		return ""
	}
	// Not stated: profiles 7 and 8 are HDR10 based in practice.
	return "pq"
}

// Prober runs ffprobe.
type Prober struct {
	// Binary is the ffprobe executable; "ffprobe" from PATH if empty.
	Binary  string
	Timeout time.Duration
}

// Available reports whether ffprobe can be run.
func (p *Prober) Available() error {
	_, err := exec.LookPath(p.binary())
	return err
}

func (p *Prober) binary() string {
	if p.Binary == "" {
		return "ffprobe"
	}
	return p.Binary
}

// Probe inspects one file.
func (p *Prober) Probe(ctx context.Context, path string) (Result, error) {
	timeout := p.Timeout
	if timeout == 0 {
		timeout = 60 * time.Second
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	// Fixed argument list; the path is passed as one argument, never through a shell.
	cmd := exec.CommandContext(ctx, p.binary(),
		"-v", "error", "-print_format", "json", "-show_format", "-show_streams", "--", path)
	out, err := cmd.Output()
	if err != nil {
		var ee *exec.ExitError
		if errors.As(err, &ee) && len(ee.Stderr) > 0 {
			return Result{}, fmt.Errorf("ffprobe: %s", strings.TrimSpace(string(ee.Stderr)))
		}
		return Result{}, fmt.Errorf("ffprobe: %w", err)
	}
	return Parse(out, filepath.Ext(path))
}

type sideData struct {
	Type      string `json:"side_data_type"`
	DVProfile int    `json:"dv_profile"`
	DVLevel   int    `json:"dv_level"`
	BL        int    `json:"bl_present_flag"`
	EL        int    `json:"el_present_flag"`
	RPU       int    `json:"rpu_present_flag"`
	// A pointer: 0 is a real value (profile 5), unlike a missing field.
	Compat *int `json:"dv_bl_signal_compatibility_id"`
}

func (sd sideData) dolbyVision() DolbyVision {
	d := DolbyVision{Profile: sd.DVProfile, Level: sd.DVLevel, BL: sd.BL != 0, EL: sd.EL != 0, RPU: sd.RPU != 0, Compat: -1}
	if sd.Compat != nil {
		d.Compat = *sd.Compat
	}
	return d
}

type ffOutput struct {
	Format struct {
		FormatName string `json:"format_name"`
		Duration   string `json:"duration"`
		BitRate    string `json:"bit_rate"`
	} `json:"format"`
	Streams []struct {
		Index         int    `json:"index"`
		CodecType     string `json:"codec_type"`
		CodecName     string `json:"codec_name"`
		Profile       string `json:"profile"`
		Width         int    `json:"width"`
		Height        int    `json:"height"`
		ColorTransfer string `json:"color_transfer"`
		Channels      int    `json:"channels"`
		Tags          struct {
			Language string `json:"language"`
			Title    string `json:"title"`
		} `json:"tags"`
		Disposition struct {
			Default     int `json:"default"`
			Forced      int `json:"forced"`
			AttachedPic int `json:"attached_pic"`
		} `json:"disposition"`
		SideData []sideData `json:"side_data_list"`
	} `json:"streams"`
}

// Parse reads ffprobe's JSON output. ext is the file extension, used to name
// the container precisely.
func Parse(data []byte, ext string) (Result, error) {
	var ff ffOutput
	if err := json.Unmarshal(data, &ff); err != nil {
		return Result{}, fmt.Errorf("ffprobe output: %w", err)
	}
	var r Result
	r.Container = containerName(ff.Format.FormatName, ext)
	r.DurationSec, _ = strconv.ParseFloat(ff.Format.Duration, 64)
	r.Bitrate, _ = strconv.ParseInt(ff.Format.BitRate, 10, 64)

	for _, s := range ff.Streams {
		if s.Disposition.AttachedPic != 0 {
			continue // cover art embedded as a video stream
		}
		switch s.CodecType {
		case "video", "audio", "subtitle":
		default:
			continue
		}
		r.Streams = append(r.Streams, store.Stream{
			Index:    s.Index,
			Type:     s.CodecType,
			Codec:    s.CodecName,
			Profile:  s.Profile,
			Language: s.Tags.Language,
			Title:    s.Tags.Title,
			Channels: s.Channels,
			Default:  s.Disposition.Default != 0,
			Forced:   s.Disposition.Forced != 0,
		})
		if s.CodecType != "video" || r.VideoCodec != "" {
			continue
		}
		// The first real video stream describes the picture.
		r.VideoCodec = s.CodecName
		r.Width, r.Height = s.Width, s.Height
		switch s.ColorTransfer {
		case "smpte2084":
			r.HDR = "hdr10"
		case "arib-std-b67":
			r.HDR = "hlg"
		}
		for _, sd := range s.SideData {
			if strings.Contains(strings.ToLower(sd.Type), "dovi") {
				r.HDR = "dolby_vision"
				r.DVProfile = sd.DVProfile
				r.DV = sd.dolbyVision()
			}
		}
	}
	if r.VideoCodec == "" {
		return r, errors.New("no video stream")
	}
	return r, nil
}

func containerName(formatName, ext string) string {
	ext = strings.TrimPrefix(strings.ToLower(ext), ".")
	switch {
	case strings.Contains(formatName, "matroska"):
		if ext == "webm" {
			return "webm"
		}
		return "mkv"
	case strings.Contains(formatName, "mp4"), strings.Contains(formatName, "mov"):
		switch ext {
		case "mov", "m4v":
			return ext
		}
		return "mp4"
	case formatName == "mpegts":
		return "ts"
	case formatName == "avi":
		return "avi"
	}
	if i := strings.IndexByte(formatName, ','); i > 0 {
		return formatName[:i]
	}
	return formatName
}

// DV reads just the first video stream's Dolby Vision configuration: a
// quick look for deciding how to stream a file (the library keeps only the
// profile). ffprobe "" means ffprobe from PATH.
func DV(ctx context.Context, ffprobe, path string) (DolbyVision, error) {
	if ffprobe == "" {
		ffprobe = "ffprobe"
	}
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, ffprobe, "-v", "error", "-select_streams", "v:0",
		"-show_entries", "stream_side_data", "-print_format", "json", "--", path)
	out, err := cmd.Output()
	if err != nil {
		return DolbyVision{}, fmt.Errorf("ffprobe: %w", err)
	}
	return parseDV(out)
}

func parseDV(out []byte) (DolbyVision, error) {
	var ff struct {
		Streams []struct {
			SideData []sideData `json:"side_data_list"`
		} `json:"streams"`
	}
	if err := json.Unmarshal(out, &ff); err != nil {
		return DolbyVision{}, fmt.Errorf("ffprobe output: %w", err)
	}
	for _, st := range ff.Streams {
		for _, sd := range st.SideData {
			if strings.Contains(strings.ToLower(sd.Type), "dovi") {
				return sd.dolbyVision(), nil
			}
		}
	}
	return DolbyVision{}, nil
}
