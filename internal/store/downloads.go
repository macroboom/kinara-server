package store

import (
	"context"
	"database/sql"
	"errors"
	"strconv"
)

// DownloadFile is a copy the server prepares for downloads.
type DownloadFile struct {
	ID          int64   `json:"id"`
	VersionID   string  `json:"versionId"`
	Quality     string  `json:"quality"`
	Audio       int     `json:"audio"`
	State       string  `json:"state"`
	Progress    float64 `json:"progress"`
	Path        string  `json:"-"`
	Size        int64   `json:"size"`
	Estimate    int64   `json:"estimate"`
	Error       string  `json:"error,omitempty"`
	SourceMtime int64   `json:"-"`
	CreatedAt   int64   `json:"createdAt"`
	ReadyAt     int64   `json:"readyAt,omitempty"`
	UsedAt      int64   `json:"usedAt,omitempty"`
}

// Download file states.
const (
	DownloadQueued    = "queued"
	DownloadPreparing = "preparing"
	DownloadReady     = "ready"
	DownloadFailed    = "failed"
	DownloadExpired   = "expired"
)

// Download is one person's download of a title on one device.
type Download struct {
	ID         int64  `json:"id"`
	UserID     int64  `json:"-"`
	Username   string `json:"username,omitempty"`
	ProfileID  int64  `json:"-"`
	Device     string `json:"deviceId"`
	DeviceName string `json:"deviceName"`
	ItemID     string `json:"itemId"`
	FileID     int64  `json:"-"`
	CreatedAt  int64  `json:"createdAt"`
	FetchedAt  int64  `json:"fetchedAt,omitempty"`
	SavedAt    int64  `json:"savedAt,omitempty"`
	// File is the copy it's made from.
	File DownloadFile `json:"file"`
}

const downloadFileCols = `f.id, f.version_id, f.quality, f.audio, f.state, f.progress, f.path, f.size, f.estimate,
	f.error, f.source_mtime, f.created_at, f.ready_at, f.used_at`

func scanDownloadFile(sc interface{ Scan(...any) error }, extra ...any) (DownloadFile, error) {
	var f DownloadFile
	dest := append([]any{&f.ID, &f.VersionID, &f.Quality, &f.Audio, &f.State, &f.Progress, &f.Path, &f.Size,
		&f.Estimate, &f.Error, &f.SourceMtime, &f.CreatedAt, &f.ReadyAt, &f.UsedAt}, extra...)
	err := sc.Scan(dest...)
	return f, err
}

// EnsureDownloadFile finds the copy for a version, quality and audio
// track, adding f when there's none yet (created reports which).
func (s *Store) EnsureDownloadFile(ctx context.Context, f DownloadFile) (out DownloadFile, created bool, err error) {
	if f.CreatedAt == 0 {
		f.CreatedAt = now()
	}
	res, err := s.db.ExecContext(ctx, `INSERT INTO download_files
		(version_id, quality, audio, state, progress, path, size, estimate, source_mtime, created_at, ready_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT (version_id, quality, audio) DO NOTHING`,
		f.VersionID, f.Quality, f.Audio, f.State, f.Progress, f.Path, f.Size, f.Estimate, f.SourceMtime, f.CreatedAt, f.ReadyAt)
	if err != nil {
		return f, false, err
	}
	n, _ := res.RowsAffected()
	out, err = scanDownloadFile(s.db.QueryRowContext(ctx, `SELECT `+downloadFileCols+` FROM download_files f
		WHERE f.version_id = ? AND f.quality = ? AND f.audio = ?`, f.VersionID, f.Quality, f.Audio))
	return out, n > 0, err
}

