// Package qr makes QR codes (byte mode, error correction level M), drawn as
// SVG, for pairing a device by scanning the server's remote access ticket.
// Written from the QR specification (ISO/IEC 18004) because the server
// takes no outside dependencies.
package qr

import (
	"errors"
	"fmt"
	"strings"
)

// Code is a finished QR code: Size x Size modules, true for dark.
type Code struct {
	Size    int
	Version int
	modules [][]bool
}

// Dark reports whether the module at (x, y) is dark.
func (c *Code) Dark(x, y int) bool { return c.modules[y][x] }

// Level M recovers about 15% damage: plenty for a screen.
var (
	eccPerBlock = [41]int{-1, 10, 16, 26, 18, 24, 16, 18, 22, 22, 26, 30, 22, 22, 24, 24, 28, 28, 26, 26, 26, 26, 28, 28, 28, 28, 28, 28, 28, 28, 28, 28, 28, 28, 28, 28, 28, 28, 28, 28, 28}
	numBlocks   = [41]int{-1, 1, 1, 1, 2, 2, 4, 4, 4, 5, 5, 5, 8, 9, 9, 10, 10, 11, 13, 14, 16, 17, 17, 18, 20, 21, 23, 25, 26, 28, 29, 31, 33, 35, 37, 38, 40, 43, 45, 47, 49}
)

// formatECL is level M's value in the format bits.
const formatECL = 0

// ErrTooLong: the data doesn't fit in the largest QR code.
var ErrTooLong = errors.New("qr: data too long")

// Encode makes the smallest QR code that holds data.
func Encode(data []byte) (*Code, error) {
	ver := 0
	for v := 1; v <= 40; v++ {
		ccBits := 8
		if v > 9 {
			ccBits = 16
		}
		if 4+ccBits+8*len(data) <= dataCodewords(v)*8 && (ccBits == 16 || len(data) < 256) {
			ver = v
			break
		}
	}
	if ver == 0 {
		return nil, ErrTooLong
	}
	ccBits := 8
	if ver > 9 {
		ccBits = 16
	}

	// Mode (byte), count, data, terminator, padding.
	var bits bitBuf
	bits.add(0b0100, 4)
	bits.add(len(data), ccBits)
	for _, b := range data {
		bits.add(int(b), 8)
	}
	capBits := dataCodewords(ver) * 8
	bits.add(0, min(4, capBits-len(bits)))
	bits.add(0, (8-len(bits)%8)%8)
	for pad := 0xEC; len(bits) < capBits; pad ^= 0xEC ^ 0x11 {
		bits.add(pad, 8)
	}
	codewords := make([]byte, len(bits)/8)
	for i, b := range bits {
		if b {
			codewords[i>>3] |= 1 << (7 - i&7)
		}
	}

	c := newCode(ver)
	c.drawFunctionPatterns()
	c.drawCodewords(addECC(codewords, ver))

	best, bestScore := 0, -1
	for m := 0; m < 8; m++ {
		c.applyMask(m)
		c.drawFormat(m)
		if s := c.penalty(); bestScore < 0 || s < bestScore {
			best, bestScore = m, s
		}
		c.applyMask(m) // masks are their own inverse
	}
	c.applyMask(best)
	c.drawFormat(best)
	return &Code{Size: c.size, Version: ver, modules: c.mod}, nil
}

type bitBuf []bool

func (b *bitBuf) add(v, n int) {
	for i := n - 1; i >= 0; i-- {
		*b = append(*b, (v>>i)&1 == 1)
	}
}

// rawModules is how many modules carry data and error correction bits.
func rawModules(ver int) int {
	r := (16*ver+128)*ver + 64
	if ver >= 2 {
		n := ver/7 + 2
		r -= (25*n-10)*n - 55
		if ver >= 7 {
			r -= 36
		}
	}
	return r
}

func dataCodewords(ver int) int {
	return rawModules(ver)/8 - eccPerBlock[ver]*numBlocks[ver]
}

// addECC splits the data into blocks, appends each block's Reed-Solomon
// codewords, and interleaves them.
func addECC(data []byte, ver int) []byte {
	nb, eccLen := numBlocks[ver], eccPerBlock[ver]
	raw := rawModules(ver) / 8
	numShort := nb - raw%nb
	shortLen := raw / nb
	div := rsDivisor(eccLen)
	blocks := make([][]byte, nb)
	k := 0
	for i := range blocks {
		n := shortLen - eccLen
		if i >= numShort {
			n++
		}
		dat := append([]byte(nil), data[k:k+n]...)
		k += n
		ecc := rsRemainder(dat, div)
		if i < numShort {
			dat = append(dat, 0) // placeholder, skipped below
		}
		blocks[i] = append(dat, ecc...)
	}
	out := make([]byte, 0, raw)
	for i := range blocks[0] {
		for j, b := range blocks {
			if i != shortLen-eccLen || j >= numShort {
				out = append(out, b[i])
			}
		}
	}
	return out
}

