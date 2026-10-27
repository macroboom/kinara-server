-- Downloads: titles saved on a phone or tablet to watch without a
-- connection. download_files are the copies the server prepares (shared
-- when two people download the same file at the same quality); downloads
-- are each person's, one per title on each device.
CREATE TABLE download_files (
    id           INTEGER PRIMARY KEY AUTOINCREMENT,
    version_id   TEXT    NOT NULL REFERENCES versions(id) ON DELETE CASCADE,
    -- original, high, medium or low
    quality      TEXT    NOT NULL,
    -- The source audio track kept (-1: the first).
    audio        INTEGER NOT NULL DEFAULT -1,
    -- queued, preparing, ready, failed, or expired (the prepared copy was
    -- removed to save room; asking again prepares it again)
    state        TEXT    NOT NULL,
    -- Share done while preparing, 0 to 1.
    progress     REAL    NOT NULL DEFAULT 0,
    -- The prepared copy, relative to the downloads folder ("" for the
    -- original, which is served from the library).
    path         TEXT    NOT NULL DEFAULT '',
    size         INTEGER NOT NULL DEFAULT 0,
    -- The expected size before it's ready.
    estimate     INTEGER NOT NULL DEFAULT 0,
    error        TEXT    NOT NULL DEFAULT '',
    -- The source file's modified time when it was prepared: a replaced
    -- file is prepared again.
    source_mtime INTEGER NOT NULL DEFAULT 0,
    created_at   INTEGER NOT NULL,
    ready_at     INTEGER NOT NULL DEFAULT 0,
    -- The last time a device fetched from it.
    used_at      INTEGER NOT NULL DEFAULT 0,
    UNIQUE (version_id, quality, audio)
);
CREATE INDEX download_files_state ON download_files(state);

CREATE TABLE downloads (
    id          INTEGER PRIMARY KEY AUTOINCREMENT,
    user_id     INTEGER NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    -- Who's watching, for picking the audio language.
    profile_id  INTEGER NOT NULL DEFAULT 0,
    -- The signed-in device (see DeviceID) and its name.
    device      TEXT    NOT NULL,
    device_name TEXT    NOT NULL DEFAULT '',
    item_id     TEXT    NOT NULL REFERENCES items(id) ON DELETE CASCADE,
    file_id     INTEGER NOT NULL REFERENCES download_files(id) ON DELETE CASCADE,
    created_at  INTEGER NOT NULL,
    -- The last time the device fetched from the file.
    fetched_at  INTEGER NOT NULL DEFAULT 0,
    -- When the device said it has all of it (0: not yet).
    saved_at    INTEGER NOT NULL DEFAULT 0,
    UNIQUE (user_id, device, item_id)
);
CREATE INDEX downloads_file ON downloads(file_id);
CREATE INDEX downloads_user ON downloads(user_id);
