package transcode

import (
	"bytes"
	"context"
	"io"
	"log/slog"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"kinora/internal/ffmpeg"
)

func TestStagesFor(t *testing.T) {
	tr := &Transcoder{}
	nv := ffmpeg.Caps{Found: true, H264Encoder: "h264_nvenc", HW: ffmpeg.HWNVENC, Software: true}
	if got := tr.stagesFor(Request{Path: "x", Video: "h264"}, nv); len(got) != 3 || got[0] != StageGPU || got[2] != StageCPU {
		t.Errorf("nvenc: %v", got)
	}
	if got := tr.stagesFor(Request{Path: "x", Video: "copy"}, nv); len(got) != 1 || got[0] != "" {
		t.Errorf("copy: %v", got)
	}
	noX264 := nv
	noX264.Software = false
	if got := tr.stagesFor(Request{Path: "x", Video: "h264"}, noX264); len(got) != 2 {
		t.Errorf("without libx264: %v", got)
	}
	qsv := ffmpeg.Caps{Found: true, H264Encoder: "h264_qsv", HW: ffmpeg.HWQSV, Software: true}
	hi10 := Request{Path: "x", Video: "h264", SourceCodec: "h264", SourceProfile: "High 10"}
	if got := tr.stagesFor(hi10, qsv); got[0] != StageCPUDecode {
		t.Errorf("high 10 on quick sync starts at cpu decode: %v", got)
	}
	if got := tr.stagesFor(hi10, nv); got[0] != StageGPU {
		t.Errorf("nvenc keeps gpu first: %v", got)
	}
	sw := ffmpeg.Caps{Found: true, H264Encoder: "libx264", Software: true}
	if got := tr.stagesFor(Request{Path: "x", Video: "h264"}, sw); len(got) != 2 || got[1] != StageCPU {
		t.Errorf("software only: %v", got)
	}
}

func TestPlanVideoBackends(t *testing.T) {
	sdr := Request{Path: "x", Video: "h264", SourceCodec: "hevc", MaxHeight: 720}
	hdr := sdr
	hdr.HDR = "hdr10"
	vaapi := ffmpeg.Caps{Found: true, H264Encoder: "h264_vaapi", HW: ffmpeg.HWVAAPI, Software: true, ToneMap: true,
		HWInit: []string{"-init_hw_device", "vaapi=hw:/dev/dri/renderD128", "-filter_hw_device", "hw"}, VAAPIRateControl: "CBR"}

	p := planVideo(sdr, vaapi)
	if !has(p.input, "-hwaccel", "vaapi", "-hwaccel_output_format", "vaapi") || p.vf != "scale_vaapi=w=-2:h='min(ih,720)':format=nv12" {
		t.Errorf("vaapi sdr: %+v", p)
	}
	p = planVideo(hdr, vaapi)
	if has(p.input, "-hwaccel_output_format", "vaapi") || !strings.Contains(p.vf, "tonemap") || !strings.HasSuffix(p.vf, ",format=nv12,hwupload") {
		t.Errorf("vaapi hdr: %+v", p)
	}
	mixed := sdr
	mixed.Stage = StageCPUDecode
	p = planVideo(mixed, vaapi)
	if has(p.input, "-hwaccel", "vaapi") || !strings.HasSuffix(p.vf, "hwupload") || p.encoder != "h264_vaapi" {
		t.Errorf("vaapi cpu decode: %+v", p)
	}
	cpu := sdr
	cpu.Stage = StageCPU
	if p = planVideo(cpu, vaapi); p.encoder != "libx264" || len(p.input) != 0 || strings.Contains(p.vf, "hwupload") {
		t.Errorf("cpu: %+v", p)
	}
	a := videoEncoderArgs("h264_vaapi", 4000, vaapi)
	if !has(a, "-rc_mode", "CBR", "-b:v", "3600k", "-maxrate", "4000k") {
		t.Errorf("vaapi encoder %v", a)
	}
	if a := videoEncoderArgs("h264_qsv", 4000, vaapi); !has(a, "-forced_idr", "1") {
		t.Errorf("qsv encoder %v", a)
	}

	amf := ffmpeg.Caps{Found: true, H264Encoder: "h264_amf", HW: ffmpeg.HWAMF, Software: true}
	if p = planVideo(sdr, amf); !has(p.input, "-hwaccel", "d3d11va") || !strings.HasPrefix(p.vf, "scale=-2") {
		t.Errorf("amf: %+v", p)
	}
}

