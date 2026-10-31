package api

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"kinara/internal/store"
)

// The delivery worker: queues what emit hands it, sends each webhook's
// deliveries in order, retries failures with growing waits, and does the
// integrations' housekeeping. It runs while any webhook is turned on and
// stops with BaseCtx.

// retryWaits are the waits before each retry. Added up they match the
// manager's labels: 1 retry over 1 min, 3 over 15 min, 5 over 2 hr and 8
// over 12 hr.
var retryWaits = []time.Duration{
	time.Minute, 4 * time.Minute, 10 * time.Minute, 45 * time.Minute,
	time.Hour, 2 * time.Hour, 4 * time.Hour, 4 * time.Hour,
}

// retryWait is how long to wait after a delivery's nth failed try (1 for
// the first).
func retryWait(tries int) time.Duration {
	if tries < 1 {
		tries = 1
	}
	if tries > len(retryWaits) {
		return retryWaits[len(retryWaits)-1]
	}
	return retryWaits[tries-1]
}

// keepDeliveries is how long the delivery log is kept.
const keepDeliveries = 7 * 24 * time.Hour

// maxResponseLog is how much of a receiver's answer the log keeps.
const maxResponseLog = 4 << 10

// tick is how often the worker looks around when nothing wakes it.
const tick = 5 * time.Second

// signature is the X-Kinara-Signature value: "sha256=" and the hex HMAC
// SHA-256 of "<timestamp>.<body>" with the webhook's secret.
func signature(secret string, timestamp int64, body []byte) string {
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write([]byte(strconv.FormatInt(timestamp, 10)))
	mac.Write([]byte("."))
	mac.Write(body)
	return "sha256=" + hex.EncodeToString(mac.Sum(nil))
}

// ensureWorker starts the delivery worker if a webhook is on and it isn't
// running.
func (s *Server) ensureWorker() {
	in := &s.integ
	in.mu.Lock()
	defer in.mu.Unlock()
	if in.running || in.wake == nil || (len(in.hooks) == 0 && len(in.pending) == 0) {
		return
	}
	ctx := s.baseCtx()
	if ctx.Err() != nil {
		return
	}
	in.running = true
	if in.busy == nil {
		in.busy = map[int64]bool{}
	}
	in.wg.Add(1)
	go func() {
		defer in.wg.Done()
		s.deliveryLoop(ctx)
	}()
}

func (s *Server) wakeWorker() {
	select {
	case s.integ.wake <- struct{}{}:
	default:
	}
}

func (s *Server) deliveryLoop(ctx context.Context) {
	in := &s.integ
	done := make(chan int64, 16)
	lastChores := time.Time{}
	timer := time.NewTimer(0)
	defer timer.Stop()
	for {
		s.queuePending(ctx)
		next := s.dispatch(ctx, done)
		if time.Since(lastChores) >= tick {
			lastChores = time.Now()
			s.chores(ctx)
		}

		if ctx.Err() != nil {
			// Shutting down: sends in flight end with the context.
			for len(in.busy) > 0 {
				id := <-done
				in.mu.Lock()
				delete(in.busy, id)
				in.mu.Unlock()
			}
			in.mu.Lock()
			in.running = false
			in.mu.Unlock()
			return
		}
		// Stop when nobody is listening and nothing is in flight. Turning a
		// webhook on starts it again.
		in.mu.Lock()
		if len(in.hooks) == 0 && len(in.pending) == 0 && len(in.busy) == 0 {
			in.running = false
			in.mu.Unlock()
			return
		}
		in.mu.Unlock()

		wait := tick
		if !next.IsZero() {
			if d := time.Until(next); d < wait {
				wait = max(d, 50*time.Millisecond)
			}
		}
		timer.Reset(wait)
		select {
		case <-ctx.Done():
		case <-in.wake:
		case id := <-done:
			in.mu.Lock()
			delete(in.busy, id)
			in.mu.Unlock()
		case <-timer.C:
		}
		if !timer.Stop() {
			select {
			case <-timer.C:
			default:
			}
		}
	}
}

// queuePending moves what emit handed over into the database queue.
func (s *Server) queuePending(ctx context.Context) {
	for {
		select {
		case d := <-s.integ.pending:
			if err := s.Store.EnqueueDelivery(ctx, d); err != nil {
				s.Log.Warn("integrations: queueing a delivery", "event", d.Event, "err", err)
			}
		default:
			return
		}
	}
}

// dispatch sends the head of each webhook's queue that is due and not
// already in flight. It returns when the next waiting one is due (zero if
// none).
func (s *Server) dispatch(ctx context.Context, done chan<- int64) time.Time {
	if ctx.Err() != nil {
		return time.Time{}
	}
	heads, err := s.Store.QueueHeads(ctx)
	if err != nil {
		s.Log.Warn("integrations: reading the queue", "err", err)
		return time.Now().Add(tick)
	}
	in := &s.integ
	now := time.Now()
	var next time.Time
	for _, d := range heads {
		in.mu.Lock()
		busy := in.busy[d.WebhookID]
		in.mu.Unlock()
		if busy {
			continue
		}
		if d.NextAt > now.Unix() {
			if t := time.Unix(d.NextAt, 0); next.IsZero() || t.Before(next) {
				next = t
			}
			continue
		}
		in.mu.Lock()
		in.busy[d.WebhookID] = true
		in.mu.Unlock()
		in.wg.Add(1)
		go func(d store.QueuedDelivery) {
			defer in.wg.Done()
			s.sendQueued(ctx, d)
			done <- d.WebhookID
		}(d)
	}
	return next
}

