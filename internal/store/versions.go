package store

import (
	"context"
	"database/sql"
	"errors"
)

func scanVersion(r scanner) (Version, error) {
	var v Version
	var missing int
	err := r.Scan(&v.ID, &v.ItemID, &v.LibraryID, &v.Path, &v.RelPath, &v.Part, &v.EpisodeEnd,
		&v.Size, &v.Mtime, &v.Container, &v.DurationSec, &v.Bitrate, &v.Width, &v.Height,
		&v.VideoCodec, &v.HDR, &v.DVProfile, &v.ProbeError, &missing, &v.AddedAt, &v.ScannedAt)
	v.Missing = missing != 0
	return v, err
}

func scanVersions(rows *sql.Rows) ([]Version, error) {
	defer rows.Close()
	var out []Version
	for rows.Next() {
		v, err := scanVersion(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, v)
	}
	return out, rows.Err()
}

func (s *Store) GetVersion(ctx context.Context, id string) (Version, error) {
	v, err := scanVersion(s.db.QueryRowContext(ctx, qGetVersion, id))
	if errors.Is(err, sql.ErrNoRows) {
		return v, ErrNotFound
	}
	return v, err
}

// VersionByPath returns the version for an absolute path, or ErrNotFound.
func (s *Store) VersionByPath(ctx context.Context, path string) (Version, error) {
	v, err := scanVersion(s.db.QueryRowContext(ctx, qGetVersionByPath, path))
	if errors.Is(err, sql.ErrNoRows) {
		return v, ErrNotFound
	}
	return v, err
}

// LibraryVersions loads every file record in a library, keyed by absolute
// path, so a rescan can check thousands of files without a query each.
func (s *Store) LibraryVersions(ctx context.Context, libraryID int64) (map[string]Version, error) {
	rows, err := s.db.QueryContext(ctx, qLibraryVersions, libraryID)
	if err != nil {
		return nil, err
	}
	list, err := scanVersions(rows)
	if err != nil {
		return nil, err
	}
	out := make(map[string]Version, len(list))
	for _, v := range list {
		out[v.Path] = v
	}
	return out, nil
}

// TouchVersions records that unchanged files were seen in this scan, in one
// transaction.
func (s *Store) TouchVersions(ctx context.Context, ids []string) error {
	if len(ids) == 0 {
		return nil
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	t := now()
	const chunk = 500
	for start := 0; start < len(ids); start += chunk {
		part := ids[start:min(start+chunk, len(ids))]
		args := make([]any, 0, len(part)+1)
		args = append(args, t)
		for _, id := range part {
			args = append(args, id)
		}
		q := `UPDATE versions SET scanned_at = ?, missing = 0 WHERE id IN (` + placeholders(len(part)) + `)`
		if _, err := tx.ExecContext(ctx, q, args...); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// ForgetVersions deletes records of files that are still on disk but no
// longer count as library files (extras, samples, Blu-ray disc folders). Any
// item left with no files is removed by DeleteEmptyItems afterwards.
func (s *Store) ForgetVersions(ctx context.Context, ids []string) error {
	if len(ids) == 0 {
		return nil
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	const chunk = 500
	for start := 0; start < len(ids); start += chunk {
		part := ids[start:min(start+chunk, len(ids))]
		args := make([]any, 0, len(part))
		for _, id := range part {
			args = append(args, id)
		}
		q := `DELETE FROM versions WHERE id IN (` + placeholders(len(part)) + `)`
		if _, err := tx.ExecContext(ctx, q, args...); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// SetVersionItem links a file to an item.
func (s *Store) SetVersionItem(ctx context.Context, versionID, itemID string) error {
	_, err := s.db.ExecContext(ctx, qSetVersionItem, itemID, now(), versionID)
	return err
}

// VersionsForItem lists an item's present files, highest resolution first.
func (s *Store) VersionsForItem(ctx context.Context, itemID string) ([]Version, error) {
	rows, err := s.db.QueryContext(ctx, qVersionsForItem, itemID)
	if err != nil {
		return nil, err
	}
	return scanVersions(rows)
}

func (s *Store) ProbeErrors(ctx context.Context, libraryID int64, limit int) ([]Version, error) {
	rows, err := s.db.QueryContext(ctx, qProbeErrors, libraryID, limit)
	if err != nil {
		return nil, err
	}
	return scanVersions(rows)
}

func (s *Store) UnlinkedVersions(ctx context.Context, libraryID int64, limit int) ([]Version, error) {
	rows, err := s.db.QueryContext(ctx, qUnlinkedVersions, libraryID, limit)
	if err != nil {
		return nil, err
	}
	return scanVersions(rows)
}

// SaveVersion inserts or updates a version and replaces its streams.
func (s *Store) SaveVersion(ctx context.Context, v *Version, streams []Stream) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	t := now()
	v.ScannedAt = t
	if v.ID == "" {
		v.ID = NewID()
		v.AddedAt = t
		_, err = tx.ExecContext(ctx, qInsertVersion, v.ID, nullStr(v.ItemID), v.LibraryID, v.Path,
			v.RelPath, v.Part, nullPositive(v.EpisodeEnd), v.Size, v.Mtime, v.Container, v.DurationSec,
			v.Bitrate, v.Width, v.Height, v.VideoCodec, v.HDR, v.DVProfile, v.ProbeError, t, t)
	} else {
		_, err = tx.ExecContext(ctx, qUpdateVersion, nullStr(v.ItemID), v.RelPath, v.Part,
			nullPositive(v.EpisodeEnd), v.Size, v.Mtime, v.Container, v.DurationSec, v.Bitrate,
			v.Width, v.Height, v.VideoCodec, v.HDR, v.DVProfile, v.ProbeError, t, v.ID)
	}
	if err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, qDeleteStreams, v.ID); err != nil {
		return err
	}
	for _, st := range streams {
		if _, err := tx.ExecContext(ctx, qInsertStream, v.ID, st.Index, st.Type, st.Codec,
			st.Profile, st.Language, st.Title, st.Channels, boolInt(st.Default), boolInt(st.Forced)); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// TouchVersion records that an unchanged file was seen in this scan.
func (s *Store) TouchVersion(ctx context.Context, id string) error {
	_, err := s.db.ExecContext(ctx, qTouchVersion, now(), id)
	return err
}

// MarkMissing flags files not seen since scanStart. It returns how many.
func (s *Store) MarkMissing(ctx context.Context, libraryID, scanStart int64) (int64, error) {
	res, err := s.db.ExecContext(ctx, qMarkMissing, libraryID, scanStart)
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}

func (s *Store) Streams(ctx context.Context, versionID string) ([]Stream, error) {
	rows, err := s.db.QueryContext(ctx, qStreamsForVersion, versionID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Stream
	for rows.Next() {
		var st Stream
		var def, forced int
		if err := rows.Scan(&st.Index, &st.Type, &st.Codec, &st.Profile, &st.Language,
			&st.Title, &st.Channels, &def, &forced); err != nil {
			return nil, err
		}
		st.Default, st.Forced = def != 0, forced != 0
		out = append(out, st)
	}
	return out, rows.Err()
}
