package transcode

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"io"
	"regexp"
	"sort"
	"strconv"
	"sync"
	"time"
)

// Owner says who a stream is for, so the manager can see what's converting
// and the server can decide whose quality gives way when it can't keep up.
type Owner struct {
	// Device names the signed-in device (a short form of its session).
	Device     string
	DeviceName string
	User       string
	ItemID     string
	VersionID  string
	Title      string
	// Priority: this device keeps its quality; others step down first, and
	// it gets a slot even when every slot is taken.
	Priority bool
}

// Live is a conversion running now.
type Live struct {
	ID        string
	Kind      string // "pipe" or "hls"
	Owner     Owner
	Video     string
	Bitrate   int
	Height    int
	Start     float64
	StartedAt time.Time

	mu      sync.Mutex
	samples []sample
	// blocked is the total time spent waiting to hand output to the
	// player (a pipe only): a full player buffer, or a slow connection.
	blocked time.Duration
	advised bool
}

type sample struct {
	at      time.Time
	media   float64       // seconds of video made so far
	blocked time.Duration // total blocked by then
}

// LiveInfo is a snapshot of a running conversion.
type LiveInfo struct {
	ID         string    `json:"id"`
	Kind       string    `json:"kind"`
	Device     string    `json:"device"`
	DeviceName string    `json:"deviceName"`
	User       string    `json:"user"`
	ItemID     string    `json:"itemId"`
	VersionID  string    `json:"versionId"`
	Title      string    `json:"title"`
	Video      string    `json:"video"`
	MaxBitrate int       `json:"maxBitrate"`
	MaxHeight  int       `json:"maxHeight"`
	StartedAt  time.Time `json:"startedAt"`
	// Rate is how fast the conversion runs against real time over the last
	// few seconds (1.0 = just keeping up), 0 until known.
	Rate float64 `json:"rate"`
	// PlayerLimited: the stream is waiting on the player (paused, buffer
	// full, slow connection), so a low rate says nothing about the server.
	PlayerLimited bool `json:"playerLimited"`
	// Behind: the server isn't converting fast enough to keep playing.
	Behind   bool `json:"behind"`
	Priority bool `json:"priority"`
}

const (
	// warmup is how long a stream runs before its rate is judged.
	warmup = 20 * time.Second
	// window is how far back the rate looks.
	window = 15 * time.Second
	// keepUp is the rate below which a stream is falling behind.
	keepUp = 0.95
	// playerBound is the share of time waiting on the player above which a
	// slow rate is the player's doing, not the server's.
	playerBound = 0.3
)

var reStatTime = regexp.MustCompile(`time=\s*(-?)(\d+):(\d{2}):(\d{2}(?:\.\d+)?)`)

// parseStatTime reads "time=00:01:23.45" from an ffmpeg progress line.
func parseStatTime(line string) (float64, bool) {
	m := reStatTime.FindStringSubmatch(line)
	if m == nil || m[1] == "-" {
		return 0, false
	}
	h, _ := strconv.Atoi(m[2])
	mi, _ := strconv.Atoi(m[3])
	s, _ := strconv.ParseFloat(m[4], 64)
	return float64(h*3600+mi*60) + s, true
}

func (l *Live) stat(line string) {
	t, ok := parseStatTime(line)
	if !ok {
		return
	}
	l.addSample(time.Now(), t)
}

func (l *Live) addSample(at time.Time, media float64) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.samples = append(l.samples, sample{at: at, media: media, blocked: l.blocked})
	// Keep a little more than the window.
	cut := 0
	for cut < len(l.samples)-2 && at.Sub(l.samples[cut].at) > 2*window {
		cut++
	}
	l.samples = l.samples[cut:]
}

func (l *Live) addBlocked(d time.Duration) {
	l.mu.Lock()
	l.blocked += d
	l.mu.Unlock()
}

