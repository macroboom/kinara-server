package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"strings"
)

// Incoming hooks: addresses the owner's tools post title statuses to, what
// each was sent, and the statuses themselves ("on the way").

// StateMapping turns one of a tool's own state words into Kinora's.
type StateMapping struct {
	From string `json:"from"`
	To   string `json:"to"`
}

// IncomingHook is an address a tool posts status updates to.
type IncomingHook struct {
	ID      int64
	Name    string
	Token   string
	Enabled bool
	// Format is "kinora" or "custom" (read with Mapping and StateMap).
	Format         string
	Mapping        map[string]string
	StateMap       []StateMapping
	ProgressScale  string
	OnDone         string
	ExpireHours    int
	Visibility     string
	LastReceivedAt int64
	CreatedAt      int64
}

// Received is one update an incoming hook was sent.
type Received struct {
	ID      int64  `json:"id"`
	At      int64  `json:"at"`
	OK      bool   `json:"ok"`
	Summary string `json:"summary"`
	Body    string `json:"body"`
}

// TitleStatus is where a title on its way is, from the newest update about
// it. Season and Episode are -1 when not given; Progress is nil when not
// given.
type TitleStatus struct {
	ID         int64
	Source     string
	SourceName string
	HookID     int64
	Ident      string
	Kind       string
	TMDBID     int
	Title      string
	Year       int
	Season     int
	Episode    int
	State      string
	Progress   *float64
	Message    string
	EtaSec     int
	ItemID     string
	Visibility string
	UpdatedAt  int64
	ExpiresAt  int64
}

// keepReceived is how many updates each hook's log keeps.
const keepReceived = 200

const incomingCols = `id, name, token, enabled, format, mapping, state_map, progress_scale, on_done, expire_hours, visibility, last_received_at, created_at`

func scanIncoming(r scanner) (IncomingHook, error) {
	var h IncomingHook
	var mapping, stateMap string
	err := r.Scan(&h.ID, &h.Name, &h.Token, &h.Enabled, &h.Format, &mapping, &stateMap, &h.ProgressScale,
		&h.OnDone, &h.ExpireHours, &h.Visibility, &h.LastReceivedAt, &h.CreatedAt)
	if err != nil {
		return h, err
	}
	_ = json.Unmarshal([]byte(mapping), &h.Mapping)
	_ = json.Unmarshal([]byte(stateMap), &h.StateMap)
	if h.Mapping == nil {
		h.Mapping = map[string]string{}
	}
	if h.StateMap == nil {
		h.StateMap = []StateMapping{}
	}
	return h, nil
}

// ListIncoming lists incoming hooks, oldest first.
func (s *Store) ListIncoming(ctx context.Context) ([]IncomingHook, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT `+incomingCols+` FROM integ_incoming ORDER BY id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []IncomingHook{}
	for rows.Next() {
		h, err := scanIncoming(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, h)
	}
	return out, rows.Err()
}

