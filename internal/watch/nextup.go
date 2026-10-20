// Package watch works out progress, watched state, and the next episode.
package watch

import (
	"fmt"
	"time"

	"kinara/internal/store"
)

const (
	// MinProgressSec: positions under this are not saved as "in progress",
	// so sampling a title doesn't clutter Continue Watching.
	MinProgressSec = 120
	// MovieStartedSec: a movie joins Continue Watching once played this
	// far. Lower than for episodes, which always have a next one to show.
	MovieStartedSec = 30
	// WatchedFraction: an item counts as watched past this share of its
	// length. Credits markers will replace this later.
	WatchedFraction = 0.92
)

// IsWatched reports whether a position counts as finishing the item.
func IsWatched(positionSec, durationSec float64) bool {
	return durationSec > 0 && positionSec >= durationSec*WatchedFraction
}

// NextUp is the answer to "which episode is next?" for one show.
type NextUp struct {
	// Next is nil when there is nothing left to watch.
	Next *store.EpisodeRef
	// InProgress is true when Next was started but not finished.
	InProgress bool
	// Last is the furthest episode watched or started, if any.
	Last *store.EpisodeRef
	// Gaps are unwatched episodes before Last, for the "mark as watched?" prompt.
	Gaps []store.EpisodeRef
	// Reason explains the choice in plain words.
	Reason string
}

// Compute works out the next episode from episodes in order (season, then
// episode) and the user's watch states. Specials (season 0) are never
// chosen and never count as gaps.
//
// Rule: find the furthest episode watched or started. If it is unfinished,
// that is next; otherwise the one after it. Earlier unwatched episodes are
// gaps, never "next", so skipping or rewatching doesn't send people back.
func Compute(episodes []store.EpisodeRef, states map[string]store.WatchState, now time.Time) NextUp {
	var regular []store.EpisodeRef
	for _, e := range episodes {
		if e.Season > 0 {
			regular = append(regular, e)
		}
	}
	if len(regular) == 0 {
		return NextUp{}
	}

	furthest := -1
	for i, e := range regular {
		if w, ok := states[e.ID]; ok && (w.Completed || w.PositionSec >= MinProgressSec) {
			furthest = i
		}
	}
	if furthest < 0 {
		first := regular[0]
		return NextUp{Next: &first, Reason: "Start from the beginning"}
	}

	last := regular[furthest]
	var res NextUp
	res.Last = &last
	for _, e := range regular[:furthest] {
		if w, ok := states[e.ID]; !ok || !w.Completed {
			res.Gaps = append(res.Gaps, e)
		}
	}

	w := states[last.ID]
	switch {
	case !w.Completed:
		res.Next = &last
		res.InProgress = true
		res.Reason = fmt.Sprintf("You're partway through %s", Code(last))
	case furthest+1 < len(regular):
		next := regular[furthest+1]
		res.Next = &next
		res.Reason = fmt.Sprintf("You finished %s %s", Code(last), When(w.LastPlayedAt, now))
	default:
		res.Reason = "You're all caught up"
	}
	return res
}

// Code formats an episode as S02E05.
func Code(e store.EpisodeRef) string {
	return fmt.Sprintf("S%02dE%02d", e.Season, e.Episode)
}

// When describes a Unix time relative to now: "today", "yesterday",
// "3 days ago", "on Mar 4".
func When(unix int64, now time.Time) string {
	if unix <= 0 {
		return "earlier"
	}
	t := time.Unix(unix, 0).In(now.Location())
	y1, m1, d1 := t.Date()
	y2, m2, d2 := now.Date()
	days := int(time.Date(y2, m2, d2, 0, 0, 0, 0, now.Location()).
		Sub(time.Date(y1, m1, d1, 0, 0, 0, 0, now.Location())).Hours() / 24)
	switch {
	case days <= 0:
		return "today"
	case days == 1:
		return "yesterday"
	case days < 7:
		return fmt.Sprintf("%d days ago", days)
	case t.Year() == now.Year():
		return "on " + t.Format("Jan 2")
	default:
		return "on " + t.Format("Jan 2, 2006")
	}
}
