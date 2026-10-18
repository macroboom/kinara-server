package api

import (
	"context"
	"errors"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"kinora/internal/party"
	"kinora/internal/store"
)

// Watch parties (see internal/party for how the timing works). Everything
// goes through this server, which every device already reaches (at home
// directly, away through remote access), so a party needs no cloud account.
//
// A device's ID is the one /devices shows: it comes from its session.

// partyState is the hub plus which devices are online, kept here so the
// Server type only grows by one field.
type partyState struct {
	once sync.Once
	hub  *party.Hub
	// seen is device ID -> last pulse (Unix ms), for the invite list.
	seen sync.Map
}

// onlineWindow is how recently a device must have checked in to be offered
// in the invite list. Apps pulse every 4 to 20 seconds.
const onlineWindow = 60 * time.Second

// partyWaitLimit is how long a long-poll waits for news before answering
// "nothing new" (204). Under every proxy timeout we know of.
const partyWaitLimit = 25 * time.Second

func (s *Server) parties() *party.Hub {
	s.party.once.Do(func() {
		s.party.hub = party.New(time.Now)
		ctx := s.BaseCtx
		if ctx == nil {
			ctx = context.Background()
		}
		go s.party.hub.Run(ctx)
	})
	return s.party.hub
}

func myDeviceID(r *http.Request) string { return store.DeviceID(sessionFrom(r).tokenHash) }

// markOnline records that this device just checked in (the pulse calls it).
func (s *Server) markOnline(r *http.Request) {
	if id := myDeviceID(r); id != "" {
		s.party.seen.Store(id, time.Now().UnixMilli())
	}
}

// partyDevice is how this device appears in a party: its own name (the one
// set on it, or the one it signed in with) and the account it's signed into.
func (s *Server) partyDevice(r *http.Request, u store.User) party.Device {
	id := myDeviceID(r)
	name := ""
	if ds, err := s.Store.Devices(r.Context()); err == nil {
		for _, d := range ds {
			if d.ID == id {
				name = d.Name
				break
			}
		}
	}
	if name == "" {
		name = s.Store.DeviceName(r.Context(), sessionFrom(r).tokenHash)
	}
	if name == "" {
		name = u.Username + "'s device"
	}
	return party.Device{ID: id, Name: name, User: u.Username}
}

func writePartyErr(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, party.ErrNotFound):
		writeErr(w, http.StatusGone, err.Error())
	case errors.Is(err, party.ErrNoCode):
		writeErr(w, http.StatusNotFound, err.Error())
	case errors.Is(err, party.ErrNotMember):
		writeErr(w, http.StatusForbidden, err.Error())
	case errors.Is(err, party.ErrFull):
		writeErr(w, http.StatusConflict, err.Error())
	default:
		writeErr(w, http.StatusBadRequest, err.Error())
	}
}

// serverTime answers GET /api/v1/time: the server clock, for apps to line
// their clocks up with. Kept tiny so its round trip is as short as can be.
func (s *Server) serverTime(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Content-Type", "application/json")
	t := float64(time.Now().UnixMicro()) / 1000
	_, _ = w.Write([]byte(`{"t":` + strconv.FormatFloat(t, 'f', 3, 64) + `}`))
}

// partyMine answers GET /api/v1/parties/mine: this device's party, the
// parties inviting it, and who this device is.
func (s *Server) partyMine(w http.ResponseWriter, r *http.Request, u store.User) {
	me := s.partyDevice(r, u)
	mine, invites := s.parties().Mine(me.ID)
	if invites == nil {
		invites = []party.Party{}
	}
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, http.StatusOK, map[string]any{"me": me, "party": mine, "invites": invites})
}

// partyDevices answers GET /api/v1/parties/devices: devices online now that
// could be invited (every account on this server, this device left out).
func (s *Server) partyDevices(w http.ResponseWriter, r *http.Request, u store.User) {
	ds, err := s.Store.Devices(r.Context())
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	me := myDeviceID(r)
	cutoff := time.Now().Add(-onlineWindow).UnixMilli()
	type onlineDevice struct {
		ID    string `json:"device"`
		Name  string `json:"name"`
		User  string `json:"user"`
		Label string `json:"label"`
		Icon  string `json:"icon"`
	}
	out := []onlineDevice{}
	for _, d := range ds {
		if d.ID == me {
			continue
		}
		at, ok := s.party.seen.Load(d.ID)
		if !ok || at.(int64) < cutoff {
			continue
		}
		out = append(out, onlineDevice{ID: d.ID, Name: d.Name, User: d.Username, Label: d.Label, Icon: d.Icon})
	}
	writeJSON(w, http.StatusOK, out)
}

