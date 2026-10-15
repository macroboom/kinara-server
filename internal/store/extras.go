package store

import (
	"context"
	"database/sql"
	"errors"
)

// --- Genres ---

// GenreTarget is a matched movie or show whose genres need fetching.
type GenreTarget struct {
	ItemID string
	TMDBID string
	Kind   string
}

// GenreCount is a genre and how many available titles have it.
type GenreCount struct {
	Name  string `json:"name"`
	Count int    `json:"count"`
}

// SetGenres replaces an item's genres and records when they were fetched.
func (s *Store) SetGenres(ctx context.Context, itemID string, genres []string) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx, `DELETE FROM item_genres WHERE item_id = ?`, itemID); err != nil {
		return err
	}
	for _, g := range genres {
		if g == "" {
			continue
		}
		if _, err := tx.ExecContext(ctx,
			`INSERT OR IGNORE INTO item_genres (item_id, genre) VALUES (?, ?)`, itemID, g); err != nil {
			return err
		}
	}
	if _, err := tx.ExecContext(ctx, `UPDATE items SET genres_at = ? WHERE id = ?`, now(), itemID); err != nil {
		return err
	}
	return tx.Commit()
}

// ItemsNeedingGenres lists up to limit matched titles with no genres fetched yet.
func (s *Store) ItemsNeedingGenres(ctx context.Context, libraryID int64, limit int) ([]GenreTarget, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT i.id, e.value, i.kind FROM items i
		JOIN external_ids e ON e.item_id = i.id
		WHERE i.library_id = ? AND i.kind IN ('movie', 'show') AND i.genres_at = 0
		AND e.provider = CASE i.kind WHEN 'movie' THEN 'tmdb_movie' ELSE 'tmdb_tv' END
		LIMIT ?`, libraryID, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []GenreTarget
	for rows.Next() {
		var g GenreTarget
		if err := rows.Scan(&g.ItemID, &g.TMDBID, &g.Kind); err != nil {
			return nil, err
		}
		out = append(out, g)
	}
	return out, rows.Err()
}

// ResetGenres forgets an item's genres so they are fetched again.
func (s *Store) ResetGenres(ctx context.Context, itemID string) error {
	if _, err := s.db.ExecContext(ctx, `DELETE FROM item_genres WHERE item_id = ?`, itemID); err != nil {
		return err
	}
	_, err := s.db.ExecContext(ctx, `UPDATE items SET genres_at = 0 WHERE id = ?`, itemID)
	return err
}

// Genres returns the genres of the given items, keyed by item ID.
func (s *Store) Genres(ctx context.Context, itemIDs []string) (map[string][]string, error) {
	out := map[string][]string{}
	for start := 0; start < len(itemIDs); start += 500 {
		chunk := itemIDs[start:min(start+500, len(itemIDs))]
		args := make([]any, len(chunk))
		for i, id := range chunk {
			args[i] = id
		}
		rows, err := s.db.QueryContext(ctx, `SELECT item_id, genre FROM item_genres
			WHERE item_id IN (`+placeholders(len(chunk))+`) ORDER BY item_id, genre`, args...)
		if err != nil {
			return nil, err
		}
		for rows.Next() {
			var id, g string
			if err := rows.Scan(&id, &g); err != nil {
				rows.Close()
				return nil, err
			}
			out[id] = append(out[id], g)
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return nil, err
		}
	}
	return out, nil
}

// GenreCounts lists genres with available titles, most titles first. kind
// limits it to movies or shows ("" for both).
func (s *Store) GenreCounts(ctx context.Context, kind string) ([]GenreCount, error) {
	q := `SELECT g.genre, COUNT(*) FROM item_genres g JOIN items i ON i.id = g.item_id
		WHERE ` + availableSQL
	var args []any
	if kind != "" {
		q += ` AND i.kind = ?`
		args = append(args, kind)
	}
	q += ` GROUP BY g.genre ORDER BY COUNT(*) DESC, g.genre`
	rows, err := s.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []GenreCount{}
	for rows.Next() {
		var g GenreCount
		if err := rows.Scan(&g.Name, &g.Count); err != nil {
			return nil, err
		}
		out = append(out, g)
	}
	return out, rows.Err()
}

// ItemsByExternalIDs maps TMDB IDs to available library items: provider is
// tmdb_movie or tmdb_tv. Used to mark trending titles that are in the library.
func (s *Store) ItemsByExternalIDs(ctx context.Context, provider string, values []string) (map[string]Item, error) {
	out := map[string]Item{}
	if len(values) == 0 {
		return out, nil
	}
	args := []any{provider}
	for _, v := range values {
		args = append(args, v)
	}
	rows, err := s.db.QueryContext(ctx, `SELECT e.value, `+itemColsI+` FROM external_ids e
		JOIN items i ON i.id = e.item_id
		WHERE e.provider = ? AND e.value IN (`+placeholders(len(values))+`) AND `+availableSQL, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var v string
		var it Item
		err := rows.Scan(&v, &it.ID, &it.LibraryID, &it.Kind, &it.ParentID, &it.Title, &it.SortTitle,
			&it.Year, &it.SeasonNumber, &it.EpisodeNumber, &it.Overview, &it.Rating, &it.RuntimeMin,
			&it.AirDate, &it.MatchState, &it.MatchConfidence, &it.ParsedTitle, &it.ParsedYear,
			&it.AddedAt, &it.UpdatedAt)
		if err != nil {
			return nil, err
		}
		if _, dup := out[v]; !dup {
			out[v] = it
		}
	}
	return out, rows.Err()
}

// --- Intros ---

// Intro is where an episode's opening titles are, in seconds.
type Intro struct {
	Start float64 `json:"start"`
	End   float64 `json:"end"`
}

// IntroCandidate is an episode file in a season, for intro detection.
type IntroCandidate struct {
	VersionID string
	Path      string
	Mtime     int64
	Episode   int
	// Checked is true when the file was already checked at this mtime.
	Checked bool
}

// SeasonsNeedingIntros lists season IDs with at least one episode file not
// yet checked for an intro (or replaced since), and at least two episodes.
func (s *Store) SeasonsNeedingIntros(ctx context.Context, limit int) ([]string, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT sn.id FROM items sn
		WHERE sn.kind = 'season'
		AND (SELECT COUNT(*) FROM items e JOIN versions v ON v.item_id = e.id
			WHERE e.parent_id = sn.id AND v.missing = 0) >= 2
		AND EXISTS (SELECT 1 FROM items e JOIN versions v ON v.item_id = e.id
			LEFT JOIN intros n ON n.version_id = v.id
			WHERE e.parent_id = sn.id AND v.missing = 0 AND v.probe_error = '' AND v.part <= 1
			AND (n.version_id IS NULL OR n.file_mtime != v.mtime))
		ORDER BY sn.parent_id, sn.season_number
		LIMIT ?`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		out = append(out, id)
	}
	return out, rows.Err()
}

