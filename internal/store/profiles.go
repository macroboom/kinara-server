package store

import (
	"context"
	"database/sql"
	"errors"
)

// DefaultProfileColor is the avatar color new profiles get.
const DefaultProfileColor = "#F2B33D"

func scanProfile(r scanner) (Profile, error) {
	var p Profile
	var kids int
	err := r.Scan(&p.ID, &p.UserID, &p.Name, &p.Color, &p.PinHash, &kids, &p.AudioLang, &p.SubtitleLang,
		&p.SubtitleMode, &p.CreatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return p, ErrNotFound
	}
	p.Kids = kids != 0
	p.HasPin = p.PinHash != ""
	return p, err
}

// Profiles lists an account's profiles, oldest first.
func (s *Store) Profiles(ctx context.Context, userID int64) ([]Profile, error) {
	rows, err := s.db.QueryContext(ctx, qProfilesForUser, userID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Profile
	for rows.Next() {
		p, err := scanProfile(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

func (s *Store) GetProfile(ctx context.Context, id int64) (Profile, error) {
	return scanProfile(s.db.QueryRowContext(ctx, qGetProfile, id))
}

// CreateProfile adds a profile; p.ID and p.CreatedAt are filled in.
func (s *Store) CreateProfile(ctx context.Context, p *Profile) error {
	if p.Color == "" {
		p.Color = DefaultProfileColor
	}
	if p.SubtitleMode == "" {
		p.SubtitleMode = "forced"
	}
	p.CreatedAt = now()
	res, err := s.db.ExecContext(ctx, qInsertProfile, p.UserID, p.Name, p.Color, p.PinHash, boolInt(p.Kids),
		p.AudioLang, p.SubtitleLang, p.SubtitleMode, p.CreatedAt)
	if err != nil {
		return err
	}
	p.ID, err = res.LastInsertId()
	p.HasPin = p.PinHash != ""
	return err
}

func (s *Store) UpdateProfile(ctx context.Context, p Profile) error {
	_, err := s.db.ExecContext(ctx, qUpdateProfile, p.Name, p.Color, p.PinHash, boolInt(p.Kids),
		p.AudioLang, p.SubtitleLang, p.SubtitleMode, p.ID)
	return err
}

// DeleteProfile removes a profile and its watch history.
func (s *Store) DeleteProfile(ctx context.Context, id int64) error {
	_, err := s.db.ExecContext(ctx, qDeleteProfile, id)
	return err
}

// TrackPrefs returns a profile's choices for a show or movie (empty if none).
func (s *Store) TrackPrefs(ctx context.Context, profileID int64, itemID string) (TrackPrefs, error) {
	var t TrackPrefs
	err := s.db.QueryRowContext(ctx, qGetTrackPrefs, profileID, itemID).Scan(&t.AudioLang, &t.SubtitleLang, &t.SubtitleMode)
	if errors.Is(err, sql.ErrNoRows) {
		return TrackPrefs{}, nil
	}
	return t, err
}

func (s *Store) PutTrackPrefs(ctx context.Context, profileID int64, itemID string, t TrackPrefs) error {
	_, err := s.db.ExecContext(ctx, qPutTrackPrefs, profileID, itemID, t.AudioLang, t.SubtitleLang, t.SubtitleMode, now())
	return err
}

// Credits returns an item's cast, top-billed first.
func (s *Store) Credits(ctx context.Context, itemID string) ([]Credit, error) {
	rows, err := s.db.QueryContext(ctx, qCredits, itemID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Credit{}
	for rows.Next() {
		var c Credit
		if err := rows.Scan(&c.Order, &c.Name, &c.Role, &c.PhotoURL); err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

// CreditPhoto returns the source URL of a cast member's photo ("" if none).
func (s *Store) CreditPhoto(ctx context.Context, itemID string, order int) (string, error) {
	var url string
	err := s.db.QueryRowContext(ctx, qCreditPhoto, itemID, order).Scan(&url)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	return url, err
}

// SetCredits replaces an item's cast and records when it was fetched.
func (s *Store) SetCredits(ctx context.Context, itemID string, cast []Credit) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx, qDeleteCredits, itemID); err != nil {
		return err
	}
	for i, c := range cast {
		if _, err := tx.ExecContext(ctx, qInsertCredit, itemID, i, c.Name, c.Role, c.PhotoURL); err != nil {
			return err
		}
	}
	if _, err := tx.ExecContext(ctx, qSetCreditsAt, now(), itemID); err != nil {
		return err
	}
	return tx.Commit()
}

// ResetCredits forgets an item's cast so it is fetched again (after its
// match changes).
func (s *Store) ResetCredits(ctx context.Context, itemID string) error {
	if _, err := s.db.ExecContext(ctx, qDeleteCredits, itemID); err != nil {
		return err
	}
	_, err := s.db.ExecContext(ctx, qResetCreditsAt, itemID)
	return err
}

// CastTarget is a matched movie or show whose cast hasn't been fetched.
type CastTarget struct {
	ItemID string
	TMDBID string
	Kind   string
}

// ItemsNeedingCast lists up to limit matched titles with no cast fetched yet.
func (s *Store) ItemsNeedingCast(ctx context.Context, libraryID int64, limit int) ([]CastTarget, error) {
	rows, err := s.db.QueryContext(ctx, qItemsNeedCast, libraryID, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []CastTarget
	for rows.Next() {
		var c CastTarget
		if err := rows.Scan(&c.ItemID, &c.TMDBID, &c.Kind); err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}
