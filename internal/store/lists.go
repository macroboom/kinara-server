package store

import (
	"context"
	"database/sql"
	"errors"
	"strings"
)

// List is a profile's list of titles: its "My List" (IsDefault) or one it named.
type List struct {
	ID        int64  `json:"id"`
	Name      string `json:"name"`
	IsDefault bool   `json:"isDefault"`
	Count     int    `json:"count"`
	CreatedAt int64  `json:"createdAt"`
}

// DefaultListName is what each profile's own list is called.
const DefaultListName = "My List"

// ErrBadInput is returned for a list name or item that can't be used.
var ErrBadInput = errors.New("bad input")

// ensureDefaultList makes the profile's "My List" if it has none yet.
func (s *Store) ensureDefaultList(ctx context.Context, profileID int64) (int64, error) {
	var id int64
	err := s.db.QueryRowContext(ctx, `SELECT id FROM lists WHERE profile_id = ? AND is_default = 1`, profileID).Scan(&id)
	if err == nil {
		return id, nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return 0, err
	}
	// Two requests can race to make it; the unique index keeps one.
	if _, err := s.db.ExecContext(ctx,
		`INSERT OR IGNORE INTO lists (profile_id, name, is_default, created_at) VALUES (?, ?, 1, ?)`,
		profileID, DefaultListName, now()); err != nil {
		return 0, err
	}
	err = s.db.QueryRowContext(ctx, `SELECT id FROM lists WHERE profile_id = ? AND is_default = 1`, profileID).Scan(&id)
	return id, err
}

// DefaultList returns the ID of the profile's "My List", making it if needed.
func (s *Store) DefaultList(ctx context.Context, profileID int64) (int64, error) {
	return s.ensureDefaultList(ctx, profileID)
}

// Lists returns the profile's lists, "My List" first, with how many
// playable titles each holds.
func (s *Store) Lists(ctx context.Context, profileID int64) ([]List, error) {
	if _, err := s.ensureDefaultList(ctx, profileID); err != nil {
		return nil, err
	}
	rows, err := s.db.QueryContext(ctx, `SELECT l.id, l.name, l.is_default, l.created_at,
		(SELECT COUNT(*) FROM list_items li JOIN items i ON i.id = li.item_id WHERE li.list_id = l.id AND `+availableTop+`)
		FROM lists l WHERE l.profile_id = ? ORDER BY l.is_default DESC, l.name COLLATE NOCASE`, profileID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []List{}
	for rows.Next() {
		var l List
		if err := rows.Scan(&l.ID, &l.Name, &l.IsDefault, &l.CreatedAt, &l.Count); err != nil {
			return nil, err
		}
		out = append(out, l)
	}
	return out, rows.Err()
}

// availableTop: a movie with a file, or a show with any episode file.
const availableTop = `(
	(i.kind = 'movie' AND EXISTS (SELECT 1 FROM versions v WHERE v.item_id = i.id AND v.missing = 0))
	OR (i.kind = 'show' AND EXISTS (SELECT 1 FROM items sn JOIN items e ON e.parent_id = sn.id
		JOIN versions v ON v.item_id = e.id WHERE sn.parent_id = i.id AND v.missing = 0)))`

func cleanListName(name string) (string, error) {
	name = strings.TrimSpace(name)
	if name == "" || len(name) > 80 {
		return "", ErrBadInput
	}
	return name, nil
}

// CreateList makes a named list for the profile.
func (s *Store) CreateList(ctx context.Context, profileID int64, name string) (List, error) {
	name, err := cleanListName(name)
	if err != nil {
		return List{}, err
	}
	t := now()
	res, err := s.db.ExecContext(ctx, `INSERT INTO lists (profile_id, name, is_default, created_at) VALUES (?, ?, 0, ?)`, profileID, name, t)
	if err != nil {
		return List{}, err
	}
	id, err := res.LastInsertId()
	return List{ID: id, Name: name, CreatedAt: t}, err
}

// ownList checks the list belongs to the profile.
func (s *Store) ownList(ctx context.Context, profileID, listID int64) (List, error) {
	var l List
	err := s.db.QueryRowContext(ctx, `SELECT id, name, is_default, created_at FROM lists WHERE id = ? AND profile_id = ?`,
		listID, profileID).Scan(&l.ID, &l.Name, &l.IsDefault, &l.CreatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return l, ErrNotFound
	}
	return l, err
}

// RenameList renames one of the profile's lists.
func (s *Store) RenameList(ctx context.Context, profileID, listID int64, name string) error {
	name, err := cleanListName(name)
	if err != nil {
		return err
	}
	if _, err := s.ownList(ctx, profileID, listID); err != nil {
		return err
	}
	_, err = s.db.ExecContext(ctx, `UPDATE lists SET name = ? WHERE id = ?`, name, listID)
	return err
}

// DeleteList removes a named list. "My List" can't be removed, only emptied.
func (s *Store) DeleteList(ctx context.Context, profileID, listID int64) error {
	l, err := s.ownList(ctx, profileID, listID)
	if err != nil {
		return err
	}
	if l.IsDefault {
		return ErrBadInput
	}
	_, err = s.db.ExecContext(ctx, `DELETE FROM lists WHERE id = ?`, listID)
	return err
}

// ItemsInList returns the playable titles in a list, most recently added first.
func (s *Store) ItemsInList(ctx context.Context, profileID, listID int64) ([]Item, error) {
	if _, err := s.ownList(ctx, profileID, listID); err != nil {
		return nil, err
	}
	rows, err := s.db.QueryContext(ctx, `SELECT `+itemColsI+` FROM list_items li JOIN items i ON i.id = li.item_id
		WHERE li.list_id = ? AND `+availableTop+` ORDER BY li.added_at DESC`, listID)
	if err != nil {
		return nil, err
	}
	return scanItems(rows)
}

// AddToList puts a movie or show on one of the profile's lists (again is fine).
func (s *Store) AddToList(ctx context.Context, profileID, listID int64, itemID string) error {
	if _, err := s.ownList(ctx, profileID, listID); err != nil {
		return err
	}
	it, err := s.GetItem(ctx, itemID)
	if err != nil {
		return err
	}
	if it.Kind != ItemMovie && it.Kind != ItemShow {
		return ErrBadInput
	}
	_, err = s.db.ExecContext(ctx, `INSERT INTO list_items (list_id, item_id, added_at) VALUES (?, ?, ?)
		ON CONFLICT (list_id, item_id) DO NOTHING`, listID, itemID, now())
	return err
}

// RemoveFromList takes a title off one of the profile's lists.
func (s *Store) RemoveFromList(ctx context.Context, profileID, listID int64, itemID string) error {
	if _, err := s.ownList(ctx, profileID, listID); err != nil {
		return err
	}
	_, err := s.db.ExecContext(ctx, `DELETE FROM list_items WHERE list_id = ? AND item_id = ?`, listID, itemID)
	return err
}

// ListsWith returns which of the profile's lists hold an item.
func (s *Store) ListsWith(ctx context.Context, profileID int64, itemID string) ([]int64, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT l.id FROM lists l JOIN list_items li ON li.list_id = l.id
		WHERE l.profile_id = ? AND li.item_id = ? ORDER BY l.is_default DESC, l.id`, profileID, itemID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []int64{}
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		out = append(out, id)
	}
	return out, rows.Err()
}
