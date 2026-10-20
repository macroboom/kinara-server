package api

import (
	"net/http/httptest"
	"testing"
)

func TestClientIP(t *testing.T) {
	proxies := parseProxies("127.0.0.1, 10.0.0.0/8, ::1")
	cases := []struct {
		name, remote, xff, want string
		trust                   bool
	}{
		{"direct", "203.0.113.9:5000", "", "203.0.113.9", true},
		{"header from a stranger is ignored", "203.0.113.9:5000", "192.168.1.5", "203.0.113.9", true},
		{"behind the proxy", "127.0.0.1:5000", "198.51.100.7", "198.51.100.7", true},
		{"client-written hops are skipped", "127.0.0.1:5000", "192.168.1.5, 198.51.100.7", "198.51.100.7", true},
		{"through two trusted proxies", "127.0.0.1:5000", "198.51.100.7, 10.1.2.3", "198.51.100.7", true},
		{"proxy without a header", "127.0.0.1:5000", "", "127.0.0.1", true},
		{"no proxies trusted", "127.0.0.1:5000", "198.51.100.7", "127.0.0.1", false},
		{"ipv6 proxy", "[::1]:5000", "2001:db8::5", "2001:db8::5", true},
	}
	for _, c := range cases {
		r := httptest.NewRequest("GET", "/", nil)
		r.RemoteAddr = c.remote
		if c.xff != "" {
			r.Header.Set("X-Forwarded-For", c.xff)
		}
		p := proxies
		if !c.trust {
			p = nil
		}
		if got := clientIPWith(r, p); got != c.want {
			t.Errorf("%s: got %s, want %s", c.name, got, c.want)
		}
	}
}

func TestFromInternetByAddress(t *testing.T) {
	for addr, want := range map[string]bool{
		"192.168.1.20:1": false, "10.0.0.5:1": false, "127.0.0.1:1": false,
		"100.101.102.103:1": false, // Tailscale
		"[fd00::1]:1":       false,
		"203.0.113.9:1":     true, "[2001:db8::1]:1": true,
	} {
		r := httptest.NewRequest("GET", "/", nil)
		r.RemoteAddr = addr
		if got := fromInternet(r); got != want {
			t.Errorf("%s: fromInternet = %v, want %v", addr, got, want)
		}
	}
}
