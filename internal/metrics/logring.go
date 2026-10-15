package metrics

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"
)

// LogEntry is a warning or error the server logged.
type LogEntry struct {
	Time    int64  `json:"t"`
	Level   string `json:"level"`
	Message string `json:"message"`
	Attrs   string `json:"attrs"`
}

// LogRing keeps the latest warnings and errors for the page, so a problem
// can be seen without opening the server's console.
type LogRing struct {
	mu      sync.Mutex
	entries []LogEntry
	Keep    int
}

// Entries are the kept warnings and errors, newest first.
func (r *LogRing) Entries() []LogEntry {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]LogEntry, len(r.entries))
	for i, e := range r.entries {
		out[len(r.entries)-1-i] = e
	}
	return out
}

func (r *LogRing) add(e LogEntry) {
	r.mu.Lock()
	defer r.mu.Unlock()
	keep := r.Keep
	if keep <= 0 {
		keep = 300
	}
	r.entries = append(r.entries, e)
	if len(r.entries) > keep {
		r.entries = append(r.entries[:0], r.entries[len(r.entries)-keep:]...)
	}
}

// Handler wraps a log handler, copying warnings and errors into the ring.
func (r *LogRing) Handler(next slog.Handler) slog.Handler {
	return &ringHandler{next: next, ring: r}
}

type ringHandler struct {
	next   slog.Handler
	ring   *LogRing
	prefix []string
	group  string
}

func (h *ringHandler) Enabled(ctx context.Context, l slog.Level) bool {
	return l >= slog.LevelWarn || h.next.Enabled(ctx, l)
}

func (h *ringHandler) Handle(ctx context.Context, rec slog.Record) error {
	if rec.Level >= slog.LevelWarn {
		attrs := append([]string{}, h.prefix...)
		rec.Attrs(func(a slog.Attr) bool {
			attrs = append(attrs, h.key(a.Key)+"="+fmt.Sprint(a.Value.Any()))
			return true
		})
		t := rec.Time
		if t.IsZero() {
			t = time.Now()
		}
		h.ring.add(LogEntry{Time: t.Unix(), Level: rec.Level.String(), Message: rec.Message, Attrs: strings.Join(attrs, " ")})
	}
	if h.next.Enabled(ctx, rec.Level) {
		return h.next.Handle(ctx, rec)
	}
	return nil
}

func (h *ringHandler) key(k string) string {
	if h.group != "" {
		return h.group + "." + k
	}
	return k
}

func (h *ringHandler) WithAttrs(as []slog.Attr) slog.Handler {
	p := append([]string{}, h.prefix...)
	for _, a := range as {
		p = append(p, h.key(a.Key)+"="+fmt.Sprint(a.Value.Any()))
	}
	return &ringHandler{next: h.next.WithAttrs(as), ring: h.ring, prefix: p, group: h.group}
}

func (h *ringHandler) WithGroup(name string) slog.Handler {
	g := name
	if h.group != "" {
		g = h.group + "." + name
	}
	return &ringHandler{next: h.next.WithGroup(name), ring: h.ring, prefix: h.prefix, group: g}
}