// info judges the stream as of now.
func (l *Live) info(now time.Time) LiveInfo {
	l.mu.Lock()
	defer l.mu.Unlock()
	in := LiveInfo{
		ID: l.ID, Kind: l.Kind, Device: l.Owner.Device, DeviceName: l.Owner.DeviceName, User: l.Owner.User,
		ItemID: l.Owner.ItemID, VersionID: l.Owner.VersionID, Title: l.Owner.Title, Video: l.Video,
		MaxBitrate: l.Bitrate, MaxHeight: l.Height, StartedAt: l.StartedAt, Priority: l.Owner.Priority,
	}
	n := len(l.samples)
	if n < 2 {
		return in
	}
	last := l.samples[n-1]
	first := last
	for i := n - 2; i >= 0; i-- {
		first = l.samples[i]
		if last.at.Sub(first.at) >= window {
			break
		}
	}
	span := last.at.Sub(first.at)
	if span < window/2 {
		return in
	}
	in.Rate = (last.media - first.media) / span.Seconds()
	in.PlayerLimited = (last.blocked-first.blocked).Seconds()/span.Seconds() > playerBound
	// A stale reading (ffmpeg stopped reporting) isn't judged either.
	fresh := now.Sub(last.at) < 2*window
	in.Behind = fresh && now.Sub(l.StartedAt) >= warmup && in.Rate < keepUp && !in.PlayerLimited
	return in
}

// registry is the transcoder's list of running conversions.
type registry struct {
	mu   sync.Mutex
	live map[string]*Live
}

func (t *Transcoder) register(r Request, kind string) *Live {
	id := make([]byte, 8)
	_, _ = rand.Read(id)
	l := &Live{
		ID: hex.EncodeToString(id), Kind: kind, Owner: r.Owner, Video: r.Video,
		Start: r.Start, StartedAt: time.Now(),
	}
	if r.Video == "h264" {
		l.Bitrate, l.Height = r.MaxBitrate, r.MaxHeight
	}
	t.reg.mu.Lock()
	if t.reg.live == nil {
		t.reg.live = map[string]*Live{}
	}
	t.reg.live[l.ID] = l
	t.reg.mu.Unlock()
	return l
}

func (t *Transcoder) unregister(l *Live) {
	t.reg.mu.Lock()
	delete(t.reg.live, l.ID)
	t.reg.mu.Unlock()
}

func (t *Transcoder) lives() []*Live {
	t.reg.mu.Lock()
	defer t.reg.mu.Unlock()
	out := make([]*Live, 0, len(t.reg.live))
	for _, l := range t.reg.live {
		out = append(out, l)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].StartedAt.Before(out[j].StartedAt) })
	return out
}

// Live lists the conversions running now, oldest first.
func (t *Transcoder) Live() []LiveInfo {
	now := time.Now()
	ls := t.lives()
	out := make([]LiveInfo, 0, len(ls))
	for _, l := range ls {
		out = append(out, l.info(now))
	}
	return out
}

// acquire takes a conversion slot. A priority device never waits: when
// every slot is taken it runs anyway (and others are asked to step down).
func (t *Transcoder) acquire(ctx context.Context, priority bool) (release func(), err error) {
	sem := t.slots()
	select {
	case sem <- struct{}{}:
		return func() { <-sem }, nil
	default:
	}
	if priority {
		t.Log.Info("transcode slots full; priority device goes ahead")
		return func() {}, nil
	}
	select {
	case sem <- struct{}{}:
		return func() { <-sem }, nil
	case <-time.After(5 * time.Second):
		return nil, ErrBusy
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

// timedWriter counts the time spent handing output on, which is time the
// stream waits for the player.
type timedWriter struct {
	w    io.Writer
	live *Live
}

func (tw *timedWriter) Write(p []byte) (int, error) {
	start := time.Now()
	n, err := tw.w.Write(p)
	if d := time.Since(start); d > time.Millisecond {
		tw.live.addBlocked(d)
	}
	return n, err
}
