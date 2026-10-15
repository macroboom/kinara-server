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
}

// DefaultSettings applies until the owner changes something.
var DefaultSettings = Settings{ListAccounts: true, AllowSignup: false}

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
	return s.setSetting(ctx, "allow_signup", strconv.FormatBool(st.AllowSignup))
}
