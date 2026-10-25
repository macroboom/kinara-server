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

// EndCredits is where an episode's end credits are, in seconds. Source
// says how they were found: "audio" or "picture". (Credits are the cast.)
type EndCredits struct {
	Start  float64 `json:"start"`
	End    float64 `json:"end"`
	Source string  `json:"source,omitempty"`
}

// IntroCandidate is an episode file in a season, for intro and credits
// detection.
type IntroCandidate struct {
	VersionID string
	Path      string
	Mtime     int64
	Episode   int
	// Duration is the file's length in seconds (0 when unknown).
	Duration float64
	// Checked is true when the file was already checked for an intro at
	// this mtime.
	Checked bool
	// CreditsChecked is the same for end credits.
	CreditsChecked bool
}

// introActiveWindow is how far back watching or new episodes make a show
// "active" for intro detection.
const introActiveWindow = 30 * 24 * 60 * 60

// SeasonsNeedingIntros lists season IDs with at least one episode file not
// yet checked for an intro or end credits (or replaced since), and at least
// two episodes, most wanted first:
//
//  1. Shows someone watched in the last 30 days: the season they're on,
//     then the next ones, most recently watched show first. That's where a
//     Skip intro button is about to be wanted.
//  2. Seasons with episodes added in the last 30 days (a show that's
//     airing), newest first.
//  3. Earlier seasons of shows being watched (already seen; for rewatches).
//  4. Everything else, newest additions first.
func (s *Store) SeasonsNeedingIntros(ctx context.Context, limit int) ([]string, error) {
	since := now() - introActiveWindow
	rows, err := s.db.QueryContext(ctx, `WITH watched AS (
			SELECT sn.parent_id AS show_id, MAX(w.last_played_at) AS at, MAX(sn.season_number) AS season
			FROM watch_state w
			JOIN items e ON e.id = w.item_id AND e.kind = 'episode'
			JOIN items sn ON sn.id = e.parent_id
			WHERE w.last_played_at > ?
			GROUP BY sn.parent_id),
		fresh AS (
			SELECT e.parent_id AS season_id, MAX(e.added_at) AS at
			FROM items e
			WHERE e.kind = 'episode' AND e.added_at > ?
			GROUP BY e.parent_id)
		SELECT sn.id FROM items sn
		LEFT JOIN watched w ON w.show_id = sn.parent_id
		LEFT JOIN fresh f ON f.season_id = sn.id
		WHERE sn.kind = 'season'
		AND (SELECT COUNT(*) FROM items e JOIN versions v ON v.item_id = e.id
			WHERE e.parent_id = sn.id AND v.missing = 0) >= 2
		AND EXISTS (SELECT 1 FROM items e JOIN versions v ON v.item_id = e.id
			LEFT JOIN intros n ON n.version_id = v.id
			LEFT JOIN end_credits c ON c.version_id = v.id
			WHERE e.parent_id = sn.id AND v.missing = 0 AND v.probe_error = '' AND v.part <= 1
			AND (n.version_id IS NULL OR n.file_mtime != v.mtime
				OR c.version_id IS NULL OR c.file_mtime != v.mtime))
		ORDER BY
			CASE WHEN w.show_id IS NOT NULL AND sn.season_number >= w.season THEN 0
				WHEN f.season_id IS NOT NULL THEN 1
				WHEN w.show_id IS NOT NULL THEN 2
				ELSE 3 END,
			CASE WHEN w.show_id IS NOT NULL AND sn.season_number >= w.season THEN sn.season_number - w.season ELSE 0 END,
			COALESCE(w.at, 0) DESC,
			COALESCE(f.at, 0) DESC,
			sn.added_at DESC,
			sn.season_number
		LIMIT ?`, since, since, limit)
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
	rows, err := s.db.QueryContext(ctx, `SELECT v.id, v.path, v.mtime, e.episode_number, v.duration_sec,
		CASE WHEN n.version_id IS NOT NULL AND n.file_mtime = v.mtime THEN 1 ELSE 0 END,
		CASE WHEN c.version_id IS NOT NULL AND c.file_mtime = v.mtime THEN 1 ELSE 0 END
		FROM items e JOIN versions v ON v.item_id = e.id
		LEFT JOIN intros n ON n.version_id = v.id
		LEFT JOIN end_credits c ON c.version_id = v.id
		WHERE e.parent_id = ? AND e.kind = 'episode' AND v.missing = 0 AND v.probe_error = '' AND v.part <= 1
		ORDER BY e.episode_number, v.height DESC`, seasonID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []IntroCandidate
	for rows.Next() {
		var c IntroCandidate
		if err := rows.Scan(&c.VersionID, &c.Path, &c.Mtime, &c.Episode, &c.Duration, &c.Checked, &c.CreditsChecked); err != nil {
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

// SetEndCredits records end credits for a file (zero end means none were
// found).
func (s *Store) SetEndCredits(ctx context.Context, versionID string, c EndCredits, fileMtime int64) error {
	_, err := s.db.ExecContext(ctx, `INSERT INTO end_credits (version_id, start_sec, end_sec, source, file_mtime, detected_at)
		VALUES (?, ?, ?, ?, ?, ?)
		ON CONFLICT (version_id) DO UPDATE SET start_sec = excluded.start_sec, end_sec = excluded.end_sec,
			source = excluded.source, file_mtime = excluded.file_mtime, detected_at = excluded.detected_at`,
		versionID, c.Start, c.End, c.Source, fileMtime, now())
	return err
}

// EndCreditsFor returns a file's end credits, or nil when there are none
// (or it hasn't been checked).
func (s *Store) EndCreditsFor(ctx context.Context, versionID string) (*EndCredits, error) {
	var c EndCredits
	err := s.db.QueryRowContext(ctx, `SELECT c.start_sec, c.end_sec, c.source FROM end_credits c
		JOIN versions v ON v.id = c.version_id
		WHERE c.version_id = ? AND c.end_sec > 0 AND c.file_mtime = v.mtime`,
		versionID).Scan(&c.Start, &c.End, &c.Source)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &c, nil
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
