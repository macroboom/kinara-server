package store

import (
	"context"
	"database/sql"
	"errors"
	"sort"
	"strconv"
	"strings"
)

// Request statuses.
const (
	RequestPending   = "pending"
	RequestApproved  = "approved"
	RequestDeclined  = "declined"
	RequestAvailable = "available"
)

// Request modes (shows; movies are always RequestModeAll).
const (
	RequestModeAll     = "all"
	RequestModeSeasons = "seasons"
	RequestModePilot   = "pilot"
)

// Request is a title someone asked the owner to add.
type Request struct {
	ID         int64
	Kind       string // movie or show
	TMDBID     int
	TVDBID     int
	IMDBID     string
	Title      string
	Year       int
	PosterPath string
	Mode       string
	Seasons    []int
	Status     string
	Auto       bool
	Note       string
	// DeclineReason is the owner's reason, when declined.
	DeclineReason string
	UserID        int64
	// Username is filled in when listing.
	Username       string
	ClientID       string
	DeviceName     string
	DeviceIcon     string
	DevicePlatform string
	CreatedAt      int64
	DecidedAt      int64
}

// Open reports whether a request still waits for the title: pending or
// approved.
func (r Request) Open() bool {
	return r.Status == RequestPending || r.Status == RequestApproved
}

// SeasonsKey is a request's seasons as stored ("1,2"): sorted, no repeats.
func SeasonsKey(seasons []int) string {
	s := append([]int(nil), seasons...)
	sort.Ints(s)
	parts := make([]string, 0, len(s))
	for i, n := range s {
		if i > 0 && s[i-1] == n {
			continue
		}
		parts = append(parts, strconv.Itoa(n))
	}
	return strings.Join(parts, ",")
}

func parseSeasons(key string) []int {
	out := []int{}
	for _, p := range strings.Split(key, ",") {
		if n, err := strconv.Atoi(p); err == nil {
			out = append(out, n)
		}
	}
	return out
}

const requestCols = `r.id, r.kind, r.tmdb_id, r.tvdb_id, r.imdb_id, r.title, r.year, r.poster_path, r.mode, r.seasons,
	r.status, r.auto, r.note, r.decline_reason, r.user_id, COALESCE(u.username, ''),
	r.client_id, COALESCE(NULLIF(c.name, ''), r.device_name), r.device_icon, r.device_platform, r.created_at, r.decided_at`

// requestFrom joins what a request view needs: the requester's name and the
// device's current name (the owner may have renamed it).
const requestFrom = ` FROM requests r LEFT JOIN users u ON u.id = r.user_id LEFT JOIN clients c ON c.id = r.client_id`

func scanRequest(row scanner) (Request, error) {
	var r Request
	var seasons string
	var auto int
	err := row.Scan(&r.ID, &r.Kind, &r.TMDBID, &r.TVDBID, &r.IMDBID, &r.Title, &r.Year, &r.PosterPath, &r.Mode, &seasons,
		&r.Status, &auto, &r.Note, &r.DeclineReason, &r.UserID, &r.Username,
		&r.ClientID, &r.DeviceName, &r.DeviceIcon, &r.DevicePlatform, &r.CreatedAt, &r.DecidedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return r, ErrNotFound
	}
	r.Auto = auto != 0
	r.Seasons = parseSeasons(seasons)
	return r, err
}

func (s *Store) queryRequests(ctx context.Context, where string, args ...any) ([]Request, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT `+requestCols+requestFrom+` `+where, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Request{}
	for rows.Next() {
		r, err := scanRequest(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// CreateRequest saves a new request, filling in its ID and created time.
func (s *Store) CreateRequest(ctx context.Context, r *Request) error {
	if r.CreatedAt == 0 {
		r.CreatedAt = now()
	}
	if r.Mode == "" {
		r.Mode = RequestModeAll
	}
	res, err := s.db.ExecContext(ctx, `INSERT INTO requests (kind, tmdb_id, tvdb_id, imdb_id, title, year, poster_path, mode, seasons,
		status, auto, note, decline_reason, user_id, client_id, device_name, device_icon, device_platform, created_at, decided_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		r.Kind, r.TMDBID, r.TVDBID, r.IMDBID, r.Title, r.Year, r.PosterPath, r.Mode, SeasonsKey(r.Seasons),
		r.Status, boolInt(r.Auto), r.Note, r.DeclineReason, r.UserID, r.ClientID, r.DeviceName, r.DeviceIcon, r.DevicePlatform,
		r.CreatedAt, r.DecidedAt)
	if err != nil {
		return err
	}
	s.requestsChanged()
	r.ID, err = res.LastInsertId()
	return err
}

