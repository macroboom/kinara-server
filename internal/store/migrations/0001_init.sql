-- Kinora initial schema.
-- Times are Unix seconds. Item and version IDs are random 128-bit hex strings.

CREATE TABLE libraries (
    id            INTEGER PRIMARY KEY,
    name          TEXT    NOT NULL,
    path          TEXT    NOT NULL UNIQUE,
    kind          TEXT    NOT NULL CHECK (kind IN ('movies', 'tv')),
    created_at    INTEGER NOT NULL,
    last_scan_at  INTEGER
);

-- What a title is: a movie, or a show with its seasons and episodes.
CREATE TABLE items (
    id               TEXT    PRIMARY KEY,
    library_id       INTEGER NOT NULL REFERENCES libraries(id) ON DELETE CASCADE,
    kind             TEXT    NOT NULL CHECK (kind IN ('movie', 'show', 'season', 'episode')),
    parent_id        TEXT    REFERENCES items(id) ON DELETE CASCADE,
    title            TEXT    NOT NULL,
    sort_title       TEXT    NOT NULL,
    year             INTEGER,
    season_number    INTEGER,
    episode_number   INTEGER,
    overview         TEXT    NOT NULL DEFAULT '',
    rating           REAL,
    runtime_min      INTEGER,
    air_date         TEXT,
    match_state      TEXT    NOT NULL DEFAULT 'pending'
                     CHECK (match_state IN ('pending', 'matched', 'unmatched', 'manual')),
    match_confidence REAL,
    -- What the file names said, kept so rescans find the same item.
    parsed_title     TEXT    NOT NULL DEFAULT '',
    parsed_year      INTEGER NOT NULL DEFAULT 0,
    added_at         INTEGER NOT NULL,
    updated_at       INTEGER NOT NULL
);
CREATE INDEX items_parent         ON items(parent_id, season_number, episode_number);
CREATE INDEX items_library_kind   ON items(library_id, kind, sort_title);
CREATE INDEX items_library_added  ON items(library_id, kind, added_at);
CREATE INDEX items_parsed         ON items(library_id, kind, parsed_title, parsed_year);
CREATE UNIQUE INDEX items_season_unique  ON items(parent_id, season_number)  WHERE kind = 'season';
CREATE UNIQUE INDEX items_episode_unique ON items(parent_id, episode_number) WHERE kind = 'episode';

-- tmdb_movie, tmdb_tv, tmdb_episode, imdb
CREATE TABLE external_ids (
    item_id  TEXT NOT NULL REFERENCES items(id) ON DELETE CASCADE,
    provider TEXT NOT NULL,
    value    TEXT NOT NULL,
    PRIMARY KEY (item_id, provider)
);
CREATE INDEX external_ids_lookup ON external_ids(provider, value);

-- Which files play an item. An item can have several (4K and 1080p).
CREATE TABLE versions (
    id            TEXT    PRIMARY KEY,
    item_id       TEXT    REFERENCES items(id) ON DELETE SET NULL,
    library_id    INTEGER NOT NULL REFERENCES libraries(id) ON DELETE CASCADE,
    path          TEXT    NOT NULL UNIQUE,
    rel_path      TEXT    NOT NULL,
    part          INTEGER NOT NULL DEFAULT 0,
    episode_end   INTEGER,
    size          INTEGER NOT NULL,
    mtime         INTEGER NOT NULL,
    container     TEXT    NOT NULL DEFAULT '',
    duration_sec  REAL    NOT NULL DEFAULT 0,
    bitrate       INTEGER NOT NULL DEFAULT 0,
    width         INTEGER NOT NULL DEFAULT 0,
    height        INTEGER NOT NULL DEFAULT 0,
    video_codec   TEXT    NOT NULL DEFAULT '',
    hdr           TEXT    NOT NULL DEFAULT '',
    dv_profile    INTEGER NOT NULL DEFAULT 0,
    probe_error   TEXT    NOT NULL DEFAULT '',
    missing       INTEGER NOT NULL DEFAULT 0,
    added_at      INTEGER NOT NULL,
    scanned_at    INTEGER NOT NULL
);
CREATE INDEX versions_item    ON versions(item_id);
CREATE INDEX versions_library ON versions(library_id, scanned_at);

CREATE TABLE streams (
    version_id TEXT    NOT NULL REFERENCES versions(id) ON DELETE CASCADE,
    idx        INTEGER NOT NULL,
    type       TEXT    NOT NULL,
    codec      TEXT    NOT NULL DEFAULT '',
    profile    TEXT    NOT NULL DEFAULT '',
    language   TEXT    NOT NULL DEFAULT '',
    title      TEXT    NOT NULL DEFAULT '',
    channels   INTEGER NOT NULL DEFAULT 0,
    is_default INTEGER NOT NULL DEFAULT 0,
    is_forced  INTEGER NOT NULL DEFAULT 0,
    PRIMARY KEY (version_id, idx)
);

CREATE TABLE images (
    item_id TEXT NOT NULL REFERENCES items(id) ON DELETE CASCADE,
    kind    TEXT NOT NULL CHECK (kind IN ('poster', 'backdrop', 'still')),
    url     TEXT NOT NULL,
    PRIMARY KEY (item_id, kind)
);

CREATE TABLE users (
    id            INTEGER PRIMARY KEY,
    username      TEXT    NOT NULL UNIQUE COLLATE NOCASE,
    password_hash TEXT    NOT NULL,
    is_admin      INTEGER NOT NULL DEFAULT 0,
    created_at    INTEGER NOT NULL
);

-- Device tokens. Only a SHA-256 of the token is stored.
CREATE TABLE sessions (
    token_hash   TEXT    PRIMARY KEY,
    user_id      INTEGER NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    device_name  TEXT    NOT NULL DEFAULT '',
    created_at   INTEGER NOT NULL,
    last_seen_at INTEGER NOT NULL,
    expires_at   INTEGER NOT NULL
);
CREATE INDEX sessions_user ON sessions(user_id);

-- What each user did with each item. Attached to items, not versions, so the
-- 4K copy resumes where the 1080p copy stopped.
CREATE TABLE watch_state (
    user_id        INTEGER NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    item_id        TEXT    NOT NULL REFERENCES items(id) ON DELETE CASCADE,
    position_sec   REAL    NOT NULL DEFAULT 0,
    completed      INTEGER NOT NULL DEFAULT 0,
    play_count     INTEGER NOT NULL DEFAULT 0,
    last_played_at INTEGER NOT NULL DEFAULT 0,
    client_time    INTEGER NOT NULL DEFAULT 0,
    PRIMARY KEY (user_id, item_id)
);
CREATE INDEX watch_state_recent ON watch_state(user_id, last_played_at);
