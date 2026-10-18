package party

import (
	"context"
	"math"
	"testing"
	"time"
)

type clock struct{ t time.Time }

func (c *clock) now() time.Time      { return c.t }
func (c *clock) add(d time.Duration) { c.t = c.t.Add(d) }
func near(a, b float64) bool         { return math.Abs(a-b) < 1e-6 }
func dev(id, name string) Device     { return Device{ID: id, Name: name, User: "u"} }
func newHub() (*Hub, *clock)         { c := &clock{t: time.UnixMilli(1_700_000_000_000)}; return New(c.now), c }
func ready(h *Hub, id, d string, item string) {
	_, _ = h.Report(id, d, Report{ItemID: item, Ready: true})
}

func TestLineMath(t *testing.T) {
	s := State{Playing: true, Position: 10, Anchor: 1000, Rate: 1}
	if !near(s.At(500), 10) {
		t.Fatalf("before the anchor it waits at the start: %v", s.At(500))
	}
	if !near(s.At(3500), 12.5) {
		t.Fatalf("2.5 s after the anchor: %v", s.At(3500))
	}
	s.Playing = false
	if !near(s.At(9999), 10) {
		t.Fatal("paused stays put")
	}
}

func TestCreateKeepsHostPlaying(t *testing.T) {
	h, c := newHub()
	p := h.Create(dev("a", "Phone"), "i7", "Spring", 42, true)
	if len(p.Code) != 4 || !p.State.Playing || p.State.Anchor != c.t.UnixMilli() || !near(p.State.Position, 42) {
		t.Fatalf("party: %+v", p)
	}
	c.add(3 * time.Second)
	if got := p.State.At(c.t.UnixMilli()); !near(got, 45) {
		t.Fatalf("host keeps going without a hitch: %v", got)
	}
}

func TestJoinByCodeAndInvite(t *testing.T) {
	h, _ := newHub()
	p := h.Create(dev("a", "Phone"), "i7", "Spring", 0, false)
	if _, err := h.Join(dev("b", "TV"), "", "x y z q"); err != ErrNoCode {
		t.Fatalf("wrong code: %v", err)
	}
	lower := ""
	for _, r := range p.Code {
		lower += string(r + 32)
	}
	if _, err := h.Join(dev("b", "TV"), "", lower[:2]+"-"+lower[2:]); err != nil {
		t.Fatalf("code is case and dash tolerant: %v", err)
	}
	if _, err := h.InviteDevices(p.ID, dev("a", "Phone"), []string{"c", "b"}); err != nil {
		t.Fatal(err)
	}
	mine, invites := h.Mine("c")
	if mine != nil || len(invites) != 1 || invites[0].Invited[0].From != "Phone" {
		t.Fatalf("c sees the invite: %v %+v", mine, invites)
	}
	if _, invites := h.Mine("b"); len(invites) != 0 {
		t.Fatal("members aren't invited")
	}
	if _, err := h.Join(dev("c", "Tablet"), p.ID, ""); err != nil {
		t.Fatal(err)
	}
	if _, invites := h.Mine("c"); len(invites) != 0 {
		t.Fatal("joining clears the invite")
	}
}

func TestPlayIsScheduledAhead(t *testing.T) {
	h, c := newHub()
	p := h.Create(dev("a", "Phone"), "i7", "Spring", 30, false)
	_, _ = h.Join(dev("b", "TV"), p.ID, "")
	ready(h, p.ID, "b", "i7")
	got, err := h.Do(p.ID, "a", Control{Action: "play"})
	if err != nil {
		t.Fatal(err)
	}
	want := c.t.UnixMilli() + Lead.Milliseconds()
	if !got.State.Playing || got.State.Anchor != want || !near(got.State.Position, 30) || got.State.Hold != "" {
		t.Fatalf("everyone ready: starts together one lead from now: %+v", got.State)
	}
}

