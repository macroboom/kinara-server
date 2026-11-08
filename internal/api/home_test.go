package api

import (
	"net/http"
	"os"
	"path/filepath"
	"testing"

	"kinara/internal/store"
)

// lan is a device on the home network (the test server's own clients are
// on 127.0.0.1, which doesn't count: see homeAddress).
func lan(t *testing.T, base string) *client {
	return &client{t: t, base: base, http: &http.Client{Transport: forwardedFor("192.168.1.50")}}
}

func homeSignIn(c *client, body map[string]any) (*http.Response, map[string]any) {
	var out map[string]any
	resp := c.do("POST", "/auth/home", body, &out)
	return resp, out
}

func TestHomeSignIn(t *testing.T) {
	admin, _ := remoteServer(t)
	home := lan(t, admin.base)

	// The sign-in screen offers it on the home network.
	var acc struct {
		HomeSignIn bool `json:"homeSignIn"`
		HomePin    bool `json:"homePin"`
	}
	home.do("GET", "/auth/accounts", nil, &acc)
	if !acc.HomeSignIn || acc.HomePin {
		t.Fatalf("accounts: %+v", acc)
	}

	// Picking the account signs in, with no password.
	resp, out := homeSignIn(home, map[string]any{"username": "alex", "deviceName": "Living room TV"})
	if resp.StatusCode != 200 || out["token"] == "" {
		t.Fatalf("home sign-in: %d %v", resp.StatusCode, out)
	}
	tv := lan(t, admin.base)
	tv.token = out["token"].(string)
	var me store.User
	if r := tv.do("GET", "/me", nil, &me); r.StatusCode != 200 || !me.HomeOnly || me.ProfileID == 0 || me.IsAdmin {
		t.Fatalf("me: %d %+v", r.StatusCode, me)
	}
	// It watches...
	if r := tv.do("GET", "/libraries", nil, nil); r.StatusCode != 200 {
		t.Fatalf("libraries: %d", r.StatusCode)
	}
	// ...but can't manage the server, even on the owner's account...
	var refusal map[string]string
	if r := tv.do("GET", "/settings", nil, &refusal); r.StatusCode != 403 || refusal["code"] != "owner_sign_in" {
		t.Fatalf("settings: %d %v", r.StatusCode, refusal)
	}
	// ...or approve another device's sign-in.
	if r := tv.do("POST", "/auth/quick/approve", map[string]string{"code": "123456"}, &refusal); r.StatusCode != 403 || refusal["code"] != "password_needed" {
		t.Fatalf("quick approve: %d %v", r.StatusCode, refusal)
	}
	// Away from home, the same token is refused.
	away := &client{t: t, base: admin.base, token: tv.token, http: &http.Client{Transport: forwardedFor("203.0.113.9")}}
	if r := away.do("GET", "/libraries", nil, &refusal); r.StatusCode != 401 || refusal["code"] != "home_only" {
		t.Fatalf("away: %d %v", r.StatusCode, refusal)
	}
	// A password sign-in still manages the server.
	if r := admin.do("GET", "/settings", nil, nil); r.StatusCode != 200 {
		t.Fatalf("owner settings: %d", r.StatusCode)
	}

	// Not from the internet, nor through a proxy nobody named.
	awayNew := &client{t: t, base: admin.base, http: &http.Client{Transport: forwardedFor("203.0.113.9")}}
	if r, out := homeSignIn(awayNew, map[string]any{"username": "alex"}); r.StatusCode != 403 || out["code"] != "not_home" {
		t.Fatalf("from the internet: %d %v", r.StatusCode, out)
	}
	var proxied map[string]any
	if r := home.do("POST", "/auth/home", map[string]any{"username": "alex"}, &proxied, "X-Forwarded-For", "203.0.113.9"); r.StatusCode != 403 {
		t.Fatalf("through a proxy: %d %v", r.StatusCode, proxied)
	}
	// Not from this computer either (a reverse proxy or port forwarder
	// here would make every visitor look like it).
	local := &client{t: t, base: admin.base}
	if r, out := homeSignIn(local, map[string]any{"username": "alex"}); r.StatusCode != 403 || out["code"] != "not_home" {
		t.Fatalf("from loopback: %d %v", r.StatusCode, out)
	}
	// A wrong name.
	if r, _ := homeSignIn(home, map[string]any{"username": "nobody"}); r.StatusCode != 401 {
		t.Fatalf("unknown name: %d", r.StatusCode)
	}

	// The owner can turn it off.
	if r := admin.do("PUT", "/settings", map[string]any{"homeSignIn": false}, nil); r.StatusCode != 200 {
		t.Fatalf("turn off: %d", r.StatusCode)
	}
	if r, out := homeSignIn(home, map[string]any{"username": "alex"}); r.StatusCode != 403 || out["code"] != "home_sign_in_off" {
		t.Fatalf("off: %d %v", r.StatusCode, out)
	}
	// Devices that came in that way are signed out.
	if r := tv.do("GET", "/libraries", nil, nil); r.StatusCode != 401 {
		t.Fatalf("home session after turning off: %d", r.StatusCode)
	}
	home.do("GET", "/auth/accounts", nil, &acc)
	if acc.HomeSignIn {
		t.Fatal("still offered when off")
	}
	admin.do("PUT", "/settings", map[string]any{"homeSignIn": true}, nil)
}

