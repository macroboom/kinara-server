package images

import (
	"bytes"
	"context"
	"image"
	"image/color"
	"image/png"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync/atomic"
	"testing"
)

func TestSnapWidth(t *testing.T) {
	for in, want := range map[int]int{0: 0, 100: 160, 160: 160, 161: 240, 500: 640, 1920: 1920, 4000: 0} {
		if got := SnapWidth(in); got != want {
			t.Errorf("SnapWidth(%d) = %d, want %d", in, got, want)
		}
	}
}

func TestResize(t *testing.T) {
	src := image.NewRGBA(image.Rect(0, 0, 1000, 1500))
	for y := 0; y < 1500; y++ {
		for x := 0; x < 1000; x++ {
			src.Set(x, y, color.RGBA{R: 200, G: 100, B: 50, A: 255})
		}
	}
	out := Resize(src, 400)
	if out.Bounds().Dx() != 400 || out.Bounds().Dy() != 600 {
		t.Fatalf("size %v", out.Bounds())
	}
	r, g, b, _ := out.At(200, 300).RGBA()
	if r>>8 != 200 || g>>8 != 100 || b>>8 != 50 {
		t.Errorf("colour changed: %d %d %d", r>>8, g>>8, b>>8)
	}
	if small := Resize(src, 2000); small != image.Image(src) {
		t.Error("should not upscale")
	}
}

func TestCacheGet(t *testing.T) {
	var buf bytes.Buffer
	png.Encode(&buf, image.NewRGBA(image.Rect(0, 0, 800, 1200)))
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		w.Write(buf.Bytes())
	}))
	defer srv.Close()

	c := New(t.TempDir())
	p1, err := c.Get(context.Background(), srv.URL+"/poster.png", 300)
	if err != nil {
		t.Fatal(err)
	}
	f, _ := os.Open(p1)
	cfg, format, err := image.DecodeConfig(f)
	f.Close()
	if err != nil || format != "jpeg" || cfg.Width != 320 {
		t.Fatalf("resized: %v %s %d", err, format, cfg.Width)
	}
	if _, err := c.Get(context.Background(), srv.URL+"/poster.png", 300); err != nil {
		t.Fatal(err)
	}
	if hits.Load() != 1 {
		t.Errorf("want one download, got %d", hits.Load())
	}
}

func TestSources(t *testing.T) {
	const o = "https://image.tmdb.org/t/p/original/abc.jpg"
	cases := []struct {
		w    int
		want []string
	}{
		{160, []string{"https://image.tmdb.org/t/p/w185/abc.jpg", o}},
		{640, []string{"https://image.tmdb.org/t/p/w780/abc.jpg", o}},
		{1280, []string{"https://image.tmdb.org/t/p/w1280/abc.jpg", o}},
		{1920, []string{o}},
		{0, []string{o}},
	}
	for _, c := range cases {
		if got := sources(o, c.w); strings.Join(got, " ") != strings.Join(c.want, " ") {
			t.Errorf("sources(%d) = %v, want %v", c.w, got, c.want)
		}
	}
	if got := sources("http://other/x.jpg", 320); len(got) != 1 {
		t.Errorf("non-TMDB URL rewritten: %v", got)
	}
}
