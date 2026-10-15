package pgs

import (
	"bytes"
	"encoding/binary"
	"image/color"
	"testing"
)

// supWriter builds a PGS stream for tests.
type supWriter struct{ bytes.Buffer }

func (w *supWriter) seg(pts float64, kind byte, body []byte) {
	var h [13]byte
	h[0], h[1] = 'P', 'G'
	binary.BigEndian.PutUint32(h[2:6], uint32(pts*90000))
	h[10] = kind
	binary.BigEndian.PutUint16(h[11:13], uint16(len(body)))
	w.Write(h[:])
	w.Write(body)
}

type testObj struct {
	id     uint16
	x, y   int
	forced bool
	crop   []int // x, y, w, h
}

func u16(v int) []byte { return []byte{byte(v >> 8), byte(v)} }

func (w *supWriter) pcs(pts float64, state byte, palID byte, objs ...testObj) {
	b := append(u16(1920), u16(1080)...)
	b = append(b, 0x10, 0, 1, state, 0, palID, byte(len(objs)))
	for _, o := range objs {
		flags := byte(0)
		if o.forced {
			flags |= 0x40
		}
		if o.crop != nil {
			flags |= 0x80
		}
		b = append(b, u16(int(o.id))...)
		b = append(b, 0, flags)
		b = append(b, u16(o.x)...)
		b = append(b, u16(o.y)...)
		for _, c := range o.crop {
			b = append(b, u16(c)...)
		}
	}
	w.seg(pts, segComposition, b)
}

// pds writes palette id with entries index -> (Y, Cr, Cb, A).
func (w *supWriter) pds(pts float64, id byte, entries map[byte][4]byte) {
	b := []byte{id, 0}
	for i := 0; i < 256; i++ {
		if e, ok := entries[byte(i)]; ok {
			b = append(b, byte(i), e[0], e[1], e[2], e[3])
		}
	}
	w.seg(pts, segPalette, b)
}

func (w *supWriter) ods(pts float64, id uint16, width, height int, px []byte) {
	data := encodeRLE(px, width, height)
	n := len(data) + 4
	b := append(u16(int(id)), 0, 0xC0, byte(n>>16), byte(n>>8), byte(n))
	b = append(b, u16(width)...)
	b = append(b, u16(height)...)
	b = append(b, data...)
	w.seg(pts, segObject, b)
}

func (w *supWriter) end(pts float64) { w.seg(pts, segEnd, nil) }

// encodeRLE is the PGS run-length encoding of palette indexes.
func encodeRLE(px []byte, width, height int) []byte {
	var out []byte
	for y := 0; y < height; y++ {
		row := px[y*width : (y+1)*width]
		for x := 0; x < width; {
			c := row[x]
			n := 1
			for x+n < width && row[x+n] == c && n < 16383 {
				n++
			}
			switch {
			case c != 0 && n == 1:
				out = append(out, c)
			case c == 0 && n < 64:
				out = append(out, 0, byte(n))
			case c == 0:
				out = append(out, 0, 0x40|byte(n>>8), byte(n))
			case n < 64:
				out = append(out, 0, 0x80|byte(n), c)
			default:
				out = append(out, 0, 0xC0|byte(n>>8), byte(n), c)
			}
			x += n
		}
		out = append(out, 0, 0)
	}
	return out
}

// white is opaque white in limited-range YCrCb.
var white = [4]byte{235, 128, 128, 255}

// sampleSup is three display sets: a line at 1s, cleared at 3s, then a
// forced, cropped one at 5s that runs to the end of the stream.
func sampleSup() []byte {
	var w supWriter
	px := make([]byte, 100*2)
	for i := range 100 {
		px[i] = 1 // top row white, bottom row transparent
	}
	px[100+50] = 1
	w.pcs(1, 0x80, 0, testObj{id: 0, x: 900, y: 950})
	w.pds(1, 0, map[byte][4]byte{1: white})
	w.ods(1, 0, 100, 2, px)
	w.end(1)

	w.pcs(3, 0x00, 0)
	w.end(3)

	wide := make([]byte, 200*4)
	for i := range wide {
		wide[i] = 2
	}
	w.pcs(5, 0x80, 0, testObj{id: 1, x: 10, y: 20, forced: true, crop: []int{0, 0, 120, 3}})
	w.pds(5, 0, map[byte][4]byte{2: {81, 240, 90, 128}}) // half-transparent red
	w.ods(5, 1, 200, 4, wide)
	w.end(5)
	return w.Bytes()
}

