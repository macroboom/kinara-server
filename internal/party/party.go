// Package party runs watch parties: several devices playing the same title
// at the same moment.
//
// The server is the one clock everybody shares. A party's playback state is
// a single line through time: "at server time Anchor the picture is at
// Position, and from then on it moves at Rate" (or, paused, it stays at
// Position). Every device works out where it should be from that line and
// its own estimate of the server clock, so nobody follows anybody else and
// small network delays don't add up.
//
// Starts and resumes are scheduled a little in the future (Lead), so every
// device has the order in hand before the moment arrives and starts on the
// same frame instead of "when the message got there". Seeks, new titles and
// a device that stalls go through a gate: the party holds still until
// everyone reports it is ready at the new spot (or GateTimeout passes), then
// starts together again.
//
// Parties live in memory: a restart ends them, and devices simply carry on
// playing on their own.
package party

import (
	"context"
	"crypto/rand"
	"errors"
	"math"
	"sort"
	"strings"
	"sync"
	"time"
)

const (
	// Lead is how far ahead a start is scheduled. It covers delivering the
	// change to every device (long-poll answer, relay hops) and the player
	// getting ready to roll. Devices that hear late catch up on their own.
	Lead = 1000 * time.Millisecond
	// GateTimeout is the longest a party waits for a slow device after a
	// seek or resume; then it starts without it and that device catches up.
	GateTimeout = 8 * time.Second
	// LoadTimeout is the same for a new title, which takes longer to open.
	LoadTimeout = 15 * time.Second
	// MemberTimeout drops a device that stopped checking in (app closed,
	// network gone).
	MemberTimeout = 30 * time.Second
	// InviteTimeout forgets unanswered invites.
	InviteTimeout = 10 * time.Minute
	// MaxMembers keeps a party to a sensible size.
	MaxMembers = 16
)

var (
	ErrNotFound  = errors.New("that watch party has ended")
	ErrNoCode    = errors.New("no watch party has that code")
	ErrNotMember = errors.New("this device isn't in that watch party")
	ErrFull      = errors.New("that watch party is full")
	ErrBadAction = errors.New("unknown action")
)

// State is the playback line every device follows.
type State struct {
	Playing bool `json:"playing"`
	// Position in seconds at Anchor.
	Position float64 `json:"position"`
	// Anchor is server time in Unix milliseconds.
	Anchor int64 `json:"anchor"`
	// Rate is the playback speed (1 for now; here so it never needs a new field).
	Rate float64 `json:"rate"`
	// Hold says why the party is paused on its own ("Waiting for Living
	// Room TV"); empty when someone paused it or it's playing.
	Hold string `json:"hold,omitempty"`
}

// At is where playback should be at server time now (Unix ms).
func (s State) At(now int64) float64 {
	if !s.Playing || now <= s.Anchor {
		return s.Position
	}
	return s.Position + float64(now-s.Anchor)/1000*s.Rate
}

// Device is who someone is: the device ID the API derives from the session,
// and names to show.
type Device struct {
	ID   string `json:"device"`
	Name string `json:"name"`
	User string `json:"user"`
}

// Member is a device in a party and how it's doing.
type Member struct {
	Device
	Host     bool  `json:"host"`
	JoinedAt int64 `json:"joinedAt"`
	// Ready means the current title is open at the gate's spot and buffered.
	Ready bool `json:"ready"`
	// Stalled means it ran out of picture while it should be playing.
	Stalled bool `json:"stalled"`
	// DriftMs is how far ahead (+) or behind (-) of the line it last was.
	DriftMs float64 `json:"driftMs"`
	SeenAt  int64   `json:"-"`
}

// Invite is a device asked to join.
type Invite struct {
	Device string `json:"device"`
	From   string `json:"from"`
	At     int64  `json:"at"`
}

