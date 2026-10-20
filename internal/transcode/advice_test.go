package transcode

import (
	"io"
	"log/slog"
	"strings"
	"testing"
	"time"

	"kinara/internal/ffmpeg"
)

func TestParseStatTime(t *testing.T) {
	for line, want := range map[string]float64{
		"frame= 100 fps=24 q=28.0 size= 1024kB time=00:01:23.45 bitrate=100kbits/s speed=1.2x": 83.45,
		"size=N/A time=01:00:00.00 bitrate=N/A speed=0.9x":                                     3600,
	} {
		got, ok := parseStatTime(line)
		if !ok || got != want {
			t.Errorf("%q = %v %v, want %v", line, got, ok, want)
		}
	}
	for _, line := range []string{"time=N/A speed=N/A", "time=-00:00:00.02 speed=0x"} {
		if _, ok := parseStatTime(line); ok {
			t.Errorf("%q should not parse", line)
		}
	}
}

// live builds a stream that has run for age, making media at rate, and
// waiting on the player for the given share of the time.
func live(owner Owner, video string, bitrate, height int, age time.Duration, rate, waiting float64) *Live {
	now := time.Now()
	l := &Live{ID: owner.Device + video, Kind: "pipe", Owner: owner, Video: video, Bitrate: bitrate, Height: height, StartedAt: now.Add(-age)}
	for s := 0; s <= 20; s += 5 {
		at := now.Add(-time.Duration(20-s) * time.Second)
		l.blocked = time.Duration(float64(s) * waiting * float64(time.Second))
		l.samples = append(l.samples, sample{at: at, media: float64(s) * rate, blocked: l.blocked})
	}
	return l
}

func TestBehind(t *testing.T) {
	now := time.Now()
	if in := live(Owner{}, "h264", 8000, 1080, time.Minute, 0.7, 0).info(now); !in.Behind || in.Rate < 0.69 || in.Rate > 0.71 {
		t.Errorf("slow conversion: %+v", in)
	}
	if in := live(Owner{}, "h264", 8000, 1080, time.Minute, 0.7, 0.6).info(now); in.Behind || !in.PlayerLimited {
		t.Errorf("waiting on the player isn't the server's fault: %+v", in)
	}
	if in := live(Owner{}, "h264", 8000, 1080, 10*time.Second, 0.5, 0).info(now); in.Behind {
		t.Errorf("judged during warmup: %+v", in)
	}
	if in := live(Owner{}, "h264", 8000, 1080, time.Minute, 1.6, 0).info(now); in.Behind {
		t.Errorf("fast conversion: %+v", in)
	}
}

func TestNextRung(t *testing.T) {
	for _, c := range []struct{ bitrate, height, want, wantH int }{
		{0, 0, 12000, 1080},
		{20000, 2160, 12000, 1080},
		{8000, 1080, 5000, 1080},
		{5000, 1080, 3000, 720},
		{10000, 720, 3000, 720},
	} {
		r, ok := NextRung(c.bitrate, c.height)
		if !ok || r.Bitrate != c.want || r.Height != c.wantH {
			t.Errorf("NextRung(%d, %d) = %+v %v, want %d/%d", c.bitrate, c.height, r, ok, c.want, c.wantH)
		}
	}
	if _, ok := NextRung(800, 360); ok {
		t.Error("nothing below the bottom")
	}
}

func testTranscoder(ls ...*Live) *Transcoder {
	t := &Transcoder{Log: slog.New(slog.NewTextHandler(io.Discard, nil))}
	t.reg.live = map[string]*Live{}
	for _, l := range ls {
		t.reg.live[l.ID] = l
	}
	return t
}

