-- Optimized versions: copies the owner asks the server to make ahead of
-- time (a 720p copy of a 4K film, say), so remote play and phones can play
-- them as they are instead of converting live. They're prepared like
-- downloads and kept until the owner's rule goes away.
ALTER TABLE download_files ADD COLUMN optimized INTEGER NOT NULL DEFAULT 0;

-- What to optimize: a whole library, or one title (a movie, an episode, a
-- season or a show), at a quality (high, medium or low).
CREATE TABLE optimize_rules (
    id          INTEGER PRIMARY KEY AUTOINCREMENT,
    library_id  INTEGER REFERENCES libraries(id) ON DELETE CASCADE,
    item_id     TEXT    REFERENCES items(id) ON DELETE CASCADE,
    quality     TEXT    NOT NULL,
    created_at  INTEGER NOT NULL,
    CHECK ((library_id IS NULL) <> (item_id IS NULL))
);
CREATE UNIQUE INDEX optimize_rules_library ON optimize_rules(library_id) WHERE library_id IS NOT NULL;
CREATE UNIQUE INDEX optimize_rules_item ON optimize_rules(item_id) WHERE item_id IS NOT NULL;
