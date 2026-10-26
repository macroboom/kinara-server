package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strconv"
	"strings"

	"kinara/internal/parse"
)

// Franchises (TMDB collections). A movie library can list a franchise's
// movies as one card: the listing then has an item of kind "collection"
// whose ID is CollectionPrefix plus TMDB's collection ID ("c119").

// ItemCollection is the kind of a franchise card in a listing.
const ItemCollection = "collection"

// CollectionPrefix starts a franchise's item ID.
const CollectionPrefix = "c"

// Collection is a franchise.
type Collection struct {
	ID           int    `json:"id"`
	Name         string `json:"name"`
	SortName     string `json:"-"`
	Overview     string `json:"overview"`
	PosterPath   string `json:"-"`
	BackdropPath string `json:"-"`
	DetailsAt    int64  `json:"-"`
}

// CollectionID reads a franchise card's item ID ("c119" -> 119, true).
func CollectionID(itemID string) (int, bool) {
	rest, ok := strings.CutPrefix(itemID, CollectionPrefix)
	if !ok {
		return 0, false
	}
	n, err := strconv.Atoi(rest)
	return n, err == nil && n > 0
}

// CollectionItemID is a franchise card's item ID.
func CollectionItemID(id int) string { return CollectionPrefix + strconv.Itoa(id) }

// SetCollection records a movie's franchise (nil: none) and when it was
// looked up.
func (s *Store) SetCollection(ctx context.Context, itemID string, c *Collection) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if c == nil || c.ID <= 0 {
		if _, err := tx.ExecContext(ctx, `DELETE FROM item_collections WHERE item_id = ?`, itemID); err != nil {
			return err
		}
	} else {
		if _, err := tx.ExecContext(ctx, `INSERT INTO collections (id, name, sort_name, poster_path, backdrop_path, updated_at)
			VALUES (?, ?, ?, ?, ?, ?)
			ON CONFLICT (id) DO UPDATE SET name = excluded.name, sort_name = excluded.sort_name,
				poster_path = CASE WHEN excluded.poster_path != '' THEN excluded.poster_path ELSE poster_path END,
				backdrop_path = CASE WHEN excluded.backdrop_path != '' THEN excluded.backdrop_path ELSE backdrop_path END,
				updated_at = excluded.updated_at`,
			c.ID, c.Name, parse.SortTitle(c.Name), c.PosterPath, c.BackdropPath, now()); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO item_collections (item_id, collection_id) VALUES (?, ?)
			ON CONFLICT (item_id) DO UPDATE SET collection_id = excluded.collection_id`, itemID, c.ID); err != nil {
			return err
		}
	}
	if _, err := tx.ExecContext(ctx, `UPDATE items SET collection_at = ? WHERE id = ?`, now(), itemID); err != nil {
		return err
	}
	return tx.Commit()
}

// MoviesNeedingCollection lists matched movies whose franchise was never
// looked up, with their TMDB IDs.
func (s *Store) MoviesNeedingCollection(ctx context.Context, libraryID int64, limit int) (map[string]int, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT i.id, e.value FROM items i
		JOIN external_ids e ON e.item_id = i.id AND e.provider = 'tmdb_movie'
		WHERE i.library_id = ? AND i.kind = 'movie' AND i.collection_at = 0 LIMIT ?`, libraryID, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]int{}
	for rows.Next() {
		var id, v string
		if err := rows.Scan(&id, &v); err != nil {
			return nil, err
		}
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			out[id] = n
		}
	}
	return out, rows.Err()
}

// GetCollection reads a franchise.
func (s *Store) GetCollection(ctx context.Context, id int) (Collection, error) {
	var c Collection
	err := s.db.QueryRowContext(ctx, `SELECT id, name, sort_name, overview, poster_path, backdrop_path, details_at
		FROM collections WHERE id = ?`, id).Scan(&c.ID, &c.Name, &c.SortName, &c.Overview, &c.PosterPath, &c.BackdropPath, &c.DetailsAt)
	if errors.Is(err, sql.ErrNoRows) {
		return c, ErrNotFound
	}
	return c, err
}

// SetCollectionDetails saves a franchise's description.
func (s *Store) SetCollectionDetails(ctx context.Context, id int, overview string) error {
	_, err := s.db.ExecContext(ctx, `UPDATE collections SET overview = ?, details_at = ? WHERE id = ?`, overview, now(), id)
	return err
}

// CollectionMovies lists a franchise's movies that have a file, oldest
// first.
func (s *Store) CollectionMovies(ctx context.Context, id int) ([]Item, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT `+itemColsI+` FROM items i
		JOIN item_collections ic ON ic.item_id = i.id
		WHERE ic.collection_id = ? AND i.kind = 'movie' AND `+availableSQL+`
		ORDER BY COALESCE(i.air_date, ''), COALESCE(i.year, 0), i.sort_title`, id)
	if err != nil {
		return nil, err
	}
	return scanItems(rows)
}

// CollectionOf is the franchise a movie belongs to (0 when none).
func (s *Store) CollectionOf(ctx context.Context, itemID string) (int, error) {
	var id int
	err := s.db.QueryRowContext(ctx, `SELECT collection_id FROM item_collections WHERE item_id = ?`, itemID).Scan(&id)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, nil
	}
	return id, err
}

// CollectionSizes counts the available movies of franchises.
func (s *Store) CollectionSizes(ctx context.Context, ids []int) (map[int]int, error) {
	out := map[int]int{}
	if len(ids) == 0 {
		return out, nil
	}
	ph := strings.TrimSuffix(strings.Repeat("?,", len(ids)), ",")
	args := make([]any, len(ids))
	for i, id := range ids {
		args[i] = id
	}
	rows, err := s.db.QueryContext(ctx, fmt.Sprintf(`SELECT ic.collection_id, COUNT(*) FROM item_collections ic
		JOIN items i ON i.id = ic.item_id
		WHERE ic.collection_id IN (%s) AND i.kind = 'movie' AND %s GROUP BY ic.collection_id`, ph, availableSQL), args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var id, n int
		if err := rows.Scan(&id, &n); err != nil {
			return nil, err
		}
		out[id] = n
	}
	return out, rows.Err()
}