// GetDownloadFile reads one copy.
func (s *Store) GetDownloadFile(ctx context.Context, id int64) (DownloadFile, error) {
	f, err := scanDownloadFile(s.db.QueryRowContext(ctx, `SELECT `+downloadFileCols+` FROM download_files f WHERE f.id = ?`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return f, ErrNotFound
	}
	return f, err
}

// UpdateDownloadFile saves a copy's state, progress, file and error.
func (s *Store) UpdateDownloadFile(ctx context.Context, f DownloadFile) error {
	_, err := s.db.ExecContext(ctx, `UPDATE download_files SET state = ?, progress = ?, path = ?, size = ?,
		estimate = ?, error = ?, source_mtime = ?, ready_at = ? WHERE id = ?`,
		f.State, f.Progress, f.Path, f.Size, f.Estimate, f.Error, f.SourceMtime, f.ReadyAt, f.ID)
	return err
}

// SetDownloadProgress saves how far preparing a copy has got.
func (s *Store) SetDownloadProgress(ctx context.Context, id int64, progress float64) error {
	_, err := s.db.ExecContext(ctx, `UPDATE download_files SET progress = ? WHERE id = ? AND state = ?`, progress, id, DownloadPreparing)
	return err
}

// TouchDownloadFile notes that a device fetched from a copy.
func (s *Store) TouchDownloadFile(ctx context.Context, id int64) error {
	_, err := s.db.ExecContext(ctx, `UPDATE download_files SET used_at = ? WHERE id = ?`, now(), id)
	return err
}

// NextDownloadFile is the oldest queued copy someone still wants.
func (s *Store) NextDownloadFile(ctx context.Context) (DownloadFile, error) {
	f, err := scanDownloadFile(s.db.QueryRowContext(ctx, `SELECT `+downloadFileCols+` FROM download_files f
		WHERE f.state = ? AND EXISTS (SELECT 1 FROM downloads d WHERE d.file_id = f.id)
		ORDER BY (SELECT MIN(d.created_at) FROM downloads d WHERE d.file_id = f.id), f.id LIMIT 1`, DownloadQueued))
	if errors.Is(err, sql.ErrNoRows) {
		return f, ErrNotFound
	}
	return f, err
}

// QueuedBefore counts the queued copies ahead of one (its place in line).
func (s *Store) QueuedBefore(ctx context.Context) (map[int64]int, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT f.id FROM download_files f
		WHERE f.state = ? AND EXISTS (SELECT 1 FROM downloads d WHERE d.file_id = f.id)
		ORDER BY (SELECT MIN(d.created_at) FROM downloads d WHERE d.file_id = f.id), f.id`, DownloadQueued)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[int64]int{}
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		out[id] = len(out)
	}
	return out, rows.Err()
}

// RequeueInterrupted puts copies that were being prepared when the server
// stopped back in line.
func (s *Store) RequeueInterrupted(ctx context.Context) error {
	_, err := s.db.ExecContext(ctx, `UPDATE download_files SET state = ?, progress = 0 WHERE state = ?`, DownloadQueued, DownloadPreparing)
	return err
}

// DownloadFileUse is a copy with how its downloads stand, for cleaning up.
type DownloadFileUse struct {
	DownloadFile
	// Downloads is how many downloads use it; Unsaved how many of those
	// devices don't have all of it yet.
	Downloads int
	Unsaved   int
	// LastSaved is when the latest device finished saving it.
	LastSaved int64
}

// DownloadFileUses lists every copy with how it's used.
func (s *Store) DownloadFileUses(ctx context.Context) ([]DownloadFileUse, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT `+downloadFileCols+`,
		(SELECT COUNT(*) FROM downloads d WHERE d.file_id = f.id),
		(SELECT COUNT(*) FROM downloads d WHERE d.file_id = f.id AND d.saved_at = 0),
		COALESCE((SELECT MAX(d.saved_at) FROM downloads d WHERE d.file_id = f.id), 0)
		FROM download_files f ORDER BY f.id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []DownloadFileUse
	for rows.Next() {
		var u DownloadFileUse
		f, err := scanDownloadFile(rows, &u.Downloads, &u.Unsaved, &u.LastSaved)
		if err != nil {
			return nil, err
		}
		u.DownloadFile = f
		out = append(out, u)
	}
	return out, rows.Err()
}

// DeleteDownloadFile forgets a copy and the downloads made from it (the
// caller removes the file).
func (s *Store) DeleteDownloadFile(ctx context.Context, id int64) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM download_files WHERE id = ?`, id)
	return err
}

// PutDownload adds a download, or points the device's existing download
// of the same title at a new copy (another quality).
func (s *Store) PutDownload(ctx context.Context, d *Download) error {
	if d.CreatedAt == 0 {
		d.CreatedAt = now()
	}
	return s.db.QueryRowContext(ctx, `INSERT INTO downloads
		(user_id, profile_id, device, device_name, item_id, file_id, created_at)
		VALUES (?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT (user_id, device, item_id) DO UPDATE SET
			profile_id = excluded.profile_id, device_name = excluded.device_name,
			saved_at = CASE WHEN file_id = excluded.file_id THEN saved_at ELSE 0 END,
			file_id = excluded.file_id
		RETURNING id, created_at`,
		d.UserID, d.ProfileID, d.Device, d.DeviceName, d.ItemID, d.FileID, d.CreatedAt).Scan(&d.ID, &d.CreatedAt)
}

const downloadSelect = `SELECT d.id, d.user_id, COALESCE(u.username, ''), d.profile_id, d.device, d.device_name, d.item_id,
	d.file_id, d.created_at, d.fetched_at, d.saved_at, ` + downloadFileCols + `
	FROM downloads d JOIN download_files f ON f.id = d.file_id LEFT JOIN users u ON u.id = d.user_id`

