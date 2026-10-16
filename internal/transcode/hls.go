package transcode

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sync"
	"time"

	"kinora/internal/ffmpeg"
)

// HLS runs conversions that write HLS (a playlist and fragmented-MP4
// segments) to disk, for players that can't take a Matroska or MP4 pipe:
// iPhones, iPads and Safari. Each session is a folder named by a random ID;
// knowing the ID is what lets a player fetch it, so segment requests need
// no token (players don't carry one from the playlist to its segments).
type HLS struct {
	T   *Transcoder
	Dir string
	// Idle is how long a session lives without requests (default 90 s).
	Idle time.Duration
	// Ready is how long Start waits for the first segment (default 30 s).
	Ready time.Duration

	mu       sync.Mutex
	sessions map[string]*hlsSession
	reaper   sync.Once
}

type hlsSession struct {
	id     string
	dir    string
	cancel context.CancelFunc
	done   chan struct{}
	err    error
	last   time.Time
	live   *Live
}

// ErrNoSession is returned for a session that ended or never existed.
var ErrNoSession = errors.New("that stream has ended")

var (
	reSession = regexp.MustCompile(`^[0-9a-f]{32}$`)
	reHLSFile = regexp.MustCompile(`^(index\.m3u8|init\.mp4|seg\d{5}\.(m4s|ts))$`)
)

// Start begins converting r to HLS and returns the session ID once the
// first segment exists, so the player can start at once. A conversion that
// fails before its first segment is tried again at the next stage (see
// pipeline.go).
func (h *HLS) Start(ctx context.Context, r Request) (string, error) {
	t := h.T
	caps := t.Tool.Caps()
	if !caps.Found {
		return "", errors.New("ffmpeg was not found; install ffmpeg to convert files this device can't play")
	}
	if r.Video == "h264" && caps.H264Encoder == "" {
		return "", errors.New("this ffmpeg has no H.264 encoder")
	}
	r.Container, r.OutDir = "hls", h.Dir
	if err := r.Validate(); err != nil {
		return "", err
	}
	if err := t.prepare(ctx, &r, caps); err != nil {
		return "", err
	}
	stages := t.stagesFor(r, caps)
	var err error
	for i, stage := range stages {
		r.Stage = stage
		var id string
		var exited bool
		id, exited, err = h.startOnce(ctx, r, caps)
		if err == nil {
			if stage != "" {
				t.rememberStage(r, stage, i > 0)
			}
			return id, nil
		}
		// Only a conversion that ended by itself moves on: a busy server,
		// a cancelled request or one that is merely slow isn't fixed by
		// another way of converting.
		if !exited || ctx.Err() != nil || i == len(stages)-1 {
			break
		}
		t.Log.Warn("hls failed before its first segment; trying the next way", "file", filepath.Base(r.Path),
			"stage", stage, "next", stages[i+1], "err", err)
	}
	return "", err
}

