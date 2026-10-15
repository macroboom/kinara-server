package qr

import (
	"strings"
	"testing"
)

// Real decoding was checked with OpenCV for versions 1 to 40; these guard
// the structure.
func TestEncode(t *testing.T) {
	for _, tc := range []struct{ n, ver int }{{1, 1}, {14, 1}, {15, 2}, {84, 5}, {255, 12}, {256, 12}, {600, 19}, {2331, 40}} {
		c, err := Encode([]byte(strings.Repeat("a", tc.n)))
		if err != nil {
			t.Fatal(err)
		}
		if c.Version != tc.ver || c.Size != tc.ver*4+17 {
			t.Errorf("%d bytes: version %d size %d, want version %d", tc.n, c.Version, c.Size, tc.ver)
		}
		// Finder patterns: dark corners with a light ring inside.
		for _, p := range [][2]int{{0, 0}, {c.Size - 7, 0}, {0, c.Size - 7}} {
			if !c.Dark(p[0], p[1]) || c.Dark(p[0]+1, p[1]+1) || !c.Dark(p[0]+3, p[1]+3) {
				t.Errorf("%d bytes: finder at %v", tc.n, p)
			}
		}
		// The always-dark module.
		if !c.Dark(8, c.Size-8) {
			t.Errorf("%d bytes: dark module missing", tc.n)
		}
	}
	if _, err := Encode(make([]byte, 2332)); err != ErrTooLong {
		t.Fatalf("too long: %v", err)
	}
	svg := mustSVG(t, "endpointabc")
	if !strings.HasPrefix(svg, "<svg") || !strings.Contains(svg, `viewBox="0 0 29 29"`) {
		t.Fatalf("svg %q", svg[:60])
	}
}

func mustSVG(t *testing.T, s string) string {
	c, err := Encode([]byte(s))
	if err != nil {
		t.Fatal(err)
	}
	return c.SVG()
}
