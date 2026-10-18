package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
)

// Outgoing webhooks: the owner's URLs, the queue of deliveries waiting to
// go out, and the log of tries.

// WebhookHeader is an extra header sent with every delivery.
type WebhookHeader struct {
	Name  string `json:"name"`
	Value string `json:"value"`
}

// Webhook is a URL the server posts events to.
type Webhook struct {
	ID      int64
	Name    string
	URL     string
	Enabled bool
	Secret  string
	Events  []string
	// Libraries and Accounts limit which events go out; nil means all.
	Libraries    []int64
	Accounts     []int64
	Headers      []WebhookHeader
	Retries      int
	TimeoutSec   int
	IncludePaths bool
	CreatedAt    int64
}

// QueuedDelivery is an event waiting to be sent to one webhook.
type QueuedDelivery struct {
	Seq        int64
	DeliveryID string
	WebhookID  int64
	Event      string
	Body       string
	Attempts   int
	NextAt     int64
	CreatedAt  int64
}

// Attempt is one try at sending a delivery.
type Attempt struct {
	ID         int64
	WebhookID  int64
	DeliveryID string
	Event      string
	At         int64
	// Status is nil when no answer came.
	Status   *int
	Ms       int64
	Attempt  int
	Error    string
	Request  string
	Response *string
}

const webhookCols = `id, name, url, enabled, secret, events, libraries, accounts, headers, retries, timeout_sec, include_paths, created_at`

func scanWebhook(r scanner) (Webhook, error) {
	var w Webhook
	var events, libs, accts, headers string
	err := r.Scan(&w.ID, &w.Name, &w.URL, &w.Enabled, &w.Secret, &events, &libs, &accts, &headers,
		&w.Retries, &w.TimeoutSec, &w.IncludePaths, &w.CreatedAt)
	if err != nil {
		return w, err
	}
	_ = json.Unmarshal([]byte(events), &w.Events)
	w.Libraries = decodeIDs(libs)
	w.Accounts = decodeIDs(accts)
	_ = json.Unmarshal([]byte(headers), &w.Headers)
	if w.Events == nil {
		w.Events = []string{}
	}
	if w.Headers == nil {
		w.Headers = []WebhookHeader{}
	}
	return w, nil
}

// encodeIDs stores nil as "all" and anything else as a JSON list.
func encodeIDs(ids []int64) string {
	if ids == nil {
		return `"all"`
	}
	b, _ := json.Marshal(ids)
	return string(b)
}

func decodeIDs(s string) []int64 {
	if s == `"all"` || s == "" {
		return nil
	}
	ids := []int64{}
	_ = json.Unmarshal([]byte(s), &ids)
	return ids
}

func jsonText(v any) string {
	b, _ := json.Marshal(v)
	return string(b)
}

// ListWebhooks lists webhooks, oldest first.
func (s *Store) ListWebhooks(ctx context.Context) ([]Webhook, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT `+webhookCols+` FROM integ_webhooks ORDER BY id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Webhook{}
	for rows.Next() {
		w, err := scanWebhook(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, w)
	}
	return out, rows.Err()
}

