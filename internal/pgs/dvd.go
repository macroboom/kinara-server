package pgs

import (
	"encoding/binary"
	"errors"
	"fmt"
	"hash/fnv"
	"image"
	"image/color"
	"strconv"
	"strings"
)

// DVD subtitles (VobSub, "dvd_subtitle"): each packet is a subpicture unit
// (SPU) holding a 2-bit run-length bitmap stored as two interlaced fields,
// and control sequences that say when it shows and hides, where, and which
// 4 of the track's 16 palette colours (and how opaque) its pixel values use.
// The palette comes from the track's header ("palette: rrggbb, ...").
//
// DVB subtitles are converted to DVD subpictures by ffmpeg first (see
// extract.go), so this decoder serves both.

// spuTick is the unit of control sequence delays: 1024 / 90 kHz.
const spuTick = 1024.0 / 90000.0

// subpicture is one decoded SPU.
type subpicture struct {
	x, y, w, h int
	// pix holds pixel values 0-3, w*h.
	pix []byte
	// colors and alpha map pixel values to palette entries and opacity (0-15).
	colors, alpha [4]byte
	// start and stop are seconds after the packet's time; stop < 0 when the
	// SPU doesn't say (it then lasts until the next one, or its duration).
	start, stop float64
	forced      bool
}

var errBadSPU = errors.New("malformed DVD subpicture")

// decodeSPU reads one subpicture unit.
func decodeSPU(b []byte) (subpicture, error) {
	sp := subpicture{stop: -1}
	if len(b) < 4 {
		return sp, errBadSPU
	}
	size := int(binary.BigEndian.Uint16(b))
	if size > len(b) || size < 4 {
		size = len(b)
	}
	b = b[:size]
	ctrl := int(binary.BigEndian.Uint16(b[2:]))
	if ctrl >= len(b) || ctrl < 4 {
		return sp, errBadSPU
	}
	var x1, x2, y1, y2, top, bottom int
	haveArea, haveRLE, started := false, false, false

	seen := map[int]bool{}
	for off := ctrl; off+4 <= len(b) && !seen[off]; {
		seen[off] = true
		delay := float64(binary.BigEndian.Uint16(b[off:])) * spuTick
		next := int(binary.BigEndian.Uint16(b[off+2:]))
		i := off + 4
	cmds:
		for i < len(b) {
			cmd := b[i]
			i++
			switch cmd {
			case 0x00: // forced start
				sp.forced = true
				fallthrough
			case 0x01: // start display
				if !started {
					sp.start, started = delay, true
				}
			case 0x02: // stop display
				if sp.stop < 0 {
					sp.stop = delay
				}
			case 0x03: // colours
				if i+2 > len(b) {
					return sp, errBadSPU
				}
				sp.colors = [4]byte{b[i+1] & 0xf, b[i+1] >> 4, b[i] & 0xf, b[i] >> 4}
				i += 2
			case 0x04: // opacity
				if i+2 > len(b) {
					return sp, errBadSPU
				}
				sp.alpha = [4]byte{b[i+1] & 0xf, b[i+1] >> 4, b[i] & 0xf, b[i] >> 4}
				i += 2
			case 0x05: // area
				if i+6 > len(b) {
					return sp, errBadSPU
				}
				x1 = int(b[i])<<4 | int(b[i+1])>>4
				x2 = int(b[i+1]&0xf)<<8 | int(b[i+2])
				y1 = int(b[i+3])<<4 | int(b[i+4])>>4
				y2 = int(b[i+4]&0xf)<<8 | int(b[i+5])
				haveArea = true
				i += 6
			case 0x06: // where the two fields start
				if i+4 > len(b) {
					return sp, errBadSPU
				}
				top = int(binary.BigEndian.Uint16(b[i:]))
				bottom = int(binary.BigEndian.Uint16(b[i+2:]))
				haveRLE = true
				i += 4
			case 0x07: // colour changes within the picture: skipped (rare)
				if i+2 > len(b) {
					return sp, errBadSPU
				}
				i += int(binary.BigEndian.Uint16(b[i:]))
			case 0xff:
				break cmds
			default:
				return sp, fmt.Errorf("%w: command %#x", errBadSPU, cmd)
			}
		}
		if next == off || next >= len(b) {
			break
		}
		off = next
	}
	if !haveArea || !haveRLE || x2 < x1 || y2 < y1 {
		return sp, errBadSPU
	}
	sp.x, sp.y, sp.w, sp.h = x1, y1, x2-x1+1, y2-y1+1
	if sp.w > 4096 || sp.h > 4096 {
		return sp, errBadSPU
	}
	sp.pix = make([]byte, sp.w*sp.h)
	// Even lines come from the top field, odd lines from the bottom one.
	if err := spuField(b, top, ctrl, sp.pix, sp.w, sp.h, 0); err != nil {
		return sp, err
	}
	if err := spuField(b, bottom, ctrl, sp.pix, sp.w, sp.h, 1); err != nil {
		return sp, err
	}
	return sp, nil
}

