-- Profiles ("who's watching"), per-show track preferences, and cast.

-- A profile is one viewer under an account: its own watch history, next
-- episodes and track preferences. Every account gets one to start with.
CREATE TABLE profiles (
    id            INTEGER PRIMARY KEY,
    user_id       INTEGER NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    name          TEXT    NOT NULL,
    color         TEXT    NOT NULL DEFAULT '#F2B33D',
    pin_hash      TEXT    NOT NULL DEFAULT '',
    kids          INTEGER NOT NULL DEFAULT 0,
    -- Defaults for every title; per-show choices in show_prefs win.
    audio_lang    TEXT    NOT NULL DEFAULT '',
    subtitle_lang TEXT    NOT NULL DEFAULT '',
    subtitle_mode TEXT    NOT NULL DEFAULT 'forced' CHECK (subtitle_mode IN ('off', 'forced', 'always')),
    created_at    INTEGER NOT NULL
);
CREATE INDEX profiles_user ON profiles(user_id);

-- Existing accounts: one profile each, with the same ID, so watch history
-- carries over as is.
INSERT INTO profiles (id, user_id, name, created_at)
    SELECT id, id, username, created_at FROM users;

-- Watch state moves from accounts to profiles.
CREATE TABLE watch_state_new (
    profile_id     INTEGER NOT NULL REFERENCES profiles(id) ON DELETE CASCADE,
    item_id        TEXT    NOT NULL REFERENCES items(id) ON DELETE CASCADE,
    position_sec   REAL    NOT NULL DEFAULT 0,
    completed      INTEGER NOT NULL DEFAULT 0,
    play_count     INTEGER NOT NULL DEFAULT 0,
    last_played_at INTEGER NOT NULL DEFAULT 0,
    client_time    INTEGER NOT NULL DEFAULT 0,
    PRIMARY KEY (profile_id, item_id)
);
INSERT INTO watch_state_new (profile_id, item_id, position_sec, completed, play_count, last_played_at, client_time)
    SELECT user_id, item_id, position_sec, completed, play_count, last_played_at, client_time FROM watch_state;
DROP TABLE watch_state;
ALTER TABLE watch_state_new RENAME TO watch_state;
CREATE INDEX watch_state_recent ON watch_state(profile_id, last_played_at);

-- A device remembers which profile it last picked. NULL means the account's
-- first profile.
ALTER TABLE sessions ADD COLUMN profile_id INTEGER REFERENCES profiles(id) ON DELETE SET NULL;

-- Audio and subtitle choices a profile made for a show or movie, so the next
-- episode starts the same way.
CREATE TABLE show_prefs (
    profile_id    INTEGER NOT NULL REFERENCES profiles(id) ON DELETE CASCADE,
    item_id       TEXT    NOT NULL REFERENCES items(id) ON DELETE CASCADE,
    audio_lang    TEXT    NOT NULL DEFAULT '',
    subtitle_lang TEXT    NOT NULL DEFAULT '',
    subtitle_mode TEXT    NOT NULL DEFAULT '' CHECK (subtitle_mode IN ('', 'off', 'forced', 'always')),
    updated_at    INTEGER NOT NULL,
    PRIMARY KEY (profile_id, item_id)
);

-- Top-billed cast from TMDB, for movies and shows.
CREATE TABLE credits (
    item_id   TEXT    NOT NULL REFERENCES items(id) ON DELETE CASCADE,
    ord       INTEGER NOT NULL,
    name      TEXT    NOT NULL,
    role      TEXT    NOT NULL DEFAULT '',
    photo_url TEXT    NOT NULL DEFAULT '',
    PRIMARY KEY (item_id, ord)
);
-- When cast was last fetched (0 = never), so titles with no cast on TMDB
-- aren't asked again every scan.
ALTER TABLE items ADD COLUMN credits_at INTEGER NOT NULL DEFAULT 0;