func TestHomeSignInPIN(t *testing.T) {
	admin, _ := remoteServer(t)
	home := lan(t, admin.base)

	if r := admin.do("PUT", "/settings/home-pin", map[string]string{"pin": "12"}, nil); r.StatusCode != 400 {
		t.Fatalf("short PIN: %d", r.StatusCode)
	}
	if r := admin.do("PUT", "/settings/home-pin", map[string]string{"pin": "2468"}, nil); r.StatusCode != 204 {
		t.Fatalf("set PIN: %d", r.StatusCode)
	}
	var st store.Settings
	admin.do("GET", "/settings", nil, &st)
	if !st.HasHomePIN {
		t.Fatal("settings don't show the PIN")
	}
	var acc struct{ HomePin bool }
	home.do("GET", "/auth/accounts", nil, &acc)
	if !acc.HomePin {
		t.Fatal("accounts don't ask for the PIN")
	}
	if r, out := homeSignIn(home, map[string]any{"username": "alex", "pin": "0000"}); r.StatusCode != 401 || out["code"] != "wrong_pin" {
		t.Fatalf("wrong PIN: %d %v", r.StatusCode, out)
	}
	r, out := homeSignIn(home, map[string]any{"username": "alex", "pin": "2468"})
	if r.StatusCode != 200 {
		t.Fatalf("right PIN: %d", r.StatusCode)
	}
	tv := lan(t, admin.base)
	tv.token = out["token"].(string)
	// A new PIN signs out the devices that came in before it.
	admin.do("PUT", "/settings/home-pin", map[string]string{"pin": "1111"}, nil)
	if r := tv.do("GET", "/libraries", nil, nil); r.StatusCode != 401 {
		t.Fatalf("after a new PIN: %d", r.StatusCode)
	}
	homeSignIn(home, map[string]any{"username": "alex", "pin": "1111"})
	// Wrong PINs count for every address together.
	for i := 0; i < 10; i++ {
		other := &client{t: t, base: admin.base, http: &http.Client{Transport: forwardedFor("192.168.1." + itoa(int64(100+i)))}}
		homeSignIn(other, map[string]any{"username": "alex", "pin": "9999"})
	}
	if r, _ := homeSignIn(lan(t, admin.base), map[string]any{"username": "alex", "pin": "1111"}); r.StatusCode != 429 {
		t.Fatalf("after 10 wrong PINs from 10 addresses: %d", r.StatusCode)
	}
	ownerStillManages(t, admin)
	// Saving other settings keeps it; an empty PIN removes it.
	admin.do("PUT", "/settings", map[string]any{"listAccounts": true, "hasHomePin": false}, &st)
	if !st.HasHomePIN {
		t.Fatal("PUT /settings removed the PIN")
	}
	admin.do("PUT", "/settings/home-pin", map[string]string{"pin": ""}, nil)
	if r, _ := homeSignIn(home, map[string]any{"username": "alex"}); r.StatusCode != 200 {
		t.Fatalf("after removing: %d", r.StatusCode)
	}
}

