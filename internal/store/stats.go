package store

import (
	"context"
	"strings"
)

// Play is one time a device played something.
type Play struct {
	ID         int64   `json:"id"`
	StartedAt  int64   `json:"startedAt"`
	UpdatedAt  int64   `json:"updatedAt"`
	UserID     int64   `json:"userId"`
	Username   string  `json:"username"`
	Device     string  `json:"device"`
	DeviceName string  `json:"deviceName"`
	ItemID     string  `json:"itemId"`
	VersionID  string  `json:"versionId"`
	Title      string  `json:"title"`
	Kind       string  `json:"kind"`
	Method     string  `json:"method"`
	WatchedSec float64 `json:"watchedSec"`
}

// methodRank orders how a play was delivered: one that was converted at
// any point counts as converted.
var methodRank = map[string]int{"direct": 0, "copy": 1, "convert": 2}

// StartPlay records the start of a play and returns its ID.
func (s *Store) StartPlay(ctx context.Context, p Play) (int64, error) {
	res, err := s.db.ExecContext(ctx, `INSERT INTO play_log (started_at, updated_at, user_id, username, device, device_name,
		item_id, version_id, title, kind, method, watched_sec) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, 0)`,
		p.StartedAt, p.StartedAt, p.UserID, p.Username, p.Device, p.DeviceName, p.ItemID, p.VersionID, p.Title, p.Kind, p.Method)
	if err != nil {
		return 0, err
	}
	return res.LastInsertId()
}

// AddPlayTime adds watched seconds to a play, and raises its method if it
// moved to a heavier one (a step down starts converting).
func (s *Store) AddPlayTime(ctx context.Context, id int64, sec float64, method string, at int64) error {
	_, err := s.db.ExecContext(ctx, `UPDATE play_log SET watched_sec = watched_sec + ?, updated_at = ?,
		method = CASE WHEN ? > (CASE method WHEN 'convert' THEN 2 WHEN 'copy' THEN 1 ELSE 0 END) THEN ? ELSE method END
		WHERE id = ?`, sec, at, methodRank[method], method, id)
	return err
}

