package store

import (
	"context"
	"database/sql"
	"errors"
)

func scanWatch(r scanner) (WatchState, error) {
	var w WatchState
	var completed int
	err := r.Scan(&w.ItemID, &w.PositionSec, &completed, &w.PlayCount, &w.LastPlayedAt, &w.ClientTime)
	w.Completed = completed != 0
	return w, err
}

// GetWatch returns a profile's state for an item; a zero state if none.
func (s *Store) GetWatch(ctx context.Context, profileID int64, itemID string) (WatchState, error) {
	w, err := scanWatch(s.db.QueryRowContext(ctx, qGetWatch, profileID, itemID))
	if errors.Is(err, sql.ErrNoRows) {
		return WatchState{ItemID: itemID}, nil
	}
	return w, err
}

// PutWatch writes a profile's state for an item.
func (s *Store) PutWatch(ctx context.Context, profileID int64, w WatchState) error {
	_, err := s.db.ExecContext(ctx, qUpsertWatch, profileID, w.ItemID, w.PositionSec,
		boolInt(w.Completed), w.PlayCount, w.LastPlayedAt, w.ClientTime)
	return err
}

// WatchStates returns states for many items at once, keyed by item ID.
func (s *Store) WatchStates(ctx context.Context, profileID int64, itemIDs []string) (map[string]WatchState, error) {
	out := make(map[string]WatchState, len(itemIDs))
	// SQLite limits bound parameters; query in chunks.
	const chunk = 500
	for start := 0; start < len(itemIDs); start += chunk {
		end := min(start+chunk, len(itemIDs))
		ids := itemIDs[start:end]
		args := make([]any, 0, len(ids)+1)
		args = append(args, profileID)
		for _, id := range ids {
			args = append(args, id)
		}
		q := `SELECT item_id, position_sec, completed, play_count, last_played_at, client_time
			FROM watch_state WHERE profile_id = ? AND item_id IN (` + placeholders(len(ids)) + `)`
		rows, err := s.db.QueryContext(ctx, q, args...)
		if err != nil {
			return nil, err
		}
		for rows.Next() {
			w, err := scanWatch(rows)
			if err != nil {
				rows.Close()
				return nil, err
			}
			out[w.ItemID] = w
		}
		if err := rows.Close(); err != nil {
			return nil, err
		}
	}
	return out, nil
}

// RecentWatch lists a profile's states played since a Unix time, newest first.
func (s *Store) RecentWatch(ctx context.Context, profileID int64, since int64, limit int) ([]WatchState, error) {
	rows, err := s.db.QueryContext(ctx, qRecentWatch, profileID, since, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []WatchState
	for rows.Next() {
		w, err := scanWatch(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, w)
	}
	return out, rows.Err()
}
