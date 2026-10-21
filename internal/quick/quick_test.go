package quick

import (
	"testing"
	"time"
)

func TestFlow(t *testing.T) {
	c := New()
	r, secret, err := c.Start("Living Room TV", "info")
	if err != nil {
		t.Fatal(err)
	}
	if len(r.Code) != 6 || Clean(r.Code) != r.Code || secret == "" {
		t.Fatalf("start: %+v %q", r, secret)
	}
	if st, _ := c.Poll(secret); st != Waiting {
		t.Fatalf("poll before approval: %v", st)
	}
	if st, _ := c.Poll("kq_wrong"); st != Gone {
		t.Errorf("wrong secret: %v", st)
	}
	p, ok := c.Pending(r.Code[:3] + " " + r.Code[3:])
	if !ok || p.Device != "Living Room TV" {
		t.Fatalf("pending with a space: %+v %v", p, ok)
	}
	if _, err := c.Approve("000000x", 7); err == nil && r.Code != "000000" {
		t.Error("approved the wrong code")
	}
	if _, err := c.Approve(r.Code, 7); err != nil {
		t.Fatal(err)
	}
	if _, err := c.Approve(r.Code, 8); err != ErrNotFound {
		t.Errorf("approved twice: %v", err)
	}
	if _, ok := c.Pending(r.Code); ok {
		t.Error("still pending after approval")
	}
	st, got := c.Poll(secret)
	if st != Approved || got.UserID != 7 || got.Info != "info" {
		t.Fatalf("collect: %v %+v", st, got)
	}
	if st, _ := c.Poll(secret); st != Gone {
		t.Errorf("collected twice: %v", st)
	}
}

func TestExpiry(t *testing.T) {
	c := New()
	now := time.Unix(1_000_000, 0)
	c.Now = func() time.Time { return now }
	r, secret, _ := c.Start("TV", nil)
	now = now.Add(TTL)
	if _, err := c.Approve(r.Code, 1); err != ErrNotFound {
		t.Errorf("approved an expired code: %v", err)
	}
	if st, _ := c.Poll(secret); st != Gone {
		t.Errorf("expired poll: %v", st)
	}
	// An approval not collected in time is gone too.
	now = time.Unix(2_000_000, 0)
	r, secret, _ = c.Start("TV", nil)
	c.Approve(r.Code, 1)
	now = now.Add(TTL + time.Second)
	if st, _ := c.Poll(secret); st != Gone {
		t.Errorf("late collect: %v", st)
	}
	// Starting sweeps them out.
	c.Start("TV", nil)
	if len(c.byCode) != 1 || len(c.bySecret) != 1 {
		t.Errorf("not swept: %d %d", len(c.byCode), len(c.bySecret))
	}
}

func TestCancelAndLimit(t *testing.T) {
	c := New()
	r, secret, _ := c.Start("TV", nil)
	c.Cancel(secret)
	if _, ok := c.Pending(r.Code); ok {
		t.Error("pending after cancel")
	}
	for i := 0; i < MaxPending; i++ {
		if _, _, err := c.Start("TV", nil); err != nil {
			t.Fatalf("start %d: %v", i, err)
		}
	}
	if _, _, err := c.Start("TV", nil); err != ErrFull {
		t.Errorf("over the cap: %v", err)
	}
	seen := map[string]bool{}
	for code := range c.byCode {
		if seen[code] || len(code) != 6 {
			t.Fatalf("code %q", code)
		}
		seen[code] = true
	}
}