// PlaysSince lists plays started at or after a time, oldest first.
func (s *Store) PlaysSince(ctx context.Context, since int64) ([]Play, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT id, started_at, updated_at, user_id, username, device, device_name,
		item_id, version_id, title, kind, method, watched_sec FROM play_log WHERE started_at >= ? ORDER BY started_at`, since)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Play
	for rows.Next() {
		var p Play
		if err := rows.Scan(&p.ID, &p.StartedAt, &p.UpdatedAt, &p.UserID, &p.Username, &p.Device, &p.DeviceName,
			&p.ItemID, &p.VersionID, &p.Title, &p.Kind, &p.Method, &p.WatchedSec); err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

// LibrarySize is what a library holds on disk.
type LibrarySize struct {
	LibraryID int64   `json:"libraryId"`
	Files     int     `json:"files"`
	Bytes     int64   `json:"bytes"`
	Hours     float64 `json:"hours"`
}

// LibrarySizes sums the files present in each library.
func (s *Store) LibrarySizes(ctx context.Context) ([]LibrarySize, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT library_id, COUNT(*), COALESCE(SUM(size), 0), COALESCE(SUM(duration_sec), 0)
		FROM versions WHERE missing = 0 GROUP BY library_id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []LibrarySize
	for rows.Next() {
		var l LibrarySize
		var sec float64
		if err := rows.Scan(&l.LibraryID, &l.Files, &l.Bytes, &sec); err != nil {
			return nil, err
		}
		l.Hours = sec / 3600
		out = append(out, l)
	}
	return out, rows.Err()
}

// Share is one slice of a breakdown: how many files, and their size.
type Share struct {
	Label string `json:"label"`
	Files int    `json:"files"`
	Bytes int64  `json:"bytes"`
}

// Composition breaks the files present down by picture, format and sound.
type Composition struct {
	Resolution []Share `json:"resolution"`
	Codec      []Share `json:"codec"`
	HDR        []Share `json:"hdr"`
	Container  []Share `json:"container"`
	Audio      []Share `json:"audio"`
}

var codecNames = map[string]string{
	"hevc": "HEVC", "h264": "H.264", "av1": "AV1", "vp9": "VP9", "mpeg2video": "MPEG-2", "mpeg4": "MPEG-4",
	"vc1": "VC-1", "vp8": "VP8",
}

var audioNames = map[string]string{
	"truehd": "TrueHD", "eac3": "Dolby Digital+", "ac3": "Dolby Digital", "dts": "DTS", "aac": "AAC",
	"flac": "FLAC", "opus": "Opus", "mp3": "MP3", "pcm_s16le": "PCM", "pcm_s24le": "PCM",
}

// LibraryComposition breaks down every file present.
func (s *Store) LibraryComposition(ctx context.Context) (Composition, error) {
	var c Composition
	group := func(expr string, label func(string) string) ([]Share, error) {
		rows, err := s.db.QueryContext(ctx, `SELECT `+expr+`, COUNT(*), COALESCE(SUM(size), 0) FROM versions
			WHERE missing = 0 AND probe_error = '' GROUP BY 1`)
		if err != nil {
			return nil, err
		}
		defer rows.Close()
		by := map[string]*Share{}
		var order []string
		for rows.Next() {
			var k string
			var n int
			var b int64
			if err := rows.Scan(&k, &n, &b); err != nil {
				return nil, err
			}
			l := label(k)
			if by[l] == nil {
				by[l] = &Share{Label: l}
				order = append(order, l)
			}
			by[l].Files += n
			by[l].Bytes += b
		}
		out := make([]Share, 0, len(order))
		for _, l := range order {
			out = append(out, *by[l])
		}
		return out, rows.Err()
	}
	var err error
	if c.Resolution, err = group(`CASE WHEN height >= 1800 OR width >= 3200 THEN '4K' WHEN height >= 1000 OR width >= 1900 THEN '1080p'
		WHEN height >= 700 THEN '720p' WHEN height > 0 THEN 'SD' ELSE 'Unknown' END`, func(k string) string { return k }); err != nil {
		return c, err
	}
	if c.Codec, err = group(`video_codec`, func(k string) string {
		if n := codecNames[k]; n != "" {
			return n
		}
		if k == "" {
			return "Unknown"
		}
		return strings.ToUpper(k)
	}); err != nil {
		return c, err
	}
	if c.HDR, err = group(`CASE WHEN hdr = 'dolby_vision' THEN 'Dolby Vision' || CASE WHEN dv_profile > 0 THEN ' ' || dv_profile ELSE '' END
		WHEN hdr = 'hdr10' THEN 'HDR10' WHEN hdr = 'hlg' THEN 'HLG' ELSE 'SDR' END`, func(k string) string { return k }); err != nil {
		return c, err
	}
	if c.Container, err = group(`container`, func(k string) string {
		switch {
		case k == "":
			return "Unknown"
		case strings.Contains(k, "matroska"):
			return "MKV"
		case strings.Contains(k, "mp4"), strings.Contains(k, "mov"):
			return "MP4"
		}
		return strings.ToUpper(k)
	}); err != nil {
		return c, err
	}
	// Sound: each file's main track (its default, else its first).
	if c.Audio, err = group(`COALESCE((SELECT codec || CASE WHEN profile LIKE '%Atmos%' THEN ' Atmos' WHEN profile LIKE '%MA%' AND codec = 'dts' THEN '-HD MA' ELSE '' END
		FROM streams st WHERE st.version_id = versions.id AND st.type = 'audio' ORDER BY st.is_default DESC, st.idx LIMIT 1), '')`,
		func(k string) string {
			if k == "" {
				return "No sound"
			}
			base, extra, _ := strings.Cut(k, " ")
			if strings.HasPrefix(k, "dts-HD") {
				return "DTS-HD MA"
			}
			name := audioNames[base]
			if name == "" {
				name = strings.ToUpper(base)
			}
			if extra != "" {
				return name + " " + extra
			}
			return name
		}); err != nil {
		return c, err
	}
	return c, nil
}
