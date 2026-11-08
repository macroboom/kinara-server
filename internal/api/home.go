package api

import (
	"bufio"
	"encoding/hex"
	"net"
	"net/http"
	"os"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"kinara/internal/auth"
	"kinara/internal/store"
)

// Signing in on the home network without a password.
//
// On the home network, a TV or phone signs in by picking an account: no
// password (the owner can ask for a server-wide PIN instead, for a shared
// network). That session is "home only": it watches, but it's refused
// anywhere other than the home network, can't manage the server (the
// owner pages need the password or a Kinara account), and can't approve
// another device's sign-in. Watching away from home goes through a Kinara
// account.
//
// "The home network" is strict here, since it now stands in for a
// password: the request must come straight from a private address (see
// localAddress), not through the tunnel or the cloud's relay, not through
// a reverse proxy the owner hasn't named in KINARA_TRUSTED_PROXIES (one
// on this computer would make every visitor look local), and, in a
// container, not from the container network's gateway (Docker Desktop
// and userland port forwarding make every connection, the internet's
// included, look like it comes from there).

// proxyHeaders are written by reverse proxies and tunnels (Cloudflare's
// among them). From anyone but a trusted proxy they mean "this didn't come
// straight from the person's device".
var proxyHeaders = []string{"X-Forwarded-For", "Forwarded", "X-Real-Ip", "Cf-Connecting-Ip", "True-Client-Ip"}

// homeNetwork reports whether r came straight from a device on the home
// network.
func homeNetwork(r *http.Request) bool {
	if fromInternet(r) {
		return false
	}
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		host = r.RemoteAddr
	}
	peer := net.ParseIP(host)
	if peer == nil {
		return false
	}
	if !isTrusted(peer, trustedProxies()) {
		for _, h := range proxyHeaders {
			if r.Header.Get(h) != "" {
				return false
			}
		}
	}
	ip := net.ParseIP(clientIP(r))
	if ip == nil || !homeAddress(ip) {
		return false
	}
	// A trusted proxy that didn't say who it's forwarding for.
	if ip.Equal(peer) && isTrusted(peer, trustedProxies()) {
		return false
	}
	if gw := containerGateway(); gw != nil && ip.Equal(gw) {
		return false
	}
	return true
}

// homeAddress is stricter than localAddress, since it stands in for a
// password: a private (10/8, 172.16/12, 192.168/16, fc00::/7), link-local
// or Tailscale (100.64/10) address. Not loopback: something on this
// computer, a reverse proxy or a port forwarder (ssh -R, frp, a TCP
// proxy), would make every visitor from the internet look like it. Not the
// server's own public IPv6 networks either: on a VPS or a campus network
// those are other people's machines (such a device signs in with a
// password, or connects over IPv4).
func homeAddress(ip net.IP) bool {
	if ip.IsLoopback() || ip.IsUnspecified() {
		return false
	}
	if ip.IsPrivate() || ip.IsLinkLocalUnicast() {
		return true
	}
	v4 := ip.To4()
	return v4 != nil && v4[0] == 100 && v4[1]&0xc0 == 64
}

// containerGateway is the default gateway when the server runs in a
// container (Docker, Podman), nil otherwise. Read once a minute.
func containerGateway() net.IP {
	gateway.Lock()
	defer gateway.Unlock()
	if time.Since(gateway.at) > time.Minute {
		gateway.ip = nil
		if inContainer() {
			gateway.ip = defaultGateway("/proc/net/route")
		}
		gateway.at = time.Now()
	}
	return gateway.ip
}

var gateway struct {
	sync.Mutex
	at time.Time
	ip net.IP
}

func inContainer() bool {
	for _, f := range []string{"/.dockerenv", "/run/.containerenv"} {
		if _, err := os.Stat(f); err == nil {
			return true
		}
	}
	return false
}

// defaultGateway reads the IPv4 default route from a /proc/net/route file
// (columns: Iface Destination Gateway ..., addresses in little-endian hex).
func defaultGateway(path string) net.IP {
	f, err := os.Open(path)
	if err != nil {
		return nil
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		fields := strings.Fields(sc.Text())
		if len(fields) < 3 || fields[1] != "00000000" {
			continue
		}
		b, err := hex.DecodeString(fields[2])
		if err != nil || len(b) != 4 {
			continue
		}
		return net.IPv4(b[3], b[2], b[1], b[0])
	}
	return nil
}

// homeOptions is what the sign-in screen needs to offer home sign-in.
func (s *Server) homeOptions(r *http.Request, st store.Settings) (on, pin bool) {
	return st.HomeSignIn && homeNetwork(r), st.HasHomePIN
}

// homePINKey counts wrong server PINs from every address together.
const homePINKey = "homepin"

type homeSignInRequest struct {
	Username string `json:"username"`
	// Pin is the server's home sign-in PIN, when the owner set one.
	Pin string `json:"pin"`
	// ProfileID and ProfilePin pick who's watching; without them, the
	// account's first profile that has no PIN.
	ProfileID  int64       `json:"profileId"`
	ProfilePin string      `json:"profilePin"`
	DeviceName string      `json:"deviceName"`
	Device     *deviceInfo `json:"device"`
}

