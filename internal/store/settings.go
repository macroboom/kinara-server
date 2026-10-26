package store

import (
	"context"
	"database/sql"
	"errors"
	"strconv"
)

// Settings are the server-wide options the owner sets on the manager page.
type Settings struct {
	// ListAccounts shows this server's accounts on the sign-in screen, so a
	// TV or phone can pick one instead of typing a user name.
	ListAccounts bool `json:"listAccounts"`
	// AllowSignup lets anyone who can reach the server make an account from
	// the sign-in screen. Off by default: the owner adds accounts instead.
	AllowSignup bool `json:"allowSignup"`
	// PriorityDevice is the device (see DeviceID) that keeps its quality
	// when the server can't convert everything at once: other devices step
	// down first, and it gets a slot even when all are taken. "" for none.
	PriorityDevice string `json:"priorityDevice"`
	// UpdateCheck asks the downloads site twice a day whether a newer
	// Kinara is out (it sends only this server's version and system).
	UpdateCheck bool `json:"updateCheck"`
	// GroupCollections shows movies of the same franchise (TMDB
	// collections, like "The Lord of the Rings") as one card in movie
	// libraries, which opens to the movies in order.
	GroupCollections bool `json:"groupCollections"`
}

// DefaultSettings applies until the owner changes something.
var DefaultSettings = Settings{ListAccounts: true, AllowSignup: false, UpdateCheck: true, GroupCollections: true}

func (s *Store) setting(ctx context.Context, key string) (string, bool, error) {
	var v string
	err := s.db.QueryRowContext(ctx, `SELECT value FROM settings WHERE key = ?`, key).Scan(&v)
	if errors.Is(err, sql.ErrNoRows) {
		return "", false, nil
	}
	return v, err == nil, err
}

func (s *Store) setSetting(ctx context.Context, key, value string) error {
	_, err := s.db.ExecContext(ctx, `INSERT INTO settings (key, value) VALUES (?, ?)
		ON CONFLICT (key) DO UPDATE SET value = excluded.value`, key, value)
	return err
}

func (s *Store) boolSetting(ctx context.Context, key string, def bool) (bool, error) {
	v, ok, err := s.setting(ctx, key)
	if err != nil || !ok {
		return def, err
	}
	b, err := strconv.ParseBool(v)
	if err != nil {
		return def, nil
	}
	return b, nil
}

// GetSettings reads the server settings, with defaults for anything unset.
func (s *Store) GetSettings(ctx context.Context) (Settings, error) {
	out := DefaultSettings
	var err error
	if out.ListAccounts, err = s.boolSetting(ctx, "list_accounts", DefaultSettings.ListAccounts); err != nil {
		return out, err
	}
	if out.AllowSignup, err = s.boolSetting(ctx, "allow_signup", DefaultSettings.AllowSignup); err != nil {
		return out, err
	}
	if out.UpdateCheck, err = s.boolSetting(ctx, "update_check", DefaultSettings.UpdateCheck); err != nil {
		return out, err
	}
	if out.GroupCollections, err = s.boolSetting(ctx, "group_collections", DefaultSettings.GroupCollections); err != nil {
		return out, err
	}
	out.PriorityDevice, _, err = s.setting(ctx, "priority_device")
	return out, err
}

// PutSettings saves the server settings.
func (s *Store) PutSettings(ctx context.Context, st Settings) error {
	if err := s.setSetting(ctx, "list_accounts", strconv.FormatBool(st.ListAccounts)); err != nil {
		return err
	}
	if err := s.setSetting(ctx, "priority_device", st.PriorityDevice); err != nil {
		return err
	}
	if err := s.setSetting(ctx, "update_check", strconv.FormatBool(st.UpdateCheck)); err != nil {
		return err
	}
	if err := s.setSetting(ctx, "group_collections", strconv.FormatBool(st.GroupCollections)); err != nil {
		return err
	}
	return s.setSetting(ctx, "allow_signup", strconv.FormatBool(st.AllowSignup))
}

// Value reads a value the server keeps for itself in the settings table
// ("" when unset): things it remembers between runs, not owner settings.
func (s *Store) Value(ctx context.Context, key string) (string, error) {
	v, _, err := s.setting(ctx, key)
	return v, err
}

// SetValue saves a value read back with Value.
func (s *Store) SetValue(ctx context.Context, key, value string) error {
	return s.setSetting(ctx, key, value)
}
