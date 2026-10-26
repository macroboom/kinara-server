package pgs

import (
	"encoding/binary"
	"fmt"
	"image/color"
	"image/png"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"kinara/internal/ffmpeg"
)

// --- Making DVD subpictures, for the tests ---------------------------------------------

func runNibbles(run int, val byte) []byte {
	v := run<<2 | int(val)
	switch {
	case run == 0: // to the end of the line
		return []byte{0, 0, 0, val}
	case run < 4:
		return []byte{byte(v)}
	case run < 16:
		return []byte{byte(v >> 4), byte(v & 0xf)}
	case run < 64:
		return []byte{byte(v >> 8), byte(v >> 4 & 0xf), byte(v & 0xf)}
	default:
		return []byte{byte(v >> 12), byte(v >> 8 & 0xf), byte(v >> 4 & 0xf), byte(v & 0xf)}
	}
}

func rleRows(rows [][]byte) []byte {
	var out []byte
	for _, row := range rows {
		var nb []byte
		for x := 0; x < len(row); {
			r := 1
			for x+r < len(row) && row[x+r] == row[x] && r < 255 {
				r++
			}
			if x+r == len(row) && r > 3 {
				nb = append(nb, runNibbles(0, row[x])...) // fill the line
			} else {
				nb = append(nb, runNibbles(r, row[x])...)
			}
			x += r
		}
		if len(nb)%2 == 1 {
			nb = append(nb, 0)
		}
		for i := 0; i < len(nb); i += 2 {
			out = append(out, nb[i]<<4|nb[i+1])
		}
	}
	return out
}

// testSPU makes a w by h subpicture at x, y: a checker of values 1 and 2
// with a transparent (0) border two lines deep, shown from 0 for stop
// seconds (or with no stop when stop < 0).
func testSPU(w, h, x, y int, stop float64, forced bool) ([]byte, [][]byte) {
	rows := make([][]byte, h)
	for j := range rows {
		rows[j] = make([]byte, w)
		for i := range rows[j] {
			if j >= 2 && j < h-2 {
				rows[j][i] = 1
				if (i/4+j/4)%2 == 1 {
					rows[j][i] = 2
				}
			}
		}
	}
	var even, odd [][]byte
	for j, r := range rows {
		if j%2 == 0 {
			even = append(even, r)
		} else {
			odd = append(odd, r)
		}
	}
	top, bot := rleRows(even), rleRows(odd)
	topOff, botOff := 4, 4+len(top)
	ctrl := botOff + len(bot)
	start := byte(0x01)
	if forced {
		start = 0x00
	}
	x2, y2 := x+w-1, y+h-1
	c1 := []byte{start, 0x03, 0x32, 0x10, 0x04, 0xff, 0xf0,
		0x05, byte(x >> 4), byte(x&0xf)<<4 | byte(x2>>8), byte(x2), byte(y >> 4), byte(y&0xf)<<4 | byte(y2>>8), byte(y2),
		0x06, byte(topOff >> 8), byte(topOff), byte(botOff >> 8), byte(botOff), 0xff}
	d2 := ctrl + 4 + len(c1)
	var body []byte
	body = append(body, top...)
	body = append(body, bot...)
	if stop >= 0 {
		body = binary.BigEndian.AppendUint16(body, 0)
		body = binary.BigEndian.AppendUint16(body, uint16(d2))
		body = append(body, c1...)
		body = binary.BigEndian.AppendUint16(body, uint16(math.Round(stop/spuTick)))
		body = binary.BigEndian.AppendUint16(body, uint16(d2))
		body = append(body, 0x02, 0xff)
	} else {
		body = binary.BigEndian.AppendUint16(body, 0)
		body = binary.BigEndian.AppendUint16(body, uint16(ctrl))
		body = append(body, c1...)
	}
	out := binary.BigEndian.AppendUint16(nil, uint16(4+len(body)))
	out = binary.BigEndian.AppendUint16(out, uint16(ctrl))
	return append(out, body...), rows
}

