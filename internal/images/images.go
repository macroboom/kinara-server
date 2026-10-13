// Package images downloads artwork once and serves resized copies, so a TV
// grid never has to decode full-size posters.
package images

import (
	"context"
	"crypto/sha1"
	"encoding/hex"
	"fmt"
	"image"
	"image/color"
	"image/jpeg"
	_ "image/png"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"time"
)

// resizeSlots limits how many images are decoded and resized at once. A TV
// grid asks for dozens of posters together; resizing them all in parallel
// starves everything else (including streams) of CPU.
var resizeSlots = make(chan struct{}, max(2, runtime.NumCPU()/2))

// Widths are the sizes served. Requests round up to the next one, which
// keeps the cache small and lets clients share entries.
var Widths = []int{160, 240, 320, 400, 640, 960, 1280, 1920}

// Cache stores originals and resized copies on disk.
type Cache struct {
	Dir  string
	HTTP *http.Client

	locks sync.Map // key -> *sync.Mutex
}

// New returns a cache rooted at dir.
func New(dir string) *Cache {
	return &Cache{Dir: dir, HTTP: &http.Client{Timeout: 30 * time.Second}}
}

// SnapWidth rounds a requested width up to a served size; 0 means original.
func SnapWidth(w int) int {
	if w <= 0 {
		return 0
	}
	for _, s := range Widths {
		if w <= s {
			return s
		}
	}
	return 0
}

func (c *Cache) lock(key string) func() {
	m, _ := c.locks.LoadOrStore(key, &sync.Mutex{})
	mu := m.(*sync.Mutex)
	mu.Lock()
	return mu.Unlock
}

// Get returns a local file path for the image at srcURL, resized to width
// (0 for the original). The first request downloads it.
//
// TMDB "original" images can be 4K and several MB, so when a smaller copy
// will do, the matching TMDB size is downloaded instead (falling back to the
// original if TMDB doesn't have that size for this image).
func (c *Cache) Get(ctx context.Context, srcURL string, width int) (string, error) {
	width = SnapWidth(width)
	var lastErr error
	for _, src := range sources(srcURL, width) {
		path, err := c.get(ctx, src, width)
		if err == nil {
			return path, nil
		}
		lastErr = err
	}
	return "", lastErr
}

func (c *Cache) get(ctx context.Context, srcURL string, width int) (string, error) {
	sum := sha1.Sum([]byte(srcURL))
	key := hex.EncodeToString(sum[:])

	unlock := c.lock(key)
	defer unlock()

	orig := filepath.Join(c.Dir, "orig", key[:2], key)
	if _, err := os.Stat(orig); err != nil {
		if err := c.download(ctx, srcURL, orig); err != nil {
			return "", err
		}
	}
	if width == 0 {
		return orig, nil
	}
	sized := filepath.Join(c.Dir, fmt.Sprintf("w%d", width), key[:2], key+".jpg")
	if _, err := os.Stat(sized); err == nil {
		return sized, nil
	}
	select {
	case resizeSlots <- struct{}{}:
	case <-ctx.Done():
		return "", ctx.Err()
	}
	defer func() { <-resizeSlots }()
	if err := resizeFile(orig, sized, width); err != nil {
		return "", err
	}
	return sized, nil
}

// sources lists URLs to try for an image at a width: the smallest TMDB size
// at least that wide, then the original.
func sources(srcURL string, width int) []string {
	const orig = "/t/p/original/"
	i := strings.Index(srcURL, orig)
	if i < 0 || width == 0 {
		return []string{srcURL}
	}
	var size string
	switch {
	case width <= 185:
		size = "w185"
	case width <= 780:
		size = "w780"
	case width <= 1280:
		size = "w1280"
	default:
		return []string{srcURL}
	}
	sized := srcURL[:i] + "/t/p/" + size + "/" + srcURL[i+len(orig):]
	return []string{sized, srcURL}
}

func (c *Cache) download(ctx context.Context, url, dst string) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return err
	}
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("download image: HTTP %d", resp.StatusCode)
	}
	return writeAtomic(dst, func(w io.Writer) error {
		_, err := io.Copy(w, io.LimitReader(resp.Body, 50<<20))
		return err
	})
}

// writeAtomic writes through a temp file so readers never see half a file.
func writeAtomic(dst string, write func(io.Writer) error) error {
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return err
	}
	f, err := os.CreateTemp(filepath.Dir(dst), ".tmp-*")
	if err != nil {
		return err
	}
	tmp := f.Name()
	if err := write(f); err != nil {
		f.Close()
		os.Remove(tmp)
		return err
	}
	if err := f.Close(); err != nil {
		os.Remove(tmp)
		return err
	}
	return os.Rename(tmp, dst)
}

func resizeFile(src, dst string, width int) error {
	f, err := os.Open(src)
	if err != nil {
		return err
	}
	img, _, err := image.Decode(f)
	f.Close()
	if err != nil {
		return fmt.Errorf("decode image: %w", err)
	}
	out := Resize(img, width)
	return writeAtomic(dst, func(w io.Writer) error {
		return jpeg.Encode(w, out, &jpeg.Options{Quality: 85})
	})
}

// Resize scales img down to width, keeping its aspect ratio, by averaging
// the source pixels under each output pixel. Images already narrower than
// width are returned unchanged.
func Resize(img image.Image, width int) image.Image {
	b := img.Bounds()
	sw, sh := b.Dx(), b.Dy()
	if width <= 0 || sw <= width || sh == 0 {
		return img
	}
	height := max(1, int(float64(sh)*float64(width)/float64(sw)+0.5))
	dst := image.NewRGBA(image.Rect(0, 0, width, height))
	xScale := float64(sw) / float64(width)
	yScale := float64(sh) / float64(height)
	for dy := 0; dy < height; dy++ {
		y0 := int(float64(dy) * yScale)
		y1 := max(y0+1, int(float64(dy+1)*yScale))
		for dx := 0; dx < width; dx++ {
			x0 := int(float64(dx) * xScale)
			x1 := max(x0+1, int(float64(dx+1)*xScale))
			var r, g, bl, a, n uint64
			for y := y0; y < y1 && y < sh; y++ {
				for x := x0; x < x1 && x < sw; x++ {
					cr, cg, cb, ca := img.At(b.Min.X+x, b.Min.Y+y).RGBA()
					r += uint64(cr)
					g += uint64(cg)
					bl += uint64(cb)
					a += uint64(ca)
					n++
				}
			}
			if n == 0 {
				continue
			}
			dst.SetRGBA(dx, dy, color.RGBA{
				R: uint8(r / n >> 8), G: uint8(g / n >> 8), B: uint8(bl / n >> 8), A: uint8(a / n >> 8),
			})
		}
	}
	return dst
}
