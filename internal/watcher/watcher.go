// Package watcher detects new, removed and renamed video files in library
// folders by polling, without any filesystem notification dependency.
//
// Each poll walks every library root (directories only, plus Info() on video
// files; no file is ever opened) and compares a per-directory signature with
// the previous snapshot. A change is reported to OnChange only after it has
// settled, so large copies are not scanned half-written. This is deliberately
// portable: it behaves the same on NTFS, external USB drives and Linux, where
// change notifications are unreliable or unavailable.
package watcher

import (
	"context"
	"encoding/binary"
	"hash/fnv"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"time"
)

const (
	defaultInterval = 60 * time.Second
	defaultFollowUp = 90 * time.Second

	// maxWait caps how long a continuously changing library is held back
	// before OnChange fires anyway.
	maxWait = 30 * time.Minute

	// recentWindow is how young a changed video file's mtime must be for a
	// follow-up fire to be scheduled.
	recentWindow = 2 * time.Minute

	// slowPoll is the duration of a single root poll above which a warning
	// is logged.
	slowPoll = 30 * time.Second
)

// videoExts lists the file extensions (lower case) treated as video files.
var videoExts = map[string]bool{
	".mkv": true, ".mp4": true, ".m4v": true, ".avi": true, ".mov": true,
	".ts": true, ".m2ts": true, ".webm": true, ".wmv": true,
	".mpg": true, ".mpeg": true,
}

// skippedDirs lists system directory names (lower case) that are never walked.
var skippedDirs = map[string]bool{
	"system volume information": true,
	"#recycle":                  true,
	"@eadir":                    true,
}

// Root is one library folder to watch. A library with several folders has
// one Root per folder; each is polled and settled on its own.
type Root struct {
	LibraryID int64
	Path      string
}

// Watcher polls library folders and reports settled changes.
//
// Run is single goroutine and calls OnChange synchronously from it, so
// OnChange should return quickly (for example by starting a background scan).
// A Watcher must not be copied after first use, and Run must not be called
// concurrently.
type Watcher struct {
	// Roots returns the current libraries. It is called every poll, so
	// libraries may be added or removed at runtime. A root seen for the first
	// time only gets a baseline snapshot and never fires.
	Roots func(ctx context.Context) ([]Root, error)

	// OnChange is called from the watcher goroutine when a library's files
	// changed and the change has settled.
	OnChange func(libraryID int64)

	// Interval is the time between polls. Default 60s.
	Interval time.Duration

	// Settle is how long a library must show no further changes before
	// OnChange fires. Default 1.5 * Interval, so a change is reported on the
	// second quiet poll after it was first seen. A library that keeps changing
	// (a copy in progress) fires anyway after 30 minutes.
	Settle time.Duration

	// FollowUp is the delay of a second OnChange after a fire whose change
	// involved a video file modified within the last 2 minutes. The scanner
	// skips such files, so this lets it pick them up. Default 90s.
	FollowUp time.Duration

	// Log receives diagnostics. It may be nil.
	Log *slog.Logger

	// now is the clock; nil means time.Now. Tests replace it.
	now    func() time.Time
	states map[Root]*rootState
}

// rootState is the watcher's memory about one library folder.
type rootState struct {
	path        string
	snap        snapshot // nil until the baseline is taken
	unavailable bool
	pending     *pendingChange
	followUpAt  time.Time // zero when no follow-up is scheduled
}

// pendingChange is a detected change waiting to settle.
type pendingChange struct {
	first  time.Time // when the change was first seen
	last   time.Time // when the snapshot last changed
	dirs   int       // changed directories seen so far
	newest time.Time // newest mtime among added or modified video files
}

func (w *Watcher) interval() time.Duration {
	if w.Interval > 0 {
		return w.Interval
	}
	return defaultInterval
}

func (w *Watcher) settle() time.Duration {
	if w.Settle > 0 {
		return w.Settle
	}
	return w.interval() * 3 / 2
}

func (w *Watcher) followUp() time.Duration {
	if w.FollowUp > 0 {
		return w.FollowUp
	}
	return defaultFollowUp
}

func (w *Watcher) clock() time.Time {
	if w.now != nil {
		return w.now()
	}
	return time.Now()
}

func (w *Watcher) logger() *slog.Logger {
	if w.Log != nil {
		return w.Log
	}
	return slog.New(slog.DiscardHandler)
}

