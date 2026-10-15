package metrics

import (
	"sort"
	"strings"
	"sync"
	"time"
)

// Latency buckets, milliseconds (the last is everything slower).
var bucketsMs = []float64{2, 5, 10, 25, 50, 100, 250, 500, 1000, 2500, 5000, 10000}

type route struct {
	count, clientErr, serverErr uint64
	bytes                       uint64
	totalMs                     float64
	hist                        []uint64
	last                        time.Time
}

// HTTPStats counts requests by route since the server started.
type HTTPStats struct {
	mu      sync.Mutex
	routes  map[string]*route
	started time.Time
}

// Route is one route's numbers.
type Route struct {
	Route     string  `json:"route"`
	Count     uint64  `json:"count"`
	ClientErr uint64  `json:"clientErrors"`
	ServerErr uint64  `json:"serverErrors"`
	Bytes     uint64  `json:"bytes"`
	AvgMs     float64 `json:"avgMs"`
	P50Ms     float64 `json:"p50Ms"`
	P95Ms     float64 `json:"p95Ms"`
	// Streaming: the request lasts as long as playback, so its time says
	// nothing about speed.
	Streaming bool  `json:"streaming"`
	LastAt    int64 `json:"lastAt"`
}

// Streaming reports routes that stay open while something plays.
func Streaming(pattern string) bool {
	for _, s := range []string{"/stream", "/transcode/{id}", "/hls/session/", "/speedtest", "/download"} {
		if strings.Contains(pattern, s) && !strings.HasSuffix(pattern, "/start") {
			return true
		}
	}
	return false
}

// Record counts a finished request.
func (h *HTTPStats) Record(pattern string, status int, d time.Duration, bytes int64) {
	if pattern == "" {
		pattern = "(no route)"
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.routes == nil {
		h.routes = map[string]*route{}
		h.started = time.Now()
	}
	r := h.routes[pattern]
	if r == nil {
		r = &route{hist: make([]uint64, len(bucketsMs)+1)}
		h.routes[pattern] = r
	}
	ms := float64(d.Microseconds()) / 1000
	r.count++
	r.totalMs += ms
	r.bytes += uint64(max(bytes, 0))
	r.last = time.Now()
	switch {
	case status >= 500:
		r.serverErr++
	case status >= 400:
		r.clientErr++
	}
	i := sort.SearchFloat64s(bucketsMs, ms)
	r.hist[i]++
}

// quantile estimates a percentile from the buckets (the bucket's top edge).
func quantile(hist []uint64, total uint64, q float64) float64 {
	if total == 0 {
		return 0
	}
	want := uint64(float64(total)*q + 0.5)
	var seen uint64
	for i, n := range hist {
		seen += n
		if seen >= want {
			if i < len(bucketsMs) {
				return bucketsMs[i]
			}
			return bucketsMs[len(bucketsMs)-1] * 2
		}
	}
	return 0
}

// Routes lists every route seen, busiest first.
func (h *HTTPStats) Routes() []Route {
	h.mu.Lock()
	defer h.mu.Unlock()
	out := make([]Route, 0, len(h.routes))
	for p, r := range h.routes {
		x := Route{Route: p, Count: r.count, ClientErr: r.clientErr, ServerErr: r.serverErr, Bytes: r.bytes,
			Streaming: Streaming(p), LastAt: r.last.Unix()}
		if !x.Streaming && r.count > 0 {
			x.AvgMs = round1(r.totalMs / float64(r.count))
			x.P50Ms = quantile(r.hist, r.count, 0.5)
			x.P95Ms = quantile(r.hist, r.count, 0.95)
		}
		out = append(out, x)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Count > out[j].Count })
	return out
}

// Since is when counting began.
func (h *HTTPStats) Since() time.Time {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.started
}
