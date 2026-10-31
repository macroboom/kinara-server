// Package jobs runs slow background work one item at a time: preview
// thumbnails for every file, and intro and end credits detection for TV
// seasons. Work for a title someone just started playing jumps the queue.
package jobs

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"time"

	"kinara/internal/ffmpeg"
	"kinara/internal/intro"
	"kinara/internal/store"
	"kinara/internal/trickplay"
)

// Worker does the background work.
type Worker struct {
	Store     *store.Store
	Tool      *ffmpeg.Tool
	Trickplay *trickplay.Maker // nil turns previews off
	// Intros turns intro detection on.
	Intros bool
	// ThumbDir is where pictures for "Other videos" titles are written
	// ("" turns them off).
	ThumbDir string
	Log      *slog.Logger
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
	CreditsFound int    `json:"creditsFound"`
	SeasonsDone  int    `json:"seasonsDone"`
	PicturesMade int    `json:"picturesMade"`
	// Current is the job running now, if any.
	Current *Task `json:"current,omitempty"`
	// Recent lists finished jobs, newest first.
	Recent []Task `json:"recent"`
}

// Task is one piece of background work.
type Task struct {
	// Kind is "previews", "intros" or "picture".
	Kind string `json:"kind"`
	// Name is what it was for: a file name, or a season's folder.
	Name string `json:"name"`
	// Path is the full path, for a tooltip.
	Path      string  `json:"path,omitempty"`
	StartedAt int64   `json:"startedAt"`
	TookSec   float64 `json:"tookSec,omitempty"`
	Failed    bool    `json:"failed,omitempty"`
	// Result says what came of it ("14 of 16 episodes have an intro, 12 have
	// end credits").
	Result string `json:"result,omitempty"`
}

// keepRecent is how many finished jobs the status keeps.
const keepRecent = 60

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
	st := w.status
	if st.Current != nil {
		c := *st.Current
		st.Current = &c
	}
	st.Recent = append([]Task{}, w.status.Recent...)
	return st
}

func (w *Worker) setDoing(s string) {
	w.mu.Lock()
	w.status.Doing = s
	if s == "" {
		w.status.Current = nil
	}
	w.mu.Unlock()
}

// begin marks a job as running. path is shown by its last part.
func (w *Worker) begin(kind, label, path string) *Task {
	t := &Task{Kind: kind, Name: filepath.Base(path), Path: path, StartedAt: time.Now().Unix()}
	w.mu.Lock()
	w.status.Doing = label + ": " + path
	c := *t
	w.status.Current = &c
	w.mu.Unlock()
	return t
}

// step updates the running job's file (intro detection reads a season one
// episode at a time) without starting a new log line.
func (w *Worker) step(label, path string) {
	w.mu.Lock()
	w.status.Doing = label + ": " + path
	if w.status.Current != nil {
		w.status.Current.Path = path
	}
	w.mu.Unlock()
}

