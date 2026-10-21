//go:build !nosqlite

package api

import (
	"context"
	"io"
	"log/slog"
	"net/http/httptest"
	"path/filepath"
	"testing"

	"kinara/internal/store"
)

// Quick Connect: a TV asks for a code, someone signed in approves it, and
// the TV's next poll signs it in as them.
func TestQuickConnect(t *testing.T) {
	st, err := store.Open(filepath.Join(t.TempDir(), "kinara.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	srv := &Server{Store: st, Log: log, Version: "test", BaseCtx: context.Background(), WebDir: t.TempDir()}
	hs := httptest.NewServer(srv.Handler())
	t.Cleanup(hs.Close)
	tv := &client{t: t, base: hs.URL + "/api/v1"}

	var made struct {
		Token string
		User  store.User
	}
	if resp := tv.do("POST", "/setup", map[string]string{"username": "alex", "password": "password123"}, &made); resp.StatusCode != 201 {
		t.Fatalf("setup: %d", resp.StatusCode)
	}
	phone := &client{t: t, base: tv.base, token: made.Token}

	var start struct {
		Code     string
		Secret   string
		Interval int
	}
	if resp := tv.do("POST", "/auth/quick/start", map[string]any{"deviceName": "Living Room TV"}, &start); resp.StatusCode != 200 {
		t.Fatalf("start: %d", resp.StatusCode)
	}
	if len(start.Code) != 6 || start.Secret == "" || start.Interval < 1 {
		t.Fatalf("start = %+v", start)
	}
	if resp := tv.do("POST", "/auth/quick/poll", map[string]string{"secret": start.Secret}, nil); resp.StatusCode != 202 {
		t.Fatalf("poll before approval: %d", resp.StatusCode)
	}

	// Approving needs a signed-in person.
	if resp := tv.do("POST", "/auth/quick/approve", map[string]string{"code": start.Code}, nil); resp.StatusCode != 401 {
		t.Fatalf("approve signed out: %d", resp.StatusCode)
	}
	var info struct{ Device string }
	if resp := phone.do("GET", "/auth/quick/"+start.Code, nil, &info); resp.StatusCode != 200 || info.Device != "Living Room TV" {
		t.Fatalf("info: %d %+v", resp.StatusCode, info)
	}
	wrong := "000000"
	if start.Code == wrong {
		wrong = "111111"
	}
	if resp := phone.do("POST", "/auth/quick/approve", map[string]string{"code": wrong}, nil); resp.StatusCode != 404 {
		t.Fatalf("wrong code: %d", resp.StatusCode)
	}
	if resp := phone.do("POST", "/auth/quick/approve", map[string]string{"code": start.Code[:3] + " " + start.Code[3:]}, nil); resp.StatusCode != 200 {
		t.Fatalf("approve: %d", resp.StatusCode)
	}

	var in struct {
		Token    string
		User     store.User
		Profiles []store.Profile
	}
	if resp := tv.do("POST", "/auth/quick/poll", map[string]string{"secret": start.Secret}, &in); resp.StatusCode != 200 {
		t.Fatalf("poll after approval: %d", resp.StatusCode)
	}
	if in.Token == "" || in.User.Username != "alex" || in.Profiles == nil {
		t.Fatalf("signed in = %+v", in)
	}
	signedIn := &client{t: t, base: tv.base, token: in.Token}
	if resp := signedIn.do("GET", "/me", nil, nil); resp.StatusCode != 200 {
		t.Fatalf("the TV's token: %d", resp.StatusCode)
	}
	// Collected once; the code is gone.
	if resp := tv.do("POST", "/auth/quick/poll", map[string]string{"secret": start.Secret}, nil); resp.StatusCode != 410 {
		t.Fatalf("poll again: %d", resp.StatusCode)
	}
	if resp := phone.do("GET", "/auth/quick/"+start.Code, nil, nil); resp.StatusCode != 404 {
		t.Fatalf("info after use: %d", resp.StatusCode)
	}

	// Cancelled codes can't be approved.
	tv.do("POST", "/auth/quick/start", map[string]any{"deviceName": "Bedroom TV"}, &start)
	if resp := tv.do("POST", "/auth/quick/cancel", map[string]string{"secret": start.Secret}, nil); resp.StatusCode != 204 {
		t.Fatalf("cancel: %d", resp.StatusCode)
	}
	if resp := phone.do("POST", "/auth/quick/approve", map[string]string{"code": start.Code}, nil); resp.StatusCode != 404 {
		t.Fatalf("approve after cancel: %d", resp.StatusCode)
	}
}
