package store

import (
	"context"
	"database/sql"
	"errors"
)

func scanUser(r scanner, extra ...any) (User, error) {
	var u User
	var admin int
	dest := append([]any{&u.ID, &u.Username, &u.PasswordHash, &admin, &u.CreatedAt}, extra...)
	err := r.Scan(dest...)
	u.IsAdmin = admin != 0
	if errors.Is(err, sql.ErrNoRows) {
		return u, ErrNotFound
	}
	return u, err
}

// CreateUser adds an account with one profile named after it.
func (s *Store) CreateUser(ctx context.Context, username, passwordHash string, admin bool) (User, error) {
	t := now()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return User{}, err
	}
	defer tx.Rollback()
	res, err := tx.ExecContext(ctx, qInsertUser, username, passwordHash, boolInt(admin), t)
	if err != nil {
		return User{}, err
	}
	id, err := res.LastInsertId()
	if err != nil {
		return User{}, err
	}
	res, err = tx.ExecContext(ctx, qInsertProfile, id, username, DefaultProfileColor, "", 0, "", "", "forced", t)
	if err != nil {
		return User{}, err
	}
	pid, err := res.LastInsertId()
	if err != nil {
		return User{}, err
	}
	if err := tx.Commit(); err != nil {
		return User{}, err
	}
	return User{ID: id, Username: username, PasswordHash: passwordHash, IsAdmin: admin, CreatedAt: t, ProfileID: pid}, nil
}

func (s *Store) UserByName(ctx context.Context, username string) (User, error) {
	return scanUser(s.db.QueryRowContext(ctx, qGetUserByName, username))
}

func (s *Store) GetUser(ctx context.Context, id int64) (User, error) {
	return scanUser(s.db.QueryRowContext(ctx, qGetUser, id))
}

func (s *Store) CountUsers(ctx context.Context) (int, error) {
	var n int
	err := s.db.QueryRowContext(ctx, qCountUsers).Scan(&n)
	return n, err
}

// SetPassword changes a password and signs the user out everywhere.
func (s *Store) SetPassword(ctx context.Context, userID int64, hash string) error {
	if _, err := s.db.ExecContext(ctx, qSetPassword, hash, userID); err != nil {
		return err
	}
	_, err := s.db.ExecContext(ctx, qDeleteSessions, userID)
	return err
}

func (s *Store) CreateSession(ctx context.Context, tokenHash string, userID int64, device string, expiresAt int64) error {
	t := now()
	_, err := s.db.ExecContext(ctx, qInsertSession, tokenHash, userID, device, t, t, expiresAt)
	return err
}

// SessionUser returns the user for a token hash, with the session's expiry
// and last-seen time. User.ProfileID is the session's profile.
func (s *Store) SessionUser(ctx context.Context, tokenHash string) (User, int64, int64, error) {
	var expires, lastSeen, profile int64
	u, err := scanUser(s.db.QueryRowContext(ctx, qGetSessionUser, tokenHash), &expires, &lastSeen, &profile)
	u.ProfileID = profile
	return u, expires, lastSeen, err
}

// SetSessionProfile records which profile a device is watching as.
func (s *Store) SetSessionProfile(ctx context.Context, tokenHash string, profileID int64) error {
	_, err := s.db.ExecContext(ctx, qSetSessionProfile, profileID, tokenHash)
	return err
}

func (s *Store) TouchSession(ctx context.Context, tokenHash string, expiresAt int64) error {
	_, err := s.db.ExecContext(ctx, qTouchSession, now(), expiresAt, tokenHash)
	return err
}

func (s *Store) DeleteSession(ctx context.Context, tokenHash string) error {
	_, err := s.db.ExecContext(ctx, qDeleteSession, tokenHash)
	return err
}

func (s *Store) ExpireSessions(ctx context.Context) error {
	_, err := s.db.ExecContext(ctx, qExpireSessions, now())
	return err
}

// AccountCard is how an account appears on the sign-in screen's picker.
type AccountCard struct {
	Username string `json:"username"`
	// Color is the account's first profile's colour, for its avatar.
	Color string `json:"color"`
}

// ListUsers returns every account, oldest first (for the manager page).
func (s *Store) ListUsers(ctx context.Context) ([]User, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT id, username, password_hash, is_admin, created_at FROM users ORDER BY created_at, id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []User{}
	for rows.Next() {
		u, err := scanUser(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, u)
	}
	return out, rows.Err()
}

// AccountCards lists accounts for the sign-in picker: name and colour only.
func (s *Store) AccountCards(ctx context.Context) ([]AccountCard, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT u.username,
		COALESCE((SELECT p.color FROM profiles p WHERE p.user_id = u.id ORDER BY p.id LIMIT 1), ?)
		FROM users u ORDER BY u.created_at, u.id`, DefaultProfileColor)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []AccountCard{}
	for rows.Next() {
		var c AccountCard
		if err := rows.Scan(&c.Username, &c.Color); err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

// SetAdmin changes whether an account can manage the server.
func (s *Store) SetAdmin(ctx context.Context, userID int64, admin bool) error {
	res, err := s.db.ExecContext(ctx, `UPDATE users SET is_admin = ? WHERE id = ?`, boolInt(admin), userID)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

// CountAdmins counts accounts that can manage the server.
func (s *Store) CountAdmins(ctx context.Context) (int, error) {
	var n int
	err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM users WHERE is_admin = 1`).Scan(&n)
	return n, err
}

// DeleteUser removes an account with its profiles, sessions and history.
func (s *Store) DeleteUser(ctx context.Context, userID int64) error {
	res, err := s.db.ExecContext(ctx, `DELETE FROM users WHERE id = ?`, userID)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

// DeleteUserSessions signs an account out everywhere (after a password change).
func (s *Store) DeleteUserSessions(ctx context.Context, userID int64) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM sessions WHERE user_id = ?`, userID)
	return err
}