func scanDownload(sc interface{ Scan(...any) error }) (Download, error) {
	var d Download
	f, err := scanDownloadFile(&prefixScanner{sc: sc, pre: []any{&d.ID, &d.UserID, &d.Username, &d.ProfileID, &d.Device,
		&d.DeviceName, &d.ItemID, &d.FileID, &d.CreatedAt, &d.FetchedAt, &d.SavedAt}})
	d.File = f
	return d, err
}

// prefixScanner scans some leading columns before the ones asked for.
type prefixScanner struct {
	sc  interface{ Scan(...any) error }
	pre []any
}

func (p *prefixScanner) Scan(dest ...any) error { return p.sc.Scan(append(p.pre, dest...)...) }

func (s *Store) queryDownloads(ctx context.Context, where string, args ...any) ([]Download, error) {
	rows, err := s.db.QueryContext(ctx, downloadSelect+` `+where+` ORDER BY d.created_at DESC, d.id DESC`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Download{}
	for rows.Next() {
		d, err := scanDownload(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, d)
	}
	return out, rows.Err()
}

// UserDownloads lists an account's downloads on all its devices, newest first.
func (s *Store) UserDownloads(ctx context.Context, userID int64) ([]Download, error) {
	return s.queryDownloads(ctx, `WHERE d.user_id = ?`, userID)
}

// AllDownloads lists everyone's downloads, newest first.
func (s *Store) AllDownloads(ctx context.Context) ([]Download, error) {
	return s.queryDownloads(ctx, ``)
}

// GetDownload reads one download.
func (s *Store) GetDownload(ctx context.Context, id int64) (Download, error) {
	d, err := scanDownload(s.db.QueryRowContext(ctx, downloadSelect+` WHERE d.id = ?`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return d, ErrNotFound
	}
	return d, err
}

// DeleteDownload removes a download.
func (s *Store) DeleteDownload(ctx context.Context, id int64) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM downloads WHERE id = ?`, id)
	return err
}

// MarkDownloadFetched notes that the device is fetching the file.
func (s *Store) MarkDownloadFetched(ctx context.Context, id int64) error {
	_, err := s.db.ExecContext(ctx, `UPDATE downloads SET fetched_at = ? WHERE id = ?`, now(), id)
	return err
}

// MarkDownloadSaved notes that the device has all of the file.
func (s *Store) MarkDownloadSaved(ctx context.Context, id int64) error {
	_, err := s.db.ExecContext(ctx, `UPDATE downloads SET saved_at = ? WHERE id = ? AND saved_at = 0`, now(), id)
	return err
}

// DownloadSettings are the owner's choices for downloads.
type DownloadSettings struct {
	// Enabled lets apps download titles to watch offline.
	Enabled bool `json:"enabled"`
	// MaxGB caps the room prepared copies take on the server.
	MaxGB int `json:"maxGB"`
	// PerUser caps how many downloads each account can have (0: no limit).
	PerUser int `json:"perUser"`
}

// DefaultDownloadSettings apply until the owner changes something.
var DefaultDownloadSettings = DownloadSettings{Enabled: true, MaxGB: 100, PerUser: 200}

// GetDownloadSettings reads the download settings.
func (s *Store) GetDownloadSettings(ctx context.Context) (DownloadSettings, error) {
	out := DefaultDownloadSettings
	var err error
	if out.Enabled, err = s.boolSetting(ctx, "downloads_enabled", out.Enabled); err != nil {
		return out, err
	}
	if out.MaxGB, err = s.intSetting(ctx, "downloads_max_gb", out.MaxGB); err != nil {
		return out, err
	}
	out.PerUser, err = s.intSetting(ctx, "downloads_per_user", out.PerUser)
	return out, err
}

// PutDownloadSettings saves the download settings.
func (s *Store) PutDownloadSettings(ctx context.Context, d DownloadSettings) error {
	if err := s.setSetting(ctx, "downloads_enabled", strconv.FormatBool(d.Enabled)); err != nil {
		return err
	}
	if err := s.setSetting(ctx, "downloads_max_gb", strconv.Itoa(d.MaxGB)); err != nil {
		return err
	}
	return s.setSetting(ctx, "downloads_per_user", strconv.Itoa(d.PerUser))
}

func (s *Store) intSetting(ctx context.Context, key string, def int) (int, error) {
	v, ok, err := s.setting(ctx, key)
	if err != nil || !ok {
		return def, err
	}
	n, err := strconv.Atoi(v)
	if err != nil {
		return def, nil
	}
	return n, nil
}
