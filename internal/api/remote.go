package api

import (
	"context"
	"net/http"

	"kinara/internal/qr"
	"kinara/internal/remote"
	"kinara/internal/store"
)

// noteNode records the node ID a session uses through the tunnel, so the
// manager can name remote devices and block a device's key. Written only
// when it changes.
func (s *Server) noteNode(r *http.Request, tokenHash string) {
	p := remote.PeerFrom(r.Context())
	if p == nil {
		return
	}
	if v, ok := s.nodeSeen.Load(tokenHash); ok && v.(string) == p.NodeID {
		return
	}
	if err := s.Store.SetSessionNode(r.Context(), tokenHash, p.NodeID); err == nil {
		s.nodeSeen.Store(tokenHash, p.NodeID)
	}
}

// remoteSummary is the public part of remote access for /health: no
// ticket, addresses or devices, since anyone can ask.
func remoteSummary(in remote.Info) map[string]any {
	out := map[string]any{"enabled": in.Enabled, "state": in.State}
	if in.NodeID != "" {
		out["nodeId"] = in.NodeID
	}
	if st := in.Status; st != nil {
		out["relayConnected"] = st.Relay.Connected
		out["peers"] = len(st.Peers)
		if st.NetReport != nil {
			out["udp"] = st.NetReport.UDP
		}
		v6 := false
		for _, a := range st.Addrs {
			if a.Family == "ipv6" && a.Scope == "global" {
				v6 = true
			}
		}
		out["globalIpv6"] = v6
		codes := []string{}
		for _, w := range st.Warnings {
			codes = append(codes, w.Code)
		}
		out["warnings"] = codes
	}
	return out
}

// remotePeer is a connected device with the account it signed in as.
type remotePeer struct {
	remote.PeerState
	Device   string `json:"device,omitempty"`
	Username string `json:"username,omitempty"`
	DeviceID string `json:"deviceId,omitempty"`
	Blocked  bool   `json:"blocked,omitempty"`
}

type remoteResponse struct {
	remote.Info
	// Available is false when this server can't do remote access at all.
	Available bool `json:"available"`
	// QR is the ticket as an SVG QR code, for pairing a device.
	QR      string              `json:"qr,omitempty"`
	Peers   []remotePeer        `json:"peers"`
	Blocked []store.BlockedNode `json:"blocked"`
}

func (s *Server) remoteInfo(w http.ResponseWriter, r *http.Request, u store.User) {
	resp := remoteResponse{Peers: []remotePeer{}, Blocked: []store.BlockedNode{}, Info: remote.Info{State: remote.StateDisabled, Notes: []remote.Note{}}}
	rs, err := s.Store.GetRemoteSettings(r.Context())
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	if s.Remote != nil {
		resp.Available = true
		resp.Info = s.Remote.Info()
	}
	// The saved settings are what the page edits, even before a restart.
	resp.Enabled, resp.UDPPort = rs.Enabled, rs.UDPPort
	if resp.Ticket != "" {
		if c, err := qr.Encode([]byte(resp.Ticket)); err == nil {
			resp.QR = c.SVG()
		}
	}
	if resp.Blocked, err = s.Store.BlockedNodes(r.Context()); err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	blocked := map[string]bool{}
	for _, b := range resp.Blocked {
		blocked[b.NodeID] = true
	}
	if resp.Status != nil {
		names, _ := s.Store.NodeDevices(r.Context())
		for _, p := range resp.Status.Peers {
			rp := remotePeer{PeerState: p, Blocked: blocked[p.NodeID]}
			if d, ok := names[p.NodeID]; ok {
				rp.Device, rp.Username, rp.DeviceID = d.Name, d.Username, d.DeviceID
			}
			resp.Peers = append(resp.Peers, rp)
		}
		// The page shows peers once, with names; the raw list isn't needed.
		st := *resp.Status
		st.Peers = nil
		resp.Status = &st
	}
	writeJSON(w, http.StatusOK, resp)
}

func (s *Server) putRemoteSettings(w http.ResponseWriter, r *http.Request, u store.User) {
	rs, err := s.Store.GetRemoteSettings(r.Context())
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	if err := readJSON(r, &rs); err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	if rs.UDPPort < 0 || rs.UDPPort > 65535 {
		writeErr(w, http.StatusBadRequest, "udpPort must be 0 (automatic) or a port from 1 to 65535")
		return
	}
	if rs.Enabled && s.Remote == nil {
		writeErr(w, http.StatusConflict, "remote access isn't available on this server")
		return
	}
	if err := s.Store.PutRemoteSettings(r.Context(), rs); err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	if s.Remote != nil {
		s.Remote.Configure(rs.Enabled, rs.UDPPort)
	}
	writeJSON(w, http.StatusOK, rs)
}

type nodeRequest struct {
	NodeID string `json:"nodeId"`
	Label  string `json:"label"`
}

// blockNode persists a block and applies it to the running sidecar.
func (s *Server) blockNode(ctx context.Context, nodeID, label string) error {
	if len(label) > 200 {
		label = label[:200]
	}
	if err := s.Store.BlockNode(ctx, nodeID, label); err != nil {
		return err
	}
	if s.Remote != nil {
		s.Remote.Block(nodeID)
	}
	return nil
}

func (s *Server) remoteBlock(w http.ResponseWriter, r *http.Request, u store.User) {
	var req nodeRequest
	if err := readJSON(r, &req); err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	if !remote.ValidNodeID(req.NodeID) {
		writeErr(w, http.StatusBadRequest, "nodeId must be 64 lowercase hex characters")
		return
	}
	// Refuse to lock out the device doing the blocking.
	if p := remote.PeerFrom(r.Context()); p != nil && p.NodeID == req.NodeID {
		writeErr(w, http.StatusBadRequest, "that's this device")
		return
	}
	if req.Label == "" {
		if names, err := s.Store.NodeDevices(r.Context()); err == nil {
			if d, ok := names[req.NodeID]; ok {
				req.Label = d.Name + " · " + d.Username
			}
		}
	}
	if err := s.blockNode(r.Context(), req.NodeID, req.Label); err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) remoteUnblock(w http.ResponseWriter, r *http.Request, u store.User) {
	var req nodeRequest
	if err := readJSON(r, &req); err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	if !remote.ValidNodeID(req.NodeID) {
		writeErr(w, http.StatusBadRequest, "nodeId must be 64 lowercase hex characters")
		return
	}
	if _, err := s.Store.UnblockNode(r.Context(), req.NodeID); err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	if s.Remote != nil {
		s.Remote.Unblock(req.NodeID)
	}
	w.WriteHeader(http.StatusNoContent)
}
