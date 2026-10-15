-- Lists ("My List" and named lists) for each profile, server settings, and
-- "Other videos" libraries (plain folders of video, no metadata lookups).

CREATE TABLE lists (
    id          INTEGER PRIMARY KEY,
    profile_id  INTEGER NOT NULL REFERENCES profiles(id) ON DELETE CASCADE,
    name        TEXT    NOT NULL,
    -- Each profile has one default list, "My List", made on first use.
    is_default  INTEGER NOT NULL DEFAULT 0,
    created_at  INTEGER NOT NULL
);
CREATE INDEX lists_profile ON lists(profile_id);
CREATE UNIQUE INDEX lists_one_default ON lists(profile_id) WHERE is_default = 1;

CREATE TABLE list_items (
    list_id   INTEGER NOT NULL REFERENCES lists(id) ON DELETE CASCADE,
    item_id   TEXT    NOT NULL REFERENCES items(id) ON DELETE CASCADE,
    added_at  INTEGER NOT NULL,
    PRIMARY KEY (list_id, item_id)
);
CREATE INDEX list_items_item ON list_items(item_id);

-- Server-wide options the owner sets on the manager page.
CREATE TABLE settings (
    key   TEXT PRIMARY KEY,
    value TEXT NOT NULL
);

-- A plain library is shown as kind "other": every video file is its own
-- title, named from the file, with no TMDB matching. (Kept as a flag rather
-- than a new kind, so the libraries table doesn't have to be rebuilt.)
ALTER TABLE libraries ADD COLUMN plain INTEGER NOT NULL DEFAULT 0;
