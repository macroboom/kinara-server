-- Genres for browsing by category, and detected TV intros.

-- TMDB genres per movie or show, by name ("Comedy", "Sci-Fi & Fantasy").
CREATE TABLE item_genres (
    item_id TEXT NOT NULL REFERENCES items(id) ON DELETE CASCADE,
    genre   TEXT NOT NULL,
    PRIMARY KEY (item_id, genre)
);
CREATE INDEX item_genres_genre ON item_genres(genre, item_id);
-- When genres were last fetched (0 = never), so titles matched before
-- genres were stored get them at the next scan.
ALTER TABLE items ADD COLUMN genres_at INTEGER NOT NULL DEFAULT 0;

-- Where the opening titles are in an episode file, found by comparing the
-- audio of episodes in the same season. A row with end_sec = 0 means the
-- file was checked and no intro was found.
CREATE TABLE intros (
    version_id  TEXT    PRIMARY KEY REFERENCES versions(id) ON DELETE CASCADE,
    start_sec   REAL    NOT NULL DEFAULT 0,
    end_sec     REAL    NOT NULL DEFAULT 0,
    -- The file's mtime when it was checked; a replaced file is checked again.
    file_mtime  INTEGER NOT NULL DEFAULT 0,
    detected_at INTEGER NOT NULL
);
