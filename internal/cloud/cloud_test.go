package cloud

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

// sign makes a token the way kinara-cloud does.
func sign(key ed25519.PrivateKey, c map[string]any) string {
	body, _ := json.Marshal(c)
	head := "kc1." + base64.RawURLEncoding.EncodeToString(body)
	return head + "." + base64.RawURLEncoding.EncodeToString(ed25519.Sign(key, []byte(head)))
}

func TestVerifyAccess(t *testing.T) {
	pub, key, _ := ed25519.GenerateKey(rand.Reader)
	_, other, _ := ed25519.GenerateKey(rand.Reader)
	pubB64 := base64.StdEncoding.EncodeToString(pub)
	m := &Manager{}
	m.state = &State{ServerID: "0123456789abcdef", Owner: Owner{ID: "acc1"}, OwnerUserID: 7, CloudKey: pubB64}
	good := map[string]any{"typ": "server-access", "sub": "acc1", "email": "m@example.com", "sid": "0123456789abcdef", "role": "owner", "exp": time.Now().Add(time.Hour).Unix()}

	uid, owner, err := m.VerifyAccess(sign(key, good))
	if err != nil || uid != 7 || owner.Email != "m@example.com" {
		t.Fatalf("good token: %d %+v %v", uid, owner, err)
	}
	cases := map[string]string{
		"other key": sign(other, good),
		"garbage":   "kc1.e30.AAAA",
	}
	for k, v := range map[string]any{"sid": "ffffffffffffffff", "sub": "someone-else", "role": "friend", "exp": time.Now().Add(-time.Minute).Unix(), "typ": "other"} {
		c := map[string]any{}
		for a, b := range good {
			c[a] = b
		}
		c[k] = v
		cases["bad "+k] = sign(key, c)
	}
	for name, tok := range cases {
		if _, _, err := m.VerifyAccess(tok); err == nil {
			t.Errorf("%s accepted", name)
		}
	}
	m.state = nil
	if _, _, err := m.VerifyAccess(sign(key, good)); err == nil {
		t.Error("accepted while unlinked")
	}
}

func TestRelayAndInternetMarks(t *testing.T) {
	var seen *http.Request
	h := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { seen = r })

	req := httptest.NewRequest("GET", "/api/v1/health", nil)
	req.Header.Set("X-Forwarded-For", "198.51.100.4")
	relayHandler(h).ServeHTTP(httptest.NewRecorder(), req)
	if !ViaRelay(seen) || !FromInternet(seen) || seen.RemoteAddr != "198.51.100.4:0" {
		t.Errorf("relay: via=%v internet=%v addr=%s", ViaRelay(seen), FromInternet(seen), seen.RemoteAddr)
	}
	for addr, internet := range map[string]bool{"192.168.1.5:5000": false, "100.101.2.3:1": false, "[fd00::5]:1": false, "203.0.113.9:4000": true, "[2001:db8::1]:1": true} {
		req = httptest.NewRequest("GET", "/", nil)
		req.RemoteAddr = addr
		httpsHandler(h).ServeHTTP(httptest.NewRecorder(), req)
		if FromInternet(seen) != internet || ViaRelay(seen) {
			t.Errorf("%s: internet=%v", addr, FromInternet(seen))
		}
	}
}

// A fake router answering NAT-PMP on 127.0.0.2:5351.
func TestNATPMP(t *testing.T) {
	gw := net.ParseIP("127.0.0.2")
	pc, err := net.ListenUDP("udp4", &net.UDPAddr{IP: gw, Port: 5351})
	if err != nil {
		t.Skip("can't bind the NAT-PMP port here:", err)
	}
	defer pc.Close()
	go func() {
		buf := make([]byte, 64)
		for {
			n, from, err := pc.ReadFromUDP(buf)
			if err != nil {
				return
			}
			switch {
			case n == 2 && buf[1] == 0:
				out := []byte{0, 128, 0, 0, 0, 0, 0, 1, 203, 0, 113, 9}
				pc.WriteToUDP(out, from)
			case n == 12 && buf[1] == 2:
				out := make([]byte, 16)
				out[1] = 130
				copy(out[8:10], buf[4:6])     // internal
				out[10], out[11] = 0x7e, 0x8b // external 32395: the router picked another port
				binary.BigEndian.PutUint32(out[12:], 3600)
				pc.WriteToUDP(out, from)
			}
		}
	}()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	mp, err := natpmpMap(ctx, gw, 32443, 32443, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if mp.ExternalIP != "203.0.113.9" || mp.Internal != 32443 || mp.External != 32395 || mp.Lifetime != time.Hour || mp.Method != "natpmp" {
		t.Errorf("mapping %+v", mp)
	}
}

type countingMapper struct{ maps int }

func (c *countingMapper) Map(ctx context.Context, internal, external int, lifetime time.Duration) (*Mapping, error) {
	c.maps++
	return &Mapping{Method: "natpmp", External: external, Internal: internal, Lifetime: time.Hour}, nil
}
func (c *countingMapper) Unmap(ctx context.Context, m *Mapping) {}

// A router lease is renewed at half its life, whatever else is going on
// (presence reports every minute used to keep putting it off for ever).
func TestMappingRenewed(t *testing.T) {
	cm := &countingMapper{}
	m := &Manager{S: Settings{HTTPSPort: 8443}, mapper: cm, Log: slog.New(slog.DiscardHandler)}
	m.refreshMapping(context.Background())
	m.reported = time.Now() // a presence report just went out
	m.refreshMapping(context.Background())
	if cm.maps != 1 {
		t.Fatalf("mapped %d times, want 1 (fresh lease)", cm.maps)
	}
	m.mapping.MadeAt = time.Now().Add(-31 * time.Minute)
	m.reported = time.Now()
	m.refreshMapping(context.Background())
	if cm.maps != 2 {
		t.Fatalf("mapped %d times, want a renewal past half-life", cm.maps)
	}
}
