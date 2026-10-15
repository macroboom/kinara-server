package images

import (
	"context"
	"fmt"
	"image"
	"math"
	"os"
)

// Color returns the image's dominant vivid colour as "#rrggbb", tuned to
// read well as an accent on a dark page, or "" when the image is mostly
// grey. It works from the smallest cached copy, and remembers each answer.
func (c *Cache) Color(ctx context.Context, srcURL string) (string, error) {
	if v, ok := c.colors.Load(srcURL); ok {
		return v.(string), nil
	}
	path, err := c.Get(ctx, srcURL, Widths[0])
	if err != nil {
		return "", err
	}
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	img, _, err := image.Decode(f)
	f.Close()
	if err != nil {
		return "", fmt.Errorf("decode image: %w", err)
	}
	col := Dominant(img)
	c.colors.Store(srcURL, col)
	return col, nil
}

// Dominant picks the most prominent saturated hue: pixels are grouped by
// hue, weighted by how vivid they are, and the heaviest group's average
// colour wins. Near-black, near-white and grey pixels don't count.
func Dominant(img image.Image) string {
	const bins = 24
	var weight [bins]float64
	var sumR, sumG, sumB [bins]float64
	var total, vivid float64

	b := img.Bounds()
	step := max(1, int(math.Sqrt(float64(b.Dx()*b.Dy())/4000)))
	for y := b.Min.Y; y < b.Max.Y; y += step {
		for x := b.Min.X; x < b.Max.X; x += step {
			r16, g16, b16, _ := img.At(x, y).RGBA()
			r, g, bl := float64(r16>>8)/255, float64(g16>>8)/255, float64(b16>>8)/255
			h, s, v := hsv(r, g, bl)
			total++
			if v < 0.18 || s < 0.22 || (v > 0.95 && s < 0.3) {
				continue
			}
			w := s * v
			i := int(h/360*bins) % bins
			weight[i] += w
			sumR[i] += r * w
			sumG[i] += g * w
			sumB[i] += bl * w
			vivid += w
		}
	}
	if total == 0 || vivid/total < 0.03 {
		return ""
	}
	// A bin and its neighbours together, so a hue split across a bin edge
	// isn't undercounted; ties go to the heavier middle bin.
	group := func(i int) float64 { return weight[(i+bins-1)%bins] + weight[i] + weight[(i+1)%bins] }
	best := 0
	for i := range bins {
		if g := group(i); g > group(best) || (g == group(best) && weight[i] > weight[best]) {
			best = i
		}
	}
	var w, sr, sg, sb float64
	for _, i := range []int{(best + bins - 1) % bins, best, (best + 1) % bins} {
		w += weight[i]
		sr += sumR[i]
		sg += sumG[i]
		sb += sumB[i]
	}
	h, s, v := hsv(sr/w, sg/w, sb/w)
	// Readable on a dark page: lively but not neon.
	s = clamp(s, 0.45, 0.85)
	v = clamp(v, 0.7, 0.92)
	r, g, bl := rgb(h, s, v)
	return fmt.Sprintf("#%02x%02x%02x", int(r*255+0.5), int(g*255+0.5), int(bl*255+0.5))
}

func clamp(x, lo, hi float64) float64 { return math.Min(hi, math.Max(lo, x)) }

func hsv(r, g, b float64) (h, s, v float64) {
	mx := math.Max(r, math.Max(g, b))
	mn := math.Min(r, math.Min(g, b))
	v = mx
	d := mx - mn
	if mx > 0 {
		s = d / mx
	}
	if d == 0 {
		return 0, s, v
	}
	switch mx {
	case r:
		h = math.Mod((g-b)/d, 6)
	case g:
		h = (b-r)/d + 2
	default:
		h = (r-g)/d + 4
	}
	h *= 60
	if h < 0 {
		h += 360
	}
	return h, s, v
}

func rgb(h, s, v float64) (r, g, b float64) {
	c := v * s
	x := c * (1 - math.Abs(math.Mod(h/60, 2)-1))
	m := v - c
	switch {
	case h < 60:
		r, g, b = c, x, 0
	case h < 120:
		r, g, b = x, c, 0
	case h < 180:
		r, g, b = 0, c, x
	case h < 240:
		r, g, b = 0, x, c
	case h < 300:
		r, g, b = x, 0, c
	default:
		r, g, b = c, 0, x
	}
	return r + m, g + m, b + m
}
