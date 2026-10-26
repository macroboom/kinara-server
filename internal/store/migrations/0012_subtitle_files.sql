-- Subtitle files that go with a video but aren't inside it: found next to
-- it (Movie.en.srt, a Subs folder) by the scanner, or downloaded from
-- OpenSubtitles into the data folder. Apps list them apart from the file's
-- own tracks.
CREATE TABLE subtitle_files (
    id               INTEGER PRIMARY KEY AUTOINCREMENT,
    version_id       TEXT    NOT NULL REFERENCES versions(id) ON DELETE CASCADE,
    path             TEXT    NOT NULL,
    language         TEXT    NOT NULL DEFAULT '',
    title            TEXT    NOT NULL DEFAULT '',
    -- subrip, ass, ssa or webvtt
    format           TEXT    NOT NULL,
    forced           INTEGER NOT NULL DEFAULT 0,
    hearing_impaired INTEGER NOT NULL DEFAULT 0,
    -- sidecar (found by the scanner) or download
    source           TEXT    NOT NULL,
    -- For downloads: the OpenSubtitles file ID, and who got it.
    provider_id      TEXT    NOT NULL DEFAULT '',
    added_by         INTEGER REFERENCES users(id) ON DELETE SET NULL,
    added_at         INTEGER NOT NULL,
    UNIQUE (version_id, path)
);
CREATE INDEX subtitle_files_version ON subtitle_files(version_id);