// sendQueued makes one try at a queued delivery and records the outcome.
func (s *Server) sendQueued(ctx context.Context, d store.QueuedDelivery) {
	h, err := s.Store.GetWebhook(ctx, d.WebhookID)
	if err != nil {
		return // removed meanwhile; its queue went with it
	}
	try := d.Attempts + 1
	a := s.post(ctx, h, d.DeliveryID, d.Event, []byte(d.Body), try)
	if ctx.Err() != nil {
		return // shutting down: not the receiver's fault, try again after
	}
	if err := s.Store.AddAttempt(ctx, &a); err != nil {
		s.Log.Warn("integrations: logging a delivery", "err", err)
	}
	switch {
	case delivered(a):
		err = s.Store.RemoveQueued(ctx, d.Seq)
	case try <= h.Retries:
		err = s.Store.RetryQueued(ctx, d.Seq, try, time.Now().Add(retryWait(try)).Unix())
	default:
		s.Log.Warn("integrations: delivery failed for good", "webhook", h.Name, "event", d.Event, "tries", try, "err", a.Error)
		err = s.Store.RemoveQueued(ctx, d.Seq)
	}
	if err != nil {
		s.Log.Warn("integrations: updating the queue", "err", err)
	}
}

func delivered(a store.Attempt) bool {
	return a.Status != nil && *a.Status >= 200 && *a.Status < 300
}

// post sends one delivery and says what happened. It never follows a
// redirect to another host, and keeps only the start of the answer.
func (s *Server) post(ctx context.Context, h store.Webhook, deliveryID, event string, body []byte, try int) store.Attempt {
	start := time.Now()
	a := store.Attempt{WebhookID: h.ID, DeliveryID: deliveryID, Event: event, At: start.Unix(), Attempt: try, Request: string(body)}
	timeout := time.Duration(clampTimeout(h.TimeoutSec)) * time.Second
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, h.URL, bytes.NewReader(body))
	if err != nil {
		a.Error = "The address can't be used: " + err.Error()
		return a
	}
	ts := start.Unix()
	for _, hd := range h.Headers {
		if hd.Name != "" {
			req.Header.Set(hd.Name, hd.Value)
		}
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", "Kinara/"+s.Version)
	req.Header.Set("X-Kinara-Event", event)
	req.Header.Set("X-Kinara-Delivery", deliveryID)
	req.Header.Set("X-Kinara-Timestamp", strconv.FormatInt(ts, 10))
	req.Header.Set("X-Kinara-Signature", signature(h.Secret, ts, body))

	resp, err := webhookClient().Do(req)
	a.Ms = time.Since(start).Milliseconds()
	if err != nil {
		a.Error = sendError(err, timeout)
		return a
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(io.LimitReader(resp.Body, maxResponseLog))
	text := strings.ToValidUTF8(string(data), "")
	a.Response = &text
	code := resp.StatusCode
	a.Status = &code
	switch {
	case code >= 200 && code < 300:
	case code >= 300 && code < 400:
		a.Error = "Redirected to " + resp.Header.Get("Location") + "; Kinara only follows redirects on the same host"
	default:
		a.Error = http.StatusText(code)
		if a.Error == "" {
			a.Error = fmt.Sprintf("HTTP %d", code)
		}
	}
	return a
}

// sameHostRedirects follows a few redirects, but never to another host,
// away from http(s), or from https down to http: a webhook's address is
// where its data may go.
func sameHostRedirects(req *http.Request, via []*http.Request) error {
	if len(via) >= 3 || req.URL.Host != via[0].URL.Host ||
		(req.URL.Scheme != "http" && req.URL.Scheme != "https") ||
		(via[0].URL.Scheme == "https" && req.URL.Scheme != "https") {
		return http.ErrUseLastResponse
	}
	return nil
}

// sendError says in plain words why a delivery got no answer.
func sendError(err error, timeout time.Duration) string {
	var ne net.Error
	switch {
	case errors.Is(err, context.DeadlineExceeded) || (errors.As(err, &ne) && ne.Timeout()):
		return fmt.Sprintf("Timed out after %d s", int(timeout.Seconds()))
	case strings.Contains(err.Error(), "connection refused"):
		return "Couldn't connect: nothing is listening at that address"
	case strings.Contains(err.Error(), "no such host"):
		return "Couldn't find that host name"
	case errors.Is(err, errWebhookTarget):
		if webhooksLocalAllowed() {
			return "Webhooks can't be sent to link-local addresses (like 169.254.169.254)"
		}
		return "Webhooks can't be sent to this computer or the home network (KINARA_WEBHOOKS_LOCAL=off)"
	}
	msg := err.Error()
	if len(msg) > 300 {
		msg = msg[:300]
		for !utf8.ValidString(msg) {
			msg = msg[:len(msg)-1]
		}
	}
	return "Couldn't send: " + msg
}

func clampTimeout(sec int) int {
	return min(max(sec, 5), 30)
}

// chores: forget old deliveries and expired statuses, notice playback that
// stopped, and watch remote access.
func (s *Server) chores(ctx context.Context) {
	if ctx.Err() != nil {
		return
	}
	_ = s.Store.PruneAttempts(ctx, time.Now().Add(-keepDeliveries).Unix())
	_ = s.Store.PruneStatuses(ctx)
	s.sweepPlays(time.Now())
	s.watchRemote()
}
