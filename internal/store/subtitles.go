package store

import (
	"context"
	"database/sql"
	"errors"
	"strconv"
	"strings"
)

// SubtitleFile is a subtitle file that goes with a version, outside it.
type SubtitleFile struct {
	ID              int64  `json:"id"`
	VersionID       string `json:"-"`
	Path            string `json:"-"`
	Language        string `json:"language,omitempty"`
	Title           string `json:"title,omitempty"`
	Format          string `json:"format"`
	Forced          bool   `json:"forced"`
	HearingImpaired bool   `json:"hearingImpaired"`
	// Source is sidecar (next to the video) or download (OpenSubtitles).
	Source     string `json:"source"`
	ProviderID string `json:"-"`
	AddedBy    int64  `json:"-"`
	AddedAt    int64  `json:"addedAt"`
	// Provider is the service it came from ("" for opensubtitles before v2).
	Provider string `json:"provider,omitempty"`
	// Auto: the server fetched it by itself (automatic subtitles).
	Auto bool `json:"auto,omitempty"`
	// TimingScale and TimingOffsetMS correct its timing (see subsync);
	// SyncState says how that went.
	TimingScale    float64 `json:"-"`
	TimingOffsetMS int     `json:"-"`
	SyncState      string  `json:"syncState,omitempty"`
}

// Subtitle file sources.
const (
	SubtitleSidecar  = "sidecar"
	SubtitleDownload = "download"
)

const subtitleCols = `id, version_id, path, language, title, format, forced, hearing_impaired, source, provider_id, COALESCE(added_by, 0), added_at,
	provider, auto, timing_scale, timing_offset_ms, sync_state`

func scanSubtitle(sc interface{ Scan(...any) error }) (SubtitleFile, error) {
	var f SubtitleFile
	var forced, hi, auto int
	err := sc.Scan(&f.ID, &f.VersionID, &f.Path, &f.Language, &f.Title, &f.Format, &forced, &hi, &f.Source, &f.ProviderID, &f.AddedBy, &f.AddedAt,
		&f.Provider, &auto, &f.TimingScale, &f.TimingOffsetMS, &f.SyncState)
	f.Forced, f.HearingImpaired, f.Auto = forced != 0, hi != 0, auto != 0
	return f, err
}

// SubtitleFiles lists a version's subtitle files: sidecars by path, then
// downloads, oldest first.
func (s *Store) SubtitleFiles(ctx context.Context, versionID string) ([]SubtitleFile, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT `+subtitleCols+` FROM subtitle_files WHERE version_id = ?
		ORDER BY source = 'download', CASE WHEN source = 'sidecar' THEN path END, added_at, id`, versionID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []SubtitleFile{}
	for rows.Next() {
		f, err := scanSubtitle(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, f)
	}
	return out, rows.Err()
}

// SubtitleFile reads one.
func (s *Store) SubtitleFile(ctx context.Context, id int64) (SubtitleFile, error) {
	f, err := scanSubtitle(s.db.QueryRowContext(ctx, `SELECT `+subtitleCols+` FROM subtitle_files WHERE id = ?`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return f, ErrNotFound
	}
	return f, err
}

// AddSubtitleFile saves a subtitle file (replacing one at the same path).
func (s *Store) AddSubtitleFile(ctx context.Context, f *SubtitleFile) error {
	var by any
	if f.AddedBy > 0 {
		by = f.AddedBy
	}
	if f.AddedAt == 0 {
		f.AddedAt = now()
	}
	err := s.db.QueryRowContext(ctx, `INSERT INTO subtitle_files
		(version_id, path, language, title, format, forced, hearing_impaired, source, provider_id, added_by, added_at,
		 provider, auto, timing_scale, timing_offset_ms, sync_state)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT (version_id, path) DO UPDATE SET language = excluded.language, title = excluded.title,
			format = excluded.format, forced = excluded.forced, hearing_impaired = excluded.hearing_impaired,
			source = excluded.source, provider_id = excluded.provider_id, provider = excluded.provider,
			auto = excluded.auto, timing_scale = excluded.timing_scale, timing_offset_ms = excluded.timing_offset_ms,
			sync_state = excluded.sync_state
		RETURNING id`,
		f.VersionID, f.Path, f.Language, f.Title, f.Format, boolInt(f.Forced), boolInt(f.HearingImpaired), f.Source, f.ProviderID, by, f.AddedAt,
		f.Provider, boolInt(f.Auto), f.TimingScale, f.TimingOffsetMS, f.SyncState).Scan(&f.ID)
	return err
}

// DeleteSubtitleFile forgets one (the caller removes a downloaded file).
func (s *Store) DeleteSubtitleFile(ctx context.Context, id int64) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM subtitle_files WHERE id = ?`, id)
	return err
}