// Run polls until ctx is done. The first poll runs immediately and records
// baselines.
func (w *Watcher) Run(ctx context.Context) {
	t := time.NewTicker(w.interval())
	defer t.Stop()
	for {
		w.pollOnce(ctx)
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

// pollOnce performs one poll of every current root.
func (w *Watcher) pollOnce(ctx context.Context) {
	if w.states == nil {
		w.states = make(map[Root]*rootState)
	}
	log := w.logger()
	if w.Roots == nil {
		return
	}
	roots, err := w.Roots(ctx)
	if err != nil {
		if ctx.Err() == nil {
			log.Warn("watcher: listing libraries failed", "err", err)
		}
		return
	}

	live := make(map[Root]bool, len(roots))
	for _, r := range roots {
		live[r] = true
	}
	for r := range w.states {
		if !live[r] {
			delete(w.states, r)
		}
	}

	for _, r := range roots {
		if ctx.Err() != nil {
			return
		}
		w.pollRoot(ctx, r)
	}
}

func (w *Watcher) pollRoot(ctx context.Context, r Root) {
	log := w.logger()
	st := w.states[r]
	if st == nil {
		st = &rootState{path: r.Path}
		w.states[r] = st
	}

	start := time.Now()
	snap, err := scan(ctx, r.Path, st.snap)
	elapsed := time.Since(start)
	if ctx.Err() != nil {
		return
	}
	log.Debug("watcher: polled library", "library", r.LibraryID, "dirs", len(snap), "took", elapsed)
	if elapsed > slowPoll {
		log.Warn("watcher: slow library poll", "library", r.LibraryID, "path", r.Path, "took", elapsed)
	}

	if err != nil {
		// Typically an unplugged drive. Keep the old snapshot so the
		// library is not mistaken for "everything deleted".
		if !st.unavailable {
			log.Warn("watcher: library root unavailable", "library", r.LibraryID, "path", r.Path, "err", err)
		}
		st.unavailable = true
		return
	}
	if st.unavailable {
		log.Info("watcher: library root available again", "library", r.LibraryID, "path", r.Path)
		st.unavailable = false
	}

	now := w.clock()
	if st.snap == nil {
		st.snap = snap
		log.Debug("watcher: baseline recorded", "library", r.LibraryID, "dirs", len(snap))
		return
	}

	dirs, newest := diff(st.snap, snap)
	st.snap = snap
	if dirs > 0 {
		p := st.pending
		if p == nil {
			p = &pendingChange{first: now}
			st.pending = p
			log.Info("library change detected", "library", r.LibraryID, "dirs", dirs)
		} else {
			log.Debug("library still changing", "library", r.LibraryID, "dirs", dirs)
		}
		p.last = now
		p.dirs += dirs
		if newest.After(p.newest) {
			p.newest = newest
		}
	}
	w.maybeFire(st, r.LibraryID, now)
}

// maybeFire fires OnChange for a settled change or a due follow-up.
func (w *Watcher) maybeFire(st *rootState, id int64, now time.Time) {
	log := w.logger()
	if p := st.pending; p != nil {
		if now.Sub(p.last) < w.settle() && now.Sub(p.first) < maxWait {
			return
		}
		st.pending = nil
		st.followUpAt = time.Time{}
		if !p.newest.IsZero() && now.Sub(p.newest) < recentWindow {
			st.followUpAt = now.Add(w.followUp())
		}
		log.Info("library change settled, firing", "library", id, "dirs", p.dirs,
			"follow_up", !st.followUpAt.IsZero())
		w.fire(id)
		return
	}
	if !st.followUpAt.IsZero() && !now.Before(st.followUpAt) {
		st.followUpAt = time.Time{}
		log.Info("library follow-up, firing", "library", id)
		w.fire(id)
	}
}

func (w *Watcher) fire(id int64) {
	if w.OnChange != nil {
		w.OnChange(id)
	}
}

// fileStat is the part of a video file's metadata that identifies a change.
type fileStat struct {
	size int64
	mod  time.Time
}

// dirSnap is the recorded state of one directory.
type dirSnap struct {
	sig   uint64              // hash of video entries and subdirectory names
	files map[string]fileStat // video files directly inside the directory
}

// snapshot maps absolute directory paths to their state.
type snapshot map[string]*dirSnap

// scan walks root and returns a fresh snapshot. It fails only if the root
// itself cannot be read. A subdirectory that cannot be read keeps its
// previous state from prev, so a transient error is not reported as deletion.
func scan(ctx context.Context, root string, prev snapshot) (snapshot, error) {
	fi, err := os.Stat(root)
	if err != nil {
		return nil, err
	}
	if !fi.IsDir() {
		return nil, &os.PathError{Op: "watch", Path: root, Err: os.ErrInvalid}
	}
	snap := make(snapshot)
	if err := scanDir(ctx, root, root, prev, snap, map[string]bool{}); err != nil {
		return snap, err
	}
	return snap, nil
}

// scanDir records dir and the folders inside it, following linked folders
// as the scanner does. seen holds the real locations walked so far, so a
// link loop ends.
func scanDir(ctx context.Context, root, dir string, prev, out snapshot, seen map[string]bool) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if real, err := filepath.EvalSymlinks(dir); err == nil {
		if abs, err := filepath.Abs(real); err == nil {
			real = abs
		}
		if seen[real] {
			return nil
		}
		seen[real] = true
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		if dir == root {
			return err
		}
		keepPrev(dir, prev, out)
		return nil
	}

	h := fnv.New64a()
	var buf [8]byte
	put := func(v uint64) {
		binary.LittleEndian.PutUint64(buf[:], v)
		h.Write(buf[:])
	}
	files := make(map[string]fileStat)
	var subdirs []string

	// os.ReadDir returns entries sorted by name, so the hash is stable.
	for _, e := range entries {
		name := e.Name()
		isDir := e.IsDir()
		if !isDir && e.Type()&(os.ModeSymlink|os.ModeIrregular) != 0 {
			// A linked or junctioned folder reports false; look at what
			// it points at.
			if fi, err := os.Stat(filepath.Join(dir, name)); err == nil && fi.IsDir() {
				isDir = true
			}
		}
		if isDir {
			if skipDir(name) {
				continue
			}
			h.Write([]byte{'d'})
			h.Write([]byte(name))
			h.Write([]byte{0})
			subdirs = append(subdirs, filepath.Join(dir, name))
			continue
		}
		if !videoExts[strings.ToLower(filepath.Ext(name))] {
			continue
		}
		info, err := e.Info()
		if err == nil && e.Type()&os.ModeSymlink != 0 {
			info, err = os.Stat(filepath.Join(dir, name)) // the linked file
		}
		if err != nil {
			continue // vanished between ReadDir and Info
		}
		fs := fileStat{size: info.Size(), mod: info.ModTime()}
		files[name] = fs
		h.Write([]byte{'f'})
		h.Write([]byte(name))
		h.Write([]byte{0})
		put(uint64(fs.size))
		put(uint64(fs.mod.UnixNano()))
	}
	out[dir] = &dirSnap{sig: h.Sum64(), files: files}

	for _, sub := range subdirs {
		if err := scanDir(ctx, root, sub, prev, out, seen); err != nil {
			return err
		}
	}
	return nil
}

// keepPrev copies dir and all its descendants from prev into out.
func keepPrev(dir string, prev, out snapshot) {
	prefix := dir + string(filepath.Separator)
	for p, d := range prev {
		if p == dir || strings.HasPrefix(p, prefix) {
			out[p] = d
		}
	}
}

// skipDir reports whether a directory name is hidden or a system folder.
func skipDir(name string) bool {
	return strings.HasPrefix(name, ".") || strings.HasPrefix(name, "$") ||
		skippedDirs[strings.ToLower(name)]
}

// diff counts directories that were added, removed or changed between two
// snapshots, and returns the newest mtime among added or modified video files.
func diff(old, cur snapshot) (dirs int, newest time.Time) {
	for p, nd := range cur {
		od, ok := old[p]
		if ok && od.sig == nd.sig {
			continue
		}
		dirs++
		for name, nf := range nd.files {
			if ok {
				if of, had := od.files[name]; had && of == nf {
					continue
				}
			}
			if nf.mod.After(newest) {
				newest = nf.mod
			}
		}
	}
	for p := range old {
		if _, ok := cur[p]; !ok {
			dirs++
		}
	}
	return dirs, newest
}
