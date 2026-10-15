package images

import (
	"context"
	"image"
	"image/color"
	"image/jpeg"
	"os"
	"path/filepath"
	"testing"
)

// Pictures the server made itself ("file:" sources) are read from disk and
// resized like downloaded artwork.
func TestFileSource(t *testing.T) {
	dir := t.TempDir()
	src := filepath.Join(dir, "frame.jpg")
	img := image.NewRGBA(image.Rect(0, 0, 640, 360))
	for y := 0; y < 360; y++ {
		for x := 0; x < 640; x++ {
			img.Set(x, y, color.RGBA{40, 120, 220, 255})
		}
	}
	f, err := os.Create(src)
	if err != nil {
		t.Fatal(err)
	}
	if err := jpeg.Encode(f, img, nil); err != nil {
		t.Fatal(err)
	}
	f.Close()

	c := New(filepath.Join(dir, "cache"))
	path, err := c.Get(context.Background(), "file:"+src, 320)
	if err != nil {
		t.Fatal(err)
	}
	out, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer out.Close()
	cfg, _, err := image.DecodeConfig(out)
	if err != nil || cfg.Width != 320 {
		t.Fatalf("resized = %+v, %v", cfg, err)
	}
	if _, err := c.Get(context.Background(), "file:"+filepath.Join(dir, "missing.jpg"), 320); err == nil {
		t.Fatal("a missing file must fail")
	}
}
