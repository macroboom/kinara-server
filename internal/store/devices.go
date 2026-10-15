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
}

// Devices lists signed-in devices, most recently seen first.
func (s *Store) Devices(ctx context.Context) ([]Device, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT s.token_hash, s.device_name, s.user_id, u.username, s.created_at, s.last_seen_at, s.node_id
		FROM sessions s JOIN users u ON u.id = s.user_id WHERE s.expires_at >= ? ORDER BY s.last_seen_at DESC`, now())
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Device{}
	for rows.Next() {
		var d Device
		var hash string
		if err := rows.Scan(&hash, &d.Name, &d.UserID, &d.Username, &d.CreatedAt, &d.LastSeenAt, &d.NodeID); err != nil {
			return nil, err
		}
		d.ID = DeviceID(hash)
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
