package api

import (
	"encoding/json"
	"errors"
	"net/http"
	"strings"

	"kinora/internal/auth"
	"kinora/internal/store"
)

// Device identity: apps describe themselves when they sign in, so the
// owner sees "Living room TV, NVIDIA SHIELD" instead of a bare name, and can
// trust a device (its requests skip the owner's approval).

// deviceInfo is the optional "device" object on sign-in bodies.
type deviceInfo struct {
	// ID is made by the app on install and kept: 8 to 64 of A-Z, a-z, 0-9,
	// "-" and "_".
	ID         string `json:"id"`
	Name       string `json:"name"`
	Platform   string `json:"platform"`
	Brand      string `json:"brand"`
	Model      string `json:"model"`
	OS         string `json:"os"`
	App        string `json:"app"`
	FormFactor string `json:"formFactor"`
}

// UnmarshalJSON reads the device object leniently: sign-in bodies refuse
// unknown fields, but a newer app adding a detail here shouldn't stop it
// signing in.
func (d *deviceInfo) UnmarshalJSON(b []byte) error {
	type plain deviceInfo
	var p plain
	if err := json.Unmarshal(b, &p); err != nil {
		return err
	}
	*d = deviceInfo(p)
	return nil
}

// deviceFieldMax keeps what apps report to a sensible size.
const deviceFieldMax = 120

func clip(s string, n int) string {
	s = strings.TrimSpace(s)
	if len(s) > n {
		s = s[:n]
	}
	return s
}

// sessionDeviceName is the name a new session is shown under: deviceName
// as before, or the device object's name.
func sessionDeviceName(deviceName string, d *deviceInfo) string {
	if deviceName == "" && d != nil {
		return clip(d.Name, deviceFieldMax)
	}
	return deviceName
}

// attachDevice records the app a session signed in from and links the two.
// Apps that send no device (or an unusable ID) still sign in; their session
// lists with a client ID made from the session.
func (s *Server) attachDevice(r *http.Request, token string, d *deviceInfo) {
	s.attachDeviceHash(r, auth.HashToken(token), d)
}

// attachDeviceHash is attachDevice for a session known by its token hash.
// It returns the client as recorded, or nil when nothing was recorded.
func (s *Server) attachDeviceHash(r *http.Request, tokenHash string, d *deviceInfo) *store.Client {
	if d == nil || !store.ValidClientID(d.ID) {
		return nil
	}
	c := store.Client{
		ID: d.ID, Name: clip(d.Name, deviceFieldMax), Platform: strings.ToLower(clip(d.Platform, 32)),
		Brand: clip(d.Brand, deviceFieldMax), Model: clip(d.Model, deviceFieldMax), OS: clip(d.OS, deviceFieldMax),
		App: clip(d.App, deviceFieldMax), FormFactor: strings.ToLower(clip(d.FormFactor, 32)),
	}
	saved, _, err := s.Store.UpsertClient(r.Context(), c)
	if err != nil {
		s.Log.Warn("couldn't record the signed-in device", "err", err)
		return nil
	}
	if err := s.Store.LinkSessionClient(r.Context(), tokenHash, c.ID); err != nil {
		s.Log.Warn("couldn't link the session to its device", "err", err)
		return nil
	}
	// Linking may have carried trust or a name over.
	if got, err := s.Store.GetClient(r.Context(), c.ID); err == nil {
		saved = got
	}
	return &saved
}

// reportDevice answers PUT /api/v1/me/device: an app describing itself when
// it starts, with the same object it sends on sign-in. Sessions that signed
// in before apps sent one get their name and details this way, and details
// that change (a renamed phone, an app update) stay current.
func (s *Server) reportDevice(w http.ResponseWriter, r *http.Request, u store.User) {
	var d deviceInfo
	if err := readJSON(r, &d); err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	if !store.ValidClientID(d.ID) {
		writeErr(w, http.StatusBadRequest, "the device id needs 8 to 64 letters, digits, - or _")
		return
	}
	hash := sessionFrom(r).tokenHash
	c := s.attachDeviceHash(r, hash, &d)
	if c == nil {
		writeErr(w, http.StatusInternalServerError, "couldn't record this device")
		return
	}
	if c.Name != "" {
		_ = s.Store.SetSessionDeviceName(r.Context(), hash, c.Name)
	}
	w.WriteHeader(http.StatusNoContent)
}

// updateClient answers PUT /api/v1/clients/{clientId}: trust or rename a
// device. It answers with the device as GET /devices lists it.
func (s *Server) updateClient(w http.ResponseWriter, r *http.Request, u store.User) {
	var req struct {
		Trusted *bool   `json:"trusted"`
		Name    *string `json:"name"`
	}
	if err := readJSON(r, &req); err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	if req.Name != nil {
		name := strings.TrimSpace(*req.Name)
		if name == "" || len(name) > 80 {
			writeErr(w, http.StatusBadRequest, "a device name needs 1 to 80 characters")
			return
		}
		req.Name = &name
	}
	id := r.PathValue("clientId")
	c, err := s.Store.UpdateClient(r.Context(), id, store.ClientUpdate{Name: req.Name, Trusted: req.Trusted})
	if errors.Is(err, store.ErrNotFound) {
		writeErr(w, http.StatusNotFound, "no such device")
		return
	}
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	ds, err := s.Store.Devices(r.Context())
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	me := store.DeviceID(sessionFrom(r).tokenHash)
	for _, d := range ds {
		if d.ClientID == id {
			d.Current = d.ID == me
			writeJSON(w, http.StatusOK, d)
			return
		}
	}
	// Trusted while signed out: describe it from the client alone.
	writeJSON(w, http.StatusOK, store.Device{
		Name: c.Name, LastSeenAt: c.LastSeen, CreatedAt: c.FirstSeen, ClientID: c.ID, Platform: c.Platform,
		Brand: c.Brand, Model: c.Model, OS: c.OS, App: c.App, Icon: store.ClientIcon(c), Trusted: c.Trusted,
		Label: store.ClientLabel(c),
	})
}