func gfMul(x, y byte) byte {
	z := 0
	for i := 7; i >= 0; i-- {
		z = (z << 1) ^ ((z >> 7) * 0x11D)
		z ^= int((y>>i)&1) * int(x)
	}
	return byte(z)
}

func rsDivisor(degree int) []byte {
	r := make([]byte, degree)
	r[degree-1] = 1
	root := byte(1)
	for i := 0; i < degree; i++ {
		for j := range r {
			r[j] = gfMul(r[j], root)
			if j+1 < len(r) {
				r[j] ^= r[j+1]
			}
		}
		root = gfMul(root, 2)
	}
	return r
}

func rsRemainder(data, div []byte) []byte {
	r := make([]byte, len(div))
	for _, b := range data {
		f := b ^ r[0]
		copy(r, r[1:])
		r[len(r)-1] = 0
		for i := range r {
			r[i] ^= gfMul(div[i], f)
		}
	}
	return r
}

type code struct {
	size int
	ver  int
	mod  [][]bool
	fn   [][]bool // function patterns, which masks skip
}

func newCode(ver int) *code {
	size := ver*4 + 17
	c := &code{size: size, ver: ver, mod: make([][]bool, size), fn: make([][]bool, size)}
	for i := range c.mod {
		c.mod[i] = make([]bool, size)
		c.fn[i] = make([]bool, size)
	}
	return c
}

func (c *code) set(x, y int, dark bool) {
	c.mod[y][x] = dark
	c.fn[y][x] = true
}

func (c *code) drawFunctionPatterns() {
	for i := 0; i < c.size; i++ {
		c.set(6, i, i%2 == 0)
		c.set(i, 6, i%2 == 0)
	}
	for _, p := range [][2]int{{3, 3}, {c.size - 4, 3}, {3, c.size - 4}} {
		for dy := -4; dy <= 4; dy++ {
			for dx := -4; dx <= 4; dx++ {
				x, y := p[0]+dx, p[1]+dy
				if x < 0 || y < 0 || x >= c.size || y >= c.size {
					continue
				}
				d := max(abs(dx), abs(dy))
				c.set(x, y, d != 2 && d != 4)
			}
		}
	}
	pos := alignmentPositions(c.ver)
	n := len(pos)
	for i := 0; i < n; i++ {
		for j := 0; j < n; j++ {
			if i == 0 && j == 0 || i == 0 && j == n-1 || i == n-1 && j == 0 {
				continue
			}
			for dy := -2; dy <= 2; dy++ {
				for dx := -2; dx <= 2; dx++ {
					c.set(pos[i]+dx, pos[j]+dy, max(abs(dx), abs(dy)) != 1)
				}
			}
		}
	}
	c.drawFormat(0) // reserves the area; drawn for real after masking
	if c.ver >= 7 {
		rem := c.ver
		for i := 0; i < 12; i++ {
			rem = (rem << 1) ^ ((rem >> 11) * 0x1F25)
		}
		bits := c.ver<<12 | rem
		for i := 0; i < 18; i++ {
			dark := (bits>>i)&1 == 1
			a, b := c.size-11+i%3, i/3
			c.set(a, b, dark)
			c.set(b, a, dark)
		}
	}
}

func alignmentPositions(ver int) []int {
	if ver == 1 {
		return nil
	}
	n := ver/7 + 2
	step := 26
	if ver != 32 {
		step = (ver*4 + n*2 + 1) / (n*2 - 2) * 2
	}
	out := make([]int, n)
	out[0] = 6
	for i, p := n-1, ver*4+17-7; i >= 1; i, p = i-1, p-step {
		out[i] = p
	}
	return out
}

func (c *code) drawFormat(mask int) {
	data := formatECL<<3 | mask
	rem := data
	for i := 0; i < 10; i++ {
		rem = (rem << 1) ^ ((rem >> 9) * 0x537)
	}
	bits := (data<<10 | rem) ^ 0x5412
	bit := func(i int) bool { return (bits>>i)&1 == 1 }
	for i := 0; i <= 5; i++ {
		c.set(8, i, bit(i))
	}
	c.set(8, 7, bit(6))
	c.set(8, 8, bit(7))
	c.set(7, 8, bit(8))
	for i := 9; i < 15; i++ {
		c.set(14-i, 8, bit(i))
	}
	for i := 0; i < 8; i++ {
		c.set(c.size-1-i, 8, bit(i))
	}
	for i := 8; i < 15; i++ {
		c.set(8, c.size-15+i, bit(i))
	}
	c.set(8, c.size-8, true)
}

