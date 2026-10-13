package watch

import (
	"context"
	"time"

	"kinora/internal/store"
)

// Service records progress and builds Continue Watching.
type Service struct {
	Store *store.Store
	// Now is replaceable in tests.
	Now func() time.Time
}

func (s *Service) now() time.Time {
	if s.Now != nil {
		return s.Now()
	}
	return time.Now()
}

// Report records a playback position. durationSec is the length of the file
// being played. clientTime is when the client took the reading (Unix
// seconds); older readings than the stored one are ignored, so a report
// queued offline never overwrites newer progress.
func (s *Service) Report(ctx context.Context, profileID int64, itemID string, positionSec, durationSec float64, clientTime int64) (store.WatchState, error) {
	w, err := s.Store.GetWatch(ctx, profileID, itemID)
	if err != nil {
		return w, err
	}
	nowT := s.now().Unix()
	if clientTime <= 0 {
		clientTime = nowT
	}
	if clientTime < w.ClientTime {
		return w, nil
	}
	finished := IsWatched(positionSec, durationSec)
	if finished && !w.Completed {
		w.PlayCount++
	}
	// A rewatch keeps the item marked watched while showing progress.
	w.Completed = w.Completed || finished
	if finished {
		w.PositionSec = 0
	} else {
		w.PositionSec = positionSec
	}
	w.ItemID = itemID
	w.LastPlayedAt = nowT
	w.ClientTime = clientTime
	return w, s.Store.PutWatch(ctx, profileID, w)
}

// SetWatched marks an item (and for shows and seasons, every episode in it)
// watched or unwatched.
func (s *Service) SetWatched(ctx context.Context, profileID int64, item store.Item, watched bool) error {
	switch item.Kind {
	case store.ItemShow, store.ItemSeason:
		children, err := s.Store.Children(ctx, item.ID)
		if err != nil {
			return err
		}
		for _, c := range children {
			if err := s.SetWatched(ctx, profileID, c, watched); err != nil {
				return err
			}
		}
		return nil
	}
	w, err := s.Store.GetWatch(ctx, profileID, item.ID)
	if err != nil {
		return err
	}
	if watched && !w.Completed {
		w.PlayCount++
	}
	w.ItemID = item.ID
	w.Completed = watched
	w.PositionSec = 0
	w.LastPlayedAt = s.now().Unix()
	w.ClientTime = w.LastPlayedAt
	return s.Store.PutWatch(ctx, profileID, w)
}

// ShowNextUp works out the next episode of a show for a user.
func (s *Service) ShowNextUp(ctx context.Context, profileID int64, showID string) (NextUp, error) {
	eps, err := s.Store.ShowEpisodes(ctx, showID)
	if err != nil {
		return NextUp{}, err
	}
	ids := make([]string, len(eps))
	for i, e := range eps {
		ids[i] = e.ID
	}
	states, err := s.Store.WatchStates(ctx, profileID, ids)
	if err != nil {
		return NextUp{}, err
	}
	return Compute(eps, states, s.now()), nil
}

// ContinueEntry is one row of Continue Watching: a movie in progress, or the
// next episode of a show watched recently.
type ContinueEntry struct {
	Item        store.Item
	ShowID      string
	PositionSec float64
	InProgress  bool
	Reason      string
	LastPlayed  int64
}

// ContinueWindow is how far back Continue Watching looks.
const ContinueWindow = 60 * 24 * time.Hour

// Continue builds Continue Watching, most recently played first.
func (s *Service) Continue(ctx context.Context, profileID int64, limit int) ([]ContinueEntry, error) {
	since := s.now().Add(-ContinueWindow).Unix()
	recent, err := s.Store.RecentWatch(ctx, profileID, since, 300)
	if err != nil {
		return nil, err
	}
	var out []ContinueEntry
	seenShows := map[string]bool{}
	for _, w := range recent {
		if len(out) >= limit {
			break
		}
		it, err := s.Store.GetItem(ctx, w.ItemID)
		if err != nil {
			continue // item removed
		}
		switch it.Kind {
		case store.ItemMovie:
			if w.PositionSec >= MinProgressSec && !w.Completed {
				out = append(out, ContinueEntry{Item: it, PositionSec: w.PositionSec,
					InProgress: true, Reason: "Pick up where you left off", LastPlayed: w.LastPlayedAt})
			}
		case store.ItemEpisode:
			showID, err := s.Store.ShowOfEpisode(ctx, it.ID)
			if err != nil || showID == "" || seenShows[showID] {
				continue
			}
			seenShows[showID] = true
			nu, err := s.ShowNextUp(ctx, profileID, showID)
			if err != nil || nu.Next == nil {
				continue
			}
			next, err := s.Store.GetItem(ctx, nu.Next.ID)
			if err != nil {
				continue
			}
			pos := 0.0
			if nu.InProgress {
				ws, _ := s.Store.GetWatch(ctx, profileID, next.ID)
				pos = ws.PositionSec
			}
			out = append(out, ContinueEntry{Item: next, ShowID: showID, PositionSec: pos,
				InProgress: nu.InProgress, Reason: nu.Reason, LastPlayed: w.LastPlayedAt})
		}
	}
	return out, nil
}