func TestPlayWaitsForANewcomer(t *testing.T) {
	h, c := newHub()
	p := h.Create(dev("a", "Phone"), "i7", "Spring", 30, false)
	_, _ = h.Join(dev("b", "TV"), p.ID, "")
	got, _ := h.Do(p.ID, "a", Control{Action: "play"})
	if got.State.Playing {
		t.Fatal("the TV hasn't opened the title yet")
	}
	c.add(2500 * time.Millisecond)
	h.Tick()
	got, _ = h.Get(p.ID, "a")
	if got.State.Hold != "Waiting for TV" {
		t.Fatalf("after a moment it names who it waits for: %q", got.State.Hold)
	}
	got, _ = h.Report(p.ID, "b", Report{ItemID: "i7", Ready: true})
	if !got.State.Playing || got.State.Anchor != c.t.UnixMilli()+Lead.Milliseconds() || !near(got.State.Position, 30) {
		t.Fatalf("starts once the TV is ready: %+v", got.State)
	}
}

func TestSeekGateAndTimeout(t *testing.T) {
	h, c := newHub()
	p := h.Create(dev("a", "Phone"), "i7", "Spring", 0, true)
	_, _ = h.Join(dev("b", "TV"), p.ID, "")
	got, _ := h.Do(p.ID, "a", Control{Action: "seek", Position: 600})
	if got.State.Playing || !near(got.State.Position, 600) || got.State.Hold != "Syncing" {
		t.Fatalf("a seek parks everyone at the new spot: %+v", got.State)
	}
	ready(h, p.ID, "a", "i7")
	c.add(GateTimeout + time.Millisecond)
	h.Tick()
	got, _ = h.Get(p.ID, "a")
	if !got.State.Playing || !near(got.State.Position, 600) {
		t.Fatalf("a slow device doesn't hold the party forever: %+v", got.State)
	}
}

func TestPausedSeekStaysPaused(t *testing.T) {
	h, _ := newHub()
	p := h.Create(dev("a", "Phone"), "i7", "Spring", 0, false)
	got, _ := h.Do(p.ID, "a", Control{Action: "seek", Position: 90})
	ready(h, p.ID, "a", "i7")
	got, _ = h.Get(p.ID, "a")
	if got.State.Playing || !near(got.State.Position, 90) || got.State.Hold != "" {
		t.Fatalf("seeking while paused stays paused: %+v", got.State)
	}
}

func TestPauseFreezesTheLine(t *testing.T) {
	h, c := newHub()
	p := h.Create(dev("a", "Phone"), "i7", "Spring", 10, true)
	c.add(4 * time.Second)
	got, _ := h.Do(p.ID, "a", Control{Action: "pause"})
	if got.State.Playing || !near(got.State.Position, 14) {
		t.Fatalf("pause lands where the line was: %+v", got.State)
	}
}

func TestStallHoldsEveryoneThenResumes(t *testing.T) {
	h, c := newHub()
	p := h.Create(dev("a", "Phone"), "i7", "Spring", 100, true)
	_, _ = h.Join(dev("b", "TV"), p.ID, "")
	c.add(5 * time.Second)
	got, _ := h.Report(p.ID, "b", Report{ItemID: "i7", Stalled: true, Playing: true})
	if got.State.Playing || !near(got.State.Position, 105) || got.State.Hold != "Waiting for TV" {
		t.Fatalf("a stall pauses everyone where the line is: %+v", got.State)
	}
	c.add(time.Second)
	got, _ = h.Report(p.ID, "b", Report{ItemID: "i7", Ready: true})
	if !got.State.Playing || !near(got.State.Position, 105) || got.State.Anchor != c.t.UnixMilli()+Lead.Milliseconds() {
		t.Fatalf("resumes together from the same spot: %+v", got.State)
	}
}