// spuField expands one field's run-length data (nibble codes of 4, 8, 12
// or 16 bits: a run length and a 2-bit value; run 0 fills the line) into
// every other line of pix, starting at line first.
func spuField(b []byte, off, end int, pix []byte, w, h, first int) error {
	if off < 4 || off > len(b) {
		return errBadSPU
	}
	if end > len(b) || end < off {
		end = len(b)
	}
	nib := off * 2 // position in nibbles
	get := func() (int, bool) {
		if nib/2 >= end {
			return 0, false
		}
		v := b[nib/2]
		if nib%2 == 0 {
			v >>= 4
		}
		nib++
		return int(v & 0xf), true
	}
	for y := first; y < h; y += 2 {
		row := pix[y*w : (y+1)*w]
		for x := 0; x < w; {
			v, ok := get()
			if !ok {
				return nil // short field: the rest stays transparent
			}
			for _, limit := range []int{0x4, 0x10, 0x40} {
				if v >= limit {
					break
				}
				n, ok := get()
				if !ok {
					return nil
				}
				v = v<<4 | n
			}
			run, val := v>>2, byte(v&3)
			if run == 0 || x+run > w {
				run = w - x
			}
			for k := 0; k < run; k++ {
				row[x+k] = val
			}
			x += run
		}
		// Each line starts on a whole byte.
		nib += nib % 2
	}
	return nil
}

// dvdPalette is a track's 16 colours.
type dvdPalette [16]color.NRGBA

// parseIdx reads the palette and frame size from a VobSub header (the
// track's codec private data, as an .idx file starts). Missing parts stay
// zero; a missing palette is grey levels.
func parseIdx(text string) (pal dvdPalette, w, h int) {
	for i := range pal {
		g := uint8(i * 17)
		pal[i] = color.NRGBA{g, g, g, 255}
	}
	for _, line := range strings.Split(text, "\n") {
		line = strings.TrimSpace(line)
		name, val, ok := strings.Cut(line, ":")
		if !ok {
			continue
		}
		val = strings.TrimSpace(val)
		switch strings.ToLower(strings.TrimSpace(name)) {
		case "size":
			a, b, ok := strings.Cut(val, "x")
			if ok {
				w, _ = strconv.Atoi(strings.TrimSpace(a))
				h, _ = strconv.Atoi(strings.TrimSpace(b))
			}
		case "palette":
			for i, c := range strings.Split(val, ",") {
				if i >= 16 {
					break
				}
				if v, err := strconv.ParseUint(strings.TrimSpace(c), 16, 32); err == nil {
					pal[i] = color.NRGBA{uint8(v >> 16), uint8(v >> 8), uint8(v), 255}
				}
			}
		}
	}
	return pal, w, h
}

// image draws a subpicture, cropped to its visible pixels, and returns it
// with its place in the frame and a key from its pixels (nil when nothing
// shows).
func (sp subpicture) image(pal *dvdPalette) (img *image.NRGBA, x, y int, key string) {
	var cols [4]color.NRGBA
	for v := 0; v < 4; v++ {
		c := pal[sp.colors[v]&0xf]
		c.A = sp.alpha[v] * 17
		cols[v] = c
	}
	minX, minY, maxX, maxY := sp.w, sp.h, -1, -1
	for yy := 0; yy < sp.h; yy++ {
		for xx := 0; xx < sp.w; xx++ {
			if cols[sp.pix[yy*sp.w+xx]].A == 0 {
				continue
			}
			minX, minY = min(minX, xx), min(minY, yy)
			maxX, maxY = max(maxX, xx), max(maxY, yy)
		}
	}
	if maxX < 0 {
		return nil, 0, 0, ""
	}
	cw, ch := maxX-minX+1, maxY-minY+1
	img = image.NewNRGBA(image.Rect(0, 0, cw, ch))
	for yy := 0; yy < ch; yy++ {
		for xx := 0; xx < cw; xx++ {
			c := cols[sp.pix[(yy+minY)*sp.w+xx+minX]]
			if c.A == 0 {
				continue
			}
			img.SetNRGBA(xx, yy, c)
		}
	}
	h64 := fnv.New64a()
	h64.Write(img.Pix)
	return img, sp.x + minX, sp.y + minY, fmt.Sprintf("%dx%d-%016x", cw, ch, h64.Sum64())
}

// frameFor picks the frame that subpicture positions refer to when the
// track doesn't say: DVD and standard DVB pictures are 720 wide and as tall
// as their video (480 or 576 lines); high definition DVB uses the video's
// own frame.
func frameFor(videoW, videoH int) (int, int) {
	switch {
	case videoH <= 0:
		return 720, 576
	case videoH <= 480:
		return 720, 480
	case videoH <= 576:
		return 720, 576
	case videoH <= 720:
		return 1280, 720
	default:
		return 1920, 1080
	}
}

// dvbPalette is what ffmpeg gets to draw converted DVB subtitles with: the
// colours subtitles use (white, yellow, cyan, green, greys for edges, black
// for outlines) plus a few others.
const dvbPalette = "000000,404040,808080,c0c0c0,ffffff,ffff00,00ffff,00ff00,ff0000,0000ff,ff00ff,808000,008080,800000,000080,ff8000"