func TestAdvise(t *testing.T) {
	tv := Owner{Device: "tv", VersionID: "v1", Priority: true}
	phone := Owner{Device: "phone", VersionID: "v2"}
	laptop := Owner{Device: "laptop", VersionID: "v3"}

	// One slow stream: it steps down.
	tr := testTranscoder(live(phone, "h264", 8000, 1080, time.Minute, 0.6, 0))
	if a := tr.Advise("phone", "v2"); a.Action != "lower" || a.Next == nil || a.Next.Bitrate != 5000 {
		t.Fatalf("slow stream: %+v", a)
	}
	// Nothing running for that device: carry on.
	if a := tr.Advise("tv", "v1"); a.Action != "ok" || a.Stream != nil {
		t.Fatalf("no stream: %+v", a)
	}

	// The priority TV falls behind: it holds its quality, and the others
	// step down for it, even though they keep up themselves.
	tr = testTranscoder(
		live(tv, "h264", 20000, 2160, time.Minute, 0.8, 0),
		live(phone, "h264", 8000, 1080, time.Minute, 1.2, 0),
		live(laptop, "h264", 5000, 1080, time.Minute, 1.1, 0),
	)
	if a := tr.Advise("tv", "v1"); a.Action != "ok" || a.Reason == "" {
		t.Fatalf("priority should hold: %+v", a)
	}
	for _, d := range []string{"phone", "laptop"} {
		v := map[string]string{"phone": "v2", "laptop": "v3"}[d]
		if a := tr.Advise(d, v); a.Action != "lower" {
			t.Fatalf("%s should make room: %+v", d, a)
		}
	}

	// With nobody else to give way, the priority device steps down too.
	tr = testTranscoder(live(tv, "h264", 20000, 2160, time.Minute, 0.8, 0))
	if a := tr.Advise("tv", "v1"); a.Action != "lower" {
		t.Fatalf("priority alone: %+v", a)
	}

	// A stream that keeps the original picture and still can't keep up is
	// told to switch to a converted 1080p.
	tr = testTranscoder(live(phone, "copy", 0, 0, time.Minute, 0.5, 0))
	if a := tr.Advise("phone", "v2"); a.Action != "lower" || a.Next.Bitrate != 12000 {
		t.Fatalf("copy behind: %+v", a)
	}
}

// With an NVIDIA card, SDR re-encodes stay on the card from decode to
// encode; HDR decodes there but tone maps in system memory; a codec the
// card can't decode, or a retry after a GPU failure, goes the CPU way.
func TestGPUPipelineArgs(t *testing.T) {
	caps := ffmpegCaps(true)
	has := func(args []string, want ...string) bool {
		joined := " " + strings.Join(args, " ") + " "
		for _, w := range want {
			if !strings.Contains(joined, " "+w+" ") {
				return false
			}
		}
		return true
	}
	sdr := Request{Path: "/m.mkv", Video: "h264", AudioCodec: "copy", Audio: -1, SourceCodec: "hevc", MaxBitrate: 8000, MaxHeight: 1080}
	_ = sdr.Validate()
	a := Args(sdr, caps)
	if !has(a, "-hwaccel cuda -hwaccel_output_format cuda", "scale_cuda="+fit(1080)+":format=yuv420p", "-c:v h264_nvenc") {
		t.Fatalf("sdr: %v", a)
	}
	hdr := sdr
	hdr.HDR = "hdr10"
	a = Args(hdr, caps)
	if !has(a, "-hwaccel cuda", "-c:v h264_nvenc") || has(a, "-hwaccel_output_format cuda") || strings.Contains(strings.Join(a, " "), "scale_cuda") {
		t.Fatalf("hdr: %v", a)
	}
	odd := sdr
	odd.SourceCodec = "prores"
	if a = Args(odd, caps); !has(a, "-hwaccel auto") || strings.Contains(strings.Join(a, " "), "cuda") {
		t.Fatalf("codec the card can't decode: %v", a)
	}
	retry := sdr
	retry.SoftwareDecode = true
	if a = Args(retry, caps); strings.Contains(strings.Join(a, " "), "cuda") {
		t.Fatalf("software retry: %v", a)
	}
	copyVideo := sdr
	copyVideo.Video = "copy"
	if a = Args(copyVideo, caps); strings.Contains(strings.Join(a, " "), "hwaccel") {
		t.Fatalf("copy needs no decoding: %v", a)
	}
	if a = Args(sdr, ffmpegCaps(false)); strings.Contains(strings.Join(a, " "), "cuda") {
		t.Fatalf("no cuda: %v", a)
	}
}

func ffmpegCaps(cuda bool) ffmpeg.Caps {
	return ffmpeg.Caps{Found: true, H264Encoder: "h264_nvenc", HW: ffmpeg.HWNVENC, CUDA: cuda, ToneMap: true}
}
