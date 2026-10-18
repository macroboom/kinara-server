-- Where the end credits are in an episode file, found by comparing the end
-- of each episode with the ends of others in its season (shared ending
-- music), or failing that by the picture (text on black). A row with
-- end_sec = 0 means the file was checked and no credits were found.
CREATE TABLE end_credits (
    version_id  TEXT    PRIMARY KEY REFERENCES versions(id) ON DELETE CASCADE,
    start_sec   REAL    NOT NULL DEFAULT 0,
    end_sec     REAL    NOT NULL DEFAULT 0,
    -- "audio" or "picture": how they were found (empty when none were).
    source      TEXT    NOT NULL DEFAULT '',
    -- The file's mtime when it was checked; a replaced file is checked again.
    file_mtime  INTEGER NOT NULL DEFAULT 0,
    detected_at INTEGER NOT NULL
);