// SeasonIntroFiles lists a season's episode files in episode order (first
// part of each episode only).
func (s *Store) SeasonIntroFiles(ctx context.Context, seasonID string) ([]IntroCandidate, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT v.id, v.path, v.mtime, e.episode_number,
		CASE WHEN n.version_id IS NOT NULL AND n.file_mtime = v.mtime THEN 1 ELSE 0 END
		FROM items e JOIN versions v ON v.item_id = e.id
		LEFT JOIN intros n ON n.version_id = v.id
		WHERE e.parent_id = ? AND e.kind = 'episode' AND v.missing = 0 AND v.probe_error = '' AND v.part <= 1
		ORDER BY e.episode_number, v.height DESC`, seasonID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []IntroCandidate
	for rows.Next() {
		var c IntroCandidate
		if err := rows.Scan(&c.VersionID, &c.Path, &c.Mtime, &c.Episode, &c.Checked); err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

// SetIntro records an intro for a file (zero end means none was found).
func (s *Store) SetIntro(ctx context.Context, versionID string, in Intro, fileMtime int64) error {
	_, err := s.db.ExecContext(ctx, `INSERT INTO intros (version_id, start_sec, end_sec, file_mtime, detected_at)
		VALUES (?, ?, ?, ?, ?)
		ON CONFLICT (version_id) DO UPDATE SET start_sec = excluded.start_sec, end_sec = excluded.end_sec,
			file_mtime = excluded.file_mtime, detected_at = excluded.detected_at`,
		versionID, in.Start, in.End, fileMtime, now())
	return err
}

// IntroFor returns a file's intro, or nil when there is none (or it hasn't
// been checked).
func (s *Store) IntroFor(ctx context.Context, versionID string) (*Intro, error) {
	var in Intro
	// Only for the file as it is now: a replaced file waits for a new check.
	err := s.db.QueryRowContext(ctx, `SELECT n.start_sec, n.end_sec FROM intros n
		JOIN versions v ON v.id = n.version_id
		WHERE n.version_id = ? AND n.end_sec > 0 AND n.file_mtime = v.mtime`,
		versionID).Scan(&in.Start, &in.End)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &in, nil
}

// AllVersions lists every available, readable file (for background work
// such as preview thumbnails), newest first.
func (s *Store) AllVersions(ctx context.Context) ([]Version, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT `+versionCols+` FROM versions
		WHERE missing = 0 AND probe_error = '' AND item_id IS NOT NULL ORDER BY added_at DESC`)
	if err != nil {
		return nil, err
	}
	return scanVersions(rows)
}

// ThumbTarget is an "Other videos" file whose title has no picture yet.
type ThumbTarget struct {
	VersionID   string
	ItemID      string
	Path        string
	DurationSec float64
	HDR         string
}

// VideosNeedingThumbs lists files whose title has no backdrop, for the
// background worker to grab a frame from: files in plain libraries, and
// movies and shows that didn't match (a show gets a frame from one of its
// episodes), so they have a picture until they're matched.
func (s *Store) VideosNeedingThumbs(ctx context.Context, limit int) ([]ThumbTarget, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT v.id, v.item_id, v.path, v.duration_sec, v.hdr
		FROM versions v JOIN libraries l ON l.id = v.library_id JOIN items i ON i.id = v.item_id
		WHERE v.missing = 0 AND v.probe_error = ''
		AND (l.plain = 1 OR (i.kind = 'movie' AND i.match_state = 'unmatched'))
		AND NOT EXISTS (SELECT 1 FROM images im WHERE im.item_id = v.item_id AND im.kind = 'backdrop')
		UNION ALL
		SELECT v.id, sh.id, v.path, v.duration_sec, v.hdr
		FROM items sh
		JOIN items sn ON sn.parent_id = sh.id AND sn.kind = 'season'
		JOIN items e ON e.parent_id = sn.id AND e.kind = 'episode'
		JOIN versions v ON v.item_id = e.id
		WHERE sh.kind = 'show' AND sh.match_state = 'unmatched'
		AND v.missing = 0 AND v.probe_error = ''
		AND NOT EXISTS (SELECT 1 FROM images im WHERE im.item_id = sh.id AND im.kind = 'backdrop')
		LIMIT ?`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []ThumbTarget
	for rows.Next() {
		var t ThumbTarget
		if err := rows.Scan(&t.VersionID, &t.ItemID, &t.Path, &t.DurationSec, &t.HDR); err != nil {
			return nil, err
		}
		out = append(out, t)
	}
	return out, rows.Err()
}
