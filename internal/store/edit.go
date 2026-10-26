package store

import (
	"context"
	"database/sql"
	"errors"
	"slices"
	"strings"

	"kinara/internal/parse"
)

// Editing a title by hand. Every field the owner changes is locked
// (items.locked): matching and refreshing from TMDB leave it as edited,
// until it's unlocked again (see qUpdateItemMeta, qSetImage, SetGenres).

// Lockable fields.
const (
	LockTitle     = "title"
	LockSortTitle = "sort_title"
	LockYear      = "year"
	LockOverview  = "overview"
	LockRating    = "rating"
	LockAirDate   = "air_date"
	LockGenres    = "genres"
	LockPoster    = "poster"
	LockBackdrop  = "backdrop"
	LockStill     = "still"
)

// Lockable lists the fields that can be locked.
var Lockable = []string{LockTitle, LockSortTitle, LockYear, LockOverview, LockRating, LockAirDate, LockGenres, LockPoster, LockBackdrop, LockStill}

// ItemEdit is a hand edit: nil fields stay as they are.
type ItemEdit struct {
	Title     *string   `json:"title"`
	SortTitle *string   `json:"sortTitle"`
	Year      *int      `json:"year"`
	Overview  *string   `json:"overview"`
	Rating    *float64  `json:"rating"`
	AirDate   *string   `json:"airDate"`
	Genres    *[]string `json:"genres"`
}

// ItemLocks returns an item's locked fields.
func (s *Store) ItemLocks(ctx context.Context, id string) ([]string, error) {
	var locked string
	err := s.db.QueryRowContext(ctx, `SELECT locked FROM items WHERE id = ?`, id).Scan(&locked)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	return splitLocks(locked), nil
}

func splitLocks(s string) []string {
	out := []string{}
	for _, f := range strings.Split(s, ",") {
		if f != "" {
			out = append(out, f)
		}
	}
	return out
}

func joinLocks(fields []string) string {
	if len(fields) == 0 {
		return ""
	}
	return "," + strings.Join(fields, ",") + ","
}

// addLocks locks fields on an item in a transaction.
func addLocks(ctx context.Context, tx *sql.Tx, id string, fields ...string) error {
	var locked string
	if err := tx.QueryRowContext(ctx, `SELECT locked FROM items WHERE id = ?`, id).Scan(&locked); err != nil {
		return err
	}
	have := splitLocks(locked)
	for _, f := range fields {
		if !slices.Contains(have, f) {
			have = append(have, f)
		}
	}
	_, err := tx.ExecContext(ctx, `UPDATE items SET locked = ?, updated_at = ? WHERE id = ?`, joinLocks(have), now(), id)
	return err
}

// EditItem applies a hand edit and locks what it changed.
func (s *Store) EditItem(ctx context.Context, id string, e ItemEdit) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var locks []string
	set := func(col, lock string, v any) error {
		_, err := tx.ExecContext(ctx, `UPDATE items SET `+col+` = ? WHERE id = ?`, v, id)
		locks = append(locks, lock)
		return err
	}
	if e.Title != nil {
		if err := set("title", LockTitle, strings.TrimSpace(*e.Title)); err != nil {
			return err
		}
		// The sort title follows the title unless it's given too.
		if e.SortTitle == nil {
			if err := set("sort_title", LockSortTitle, parse.SortTitle(strings.TrimSpace(*e.Title))); err != nil {
				return err
			}
		}
	}
	if e.SortTitle != nil {
		if err := set("sort_title", LockSortTitle, strings.TrimSpace(*e.SortTitle)); err != nil {
			return err
		}
	}
	if e.Year != nil {
		if err := set("year", LockYear, nullPositive(*e.Year)); err != nil {
			return err
		}
	}
	if e.Overview != nil {
		if err := set("overview", LockOverview, strings.TrimSpace(*e.Overview)); err != nil {
			return err
		}
	}
	if e.Rating != nil {
		if err := set("rating", LockRating, nullFloat(*e.Rating)); err != nil {
			return err
		}
	}
	if e.AirDate != nil {
		if err := set("air_date", LockAirDate, nullStr(strings.TrimSpace(*e.AirDate))); err != nil {
			return err
		}
	}
	if e.Genres != nil {
		if _, err := tx.ExecContext(ctx, `DELETE FROM item_genres WHERE item_id = ?`, id); err != nil {
			return err
		}
		for _, g := range *e.Genres {
			if g = strings.TrimSpace(g); g != "" {
				if _, err := tx.ExecContext(ctx, `INSERT OR IGNORE INTO item_genres (item_id, genre) VALUES (?, ?)`, id, g); err != nil {
					return err
				}
			}
		}
		locks = append(locks, LockGenres)
	}
	if len(locks) == 0 {
		return nil
	}
	if err := addLocks(ctx, tx, id, locks...); err != nil {
		return err
	}
	return tx.Commit()
}

// SetImageByHand sets an item's artwork and locks it.
func (s *Store) SetImageByHand(ctx context.Context, itemID, kind, url string) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx, `INSERT INTO images (item_id, kind, url) VALUES (?, ?, ?)
		ON CONFLICT (item_id, kind) DO UPDATE SET url = excluded.url`, itemID, kind, url); err != nil {
		return err
	}
	if err := addLocks(ctx, tx, itemID, kind); err != nil {
		return err
	}
	return tx.Commit()
}

// Unlock lets matching set fields again (all of them for "all"). It
// doesn't change the values: the next refresh does.
func (s *Store) Unlock(ctx context.Context, id string, fields []string) error {
	have, err := s.ItemLocks(ctx, id)
	if err != nil {
		return err
	}
	var keep []string
	if !slices.Contains(fields, "all") {
		for _, f := range have {
			if !slices.Contains(fields, f) {
				keep = append(keep, f)
			}
		}
	}
	if slices.Contains(fields, "all") || slices.Contains(fields, LockGenres) {
		// Fetched again at the next match.
		if _, err := s.db.ExecContext(ctx, `UPDATE items SET genres_at = 0 WHERE id = ?`, id); err != nil {
			return err
		}
	}
	_, err = s.db.ExecContext(ctx, `UPDATE items SET locked = ?, updated_at = ? WHERE id = ?`, joinLocks(keep), now(), id)
	return err
}

// ItemGenres lists an item's genres.
func (s *Store) ItemGenres(ctx context.Context, id string) ([]string, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT genre FROM item_genres WHERE item_id = ? ORDER BY genre`, id)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []string{}
	for rows.Next() {
		var g string
		if err := rows.Scan(&g); err != nil {
			return nil, err
		}
		out = append(out, g)
	}
	return out, rows.Err()
}
