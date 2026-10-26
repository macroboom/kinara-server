package store

import (
	"context"
	"database/sql"
	"errors"
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
}

// Subtitle file sources.
const (
	SubtitleSidecar  = "sidecar"
	SubtitleDownload = "download"
)

const subtitleCols = `id, version_id, path, language, title, format, forced, hearing_impaired, source, provider_id, COALESCE(added_by, 0), added_at`

func scanSubtitle(sc interface{ Scan(...any) error }) (SubtitleFile, error) {
	var f SubtitleFile
	var forced, hi int
	err := sc.Scan(&f.ID, &f.VersionID, &f.Path, &f.Language, &f.Title, &f.Format, &forced, &hi, &f.Source, &f.ProviderID, &f.AddedBy, &f.AddedAt)
	f.Forced, f.HearingImpaired = forced != 0, hi != 0
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
		(version_id, path, language, title, format, forced, hearing_impaired, source, provider_id, added_by, added_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT (version_id, path) DO UPDATE SET language = excluded.language, title = excluded.title,
			format = excluded.format, forced = excluded.forced, hearing_impaired = excluded.hearing_impaired,
			source = excluded.source, provider_id = excluded.provider_id
		RETURNING id`,
		f.VersionID, f.Path, f.Language, f.Title, f.Format, boolInt(f.Forced), boolInt(f.HearingImpaired), f.Source, f.ProviderID, by, f.AddedAt).Scan(&f.ID)
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