// Party is one watch party as the API shows it.
type Party struct {
	ID      string   `json:"id"`
	Code    string   `json:"code"`
	ItemID  string   `json:"itemId"`
	Title   string   `json:"title"`
	Rev     int64    `json:"rev"`
	State   State    `json:"state"`
	Members []Member `json:"members"`
	Invited []Invite `json:"invited"`
	Created int64    `json:"created"`
	// ServerTime is when this copy was made (Unix ms), a free clock sample.
	ServerTime int64 `json:"serverTime"`

	// The gate: after a seek, a new title, a resume or a stall, playback
	// starts again once every member is ready or gateUntil passes.
	gateOpen  bool
	gateSince int64
	gateUntil int64
	gateStart bool // start playing when the gate opens (else stay paused)
}

// Report is what a member says about itself every couple of seconds and
// whenever something changes.
type Report struct {
	ItemID   string  `json:"itemId"`
	Position float64 `json:"position"`
	Playing  bool    `json:"playing"`
	Stalled  bool    `json:"stalled"`
	Ready    bool    `json:"ready"`
	DriftMs  float64 `json:"driftMs"`
}

// Control is a change one member asks for.
type Control struct {
	// "play", "pause", "seek" or "load" (a new title, e.g. the next episode).
	Action   string  `json:"action"`
	Position float64 `json:"position"`
	ItemID   string  `json:"itemId"`
	Title    string  `json:"title"`
	// Autoplay is for "load": start once everyone has it open (default true).
	Autoplay *bool `json:"autoplay"`
}

// Hub holds every party on the server.
type Hub struct {
	now func() time.Time

	mu      sync.Mutex
	parties map[string]*Party
	byCode  map[string]string
	rev     int64         // moves on any change, for the pulse
	changed chan struct{} // closed and replaced on every change
}

// New makes a hub. now is the clock (time.Now outside tests).
func New(now func() time.Time) *Hub {
	if now == nil {
		now = time.Now
	}
	return &Hub{
		now:     now,
		parties: map[string]*Party{},
		byCode:  map[string]string{},
		rev:     now().UnixMilli(),
		changed: make(chan struct{}),
	}
}

func (h *Hub) ms() int64 { return h.now().UnixMilli() }

// Rev moves whenever any party or invite changes.
func (h *Hub) Rev() int64 {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.rev
}

// bump records a change to p (nil: only invites or membership elsewhere).
// Callers hold mu.
func (h *Hub) bump(p *Party) {
	h.rev++
	if p != nil {
		p.Rev = h.rev
	}
	close(h.changed)
	h.changed = make(chan struct{})
}

func (h *Hub) view(p *Party) Party {
	c := *p
	c.Members = append([]Member(nil), p.Members...)
	c.Invited = append([]Invite(nil), p.Invited...)
	c.ServerTime = h.ms()
	return c
}

// Create starts a party around the host's current playback. It keeps
// playing (or paused) exactly as the host was, so starting one interrupts
// nobody. A host already in another party leaves it.
func (h *Hub) Create(host Device, itemID string, title string, position float64, playing bool) Party {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.leaveAllLocked(host.ID)
	now := h.ms()
	p := &Party{
		ID:      randomID(),
		Code:    h.newCodeLocked(),
		ItemID:  itemID,
		Title:   title,
		State:   State{Playing: playing, Position: math.Max(0, position), Anchor: now, Rate: 1},
		Members: []Member{{Device: host, Host: true, JoinedAt: now, Ready: true, SeenAt: now}},
		Created: now,
	}
	h.parties[p.ID] = p
	h.byCode[p.Code] = p.ID
	h.bump(p)
	return h.view(p)
}

// Join adds a device by party ID (accepting an invite) or by code.
func (h *Hub) Join(d Device, id, code string) (Party, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if id == "" {
		id = h.byCode[NormalizeCode(code)]
		if id == "" {
			return Party{}, ErrNoCode
		}
	}
	p := h.parties[id]
	if p == nil {
		return Party{}, ErrNotFound
	}
	if i := memberIndex(p, d.ID); i >= 0 {
		p.Members[i].SeenAt = h.ms()
		return h.view(p), nil
	}
	if len(p.Members) >= MaxMembers {
		return Party{}, ErrFull
	}
	h.leaveAllLocked(d.ID)
	now := h.ms()
	p.Members = append(p.Members, Member{Device: d, JoinedAt: now, SeenAt: now})
	p.Invited = dropInvite(p.Invited, d.ID)
	h.bump(p)
	return h.view(p), nil
}

