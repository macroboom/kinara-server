package api

import (
	"errors"
	"net/http"
	"strings"
	"time"

	"kinara/internal/auth"
	"kinara/internal/quick"
	"kinara/internal/store"
)

// Quick Connect: a TV signs in without typing a password. It asks for a
// six-digit code (start) and shows it; someone signed in on a phone or in
// the web app enters it (approve), and the TV, asking every few seconds
// (poll), is signed in as them. Needs no Kinara account: it all happens on
// this server. See internal/quick.

// quickPoll is how often apps are asked to poll, in seconds.
const quickPoll = 3

// quickInfo is what start keeps for poll, to describe the new session.
type quickInfo struct {
	deviceName string
	device     *deviceInfo
}

func (s *Server) quickStart(w http.ResponseWriter, r *http.Request) {
	var req struct {
		DeviceName string      `json:"deviceName"`
		Device     *deviceInfo `json:"device"`
	}
	if err := readJSON(r, &req); err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	// Every start counts: a TV gets a new code every 10 minutes or when
	// asked, far below this.
	// Everything from the internet also shares one allowance: through the
	// tunnel a new "address" (node ID) costs nothing, and the codes waiting
	// at once are capped (quick.MaxPending), so outsiders mustn't be able
	// to use them all up and leave the TVs at home without one.
	keys := []string{auth.Key("start:", clientIP(r))}
	if fromInternet(r) {
		keys = append(keys, "start:internet")
	}
	if err := s.quickStarts.Allow(keys...); err != nil {
		writeErr(w, http.StatusTooManyRequests, "too many sign-in codes asked for; wait a few minutes")
		return
	}
	s.quickStarts.Fail(keys...)
	name := strings.TrimSpace(sessionDeviceName(req.DeviceName, req.Device))
	if name == "" {
		name = "A TV"
	}
	if len(name) > 80 {
		name = name[:80]
	}
	q, secret, err := s.quick.Start(name, quickInfo{deviceName: req.DeviceName, device: req.Device})
	if err != nil {
		status := http.StatusInternalServerError
		if errors.Is(err, quick.ErrFull) {
			status = http.StatusServiceUnavailable
		}
		writeErr(w, status, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"code":      q.Code,
		"secret":    secret,
		"expiresAt": q.Expires.Unix(),
		"interval":  quickPoll,
	})
}

// quickPollHandler answers 202 while waiting, 410 once the code is gone
// (expired, or the TV should ask for a new one), and the same as a
// password sign-in once approved.
func (s *Server) quickPollHandler(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Secret string `json:"secret"`
	}
	if err := readJSON(r, &req); err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	st, q := s.quick.Poll(strings.TrimSpace(req.Secret))
	switch st {
	case quick.Waiting:
		writeJSON(w, http.StatusAccepted, map[string]string{"status": "waiting"})
		return
	case quick.Gone:
		writeErr(w, http.StatusGone, "this code ran out; get a new one")
		return
	}
	u, err := s.Store.GetUser(r.Context(), q.UserID)
	if err != nil {
		writeErr(w, http.StatusGone, "the account that approved this code no longer exists; get a new code")
		return
	}
	info, _ := q.Info.(quickInfo)
	token, hash, err := auth.NewToken()
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	device := q.Device
	if len(device) > 120 {
		device = device[:120]
	}
	if err := s.Store.CreateSession(r.Context(), hash, u.ID, device, time.Now().Add(sessionTTL).Unix()); err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	s.attachDevice(r, token, info.device)
	s.noteNode(r, hash)
	s.noteSignIn(r, u, hash)
	s.Log.Info("signed in with a Quick Connect code", "user", u.Username, "device", device)
	profiles, _ := s.Store.Profiles(r.Context(), u.ID)
	if profiles == nil {
		profiles = []store.Profile{}
	}
	writeJSON(w, http.StatusOK, map[string]any{"token": token, "user": u, "profiles": profiles})
}

// quickCancel forgets a code the TV no longer shows.
func (s *Server) quickCancel(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Secret string `json:"secret"`
	}
	if err := readJSON(r, &req); err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	s.quick.Cancel(strings.TrimSpace(req.Secret))
	w.WriteHeader(http.StatusNoContent)
}

// quickKeys are the limiter keys for someone approving codes: wrong codes
// count against both the account and the address.
func quickKeys(r *http.Request, u store.User) []string {
	return []string{"approve:ip:" + clientIP(r), "approve:user:" + u.Username}
}

// quickInfoHandler says which device is waiting behind a code, so the
// person can check it's theirs before approving.
func (s *Server) quickInfoHandler(w http.ResponseWriter, r *http.Request, u store.User) {
	keys := quickKeys(r, u)
	if err := s.quickWrong.Allow(keys...); err != nil {
		writeErr(w, http.StatusTooManyRequests, "too many wrong codes; wait a few minutes")
		return
	}
	q, ok := s.quick.Pending(r.PathValue("code"))
	if !ok {
		s.quickWrong.Fail(keys...)
		writeErr(w, http.StatusNotFound, quick.ErrNotFound.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"code": q.Code, "device": q.Device, "expiresAt": q.Expires.Unix()})
}

// quickApprove signs the code's device in as the person approving it.
func (s *Server) quickApprove(w http.ResponseWriter, r *http.Request, u store.User) {
	var req struct {
		Code string `json:"code"`
	}
	if err := readJSON(r, &req); err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	keys := quickKeys(r, u)
	if err := s.quickWrong.Allow(keys...); err != nil {
		writeErr(w, http.StatusTooManyRequests, "too many wrong codes; wait a few minutes")
		return
	}
	q, err := s.quick.Approve(req.Code, u.ID)
	if err != nil {
		s.quickWrong.Fail(keys...)
		writeErr(w, http.StatusNotFound, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"device": q.Device})
}
