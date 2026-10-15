-- One row per time a device plays something, for the manager's usage
-- charts. Title and names are copied in, so history survives a title or
-- account being removed.

CREATE TABLE play_log (
    id           INTEGER PRIMARY KEY,
    started_at   INTEGER NOT NULL,
    updated_at   INTEGER NOT NULL,
    user_id      INTEGER NOT NULL DEFAULT 0,
    username     TEXT    NOT NULL DEFAULT '',
    device       TEXT    NOT NULL,
    device_name  TEXT    NOT NULL DEFAULT '',
    item_id      TEXT    NOT NULL,
    version_id   TEXT    NOT NULL DEFAULT '',
    title        TEXT    NOT NULL DEFAULT '',
    kind         TEXT    NOT NULL DEFAULT '',
    -- direct (the file as is), copy (repackaged or new sound, original
    -- picture) or convert (picture re-encoded)
    method       TEXT    NOT NULL DEFAULT 'direct',
    watched_sec  REAL    NOT NULL DEFAULT 0
);
CREATE INDEX play_log_started ON play_log(started_at);
