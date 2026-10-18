package store

import "context"

// DeviceIDLen is how much of a session's token hash names its device. The
// hash can't be turned back into the token, and this much tells devices
// apart.
const DeviceIDLen = 12

// DeviceID names the device signed in with a session.
func DeviceID(tokenHash string) string {
	if len(tokenHash) < DeviceIDLen {
		return tokenHash
	}
	return tokenHash[:DeviceIDLen]
}

// Device is a signed-in device: one session.
type Device struct {
	ID         string `json:"id"`
	Name       string `json:"name"`
	UserID     int64  `json:"userId"`
	Username   string `json:"username"`
	CreatedAt  int64  `json:"createdAt"`
	LastSeenAt int64  `json:"lastSeenAt"`
	// Current marks the device asking.
	Current bool `json:"current,omitempty"`
	// NodeID is the iroh key it last used through the tunnel ("" if it
	// never has).
	NodeID string `json:"nodeId,omitempty"`

	// The app install it signed in from (see Client). Sessions from apps
	// that sent nothing get a client ID made from the session.
	ClientID string `json:"clientId"`
	Platform string `json:"platform"`
	Brand    string `json:"brand"`
	Model    string `json:"model"`
	OS       string `json:"os"`
	App      string `json:"app"`
	Icon     string `json:"icon"`
	Trusted  bool   `json:"trusted"`
	// Label describes the device ("Roku · TCL 55S455"); Name is its own name.
	Label string `json:"label"`
}

// Devices lists signed-in devices, most recently seen first.
func (s *Store) Devices(ctx context.Context) ([]Device, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT s.token_hash, s.device_name, s.user_id, u.username, s.created_at, s.last_seen_at, s.node_id,
		COALESCE(c.id, d.id, ''), COALESCE(c.name, d.name, ''), COALESCE(c.platform, ''), COALESCE(c.brand, ''),
		COALESCE(c.model, ''), COALESCE(c.os, ''), COALESCE(c.app, ''), COALESCE(c.form_factor, ''), COALESCE(c.trusted, d.trusted, 0)
		FROM sessions s JOIN users u ON u.id = s.user_id
		LEFT JOIN clients c ON c.id = s.client_id AND s.client_id != ''
		LEFT JOIN clients d ON d.id = 's-' || substr(s.token_hash, 1, ?) AND s.client_id = ''
		WHERE s.expires_at >= ? ORDER BY s.last_seen_at DESC`, DeviceIDLen, now())
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Device{}
	for rows.Next() {
		var d Device
		var hash, rowID, clientName string
		var c Client
		var trusted int
		if err := rows.Scan(&hash, &d.Name, &d.UserID, &d.Username, &d.CreatedAt, &d.LastSeenAt, &d.NodeID,
			&rowID, &clientName, &c.Platform, &c.Brand, &c.Model, &c.OS, &c.App, &c.FormFactor, &trusted); err != nil {
			return nil, err
		}
		d.ID = DeviceID(hash)
		if clientName != "" {
			d.Name = clientName
		}
		if c.Platform == "" {
			// No app details: describe it from its name (old web sign-ins
			// used the browser's user agent).
			c = sessionOnlyClient(rowID, d.Name)
			d.Name = c.Name
		}
		c.ID, c.Name = rowID, d.Name
		if c.ID == "" {
			c.ID = DerivedClientID(hash)
		}
		d.ClientID, d.Platform, d.Brand, d.Model, d.OS, d.App = c.ID, c.Platform, c.Brand, c.Model, c.OS, c.App
		d.Icon, d.Label, d.Trusted = ClientIcon(c), ClientLabel(c), trusted != 0
		out = append(out, d)
	}
	return out, rows.Err()
}

// DeviceName is the name a session's device gave when it signed in.
func (s *Store) DeviceName(ctx context.Context, tokenHash string) string {
	var name string
	_ = s.db.QueryRowContext(ctx, `SELECT device_name FROM sessions WHERE token_hash = ?`, tokenHash).Scan(&name)
	return name
}

// SignOutDevice ends a device's session. It reports whether there was one.
func (s *Store) SignOutDevice(ctx context.Context, id string) (bool, error) {
	if len(id) != DeviceIDLen {
		return false, nil
	}
	res, err := s.db.ExecContext(ctx, `DELETE FROM sessions WHERE substr(token_hash, 1, ?) = ?`, DeviceIDLen, id)
	if err != nil {
		return false, err
	}
	n, _ := res.RowsAffected()
	return n > 0, nil
}
