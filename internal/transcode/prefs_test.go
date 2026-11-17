package transcode

import (
	"slices"
	"strings"
	"testing"

	"kinara/internal/ffmpeg"
)

func TestPrefsInArgs(t *testing.T) {
	nv := ffmpeg.Caps{Found: true, H264Encoder: "h264_nvenc", HW: ffmpeg.HWNVENC, ToneMap: true, Placebo: true}
	cpu := ffmpeg.Caps{Found: true, H264Encoder: "libx264", Software: true, ToneMap: true}
	tr := &Transcoder{Tool: ffmpeg.WithCaps("ffmpeg", nv)}
	args := func(r Request, c ffmpeg.Caps) string {
		r.Path, r.Container = "in.mkv", "mp4"
		if r.Video == "" {
			r.Video = "h264"
		}
		if r.AudioCodec == "" {
			r.AudioCodec = "aac"
		}
		if err := r.Validate(); err != nil {
			t.Fatal(err)
		}
		tr.applyPrefs(&r)
		return strings.Join(Args(r, c), " ")
	}

	// Kinara's own choices: what conversions did before there were settings.
	if a := args(Request{}, nv); !strings.Contains(a, "-preset p5") {
		t.Errorf("default NVENC preset: %s", a)
	}
	if a := args(Request{}, cpu); !strings.Contains(a, "-preset veryfast -crf 21") {
		t.Errorf("default x264 preset: %s", a)
	}

	tr.SetPrefs(Prefs{Speed: "quality"})
	if a := args(Request{}, nv); !strings.Contains(a, "-preset p7") {
		t.Errorf("quality NVENC: %s", a)
	}
	tr.SetPrefs(Prefs{Speed: "faster"})
	if a := args(Request{}, cpu); !strings.Contains(a, "-preset superfast -crf 23") {
		t.Errorf("faster x264: %s", a)
	}

	// Sound: stereo always, own bitrates, no dialogue lift.
	tr.SetPrefs(Prefs{AudioChannels: 2, StereoKbps: 256, NoDialogueBoost: true})
	a := args(Request{AudioChannels: 6}, cpu)
	if !strings.Contains(a, "-b:a 256k") || strings.Contains(a, "alimiter") || !strings.Contains(a, "-ac 2") {
		t.Errorf("stereo without lift: %s", a)
	}
	tr.SetPrefs(Prefs{SurroundKbps: 512})
	if a := args(Request{AudioChannels: 6}, cpu); !strings.Contains(a, "-b:a 512k") {
		t.Errorf("surround bitrate: %s", a)
	}
	tr.SetPrefs(Prefs{})
	if a := args(Request{AudioChannels: 6, MaxAudioChannels: 2}, cpu); !strings.Contains(a, "alimiter") {
		t.Errorf("dialogue lift is on by default: %s", a)
	}

	// Tone mapping: curves, and zscale instead of libplacebo.
	tr.SetPrefs(Prefs{ToneMapCurve: "mobius"})
	if a := args(Request{HDR: "hdr10"}, cpu); !strings.Contains(a, "tonemap=tonemap=mobius") {
		t.Errorf("zscale curve: %s", a)
	}
	tr.SetPrefs(Prefs{ToneMapCurve: "bt.2390"})
	if a := args(Request{HDR: "hdr10"}, cpu); !strings.Contains(a, "tonemap=tonemap=hable") {
		t.Errorf("zscale has no BT.2390: %s", a)
	}
	placebo := cpu
	placebo.Placebo = true
	tr.SetPrefs(Prefs{ToneMapCurve: "reinhard"})
	if a := args(Request{HDR: "hdr10"}, placebo); !strings.Contains(a, "tonemapping=reinhard") {
		t.Errorf("libplacebo curve: %s", a)
	}
	tr.SetPrefs(Prefs{ToneMap: "zscale"})
	r := Request{HDR: "hdr10"}
	tr.applyPrefs(&r)
	if tr.capsFor(r).Placebo {
		t.Error("zscale chosen, libplacebo still used")
	}
	r = Request{HDR: "dolby_vision"}
	tr.applyPrefs(&r)
	if !tr.capsFor(r).Placebo {
		t.Error("Dolby Vision lost libplacebo")
	}
}

func TestJobLimitFollowsPrefs(t *testing.T) {
	tr := &Transcoder{Tool: ffmpeg.WithCaps("ffmpeg", ffmpeg.Caps{Found: true, H264Encoder: "libx264"})}
	if n := cap(tr.slots()); n != 3 {
		t.Fatalf("automatic without a graphics card: %d", n)
	}
	held := tr.slots()
	held <- struct{}{}
	tr.SetPrefs(Prefs{MaxJobs: 5})
	if n := cap(tr.slots()); n != 5 {
		t.Fatalf("after setting 5: %d", n)
	}
	tr.MaxJobs = 2
	tr.SetPrefs(Prefs{})
	if n := cap(tr.slots()); n != 2 {
		t.Fatalf("back to KINARA_MAX_TRANSCODES: %d", n)
	}
	<-held // a conversion on the old limit still finishes
	if !slices.Contains(Speeds, "balanced") {
		t.Fatal("speeds")
	}
}
