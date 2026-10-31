// Cue parsing and Timing adapted from Silo (https://github.com/Silo-Server/silo-server),
// internal/subtitles/cues.go and internal/subtitles/timing.go at commit
// 01e01b0417d2937ed577ce996ce1ef5ae108311e. Copyright (C) the Silo authors,
// licensed under the GNU Affero General Public License v3.0. Changes for
// Kinara: WebVTT only (Kinara serves every text subtitle as WebVTT), and a
// simpler retimer that rewrites timing lines. Modifications Copyright (C) the
// Kinara authors, under the same license.

package subsync

import (
	"bytes"
	"errors"
	"fmt"
	"math"
	"strconv"
	"strings"
	"time"
)

// Cue is one timed subtitle entry.
type Cue struct {
	Start time.Duration
	End   time.Duration
	Lines []string
}

// Timing maps an original subtitle time t to t*Scale + Offset. Scale is
// applied first, so a frame-rate correction doesn't stretch the offset.
type Timing struct {
	Scale    float64 `json:"scale"` // 0 means 1
	OffsetMS int     `json:"offsetMs"`
}

// Normalized returns t with a zero Scale replaced by 1.
func (t Timing) Normalized() Timing {
	if t.Scale == 0 {
		t.Scale = 1
	}
	return t
}

// IsIdentity reports whether t leaves every time as it is.
func (t Timing) IsIdentity() bool {
	n := t.Normalized()
	return n.Scale == 1 && n.OffsetMS == 0
}

// Apply maps an original time to its corrected time (which may be negative).
func (t Timing) Apply(d time.Duration) time.Duration {
	n := t.Normalized()
	if n.Scale != 1 {
		d = time.Duration(math.Round(float64(d) * n.Scale))
	}
	return d + time.Duration(n.OffsetMS)*time.Millisecond
}

// ErrNoCuesFound reports subtitle text with no timed cues.
var ErrNoCuesFound = errors.New("no subtitle cues found")

// ParseCues reads WebVTT (or SRT) text into cues, skipping malformed ones.
func ParseCues(data []byte) ([]Cue, error) {
	text := strings.TrimPrefix(string(data), "\ufeff")
	text = strings.ReplaceAll(text, "\r\n", "\n")
	text = strings.ReplaceAll(text, "\r", "\n")
	var cues []Cue
	for _, block := range strings.Split(text, "\n\n") {
		lines := strings.Split(strings.Trim(block, "\n"), "\n")
		at := -1
		for i, l := range lines {
			if strings.Contains(l, "-->") {
				at = i
				break
			}
		}
		if at < 0 {
			continue
		}
		start, end, err := parseTimingLine(lines[at])
		if err != nil {
			continue
		}
		body := lines[at+1:]
		for len(body) > 0 && strings.TrimSpace(body[len(body)-1]) == "" {
			body = body[:len(body)-1]
		}
		if len(body) == 0 {
			continue
		}
		cues = append(cues, Cue{Start: start, End: end, Lines: body})
	}
	if len(cues) == 0 {
		return nil, ErrNoCuesFound
	}
	return cues, nil
}

func parseTimingLine(line string) (time.Duration, time.Duration, error) {
	left, right, ok := strings.Cut(line, "-->")
	if !ok {
		return 0, 0, errors.New("no arrow")
	}
	start, err := parseTimestamp(strings.TrimSpace(left))
	if err != nil {
		return 0, 0, err
	}
	endField := strings.TrimSpace(right)
	if sp := strings.IndexAny(endField, " \t"); sp >= 0 {
		endField = endField[:sp]
	}
	end, err := parseTimestamp(endField)
	if err != nil {
		return 0, 0, err
	}
	if start < 0 || end < start {
		return 0, 0, errors.New("bad cue range")
	}
	return start, end, nil
}

// parseTimestamp reads HH:MM:SS.mmm, MM:SS.mmm and SRT's comma form.
func parseTimestamp(s string) (time.Duration, error) {
	s = strings.Replace(strings.TrimSpace(s), ",", ".", 1)
	parts := strings.Split(s, ":")
	if len(parts) < 2 || len(parts) > 3 {
		return 0, fmt.Errorf("bad timestamp %q", s)
	}
	h := 0
	if len(parts) == 3 {
		n, err := strconv.Atoi(parts[0])
		if err != nil {
			return 0, fmt.Errorf("bad timestamp %q", s)
		}
		h, parts = n, parts[1:]
	}
	m, err := strconv.Atoi(parts[0])
	if err != nil {
		return 0, fmt.Errorf("bad timestamp %q", s)
	}
	secs, frac, _ := strings.Cut(parts[1], ".")
	sec, err := strconv.Atoi(secs)
	if err != nil {
		return 0, fmt.Errorf("bad timestamp %q", s)
	}
	ms := 0
	if frac != "" {
		for len(frac) < 3 {
			frac += "0"
		}
		if ms, err = strconv.Atoi(frac[:3]); err != nil {
			return 0, fmt.Errorf("bad timestamp %q", s)
		}
	}
	return time.Duration(h)*time.Hour + time.Duration(m)*time.Minute + time.Duration(sec)*time.Second + time.Duration(ms)*time.Millisecond, nil
}

func formatVTT(d time.Duration) string {
	if d < 0 {
		d = 0
	}
	ms := int64(d / time.Millisecond)
	return fmt.Sprintf("%02d:%02d:%02d.%03d", ms/3_600_000, ms/60_000%60, ms/1000%60, ms%1000)
}

// RetimeVTT applies t to every cue of a WebVTT document. A cue that ends at
// or before zero is dropped; one that starts before zero starts at zero.
// Everything but the timing lines is kept as it is.
func RetimeVTT(data []byte, t Timing) []byte {
	if t.IsIdentity() {
		return data
	}
	text := strings.ReplaceAll(string(data), "\r\n", "\n")
	var out bytes.Buffer
	out.Grow(len(text) + 64)
	blocks := strings.Split(text, "\n\n")
	first := true
	for _, block := range blocks {
		lines := strings.Split(block, "\n")
		keep := true
		for i, l := range lines {
			if !strings.Contains(l, "-->") {
				continue
			}
			start, end, err := parseTimingLine(l)
			if err != nil {
				break
			}
			s, e := t.Apply(start), t.Apply(end)
			if e <= 0 {
				keep = false
				break
			}
			_, right, _ := strings.Cut(l, "-->")
			settings := ""
			if r := strings.TrimSpace(right); strings.ContainsAny(r, " \t") {
				settings = r[strings.IndexAny(r, " \t"):]
			}
			lines[i] = formatVTT(max(s, 0)) + " --> " + formatVTT(e) + settings
			break
		}
		if !keep {
			continue
		}
		if !first {
			out.WriteString("\n\n")
		}
		first = false
		out.WriteString(strings.Join(lines, "\n"))
	}
	return out.Bytes()
}