// Leave takes a device out. The longest-standing member becomes host if the
// host leaves; an empty party ends.
func (h *Hub) Leave(id, device string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if p := h.parties[id]; p != nil {
		h.removeMemberLocked(p, device)
	}
}

// Decline drops an invite.
func (h *Hub) Decline(id, device string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if p := h.parties[id]; p != nil {
		before := len(p.Invited)
		p.Invited = dropInvite(p.Invited, device)
		if len(p.Invited) != before {
			h.bump(p)
		}
	}
}

// InviteDevices asks devices to join. Members and repeats are skipped.
func (h *Hub) InviteDevices(id string, from Device, devices []string) (Party, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	p := h.parties[id]
	if p == nil {
		return Party{}, ErrNotFound
	}
	if memberIndex(p, from.ID) < 0 {
		return Party{}, ErrNotMember
	}
	now := h.ms()
	changed := false
	for _, d := range devices {
		if d == "" || memberIndex(p, d) >= 0 {
			continue
		}
		p.Invited = dropInvite(p.Invited, d)
		p.Invited = append(p.Invited, Invite{Device: d, From: from.Name, At: now})
		changed = true
	}
	if changed {
		h.bump(p)
	}
	return h.view(p), nil
}

// Mine is the party a device is in (if any) and the parties inviting it.
func (h *Hub) Mine(device string) (*Party, []Party) {
	h.mu.Lock()
	defer h.mu.Unlock()
	var mine *Party
	var invites []Party
	for _, p := range h.parties {
		if i := memberIndex(p, device); i >= 0 {
			p.Members[i].SeenAt = h.ms()
			v := h.view(p)
			mine = &v
			continue
		}
		for _, inv := range p.Invited {
			if inv.Device == device {
				invites = append(invites, h.view(p))
				break
			}
		}
	}
	sort.Slice(invites, func(i, j int) bool { return invites[i].Created > invites[j].Created })
	return mine, invites
}

// Get is one party, for a member.
func (h *Hub) Get(id, device string) (Party, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	p := h.parties[id]
	if p == nil {
		return Party{}, ErrNotFound
	}
	i := memberIndex(p, device)
	if i < 0 {
		return Party{}, ErrNotMember
	}
	p.Members[i].SeenAt = h.ms()
	return h.view(p), nil
}

// Do applies a member's control.
func (h *Hub) Do(id, device string, c Control) (Party, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	p := h.parties[id]
	if p == nil {
		return Party{}, ErrNotFound
	}
	if memberIndex(p, device) < 0 {
		return Party{}, ErrNotMember
	}
	now := h.ms()
	cur := p.State.At(now)
	switch c.Action {
	case "pause":
		p.State = State{Playing: false, Position: cur, Anchor: now, Rate: 1}
		p.gateOpen = false
	case "play":
		// Everyone is normally parked and buffered after a pause, so the gate
		// usually opens at once; a device that joined or is still loading
		// gets its few seconds.
		pos := p.State.Position
		if p.State.Playing {
			pos = cur
		}
		h.closeGateLocked(p, pos, true, GateTimeout, "", false)
	case "seek":
		h.closeGateLocked(p, math.Max(0, c.Position), p.State.Playing || p.gateStart, GateTimeout, "", true)
	case "load":
		if c.ItemID == "" {
			return Party{}, errors.New("load needs an itemId")
		}
		p.ItemID = c.ItemID
		if c.Title != "" {
			p.Title = c.Title
		}
		auto := c.Autoplay == nil || *c.Autoplay
		h.closeGateLocked(p, math.Max(0, c.Position), auto, LoadTimeout, "Loading", true)
	default:
		return Party{}, ErrBadAction
	}
	h.tryOpenGateLocked(p, now)
	h.bump(p)
	return h.view(p), nil
}

