-- Integrations: outgoing webhooks, incoming hooks, title statuses ("on the
-- way") and API keys. Lists and maps are stored as JSON text.

CREATE TABLE integ_webhooks (
    id            INTEGER PRIMARY KEY,
    name          TEXT    NOT NULL,
    url           TEXT    NOT NULL,
    enabled       INTEGER NOT NULL DEFAULT 1,
    secret        TEXT    NOT NULL,
    events        TEXT    NOT NULL DEFAULT '[]',
    -- "all" or a JSON list of IDs.
    libraries     TEXT    NOT NULL DEFAULT '"all"',
    accounts      TEXT    NOT NULL DEFAULT '"all"',
    headers       TEXT    NOT NULL DEFAULT '[]',
    retries       INTEGER NOT NULL DEFAULT 3,
    timeout_sec   INTEGER NOT NULL DEFAULT 10,
    include_paths INTEGER NOT NULL DEFAULT 0,
    created_at    INTEGER NOT NULL
);

-- Deliveries waiting to go out. One webhook's rows go in seq order; a row
-- stays until it is delivered or out of tries, so the queue survives a
-- restart.
CREATE TABLE integ_queue (
    seq         INTEGER PRIMARY KEY AUTOINCREMENT,
    delivery_id TEXT    NOT NULL,
    webhook_id  INTEGER NOT NULL REFERENCES integ_webhooks(id) ON DELETE CASCADE,
    event       TEXT    NOT NULL,
    body        TEXT    NOT NULL,
    -- Tries made so far.
    attempts    INTEGER NOT NULL DEFAULT 0,
    next_at     INTEGER NOT NULL,
    created_at  INTEGER NOT NULL
);
CREATE INDEX integ_queue_webhook ON integ_queue(webhook_id, seq);

-- One row per try, for the delivery log (kept 7 days).
CREATE TABLE integ_attempts (
    id          INTEGER PRIMARY KEY,
    webhook_id  INTEGER NOT NULL REFERENCES integ_webhooks(id) ON DELETE CASCADE,
    delivery_id TEXT    NOT NULL,
    event       TEXT    NOT NULL,
    at          INTEGER NOT NULL,
    -- NULL when no answer came.
    status      INTEGER,
    ms          INTEGER NOT NULL DEFAULT 0,
    attempt     INTEGER NOT NULL DEFAULT 1,
    error       TEXT    NOT NULL DEFAULT '',
    request     TEXT    NOT NULL DEFAULT '',
    -- NULL when no answer came; else the first 4 KB of it.
    response    TEXT
);
CREATE INDEX integ_attempts_webhook ON integ_attempts(webhook_id, id);
CREATE INDEX integ_attempts_at ON integ_attempts(at);

CREATE TABLE integ_incoming (
    id               INTEGER PRIMARY KEY,
    name             TEXT    NOT NULL,
    token            TEXT    NOT NULL UNIQUE,
    enabled          INTEGER NOT NULL DEFAULT 1,
    format           TEXT    NOT NULL DEFAULT 'kinora',
    mapping          TEXT    NOT NULL DEFAULT '{}',
    state_map        TEXT    NOT NULL DEFAULT '[]',
    progress_scale   TEXT    NOT NULL DEFAULT 'auto',
    on_done          TEXT    NOT NULL DEFAULT 'scan',
    expire_hours     INTEGER NOT NULL DEFAULT 6,
    visibility       TEXT    NOT NULL DEFAULT 'owners',
    last_received_at INTEGER NOT NULL DEFAULT 0,
    created_at       INTEGER NOT NULL
);

-- What each incoming hook was sent (the last 200 per hook).
CREATE TABLE integ_received (
    id      INTEGER PRIMARY KEY,
    hook_id INTEGER NOT NULL REFERENCES integ_incoming(id) ON DELETE CASCADE,
    at      INTEGER NOT NULL,
    ok      INTEGER NOT NULL,
    summary TEXT    NOT NULL,
    body    TEXT    NOT NULL
);
CREATE INDEX integ_received_hook ON integ_received(hook_id, id);

-- Titles on their way, as incoming updates last described them. source
-- is "hook:<id>" or "key:<id>"; ident says which title (or the sender's own
-- reference), so a newer update for the same thing replaces the row.
CREATE TABLE integ_status (
    id          INTEGER PRIMARY KEY,
    source      TEXT    NOT NULL,
    source_name TEXT    NOT NULL,
    hook_id     INTEGER REFERENCES integ_incoming(id) ON DELETE CASCADE,
    ident       TEXT    NOT NULL,
    kind        TEXT    NOT NULL DEFAULT '',
    tmdb_id     INTEGER NOT NULL DEFAULT 0,
    title       TEXT    NOT NULL DEFAULT '',
    year        INTEGER NOT NULL DEFAULT 0,
    -- -1 when not given.
    season      INTEGER NOT NULL DEFAULT -1,
    episode     INTEGER NOT NULL DEFAULT -1,
    state       TEXT    NOT NULL,
    -- NULL when not given; else 0 to 1.
    progress    REAL,
    message     TEXT    NOT NULL DEFAULT '',
    eta_sec     INTEGER NOT NULL DEFAULT 0,
    item_id     TEXT    NOT NULL DEFAULT '',
    visibility  TEXT    NOT NULL DEFAULT 'owners',
    updated_at  INTEGER NOT NULL,
    expires_at  INTEGER NOT NULL,
    UNIQUE (source, ident)
);
CREATE INDEX integ_status_title ON integ_status(kind, tmdb_id);

-- API keys. Only a SHA-256 of the key is stored; prefix is its first 12
-- characters, shown so the owner can tell keys apart.
CREATE TABLE integ_keys (
    id           INTEGER PRIMARY KEY,
    name         TEXT    NOT NULL,
    prefix       TEXT    NOT NULL,
    hash         TEXT    NOT NULL UNIQUE,
    scopes       TEXT    NOT NULL DEFAULT '[]',
    network      TEXT    NOT NULL DEFAULT 'local',
    created_at   INTEGER NOT NULL,
    last_used_at INTEGER NOT NULL DEFAULT 0,
    -- 0 for never.
    expires_at   INTEGER NOT NULL DEFAULT 0
);

-- Devices that have signed in before, so device.new fires once per device.
CREATE TABLE integ_seen_devices (
    key        TEXT    PRIMARY KEY,
    created_at INTEGER NOT NULL
);
