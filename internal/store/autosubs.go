package store

import "context"

// Whole-library reads for automatic subtitles (see api/autosubs.go), which
// plans every file at once: a few big queries rather than several per file.

// AutoSubsTarget is a movie or episode file with what planning needs.
type AutoSubsTarget struct {
	Version Version
	Kind    string
	// ShowID is an episode's show ("" for movies).
	ShowID string
}

// AutoSubsTargets lists every movie and episode file that's there.
func (s *Store) AutoSubsTargets(ctx context.Context) ([]AutoSubsTarget, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT `+versionCols+`, kind_, show_ FROM (
		SELECT v.*, i.kind AS kind_, COALESCE(se.parent_id, '') AS show_
		FROM versions v JOIN items i ON i.id = v.item_id
		LEFT JOIN items se ON se.id = i.parent_id AND i.kind = 'episode'
		WHERE v.missing = 0 AND i.kind IN ('movie', 'episode'))`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []AutoSubsTarget
	for rows.Next() {
		var t AutoSubsTarget
		v, err := scanVersion(&prefixScanner{sc: rows, post: []any{&t.Kind, &t.ShowID}})
		if err != nil {
			return nil, err
		}
		t.Version = v
		out = append(out, t)
	}
	return out, rows.Err()
}

// AllProfiles lists every profile on the server.
func (s *Store) AllProfiles(ctx context.Context) ([]Profile, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT id, user_id, name, audio_lang, subtitle_lang, subtitle_mode FROM profiles ORDER BY id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Profile
	for rows.Next() {
		var p Profile
		if err := rows.Scan(&p.ID, &p.UserID, &p.Name, &p.AudioLang, &p.SubtitleLang, &p.SubtitleMode); err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

// AllTracks lists every file's audio and subtitle tracks.
func (s *Store) AllTracks(ctx context.Context) (map[string][]Stream, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT version_id, idx, type, codec, profile, language, title, channels, is_default, is_forced
		FROM streams WHERE type IN ('audio', 'subtitle') ORDER BY version_id, idx`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string][]Stream{}
	for rows.Next() {
		var id string
		var st Stream
		var def, forced int
		if err := rows.Scan(&id, &st.Index, &st.Type, &st.Codec, &st.Profile, &st.Language, &st.Title, &st.Channels, &def, &forced); err != nil {
			return nil, err
		}
		st.Default, st.Forced = def != 0, forced != 0
		out[id] = append(out[id], st)
	}
	return out, rows.Err()
}

// AllSubtitleFiles lists every file's subtitle files.
func (s *Store) AllSubtitleFiles(ctx context.Context) (map[string][]SubtitleFile, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT `+subtitleCols+` FROM subtitle_files`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string][]SubtitleFile{}
	for rows.Next() {
		f, err := scanSubtitle(rows)
		if err != nil {
			return nil, err
		}
		out[f.VersionID] = append(out[f.VersionID], f)
	}
	return out, rows.Err()
}

// AllWanted lists every file's wanted subtitles.
func (s *Store) AllWanted(ctx context.Context) (map[string][]SubtitleWant, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT `+wantCols+` FROM subtitle_wanted`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string][]SubtitleWant{}
	for rows.Next() {
		w, err := scanWant(rows)
		if err != nil {
			return nil, err
		}
		out[w.VersionID] = append(out[w.VersionID], w)
	}
	return out, rows.Err()
}

// WatchedSince lists the items anyone played since a time (Unix seconds).
func (s *Store) WatchedSince(ctx context.Context, since int64) (map[string]bool, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT DISTINCT item_id FROM watch_state WHERE last_played_at >= ?`, since)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]bool{}
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		out[id] = true
	}
	return out, rows.Err()
}

// ShowsOf maps episodes to their shows, for the items given.
func (s *Store) ShowsOf(ctx context.Context, items map[string]bool) (map[string]bool, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT i.id, COALESCE(se.parent_id, '') FROM items i
		LEFT JOIN items se ON se.id = i.parent_id WHERE i.kind = 'episode'`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]bool{}
	for rows.Next() {
		var ep, show string
		if err := rows.Scan(&ep, &show); err != nil {
			return nil, err
		}
		if items[ep] && show != "" {
			out[show] = true
		}
	}
	return out, rows.Err()
}