// GetRequest returns one request.
func (s *Store) GetRequest(ctx context.Context, id int64) (Request, error) {
	return scanRequest(s.db.QueryRowContext(ctx, `SELECT `+requestCols+requestFrom+` WHERE r.id = ?`, id))
}

// FindOpenRequest finds a pending or approved request for the same title,
// mode and seasons, so asking twice doesn't make two.
func (s *Store) FindOpenRequest(ctx context.Context, kind string, tmdbID int, mode string, seasons []int) (Request, error) {
	list, err := s.queryRequests(ctx, `WHERE r.kind = ? AND r.tmdb_id = ? AND r.mode = ? AND r.seasons = ? AND r.status IN (?, ?)
		ORDER BY r.id LIMIT 1`, kind, tmdbID, mode, SeasonsKey(seasons), RequestPending, RequestApproved)
	if err != nil {
		return Request{}, err
	}
	if len(list) == 0 {
		return Request{}, ErrNotFound
	}
	return list[0], nil
}

// RequestFilter narrows ListRequests. Zero values mean no limit.
type RequestFilter struct {
	UserID int64
	Status string
	Limit  int
	Offset int
}

func (f RequestFilter) where() (string, []any) {
	conds := []string{"1 = 1"}
	args := []any{}
	if f.UserID != 0 {
		conds = append(conds, "r.user_id = ?")
		args = append(args, f.UserID)
	}
	if f.Status != "" {
		conds = append(conds, "r.status = ?")
		args = append(args, f.Status)
	}
	return "WHERE " + strings.Join(conds, " AND "), args
}

// ListRequests lists requests, newest first.
func (s *Store) ListRequests(ctx context.Context, f RequestFilter) ([]Request, error) {
	where, args := f.where()
	limit := f.Limit
	if limit <= 0 {
		limit = -1 // SQLite's "no limit"
	}
	args = append(args, limit, max(f.Offset, 0))
	return s.queryRequests(ctx, where+` ORDER BY r.created_at DESC, r.id DESC LIMIT ? OFFSET ?`, args...)
}

// CountRequests counts requests matching f (Limit and Offset are ignored).
func (s *Store) CountRequests(ctx context.Context, f RequestFilter) (int, error) {
	where, args := f.where()
	var n int
	err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM requests r `+where, args...).Scan(&n)
	return n, err
}

// OpenRequests lists every pending or approved request, oldest first.
func (s *Store) OpenRequests(ctx context.Context) ([]Request, error) {
	return s.queryRequests(ctx, `WHERE r.status IN (?, ?) ORDER BY r.id`, RequestPending, RequestApproved)
}

// OpenRequestsFor lists open requests for some titles of one kind, oldest
// first, for marking titles on discovery pages.
func (s *Store) OpenRequestsFor(ctx context.Context, kind string, tmdbIDs []int) ([]Request, error) {
	if len(tmdbIDs) == 0 {
		return []Request{}, nil
	}
	args := []any{kind, RequestPending, RequestApproved}
	for _, id := range tmdbIDs {
		args = append(args, id)
	}
	return s.queryRequests(ctx, `WHERE r.kind = ? AND r.status IN (?, ?) AND r.tmdb_id IN (`+placeholders(len(tmdbIDs))+`)
		ORDER BY r.id`, args...)
}

// SetRequestStatus moves a request to a new status. With from set, it only
// changes a request that is in one of those statuses, and reports
// ErrNotFound otherwise (so two owners deciding at once can't both win).
func (s *Store) SetRequestStatus(ctx context.Context, id int64, status, reason string, from ...string) error {
	q := `UPDATE requests SET status = ?, decline_reason = ?, decided_at = ? WHERE id = ?`
	args := []any{status, reason, now(), id}
	if status == RequestAvailable {
		// Becoming available isn't a decision: keep when it was decided.
		q = `UPDATE requests SET status = ? WHERE id = ?`
		args = []any{status, id}
	}
	if len(from) > 0 {
		q += ` AND status IN (` + placeholders(len(from)) + `)`
		for _, f := range from {
			args = append(args, f)
		}
	}
	res, err := s.db.ExecContext(ctx, q, args...)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	s.requestsChanged()
	return nil
}

// DeleteRequest removes a request.
func (s *Store) DeleteRequest(ctx context.Context, id int64) error {
	res, err := s.db.ExecContext(ctx, `DELETE FROM requests WHERE id = ?`, id)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	s.requestsChanged()
	return nil
}
