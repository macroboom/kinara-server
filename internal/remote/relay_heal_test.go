package remote

import (
	"io"
	"log/slog"
	"testing"
	"time"
)

func TestRelayKey(t *testing.T) {
	cases := map[string]string{
		"https://iroh.relay.example.com:8443/":  "iroh.relay.example.com:8443",
		"https://iroh.relay.example.com.:8443/": "iroh.relay.example.com:8443",
		"https://use1-1.relay.n0.iroh.link./":   "use1-1.relay.n0.iroh.link:443",
		"HTTPS://Relay.Example.com":             "relay.example.com:443",
	}
	for in, want := range cases {
		if got := relayKey(in); got != want {
			t.Errorf("relayKey(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestCheckRelaySwitchesBackToOurs(t *testing.T) {
	s := &Sidecar{Log: slog.New(slog.NewTextHandler(io.Discard, nil))}
	s.SetRelays(func() []string { return []string{"https://iroh.relay.example.com:8443"} })
	ours, public := "https://iroh.relay.example.com.:8443/", "https://use1-1.relay.n0.iroh.link./"
	st := func(u string) *Status { return &Status{Relay: RelayInfo{URL: &u}} }

	if s.checkRelay(st(ours)) {
		t.Fatal("our own relay is fine")
	}
	for i := 1; i < offRelayReports; i++ {
		if s.checkRelay(st(public)) {
			t.Fatalf("restarted after only %d reports", i)
		}
	}
	if !s.checkRelay(st(public)) {
		t.Fatal("should restart after reports from a public relay")
	}
	// Not again right away, even if it keeps happening.
	for i := 0; i < offRelayReports*2; i++ {
		if s.checkRelay(st(public)) {
			t.Fatal("restarted again within the cooldown")
		}
	}
	// Still on the public relay once the cooldown is over: the next report restarts it.
	s.lastHeal = time.Now().Add(-healEvery - time.Second)
	if !s.checkRelay(st(public)) {
		t.Fatal("should restart again after the cooldown")
	}

	// Not linked (no relays of our own): public relays are expected.
	s2 := &Sidecar{}
	for i := 0; i < offRelayReports*2; i++ {
		if s2.checkRelay(st(public)) {
			t.Fatal("without our relays, public ones are right")
		}
	}
}

func TestReloadOnlyWhenRelaysDiffer(t *testing.T) {
	s := &Sidecar{Log: slog.New(slog.NewTextHandler(io.Discard, nil))}
	s.init()
	stopped := 0
	s.enabled = true
	s.stop = func() { stopped++ }
	s.launchedRelays = "https://a:8443"
	s.SetRelays(func() []string { return []string{"https://a:8443"} })
	s.Reload()
	if stopped != 0 {
		t.Fatal("same relays: no restart")
	}
	s.SetRelays(func() []string { return nil })
	s.Reload()
	if stopped != 1 {
		t.Fatal("relays changed: restart")
	}
}
