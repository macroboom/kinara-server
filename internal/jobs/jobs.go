// Package jobs runs slow background work one item at a time: preview
// thumbnails for every file, and intro detection for TV seasons. Work for
// a title someone just started playing jumps the queue.
package jobs

import (
	"context"
	"log/slog"
	"os"
	"sync"
	"time"

	"kinora/internal/ffmpeg"
	"kinora/internal/intro"
	"kinora/internal/store"
	"kinora/internal/trickplay"
)

// Worker does the background work.
type Worker struct {
	Store     *store.Store
	Tool      *ffmpeg.Tool
	Trickplay *trickplay.Maker // nil turns previews off
	// Intros turns intro detection on.
	Intros bool
	Log    *slog.Logger
	// Idle is how long to wait between passes when there's nothing to do.
	Idle time.Duration

	mu     sync.Mutex
	urgent []string // version IDs to make previews for first
	// failed holds files whose previews failed since the server started, so
	// a file that can't even record its failure isn't retried in a loop.
	failed map[string]bool
	kick   chan struct{}
	status Status
}

// Status is shown on the server's web page.
type Status struct {
	Doing        string `json:"doing,omitempty"`
	PreviewsMade int    `json:"previewsMade"`
	IntrosFound  int    `json:"introsFound"`
	SeasonsDone  int    `json:"seasonsDone"`
}

func (w *Worker) init() {
	w.mu.Lock()
	if w.kick == nil {
		w.kick = make(chan struct{}, 1)
	}
	w.mu.Unlock()
}

// Kick starts a pass now (after a scan finished, say).
func (w *Worker) Kick() {
	w.init()
	select {
	case w.kick <- struct{}{}:
	default:
	}
}

// Prioritize makes previews for a file next, ahead of the backlog. Called
// when someone starts watching it.
func (w *Worker) Prioritize(versionID string) {
	if w.Trickplay == nil {
		return
	}
	w.mu.Lock()
	for _, id := range w.urgent {
		if id == versionID {
			w.mu.Unlock()
			return
		}
	}
	w.urgent = append(w.urgent, versionID)
	w.mu.Unlock()
	w.Kick()
}

// Status returns what the worker is doing.
func (w *Worker) Status() Status {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.status
}

func (w *Worker) setDoing(s string) {
	w.mu.Lock()
	w.status.Doing = s
	w.mu.Unlock()
}

// Run works until ctx is done.
func (w *Worker) Run(ctx context.Context) {
	w.init()
	idle := w.Idle
	if idle <= 0 {
		idle = 10 * time.Minute
	}
	// Let startup (and the first library scan) settle before heavy work.
	select {
	case <-ctx.Done():
		return
	case <-time.After(2 * time.Minute):
	case <-w.kick:
	}
	for {
		busy := w.pass(ctx)
		if ctx.Err() != nil {
			return
		}
		w.setDoing("")
		if busy {
			continue
		}
		select {
		case <-ctx.Done():
			return
		case <-w.kick:
		case <-time.After(idle):
		}
	}
}

// pass does one unit of work and reports whether there may be more.
func (w *Worker) pass(ctx context.Context) bool {
	if id := w.nextUrgent(); id != "" {
		v, err := w.Store.GetVersion(ctx, id)
		if err == nil && !v.Missing && w.Trickplay.Needed(v) && !w.hasFailed(v.ID) {
			w.makePreviews(ctx, v)
		}
		return true
	}
	if w.Intros {
		seasons, err := w.Store.SeasonsNeedingIntros(ctx, 1)
		if err != nil {
			w.Log.Warn("intro queue", "err", err)
		} else if len(seasons) > 0 {
			w.detectSeason(ctx, seasons[0])
			return true
		}
	}
	if w.Trickplay != nil {
		versions, err := w.Store.AllVersions(ctx)
		if err != nil {
			w.Log.Warn("preview queue", "err", err)
			return false
		}
		for _, v := range versions {
			if ctx.Err() != nil {
				return false
			}
			if w.hasUrgent() {
				return true
			}
			if _, err := os.Stat(v.Path); err != nil {
				continue // drive unplugged: try again next pass
			}
			if w.Trickplay.Needed(v) && !w.hasFailed(v.ID) {
				w.makePreviews(ctx, v)
				return true
			}
		}
	}
	return false
}

