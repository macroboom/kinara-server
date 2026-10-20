package store

import (
	"context"
	"strconv"
)

// SetSessionNode records the iroh node ID a session is using through the
// tunnel.
func (s *Store) SetSessionNode(ctx context.Context, tokenHash, nodeID string) error {
	_, err := s.db.ExecContext(ctx, `UPDATE sessions SET node_id = ? WHERE token_hash = ?`, nodeID, tokenHash)
	return err
}

// DeviceNodes lists the node IDs a device (see DeviceID) has used through
// the tunnel.
func (s *Store) DeviceNodes(ctx context.Context, id string) ([]string, error) {
	if len(id) != DeviceIDLen {
		return nil, nil
	}
	rows, err := s.db.QueryContext(ctx, `SELECT DISTINCT node_id FROM sessions
		WHERE substr(token_hash, 1, ?) = ? AND node_id != ''`, DeviceIDLen, id)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var n string
		if err := rows.Scan(&n); err != nil {
			return nil, err
		}
		out = append(out, n)
	}
	return out, rows.Err()
}

// NodeDevice is the signed-in device behind a node ID.
type NodeDevice struct {
	DeviceID string `json:"deviceId"`
	Name     string `json:"name"`
	Username string `json:"username"`
}

// NodeDevices maps node IDs to the device most recently seen using each.
func (s *Store) NodeDevices(ctx context.Context) (map[string]NodeDevice, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT s.node_id, s.token_hash, s.device_name, u.username
		FROM sessions s JOIN users u ON u.id = s.user_id
		WHERE s.node_id != '' AND s.expires_at >= ? ORDER BY s.last_seen_at ASC`, now())
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]NodeDevice{}
	for rows.Next() {
		var node, hash string
		var d NodeDevice
		if err := rows.Scan(&node, &hash, &d.Name, &d.Username); err != nil {
			return nil, err
		}
		d.DeviceID = DeviceID(hash)
		// Ordered oldest first, so the latest session wins.
		out[node] = d
	}
	return out, rows.Err()
}

// BlockedNode is a device the tunnel refuses.
type BlockedNode struct {
	NodeID    string `json:"nodeId"`
	Label     string `json:"label"`
	CreatedAt int64  `json:"createdAt"`
}

// BlockNode adds a node ID to the tunnel's refused list.
func (s *Store) BlockNode(ctx context.Context, nodeID, label string) error {
	_, err := s.db.ExecContext(ctx, `INSERT INTO remote_blocked (node_id, label, created_at) VALUES (?, ?, ?)
		ON CONFLICT (node_id) DO UPDATE SET label = CASE WHEN excluded.label != '' THEN excluded.label ELSE remote_blocked.label END`,
		nodeID, label, now())
	return err
}

// UnblockNode takes a node ID off the refused list. It reports whether it
// was there.
func (s *Store) UnblockNode(ctx context.Context, nodeID string) (bool, error) {
	res, err := s.db.ExecContext(ctx, `DELETE FROM remote_blocked WHERE node_id = ?`, nodeID)
	if err != nil {
		return false, err
	}
	n, _ := res.RowsAffected()
	return n > 0, nil
}

// BlockedNodes lists refused devices, newest first.
func (s *Store) BlockedNodes(ctx context.Context) ([]BlockedNode, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT node_id, label, created_at FROM remote_blocked ORDER BY created_at DESC, node_id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []BlockedNode{}
	for rows.Next() {
		var b BlockedNode
		if err := rows.Scan(&b.NodeID, &b.Label, &b.CreatedAt); err != nil {
			return nil, err
		}
		out = append(out, b)
	}
	return out, rows.Err()
}

// RemoteSettings are the remote access options, kept apart from Settings
// because changing them restarts the tunnel.
type RemoteSettings struct {
	// Enabled starts kinara-iroh so apps can reach the server from anywhere.
	Enabled bool `json:"enabled"`
	// UDPPort fixes the tunnel's UDP port (IPv4 and IPv6), for a firewall
	// rule or a port forward. 0 lets it pick.
	UDPPort int `json:"udpPort"`
}

// GetRemoteSettings reads the remote access options (off by default).
func (s *Store) GetRemoteSettings(ctx context.Context) (RemoteSettings, error) {
	var out RemoteSettings
	var err error
	if out.Enabled, err = s.boolSetting(ctx, "remote_enabled", false); err != nil {
		return out, err
	}
	v, ok, err := s.setting(ctx, "remote_udp_port")
	if err != nil || !ok {
		return out, err
	}
	if n, err := strconv.Atoi(v); err == nil && n > 0 && n < 65536 {
		out.UDPPort = n
	}
	return out, nil
}

// PutRemoteSettings saves the remote access options.
func (s *Store) PutRemoteSettings(ctx context.Context, rs RemoteSettings) error {
	if err := s.setSetting(ctx, "remote_enabled", strconv.FormatBool(rs.Enabled)); err != nil {
		return err
	}
	return s.setSetting(ctx, "remote_udp_port", strconv.Itoa(rs.UDPPort))
}