func TestDecodeSPU(t *testing.T) {
	b, rows := testSPU(64, 24, 100, 400, 2.5, false)
	sp, err := decodeSPU(b)
	if err != nil {
		t.Fatal(err)
	}
	if sp.x != 100 || sp.y != 400 || sp.w != 64 || sp.h != 24 {
		t.Fatalf("area: %d,%d %dx%d", sp.x, sp.y, sp.w, sp.h)
	}
	if math.Abs(sp.stop-2.5) > spuTick || sp.start != 0 || sp.forced {
		t.Fatalf("timing: start %v stop %v forced %v", sp.start, sp.stop, sp.forced)
	}
	if sp.colors != [4]byte{0, 1, 2, 3} || sp.alpha != [4]byte{0, 15, 15, 15} {
		t.Fatalf("colours %v alpha %v", sp.colors, sp.alpha)
	}
	for j, r := range rows {
		for i, v := range r {
			if sp.pix[j*64+i] != v {
				t.Fatalf("pixel %d,%d: got %d want %d", i, j, sp.pix[j*64+i], v)
			}
		}
	}
	pal, w, h := parseIdx("size: 720x480\npalette: 000000, ffffff, ff0000, 00ff00, 808080")
	if w != 720 || h != 480 || pal[2].R != 255 || pal[2].G != 0 {
		t.Fatalf("idx: %dx%d %v", w, h, pal[2])
	}
	img, x, y, key := sp.image(&pal)
	if img == nil || x != 100 || y != 402 || img.Bounds().Dx() != 64 || img.Bounds().Dy() != 20 {
		t.Fatalf("cropped to what shows: %v at %d,%d", img.Bounds(), x, y)
	}
	if c := img.NRGBAAt(0, 0); c.R != 255 || c.G != 255 || c.A != 255 {
		t.Fatalf("value 1 is palette 1 (white): %v", c)
	}
	if c := img.NRGBAAt(4, 0); c.R != 255 || c.G != 0 {
		t.Fatalf("value 2 is palette 2 (red): %v", c)
	}
	if key == "" {
		t.Fatal("no key")
	}

	forced, _ := testSPU(16, 8, 0, 0, -1, true)
	sp, err = decodeSPU(forced)
	if err != nil || !sp.forced || sp.stop >= 0 {
		t.Fatalf("forced, no stop: %+v %v", sp, err)
	}
	if _, err := decodeSPU([]byte{0, 8, 0, 2, 1, 2, 3, 4}); err == nil {
		t.Fatal("garbage decoded")
	}
}

func TestParseHexDump(t *testing.T) {
	d := "\n00000000: 7369 7a65 3a20 3732 3078 3438 300a 6f72  size: 720x480.or\n00000010: 0a                                       .\n"
	if got := string(parseHexDump(d)); got != "size: 720x480\nor\n" {
		t.Fatalf("%q", got)
	}
}

// --- Through ffmpeg ------------------------------------------------------------------------

func ptsBytes(pts int64) []byte {
	return []byte{byte(0x21 | (pts>>29)&0x0e), byte(pts >> 22), byte(0x01 | (pts>>14)&0xfe), byte(pts >> 7), byte(0x01 | (pts<<1)&0xfe)}
}