// finish logs a job as done.
func (w *Worker) finish(t *Task, failed bool, result string) {
	t.TookSec = time.Since(time.Unix(t.StartedAt, 0)).Round(100 * time.Millisecond).Seconds()
	t.Failed, t.Result = failed, result
	w.mu.Lock()
	w.status.Recent = append([]Task{*t}, w.status.Recent...)
	if len(w.status.Recent) > keepRecent {
		w.status.Recent = w.status.Recent[:keepRecent]
	}
	w.status.Current = nil
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
	// Pictures for "Other videos" first: they're quick, and without one the
	// title shows as a blank tile.
	if w.ThumbDir != "" {
		if t, ok := w.nextThumb(ctx); ok {
			w.makeThumb(ctx, t)
			return true
		}
	}
	if w.Intros {
		seasons, err := w.Store.SeasonsNeedingIntros(ctx, 50)
		if err != nil {
			w.Log.Warn("intro queue", "err", err)
		} else {
			for _, id := range seasons {
				if w.hasFailed("season:" + id) {
					continue
				}
				w.detectSeason(ctx, id)
				// Still waiting after a pass (saving failed: a full disk,
				// say): leave it until the server restarts rather than
				// decoding the season again straight away, for ever.
				if again, err := w.Store.SeasonsNeedingIntros(ctx, 50); err == nil && slices.Contains(again, id) && ctx.Err() == nil {
					w.Log.Warn("intro detection didn't finish a season; skipping it until the server restarts", "season", id)
					w.markFailed("season:" + id)
				}
				return true
			}
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
	task := w.begin("previews", "previews", v.RelPath)
	start := time.Now()
	man, err := w.Trickplay.Make(ctx, v)
	if ctx.Err() != nil {
		return
	}
	if err != nil {
		w.Log.Warn("previews failed", "file", v.RelPath, "err", err)
		w.finish(task, true, shortErr(err))
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
	w.finish(task, false, plural(man.Count, "preview frame"))
	w.Log.Debug("previews made", "file", v.RelPath, "count", man.Count,
		"took", time.Since(start).Round(100*time.Millisecond))
}

// introAudio is how much of each episode is compared. Intros almost always
// start within the first few minutes, after any cold open.
const introAudio = 10 * time.Minute

// detectSeason finds the intros and end credits of a season's episodes,
// each only when some episode hasn't been checked for it yet.
func (w *Worker) detectSeason(ctx context.Context, seasonID string) {
	files, err := w.Store.SeasonIntroFiles(ctx, seasonID)
	if err != nil {
		w.Log.Warn("intro files", "season", seasonID, "err", err)
		return
	}
	// One file per episode (the best copy is listed first).
	var eps []store.IntroCandidate
	seen := map[int]bool{}
	needIntros, needCredits := false, false
	for _, f := range files {
		if seen[f.Episode] {
			continue
		}
		seen[f.Episode] = true
		eps = append(eps, f)
		needIntros = needIntros || !f.Checked
		needCredits = needCredits || !f.CreditsChecked
	}
	if len(eps) == 0 {
		return
	}
	// The season's folder names the job (the show and season, usually).
	task := w.begin("intros", "intro detection", eps[0].Path)
	task.Name = filepath.Base(filepath.Dir(eps[0].Path))
	w.mu.Lock()
	w.status.Current.Name = task.Name
	w.mu.Unlock()
	start := time.Now()

	var parts []string
	unread := 0
	intros, credits := -1, -1
	if needIntros {
		intros, unread = w.seasonIntros(ctx, eps)
		if ctx.Err() != nil {
			return
		}
		parts = append(parts, fmt.Sprintf("%d of %d episodes have an intro", intros, len(eps)))
	}
	if needCredits {
		credits = w.seasonCredits(ctx, eps)
		if ctx.Err() != nil {
			return
		}
		if needIntros {
			parts = append(parts, fmt.Sprintf("%d have end credits", credits))
		} else {
			parts = append(parts, fmt.Sprintf("%d of %d episodes have end credits", credits, len(eps)))
		}
	}
	// Other copies of the same episodes get checked too, so the season
	// isn't picked again because of them.
	for _, f := range files {
		if containsVersion(eps, f.VersionID) {
			continue
		}
		if !f.Checked {
			_ = w.Store.SetIntro(ctx, f.VersionID, store.Intro{}, f.Mtime)
		}
		if !f.CreditsChecked {
			_ = w.Store.SetEndCredits(ctx, f.VersionID, store.EndCredits{}, f.Mtime)
		}
	}
	w.mu.Lock()
	w.status.IntrosFound += max(intros, 0)
	w.status.CreditsFound += max(credits, 0)
	w.status.SeasonsDone++
	w.mu.Unlock()
	if unread > 0 {
		parts = append(parts, fmt.Sprintf("%d couldn't be read", unread))
	}
	task.Path = filepath.Dir(eps[0].Path)
	w.finish(task, needIntros && unread == len(eps), strings.Join(parts, ", "))
	w.Log.Info("intro detection", "season", seasonID, "episodes", len(eps), "intros", intros, "credits", credits,
		"took", time.Since(start).Round(100*time.Millisecond))
}

// seasonIntros finds and saves the intro of each episode, and returns how
// many have one and how many couldn't be read.
func (w *Worker) seasonIntros(ctx context.Context, eps []store.IntroCandidate) (found, unread int) {
	prints := make([][]uint32, len(eps))
	readable := make([]bool, len(eps))
	for i, f := range eps {
		if ctx.Err() != nil {
			return 0, 0
		}
		w.step("intro detection", f.Path)
		samples, err := intro.Decode(ctx, w.Tool.Path(), f.Path, introAudio)
		if err != nil {
			w.Log.Warn("intro audio", "file", f.Path, "err", err)
			continue
		}
		prints[i] = intro.Fingerprint(samples)
		readable[i] = true
	}
	ranges := intro.Detect(prints)
	for i, f := range eps {
		in := store.Intro{}
		if !readable[i] {
			// Couldn't read it: mark it checked anyway so the season isn't
			// retried every pass. A replaced file is checked again.
			unread++
		} else if r, ok := ranges[i]; ok {
			in = store.Intro{Start: r.Start, End: r.End}
			found++
		}
		if err := w.Store.SetIntro(ctx, f.VersionID, in, f.Mtime); err != nil {
			w.Log.Warn("save intro", "err", err)
		}
	}
	return found, unread
}

// seasonCredits finds and saves the end credits of each episode, and
// returns how many have them. The ends of the episodes are compared by
// audio first; an episode whose credits share no audio with the others is
// then checked by picture (text on black).
func (w *Worker) seasonCredits(ctx context.Context, eps []store.IntroCandidate) (found int) {
	prints := make([][]uint32, len(eps))
	from := make([]time.Duration, len(eps))
	length := make([]time.Duration, len(eps))
	searchable := make([]bool, len(eps))
	for i, f := range eps {
		if ctx.Err() != nil {
			return 0
		}
		length[i] = time.Duration(f.Duration * float64(time.Second))
		start, ok := intro.TailStart(length[i])
		if !ok {
			continue // unknown length or very short: nothing to search
		}
		from[i], searchable[i] = start, true
		w.step("credits detection", f.Path)
		samples, err := intro.DecodeFrom(ctx, w.Tool.Path(), f.Path, start, length[i]-start)
		if err != nil {
			w.Log.Warn("credits audio", "file", f.Path, "err", err)
			continue
		}
		prints[i] = intro.Fingerprint(samples)
	}
	ranges := intro.DetectIn(prints, intro.CreditsBounds)
	for i, f := range eps {
		if ctx.Err() != nil {
			return 0
		}
		c := store.EndCredits{}
		off := from[i].Seconds()
		if r, ok := ranges[i]; ok && searchable[i] {
			c = store.EndCredits{Start: r.Start + off, End: r.End + off, Source: "audio"}
		} else if searchable[i] {
			w.step("credits detection (picture)", f.Path)
			r, ok, err := intro.BlackCredits(ctx, w.Tool.Path(), f.Path, from[i], length[i]-from[i])
			if ctx.Err() != nil {
				return 0
			}
			if err != nil {
				w.Log.Warn("credits picture", "file", f.Path, "err", err)
			} else if ok {
				c = store.EndCredits{Start: r.Start, End: r.End, Source: "picture"}
			}
		}
		if c.End > 0 {
			found++
		}
		if err := w.Store.SetEndCredits(ctx, f.VersionID, c, f.Mtime); err != nil {
			w.Log.Warn("save credits", "err", err)
		}
	}
	return found
}

func containsVersion(list []store.IntroCandidate, id string) bool {
	for _, c := range list {
		if c.VersionID == id {
			return true
		}
	}
	return false
}

func plural(n int, what string) string {
	if n == 1 {
		return "1 " + what
	}
	return fmt.Sprintf("%d %ss", n, what)
}

// shortErr keeps an error to its first line, for the log on the page.
func shortErr(err error) string {
	s := err.Error()
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		s = s[:i]
	}
	if len(s) > 160 {
		s = s[:160] + "…"
	}
	return s
}
