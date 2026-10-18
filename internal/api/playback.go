package api

import (
	"net/http"
	"sort"
	"sync"
	"time"

	"kinora/internal/store"
	"kinora/internal/transcode"
)

// What's playing now, from the players' progress reports, and the
// conversions running for them; plus advice for players whose conversion
// can't keep up.

// playing is the last progress report from a device.
type playing struct {
	Device      string  `json:"device"`
	DeviceName  string  `json:"deviceName"`
	User        string  `json:"user"`
	ItemID      string  `json:"itemId"`
	VersionID   string  `json:"versionId"`
	Title       string  `json:"title"`
	PositionSec float64 `json:"positionSec"`
	DurationSec float64 `json:"durationSec"`
	At          int64   `json:"at"`
	// playID is the play_log row this play adds its time to.
	playID int64
}

// nowPlaying keeps the latest report per device.
type nowPlaying struct {
	mu sync.Mutex
	by map[string]playing
}

// playingFor is how long after its last report a device still counts as
// playing (players report at least every 10 s while playing).
const playingFor = 45 * time.Second

func (n *nowPlaying) note(p playing) {
	n.mu.Lock()
	defer n.mu.Unlock()
	if n.by == nil {
		n.by = map[string]playing{}
	}
	n.by[p.Device] = p
}

// get is a device's latest report.
func (n *nowPlaying) get(device string) (playing, bool) {
	n.mu.Lock()
	defer n.mu.Unlock()
	p, ok := n.by[device]
	return p, ok
}

func (n *nowPlaying) list(now time.Time) []playing {
	n.mu.Lock()
	defer n.mu.Unlock()
	out := []playing{}
	for k, p := range n.by {
		if now.Sub(time.Unix(p.At, 0)) > playingFor {
			delete(n.by, k)
			continue
		}
		out = append(out, p)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].At > out[j].At })
	return out
}

// streamMethod says how a device is getting a version now: convert (the
// picture re-encoded), copy (repackaged or new sound) or direct.
func (s *Server) streamMethod(device, versionID string) string {
	if s.Transcoder == nil {
		return "direct"
	}
	method := "direct"
	for _, l := range s.Transcoder.Live() {
		if l.Device == device && l.VersionID == versionID {
			if l.Video == "h264" {
				return "convert"
			}
			method = "copy"
		}
	}
	return method
}

// notePlay records a live progress report: what's playing now, and the
// play log (a new row when a device starts something, else the time
// watched since its last report).
func (s *Server) notePlay(r *http.Request, u store.User, it store.Item, versionID, title string, pos, dur float64) {
	a := sessionFrom(r)
	device := store.DeviceID(a.tokenHash)
	now := time.Now()
	method := s.streamMethod(device, versionID)
	p := playing{
		Device: device, DeviceName: s.Store.DeviceName(r.Context(), a.tokenHash), User: u.Username,
		ItemID: it.ID, VersionID: versionID, Title: title, PositionSec: pos, DurationSec: dur, At: now.Unix(),
	}
	prev, ok := s.playing.get(device)
	continuing := ok && prev.ItemID == it.ID && prev.playID != 0 && now.Sub(time.Unix(prev.At, 0)) <= playingFor
	if continuing {
		p.playID = prev.playID
		// Count what played since the last report; a jump (a skip) counts
		// only as much time as passed.
		watched := pos - prev.PositionSec
		wall := now.Sub(time.Unix(prev.At, 0)).Seconds()
		if watched > 0 {
			watched = min(watched, wall+5)
			_ = s.Store.AddPlayTime(r.Context(), p.playID, watched, method, now.Unix())
		}
	} else {
		id, err := s.Store.StartPlay(r.Context(), store.Play{
			StartedAt: now.Unix(), UserID: u.ID, Username: u.Username, Device: device, DeviceName: p.DeviceName,
			ItemID: it.ID, VersionID: versionID, Title: title, Kind: it.Kind, Method: method,
		})
		if err == nil {
			p.playID = id
		}
	}
	s.playing.note(p)
	s.notePlayback(r, u, it, versionID, pos, dur)
}

// streamOwner says who a conversion is for: the device asking, and whether
// it's the priority device.
func (s *Server) streamOwner(r *http.Request, v store.Version) transcode.Owner {
	a := sessionFrom(r)
	o := transcode.Owner{Device: store.DeviceID(a.tokenHash), User: a.user.Username, ItemID: v.ItemID, VersionID: v.ID}
	o.DeviceName = s.Store.DeviceName(r.Context(), a.tokenHash)
	if it, err := s.Store.GetItem(r.Context(), v.ItemID); err == nil {
		o.Title = it.Title
	}
	if st, err := s.Store.GetSettings(r.Context()); err == nil && st.PriorityDevice != "" {
		o.Priority = st.PriorityDevice == o.Device
	}
	return o
}

