package store

import (
	"context"
	"database/sql"
	"errors"
	"regexp"
	"strings"
)

// Client is an installed app that signed in: a phone, a TV box, a browser.
// Its ID is made up by the app on install, so it stays the same across
// sign-outs and keeps the device's name and trust.
type Client struct {
	ID         string `json:"id"`
	Name       string `json:"name"`
	Platform   string `json:"platform"`
	Brand      string `json:"brand"`
	Model      string `json:"model"`
	OS         string `json:"os"`
	App        string `json:"app"`
	FormFactor string `json:"formFactor,omitempty"`
	Icon       string `json:"icon"`
	Trusted    bool   `json:"trusted"`
	FirstSeen  int64  `json:"firstSeen"`
	LastSeen   int64  `json:"lastSeen"`
}

var clientIDPattern = regexp.MustCompile(`^[A-Za-z0-9_-]{8,64}$`)

// ValidClientID reports whether an app-made client ID is acceptable.
func ValidClientID(id string) bool { return clientIDPattern.MatchString(id) }

// derivedClientPrefix starts the client ID given to sessions from apps that
// don't send one, so they still list and can be trusted.
const derivedClientPrefix = "s-"

// DerivedClientID is the client ID for a session whose app sent none.
func DerivedClientID(tokenHash string) string {
	return derivedClientPrefix + DeviceID(tokenHash)
}

// SessionOfDerivedClient returns the device ID inside a derived client ID,
// or "" when the ID was made by an app.
func SessionOfDerivedClient(clientID string) string {
	rest, ok := strings.CutPrefix(clientID, derivedClientPrefix)
	if !ok || len(rest) != DeviceIDLen {
		return ""
	}
	return rest
}

const clientCols = `id, name, platform, brand, model, os, app, form_factor, icon, trusted, first_seen, last_seen`

func scanClient(r scanner) (Client, error) {
	var c Client
	var trusted int
	err := r.Scan(&c.ID, &c.Name, &c.Platform, &c.Brand, &c.Model, &c.OS, &c.App, &c.FormFactor, &c.Icon, &trusted, &c.FirstSeen, &c.LastSeen)
	c.Trusted = trusted != 0
	if errors.Is(err, sql.ErrNoRows) {
		return c, ErrNotFound
	}
	return c, err
}

// UpsertClient records a client as it signs in. What the app reports
// replaces what was there, but trust and the first-seen time are kept. A
// name the owner gave it is kept too when the app sends none. created
// reports whether this install was new.
func (s *Store) UpsertClient(ctx context.Context, c Client) (out Client, created bool, err error) {
	c.Icon = ClientIcon(c)
	t := now()
	old, err := s.GetClient(ctx, c.ID)
	switch {
	case errors.Is(err, ErrNotFound):
		_, err = s.db.ExecContext(ctx, `INSERT INTO clients (`+clientCols+`) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, 0, ?, ?)`,
			c.ID, c.Name, c.Platform, c.Brand, c.Model, c.OS, c.App, c.FormFactor, c.Icon, t, t)
		if err != nil {
			return Client{}, false, err
		}
		c.FirstSeen, c.LastSeen = t, t
		return c, true, nil
	case err != nil:
		return Client{}, false, err
	}
	// A name the owner gave it stays; so does the old one when the app sends none.
	var named int
	_ = s.db.QueryRowContext(ctx, `SELECT named FROM clients WHERE id = ?`, c.ID).Scan(&named)
	if c.Name == "" || named != 0 {
		c.Name = old.Name
	}
	_, err = s.db.ExecContext(ctx, `UPDATE clients SET name = ?, platform = ?, brand = ?, model = ?, os = ?, app = ?,
		form_factor = ?, icon = ?, last_seen = ? WHERE id = ?`,
		c.Name, c.Platform, c.Brand, c.Model, c.OS, c.App, c.FormFactor, c.Icon, t, c.ID)
	if err != nil {
		return Client{}, false, err
	}
	c.Trusted, c.FirstSeen, c.LastSeen = old.Trusted, old.FirstSeen, t
	return c, false, nil
}

// GetClient returns a client by ID.
func (s *Store) GetClient(ctx context.Context, id string) (Client, error) {
	return scanClient(s.db.QueryRowContext(ctx, `SELECT `+clientCols+` FROM clients WHERE id = ?`, id))
}

// SetSessionDeviceName changes the name a session shows under (now
// playing, the remote access list).
func (s *Store) SetSessionDeviceName(ctx context.Context, tokenHash, name string) error {
	_, err := s.db.ExecContext(ctx, `UPDATE sessions SET device_name = ? WHERE token_hash = ?`, name, tokenHash)
	return err
}

