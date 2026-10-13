package auth

import (
	"testing"
	"time"
)

func TestPasswordRoundTrip(t *testing.T) {
	h, err := HashPassword("correct horse")
	if err != nil {
		t.Fatal(err)
	}
	if !CheckPassword(h, "correct horse") {
		t.Error("correct password rejected")
	}
	if CheckPassword(h, "wrong horse") {
		t.Error("wrong password accepted")
	}
	h2, _ := HashPassword("correct horse")
	if h == h2 {
		t.Error("hashes should be salted")
	}
	if CheckPassword("garbage", "x") {
		t.Error("malformed hash accepted")
	}
}

func TestWeakPassword(t *testing.T) {
	if _, err := HashPassword("short"); err != ErrWeakPassword {
		t.Errorf("got %v", err)
	}
}

func TestToken(t *testing.T) {
	tok, hash, err := NewToken()
	if err != nil || len(tok) < 40 || HashToken(tok) != hash {
		t.Fatalf("token %q hash %q err %v", tok, hash, err)
	}
}

func TestLimiter(t *testing.T) {
	l := &Limiter{Max: 3, Window: time.Minute}
	for i := 0; i < 3; i++ {
		if err := l.Allow("ip", "user"); err != nil {
			t.Fatalf("attempt %d refused", i)
		}
		l.Fail("ip", "user")
	}
	if l.Allow("ip") == nil {
		t.Error("fourth attempt should be refused")
	}
	if l.Allow("other-ip") != nil {
		t.Error("other keys unaffected")
	}
	l.Reset("ip", "user")
	if l.Allow("ip", "user") != nil {
		t.Error("reset should allow again")
	}
}
