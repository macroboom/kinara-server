// Package pgs reads Blu-ray (PGS, .sup) subtitles: timed bitmaps with
// positions. Apps draw them over the video themselves, so the server never
// has to burn them into the picture.
//
// A PGS stream is a series of display sets, each a run of segments ending in
// an END segment: a presentation composition (PCS) says which objects show,
// where, and from when; palettes (PDS) and objects (ODS, run-length encoded
// bitmaps) may come with it or be kept from earlier sets. What a PCS shows
// stays up until the next PCS, which often shows nothing.
package pgs

import (
	"bufio"
	"encoding/binary"
	"errors"
	"fmt"
	"hash/fnv"
	"image"
	"image/color"
	"io"
)

// Segment types.
const (
	segPalette     = 0x14
	segObject      = 0x15
	segComposition = 0x16
	segWindow      = 0x17
	segEnd         = 0x80
)

// Image is one bitmap shown on screen, placed in the video's frame (Width
// by Height of the Event).
type Image struct {
	X, Y int
	Img  *image.NRGBA
	// Forced marks a subtitle meant to show even when subtitles are off
	// (signs, foreign dialogue), in a track that has both kinds.
	Forced bool
	// Key is the same for the same picture shown again (a line restated at
	// the next acquisition point), so it's only stored once. It comes from
	// the pixels: object and palette numbers start over at every epoch, so
	// different lines often share them.
	Key string
}

// Event is what shows from Start to End, in seconds of the stream.
type Event struct {
	Start, End float64
	// Width and Height are the video frame the positions refer to.
	Width, Height int
	Images        []Image
}

type object struct {
	width, height int
	data          []byte
	version       int
	complete      bool
	decoded       []byte // palette indexes, width*height
}

type palette struct {
	colors  [256]color.NRGBA
	version int
}

type compObject struct {
	id                         uint16
	forced, cropped            bool
	x, y                       int
	cropX, cropY, cropW, cropH int
}

// Decode reads a PGS stream and calls emit for each event once its end is
// known, in order. A last event still showing when the stream ends is
// given a few seconds. It returns the first error from reading or emit;
// a stream cut short in the middle of a segment ends cleanly.
func Decode(r io.Reader, emit func(Event) error) error {
	d := &decoder{
		r:        bufio.NewReaderSize(r, 64<<10),
		objects:  map[uint16]*object{},
		palettes: map[byte]*palette{},
	}
	return d.run(emit)
}

type decoder struct {
	r        *bufio.Reader
	objects  map[uint16]*object
	palettes map[byte]*palette
	width    int
	height   int
	open     *Event // shown, waiting for its end
	// pendingPCS is a composition whose display set hasn't ended yet.
	pendingPCS *pendingPCS
}

// lastFor is how long a final event with no clear after it stays up.
const lastFor = 5.0

func (d *decoder) run(emit func(Event) error) error {
	var hdr [13]byte
	for {
		if _, err := io.ReadFull(d.r, hdr[:]); err != nil {
			if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
				return d.flush(emit)
			}
			return err
		}
		if hdr[0] != 'P' || hdr[1] != 'G' {
			return fmt.Errorf("pgs: bad segment header %x", hdr[:2])
		}
		pts := float64(binary.BigEndian.Uint32(hdr[2:6])) / 90000
		kind := hdr[10]
		size := int(binary.BigEndian.Uint16(hdr[11:13]))
		body := make([]byte, size)
		if _, err := io.ReadFull(d.r, body); err != nil {
			if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
				return d.flush(emit)
			}
			return err
		}
		switch kind {
		case segPalette:
			d.palette(body)
		case segObject:
			d.object(body)
		case segComposition:
			if err := d.composition(pts, body, emit); err != nil {
				return err
			}
		case segEnd:
			// The set's objects and palette are in: draw what it shows.
			if d.pendingPCS != nil {
				if err := d.show(emit); err != nil {
					return err
				}
			}
		case segWindow:
			// Windows only bound where objects may go.
		}
	}
}

