package api

import (
	"net"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestWebhookTargets(t *testing.T) {
	for _, c := range []struct {
		ip               string
		forbidden, local bool
	}{
		{"169.254.169.254", true, false},
		{"fe80::1", true, false},
		{"224.0.0.1", true, false},
		{"0.0.0.0", true, false},
		{"127.0.0.1", false, true},
		{"192.168.1.20", false, true},
		{"10.0.0.5", false, true},
		{"8.8.8.8", false, false},
	} {
		ip := net.ParseIP(c.ip)
		if forbiddenTarget(ip) != c.forbidden || localTarget(ip) != c.local {
			t.Errorf("%s: forbidden %v local %v", c.ip, forbiddenTarget(ip), localTarget(ip))
		}
	}
}

// The dialer refuses link-local always, and local addresses when told to.
func TestWebhookDialer(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	defer srv.Close()
	if _, err := webhookDialer(true).Dial("tcp", srv.Listener.Addr().String()); err != nil {
		t.Fatalf("local allowed: %v", err)
	}
	if _, err := webhookDialer(false).Dial("tcp", srv.Listener.Addr().String()); err == nil {
		t.Fatal("local refused, but it connected")
	}
	if _, err := webhookDialer(true).Dial("tcp", "169.254.169.254:80"); err == nil {
		t.Fatal("metadata address allowed")
	}
}

// From the internet, a webhook can't be pointed at the home network.
func TestWebhookFromInternet(t *testing.T) {
	home := httptest.NewRequest("POST", "/", nil)
	home.RemoteAddr = "192.168.1.9:5000"
	away := httptest.NewRequest("POST", "/", nil)
	away.RemoteAddr = "203.0.113.7:5000"
	if err := checkWebhookFrom(home, "http://192.168.1.20:8123/api/webhook/x"); err != nil {
		t.Fatal(err)
	}
	if err := checkWebhookFrom(away, "http://192.168.1.20:8123/api/webhook/x"); err == nil {
		t.Fatal("internet request pointed a webhook at the home network")
	}
	if err := checkWebhookFrom(away, "http://localhost:8420/"); err == nil {
		t.Fatal("internet request pointed a webhook at localhost")
	}
	if err := checkWebhookFrom(away, "https://203.0.113.50/hook"); err != nil {
		t.Fatal(err)
	}
}

func TestRedirectDowngrade(t *testing.T) {
	first, _ := http.NewRequest("POST", "https://hooks.example/a", nil)
	down, _ := http.NewRequest("POST", "http://hooks.example/b", nil)
	if sameHostRedirects(down, []*http.Request{first}) == nil {
		t.Fatal("followed https to http")
	}
}
