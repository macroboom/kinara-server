package store

import (
	"context"
	"database/sql"
	"strconv"
	"strings"
)

// Library scope: what one request may see.
//
// The API puts the signed-in account's libraries on the request's context
// (WithLibraries) when the account can't see them all. Every store method
// that reads items, versions or libraries for a person then keeps to them:
// listings and counts in SQL (scopeSQL), single lookups and everything
// else by dropping rows from other libraries as they're read (inScope), so
// a query that forgot its SQL condition still can't leak. Background work
// (scans, matching, intros) has no scope and sees everything.

type scopeKey struct{}

// libScope is a set of library IDs. A nil *libScope means no limit.
type libScope struct{ ids map[int64]bool }

// WithLibraries limits what ctx can read to these libraries (none at all
// when ids is empty).
func WithLibraries(ctx context.Context, ids []int64) context.Context {
	sc := &libScope{ids: make(map[int64]bool, len(ids))}
	for _, id := range ids {
		sc.ids[id] = true
	}
	return context.WithValue(ctx, scopeKey{}, sc)
}

// Unscoped drops any library limit from ctx (for work done on a person's
// behalf that must see everything, like a scan they started).
func Unscoped(ctx context.Context) context.Context {
	return context.WithValue(ctx, scopeKey{}, (*libScope)(nil))
}

func scopeOf(ctx context.Context) *libScope {
	sc, _ := ctx.Value(scopeKey{}).(*libScope)
	return sc
}

// Scoped reports whether ctx is limited to some libraries.
func Scoped(ctx context.Context) bool { return scopeOf(ctx) != nil }

// CanSeeLibrary reports whether ctx may read library id.
func CanSeeLibrary(ctx context.Context, id int64) bool {
	sc := scopeOf(ctx)
	return sc == nil || sc.ids[id]
}

// scopeSQL is a condition keeping col (a library_id column) to ctx's
// libraries: "1 = 1" when unlimited. IDs are numbers, so they're written
// into the SQL directly.
func scopeSQL(ctx context.Context, col string) string {
	sc := scopeOf(ctx)
	if sc == nil {
		return "1 = 1"
	}
	if len(sc.ids) == 0 {
		return "0 = 1"
	}
	parts := make([]string, 0, len(sc.ids))
	for id := range sc.ids {
		parts = append(parts, strconv.FormatInt(id, 10))
	}
	return col + " IN (" + strings.Join(parts, ",") + ")"
}

func scopeItems(ctx context.Context, items []Item) []Item {
	if scopeOf(ctx) == nil {
		return items
	}
	out := items[:0]
	for _, it := range items {
		if CanSeeLibrary(ctx, it.LibraryID) {
			out = append(out, it)
		}
	}
	return out
}

func scopeVersions(ctx context.Context, vs []Version) []Version {
	if scopeOf(ctx) == nil {
		return vs
	}
	out := vs[:0]
	for _, v := range vs {
		if CanSeeLibrary(ctx, v.LibraryID) {
			out = append(out, v)
		}
	}
	return out
}

// ItemVisible reports whether ctx may see the item with this ID (false for
// one that doesn't exist).
func (s *Store) ItemVisible(ctx context.Context, id string) bool {
	if scopeOf(ctx) == nil {
		return true
	}
	var lib int64
	if err := s.db.QueryRowContext(ctx, `SELECT library_id FROM items WHERE id = ?`, id).Scan(&lib); err != nil {
		return false
	}
	return CanSeeLibrary(ctx, lib)
}

// VersionVisible is ItemVisible for a file.
func (s *Store) VersionVisible(ctx context.Context, id string) bool {
	if scopeOf(ctx) == nil {
		return true
	}
	var lib int64
	if err := s.db.QueryRowContext(ctx, `SELECT library_id FROM versions WHERE id = ?`, id).Scan(&lib); err != nil {
		return false
	}
	return CanSeeLibrary(ctx, lib)
}

// --- Per-account access ---

// LibraryAccess is which libraries an account can see.
type LibraryAccess struct {
	// All: every library, including ones added later.
	All bool `json:"all"`
	// Libraries lists them when All is false.
	Libraries []int64 `json:"libraries"`
}

// UserLibraries says which libraries an account can see. Owners see all.
func (s *Store) UserLibraries(ctx context.Context, userID int64) (LibraryAccess, error) {
	var all, admin int
	err := s.db.QueryRowContext(ctx, `SELECT all_libraries, is_admin FROM users WHERE id = ?`, userID).Scan(&all, &admin)
	if err == sql.ErrNoRows {
		return LibraryAccess{}, ErrNotFound
	}
	if err != nil {
		return LibraryAccess{}, err
	}
	acc := LibraryAccess{All: all != 0 || admin != 0, Libraries: []int64{}}
	if acc.All {
		return acc, nil
	}
	rows, err := s.db.QueryContext(ctx, `SELECT library_id FROM user_libraries WHERE user_id = ? ORDER BY library_id`, userID)
	if err != nil {
		return acc, err
	}
	defer rows.Close()
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			return acc, err
		}
		acc.Libraries = append(acc.Libraries, id)
	}
	return acc, rows.Err()
}

// SetUserLibraries sets which libraries an account can see. Unknown
// library IDs are ignored.
func (s *Store) SetUserLibraries(ctx context.Context, userID int64, acc LibraryAccess) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	res, err := tx.ExecContext(ctx, `UPDATE users SET all_libraries = ? WHERE id = ?`, boolInt(acc.All), userID)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM user_libraries WHERE user_id = ?`, userID); err != nil {
		return err
	}
	if !acc.All {
		for _, id := range acc.Libraries {
			if _, err := tx.ExecContext(ctx, `INSERT OR IGNORE INTO user_libraries (user_id, library_id)
				SELECT ?, id FROM libraries WHERE id = ?`, userID, id); err != nil {
				return err
			}
		}
	}
	return tx.Commit()
}