// writeVobSub writes t.idx and t.sub: three subtitles, the second forced and
// the third without a stop time.
func writeVobSub(t *testing.T, dir string) string {
	t.Helper()
	type sub struct {
		at, dur float64
		forced  bool
	}
	subs := []sub{{1, 2.5, false}, {4, 1, true}, {7.5, -1, false}}
	var data []byte
	var idx []string
	for i, s := range subs {
		spu, _ := testSPU(64, 24, 100+i*10, 400, s.dur, s.forced)
		pack := []byte{0, 0, 1, 0xba, 0x44, 0, 4, 0, 4, 1, 0x01, 0x89, 0xc3, 0xf8}
		pes := append([]byte{0x81, 0x80, 0x05}, ptsBytes(int64(s.at*90000))...)
		pes = append(pes, 0x20)
		pes = append(pes, spu...)
		pkt := append(pack, 0, 0, 1, 0xbd)
		pkt = binary.BigEndian.AppendUint16(pkt, uint16(len(pes)))
		pkt = append(pkt, pes...)
		for len(pkt)%2048 != 0 {
			pkt = append(pkt, 0)
		}
		ms := int(s.at * 1000)
		idx = append(idx, fmt.Sprintf("timestamp: 00:00:%02d:%03d, filepos: %09x", ms/1000, ms%1000, len(data)))
		data = append(data, pkt...)
	}
	if err := os.WriteFile(filepath.Join(dir, "t.sub"), data, 0o644); err != nil {
		t.Fatal(err)
	}
	head := "# VobSub index file, v7\nsize: 720x480\norg: 0, 0\nscale: 100%, 100%\nalpha: 100%\nsmooth: OFF\n" +
		"fadein/out: 0, 0\nalign: OFF at LEFT TOP\ntime offset: 0\nforced subs: OFF\n" +
		"palette: 000000, ffffff, ff0000, 00ff00, 808080, 808080, 808080, 808080, 808080, 808080, 808080, 808080, 808080, 808080, 808080, 808080\n" +
		"custom colors: OFF, tridx: 0000, colors: 000000, 000000, 000000, 000000\nlangidx: 0\nid: en, index: 0\n"
	p := filepath.Join(dir, "t.idx")
	if err := os.WriteFile(p, []byte(head+strings.Join(idx, "\n")+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

func readAll(t *testing.T, x *Extractor, id, path string, index int, codec string) Timeline {
	t.Helper()
	var tl Timeline
	var events []TimelineEvent
	deadline := time.Now().Add(30 * time.Second)
	for {
		var err error
		tl, err = x.Timeline(id, path, 1, index, codec, len(events))
		if err != nil {
			t.Fatal(err)
		}
		events = append(events, tl.Events...)
		if tl.Done || time.Now().After(deadline) {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	if tl.Error != "" {
		t.Fatal(tl.Error)
	}
	tl.Events = events
	return tl
}

func TestDVDAndDVBWithFFmpeg(t *testing.T) {
	for _, bin := range []string{"ffmpeg", "ffprobe"} {
		if _, err := exec.LookPath(bin); err != nil {
			t.Skip(bin + " not installed")
		}
	}
	dir := t.TempDir()
	idx := writeVobSub(t, dir)
	x := &Extractor{Tool: &ffmpeg.Tool{}, Dir: filepath.Join(dir, "cache")}
	x.SetOriginalDVD(true)

	tl := readAll(t, x, "v1", idx, 0, "dvd_subtitle")
	t.Logf("dvd: %+v", tl)
	if tl.Width != 720 || tl.Height != 480 || len(tl.Events) != 3 {
		t.Fatalf("dvd: %dx%d, %d events", tl.Width, tl.Height, len(tl.Events))
	}
	want := [][2]float64{{1, 3.5}, {4, 5}, {7.5, 12.5}} // the last: no stop, 5 s
	for i, ev := range tl.Events {
		if math.Abs(ev.Start-want[i][0]) > 0.05 || math.Abs(ev.End-want[i][1]) > 0.05 {
			t.Errorf("event %d: %v-%v, want %v", i, ev.Start, ev.End, want[i])
		}
		if len(ev.Images) != 1 || ev.Images[0].X != 100+i*10 || ev.Images[0].Y != 402 || ev.Images[0].W != 64 || ev.Images[0].H != 20 {
			t.Errorf("event %d images: %+v", i, ev.Images)
		}
	}
	if !tl.Events[1].Images[0].Forced || tl.Events[0].Images[0].Forced {
		t.Error("forced marks")
	}
	// The same picture is stored once.
	if tl.Events[0].Images[0].N != tl.Events[1].Images[0].N {
		t.Error("identical pictures should share a PNG")
	}
	if _, err := os.Stat(x.ImagePath("v1", 1, 0, tl.Events[0].Images[0].N)); err != nil {
		t.Fatal(err)
	}

	// Redrawn in Kinara's style (the default): twice the size, white and black.
	clean := &Extractor{Tool: &ffmpeg.Tool{}, Dir: filepath.Join(dir, "cache-clean")}
	ctl := readAll(t, clean, "v1", idx, 0, "dvd_subtitle")
	if ctl.Width != 720*cleanScale || ctl.Height != 480*cleanScale || len(ctl.Events) != 3 {
		t.Fatalf("clean dvd: %dx%d, %d events", ctl.Width, ctl.Height, len(ctl.Events))
	}
	for i, ev := range ctl.Events {
		im := ev.Images[0]
		if len(ev.Images) != 1 || abs(im.X-((100+i*10)*cleanScale-cleanPad)) > 2*cleanScale || im.W < 64*cleanScale {
			t.Errorf("clean event %d: %+v", i, ev.Images)
		}
	}
	f, err := os.Open(clean.ImagePath("v1", 1, 0, ctl.Events[0].Images[0].N))
	if err != nil {
		t.Fatal(err)
	}
	pic, err := png.Decode(f)
	f.Close()
	if err != nil {
		t.Fatal(err)
	}
	var white, black int
	b := pic.Bounds()
	for y := b.Min.Y; y < b.Max.Y; y++ {
		for x := b.Min.X; x < b.Max.X; x++ {
			c := color.NRGBAModel.Convert(pic.At(x, y)).(color.NRGBA)
			if c.A < 200 {
				continue
			}
			if c.R != c.G || c.G != c.B {
				t.Fatalf("a coloured pixel %v in a redrawn subtitle", c)
			}
			if c.R > 230 {
				white++
			} else if c.R < 25 {
				black++
			}
		}
	}
	if white == 0 || black == 0 {
		t.Fatalf("redrawn picture: %d white, %d black pixels", white, black)
	}

	// DVB: made from the DVD track by ffmpeg, read back through conversion.
	dvb := filepath.Join(dir, "dvb.mkv")
	if out, err := exec.Command("ffmpeg", "-v", "error", "-y", "-i", idx, "-map", "0", "-c:s", "dvbsub", dvb).CombinedOutput(); err != nil {
		t.Skipf("can't make DVB subtitles: %v %s", err, out)
	}
	tl = readAll(t, x, "v2", dvb, 0, "dvb_subtitle")
	t.Logf("dvb: %+v", tl)
	if len(tl.Events) != 3 || tl.Width != 720 || tl.Height != 576 {
		t.Fatalf("dvb: %dx%d, %d events", tl.Width, tl.Height, len(tl.Events))
	}
	for i, ev := range tl.Events {
		if math.Abs(ev.Start-want[i][0]) > 0.1 || ev.End-ev.Start > maxSubtitle || len(ev.Images) != 1 || ev.Images[0].W < 60 {
			t.Errorf("dvb event %d: %+v", i, ev)
		}
	}
}

func abs(v int) int {
	if v < 0 {
		return -v
	}
	return v
}