// playbackAdvice tells a player whether to step down: GET
// /api/v1/playback/advice?version=. Players ask every few seconds while the
// server converts for them.
func (s *Server) playbackAdvice(w http.ResponseWriter, r *http.Request, u store.User) {
	if s.Transcoder == nil {
		writeJSON(w, http.StatusOK, transcode.Advice{Action: "ok"})
		return
	}
	device := store.DeviceID(sessionFrom(r).tokenHash)
	writeJSON(w, http.StatusOK, s.Transcoder.Advise(device, r.URL.Query().Get("version")))
}

type nowPlayingView struct {
	playing
	Priority bool `json:"priority"`
	// Stream is the conversion running for it, if the file isn't playing
	// as is.
	Stream *transcode.LiveInfo `json:"stream,omitempty"`
}

// nowPlayingList is for the manager: who's watching what, and how the
// conversions are keeping up.
func (s *Server) nowPlayingList(w http.ResponseWriter, r *http.Request, u store.User) {
	now := time.Now()
	st, _ := s.Store.GetSettings(r.Context())
	var live []transcode.LiveInfo
	if s.Transcoder != nil {
		live = s.Transcoder.Live()
	}
	out := []nowPlayingView{}
	used := map[string]bool{}
	for _, p := range s.playing.list(now) {
		v := nowPlayingView{playing: p, Priority: st.PriorityDevice != "" && p.Device == st.PriorityDevice}
		// The newest conversion for this device and file.
		for i := len(live) - 1; i >= 0; i-- {
			if live[i].Device == p.Device && live[i].VersionID == p.VersionID && !used[live[i].ID] {
				in := live[i]
				v.Stream = &in
				used[in.ID] = true
				break
			}
		}
		out = append(out, v)
	}
	// Conversions with no report yet (just started).
	for _, in := range live {
		if used[in.ID] {
			continue
		}
		in := in
		out = append(out, nowPlayingView{
			playing:  playing{Device: in.Device, DeviceName: in.DeviceName, User: in.User, ItemID: in.ItemID, VersionID: in.VersionID, Title: in.Title, At: in.StartedAt.Unix()},
			Priority: in.Priority, Stream: &in,
		})
	}
	writeJSON(w, http.StatusOK, out)
}

// devices lists signed-in devices for the manager.
func (s *Server) devices(w http.ResponseWriter, r *http.Request, u store.User) {
	ds, err := s.Store.Devices(r.Context())
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	me := store.DeviceID(sessionFrom(r).tokenHash)
	for i := range ds {
		ds[i].Current = ds[i].ID == me
	}
	writeJSON(w, http.StatusOK, ds)
}

// signOutDevice ends a device's session; it has to sign in again.
func (s *Server) signOutDevice(w http.ResponseWriter, r *http.Request, u store.User) {
	id := r.PathValue("id")
	if id == store.DeviceID(sessionFrom(r).tokenHash) {
		writeErr(w, http.StatusBadRequest, "that's this device; sign out from the menu instead")
		return
	}
	// ?block=1 also refuses the device's key through the tunnel, so it can't
	// come back remotely (it can still sign in on the home network).
	var nodes []string
	if r.URL.Query().Get("block") == "1" {
		nodes, _ = s.Store.DeviceNodes(r.Context(), id)
		if len(nodes) > 0 {
			label := ""
			if ds, err := s.Store.Devices(r.Context()); err == nil {
				for _, d := range ds {
					if d.ID == id {
						label = d.Name + " · " + d.Username
					}
				}
			}
			for _, n := range nodes {
				if err := s.blockNode(r.Context(), n, label); err != nil {
					writeErr(w, http.StatusInternalServerError, err.Error())
					return
				}
			}
		}
	}
	ok, err := s.Store.SignOutDevice(r.Context(), id)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	if !ok {
		writeErr(w, http.StatusNotFound, "no such device")
		return
	}
	if st, err := s.Store.GetSettings(r.Context()); err == nil && st.PriorityDevice == id {
		st.PriorityDevice = ""
		_ = s.Store.PutSettings(r.Context(), st)
	}
	w.WriteHeader(http.StatusNoContent)
}
