package store

import (
	"context"
	"database/sql"
	"errors"
	"path/filepath"
)

// ErrLastFolder is returned when removing a library's only folder. Remove
// the library instead.
var ErrLastFolder = errors.New("a library needs at least one folder")

// CreateLibrary adds a library with one folder. kind is movies, tv or other
// (a plain folder of videos: stored as movies with the plain flag, and read
// back as other).
func (s *Store) CreateLibrary(ctx context.Context, name, path, kind string) (Library, error) {
	t := now()
	dbKind, plain := kind, 0
	if kind == KindOther {
		dbKind, plain = KindMovies, 1
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return Library{}, err
	}
	defer tx.Rollback()
	res, err := tx.ExecContext(ctx, qInsertLibrary, name, path, dbKind, plain, t)
	if err != nil {
		return Library{}, err
	}
	id, err := res.LastInsertId()
	if err != nil {
		return Library{}, err
	}
	if _, err := tx.ExecContext(ctx, qInsertFolder, id, path, t); err != nil {
		return Library{}, err
	}
	if err := tx.Commit(); err != nil {
		return Library{}, err
	}
	return Library{ID: id, Name: name, Path: path, Folders: []string{path}, Kind: kind, CreatedAt: t}, nil
}

func (s *Store) ListLibraries(ctx context.Context) ([]Library, error) {
	rows, err := s.db.QueryContext(ctx, qListLibraries)
	if err != nil {
		return nil, err
	}
	var out []Library
	for rows.Next() {
		var l Library
		if err := rows.Scan(&l.ID, &l.Name, &l.Path, &l.Kind, &l.CreatedAt, &l.LastScanAt); err != nil {
			rows.Close()
			return nil, err
		}
		out = append(out, l)
	}
	if err := rows.Close(); err != nil {
		return nil, err
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}

	frows, err := s.db.QueryContext(ctx, qAllFolders)
	if err != nil {
		return nil, err
	}
	defer frows.Close()
	folders := map[int64][]string{}
	for frows.Next() {
		var id int64
		var p string
		if err := frows.Scan(&id, &p); err != nil {
			return nil, err
		}
		folders[id] = append(folders[id], p)
	}
	if err := frows.Err(); err != nil {
		return nil, err
	}
	for i := range out {
		out[i].Folders = withFirst(folders[out[i].ID], out[i].Path)
	}
	return out, nil
}

func (s *Store) GetLibrary(ctx context.Context, id int64) (Library, error) {
	var l Library
	err := s.db.QueryRowContext(ctx, qGetLibrary, id).
		Scan(&l.ID, &l.Name, &l.Path, &l.Kind, &l.CreatedAt, &l.LastScanAt)
	if errors.Is(err, sql.ErrNoRows) {
		return l, ErrNotFound
	}
	if err != nil {
		return l, err
	}
	folders, err := s.libraryFolders(ctx, s.db, id)
	if err != nil {
		return l, err
	}
	l.Folders = withFirst(folders, l.Path)
	return l, nil
}

// withFirst makes sure a library always lists at least its main folder.
func withFirst(folders []string, path string) []string {
	if len(folders) == 0 && path != "" {
		return []string{path}
	}
	return folders
}

type querier interface {
	QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error)
}

func (s *Store) libraryFolders(ctx context.Context, q querier, id int64) ([]string, error) {
	rows, err := q.QueryContext(ctx, qLibraryFolders, id)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var p string
		if err := rows.Scan(&p); err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

func (s *Store) DeleteLibrary(ctx context.Context, id int64) error {
	// The cascade covers this too; explicit so it holds without foreign keys.
	if _, err := s.db.ExecContext(ctx, qDeleteLibFolders, id); err != nil {
		return err
	}
	_, err := s.db.ExecContext(ctx, qDeleteLibrary, id)
	return err
}

// RenameLibrary changes a library's display name.
func (s *Store) RenameLibrary(ctx context.Context, id int64, name string) error {
	res, err := s.db.ExecContext(ctx, qRenameLibrary, name, id)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

// AddLibraryFolder adds a folder to a library. The caller scans afterwards.
func (s *Store) AddLibraryFolder(ctx context.Context, id int64, path string) error {
	_, err := s.db.ExecContext(ctx, qInsertFolder, id, path, now())
	return err
}

// RemoveLibraryFolder takes a folder out of a library and forgets the files
// found under it (the files themselves are not touched). Titles left with
// no files are removed, except ones with watch history. It returns how many
// file records were dropped.
func (s *Store) RemoveLibraryFolder(ctx context.Context, id int64, path string) (int64, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()
	folders, err := s.libraryFolders(ctx, tx, id)
	if err != nil {
		return 0, err
	}
	found := false
	for _, f := range folders {
		if f == path {
			found = true
			break
		}
	}
	if !found {
		return 0, ErrNotFound
	}
	if len(folders) < 2 {
		return 0, ErrLastFolder
	}
	prefix := path
	if prefix[len(prefix)-1] != filepath.Separator {
		prefix += string(filepath.Separator)
	}
	res, err := tx.ExecContext(ctx, qDeleteFolderVersions, id, prefix, prefix)
	if err != nil {
		return 0, err
	}
	dropped, _ := res.RowsAffected()
	if _, err := tx.ExecContext(ctx, qDeleteFolder, id, path); err != nil {
		return 0, err
	}
	if _, err := tx.ExecContext(ctx, qFirstFolderAsPath, id, id); err != nil {
		return 0, err
	}
	if err := tx.Commit(); err != nil {
		return 0, err
	}
	return dropped, s.DeleteEmptyItems(ctx, id)
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