// SyncSidecars makes a version's sidecar subtitles match what the scanner
// found: new ones added, changed ones updated, gone ones removed.
// Downloads aren't touched.
func (s *Store) SyncSidecars(ctx context.Context, versionID string, found []SubtitleFile) (changed bool, err error) {
	have, err := s.SubtitleFiles(ctx, versionID)
	if err != nil {
		return false, err
	}
	byPath := map[string]SubtitleFile{}
	for _, f := range have {
		if f.Source == SubtitleSidecar {
			byPath[f.Path] = f
		}
	}
	for _, f := range found {
		old, ok := byPath[f.Path]
		delete(byPath, f.Path)
		if ok && old.Language == f.Language && old.Title == f.Title && old.Format == f.Format &&
			old.Forced == f.Forced && old.HearingImpaired == f.HearingImpaired {
			continue
		}
		f.VersionID, f.Source = versionID, SubtitleSidecar
		if ok {
			f.AddedAt = old.AddedAt
		}
		if err := s.AddSubtitleFile(ctx, &f); err != nil {
			return changed, err
		}
		changed = true
	}
	for _, gone := range byPath {
		if err := s.DeleteSubtitleFile(ctx, gone.ID); err != nil {
			return changed, err
		}
		changed = true
	}
	return changed, nil
}

// SetSubtitleTiming saves a subtitle file's timing correction and how the
// check went.
func (s *Store) SetSubtitleTiming(ctx context.Context, id int64, scale float64, offsetMS int, state string) error {
	_, err := s.db.ExecContext(ctx, `UPDATE subtitle_files SET timing_scale = ?, timing_offset_ms = ?, sync_state = ? WHERE id = ?`, scale, offsetMS, state, id)
	return err
}

// --- Automatic subtitles --------------------------------------------------------

// SubtitleWant is a subtitle language wanted for a file.
type SubtitleWant struct {
	VersionID string `json:"versionId"`
	Language  string `json:"language"`
	Forced    bool   `json:"forced"`
	State     string `json:"state"`
	Priority  int    `json:"priority"`
	Tries     int    `json:"tries"`
	NextAt    int64  `json:"nextAt"`
	LastAt    int64  `json:"lastAt"`
	Note      string `json:"note,omitempty"`
}

// Wanted states.
const (
	WantWanted  = "wanted"
	WantFound   = "found"
	WantMissing = "missing"
)

const wantCols = `version_id, language, forced, state, priority, tries, next_at, last_at, note`

func scanWant(sc interface{ Scan(...any) error }) (SubtitleWant, error) {
	var w SubtitleWant
	var forced int
	err := sc.Scan(&w.VersionID, &w.Language, &forced, &w.State, &w.Priority, &w.Tries, &w.NextAt, &w.LastAt, &w.Note)
	w.Forced = forced != 0
	return w, err
}