func TestLoadNextTitle(t *testing.T) {
	h, c := newHub()
	p := h.Create(dev("a", "Phone"), "i7", "S1E1", 1300, true)
	_, _ = h.Join(dev("b", "TV"), p.ID, "")
	got, _ := h.Do(p.ID, "a", Control{Action: "load", ItemID: "i8", Title: "S1E2"})
	if got.ItemID != "i8" || got.State.Playing || got.State.Hold != "Loading" || !near(got.State.Position, 0) {
		t.Fatalf("load: %+v", got)
	}
	ready(h, p.ID, "a", "i7") // still on the old title: doesn't count
	ready(h, p.ID, "b", "i8")
	got, _ = h.Get(p.ID, "a")
	if got.State.Playing {
		t.Fatal("the phone hasn't opened the new title yet")
	}
	got, _ = h.Report(p.ID, "a", Report{ItemID: "i8", Ready: true})
	if !got.State.Playing || got.State.Anchor != c.t.UnixMilli()+Lead.Milliseconds() {
		t.Fatalf("both ready: %+v", got.State)
	}
}

func TestHostHandoverAndEnd(t *testing.T) {
	h, _ := newHub()
	p := h.Create(dev("a", "Phone"), "i7", "Spring", 0, false)
	_, _ = h.Join(dev("b", "TV"), p.ID, "")
	h.Leave(p.ID, "a")
	got, _ := h.Get(p.ID, "b")
	if len(got.Members) != 1 || !got.Members[0].Host {
		t.Fatalf("the next member hosts: %+v", got.Members)
	}
	h.Leave(p.ID, "b")
	if _, err := h.Get(p.ID, "b"); err != ErrNotFound {
		t.Fatal("an empty party ends")
	}
}

func TestQuietMembersDropAndUnblockTheGate(t *testing.T) {
	h, c := newHub()
	p := h.Create(dev("a", "Phone"), "i7", "Spring", 0, false)
	_, _ = h.Join(dev("b", "TV"), p.ID, "")
	_, _ = h.Do(p.ID, "a", Control{Action: "seek", Position: 50})
	for i := 0; i < 70; i++ { // the phone keeps checking in, the TV doesn't
		c.add(500 * time.Millisecond)
		ready(h, p.ID, "a", "i7")
		h.Tick()
	}
	got, _ := h.Get(p.ID, "a")
	if len(got.Members) != 1 {
		t.Fatalf("the silent TV left: %+v", got.Members)
	}
}

func TestJoiningAnotherPartyLeavesTheFirst(t *testing.T) {
	h, _ := newHub()
	p1 := h.Create(dev("a", "Phone"), "i7", "Spring", 0, false)
	p2 := h.Create(dev("b", "TV"), "i8", "Hero", 0, false)
	_, _ = h.Join(dev("c", "Tablet"), p1.ID, "")
	_, _ = h.Join(dev("c", "Tablet"), p2.ID, "")
	if got, _ := h.Get(p1.ID, "a"); len(got.Members) != 1 {
		t.Fatal("one party per device")
	}
}

func TestWaitWakesOnChange(t *testing.T) {
	h, _ := newHub()
	p := h.Create(dev("a", "Phone"), "i7", "Spring", 0, false)
	done := make(chan Party, 1)
	go func() {
		got, ok, err := h.Wait(context.Background(), p.ID, "a", p.Rev)
		if ok && err == nil {
			done <- got
		}
	}()
	time.Sleep(20 * time.Millisecond)
	_, _ = h.Do(p.ID, "a", Control{Action: "seek", Position: 12})
	select {
	case got := <-done:
		if got.Rev <= p.Rev || !near(got.State.Position, 12) {
			t.Fatalf("woke with the change: %+v", got.State)
		}
	case <-time.After(time.Second):
		t.Fatal("wait didn't wake")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
	defer cancel()
	cur, _ := h.Get(p.ID, "a")
	if _, ok, _ := h.Wait(ctx, p.ID, "a", cur.Rev); ok {
		t.Fatal("nothing new before the timeout")
	}
}