func (c *code) drawCodewords(data []byte) {
	i := 0
	for right := c.size - 1; right >= 1; right -= 2 {
		if right == 6 {
			right = 5
		}
		for vert := 0; vert < c.size; vert++ {
			for j := 0; j < 2; j++ {
				x := right - j
				y := vert
				if (right+1)&2 == 0 {
					y = c.size - 1 - vert
				}
				if !c.fn[y][x] && i < len(data)*8 {
					c.mod[y][x] = (data[i>>3]>>(7-i&7))&1 == 1
					i++
				}
			}
		}
	}
}

func (c *code) applyMask(m int) {
	for y := 0; y < c.size; y++ {
		for x := 0; x < c.size; x++ {
			if c.fn[y][x] {
				continue
			}
			var inv bool
			switch m {
			case 0:
				inv = (x+y)%2 == 0
			case 1:
				inv = y%2 == 0
			case 2:
				inv = x%3 == 0
			case 3:
				inv = (x+y)%3 == 0
			case 4:
				inv = (x/3+y/2)%2 == 0
			case 5:
				inv = x*y%2+x*y%3 == 0
			case 6:
				inv = (x*y%2+x*y%3)%2 == 0
			case 7:
				inv = ((x+y)%2+x*y%3)%2 == 0
			}
			if inv {
				c.mod[y][x] = !c.mod[y][x]
			}
		}
	}
}

// penalty scores a masked code the way the specification does (runs,
// blocks, finder look-alikes, balance). Any mask decodes; a low score
// just scans more easily.
func (c *code) penalty() int {
	score := 0
	n := c.size
	line := func(get func(i int) bool) {
		run, prev := 0, false
		for i := 0; i < n; i++ {
			v := get(i)
			if i > 0 && v == prev {
				run++
			} else {
				if run >= 5 {
					score += 3 + run - 5
				}
				run, prev = 1, v
			}
		}
		if run >= 5 {
			score += 3 + run - 5
		}
		// 1:1:3:1:1 finder look-alikes with four light modules on a side.
		for i := 0; i+11 <= n; i++ {
			a := [11]bool{}
			for k := range a {
				a[k] = get(i + k)
			}
			p1 := !a[0] && !a[1] && !a[2] && !a[3] && a[4] && !a[5] && a[6] && a[7] && a[8] && !a[9] && a[10]
			p2 := a[0] && !a[1] && a[2] && a[3] && a[4] && !a[5] && a[6] && !a[7] && !a[8] && !a[9] && !a[10]
			if p1 || p2 {
				score += 40
			}
		}
	}
	for y := 0; y < n; y++ {
		line(func(i int) bool { return c.mod[y][i] })
	}
	for x := 0; x < n; x++ {
		line(func(i int) bool { return c.mod[i][x] })
	}
	dark := 0
	for y := 0; y < n; y++ {
		for x := 0; x < n; x++ {
			if c.mod[y][x] {
				dark++
			}
			if x+1 < n && y+1 < n {
				v := c.mod[y][x]
				if c.mod[y][x+1] == v && c.mod[y+1][x] == v && c.mod[y+1][x+1] == v {
					score += 3
				}
			}
		}
	}
	total := n * n
	k := (abs(dark*20-total*10) + total - 1) / total
	return score + max(k-1, 0)*10
}

func abs(x int) int {
	if x < 0 {
		return -x
	}
	return x
}

// SVG draws the code with a four-module quiet zone, dark on light, scaled
// to fit any size (it has a viewBox and no fixed width).
func (c *Code) SVG() string {
	const quiet = 4
	full := c.Size + 2*quiet
	var b strings.Builder
	fmt.Fprintf(&b, `<svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 %d %d" shape-rendering="crispEdges">`, full, full)
	fmt.Fprintf(&b, `<rect width="%d" height="%d" fill="#fff"/><path fill="#000" d="`, full, full)
	for y := 0; y < c.Size; y++ {
		for x := 0; x < c.Size; x++ {
			if !c.modules[y][x] {
				continue
			}
			// Merge runs along the row into one rectangle.
			run := 1
			for x+run < c.Size && c.modules[y][x+run] {
				run++
			}
			fmt.Fprintf(&b, "M%d %dh%dv1h-%dz", x+quiet, y+quiet, run, run)
			x += run - 1
		}
	}
	b.WriteString(`"/></svg>`)
	return b.String()
}
