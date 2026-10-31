-- Indexes that grow with the library: watch state and show choices by
-- title (deleting or merging titles looks them up that way), and the home
-- page rows across every library ("recently added", "A to Z").
CREATE INDEX IF NOT EXISTS watch_state_item ON watch_state(item_id);
CREATE INDEX IF NOT EXISTS show_prefs_item ON show_prefs(item_id);
CREATE INDEX IF NOT EXISTS items_kind_added ON items(kind, added_at);
CREATE INDEX IF NOT EXISTS items_kind_sort ON items(kind, sort_title);