func (d *decoder) flush(emit func(Event) error) error {
	if d.pendingPCS != nil {
		if err := d.show(emit); err != nil {
			return err
		}
	}
	if d.open == nil {
		return nil
	}
	ev := *d.open
	d.open = nil
	ev.End = ev.Start + lastFor
	return emit(ev)
}

func (d *decoder) palette(b []byte) {
	if len(b) < 2 {
		return
	}
	id := b[0]
	p := d.palettes[id]
	if p == nil {
		p = &palette{}
		d.palettes[id] = p
	}
	p.version++
	bt709 := d.height > 576
	for i := 2; i+5 <= len(b); i += 5 {
		p.colors[b[i]] = ycbcr(b[i+1], b[i+3], b[i+2], b[i+4], bt709)
	}
}

// ycbcr converts a palette entry (limited range) to RGB with alpha.
func ycbcr(y, cb, cr, a byte, bt709 bool) color.NRGBA {
	Y := 1.164 * (float64(y) - 16)
	Cb := float64(cb) - 128
	Cr := float64(cr) - 128
	var r, g, b float64
	if bt709 {
		r = Y + 1.793*Cr
		g = Y - 0.213*Cb - 0.533*Cr
		b = Y + 2.112*Cb
	} else {
		r = Y + 1.596*Cr
		g = Y - 0.392*Cb - 0.813*Cr
		b = Y + 2.017*Cb
	}
	return color.NRGBA{R: clamp(r), G: clamp(g), B: clamp(b), A: a}
}

func clamp(v float64) uint8 {
	switch {
	case v <= 0:
		return 0
	case v >= 255:
		return 255
	}
	return uint8(v + 0.5)
}

func (d *decoder) object(b []byte) {
	if len(b) < 4 {
		return
	}
	id := binary.BigEndian.Uint16(b[0:2])
	flags := b[3]
	o := d.objects[id]
	if flags&0x80 != 0 { // first in sequence
		if len(b) < 11 {
			return
		}
		prev := 0
		if o != nil {
			prev = o.version
		}
		o = &object{
			width:   int(binary.BigEndian.Uint16(b[7:9])),
			height:  int(binary.BigEndian.Uint16(b[9:11])),
			version: prev + 1,
		}
		d.objects[id] = o
		o.data = append(o.data, b[11:]...)
	} else if o != nil && !o.complete {
		o.data = append(o.data, b[4:]...)
	}
	if o != nil && flags&0x40 != 0 { // last in sequence
		o.complete = true
		o.decoded = nil
	}
}

// composition handles a PCS: the event showing before it ends now, and what
// it shows is drawn at its set's END, once the objects and palette that
// follow it in the set have arrived.
func (d *decoder) composition(pts float64, b []byte, emit func(Event) error) error {
	if d.pendingPCS != nil {
		if err := d.show(emit); err != nil {
			return err
		}
	}
	if len(b) < 11 {
		return nil
	}
	d.width = int(binary.BigEndian.Uint16(b[0:2]))
	d.height = int(binary.BigEndian.Uint16(b[2:4]))
	state := b[7]
	if state&0x80 != 0 { // epoch start: everything before is gone
		d.objects = map[uint16]*object{}
		d.palettes = map[byte]*palette{}
	}
	pc := &pendingPCS{pts: pts, paletteID: b[9]}
	n := int(b[10])
	at := 11
	for i := 0; i < n && at+8 <= len(b); i++ {
		c := compObject{
			id: binary.BigEndian.Uint16(b[at : at+2]),
			x:  int(binary.BigEndian.Uint16(b[at+4 : at+6])),
			y:  int(binary.BigEndian.Uint16(b[at+6 : at+8])),
		}
		flags := b[at+3]
		c.cropped, c.forced = flags&0x80 != 0, flags&0x40 != 0
		at += 8
		if c.cropped {
			if at+8 > len(b) {
				break
			}
			c.cropX = int(binary.BigEndian.Uint16(b[at : at+2]))
			c.cropY = int(binary.BigEndian.Uint16(b[at+2 : at+4]))
			c.cropW = int(binary.BigEndian.Uint16(b[at+4 : at+6]))
			c.cropH = int(binary.BigEndian.Uint16(b[at+6 : at+8]))
			at += 8
		}
		pc.objects = append(pc.objects, c)
	}
	d.pendingPCS = pc
	// End what was showing at this composition's time.
	if d.open != nil {
		ev := *d.open
		d.open = nil
		if pts > ev.Start {
			ev.End = pts
			if err := emit(ev); err != nil {
				return err
			}
		}
	}
	return nil
}

