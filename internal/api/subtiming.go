package api

import (
	"context"
	"errors"
	"fmt"
	"os"
	"sort"
	"strconv"
	"sync"
	"time"

	"kinara/internal/store"
	"kinara/internal/subsync"
)

// Subtitle timing for every subtitle file, not only the ones the server
// finds by itself (autosubs.go checks those as it downloads them). Files
// next to the video and ones downloaded by hand are checked against the
// film's speech here, once, in the background, and the correction is kept
// with the file: every app gets the corrected WebVTT (serveExternalVTT),
// the Roku included, with nothing new on its side.
//
// What gets checked, and when:
//   - a file someone is playing, or a subtitle someone picks: straight away
//     (it's what they're watching);
//   - the rest of the library: one video at a time, only while nobody is
//     watching anything, and not at all when the owner turns it off
//     (Manage, Subtitles). Reading a film's speech takes about 45
//     CPU-seconds at low priority, once per film (it's cached), so a big
//     library takes a while the first time.
//
// A check is made against the video's and the subtitle's modification
// times (store.SubtitleFile.SyncKey): an edited .srt, or a new release of
// the film at the same path, is checked again. A file that doesn't line up
// with the speech is kept as it is (it's the owner's own, or someone's
// pick) and marked no_match.
//
// The file's own subtitle tracks aren't checked: they're timed for that
// file, and players show them by themselves when playing it directly, so
// a correction from here wouldn't reach every player anyway.

const (
	keySubsCheckTiming = "subtitles_check_timing"
	// timingGap paces background checks between files.
	timingGap = 30 * time.Second
	// timingReplan is how often the library is looked over for files to check.
	timingReplan = 6 * time.Hour
	// timingRetry is the key kept when the speech couldn't be read for a
	// reason that may pass (ffmpeg missing, a read error): it never matches,
	// so the next look over the library tries again.
	timingRetry = "retry"
)

type subTimingState struct {
	mu      sync.Mutex
	urgent  []int64
	queued  map[int64]bool
	pending []int64
	kick    chan struct{}
	replan  bool
	// speech holds a lock per version, so two checks of one film read
	// its speech once (the second finds it cached).
	speech sync.Map
}

// timingInBackground says whether the whole library is checked (on unless
// the owner turned it off). Files someone plays or picks are checked either way.
func (s *Server) timingInBackground(ctx context.Context) bool {
	v, _ := s.Store.Value(ctx, keySubsCheckTiming)
	return v != "false"
}

// timingKey is what a check is made against: the video's and the
// subtitle file's modification times.
func timingKey(videoMtime, subMtime int64) string {
	return strconv.FormatInt(videoMtime, 10) + ":" + strconv.FormatInt(subMtime, 10)
}

// needsTimingCheck says whether f should be checked against key, or only
// have key recorded (adopt: checked before keys were kept).
func needsTimingCheck(f store.SubtitleFile, key string) (check, adopt bool) {
	switch {
	case f.SyncState == "":
		return true, false
	case f.SyncKey == "":
		return false, true
	default:
		return f.SyncKey != key, false
	}
}

// subtitleFileKey is f's current key, for v.
func (s *Server) subtitleFileKey(f store.SubtitleFile, v store.Version) (string, os.FileInfo, error) {
	fi, err := os.Stat(s.dataPath(f.Path))
	if err != nil {
		return "", nil, err
	}
	return timingKey(v.Mtime, fi.ModTime().Unix()), fi, nil
}

func (s *Server) timingKick() {
	s.timing.mu.Lock()
	if s.timing.kick == nil {
		s.timing.kick = make(chan struct{}, 1)
	}
	k := s.timing.kick
	s.timing.mu.Unlock()
	select {
	case k <- struct{}{}:
	default:
	}
}

// checkTimingSoon puts subtitle files at the front of the line. Ones that
// don't need a check are passed over quickly.
func (s *Server) checkTimingSoon(ids ...int64) {
	if len(ids) == 0 {
		return
	}
	s.timing.mu.Lock()
	if s.timing.queued == nil {
		s.timing.queued = map[int64]bool{}
	}
	for _, id := range ids {
		if !s.timing.queued[id] {
			s.timing.queued[id] = true
			s.timing.urgent = append(s.timing.urgent, id)
		}
	}
	s.timing.mu.Unlock()
	s.timingKick()
}

// timingPlaying is told when a file starts playing: its subtitle files
// are checked now.
func (s *Server) timingPlaying(ctx context.Context, versionID string) {
	files, err := s.Store.SubtitleFiles(ctx, versionID)
	if err != nil {
		return
	}
	ids := make([]int64, 0, len(files))
	for _, f := range files {
		ids = append(ids, f.ID)
	}
	s.checkTimingSoon(ids...)
}

// replanTiming has the library looked over again soon (after a scan).
func (s *Server) replanTiming() {
	s.timing.mu.Lock()
	s.timing.replan = true
	s.timing.mu.Unlock()
	s.timingKick()
}

