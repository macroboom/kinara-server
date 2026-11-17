-- What a subtitle file's timing check was made against: the video's and
-- the subtitle's modification times. When either changes (a new release
-- of the film, an edited .srt next to it) the server checks it again.
-- '' for files checked before this was kept (taken as current).
ALTER TABLE subtitle_files ADD COLUMN sync_key TEXT NOT NULL DEFAULT '';