// homeSignIn handles POST /api/v1/auth/home: sign in by picking an
// account, on the home network. It answers like /auth/login.
func (s *Server) homeSignIn(w http.ResponseWriter, r *http.Request) {
	st, err := s.Store.GetSettings(r.Context())
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	if !st.HomeSignIn {
		writeJSON(w, http.StatusForbidden, map[string]string{"error": "this server asks for a password to sign in", "code": "home_sign_in_off"})
		return
	}
	if !homeNetwork(r) {
		writeJSON(w, http.StatusForbidden, map[string]string{
			"error": "signing in without a password only works on the server's home network; away from home, sign in with your Kinara account",
			"code":  "not_home",
		})
		return
	}
	key := auth.Key("ip:", clientIP(r))
	if err := s.limiter.Allow(key); err != nil {
		writeErr(w, http.StatusTooManyRequests, err.Error())
		return
	}
	var req homeSignInRequest
	if err := readJSON(r, &req); err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	if st.HasHomePIN {
		// Counted for every address together too: a device can take
		// new addresses on its network at will.
		if err := s.limiter.Allow(homePINKey); err != nil {
			writeErr(w, http.StatusTooManyRequests, err.Error())
			return
		}
		hash, err := s.Store.HomePINHash(r.Context())
		if err != nil {
			writeErr(w, http.StatusInternalServerError, err.Error())
			return
		}
		if !auth.CheckPassword(hash, req.Pin) {
			s.limiter.Fail(key, homePINKey)
			writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "wrong PIN", "code": "wrong_pin"})
			return
		}
	}
	u, err := s.Store.UserByName(r.Context(), strings.TrimSpace(req.Username))
	if err != nil {
		s.limiter.Fail(key)
		writeErr(w, http.StatusUnauthorized, "no such account")
		return
	}
	profiles, err := s.Store.Profiles(r.Context(), u.ID)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	if profiles == nil {
		profiles = []store.Profile{}
	}
	p, ok := pickHomeProfile(profiles, req.ProfileID)
	if !ok {
		writeErr(w, http.StatusBadRequest, "that profile isn't on this account")
		return
	}
	if p == nil || p.HasPin {
		if p == nil || req.ProfilePin == "" {
			// Every profile has a PIN: the app asks which one, and its PIN.
			writeJSON(w, http.StatusForbidden, map[string]any{
				"error": "pick a profile and enter its PIN", "code": "profile_pin", "profiles": profiles,
			})
			return
		}
		pk := "pin:" + strconv.FormatInt(p.ID, 10)
		if err := s.limiter.Allow(pk); err != nil {
			writeErr(w, http.StatusTooManyRequests, err.Error())
			return
		}
		if !auth.CheckPassword(p.PinHash, req.ProfilePin) {
			s.limiter.Fail(pk, key)
			writeJSON(w, http.StatusForbidden, map[string]string{"error": "wrong PIN", "code": "wrong_profile_pin"})
			return
		}
		s.limiter.Reset(pk)
	}
	s.limiter.Reset(key)
	device := sessionDeviceName(req.DeviceName, req.Device)
	if device == "" {
		device = r.UserAgent()
	}
	if len(device) > 120 {
		device = device[:120]
	}
	token, hash, err := auth.NewToken()
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	if err := s.Store.CreateHomeSession(r.Context(), hash, u.ID, p.ID, device, time.Now().Add(sessionTTL).Unix()); err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	s.attachDevice(r, token, req.Device)
	u.ProfileID, u.HomeOnly = p.ID, true
	writeJSON(w, http.StatusOK, map[string]any{"token": token, "user": u, "profiles": profiles})
}

// pickHomeProfile is the profile a home sign-in watches as: the one asked
// for (false if it isn't on the account), else the first without a PIN
// (nil when they all have one).
func pickHomeProfile(profiles []store.Profile, id int64) (*store.Profile, bool) {
	if id != 0 {
		for i := range profiles {
			if profiles[i].ID == id {
				return &profiles[i], true
			}
		}
		return nil, false
	}
	for i := range profiles {
		if !profiles[i].HasPin {
			return &profiles[i], true
		}
	}
	return nil, true
}

var reHomePIN = regexp.MustCompile(`^[0-9]{4,8}$`)

// setHomePIN handles PUT /api/v1/settings/home-pin {pin}: "" removes it.
func (s *Server) setHomePIN(w http.ResponseWriter, r *http.Request, u store.User) {
	var req struct {
		Pin string `json:"pin"`
	}
	if err := readJSON(r, &req); err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	hash := ""
	if req.Pin != "" {
		if !reHomePIN.MatchString(req.Pin) {
			writeErr(w, http.StatusBadRequest, "the PIN is 4 to 8 digits")
			return
		}
		h, err := auth.HashPIN(req.Pin)
		if err != nil {
			writeErr(w, http.StatusInternalServerError, err.Error())
			return
		}
		hash = h
	}
	if err := s.Store.SetHomePINHash(r.Context(), hash); err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	// A new PIN signs out the devices that came in without it.
	if hash != "" {
		if err := s.Store.DeleteHomeSessions(r.Context()); err != nil {
			writeErr(w, http.StatusInternalServerError, err.Error())
			return
		}
	}
	w.WriteHeader(http.StatusNoContent)
}

// homeOnlyRefusal writes why a home-only session can't do this.
func homeOnlyRefusal(w http.ResponseWriter, status int, code, msg string) {
	writeJSON(w, status, map[string]string{"error": msg, "code": code})
}