// closeGateLocked parks everyone at pos until they're all ready. After a
// move (moved true) every member has to confirm again, including the one
// who asked: its own seek takes time too. A plain resume keeps what members
// last said, since they're parked and buffered where they paused.
func (h *Hub) closeGateLocked(p *Party, pos float64, start bool, timeout time.Duration, hold string, moved bool) {
	now := h.ms()
	if hold == "" && start {
		hold = "Syncing"
	}
	p.State = State{Playing: false, Position: pos, Anchor: now, Rate: 1, Hold: hold}
	p.gateOpen = true
	p.gateStart = start
	p.gateSince = now
	p.gateUntil = now + timeout.Milliseconds()
	if moved {
		for i := range p.Members {
			p.Members[i].Ready = false
			p.Members[i].Stalled = false
		}
	}
}

// labelGrace is how long a gate shows its own label ("Syncing") before
// naming the devices it's still waiting for.
const labelGrace = 2000

// tryOpenGateLocked starts playback once everyone is ready, or the wait ran
// out. Reports true if the state changed.
func (h *Hub) tryOpenGateLocked(p *Party, now int64) bool {
	if !p.gateOpen {
		return false
	}
	var waiting []string
	for _, m := range p.Members {
		if !m.Ready {
			waiting = append(waiting, m.Name)
		}
	}
	if len(waiting) > 0 && now < p.gateUntil {
		hold := "Waiting for " + strings.Join(waiting, ", ")
		if (p.State.Hold == "Loading" || p.State.Hold == "Syncing") && now-p.gateSince < labelGrace {
			// Keep the first label briefly; it reads better than a list of
			// everyone the moment a seek is made.
			return false
		}
		if p.State.Hold != hold {
			p.State.Hold = hold
			return true
		}
		return false
	}
	p.gateOpen = false
	if p.gateStart {
		p.State = State{Playing: true, Position: p.State.Position, Anchor: now + Lead.Milliseconds(), Rate: 1}
	} else {
		p.State.Hold = ""
	}
	return true
}

// Report records how a member is doing and acts on it.
func (h *Hub) Report(id, device string, r Report) (Party, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	p := h.parties[id]
	if p == nil {
		return Party{}, ErrNotFound
	}
	i := memberIndex(p, device)
	if i < 0 {
		return Party{}, ErrNotMember
	}
	now := h.ms()
	m := &p.Members[i]
	m.SeenAt = now
	m.DriftMs = r.DriftMs
	changed := false

	onItem := r.ItemID == p.ItemID
	ready := onItem && r.Ready && !r.Stalled
	if m.Ready != ready {
		m.Ready = ready
		changed = true
	}
	stalled := onItem && r.Stalled
	if m.Stalled != stalled {
		m.Stalled = stalled
		changed = true
	}

	// Someone ran dry while the party plays: everyone holds where the line
	// is now, and starts again together once they've caught their breath.
	if stalled && p.State.Playing && now > p.State.Anchor {
		h.closeGateLocked(p, p.State.At(now), true, GateTimeout, "Waiting for "+m.Name, false)
		p.Members[i].Ready = false
		// The others are fine where they are: they only need to stop.
		for j := range p.Members {
			if j != i {
				p.Members[j].Ready = true
			}
		}
		changed = true
	}
	if h.tryOpenGateLocked(p, now) {
		changed = true
	}
	if changed {
		h.bump(p)
	}
	return h.view(p), nil
}