// GetIncoming returns one incoming hook, or ErrNotFound.
func (s *Store) GetIncoming(ctx context.Context, id int64) (IncomingHook, error) {
	h, err := scanIncoming(s.db.QueryRowContext(ctx, `SELECT `+incomingCols+` FROM integ_incoming WHERE id = ?`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return h, ErrNotFound
	}
	return h, err
}

// IncomingByToken finds the hook an address belongs to, or ErrNotFound.
func (s *Store) IncomingByToken(ctx context.Context, token string) (IncomingHook, error) {
	h, err := scanIncoming(s.db.QueryRowContext(ctx, `SELECT `+incomingCols+` FROM integ_incoming WHERE token = ?`, token))
	if errors.Is(err, sql.ErrNoRows) {
		return h, ErrNotFound
	}
	return h, err
}

// CreateIncoming saves a new incoming hook, filling in its ID.
func (s *Store) CreateIncoming(ctx context.Context, h *IncomingHook) error {
	h.CreatedAt = now()
	res, err := s.db.ExecContext(ctx, `INSERT INTO integ_incoming
		(name, token, enabled, format, mapping, state_map, progress_scale, on_done, expire_hours, visibility, created_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		h.Name, h.Token, boolInt(h.Enabled), h.Format, jsonText(h.Mapping), jsonText(h.StateMap), h.ProgressScale,
		h.OnDone, h.ExpireHours, h.Visibility, h.CreatedAt)
	if err != nil {
		return err
	}
	h.ID, err = res.LastInsertId()
	return err
}

// UpdateIncoming saves a changed incoming hook (not its token: see
// SetIncomingToken).
func (s *Store) UpdateIncoming(ctx context.Context, h IncomingHook) error {
	res, err := s.db.ExecContext(ctx, `UPDATE integ_incoming SET name = ?, enabled = ?, format = ?, mapping = ?,
		state_map = ?, progress_scale = ?, on_done = ?, expire_hours = ?, visibility = ? WHERE id = ?`,
		h.Name, boolInt(h.Enabled), h.Format, jsonText(h.Mapping), jsonText(h.StateMap), h.ProgressScale,
		h.OnDone, h.ExpireHours, h.Visibility, h.ID)
	if err := mustAffect(res, err); err != nil {
		return err
	}
	// Statuses it reported follow its visibility.
	_, err = s.db.ExecContext(ctx, `UPDATE integ_status SET visibility = ? WHERE hook_id = ?`, h.Visibility, h.ID)
	return err
}

// SetIncomingToken gives a hook a new address; the old one stops working.
func (s *Store) SetIncomingToken(ctx context.Context, id int64, token string) error {
	res, err := s.db.ExecContext(ctx, `UPDATE integ_incoming SET token = ? WHERE id = ?`, token, id)
	return mustAffect(res, err)
}

// DeleteIncoming removes a hook, its log and the statuses it reported.
func (s *Store) DeleteIncoming(ctx context.Context, id int64) error {
	res, err := s.db.ExecContext(ctx, `DELETE FROM integ_incoming WHERE id = ?`, id)
	return mustAffect(res, err)
}

// AddReceived logs an update a hook was sent, keeping the newest 200.
func (s *Store) AddReceived(ctx context.Context, hookID int64, r Received) error {
	if r.At == 0 {
		r.At = now()
	}
	if _, err := s.db.ExecContext(ctx, `INSERT INTO integ_received (hook_id, at, ok, summary, body) VALUES (?, ?, ?, ?, ?)`,
		hookID, r.At, boolInt(r.OK), r.Summary, r.Body); err != nil {
		return err
	}
	if _, err := s.db.ExecContext(ctx, `UPDATE integ_incoming SET last_received_at = ? WHERE id = ?`, r.At, hookID); err != nil {
		return err
	}
	_, err := s.db.ExecContext(ctx, `DELETE FROM integ_received WHERE hook_id = ? AND id NOT IN
		(SELECT id FROM integ_received WHERE hook_id = ? ORDER BY id DESC LIMIT ?)`, hookID, hookID, keepReceived)
	return err
}

// ReceivedFor lists what a hook was sent, newest first.
func (s *Store) ReceivedFor(ctx context.Context, hookID int64, limit int) ([]Received, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT id, at, ok, summary, body FROM integ_received
		WHERE hook_id = ? ORDER BY id DESC LIMIT ?`, hookID, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Received{}
	for rows.Next() {
		var r Received
		if err := rows.Scan(&r.ID, &r.At, &r.OK, &r.Summary, &r.Body); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

const statusCols = `id, source, source_name, COALESCE(hook_id, 0), ident, kind, tmdb_id, title, year, season, episode,
	state, progress, message, eta_sec, item_id, visibility, updated_at, expires_at`

func scanStatus(r scanner) (TitleStatus, error) {
	var st TitleStatus
	var progress sql.NullFloat64
	err := r.Scan(&st.ID, &st.Source, &st.SourceName, &st.HookID, &st.Ident, &st.Kind, &st.TMDBID, &st.Title,
		&st.Year, &st.Season, &st.Episode, &st.State, &progress, &st.Message, &st.EtaSec, &st.ItemID,
		&st.Visibility, &st.UpdatedAt, &st.ExpiresAt)
	if progress.Valid {
		p := progress.Float64
		st.Progress = &p
	}
	return st, err
}

// GetStatusFor returns the status a source last sent about ident, or
// ErrNotFound.
func (s *Store) GetStatusFor(ctx context.Context, source, ident string) (TitleStatus, error) {
	st, err := scanStatus(s.db.QueryRowContext(ctx, `SELECT `+statusCols+` FROM integ_status
		WHERE source = ? AND ident = ? AND expires_at > ?`, source, ident, now()))
	if errors.Is(err, sql.ErrNoRows) {
		return st, ErrNotFound
	}
	return st, err
}

// PutStatus saves a status, replacing the source's last one for the same
// ident. It fills in the ID.
func (s *Store) PutStatus(ctx context.Context, st *TitleStatus) error {
	var hook any
	if st.HookID > 0 {
		hook = st.HookID
	}
	var progress any
	if st.Progress != nil {
		progress = *st.Progress
	}
	err := s.db.QueryRowContext(ctx, `INSERT INTO integ_status
		(source, source_name, hook_id, ident, kind, tmdb_id, title, year, season, episode, state, progress, message,
		 eta_sec, item_id, visibility, updated_at, expires_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT (source, ident) DO UPDATE SET source_name = excluded.source_name, kind = excluded.kind,
		 tmdb_id = excluded.tmdb_id, title = excluded.title, year = excluded.year, season = excluded.season,
		 episode = excluded.episode, state = excluded.state, progress = excluded.progress, message = excluded.message,
		 eta_sec = excluded.eta_sec, item_id = excluded.item_id, visibility = excluded.visibility,
		 updated_at = excluded.updated_at, expires_at = excluded.expires_at
		RETURNING id`,
		st.Source, st.SourceName, hook, st.Ident, st.Kind, st.TMDBID, st.Title, st.Year, st.Season, st.Episode,
		st.State, progress, st.Message, st.EtaSec, st.ItemID, st.Visibility, st.UpdatedAt, st.ExpiresAt).Scan(&st.ID)
	if err == nil {
		s.requestsChanged()
	}
	return err
}

// DeleteStatusFor clears what a source said about ident.
func (s *Store) DeleteStatusFor(ctx context.Context, source, ident string) error {
	res, err := s.db.ExecContext(ctx, `DELETE FROM integ_status WHERE source = ? AND ident = ?`, source, ident)
	s.statusesGone(res, err)
	return err
}

// DeleteStatus clears one status by ID.
func (s *Store) DeleteStatus(ctx context.Context, id int64) error {
	res, err := s.db.ExecContext(ctx, `DELETE FROM integ_status WHERE id = ?`, id)
	s.statusesGone(res, err)
	return mustAffect(res, err)
}

// Statuses lists statuses that haven't expired, newest first. With
// ownersToo false, only those everyone may see.
func (s *Store) Statuses(ctx context.Context, ownersToo bool) ([]TitleStatus, error) {
	q := `SELECT ` + statusCols + ` FROM integ_status WHERE expires_at > ?`
	if !ownersToo {
		q += ` AND visibility = 'everyone'`
	}
	rows, err := s.db.QueryContext(ctx, q+` ORDER BY updated_at DESC, id DESC`, now())
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []TitleStatus{}
	for rows.Next() {
		st, err := scanStatus(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, st)
	}
	return out, rows.Err()
}

// StatusForTitle is the newest unexpired status for a title.
func (s *Store) StatusForTitle(ctx context.Context, kind string, tmdbID int, ownersToo bool) (TitleStatus, error) {
	q := `SELECT ` + statusCols + ` FROM integ_status WHERE kind = ? AND tmdb_id = ? AND expires_at > ?`
	if !ownersToo {
		q += ` AND visibility = 'everyone'`
	}
	st, err := scanStatus(s.db.QueryRowContext(ctx, q+` ORDER BY updated_at DESC, id DESC LIMIT 1`, kind, tmdbID, now()))
	if errors.Is(err, sql.ErrNoRows) {
		return st, ErrNotFound
	}
	return st, err
}

// DeleteStatusesForTitle forgets every status about one title (its
// request was removed, so nothing should say it's on its way).
func (s *Store) DeleteStatusesForTitle(ctx context.Context, kind string, tmdbID int) (int64, error) {
	res, err := s.db.ExecContext(ctx, `DELETE FROM integ_status WHERE kind = ? AND tmdb_id = ?`, kind, tmdbID)
	if err != nil {
		return 0, err
	}
	s.statusesGone(res, nil)
	return res.RowsAffected()
}

// StatusesForTitles is the newest live status for each of these TMDB IDs of
// one kind (Discover and search cards), keyed by TMDB ID.
func (s *Store) StatusesForTitles(ctx context.Context, kind string, tmdbIDs []int, ownersToo bool) (map[int]TitleStatus, error) {
	out := map[int]TitleStatus{}
	if len(tmdbIDs) == 0 {
		return out, nil
	}
	args := []any{kind, now()}
	marks := make([]string, len(tmdbIDs))
	for i, id := range tmdbIDs {
		marks[i] = "?"
		args = append(args, id)
	}
	q := `SELECT ` + statusCols + ` FROM integ_status WHERE kind = ? AND expires_at > ? AND tmdb_id IN (` + strings.Join(marks, ",") + `)`
	if !ownersToo {
		q += ` AND visibility = 'everyone'`
	}
	rows, err := s.db.QueryContext(ctx, q+` ORDER BY updated_at ASC, id ASC`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		st, err := scanStatus(rows)
		if err != nil {
			return nil, err
		}
		out[st.TMDBID] = st // newest last, so it wins
	}
	return out, rows.Err()
}

// PruneStatuses forgets statuses that expired.
func (s *Store) PruneStatuses(ctx context.Context) error {
	res, err := s.db.ExecContext(ctx, `DELETE FROM integ_status WHERE expires_at <= ?`, now())
	s.statusesGone(res, err)
	return err
}

// statusesGone moves the requests revision on when a delete removed rows.
func (s *Store) statusesGone(res sql.Result, err error) {
	if err != nil {
		return
	}
	if n, _ := res.RowsAffected(); n > 0 {
		s.requestsChanged()
	}
}
