package api

import (
	"net/http"
	"strings"
	"time"

	"kinara/internal/auth"
	"kinara/internal/cloud"
	"kinara/internal/remote"
	"kinara/internal/store"
)

// fromInternet: the request came from outside the home network, through
// the iroh tunnel, the cloud relay, or the direct HTTPS listener. Such
// requests can't create accounts, set the server up or list accounts.
func fromInternet(r *http.Request) bool {
	return remote.PeerFrom(r.Context()) != nil || cloud.FromInternet(r)
}

// cloudStatus: the manager's Remote access page reads this.
func (s *Server) cloudStatus(w http.ResponseWriter, r *http.Request, u store.User) {
	if s.Cloud == nil {
		writeJSON(w, http.StatusOK, map[string]any{"enabled": false})
		return
	}
	writeJSON(w, http.StatusOK, s.Cloud.Status())
}

// cloudLink shows a code to approve at the cloud's sign-in page. The admin
// who starts it is who the Kinara account signs in as.
func (s *Server) cloudLink(w http.ResponseWriter, r *http.Request, u store.User) {
	if s.Cloud == nil || !s.Cloud.Enabled() {
		writeErr(w, http.StatusNotImplemented, "remote access through a Kinara account isn't set up on this server (KINARA_CLOUD_URL)")
		return
	}
	st, err := s.Cloud.StartLink(r.Context(), u.ID)
	if err != nil {
		writeErr(w, http.StatusBadGateway, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, st)
}

func (s *Server) cloudCancelLink(w http.ResponseWriter, r *http.Request, u store.User) {
	if s.Cloud != nil {
		s.Cloud.CancelLink()
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) cloudUnlink(w http.ResponseWriter, r *http.Request, u store.User) {
	if s.Cloud == nil {
		w.WriteHeader(http.StatusNoContent)
		return
	}
	if err := s.Cloud.Unlink(r.Context()); err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// cloudSignIn turns a Kinara account token (from the cloud's
// /v1/servers/{id}/access) into a session on this server.
func (s *Server) cloudSignIn(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Token      string      `json:"token"`
		DeviceName string      `json:"deviceName"`
		Device     *deviceInfo `json:"device"`
	}
	if err := readJSON(r, &req); err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	if s.Cloud == nil {
		writeErr(w, http.StatusNotImplemented, "this server isn't linked to a Kinara account")
		return
	}
	keys := []string{"ip:" + clientIP(r)}
	if err := s.limiter.Allow(keys...); err != nil {
		writeErr(w, http.StatusTooManyRequests, err.Error())
		return
	}
	userID, owner, err := s.Cloud.VerifyAccess(strings.TrimSpace(req.Token))
	if err != nil {
		s.limiter.Fail(keys...)
		writeErr(w, http.StatusUnauthorized, err.Error())
		return
	}
	u, err := s.Store.GetUser(r.Context(), userID)
	if err != nil {
		writeErr(w, http.StatusUnauthorized, "the account this server was linked with no longer exists; link it again")
		return
	}
	token, hash, err := auth.NewToken()
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	device := sessionDeviceName(req.DeviceName, req.Device)
	if device == "" {
		device = r.UserAgent()
	}
	if len(device) > 120 {
		device = device[:120]
	}
	if err := s.Store.CreateSession(r.Context(), hash, u.ID, device, time.Now().Add(sessionTTL).Unix()); err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	s.attachDevice(r, token, req.Device)
	s.noteNode(r, hash)
	s.noteSignIn(r, u, hash)
	profiles, _ := s.Store.Profiles(r.Context(), u.ID)
	if profiles == nil {
		profiles = []store.Profile{}
	}
	writeJSON(w, http.StatusOK, map[string]any{"token": token, "user": u, "profiles": profiles, "account": owner})
}
