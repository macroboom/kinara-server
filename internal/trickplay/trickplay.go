// Package trickplay makes the small preview pictures shown above the
// timeline while skipping through a video: one frame every Interval
// seconds, packed 10 by 10 into JPEG sheets.
package trickplay

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"image/jpeg"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"time"

	"kinara/internal/ffmpeg"
	"kinara/internal/store"
)

const (
	// Interval is the seconds between preview frames.
	Interval = 10
	// Width is each preview's width in pixels.
	Width = 240
	// Cols and Rows are how many previews fit on one sheet.
	Cols = 10
	Rows = 10
)

// Manifest describes a file's previews. Preview n (time n*Interval) is on
// sheet n/(Cols*Rows), at column n%Cols and row (n/Cols)%Rows.
type Manifest struct {
	Interval   int    `json:"interval"`
	Width      int    `json:"width"`
	Height     int    `json:"height"`
	Cols       int    `json:"cols"`
	Rows       int    `json:"rows"`
	Count      int    `json:"count"`
	Sheets     int    `json:"sheets"`
	FileMtime  int64  `json:"fileMtime"`
	MadeAt     int64  `json:"madeAt"`
	FailReason string `json:"failReason,omitempty"`
	// Colour is how HDR was turned into SDR for the sheets: "placebo",
	// "zscale" or "" (none). HDR previews are remade when a better way
	// becomes available (Dolby Vision through zscale comes out pink).
	Colour string `json:"colour,omitempty"`
}

// colourMethod names the HDR conversion this ffmpeg will use.
func colourMethod(c ffmpeg.Caps) string {
	switch {
	case c.Placebo:
		return "placebo"
	case c.ToneMap:
		return "zscale"
	}
	return ""
}

// Maker writes previews under Dir/<version ID>/.
type Maker struct {
	Tool *ffmpeg.Tool
	Dir  string
}

func (m *Maker) dir(versionID string) string { return filepath.Join(m.Dir, versionID) }

// Sheet returns the path of one sheet (numbered from 1).
func (m *Maker) Sheet(versionID string, n int) string {
	return filepath.Join(m.dir(versionID), strconv.Itoa(n)+".jpg")
}

// Load returns a file's manifest, or an error if previews aren't made (or
// were made for an older copy of the file). A manifest with FailReason set
// records a failed attempt so it isn't retried every pass.
func (m *Maker) Load(v store.Version) (Manifest, error) {
	var man Manifest
	b, err := os.ReadFile(filepath.Join(m.dir(v.ID), "manifest.json"))
	if err != nil {
		return man, err
	}
	if err := json.Unmarshal(b, &man); err != nil {
		return man, err
	}
	if man.FileMtime != v.Mtime {
		return man, errors.New("file changed since previews were made")
	}
	return man, nil
}

// Ready reports whether usable previews exist.
func (m *Maker) Ready(v store.Version) (Manifest, bool) {
	man, err := m.Load(v)
	return man, err == nil && man.FailReason == "" && man.Count > 0
}

// Needed reports whether previews should be made (none yet, or the file
// changed). Failed attempts are retried after a week.
func (m *Maker) Needed(v store.Version) bool {
	man, err := m.Load(v)
	if err != nil {
		return true
	}
	if man.FailReason != "" {
		return time.Since(time.Unix(man.MadeAt, 0)) > 7*24*time.Hour
	}
	if v.HDR != "" && man.Colour != colourMethod(m.Tool.Caps()) {
		return true
	}
	return false
}