func (w *Worker) nextUrgent() string {
	w.mu.Lock()
	defer w.mu.Unlock()
	if len(w.urgent) == 0 {
		return ""
	}
	id := w.urgent[0]
	w.urgent = w.urgent[1:]
	return id
}

func (w *Worker) hasFailed(id string) bool {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.failed[id]
}

func (w *Worker) hasUrgent() bool {
	w.mu.Lock()
	defer w.mu.Unlock()
	return len(w.urgent) > 0
}

func (w *Worker) makePreviews(ctx context.Context, v store.Version) {
	w.setDoing("previews: " + v.RelPath)
	start := time.Now()
	man, err := w.Trickplay.Make(ctx, v)
	if ctx.Err() != nil {
		return
	}
	if err != nil {
		w.Log.Warn("previews failed", "file", v.RelPath, "err", err)
		w.mu.Lock()
		if w.failed == nil {
			w.failed = map[string]bool{}
		}
		w.failed[v.ID] = true
		w.mu.Unlock()
		return
	}
	w.mu.Lock()
	w.status.PreviewsMade++
	w.mu.Unlock()
	w.Log.Info("previews made", "file", v.RelPath, "count", man.Count,
		"took", time.Since(start).Round(100*time.Millisecond))
}

// introAudio is how much of each episode is compared. Intros almost always
// start within the first few minutes, after any cold open.
const introAudio = 10 * time.Minute

func (w *Worker) detectSeason(ctx context.Context, seasonID string) {
	files, err := w.Store.SeasonIntroFiles(ctx, seasonID)
	if err != nil {
		w.Log.Warn("intro files", "season", seasonID, "err", err)
		return
	}
	// One file per episode (the best copy is listed first).
	var eps []store.IntroCandidate
	seen := map[int]bool{}
	for _, f := range files {
		if seen[f.Episode] {
			continue
		}
		seen[f.Episode] = true
		eps = append(eps, f)
	}
	start := time.Now()
	prints := make([][]uint32, len(eps))
	readable := make([]bool, len(eps))
	for i, f := range eps {
		if ctx.Err() != nil {
			return
		}
		w.setDoing("intro detection: " + f.Path)
		samples, err := intro.Decode(ctx, w.Tool.Path(), f.Path, introAudio)
		if err != nil {
			w.Log.Warn("intro audio", "file", f.Path, "err", err)
			continue
		}
		prints[i] = intro.Fingerprint(samples)
		readable[i] = true
	}
	found := intro.Detect(prints)
	n := 0
	for i, f := range eps {
		if !readable[i] {
			// Couldn't read it: mark it checked anyway so the season isn't
			// retried every pass. A replaced file is checked again.
			_ = w.Store.SetIntro(ctx, f.VersionID, store.Intro{}, f.Mtime)
			continue
		}
		r, ok := found[i]
		in := store.Intro{}
		if ok {
			in = store.Intro{Start: r.Start, End: r.End}
			n++
		}
		if err := w.Store.SetIntro(ctx, f.VersionID, in, f.Mtime); err != nil {
			w.Log.Warn("save intro", "err", err)
		}
	}
	// Other copies of the same episodes get checked too, so the season
	// isn't picked again because of them.
	for _, f := range files {
		if !f.Checked && !containsVersion(eps, f.VersionID) {
			_ = w.Store.SetIntro(ctx, f.VersionID, store.Intro{}, f.Mtime)
		}
	}
	w.mu.Lock()
	w.status.IntrosFound += n
	w.status.SeasonsDone++
	w.mu.Unlock()
	w.Log.Info("intro detection", "season", seasonID, "episodes", len(eps), "intros", n,
		"took", time.Since(start).Round(100*time.Millisecond))
}

func containsVersion(list []store.IntroCandidate, id string) bool {
	for _, c := range list {
		if c.VersionID == id {
			return true
		}
	}
	return false
}
