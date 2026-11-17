-- What happened when an approved request was sent to Radarr or Sonarr:
-- arr_state is '' (not sent: no service set up), sent, or error;
-- arr_note says what (added, searching, already there) or what went wrong.
ALTER TABLE requests ADD COLUMN arr_state TEXT NOT NULL DEFAULT '';
ALTER TABLE requests ADD COLUMN arr_note TEXT NOT NULL DEFAULT '';
ALTER TABLE requests ADD COLUMN arr_at INTEGER NOT NULL DEFAULT 0;