// Tick drops devices that went quiet, forgets old invites and opens gates
// whose wait ran out. Call it every half second or so.
func (h *Hub) Tick() {
	h.mu.Lock()
	defer h.mu.Unlock()
	now := h.ms()
	for _, p := range h.parties {
		for _, m := range append([]Member(nil), p.Members...) {
			if now-m.SeenAt > MemberTimeout.Milliseconds() {
				h.removeMemberLocked(p, m.ID)
			}
		}
		if h.parties[p.ID] == nil {
			continue
		}
		kept := p.Invited[:0]
		for _, inv := range p.Invited {
			if now-inv.At < InviteTimeout.Milliseconds() {
				kept = append(kept, inv)
			}
		}
		changed := len(kept) != len(p.Invited)
		p.Invited = kept
		if h.tryOpenGateLocked(p, now) {
			changed = true
		}
		if changed {
			h.bump(p)
		}
	}
}

// Run calls Tick until ctx ends.
func (h *Hub) Run(ctx context.Context) {
	t := time.NewTicker(500 * time.Millisecond)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			h.Tick()
		}
	}
}

// Wait returns once the party's revision passes rev, the party ends, or ctx
// ends (ok false: nothing new).
func (h *Hub) Wait(ctx context.Context, id, device string, rev int64) (Party, bool, error) {
	for {
		h.mu.Lock()
		p := h.parties[id]
		if p == nil {
			h.mu.Unlock()
			return Party{}, true, ErrNotFound
		}
		i := memberIndex(p, device)
		if i < 0 {
			h.mu.Unlock()
			return Party{}, true, ErrNotMember
		}
		p.Members[i].SeenAt = h.ms()
		if p.Rev > rev {
			v := h.view(p)
			h.mu.Unlock()
			return v, true, nil
		}
		ch := h.changed
		h.mu.Unlock()
		select {
		case <-ctx.Done():
			h.mu.Lock()
			var v Party
			if p := h.parties[id]; p != nil {
				v = h.view(p)
			}
			h.mu.Unlock()
			return v, false, nil
		case <-ch:
		}
	}
}

func (h *Hub) leaveAllLocked(device string) {
	for _, p := range h.parties {
		if memberIndex(p, device) >= 0 {
			h.removeMemberLocked(p, device)
		}
	}
}

func (h *Hub) removeMemberLocked(p *Party, device string) {
	i := memberIndex(p, device)
	if i < 0 {
		return
	}
	wasHost := p.Members[i].Host
	p.Members = append(p.Members[:i], p.Members[i+1:]...)
	if len(p.Members) == 0 {
		delete(h.parties, p.ID)
		delete(h.byCode, p.Code)
		h.bump(nil)
		return
	}
	if wasHost {
		p.Members[0].Host = true // members stay in join order
	}
	h.tryOpenGateLocked(p, h.ms()) // a gate may have been waiting on them
	h.bump(p)
}

func memberIndex(p *Party, device string) int {
	for i, m := range p.Members {
		if m.ID == device {
			return i
		}
	}
	return -1
}

func dropInvite(list []Invite, device string) []Invite {
	out := list[:0]
	for _, inv := range list {
		if inv.Device != device {
			out = append(out, inv)
		}
	}
	return out
}

// Codes leave out letters that read alike (I/L/1, O/0) on a TV across a room.
const codeLetters = "ABCDEFGHJKMNPQRSTUVWXYZ"

func (h *Hub) newCodeLocked() string {
	for {
		b := make([]byte, 4)
		_, _ = rand.Read(b)
		for i := range b {
			b[i] = codeLetters[int(b[i])%len(codeLetters)]
		}
		if c := string(b); h.byCode[c] == "" {
			return c
		}
	}
}

// NormalizeCode upper-cases a typed code and drops spaces and dashes.
func NormalizeCode(s string) string {
	s = strings.ToUpper(s)
	return strings.Map(func(r rune) rune {
		if r == ' ' || r == '-' {
			return -1
		}
		return r
	}, s)
}

func randomID() string {
	const hex = "0123456789abcdef"
	b := make([]byte, 12)
	_, _ = rand.Read(b)
	out := make([]byte, 24)
	for i, v := range b {
		out[i*2] = hex[v>>4]
		out[i*2+1] = hex[v&15]
	}
	return string(out)
}
