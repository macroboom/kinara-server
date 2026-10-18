package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"strings"
)

// API keys, and small lookups the integrations need.

// APIKey lets a script use a few endpoints without signing in. The key
// itself is never stored, only its hash.
type APIKey struct {
	ID         int64
	Name       string
	Prefix     string
	Hash       string
	Scopes     []string
	Network    string
	CreatedAt  int64
	LastUsedAt int64
	ExpiresAt  int64
}

const keyCols = `id, name, prefix, hash, scopes, network, created_at, last_used_at, expires_at`

func scanKey(r scanner) (APIKey, error) {
	var k APIKey
	var scopes string
	err := r.Scan(&k.ID, &k.Name, &k.Prefix, &k.Hash, &scopes, &k.Network, &k.CreatedAt, &k.LastUsedAt, &k.ExpiresAt)
	if err != nil {
		return k, err
	}
	_ = json.Unmarshal([]byte(scopes), &k.Scopes)
	if k.Scopes == nil {
		k.Scopes = []string{}
	}
	return k, nil
}

// CreateAPIKey saves a key, filling in its ID.
func (s *Store) CreateAPIKey(ctx context.Context, k *APIKey) error {
	k.CreatedAt = now()
	res, err := s.db.ExecContext(ctx, `INSERT INTO integ_keys (name, prefix, hash, scopes, network, created_at, expires_at)
		VALUES (?, ?, ?, ?, ?, ?, ?)`, k.Name, k.Prefix, k.Hash, jsonText(k.Scopes), k.Network, k.CreatedAt, k.ExpiresAt)
	if err != nil {
		return err
	}
	k.ID, err = res.LastInsertId()
	return err
}

// ListAPIKeys lists keys, oldest first.
func (s *Store) ListAPIKeys(ctx context.Context) ([]APIKey, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT `+keyCols+` FROM integ_keys ORDER BY id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []APIKey{}
	for rows.Next() {
		k, err := scanKey(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, k)
	}
	return out, rows.Err()
}

// APIKeyByHash finds a key by the hash of what was sent, or ErrNotFound.
func (s *Store) APIKeyByHash(ctx context.Context, hash string) (APIKey, error) {
	k, err := scanKey(s.db.QueryRowContext(ctx, `SELECT `+keyCols+` FROM integ_keys WHERE hash = ?`, hash))
	if errors.Is(err, sql.ErrNoRows) {
		return k, ErrNotFound
	}
	return k, err
}

// TouchAPIKey records that a key was used.
func (s *Store) TouchAPIKey(ctx context.Context, id, at int64) error {
	_, err := s.db.ExecContext(ctx, `UPDATE integ_keys SET last_used_at = ? WHERE id = ?`, at, id)
	return err
}

// DeleteAPIKey revokes a key.
func (s *Store) DeleteAPIKey(ctx context.Context, id int64) error {
	res, err := s.db.ExecContext(ctx, `DELETE FROM integ_keys WHERE id = ?`, id)
	return mustAffect(res, err)
}

// SeeDevice records a device and reports whether it was new.
func (s *Store) SeeDevice(ctx context.Context, key string) (bool, error) {
	res, err := s.db.ExecContext(ctx, `INSERT OR IGNORE INTO integ_seen_devices (key, created_at) VALUES (?, ?)`, key, now())
	if err != nil {
		return false, err
	}
	n, _ := res.RowsAffected()
	return n > 0, nil
}

// SessionClientID is the app install a session belongs to, when the
// sessions table records one ("" otherwise).
func (s *Store) SessionClientID(ctx context.Context, tokenHash string) string {
	var id sql.NullString
	if err := s.db.QueryRowContext(ctx, `SELECT client_id FROM sessions WHERE token_hash = ?`, tokenHash).Scan(&id); err != nil {
		return ""
	}
	return id.String
}

// IntegrationsServerID is a stable ID for this server in webhook payloads,
// made the first time it's asked for.
func (s *Store) IntegrationsServerID(ctx context.Context) (string, error) {
	v, ok, err := s.setting(ctx, "integrations_server_id")
	if err != nil || ok {
		return v, err
	}
	v = "srv_" + NewID()[:12]
	return v, s.setSetting(ctx, "integrations_server_id", v)
}

// ItemExternalIDs lists an item's IDs by provider.
func (s *Store) ItemExternalIDs(ctx context.Context, itemID string) (map[string]string, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT provider, value FROM external_ids WHERE item_id = ?`, itemID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]string{}
	for rows.Next() {
		var p, v string
		if err := rows.Scan(&p, &v); err != nil {
			return nil, err
		}
		out[p] = v
	}
	return out, rows.Err()
}

// FindTitle finds a movie or show in the library by its title (any case)
// and, when year is not 0, its year. "" when there is none.
func (s *Store) FindTitle(ctx context.Context, kind, title string, year int) (Item, error) {
	q := `SELECT ` + itemColsI + ` FROM items i WHERE i.kind = ? AND lower(i.title) = lower(?)`
	args := []any{kind, strings.TrimSpace(title)}
	if year > 0 {
		q += ` AND i.year = ?`
		args = append(args, year)
	}
	it, err := scanItem(s.db.QueryRowContext(ctx, q+` ORDER BY i.added_at LIMIT 1`, args...))
	if errors.Is(err, sql.ErrNoRows) {
		return it, ErrNotFound
	}
	return it, err
}

// --- Scans ---

// GoingMissing lists the items of files not seen since scanStart that are
// not yet flagged missing: what MarkMissing is about to flag. With under
// set, only files inside that folder count.
func (s *Store) GoingMissing(ctx context.Context, libraryID, scanStart int64, under string) ([]string, error) {
	q := `SELECT DISTINCT item_id FROM versions WHERE library_id = ? AND scanned_at < ? AND missing = 0 AND item_id IS NOT NULL`
	args := []any{libraryID, scanStart}
	if under != "" {
		q += ` AND instr(path, ?) = 1`
		args = append(args, under)
	}
	rows, err := s.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		out = append(out, id)
	}
	return out, rows.Err()
}

// MarkMissingUnder is MarkMissing for one folder of a library: files inside
// it not seen since scanStart.
func (s *Store) MarkMissingUnder(ctx context.Context, libraryID, scanStart int64, under string) (int64, error) {
	res, err := s.db.ExecContext(ctx, `UPDATE versions SET missing = 1 WHERE library_id = ? AND scanned_at < ? AND missing = 0
		AND instr(path, ?) = 1`, libraryID, scanStart, under)
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}

// WithoutFiles keeps the items that no longer have a file present.
func (s *Store) WithoutFiles(ctx context.Context, ids []string) ([]string, error) {
	var out []string
	for _, id := range ids {
		var n int
		if err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM versions WHERE item_id = ? AND missing = 0`, id).Scan(&n); err != nil {
			return nil, err
		}
		if n == 0 {
			out = append(out, id)
		}
	}
	return out, nil
}

// ItemPath is where an item's file is: a present one first, else the last
// known place of a missing one ("" without files).
func (s *Store) ItemPath(ctx context.Context, itemID string) string {
	var p string
	_ = s.db.QueryRowContext(ctx, `SELECT path FROM versions WHERE item_id = ? ORDER BY missing, height DESC, part LIMIT 1`, itemID).Scan(&p)
	return p
}
