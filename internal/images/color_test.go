package images

import (
	"fmt"
	"image"
	"image/color"
	"testing"
)

func fill(c color.Color, w, h int) *image.RGBA {
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	for y := range h {
		for x := range w {
			img.Set(x, y, c)
		}
	}
	return img
}

func TestDominantPicksVividHue(t *testing.T) {
	img := fill(color.RGBA{20, 20, 20, 255}, 100, 100)
	// A third of the picture strong blue, a little red.
	for y := range 100 {
		for x := range 33 {
			img.Set(x, y, color.RGBA{30, 90, 220, 255})
		}
		for x := 90; x < 95; x++ {
			img.Set(x, y, color.RGBA{220, 30, 30, 255})
		}
	}
	got := Dominant(img)
	if got == "" {
		t.Fatal("no colour")
	}
	var r, g, b int
	if _, err := fmtSscanf(got, &r, &g, &b); err != nil {
		t.Fatal(err)
	}
	if !(b > r && b > g) {
		t.Fatalf("want a blue, got %s", got)
	}
}

func TestDominantGreyIsEmpty(t *testing.T) {
	if got := Dominant(fill(color.RGBA{128, 128, 128, 255}, 50, 50)); got != "" {
		t.Fatalf("want empty for grey, got %s", got)
	}
}

func fmtSscanf(s string, r, g, b *int) (int, error) {
	return fmt.Sscanf(s, "#%02x%02x%02x", r, g, b)
}