// LinkSessionClient records which client a session signed in from. When
// the session was listed under another client before (an app that only
// now describes itself, listed under one made from its session), what the
// owner set there carries over: trust, and a name they gave it.
func (s *Store) LinkSessionClient(ctx context.Context, tokenHash, clientID string) error {
	var prev string
	if err := s.db.QueryRowContext(ctx, `SELECT client_id FROM sessions WHERE token_hash = ?`, tokenHash).Scan(&prev); err != nil && !errors.Is(err, sql.ErrNoRows) {
		return err
	}
	if prev == "" {
		prev = DerivedClientID(tokenHash)
	}
	if prev != clientID {
		var trusted, named int
		var name string
		err := s.db.QueryRowContext(ctx, `SELECT trusted, named, name FROM clients WHERE id = ?`, prev).Scan(&trusted, &named, &name)
		if err == nil {
			if trusted != 0 {
				if _, err := s.db.ExecContext(ctx, `UPDATE clients SET trusted = 1 WHERE id = ?`, clientID); err != nil {
					return err
				}
			}
			if named != 0 {
				if _, err := s.db.ExecContext(ctx, `UPDATE clients SET name = ?, named = 1 WHERE id = ? AND named = 0`, name, clientID); err != nil {
					return err
				}
			}
		} else if !errors.Is(err, sql.ErrNoRows) {
			return err
		}
	}
	_, err := s.db.ExecContext(ctx, `UPDATE sessions SET client_id = ? WHERE token_hash = ?`, clientID, tokenHash)
	return err
}

// SessionClient is the client a session signed in from. Sessions from apps
// that sent no client get one made up from the session (not saved), named
// after the session, so requests and the device list can still say where
// they came from.
func (s *Store) SessionClient(ctx context.Context, tokenHash string) (Client, error) {
	var clientID, name string
	var created, seen int64
	err := s.db.QueryRowContext(ctx, `SELECT client_id, device_name, created_at, last_seen_at FROM sessions WHERE token_hash = ?`, tokenHash).
		Scan(&clientID, &name, &created, &seen)
	if errors.Is(err, sql.ErrNoRows) {
		return Client{}, ErrNotFound
	}
	if err != nil {
		return Client{}, err
	}
	if clientID != "" {
		if c, err := s.GetClient(ctx, clientID); err == nil {
			return c, nil
		}
	}
	c := sessionOnlyClient(DerivedClientID(tokenHash), name)
	c.FirstSeen, c.LastSeen = created, seen
	// A derived client the owner has already trusted or renamed has a row.
	if saved, err := s.GetClient(ctx, c.ID); err == nil {
		return saved, nil
	}
	return c, nil
}

// sessionOnlyClient describes a session's device from its name alone. Old
// web sign-ins stored the browser's user agent as the name.
func sessionOnlyClient(id, name string) Client {
	c := Client{ID: id, Name: name}
	if browser, osName := BrowserFromUserAgent(name); browser != "" {
		c.Platform = "web"
		c.Model = browser
		if osName != "" {
			c.Model += " on " + osName
		}
		c.Name = c.Model
	}
	c.Icon = ClientIcon(c)
	return c
}

// ClientUpdate is what the owner can change about a client. Nil fields stay.
type ClientUpdate struct {
	Name    *string
	Trusted *bool
}

// UpdateClient changes a client's name or trust. A derived client ID (from a
// session whose app sent none) gets its row made here, and the session is
// linked to it, so the change sticks.
func (s *Store) UpdateClient(ctx context.Context, id string, u ClientUpdate) (Client, error) {
	c, err := s.GetClient(ctx, id)
	if errors.Is(err, ErrNotFound) {
		dev := SessionOfDerivedClient(id)
		if dev == "" {
			return Client{}, ErrNotFound
		}
		var hash, name string
		err := s.db.QueryRowContext(ctx, `SELECT token_hash, device_name FROM sessions WHERE substr(token_hash, 1, ?) = ?`, DeviceIDLen, dev).Scan(&hash, &name)
		if errors.Is(err, sql.ErrNoRows) {
			return Client{}, ErrNotFound
		}
		if err != nil {
			return Client{}, err
		}
		c = sessionOnlyClient(id, name)
		if c, _, err = s.UpsertClient(ctx, c); err != nil {
			return Client{}, err
		}
		if err := s.LinkSessionClient(ctx, hash, id); err != nil {
			return Client{}, err
		}
	} else if err != nil {
		return Client{}, err
	}
	if u.Name != nil {
		c.Name = *u.Name
		// Sessions carry the name too (now playing, the tunnel list).
		if _, err := s.db.ExecContext(ctx, `UPDATE sessions SET device_name = ? WHERE client_id = ?`, c.Name, id); err != nil {
			return Client{}, err
		}
	}
	if u.Trusted != nil {
		c.Trusted = *u.Trusted
	}
	named := 0
	if u.Name != nil {
		named = 1
	}
	_, err = s.db.ExecContext(ctx, `UPDATE clients SET name = ?, trusted = ?, named = MAX(named, ?) WHERE id = ?`, c.Name, boolInt(c.Trusted), named, id)
	return c, err
}
