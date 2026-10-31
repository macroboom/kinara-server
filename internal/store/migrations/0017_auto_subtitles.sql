-- Automatic subtitles: the server finds subtitles in the languages its
-- profiles use, checks each against the film's speech (subtitle sync) and
-- keeps a timing correction with it.

-- Which service a downloaded subtitle came from, whether the server
-- fetched it by itself, and the timing correction found by syncing it
-- (original time t plays at t * scale + offset; scale 0 means 1).
ALTER TABLE subtitle_files ADD COLUMN provider TEXT NOT NULL DEFAULT '';
ALTER TABLE subtitle_files ADD COLUMN auto INTEGER NOT NULL DEFAULT 0;
ALTER TABLE subtitle_files ADD COLUMN timing_scale REAL NOT NULL DEFAULT 0;
ALTER TABLE subtitle_files ADD COLUMN timing_offset_ms INTEGER NOT NULL DEFAULT 0;
-- '' (not checked), synced, already (the timing was right), unchecked
-- (couldn't be checked: no speech to compare with)
ALTER TABLE subtitle_files ADD COLUMN sync_state TEXT NOT NULL DEFAULT '';

-- Subtitles wanted for a file: a language (forced: only the foreign parts).
CREATE TABLE subtitle_wanted (
    version_id  TEXT    NOT NULL REFERENCES versions(id) ON DELETE CASCADE,
    language    TEXT    NOT NULL,
    forced      INTEGER NOT NULL DEFAULT 0,
    -- wanted (to look for), found, or missing (nothing good enough yet)
    state       TEXT    NOT NULL DEFAULT 'wanted',
    -- 3 someone is playing it, 2 added lately, 1 watched lately, 0 the rest
    priority    INTEGER NOT NULL DEFAULT 0,
    tries       INTEGER NOT NULL DEFAULT 0,
    -- Not looked for again before this (Unix seconds).
    next_at     INTEGER NOT NULL DEFAULT 0,
    last_at     INTEGER NOT NULL DEFAULT 0,
    note        TEXT    NOT NULL DEFAULT '',
    PRIMARY KEY (version_id, language, forced)
);
CREATE INDEX subtitle_wanted_queue ON subtitle_wanted(state, priority, next_at);

-- Candidates never to pick again for a file: they didn't line up with its
-- speech, or someone said they were wrong.
CREATE TABLE subtitle_rejects (
    version_id  TEXT    NOT NULL REFERENCES versions(id) ON DELETE CASCADE,
    provider    TEXT    NOT NULL,
    key         TEXT    NOT NULL,
    reason      TEXT    NOT NULL DEFAULT '',
    at          INTEGER NOT NULL,
    PRIMARY KEY (version_id, provider, key)
);

-- Downloads from each service per day, for the daily budgets.
CREATE TABLE subtitle_usage (
    provider    TEXT    NOT NULL,
    day         TEXT    NOT NULL,
    count       INTEGER NOT NULL DEFAULT 0,
    PRIMARY KEY (provider, day)
);
