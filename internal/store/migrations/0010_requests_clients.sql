-- Apps identify themselves when they sign in, and people can request titles
-- that aren't in the library.

-- One row per installed app (a client), keyed by the ID the app makes up on
-- install. It outlives sessions, so signing out and back in on the same
-- install keeps its name and trust.
CREATE TABLE clients (
    id          TEXT    PRIMARY KEY,
    -- What the owner calls the device (the OS device name), renamable.
    name        TEXT    NOT NULL DEFAULT '',
    -- android, android-tv, ios, tvos, roku or web.
    platform    TEXT    NOT NULL DEFAULT '',
    brand       TEXT    NOT NULL DEFAULT '',
    model       TEXT    NOT NULL DEFAULT '',
    os          TEXT    NOT NULL DEFAULT '',
    app         TEXT    NOT NULL DEFAULT '',
    -- "tablet" when the app says so; empty otherwise.
    form_factor TEXT    NOT NULL DEFAULT '',
    -- Derived from platform, brand and model when it signs in.
    icon        TEXT    NOT NULL DEFAULT '',
    -- Requests from a trusted device are approved straight away.
    trusted     INTEGER NOT NULL DEFAULT 0,
    first_seen  INTEGER NOT NULL,
    last_seen   INTEGER NOT NULL
);

-- The client a session signed in from ('' for apps that don't say).
ALTER TABLE sessions ADD COLUMN client_id TEXT NOT NULL DEFAULT '';
CREATE INDEX sessions_client ON sessions(client_id);

-- Titles people asked for. The title, year, poster and IDs are copied from
-- TMDB when the request is made, so the list needs no TMDB calls.
CREATE TABLE requests (
    id             INTEGER PRIMARY KEY AUTOINCREMENT,
    -- movie or show.
    kind           TEXT    NOT NULL,
    tmdb_id        INTEGER NOT NULL,
    tvdb_id        INTEGER NOT NULL DEFAULT 0,
    imdb_id        TEXT    NOT NULL DEFAULT '',
    title          TEXT    NOT NULL DEFAULT '',
    year           INTEGER NOT NULL DEFAULT 0,
    -- TMDB's image path ("/abc.jpg").
    poster_path    TEXT    NOT NULL DEFAULT '',
    -- all, seasons or pilot (movies are always all).
    mode           TEXT    NOT NULL DEFAULT 'all',
    -- Season numbers for mode "seasons", comma separated and sorted ("1,2").
    seasons        TEXT    NOT NULL DEFAULT '',
    -- pending, approved, declined or available.
    status         TEXT    NOT NULL,
    -- Approved without an owner because the device is trusted.
    auto           INTEGER NOT NULL DEFAULT 0,
    note           TEXT    NOT NULL DEFAULT '',
    decline_reason TEXT    NOT NULL DEFAULT '',
    user_id        INTEGER NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    -- The device it came from, copied so the request still names it after
    -- the device signs out.
    client_id      TEXT    NOT NULL DEFAULT '',
    device_name    TEXT    NOT NULL DEFAULT '',
    device_icon    TEXT    NOT NULL DEFAULT '',
    device_platform TEXT   NOT NULL DEFAULT '',
    created_at     INTEGER NOT NULL,
    decided_at     INTEGER NOT NULL DEFAULT 0
);
CREATE INDEX requests_title ON requests(kind, tmdb_id, status);
CREATE INDEX requests_status ON requests(status, created_at);
CREATE INDEX requests_user ON requests(user_id, created_at);
