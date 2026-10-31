package api

import (
	"errors"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"sync"

	"kinara/internal/auth"
	"kinara/internal/store"
)

// Local accounts: the sign-in screen can list them (if the owner allows)
// and make new ones (if the owner turned sign-up on); the owner manages
// them on the manager page.

var reUsername = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9 ._-]{0,31}$`)

func validUsername(name string) (string, error) {
	name = strings.TrimSpace(name)
	if !reUsername.MatchString(name) {
		return "", errors.New("a user name is 1 to 32 letters, digits, spaces, dots, dashes or underscores")
	}
	return name, nil
}

// accounts is public: what the sign-in screen offers.
func (s *Server) accounts(w http.ResponseWriter, r *http.Request) {
	st, err := s.Store.GetSettings(r.Context())
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	cards := []store.AccountCard{}
	// Over the tunnel, sign-in asks for a user name: listing accounts would
	// show them to anyone who has the server's ticket.
	remoteDevice := fromInternet(r)
	if st.ListAccounts && !remoteDevice {
		if cards, err = s.Store.AccountCards(r.Context()); err != nil {
			writeErr(w, http.StatusInternalServerError, err.Error())
			return
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{"accounts": cards, "allowSignup": st.AllowSignup && !remoteDevice})
}

type signupRequest struct {
	Username string `json:"username"`
	Password string `json:"password"`
	// DeviceName names the session, as on /auth/login.
	DeviceName string `json:"deviceName"`
	// Device describes the app signing in (see clients.go).
	Device *deviceInfo `json:"device"`
}

// signup makes an ordinary account when the owner allows it, and signs it in
// (answering like /auth/login).
func (s *Server) signup(w http.ResponseWriter, r *http.Request) {
	st, err := s.Store.GetSettings(r.Context())
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	if !st.AllowSignup {
		writeErr(w, http.StatusForbidden, "this server doesn't allow new accounts; ask its owner to add you")
		return
	}
	// New accounts only from the home network: remotely, the per-address
	// limit below can be dodged with a fresh node ID.
	if fromInternet(r) {
		writeErr(w, http.StatusForbidden, "new accounts can only be made on the server's home network")
		return
	}
	// Shares the sign-in limiter, so sign-up can't be used to hammer the server.
	if err := s.limiter.Allow("ip:" + clientIP(r)); err != nil {
		writeErr(w, http.StatusTooManyRequests, err.Error())
		return
	}
	var req signupRequest
	if err := readJSON(r, &req); err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	name, err := validUsername(req.Username)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	if _, err := s.createAccount(r, name, req.Password, false); err != nil {
		s.limiter.Fail("ip:" + clientIP(r))
		accountErr(w, err)
		return
	}
	token, u, err := s.signIn(r, name, req.Password, sessionDeviceName(req.DeviceName, req.Device))
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	s.attachDevice(r, token, req.Device)
	profiles, _ := s.Store.Profiles(r.Context(), u.ID)
	if profiles == nil {
		profiles = []store.Profile{}
	}
	writeJSON(w, http.StatusCreated, map[string]any{"token": token, "user": u, "profiles": profiles})
}

var errNameTaken = errors.New("that user name is taken")

func (s *Server) createAccount(r *http.Request, name, password string, admin bool) (store.User, error) {
	if _, err := s.Store.UserByName(r.Context(), name); err == nil {
		return store.User{}, errNameTaken
	}
	hash, err := auth.HashPassword(password)
	if err != nil {
		return store.User{}, err
	}
	nu, err := s.Store.CreateUser(r.Context(), name, hash, admin)
	if err == nil {
		s.noteUserCreated(nu)
	}
	return nu, err
}

func accountErr(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, errNameTaken):
		writeErr(w, http.StatusConflict, err.Error())
	case errors.Is(err, auth.ErrWeakPassword):
		writeErr(w, http.StatusBadRequest, err.Error())
	case errors.Is(err, store.ErrNotFound):
		writeErr(w, http.StatusNotFound, "account not found")
	default:
		writeErr(w, http.StatusInternalServerError, err.Error())
	}
}

// --- Owner: accounts and settings ---

func (s *Server) listUsers(w http.ResponseWriter, r *http.Request, u store.User) {
	users, err := s.Store.ListUsers(r.Context())
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, users)
}

type userRequest struct {
	Username string  `json:"username"`
	Password *string `json:"password"`
	IsAdmin  *bool   `json:"isAdmin"`
}

func (s *Server) createUserHandler(w http.ResponseWriter, r *http.Request, u store.User) {
	var req userRequest
	if err := readJSON(r, &req); err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	name, err := validUsername(req.Username)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	if req.Password == nil {
		writeErr(w, http.StatusBadRequest, "password is required")
		return
	}
	nu, err := s.createAccount(r, name, *req.Password, req.IsAdmin != nil && *req.IsAdmin)
	if err != nil {
		accountErr(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, nu)
}

func userID(r *http.Request) (int64, bool) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	return id, err == nil && id > 0
}

// updateUser sets a new password (signing the account out everywhere) or
// changes whether it's an owner. The last owner can't be demoted.
func (s *Server) updateUser(w http.ResponseWriter, r *http.Request, u store.User) {
	id, ok := userID(r)
	if !ok {
		writeErr(w, http.StatusBadRequest, "bad account id")
		return
	}
	var req userRequest
	if err := readJSON(r, &req); err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	target, err := s.Store.GetUser(r.Context(), id)
	if err != nil {
		accountErr(w, err)
		return
	}
	if req.IsAdmin != nil && !*req.IsAdmin && target.IsAdmin {
		if n, err := s.Store.CountAdmins(r.Context()); err != nil || n <= 1 {
			writeErr(w, http.StatusConflict, "the server needs at least one owner")
			return
		}
	}
	if req.Password != nil {
		hash, err := auth.HashPassword(*req.Password)
		if err != nil {
			accountErr(w, err)
			return
		}
		// Signed out everywhere, except here when it's your own password.
		keep := ""
		if id == u.ID {
			keep = sessionFrom(r).tokenHash
		}
		if err := s.Store.SetPassword(r.Context(), id, hash, keep); err != nil {
			accountErr(w, err)
			return
		}
	}
	if req.IsAdmin != nil {
		if err := s.Store.SetAdmin(r.Context(), id, *req.IsAdmin); err != nil {
			accountErr(w, err)
			return
		}
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) deleteUser(w http.ResponseWriter, r *http.Request, u store.User) {
	id, ok := userID(r)
	if !ok {
		writeErr(w, http.StatusBadRequest, "bad account id")
		return
	}
	if id == u.ID {
		writeErr(w, http.StatusConflict, "you can't remove your own account")
		return
	}
	if err := s.Store.DeleteUser(r.Context(), id); err != nil {
		accountErr(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) getSettings(w http.ResponseWriter, r *http.Request, u store.User) {
	st, err := s.Store.GetSettings(r.Context())
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, st)
}

func (s *Server) putSettings(w http.ResponseWriter, r *http.Request, u store.User) {
	// Fields left out keep their values, so pages that know only some
	// settings don't clear the rest.
	st, err := s.Store.GetSettings(r.Context())
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	if err := readJSON(r, &st); err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	if err := s.Store.PutSettings(r.Context(), st); err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, st)
}

// setupMu makes first-run setup one at a time, so two browsers racing on a
// fresh server can't both become the owner.
var setupMu sync.Mutex

// setup makes the owner account on a server that has no accounts yet, and
// signs that browser or app in. It answers like /auth/signup. Only from the
// home network: whoever makes the first account owns the server. Once any
// account exists it refuses (409), so it can't be used to add a second owner.
func (s *Server) setup(w http.ResponseWriter, r *http.Request) {
	if fromInternet(r) {
		writeErr(w, http.StatusForbidden, "set the server up from its home network")
		return
	}
	var req signupRequest
	if err := readJSON(r, &req); err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	name, err := validUsername(req.Username)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	setupMu.Lock()
	n, err := s.Store.CountUsers(r.Context())
	if err != nil {
		setupMu.Unlock()
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	if n > 0 {
		setupMu.Unlock()
		writeErr(w, http.StatusConflict, "this server is already set up; sign in instead")
		return
	}
	_, err = s.createAccount(r, name, req.Password, true)
	setupMu.Unlock()
	if err != nil {
		accountErr(w, err)
		return
	}
	token, u, err := s.signIn(r, name, req.Password, sessionDeviceName(req.DeviceName, req.Device))
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	s.attachDevice(r, token, req.Device)
	profiles, _ := s.Store.Profiles(r.Context(), u.ID)
	if profiles == nil {
		profiles = []store.Profile{}
	}
	writeJSON(w, http.StatusCreated, map[string]any{"token": token, "user": u, "profiles": profiles})
}