// SyncWanted makes a file's wanted list match want: new ones are added (to
// look for), ones no longer wanted removed, priorities raised where asked.
// Found and missing ones keep their state.
func (s *Store) SyncWanted(ctx context.Context, versionID string, want []SubtitleWant) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	rows, err := tx.QueryContext(ctx, `SELECT `+wantCols+` FROM subtitle_wanted WHERE version_id = ?`, versionID)
	if err != nil {
		return err
	}
	have := map[string]SubtitleWant{}
	for rows.Next() {
		w, err := scanWant(rows)
		if err != nil {
			rows.Close()
			return err
		}
		have[w.Language+"|"+strconv.FormatBool(w.Forced)] = w
	}
	rows.Close()
	keep := map[string]bool{}
	for _, w := range want {
		k := w.Language + "|" + strconv.FormatBool(w.Forced)
		keep[k] = true
		old, ok := have[k]
		switch {
		case !ok:
			_, err = tx.ExecContext(ctx, `INSERT INTO subtitle_wanted (version_id, language, forced, state, priority) VALUES (?, ?, ?, ?, ?)`,
				versionID, w.Language, boolInt(w.Forced), WantWanted, w.Priority)
		case w.Priority != old.Priority:
			_, err = tx.ExecContext(ctx, `UPDATE subtitle_wanted SET priority = ? WHERE version_id = ? AND language = ? AND forced = ?`,
				w.Priority, versionID, w.Language, boolInt(w.Forced))
		}
		if err != nil {
			return err
		}
	}
	for k, w := range have {
		if keep[k] {
			continue
		}
		if _, err := tx.ExecContext(ctx, `DELETE FROM subtitle_wanted WHERE version_id = ? AND language = ? AND forced = ?`,
			versionID, w.Language, boolInt(w.Forced)); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// NextWanted is the next subtitle to look for: highest priority first, then
// the files added most recently.
func (s *Store) NextWanted(ctx context.Context, nowUnix int64) (SubtitleWant, error) {
	w, err := scanWant(s.db.QueryRowContext(ctx, `SELECT `+prefixed("w.", wantCols)+` FROM subtitle_wanted w
		JOIN versions v ON v.id = w.version_id
		WHERE w.state IN ('wanted', 'missing') AND w.next_at <= ?
		ORDER BY w.priority DESC, w.state = 'missing', v.added_at DESC LIMIT 1`, nowUnix))
	if errors.Is(err, sql.ErrNoRows) {
		return w, ErrNotFound
	}
	return w, err
}

func prefixed(p, cols string) string {
	parts := strings.Split(cols, ",")
	for i, c := range parts {
		parts[i] = p + strings.TrimSpace(c)
	}
	return strings.Join(parts, ", ")
}

// UpdateWanted saves how looking for a subtitle went.
func (s *Store) UpdateWanted(ctx context.Context, w SubtitleWant) error {
	_, err := s.db.ExecContext(ctx, `UPDATE subtitle_wanted SET state = ?, priority = ?, tries = ?, next_at = ?, last_at = ?, note = ?
		WHERE version_id = ? AND language = ? AND forced = ?`,
		w.State, w.Priority, w.Tries, w.NextAt, w.LastAt, w.Note, w.VersionID, w.Language, boolInt(w.Forced))
	return err
}

// WantedFor lists a file's wanted subtitles.
func (s *Store) WantedFor(ctx context.Context, versionID string) ([]SubtitleWant, error) {
	return s.queryWanted(ctx, `WHERE version_id = ? ORDER BY language, forced`, versionID)
}

// WantedByState lists wanted subtitles in a state, highest priority first.
func (s *Store) WantedByState(ctx context.Context, state string, limit int) ([]SubtitleWant, error) {
	return s.queryWanted(ctx, `WHERE state = ? ORDER BY priority DESC, last_at DESC LIMIT ?`, state, limit)
}

// WantedCounts counts wanted subtitles by state.
func (s *Store) WantedCounts(ctx context.Context) (map[string]int, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT state, COUNT(*) FROM subtitle_wanted GROUP BY state`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]int{}
	for rows.Next() {
		var st string
		var n int
		if err := rows.Scan(&st, &n); err != nil {
			return nil, err
		}
		out[st] = n
	}
	return out, rows.Err()
}

func (s *Store) queryWanted(ctx context.Context, where string, args ...any) ([]SubtitleWant, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT `+wantCols+` FROM subtitle_wanted `+where, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []SubtitleWant{}
	for rows.Next() {
		w, err := scanWant(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, w)
	}
	return out, rows.Err()
}

// RejectSubtitle remembers never to pick a candidate for a file again.
func (s *Store) RejectSubtitle(ctx context.Context, versionID, provider, key, reason string) error {
	_, err := s.db.ExecContext(ctx, `INSERT INTO subtitle_rejects (version_id, provider, key, reason, at) VALUES (?, ?, ?, ?, ?)
		ON CONFLICT (version_id, provider, key) DO UPDATE SET reason = excluded.reason, at = excluded.at`, versionID, provider, key, reason, now())
	return err
}

// Rejected lists a file's rejected candidates ("provider|key").
func (s *Store) Rejected(ctx context.Context, versionID string) (map[string]bool, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT provider, key FROM subtitle_rejects WHERE version_id = ?`, versionID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]bool{}
	for rows.Next() {
		var p, k string
		if err := rows.Scan(&p, &k); err != nil {
			return nil, err
		}
		out[p+"|"+k] = true
	}
	return out, rows.Err()
}

// CountSubtitleUse adds one to a service's downloads today and returns
// the new count. day is "2006-01-02".
func (s *Store) CountSubtitleUse(ctx context.Context, provider, day string) (int, error) {
	var n int
	err := s.db.QueryRowContext(ctx, `INSERT INTO subtitle_usage (provider, day, count) VALUES (?, ?, 1)
		ON CONFLICT (provider, day) DO UPDATE SET count = count + 1 RETURNING count`, provider, day).Scan(&n)
	return n, err
}

// SubtitleUse is each service's downloads on a day.
func (s *Store) SubtitleUse(ctx context.Context, day string) (map[string]int, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT provider, count FROM subtitle_usage WHERE day = ?`, day)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]int{}
	for rows.Next() {
		var p string
		var n int
		if err := rows.Scan(&p, &n); err != nil {
			return nil, err
		}
		out[p] = n
	}
	return out, rows.Err()
}
