-- Remote access through the kinora-iroh tunnel.

-- The iroh node ID a session last used through the tunnel ('' if it never
-- has). Lets the manager name tunnel peers and block a device's key.
ALTER TABLE sessions ADD COLUMN node_id TEXT NOT NULL DEFAULT '';
CREATE INDEX sessions_node ON sessions(node_id);

-- Devices refused by the tunnel, passed to kinora-iroh on every launch so a
-- restart has no unblocked window.
CREATE TABLE remote_blocked (
    node_id    TEXT    PRIMARY KEY,
    -- What it was when blocked ("Living room TV · alex"), for the manager.
    label      TEXT    NOT NULL DEFAULT '',
    created_at INTEGER NOT NULL
);