// GetWebhook returns one webhook, or ErrNotFound.
func (s *Store) GetWebhook(ctx context.Context, id int64) (Webhook, error) {
	w, err := scanWebhook(s.db.QueryRowContext(ctx, `SELECT `+webhookCols+` FROM integ_webhooks WHERE id = ?`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return w, ErrNotFound
	}
	return w, err
}

// CreateWebhook saves a new webhook, filling in its ID.
func (s *Store) CreateWebhook(ctx context.Context, w *Webhook) error {
	w.CreatedAt = now()
	res, err := s.db.ExecContext(ctx, `INSERT INTO integ_webhooks
		(name, url, enabled, secret, events, libraries, accounts, headers, retries, timeout_sec, include_paths, created_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		w.Name, w.URL, boolInt(w.Enabled), w.Secret, jsonText(w.Events), encodeIDs(w.Libraries), encodeIDs(w.Accounts),
		jsonText(w.Headers), w.Retries, w.TimeoutSec, boolInt(w.IncludePaths), w.CreatedAt)
	if err != nil {
		return err
	}
	w.ID, err = res.LastInsertId()
	return err
}

// UpdateWebhook saves a changed webhook (everything but its ID and when it
// was made).
func (s *Store) UpdateWebhook(ctx context.Context, w Webhook) error {
	res, err := s.db.ExecContext(ctx, `UPDATE integ_webhooks SET name = ?, url = ?, enabled = ?, secret = ?, events = ?,
		libraries = ?, accounts = ?, headers = ?, retries = ?, timeout_sec = ?, include_paths = ? WHERE id = ?`,
		w.Name, w.URL, boolInt(w.Enabled), w.Secret, jsonText(w.Events), encodeIDs(w.Libraries), encodeIDs(w.Accounts),
		jsonText(w.Headers), w.Retries, w.TimeoutSec, boolInt(w.IncludePaths), w.ID)
	return mustAffect(res, err)
}

// DeleteWebhook removes a webhook with its queue and log.
func (s *Store) DeleteWebhook(ctx context.Context, id int64) error {
	res, err := s.db.ExecContext(ctx, `DELETE FROM integ_webhooks WHERE id = ?`, id)
	return mustAffect(res, err)
}

// mustAffect turns "no rows changed" into ErrNotFound.
func mustAffect(res sql.Result, err error) error {
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

// EnqueueDelivery adds a delivery to the end of its webhook's queue.
func (s *Store) EnqueueDelivery(ctx context.Context, d QueuedDelivery) error {
	if d.CreatedAt == 0 {
		d.CreatedAt = now()
	}
	_, err := s.db.ExecContext(ctx, `INSERT INTO integ_queue (delivery_id, webhook_id, event, body, attempts, next_at, created_at)
		VALUES (?, ?, ?, ?, ?, ?, ?)`, d.DeliveryID, d.WebhookID, d.Event, d.Body, d.Attempts, d.NextAt, d.CreatedAt)
	return err
}

// QueueHeads returns the oldest waiting delivery of each turned-on webhook:
// only the head of a queue may go out, so deliveries stay in order.
func (s *Store) QueueHeads(ctx context.Context) ([]QueuedDelivery, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT q.seq, q.delivery_id, q.webhook_id, q.event, q.body, q.attempts, q.next_at, q.created_at
		FROM integ_queue q JOIN integ_webhooks w ON w.id = q.webhook_id
		WHERE w.enabled = 1 AND q.seq = (SELECT MIN(seq) FROM integ_queue WHERE webhook_id = q.webhook_id)
		ORDER BY q.next_at`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []QueuedDelivery
	for rows.Next() {
		var d QueuedDelivery
		if err := rows.Scan(&d.Seq, &d.DeliveryID, &d.WebhookID, &d.Event, &d.Body, &d.Attempts, &d.NextAt, &d.CreatedAt); err != nil {
			return nil, err
		}
		out = append(out, d)
	}
	return out, rows.Err()
}

// RemoveQueued takes a delivery off the queue (sent, or out of tries).
func (s *Store) RemoveQueued(ctx context.Context, seq int64) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM integ_queue WHERE seq = ?`, seq)
	return err
}

// RetryQueued records a failed try and when to try again.
func (s *Store) RetryQueued(ctx context.Context, seq int64, attempts int, nextAt int64) error {
	_, err := s.db.ExecContext(ctx, `UPDATE integ_queue SET attempts = ?, next_at = ? WHERE seq = ?`, attempts, nextAt, seq)
	return err
}

// AddAttempt logs a try, filling in its ID.
func (s *Store) AddAttempt(ctx context.Context, a *Attempt) error {
	res, err := s.db.ExecContext(ctx, `INSERT INTO integ_attempts
		(webhook_id, delivery_id, event, at, status, ms, attempt, error, request, response)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		a.WebhookID, a.DeliveryID, a.Event, a.At, a.Status, a.Ms, a.Attempt, a.Error, a.Request, a.Response)
	if err != nil {
		return err
	}
	a.ID, err = res.LastInsertId()
	return err
}

const attemptCols = `id, webhook_id, delivery_id, event, at, status, ms, attempt, error, request, response`

func scanAttempt(r scanner) (Attempt, error) {
	var a Attempt
	var status sql.NullInt64
	var resp sql.NullString
	err := r.Scan(&a.ID, &a.WebhookID, &a.DeliveryID, &a.Event, &a.At, &status, &a.Ms, &a.Attempt, &a.Error, &a.Request, &resp)
	if status.Valid {
		n := int(status.Int64)
		a.Status = &n
	}
	if resp.Valid {
		a.Response = &resp.String
	}
	return a, err
}

// Attempts lists a webhook's tries, newest first.
func (s *Store) Attempts(ctx context.Context, webhookID int64, limit int) ([]Attempt, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT `+attemptCols+` FROM integ_attempts WHERE webhook_id = ?
		ORDER BY id DESC LIMIT ?`, webhookID, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Attempt{}
	for rows.Next() {
		a, err := scanAttempt(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, a)
	}
	return out, rows.Err()
}

// GetAttempt returns one of a webhook's tries, or ErrNotFound.
func (s *Store) GetAttempt(ctx context.Context, webhookID, id int64) (Attempt, error) {
	a, err := scanAttempt(s.db.QueryRowContext(ctx, `SELECT `+attemptCols+` FROM integ_attempts
		WHERE webhook_id = ? AND id = ?`, webhookID, id))
	if errors.Is(err, sql.ErrNoRows) {
		return a, ErrNotFound
	}
	return a, err
}

// LastAttempts is each webhook's newest try, for its health.
func (s *Store) LastAttempts(ctx context.Context) (map[int64]Attempt, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT `+attemptCols+` FROM integ_attempts a
		WHERE id = (SELECT MAX(id) FROM integ_attempts WHERE webhook_id = a.webhook_id)`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[int64]Attempt{}
	for rows.Next() {
		a, err := scanAttempt(rows)
		if err != nil {
			return nil, err
		}
		out[a.WebhookID] = a
	}
	return out, rows.Err()
}

// PruneAttempts forgets tries made before a time.
func (s *Store) PruneAttempts(ctx context.Context, before int64) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM integ_attempts WHERE at < ?`, before)
	return err
}

// CountQueued is how many deliveries are waiting for turned-on webhooks.
func (s *Store) CountQueued(ctx context.Context) (int, error) {
	var n int
	err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM integ_queue q JOIN integ_webhooks w ON w.id = q.webhook_id
		WHERE w.enabled = 1`).Scan(&n)
	return n, err
}
