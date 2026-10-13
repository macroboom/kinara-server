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