// A card that fails is worked around: the conversion falls back stage by
// stage until libx264 makes it, and the stage is remembered for the file.
func TestStreamFallsBackToCPU(t *testing.T) {
	if _, err := exec.LookPath("ffmpeg"); err != nil {
		t.Skip("ffmpeg not installed")
	}
	dir := t.TempDir()
	src := filepath.Join(dir, "in.mkv")
	gen := exec.Command("ffmpeg", "-hide_banner", "-loglevel", "error",
		"-f", "lavfi", "-i", "testsrc2=size=320x180:rate=24:duration=2", "-c:v", "libx264", src)
	if out, err := gen.CombinedOutput(); err != nil {
		t.Skipf("can't make clip: %v %s", err, out)
	}
	// Pretend this machine has a VAAPI card on a node that doesn't exist:
	// both card stages fail, the CPU stage works.
	caps := ffmpeg.Caps{Found: true, H264Encoder: "h264_vaapi", HW: ffmpeg.HWVAAPI, Software: true,
		HWInit: []string{"-init_hw_device", "vaapi=hw:/dev/dri/renderD999", "-filter_hw_device", "hw"}, VAAPIRateControl: "VBR"}
	tr := &Transcoder{Tool: ffmpeg.WithCaps("", caps), Log: slog.New(slog.NewTextHandler(io.Discard, nil))}
	var out bytes.Buffer
	r := Request{Path: src, Video: "h264", Audio: -1, AudioCodec: "copy", MaxHeight: 240}
	if err := tr.Stream(context.Background(), &out, r); err != nil {
		t.Fatal(err)
	}
	if out.Len() < 1000 {
		t.Fatalf("only %d bytes", out.Len())
	}
	if got := tr.preferredStage(r); got != StageCPU {
		t.Errorf("remembered %q", got)
	}
	if got := tr.stagesFor(r, caps); got[0] != StageCPU {
		t.Errorf("next time starts at %v", got)
	}

	// HLS does the same before its first segment.
	h := &HLS{T: tr, Dir: filepath.Join(dir, "hls")}
	defer h.Close()
	r2 := r
	r2.Path = src
	tr2 := &Transcoder{Tool: ffmpeg.WithCaps("", caps), Log: tr.Log}
	h.T = tr2
	if _, err := h.Start(context.Background(), r2); err != nil {
		t.Fatal(err)
	}
	if got := tr2.preferredStage(r2); got != StageCPU {
		t.Errorf("hls remembered %q", got)
	}
}

func TestHDRToCardTagsInChain(t *testing.T) {
	vaapi := ffmpeg.Caps{Found: true, H264Encoder: "h264_vaapi", HW: ffmpeg.HWVAAPI, Software: true, ToneMap: true, SetParams: true,
		HWInit: []string{"-init_hw_device", "vaapi=hw:/dev/dri/renderD128", "-filter_hw_device", "hw"}}
	r := Request{Path: "x.mkv", Video: "h264", AudioCodec: "copy", Audio: -1, HDR: "hdr10", SourceCodec: "hevc"}
	if err := r.Validate(); err != nil {
		t.Fatal(err)
	}
	a := Args(r, vaapi)
	vf := a[indexOf(a, "-vf")+1]
	if has(a, "-colorspace", "bt709") || !strings.Contains(vf, "setparams=range=tv:color_primaries=bt709") || !strings.HasSuffix(vf, "hwupload") {
		t.Errorf("gpu frames: %v", a)
	}
	r.Stage = StageCPU
	if a = Args(r, vaapi); !has(a, ffmpeg.SDROutputArgs...) {
		t.Errorf("cpu stage keeps output tags: %v", a)
	}
	if a := videoEncoderArgs("h264_vaapi", 4000, ffmpeg.Caps{}); indexOf(a, "-rc_mode") >= 0 {
		t.Errorf("no mode known: %v", a)
	}
}
