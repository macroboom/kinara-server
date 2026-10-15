-- A library can span several folders (a second drive, a new share). Each
-- folder belongs to one library and no folder sits inside another; the API
-- checks the nesting, the UNIQUE path catches exact repeats.
-- libraries.path stays as the library's first folder, so older readers and
-- the CLI keep working.

CREATE TABLE library_folders (
    library_id  INTEGER NOT NULL REFERENCES libraries(id) ON DELETE CASCADE,
    path        TEXT    NOT NULL UNIQUE,
    added_at    INTEGER NOT NULL,
    PRIMARY KEY (library_id, path)
);

INSERT INTO library_folders (library_id, path, added_at)
SELECT id, path, created_at FROM libraries;
