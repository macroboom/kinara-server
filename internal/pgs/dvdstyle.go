package pgs

import (
	"fmt"
	"hash/fnv"
	"image"
	"image/color"
	"math"
)

// DVD subtitles are small (720 wide), use whatever four colours the disc
// picked (often yellow or grey, with a chunky outline), and blow up into
// jagged blocks on a big TV. clean redraws them in Kinara's subtitle style:
// the letter shapes, smoothed at twice the size, in white with a thin black
// outline, the same look text subtitles have.
//
// Which of the four pixel values are the letters: the brightest visible
// colour. The others (outline, anti-aliasing) are dropped and the outline
// is drawn afresh around the letters.

// cleanScale is how much bigger the redrawn pictures are than the disc's.
const cleanScale = 3

// outlineWidth is the outline's thickness in redrawn pixels.
const outlineWidth = 3.2

// cleanPad is the room left around the letters for the outline.
const cleanPad = 6

// clean returns the subpicture redrawn (nil when nothing shows), with its
// place in a frame cleanScale times the disc's, and a key from its pixels.
func (sp subpicture) clean(pal *dvdPalette) (img *image.NRGBA, x, y int, key string) {
	var cols [4]color.NRGBA
	visible := [4]bool{}
	for v := 0; v < 4; v++ {
		c := pal[sp.colors[v]&0xf]
		c.A = sp.alpha[v] * 17
		cols[v] = c
		visible[v] = c.A >= 0x40
	}
	// The letters: the brightest visible value that's used.
	used := [4]int{}
	for _, p := range sp.pix {
		used[p&3]++
	}
	fill, best := -1, -1.0
	for v := 0; v < 4; v++ {
		if !visible[v] || used[v] == 0 {
			continue
		}
		if l := luma(cols[v]); l > best {
			fill, best = v, l
		}
	}
	if fill < 0 {
		return nil, 0, 0, ""
	}
	// A mid colour between the letters and the rest (anti-aliasing) counts
	// as half letter.
	var weight [4]float64
	weight[fill] = 1
	for v := 0; v < 4; v++ {
		if v == fill || !visible[v] || used[v] == 0 {
			continue
		}
		if l := luma(cols[v]); best > 0 && l > best*0.55 {
			weight[v] = 0.5
		}
	}

	// The letters' bounding box, plus room for the outline once scaled.
	minX, minY, maxX, maxY := sp.w, sp.h, -1, -1
	for yy := 0; yy < sp.h; yy++ {
		for xx := 0; xx < sp.w; xx++ {
			if weight[sp.pix[yy*sp.w+xx]&3] == 0 {
				continue
			}
			minX, minY = min(minX, xx), min(minY, yy)
			maxX, maxY = max(maxX, xx), max(maxY, yy)
		}
	}
	if maxX < 0 {
		return nil, 0, 0, ""
	}
	pad := cleanPad
	sw, sh := maxX-minX+1, maxY-minY+1
	W, H := sw*cleanScale+2*pad, sh*cleanScale+2*pad

	// The letter mask at the disc's size, softened a little (a 3 by 3
	// tent), then smoothly enlarged (bilinear) and cut with a narrow ramp:
	// the staircase edges come out as smooth curves and slopes.
	raw := func(xx, yy int) float64 {
		if xx < 0 || yy < 0 || xx >= sw || yy >= sh {
			return 0
		}
		return weight[sp.pix[(yy+minY)*sp.w+xx+minX]&3]
	}
	mw, mh := sw+2, sh+2
	soft := make([]float64, mw*mh)
	for yy := 0; yy < mh; yy++ {
		for xx := 0; xx < mw; xx++ {
			sx, sy := xx-1, yy-1
			v := 4 * raw(sx, sy)
			v += 2 * (raw(sx-1, sy) + raw(sx+1, sy) + raw(sx, sy-1) + raw(sx, sy+1))
			v += raw(sx-1, sy-1) + raw(sx+1, sy-1) + raw(sx-1, sy+1) + raw(sx+1, sy+1)
			soft[yy*mw+xx] = v / 16
		}
	}
	src := func(xx, yy int) float64 {
		xx, yy = xx+1, yy+1
		if xx < 0 || yy < 0 || xx >= mw || yy >= mh {
			return 0
		}
		return soft[yy*mw+xx]
	}
	fillA := make([]float64, W*H)
	for Y := 0; Y < H; Y++ {
		fy := (float64(Y-pad)+0.5)/cleanScale - 0.5
		y0 := int(math.Floor(fy))
		ty := fy - float64(y0)
		for X := 0; X < W; X++ {
			fx := (float64(X-pad)+0.5)/cleanScale - 0.5
			x0 := int(math.Floor(fx))
			tx := fx - float64(x0)
			v := (src(x0, y0)*(1-tx)+src(x0+1, y0)*tx)*(1-ty) + (src(x0, y0+1)*(1-tx)+src(x0+1, y0+1)*tx)*ty
			fillA[Y*W+X] = smoothstep(0.38, 0.62, v)
		}
	}

	// The outline: how far each pixel is from the letters (a two-pass
	// chamfer distance), turned into a soft-edged ring outlineWidth wide.
	const inf = 1e9
	dist := make([]float64, W*H)
	for i, a := range fillA {
		if a >= 0.5 {
			dist[i] = 0
		} else {
			dist[i] = inf
		}
	}
	const d1, d2 = 1.0, math.Sqrt2
	for Y := 0; Y < H; Y++ {
		for X := 0; X < W; X++ {
			i := Y*W + X
			d := dist[i]
			if X > 0 {
				d = math.Min(d, dist[i-1]+d1)
			}
			if Y > 0 {
				d = math.Min(d, dist[i-W]+d1)
				if X > 0 {
					d = math.Min(d, dist[i-W-1]+d2)
				}
				if X < W-1 {
					d = math.Min(d, dist[i-W+1]+d2)
				}
			}
			dist[i] = d
		}
	}
	for Y := H - 1; Y >= 0; Y-- {
		for X := W - 1; X >= 0; X-- {
			i := Y*W + X
			d := dist[i]
			if X < W-1 {
				d = math.Min(d, dist[i+1]+d1)
			}
			if Y < H-1 {
				d = math.Min(d, dist[i+W]+d1)
				if X < W-1 {
					d = math.Min(d, dist[i+W+1]+d2)
				}
				if X > 0 {
					d = math.Min(d, dist[i+W-1]+d2)
				}
			}
			dist[i] = d
		}
	}

	img = image.NewNRGBA(image.Rect(0, 0, W, H))
	for i := range fillA {
		f := fillA[i]
		o := clamp01(outlineWidth + 0.5 - dist[i])
		a := math.Max(f, o)
		if a <= 0 {
			continue
		}
		// White letters over a black outline; where both, the letters win.
		g := f / a
		px := img.Pix[i*4 : i*4+4]
		lum := uint8(math.Round(255 * g))
		px[0], px[1], px[2] = lum, lum, lum
		px[3] = uint8(math.Round(255 * a))
	}
	h64 := fnv.New64a()
	h64.Write(img.Pix)
	x = (sp.x+minX)*cleanScale - pad
	y = (sp.y+minY)*cleanScale - pad
	return img, x, y, fmt.Sprintf("c%dx%d-%016x", W, H, h64.Sum64())
}

func luma(c color.NRGBA) float64 {
	return 0.2126*float64(c.R) + 0.7152*float64(c.G) + 0.0722*float64(c.B)
}

func smoothstep(a, b, v float64) float64 {
	t := clamp01((v - a) / (b - a))
	return t * t * (3 - 2*t)
}

func clamp01(v float64) float64 {
	if v < 0 {
		return 0
	}
	if v > 1 {
		return 1
	}
	return v
}
