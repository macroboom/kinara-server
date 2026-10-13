package store

import (
	"context"
	"database/sql"
	"errors"
)

func (s *Store) CreateLibrary(ctx context.Context, name, path, kind string) (Library, error) {
	t := now()
	res, err := s.db.ExecContext(ctx, qInsertLibrary, name, path, kind, t)
	if err != nil {
		return Library{}, err
	}
	id, err := res.LastInsertId()
	if err != nil {
		return Library{}, err
	}
	return Library{ID: id, Name: name, Path: path, Kind: kind, CreatedAt: t}, nil
}

func (s *Store) ListLibraries(ctx context.Context) ([]Library, error) {
	rows, err := s.db.QueryContext(ctx, qListLibraries)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Library
	for rows.Next() {
		var l Library
		if err := rows.Scan(&l.ID, &l.Name, &l.Path, &l.Kind, &l.CreatedAt, &l.LastScanAt); err != nil {
			return nil, err
		}
		out = append(out, l)
	}
	return out, rows.Err()
}

func (s *Store) GetLibrary(ctx context.Context, id int64) (Library, error) {
	var l Library
	err := s.db.QueryRowContext(ctx, qGetLibrary, id).
		Scan(&l.ID, &l.Name, &l.Path, &l.Kind, &l.CreatedAt, &l.LastScanAt)
	if errors.Is(err, sql.ErrNoRows) {
		return l, ErrNotFound
	}
	return l, err
}

func (s *Store) DeleteLibrary(ctx context.Context, id int64) error {
	_, err := s.db.ExecContext(ctx, qDeleteLibrary, id)
	return err
}

func (s *Store) SetLibraryScanned(ctx context.Context, id int64, at int64) error {
	_, err := s.db.ExecContext(ctx, qSetLibraryScanned, at, id)
	return err
}

func (s *Store) LibraryStats(ctx context.Context, id int64) (LibraryStats, error) {
	var st LibraryStats
	args := make([]any, 9)
	for i := range args {
		args[i] = id
	}
	err := s.db.QueryRowContext(ctx, qLibraryStats, args...).Scan(
		&st.Movies, &st.Shows, &st.Episodes, &st.Unmatched, &st.Pending,
		&st.Files, &st.Missing, &st.ProbeErrors, &st.Unplaced)
	return st, err
}