func TestDecode(t *testing.T) {
	var got []Event
	if err := Decode(bytes.NewReader(sampleSup()), func(ev Event) error {
		got = append(got, ev)
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 {
		t.Fatalf("got %d events, want 2", len(got))
	}
	a, b := got[0], got[1]
	if a.Start != 1 || a.End != 3 || a.Width != 1920 || a.Height != 1080 || len(a.Images) != 1 {
		t.Fatalf("first event %+v", a)
	}
	im := a.Images[0]
	if im.X != 900 || im.Y != 950 || im.Img.Bounds().Dx() != 100 || im.Img.Bounds().Dy() != 2 || im.Forced {
		t.Fatalf("first image %+v %v", im, im.Img.Bounds())
	}
	if c := im.Img.NRGBAAt(10, 0); c != (color.NRGBA{255, 255, 255, 255}) {
		t.Errorf("top row = %v, want opaque white", c)
	}
	if c := im.Img.NRGBAAt(10, 1); c.A != 0 {
		t.Errorf("bottom row = %v, want transparent", c)
	}
	if c := im.Img.NRGBAAt(50, 1); c.A != 255 {
		t.Errorf("single pixel = %v, want opaque", c)
	}

	if b.Start != 5 || b.End != 5+lastFor || len(b.Images) != 1 {
		t.Fatalf("second event %+v", b)
	}
	im = b.Images[0]
	if !im.Forced || im.Img.Bounds().Dx() != 120 || im.Img.Bounds().Dy() != 3 {
		t.Fatalf("cropped forced image %+v %v", im, im.Img.Bounds())
	}
	c := im.Img.NRGBAAt(0, 0)
	if c.A != 128 || c.R < 200 || c.G > 60 || c.B > 60 {
		t.Errorf("red = %v", c)
	}
}

func TestRLELongRuns(t *testing.T) {
	px := make([]byte, 300*3)
	for i := range px {
		switch {
		case i%300 < 70:
			px[i] = 0
		case i%300 < 200:
			px[i] = 7
		default:
			px[i] = byte(i % 5)
		}
	}
	got := rle(encodeRLE(px, 300, 3), 300, 3)
	if !bytes.Equal(got, px) {
		t.Fatal("round trip changed the bitmap")
	}
}

func TestDecodeCutShort(t *testing.T) {
	sup := sampleSup()
	n := 0
	err := Decode(bytes.NewReader(sup[:len(sup)-30]), func(Event) error { n++; return nil })
	if err != nil || n != 1 {
		t.Fatalf("cut stream: %d events, err %v", n, err)
	}
	if err := Decode(bytes.NewReader([]byte("XX garbage that is long")), func(Event) error { return nil }); err == nil {
		t.Fatal("garbage should fail")
	}
}

func bytesReader(b []byte) *bytes.Reader { return bytes.NewReader(b) }

// Real discs start a new epoch for each line, numbering objects and palettes
// from scratch, so two different lines can both be "object 0, palette 0".
// They must stay two different pictures.
func TestNewEpochNewPicture(t *testing.T) {
	var w supWriter
	line := func(at float64, fill byte) {
		px := make([]byte, 50*10)
		for i := range px {
			px[i] = fill
		}
		w.pcs(at, 0x80, 0, testObj{id: 0, x: 100, y: 900})
		w.pds(at, 0, map[byte][4]byte{1: white, 2: {81, 240, 90, 255}})
		w.ods(at, 0, 50, 10, px)
		w.end(at)
		w.pcs(at+1, 0, 0)
		w.end(at + 1)
	}
	line(1, 1)
	line(3, 2)
	line(5, 1) // the first picture again
	var got []Event
	if err := Decode(bytes.NewReader(w.Bytes()), func(ev Event) error { got = append(got, ev); return nil }); err != nil {
		t.Fatal(err)
	}
	if len(got) != 3 {
		t.Fatalf("got %d events", len(got))
	}
	a, b, c := got[0].Images[0].Key, got[1].Images[0].Key, got[2].Images[0].Key
	if a == b {
		t.Fatal("two different lines got the same picture")
	}
	if a != c {
		t.Fatal("the same picture shown again should be stored once")
	}

	x := &Extractor{Dir: t.TempDir()}
	j := &job{keys: map[string]TimelineImage{}}
	for _, ev := range got {
		if err := x.add(j, x.Dir, ev); err != nil {
			t.Fatal(err)
		}
	}
	ns := []int{j.tl.Events[0].Images[0].N, j.tl.Events[1].Images[0].N, j.tl.Events[2].Images[0].N}
	if ns[0] == ns[1] || ns[0] != ns[2] || j.images != 2 {
		t.Fatalf("pictures %v, stored %d", ns, j.images)
	}
}