// startOnce runs one conversion attempt. exited reports that ffmpeg ended
// before making a segment (rather than timing out or being cancelled).
func (h *HLS) startOnce(ctx context.Context, r Request, caps ffmpeg.Caps) (id string, exited bool, err error) {
	t := h.T
	id, err = newSessionID()
	if err != nil {
		return "", false, err
	}
	dir := filepath.Join(h.Dir, id)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", false, err
	}
	r.OutDir = dir
	release, err := t.acquire(ctx, r.Owner.Priority)
	if err != nil {
		os.RemoveAll(dir)
		return "", false, err
	}
	live := t.register(r, "hls")

	// The conversion outlives the request that started it.
	sctx, cancel := context.WithCancel(context.Background())
	cmd := exec.CommandContext(sctx, t.Tool.Path(), Args(r, caps)...)
	ffmpeg.HideWindow(cmd)
	tail := &ffmpeg.Tail{N: 2048}
	cmd.Stderr = &statsSplitter{rest: tail, onStat: live.stat}
	if err := ffmpeg.StartLow(cmd); err != nil {
		cancel()
		release()
		t.unregister(live)
		os.RemoveAll(dir)
		return "", false, err
	}
	s := &hlsSession{id: id, dir: dir, cancel: cancel, done: make(chan struct{}), last: time.Now(), live: live}
	enc := ""
	if r.Video == "h264" {
		enc = planVideo(r, caps).encoder
	}
	t.Log.Info("hls start", "file", r.Path, "from", r.Start, "video", r.Video, "stage", r.Stage, "encoder", enc,
		"audio", r.AudioCodec, "dolby_vision", dvSummary(r), "session", id)
	go func() {
		err := cmd.Wait()
		if sctx.Err() == nil && err != nil {
			s.err = fmt.Errorf("ffmpeg: %v %s", err, tail.String())
			t.Log.Warn("hls failed", "file", r.Path, "err", s.err)
		}
		release()
		t.unregister(live)
		close(s.done)
	}()

	h.mu.Lock()
	if h.sessions == nil {
		h.sessions = map[string]*hlsSession{}
	}
	h.sessions[id] = s
	h.mu.Unlock()
	h.reaper.Do(func() { go h.reap() })

	wait := h.Ready
	if wait <= 0 {
		wait = 30 * time.Second
	}
	deadline := time.After(wait)
	tick := time.NewTicker(150 * time.Millisecond)
	defer tick.Stop()
	for {
		if b, err := os.ReadFile(filepath.Join(dir, "index.m3u8")); err == nil && bytes.Contains(b, []byte("#EXTINF")) {
			return id, false, nil
		}
		// Starting counts as use: the idle reaper must not end a session
		// that is still making its first segment.
		h.mu.Lock()
		s.last = time.Now()
		h.mu.Unlock()
		select {
		case <-s.done:
			// It ended before a segment was listed: failed, or a very short file.
			if b, err := os.ReadFile(filepath.Join(dir, "index.m3u8")); err == nil && bytes.Contains(b, []byte("#EXTINF")) {
				return id, false, nil
			}
			h.Stop(id)
			if s.err != nil {
				return "", true, s.err
			}
			return "", true, errors.New("ffmpeg made no video")
		case <-deadline:
			h.Stop(id)
			return "", false, errors.New("the conversion didn't start in time")
		case <-ctx.Done():
			h.Stop(id)
			return "", false, ctx.Err()
		case <-tick.C:
		}
	}
}

// File returns the path of a session's playlist or segment, and keeps the
// session alive. The playlist gets a start tag, so players begin at its
// start rather than its newest segment.
func (h *HLS) File(id, name string) (path string, playlist []byte, err error) {
	if !reSession.MatchString(id) || !reHLSFile.MatchString(name) {
		return "", nil, ErrNoSession
	}
	h.mu.Lock()
	s := h.sessions[id]
	if s != nil {
		s.last = time.Now()
	}
	h.mu.Unlock()
	if s == nil {
		return "", nil, ErrNoSession
	}
	path = filepath.Join(s.dir, name)
	if name != "index.m3u8" {
		return path, nil, nil
	}
	b, err := os.ReadFile(path)
	if err != nil {
		return "", nil, err
	}
	return path, StartAtBeginning(b), nil
}

// StartAtBeginning adds EXT-X-START so a player starts an event playlist at
// its first segment (by default some start near the newest one).
func StartAtBeginning(playlist []byte) []byte {
	if bytes.Contains(playlist, []byte("#EXT-X-START")) {
		return playlist
	}
	head := []byte("#EXTM3U\n")
	if !bytes.HasPrefix(playlist, head) {
		return playlist
	}
	out := append([]byte{}, head...)
	out = append(out, "#EXT-X-START:TIME-OFFSET=0,PRECISE=YES\n"...)
	return append(out, playlist[len(head):]...)
}

// Stop ends a session and deletes its files.
func (h *HLS) Stop(id string) {
	h.mu.Lock()
	s := h.sessions[id]
	delete(h.sessions, id)
	h.mu.Unlock()
	if s == nil {
		return
	}
	s.cancel()
	<-s.done
	os.RemoveAll(s.dir)
}

// Close ends every session (shutdown, tests).
func (h *HLS) Close() {
	h.mu.Lock()
	ids := make([]string, 0, len(h.sessions))
	for id := range h.sessions {
		ids = append(ids, id)
	}
	h.mu.Unlock()
	for _, id := range ids {
		h.Stop(id)
	}
}

// reap ends sessions nobody has asked for lately.
func (h *HLS) reap() {
	idle := h.Idle
	if idle <= 0 {
		idle = 90 * time.Second
	}
	tick := time.NewTicker(min(idle/3, 15*time.Second))
	defer tick.Stop()
	for range tick.C {
		var stale []string
		h.mu.Lock()
		for id, s := range h.sessions {
			if time.Since(s.last) > idle {
				stale = append(stale, id)
			}
		}
		h.mu.Unlock()
		for _, id := range stale {
			h.T.Log.Info("hls ended (idle)", "session", id)
			h.Stop(id)
		}
	}
}

func newSessionID() (string, error) {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}