type pendingPCS struct {
	pts       float64
	paletteID byte
	objects   []compObject
}

// show draws the pending composition with the objects and palette it now
// has, and opens it as the event on screen.
func (d *decoder) show(emit func(Event) error) error {
	pc := d.pendingPCS
	d.pendingPCS = nil
	if len(pc.objects) == 0 {
		return nil
	}
	pal := d.palettes[pc.paletteID]
	ev := Event{Start: pc.pts, Width: d.width, Height: d.height}
	for _, c := range pc.objects {
		o := d.objects[c.id]
		if o == nil || !o.complete || pal == nil {
			continue
		}
		img, key := d.render(c, o, pal)
		if img == nil {
			continue
		}
		ev.Images = append(ev.Images, Image{X: c.x, Y: c.y, Img: img, Forced: c.forced, Key: key})
	}
	if len(ev.Images) == 0 {
		return nil
	}
	d.open = &ev
	return nil
}

func (d *decoder) render(c compObject, o *object, pal *palette) (*image.NRGBA, string) {
	if o.decoded == nil {
		o.decoded = rle(o.data, o.width, o.height)
		if o.decoded == nil {
			return nil, ""
		}
	}
	x0, y0, w, h := 0, 0, o.width, o.height
	if c.cropped {
		x0, y0 = min(c.cropX, o.width), min(c.cropY, o.height)
		w, h = min(c.cropW, o.width-x0), min(c.cropH, o.height-y0)
	}
	if w <= 0 || h <= 0 {
		return nil, ""
	}
	img := image.NewNRGBA(image.Rect(0, 0, w, h))
	visible := false
	for y := 0; y < h; y++ {
		row := o.decoded[(y0+y)*o.width+x0:]
		for x := 0; x < w; x++ {
			col := pal.colors[row[x]]
			if col.A != 0 {
				visible = true
			}
			img.SetNRGBA(x, y, col)
		}
	}
	if !visible {
		return nil, ""
	}
	h64 := fnv.New64a()
	h64.Write(img.Pix)
	return img, fmt.Sprintf("%dx%d-%016x", w, h, h64.Sum64())
}

// rle expands a PGS run-length bitmap into palette indexes, or nil if it's
// malformed.
func rle(b []byte, width, height int) []byte {
	if width <= 0 || height <= 0 || width*height > 16<<20 {
		return nil
	}
	out := make([]byte, width*height)
	x, y, i := 0, 0, 0
	put := func(n int, c byte) {
		for ; n > 0 && x < width; n-- {
			out[y*width+x] = c
			x++
		}
	}
	for i < len(b) && y < height {
		v := b[i]
		i++
		if v != 0 {
			put(1, v)
			continue
		}
		if i >= len(b) {
			break
		}
		f := b[i]
		i++
		switch {
		case f == 0: // end of line
			x = 0
			y++
		case f&0xC0 == 0x00:
			put(int(f&0x3F), 0)
		case f&0xC0 == 0x40:
			if i >= len(b) {
				return out
			}
			put(int(f&0x3F)<<8|int(b[i]), 0)
			i++
		case f&0xC0 == 0x80:
			if i >= len(b) {
				return out
			}
			put(int(f&0x3F), b[i])
			i++
		default:
			if i+1 >= len(b) {
				return out
			}
			put(int(f&0x3F)<<8|int(b[i]), b[i+1])
			i += 2
		}
	}
	return out
}
