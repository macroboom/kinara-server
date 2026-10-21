// Package quick keeps Quick Connect codes: a TV (or any app) that can't
// comfortably type a password asks the server for a short code, someone
// already signed in on a phone or in the web app enters it, and the TV is
// signed in as them.
//
// A request has two halves. The code (six digits) is what the TV shows and
// a person types; on its own it only lets a signed-in person approve it. The
// secret goes only to the TV, which polls with it; once the code is
// approved, the first poll takes the approval (and the request is gone).
// Codes last a few minutes and are kept in memory: a restart just means
// asking for a new one.
package quick

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"math/big"
	"sync"
	"time"
)

// TTL is how long a code can be approved, and an approval collected.
const TTL = 10 * time.Minute

// MaxPending caps the codes waiting at once, so a flood of starts can't
// grow memory without end (or make guessing a live code likely).
const MaxPending = 200

var (
	// ErrFull is returned by Start when MaxPending codes are waiting.
	ErrFull = errors.New("too many sign-in codes waiting; try again in a few minutes")
	// ErrNotFound is returned for a code that doesn't exist, ran out or was
	// already approved.
	ErrNotFound = errors.New("that code isn't waiting to be approved; check it, or get a new one on the TV")
)

// Request is a code waiting to be approved, or approved and waiting to be
// collected.
type Request struct {
	Code string
	// Device is the name the app gave ("Living Room TV").
	Device string
	// Info is anything the caller wants back when the approval is
	// collected (the app's device description).
	Info    any
	Created time.Time
	Expires time.Time
	// UserID is who approved it; 0 while waiting.
	UserID int64
}

// Status is what a poll found.
type Status int

const (
	// Waiting: not approved yet.
	Waiting Status = iota
	// Approved: approved, and this poll collected it.
	Approved
	// Gone: unknown, expired or already collected.
	Gone
)

// Codes holds the live requests. The zero value is not usable; use New.
type Codes struct {
	mu       sync.Mutex
	byCode   map[string]*Request
	bySecret map[string]*Request // by sha256 of the secret
	// Now is the clock; tests replace it.
	Now func() time.Time
}

func New() *Codes {
	return &Codes{byCode: map[string]*Request{}, bySecret: map[string]*Request{}, Now: time.Now}
}

// Start makes a new code for device. It returns the request and the secret
// the app polls with.
func (c *Codes) Start(device string, info any) (Request, string, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	now := c.Now()
	c.sweep(now)
	if len(c.byCode) >= MaxPending {
		return Request{}, "", ErrFull
	}
	var code string
	for {
		n, err := rand.Int(rand.Reader, big.NewInt(1_000_000))
		if err != nil {
			return Request{}, "", err
		}
		code = leftPad(n.String())
		if _, taken := c.byCode[code]; !taken {
			break
		}
	}
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return Request{}, "", err
	}
	secret := "kq_" + base64.RawURLEncoding.EncodeToString(b)
	r := &Request{Code: code, Device: device, Info: info, Created: now, Expires: now.Add(TTL)}
	c.byCode[code] = r
	c.bySecret[hashOf(secret)] = r
	return *r, secret, nil
}

// Pending finds a code that is waiting to be approved.
func (c *Codes) Pending(code string) (Request, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	r, ok := c.byCode[Clean(code)]
	if !ok || r.UserID != 0 || !c.Now().Before(r.Expires) {
		return Request{}, false
	}
	return *r, true
}

// Approve signs the code's app in as userID, once.
func (c *Codes) Approve(code string, userID int64) (Request, error) {
	if userID == 0 {
		return Request{}, errors.New("no user")
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	r, ok := c.byCode[Clean(code)]
	if !ok || r.UserID != 0 || !c.Now().Before(r.Expires) {
		return Request{}, ErrNotFound
	}
	r.UserID = userID
	return *r, nil
}

// Poll reports on the request behind secret. Approved hands over the
// request (with UserID set) and forgets it, so only one poll gets it.
func (c *Codes) Poll(secret string) (Status, Request) {
	c.mu.Lock()
	defer c.mu.Unlock()
	h := hashOf(secret)
	r, ok := c.bySecret[h]
	if !ok || !c.Now().Before(r.Expires) {
		return Gone, Request{}
	}
	if r.UserID == 0 {
		return Waiting, Request{}
	}
	delete(c.bySecret, h)
	delete(c.byCode, r.Code)
	return Approved, *r
}

// Cancel forgets the request behind secret (the TV went back).
func (c *Codes) Cancel(secret string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	h := hashOf(secret)
	if r, ok := c.bySecret[h]; ok {
		delete(c.bySecret, h)
		delete(c.byCode, r.Code)
	}
}

// sweep drops expired requests. Called with mu held.
func (c *Codes) sweep(now time.Time) {
	for h, r := range c.bySecret {
		if !now.Before(r.Expires) {
			delete(c.bySecret, h)
			delete(c.byCode, r.Code)
		}
	}
}

// Clean keeps only the digits of what someone typed ("123 456" is 123456).
func Clean(s string) string {
	out := make([]byte, 0, 6)
	for i := 0; i < len(s); i++ {
		if s[i] >= '0' && s[i] <= '9' {
			out = append(out, s[i])
		}
	}
	return string(out)
}

func leftPad(s string) string {
	for len(s) < 6 {
		s = "0" + s
	}
	return s
}

func hashOf(secret string) string {
	h := sha256.Sum256([]byte(secret))
	return hex.EncodeToString(h[:])
}