// Make generates previews for a file. It reads only keyframes, so it's
// quick for most files, and runs at low priority.
func (m *Maker) Make(ctx context.Context, v store.Version) (Manifest, error) {
	caps := m.Tool.Caps()
	if !caps.Found {
		return Manifest{}, errors.New("ffmpeg not found")
	}
	final := m.dir(v.ID)
	// Every failure is written down, so the queue moves on instead of
	// trying the same file again straight away.
	fail := func(err error) (Manifest, error) {
		man := Manifest{FileMtime: v.Mtime, MadeAt: time.Now().Unix(), FailReason: err.Error()}
		_ = os.MkdirAll(final, 0o755)
		_ = writeManifest(final, man)
		return man, err
	}
	if v.DurationSec <= 0 {
		return fail(errors.New("unknown duration"))
	}
	tmp := final + ".tmp"
	_ = os.RemoveAll(tmp)
	if err := os.MkdirAll(tmp, 0o755); err != nil {
		return fail(err)
	}
	defer os.RemoveAll(tmp)

	run := func(tonemap bool) error {
		vf := fmt.Sprintf("fps=1/%d,scale=%d:-2", Interval, Width)
		if tonemap {
			vf = fmt.Sprintf("fps=1/%d,scale=%d:-2,%s", Interval, Width, caps.HDRToSDR())
		}
		vf += fmt.Sprintf(",tile=%dx%d", Cols, Rows)
		cmd := exec.CommandContext(ctx, m.Tool.Path(),
			"-nostdin", "-hide_banner", "-loglevel", "error",
			"-skip_frame", "nokey", "-i", v.Path,
			"-map", "0:v:0", "-an", "-sn", "-dn",
			"-vf", vf, "-q:v", "6", "-threads", "2",
			"-f", "image2", "-start_number", "1", filepath.Join(tmp, "%d.jpg"))
		tail := &ffmpeg.Tail{N: 1024}
		cmd.Stderr = tail
		if err := ffmpeg.StartLow(cmd); err != nil {
			return err
		}
		if err := cmd.Wait(); err != nil {
			if tail.String() != "" {
				return fmt.Errorf("ffmpeg: %s", tail.String())
			}
			return err
		}
		return nil
	}
	tonemap := v.HDR != "" && caps.HDRToSDR() != ""
	err := run(tonemap)
	if err != nil && ctx.Err() == nil && tonemap {
		err = run(false) // tone mapping failed (an unusual format): plain previews
	}
	if ctx.Err() != nil {
		return Manifest{}, ctx.Err()
	}
	man := Manifest{Interval: Interval, Cols: Cols, Rows: Rows, FileMtime: v.Mtime, MadeAt: time.Now().Unix()}
	if v.HDR != "" {
		// Recorded even when it fell back to plain previews, so a file the
		// filter can't handle isn't retried on every pass.
		man.Colour = colourMethod(caps)
	}
	if err == nil {
		err = m.measure(tmp, &man, v.DurationSec)
	}
	if err != nil {
		return fail(err)
	}
	if err := writeManifest(tmp, man); err != nil {
		return fail(err)
	}
	if err := replaceDir(tmp, final); err != nil {
		// Probably a sheet locked for a moment (Windows): not the file's
		// fault, so no failure is recorded and a later pass tries again.
		return man, err
	}
	return man, nil
}

// replaceDir puts the new previews in place. Renaming the folder is
// quickest, but on Windows it fails while a sheet is open (being served, or
// scanned by antivirus); then the files are moved one at a time, replacing
// the old ones, with the manifest last so it never describes missing sheets.
func replaceDir(tmp, final string) error {
	_ = os.RemoveAll(final)
	if err := os.Rename(tmp, final); err == nil {
		return nil
	}
	if err := os.MkdirAll(final, 0o755); err != nil {
		return err
	}
	entries, err := os.ReadDir(tmp)
	if err != nil {
		return err
	}
	for _, e := range entries {
		if e.Name() == "manifest.json" {
			continue
		}
		if err := os.Rename(filepath.Join(tmp, e.Name()), filepath.Join(final, e.Name())); err != nil {
			return err
		}
	}
	return os.Rename(filepath.Join(tmp, "manifest.json"), filepath.Join(final, "manifest.json"))
}

// measure reads the first sheet's size to learn each preview's height, and
// counts sheets and previews.
func (m *Maker) measure(dir string, man *Manifest, duration float64) error {
	f, err := os.Open(filepath.Join(dir, "1.jpg"))
	if err != nil {
		return errors.New("no previews were produced")
	}
	cfg, err := jpeg.DecodeConfig(f)
	f.Close()
	if err != nil {
		return fmt.Errorf("reading previews: %w", err)
	}
	man.Width = cfg.Width / Cols
	man.Height = cfg.Height / Rows
	for n := 1; ; n++ {
		if _, err := os.Stat(filepath.Join(dir, strconv.Itoa(n)+".jpg")); err != nil {
			man.Sheets = n - 1
			break
		}
	}
	man.Count = min(int(math.Ceil(duration/Interval)), man.Sheets*Cols*Rows)
	return nil
}

func writeManifest(dir string, man Manifest) error {
	b, err := json.Marshal(man)
	if err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(dir, "manifest.json"), b, 0o644)
}

// Remove deletes a file's previews.
func (m *Maker) Remove(versionID string) error { return os.RemoveAll(m.dir(versionID)) }