// planTiming lists the subtitle files to check in the background: the
// newest files first, each video's together (its speech is read once).
func (s *Server) planTiming(ctx context.Context) {
	all, err := s.Store.AllSubtitleFiles(ctx)
	if err != nil {
		s.Log.Warn("subtitle timing: couldn't list subtitle files", "err", err)
		return
	}
	type group struct {
		newest int64
		ids    []int64
	}
	var groups []group
	for versionID, files := range all {
		v, err := s.Store.GetVersion(ctx, versionID)
		if err != nil {
			continue
		}
		var g group
		for _, f := range files {
			key, _, err := s.subtitleFileKey(f, v)
			if err != nil {
				continue
			}
			check, adopt := needsTimingCheck(f, key)
			if adopt {
				_ = s.Store.SetSubtitleSyncKey(ctx, f.ID, key)
			}
			if check {
				g.ids = append(g.ids, f.ID)
				g.newest = max(g.newest, f.AddedAt, v.AddedAt)
			}
		}
		if len(g.ids) > 0 {
			groups = append(groups, g)
		}
	}
	sort.Slice(groups, func(i, j int) bool { return groups[i].newest > groups[j].newest })
	var pending []int64
	for _, g := range groups {
		pending = append(pending, g.ids...)
	}
	s.timing.mu.Lock()
	s.timing.pending = pending
	s.timing.mu.Unlock()
	if len(pending) > 0 {
		s.Log.Info("subtitle timing: files to check", "count", len(pending))
	}
}

// nextTiming is the next file to check: one asked for first, then the
// background list while nobody is watching (and it's turned on).
func (s *Server) nextTiming(ctx context.Context) (id int64, urgent, ok bool) {
	s.timing.mu.Lock()
	if len(s.timing.urgent) > 0 {
		id = s.timing.urgent[0]
		s.timing.urgent = s.timing.urgent[1:]
		delete(s.timing.queued, id)
		s.timing.mu.Unlock()
		return id, true, true
	}
	waiting := len(s.timing.pending) > 0
	s.timing.mu.Unlock()
	if !waiting || !s.timingInBackground(ctx) || len(s.playing.list(time.Now())) > 0 {
		return 0, false, false
	}
	s.timing.mu.Lock()
	defer s.timing.mu.Unlock()
	if len(s.timing.pending) == 0 {
		return 0, false, false
	}
	id = s.timing.pending[0]
	s.timing.pending = s.timing.pending[1:]
	return id, false, true
}

// RunSubtitleTiming checks subtitle files against the speech until ctx ends.
func (s *Server) RunSubtitleTiming(ctx context.Context) {
	if s.Subtitles == nil {
		return
	}
	s.timingKick()
	s.timing.mu.Lock()
	kick := s.timing.kick
	s.timing.mu.Unlock()
	s.planTiming(ctx)
	replan := time.NewTicker(timingReplan)
	defer replan.Stop()
	// Background files wait while someone watches: look again now and then.
	poll := time.NewTicker(time.Minute)
	defer poll.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-replan.C:
			s.planTiming(ctx)
		case <-poll.C:
		case <-kick:
		}
		s.timing.mu.Lock()
		again := s.timing.replan
		s.timing.replan = false
		s.timing.mu.Unlock()
		if again {
			s.planTiming(ctx)
		}
		for ctx.Err() == nil {
			id, urgent, ok := s.nextTiming(ctx)
			if !ok {
				break
			}
			if !s.checkSubtitleTiming(ctx, id) || urgent {
				continue
			}
			select {
			case <-ctx.Done():
				return
			case <-kick:
				// Something asked for: the loop takes it first.
			case <-time.After(timingGap):
			}
		}
	}
}

// checkSubtitleTiming checks one subtitle file against the speech, if it
// needs it, and keeps the result. worked says it did a check (so the
// background slows down after it).
func (s *Server) checkSubtitleTiming(ctx context.Context, id int64) (worked bool) {
	f, err := s.Store.SubtitleFile(ctx, id)
	if err != nil {
		return false
	}
	v, err := s.Store.GetVersion(ctx, f.VersionID)
	if err != nil {
		return false
	}
	key, fi, err := s.subtitleFileKey(f, v)
	if err != nil {
		return false
	}
	check, adopt := needsTimingCheck(f, key)
	if adopt {
		_ = s.Store.SetSubtitleSyncKey(ctx, f.ID, key)
	}
	if !check {
		return false
	}
	vtt, err := s.Subtitles.VTTFile(ctx, fmt.Sprintf("%s-x%d", v.ID, f.ID), s.dataPath(f.Path), fi.ModTime().Unix())
	if err != nil {
		if ctx.Err() == nil {
			_ = s.Store.SetSubtitleTiming(ctx, f.ID, 0, 0, "unchecked", key)
		}
		return false
	}
	start := time.Now()
	speech, speechErr := s.speechFor(ctx, v, f.Language)
	if ctx.Err() != nil {
		return false
	}
	runtime := time.Duration(v.DurationSec * float64(time.Second))
	state, timing, verdict := s.checkTiming(vtt, speech, speechErr, runtime, f.Forced)
	if verdict == "no_match" {
		// Kept all the same: it's the owner's file, or someone's pick.
		state = "no_match"
	}
	saveKey := key
	if speechErr != nil && !errors.Is(speechErr, subsync.ErrNoSpeech) {
		saveKey = timingRetry
	}
	if err := s.Store.SetSubtitleTiming(ctx, f.ID, timing.Scale, timing.OffsetMS, state, saveKey); err != nil {
		s.Log.Warn("subtitle timing: couldn't save", "file", v.RelPath, "err", err)
		return true
	}
	s.Log.Info("subtitle timing: checked", "file", v.RelPath, "subtitle", f.Path, "source", f.Source,
		"sync", state, "offset_ms", timing.OffsetMS, "scale", timing.Scale, "took", time.Since(start).Round(time.Second))
	return true
}

// speechLock is held while a version's speech is read.
func (s *Server) speechLock(versionID string) *sync.Mutex {
	l, _ := s.timing.speech.LoadOrStore(versionID, &sync.Mutex{})
	return l.(*sync.Mutex)
}
