-- Franchises (TMDB collections), like "The Lord of the Rings Collection".
-- Movie libraries can show a franchise's movies as one card (Settings,
-- groupCollections).
CREATE TABLE collections (
    id            INTEGER PRIMARY KEY,   -- TMDB's collection ID
    name          TEXT    NOT NULL,
    sort_name     TEXT    NOT NULL,
    overview      TEXT    NOT NULL DEFAULT '',
    poster_path   TEXT    NOT NULL DEFAULT '',
    backdrop_path TEXT    NOT NULL DEFAULT '',
    -- When the overview was fetched (0 = never).
    details_at    INTEGER NOT NULL DEFAULT 0,
    updated_at    INTEGER NOT NULL
);
CREATE TABLE item_collections (
    item_id       TEXT    PRIMARY KEY REFERENCES items(id) ON DELETE CASCADE,
    collection_id INTEGER NOT NULL REFERENCES collections(id) ON DELETE CASCADE
);
CREATE INDEX item_collections_collection ON item_collections(collection_id);
-- When a movie's franchise was looked up (0 = never), so movies matched
-- before franchises were stored get theirs at the next scan.
ALTER TABLE items ADD COLUMN collection_at INTEGER NOT NULL DEFAULT 0;