func TestHomeSignInProfiles(t *testing.T) {
	admin, _ := remoteServer(t)
	home := lan(t, admin.base)
	var list struct{ Profiles []store.Profile }
	admin.do("GET", "/profiles", nil, &list)
	first := list.Profiles[0]
	var kid store.Profile
	admin.do("POST", "/profiles", map[string]any{"name": "Kid"}, &kid)

	// The first profile gets a PIN: home sign-in watches as the next one.
	if r := admin.do("PATCH", "/profiles/"+itoa(first.ID), map[string]any{"pin": "1357"}, nil); r.StatusCode != 200 {
		t.Fatalf("set profile PIN: %d", r.StatusCode)
	}
	resp, out := homeSignIn(home, map[string]any{"username": "alex"})
	if resp.StatusCode != 200 || int64(out["user"].(map[string]any)["profileId"].(float64)) != kid.ID {
		t.Fatalf("default profile: %d %v", resp.StatusCode, out)
	}
	// Asking for the PIN profile needs its PIN.
	if r, out := homeSignIn(home, map[string]any{"username": "alex", "profileId": first.ID}); r.StatusCode != 403 || out["code"] != "profile_pin" {
		t.Fatalf("PIN profile without PIN: %d %v", r.StatusCode, out)
	}
	if r, out := homeSignIn(home, map[string]any{"username": "alex", "profileId": first.ID, "profilePin": "0000"}); r.StatusCode != 403 || out["code"] != "wrong_profile_pin" {
		t.Fatalf("wrong profile PIN: %d %v", r.StatusCode, out)
	}
	if r, _ := homeSignIn(home, map[string]any{"username": "alex", "profileId": first.ID, "profilePin": "1357"}); r.StatusCode != 200 {
		t.Fatalf("right profile PIN: %d", r.StatusCode)
	}
	// Another account's profile isn't accepted.
	if r, _ := homeSignIn(home, map[string]any{"username": "alex", "profileId": 99999}); r.StatusCode != 400 {
		t.Fatalf("foreign profile: %d", r.StatusCode)
	}
	// Deleting the profile it watches as doesn't fall back to the PIN one.
	resp, out = homeSignIn(home, map[string]any{"username": "alex"})
	tv := lan(t, admin.base)
	tv.token = out["token"].(string)
	var extra store.Profile
	admin.do("POST", "/profiles", map[string]any{"name": "Extra"}, &extra)
	if r := tv.do("DELETE", "/profiles/"+itoa(kid.ID), nil, nil); r.StatusCode != 204 {
		t.Fatalf("delete current: %d", r.StatusCode)
	}
	var me store.User
	if r := tv.do("GET", "/me", nil, &me); r.StatusCode != 200 || me.ProfileID != extra.ID {
		t.Fatalf("after deleting its profile: %d %+v (want profile %d)", r.StatusCode, me, extra.ID)
	}
	admin.do("PATCH", "/profiles/"+itoa(extra.ID), map[string]any{"pin": "8642"}, nil)
	if r := tv.do("GET", "/libraries", nil, nil); r.StatusCode != 401 {
		t.Fatalf("every profile locked, old session: %d", r.StatusCode)
	}
	kid = extra

	// Every profile locked: the app is told to ask.
	admin.do("PATCH", "/profiles/"+itoa(kid.ID), map[string]any{"pin": "2468", "currentPin": ""}, nil)
	if r, out := homeSignIn(home, map[string]any{"username": "alex"}); r.StatusCode != 403 || out["code"] != "profile_pin" || len(out["profiles"].([]any)) != 2 {
		t.Fatalf("all locked: %d %v", r.StatusCode, out)
	}
}

func TestDefaultGateway(t *testing.T) {
	p := filepath.Join(t.TempDir(), "route")
	os.WriteFile(p, []byte("Iface\tDestination\tGateway \tFlags\tRefCnt\tUse\tMetric\tMask\t\tMTU\tWindow\tIRTT\n"+
		"eth0\t00000000\t010011AC\t0003\t0\t0\t0\t00000000\t0\t0\t0\n"+
		"eth0\t000011AC\t00000000\t0001\t0\t0\t0\t0000FFFF\t0\t0\t0\n"), 0o644)
	if gw := defaultGateway(p); gw == nil || gw.String() != "172.17.0.1" {
		t.Fatalf("gateway %v", gw)
	}
	if defaultGateway(filepath.Join(t.TempDir(), "missing")) != nil {
		t.Fatal("missing file")
	}
}

// A session made with a password isn't home-only, whatever network it's on.
func TestPasswordSessionNotHomeOnly(t *testing.T) {
	admin, _ := remoteServer(t)
	var me store.User
	admin.do("GET", "/me", nil, &me)
	if me.HomeOnly {
		t.Fatal("password session marked home-only")
	}
}

// ownerStillManages checks the server PIN's limit doesn't touch the
// owner's password sign-in.
func ownerStillManages(t *testing.T, admin *client) {
	t.Helper()
	if r := admin.do("GET", "/settings", nil, nil); r.StatusCode != 200 {
		t.Fatalf("owner after PIN lockout: %d", r.StatusCode)
	}
}

// A home session can't use a profile's "account password" unlock to guess
// the password past the sign-in limit, by clearing the count with a PIN.
func TestProfilePasswordGuessLimit(t *testing.T) {
	admin, _ := remoteServer(t)
	home := lan(t, admin.base)
	_, out := homeSignIn(home, map[string]any{"username": "alex"})
	home.token = out["token"].(string)
	var p store.Profile
	home.do("POST", "/profiles", map[string]any{"name": "Mine", "pin": "1234"}, &p)
	path := "/profiles/" + itoa(p.ID)
	got429 := false
	for round := 0; round < 3 && !got429; round++ {
		for i := 0; i < 9; i++ {
			if r := home.do("PATCH", path, map[string]any{"name": "X", "password": "guess"}, nil); r.StatusCode == 429 {
				got429 = true
				break
			}
		}
		home.do("PATCH", path, map[string]any{"name": "X", "currentPin": "1234"}, nil)
	}
	if !got429 {
		t.Fatal("password guesses weren't limited")
	}
}