// partyCreate answers POST /api/v1/parties: start a party around what this
// device is playing, optionally inviting devices straight away.
func (s *Server) partyCreate(w http.ResponseWriter, r *http.Request, u store.User) {
	var req struct {
		ItemID   string   `json:"itemId"`
		Title    string   `json:"title"`
		Position float64  `json:"position"`
		Playing  bool     `json:"playing"`
		Invite   []string `json:"invite"`
	}
	if err := readJSON(r, &req); err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	if strings.TrimSpace(req.ItemID) == "" {
		writeErr(w, http.StatusBadRequest, "a watch party needs a title to watch")
		return
	}
	me := s.partyDevice(r, u)
	p := s.parties().Create(me, req.ItemID, strings.TrimSpace(req.Title), req.Position, req.Playing)
	if len(req.Invite) > 0 {
		if q, err := s.parties().InviteDevices(p.ID, me, req.Invite); err == nil {
			p = q
		}
	}
	writeJSON(w, http.StatusCreated, p)
}

// partyJoin answers POST /api/v1/parties/join with {"code"} or {"id"} (an invite).
func (s *Server) partyJoin(w http.ResponseWriter, r *http.Request, u store.User) {
	var req struct {
		ID   string `json:"id"`
		Code string `json:"code"`
	}
	if err := readJSON(r, &req); err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	if req.ID == "" && strings.TrimSpace(req.Code) == "" {
		writeErr(w, http.StatusBadRequest, "enter the party's code")
		return
	}
	p, err := s.parties().Join(s.partyDevice(r, u), req.ID, req.Code)
	if err != nil {
		writePartyErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, p)
}

func (s *Server) partyLeave(w http.ResponseWriter, r *http.Request, u store.User) {
	s.parties().Leave(r.PathValue("id"), myDeviceID(r))
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) partyDecline(w http.ResponseWriter, r *http.Request, u store.User) {
	s.parties().Decline(r.PathValue("id"), myDeviceID(r))
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) partyInvite(w http.ResponseWriter, r *http.Request, u store.User) {
	var req struct {
		Devices []string `json:"devices"`
	}
	if err := readJSON(r, &req); err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	p, err := s.parties().InviteDevices(r.PathValue("id"), s.partyDevice(r, u), req.Devices)
	if err != nil {
		writePartyErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, p)
}

// partyControl answers POST /api/v1/parties/{id}/control: play, pause, seek
// or load, from any member.
func (s *Server) partyControl(w http.ResponseWriter, r *http.Request, u store.User) {
	var c party.Control
	if err := readJSON(r, &c); err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	p, err := s.parties().Do(r.PathValue("id"), myDeviceID(r), c)
	if err != nil {
		writePartyErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, p)
}

// partyReport answers POST /api/v1/parties/{id}/report: a member saying how
// it's doing. The answer is the party, so a report doubles as a poll.
func (s *Server) partyReport(w http.ResponseWriter, r *http.Request, u store.User) {
	var rep party.Report
	if err := readJSON(r, &rep); err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	p, err := s.parties().Report(r.PathValue("id"), myDeviceID(r), rep)
	if err != nil {
		writePartyErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, p)
}

// partyWait answers GET /api/v1/parties/{id}/wait?rev=N: holds the request
// until the party changes past N (200 with the party) or partyWaitLimit
// passes (204). 410 means the party ended.
func (s *Server) partyWait(w http.ResponseWriter, r *http.Request, u store.User) {
	rev, _ := strconv.ParseInt(r.URL.Query().Get("rev"), 10, 64)
	s.markOnline(r)
	ctx, cancel := context.WithTimeout(r.Context(), partyWaitLimit)
	defer cancel()
	p, ok, err := s.parties().Wait(ctx, r.PathValue("id"), myDeviceID(r), rev)
	w.Header().Set("Cache-Control", "no-store")
	if err != nil {
		writePartyErr(w, err)
		return
	}
	if !ok {
		w.WriteHeader(http.StatusNoContent)
		return
	}
	writeJSON(w, http.StatusOK, p)
}

// renameMyDevice answers PUT /api/v1/me/device/name: a device naming itself
// ("Living Room TV"), so it's easy to spot in invites. The name sticks
// across app restarts.
func (s *Server) renameMyDevice(w http.ResponseWriter, r *http.Request, u store.User) {
	var req struct {
		Name string `json:"name"`
	}
	if err := readJSON(r, &req); err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	name := strings.TrimSpace(req.Name)
	if name == "" || len(name) > 40 {
		writeErr(w, http.StatusBadRequest, "a device name needs 1 to 40 characters")
		return
	}
	hash := sessionFrom(r).tokenHash
	me := store.DeviceID(hash)
	if ds, err := s.Store.Devices(r.Context()); err == nil {
		for _, d := range ds {
			if d.ID == me && d.ClientID != "" {
				if _, err := s.Store.UpdateClient(r.Context(), d.ClientID, store.ClientUpdate{Name: &name}); err != nil {
					writeErr(w, http.StatusInternalServerError, err.Error())
					return
				}
				break
			}
		}
	}
	if err := s.Store.SetSessionDeviceName(r.Context(), hash, name); err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"device": me, "name": name})
}
