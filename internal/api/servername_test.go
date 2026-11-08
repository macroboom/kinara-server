package api

import (
	"net/http"
	"os"
	"strings"
	"testing"
)

func TestServerName(t *testing.T) {
	admin, _ := remoteServer(t)
	health := func() string {
		var h struct{ Name string }
		lan(t, admin.base).do("GET", "/health", nil, &h)
		return h.Name
	}
	host, _ := os.Hostname()
	if got := health(); got != host {
		t.Fatalf("before: %q, want the computer's name %q", got, host)
	}

	var st struct {
		ServerName string `json:"serverName"`
	}
	if resp := admin.do("PUT", "/settings", map[string]any{"serverName": "  Living   room  "}, &st); resp.StatusCode != 200 || st.ServerName != "Living room" {
		t.Fatalf("set: %d %+v", resp.StatusCode, st)
	}
	if got := health(); got != "Living room" {
		t.Fatalf("after: %q", got)
	}
	// Other settings saved later keep it.
	admin.do("PUT", "/settings", map[string]any{"groupCollections": false}, &st)
	if st.ServerName != "Living room" || health() != "Living room" {
		t.Fatalf("kept: %+v", st)
	}
	if resp := admin.do("PUT", "/settings", map[string]any{"serverName": strings.Repeat("x", 41)}, nil); resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("too long: %d", resp.StatusCode)
	}
	// Empty goes back to the computer's name.
	admin.do("PUT", "/settings", map[string]any{"serverName": ""}, nil)
	if got := health(); got != host {
		t.Fatalf("cleared: %q", got)
	}
}
