package api

import (
	"context"
	"math"
	"net/http"
	"strconv"
	"strings"
	"time"

	"kinora/internal/scanner"
	"kinora/internal/store"
	"kinora/internal/watch"
)

// Building event payloads where things happen. Each note* function is a
// one-line hook in the code it reports on; it does nothing when no webhook
// wants the event.

// --- Payload parts ---

type titleIDs struct {
	TMDB int    `json:"tmdb,omitempty"`
	TVDB int    `json:"tvdb,omitempty"`
	IMDB string `json:"imdb,omitempty"`
}

type libraryRef struct {
	ID   int64  `json:"id"`
	Name string `json:"name"`
}

// titleObj is a title in a payload. ID is null for a title that isn't in
// the library.
type titleObj struct {
	ID      *string     `json:"id"`
	Kind    string      `json:"kind"`
	Title   string      `json:"title"`
	Show    string      `json:"show,omitempty"`
	Season  *int        `json:"season,omitempty"`
	Episode *int        `json:"episode,omitempty"`
	Year    int         `json:"year,omitempty"`
	IDs     titleIDs    `json:"ids"`
	Library *libraryRef `json:"library,omitempty"`
	// Path is where the file is, only for webhooks that ask for paths.
	Path string `json:"path,omitempty"`
}

type accountRef struct {
	ID      int64  `json:"id"`
	Name    string `json:"name"`
	Profile string `json:"profile,omitempty"`
}

type deviceRef struct {
	ID       string `json:"id"`
	Name     string `json:"name"`
	Platform string `json:"platform,omitempty"`
	// Remote is true when it came in over remote access.
	Remote bool `json:"remote"`
}

// titleOf describes a library item. For episodes and seasons, the IDs are
// the show's (that's what other sites know).
func (s *Server) titleOf(ctx context.Context, it store.Item, paths bool) titleObj {
	id := it.ID
	t := titleObj{ID: &id, Kind: it.Kind, Title: it.Title, Year: it.Year}
	top := it
	switch it.Kind {
	case store.ItemEpisode, store.ItemSeason:
		season := it
		if it.Kind == store.ItemEpisode {
			season, _ = s.Store.GetItem(ctx, it.ParentID)
			ep := it.EpisodeNumber
			t.Episode = &ep
		}
		sn := it.SeasonNumber
		t.Season = &sn
		if show, err := s.Store.GetItem(ctx, season.ParentID); err == nil {
			top = show
			t.Show = show.Title
			if t.Year == 0 {
				t.Year = show.Year
			}
		}
	}
	if ids, err := s.Store.ItemExternalIDs(ctx, top.ID); err == nil {
		if top.Kind == store.ItemShow {
			t.IDs.TMDB, _ = strconv.Atoi(ids[store.ProviderTMDBTV])
		} else {
			t.IDs.TMDB, _ = strconv.Atoi(ids[store.ProviderTMDBMovie])
		}
		t.IDs.TVDB, _ = strconv.Atoi(ids["tvdb"])
		t.IDs.IMDB = ids[store.ProviderIMDB]
	}
	if lib, err := s.Store.GetLibrary(ctx, it.LibraryID); err == nil {
		t.Library = &libraryRef{ID: lib.ID, Name: lib.Name}
	}
	if paths && (it.Kind == store.ItemMovie || it.Kind == store.ItemEpisode) {
		t.Path = s.Store.ItemPath(ctx, it.ID)
	}
	return t
}

func (s *Server) accountOf(ctx context.Context, u store.User) accountRef {
	a := accountRef{ID: u.ID, Name: u.Username}
	if u.ProfileID > 0 {
		if p, err := s.Store.GetProfile(ctx, u.ProfileID); err == nil {
			a.Profile = p.Name
		}
	}
	return a
}

// deviceOf describes the device a session signed in on: its app install
// when the server knows it, else the session.
func (s *Server) deviceOf(r *http.Request, tokenHash string) deviceRef {
	d := deviceRef{ID: store.DeviceID(tokenHash), Name: s.Store.DeviceName(r.Context(), tokenHash), Remote: fromInternet(r)}
	if c := s.Store.SessionClientID(r.Context(), tokenHash); c != "" {
		d.ID = c
	}
	return d
}

// --- Playback ---

var playbackEvents = []string{"playback.started", "playback.paused", "playback.resumed", "playback.stopped"}

// pauseAfter is how long the position must stand still to count as paused.
const pauseAfter = 8 * time.Second

// playEntry is what a device is playing, as far as the webhooks know.
type playEntry struct {
	user      store.User
	item      store.Item
	device    deviceRef
	versionID string
	pos, dur  float64
	at        time.Time
	lastMove  time.Time
	paused    bool
}

// notePlayback turns a device's progress reports into started, paused,
// resumed and stopped. Players don't say they paused: a position that
// stands still between reports is a pause.
func (s *Server) notePlayback(r *http.Request, u store.User, it store.Item, versionID string, pos, dur float64) {
	if !s.wants(playbackEvents...) {
		return
	}
	now := time.Now()
	dev := s.deviceOf(r, sessionFrom(r).tokenHash)
	type pending struct {
		event string
		e     playEntry
	}
	var out []pending
	in := &s.integ
	in.mu.Lock()
	if in.plays == nil {
		in.plays = map[string]*playEntry{}
	}
	prev := in.plays[dev.ID]
	if prev == nil || prev.item.ID != it.ID || now.Sub(prev.at) > playingFor {
		if prev != nil {
			out = append(out, pending{"playback.stopped", *prev})
		}
		e := &playEntry{user: u, item: it, device: dev, versionID: versionID, pos: pos, dur: dur, at: now, lastMove: now}
		in.plays[dev.ID] = e
		out = append(out, pending{"playback.started", *e})
	} else {
		if math.Abs(pos-prev.pos) > 0.5 {
			prev.lastMove = now
			if prev.paused {
				prev.paused = false
				defer func(e playEntry) { s.emitPlay("playback.resumed", e) }(*prev)
			}
		} else if !prev.paused && now.Sub(prev.lastMove) >= pauseAfter {
			prev.paused = true
			defer func(e playEntry) { s.emitPlay("playback.paused", e) }(*prev)
		}
		prev.pos, prev.dur, prev.at, prev.versionID = pos, dur, now, versionID
	}
	in.mu.Unlock()
	for _, p := range out {
		s.emitPlay(p.event, p.e)
	}
}

// sweepPlays reports playback that stopped reporting as stopped.
func (s *Server) sweepPlays(now time.Time) {
	in := &s.integ
	var gone []playEntry
	in.mu.Lock()
	for k, e := range in.plays {
		if now.Sub(e.at) > playingFor {
			gone = append(gone, *e)
			delete(in.plays, k)
		}
	}
	in.mu.Unlock()
	for _, e := range gone {
		s.emitPlay("playback.stopped", e)
	}
}

type qualityRef struct {
	// Mode is direct, copy (repackaged) or convert (re-encoded).
	Mode   string `json:"mode"`
	Video  string `json:"video,omitempty"`
	Height int    `json:"height,omitempty"`
	HDR    string `json:"hdr,omitempty"`
}

func (s *Server) emitPlay(event string, e playEntry) {
	s.emitWith(event, eventScope{LibraryID: e.item.LibraryID, UserID: e.user.ID}, func(paths bool) any {
		ctx := s.baseCtx()
		data := map[string]any{
			"title":       s.titleOf(ctx, e.item, paths),
			"account":     s.accountOf(ctx, e.user),
			"device":      e.device,
			"positionSec": math.Round(e.pos),
			"durationSec": math.Round(e.dur),
		}
		if event == "playback.stopped" {
			share := 0.0
			if e.dur > 0 {
				share = math.Round(min(e.pos/e.dur, 1)*1000) / 1000
			}
			data["watchedShare"] = share
		} else if v, err := s.Store.GetVersion(ctx, e.versionID); err == nil {
			data["quality"] = qualityRef{Mode: s.streamMethod(e.device.ID, v.ID), Video: v.VideoCodec, Height: v.Height, HDR: v.HDR}
		}
		return data
	})
}

// --- Watched ---

// noteFinished reports item.watched when a progress report reaches the
// watched share.
func (s *Server) noteFinished(r *http.Request, u store.User, it store.Item, pos, dur float64) {
	if watch.IsWatched(pos, dur) {
		s.noteWatched(r, u, it, false)
	}
}

// noteWatched reports item.watched: by hand (the watched button), or by
// playing to the end (once per profile and title for a few hours, since
// players keep reporting at the end).
func (s *Server) noteWatched(r *http.Request, u store.User, it store.Item, byHand bool) {
	if !s.wants("item.watched") {
		return
	}
	if !byHand {
		key := strconv.FormatInt(u.ProfileID, 10) + ":" + it.ID
		now := time.Now()
		s.integ.mu.Lock()
		if s.integ.watched == nil || len(s.integ.watched) > 10000 {
			s.integ.watched = map[string]time.Time{}
		}
		last, seen := s.integ.watched[key]
		s.integ.watched[key] = now
		s.integ.mu.Unlock()
		if seen && now.Sub(last) < 6*time.Hour {
			return
		}
	}
	s.emitWith("item.watched", eventScope{LibraryID: it.LibraryID, UserID: u.ID}, func(paths bool) any {
		ctx := s.baseCtx()
		return map[string]any{"title": s.titleOf(ctx, it, paths), "account": s.accountOf(ctx, u), "byHand": byHand}
	})
}

// --- Accounts ---

// noteSignIn reports user.signed_in, and device.new the first time a
// device signs in. Devices are told apart by their app install when the
// server records one, else by account and device name.
func (s *Server) noteSignIn(r *http.Request, u store.User, tokenHash string) {
	ctx := r.Context()
	dev := s.deviceOf(r, tokenHash)
	key := s.Store.SessionClientID(ctx, tokenHash)
	if key == "" {
		key = "u" + strconv.FormatInt(u.ID, 10) + ":" + strings.ToLower(dev.Name)
	}
	isNew, err := s.Store.SeeDevice(ctx, key)
	if err != nil {
		s.Log.Debug("integrations: noting a device", "err", err)
	}
	scope := eventScope{UserID: u.ID}
	acct := s.accountOf(ctx, u)
	s.emit("user.signed_in", scope, map[string]any{"account": acct, "device": dev})
	if isNew {
		s.emit("device.new", scope, map[string]any{"account": acct, "device": dev})
	}
}

// noteUserCreated reports user.created.
func (s *Server) noteUserCreated(u store.User) {
	s.emit("user.created", eventScope{UserID: u.ID}, map[string]any{
		"account": map[string]any{"id": u.ID, "name": u.Username, "admin": u.IsAdmin}})
}

// --- Server ---

func (s *Server) noteServerStarted(ctx context.Context) {
	s.emitWith("server.started", eventScope{}, func(bool) any {
		libs, _ := s.Store.ListLibraries(ctx)
		return map[string]any{"version": s.Version, "libraries": len(libs)}
	})
}

// noteConversionFailed reports conversion.failed. stage says which kind of
// conversion: stream or hls.
func (s *Server) noteConversionFailed(r *http.Request, u store.User, v store.Version, err error, stage string) {
	if !s.wants("conversion.failed") {
		return
	}
	dev := s.deviceOf(r, sessionFrom(r).tokenHash)
	msg := err.Error()
	if len(msg) > 500 {
		msg = strings.ToValidUTF8(msg[:500], "")
	}
	s.emitWith("conversion.failed", eventScope{LibraryID: v.LibraryID, UserID: u.ID}, func(paths bool) any {
		ctx := s.baseCtx()
		data := map[string]any{"account": s.accountOf(ctx, u), "device": dev, "error": msg, "stage": stage}
		if it, err := s.Store.GetItem(ctx, v.ItemID); err == nil {
			data["title"] = s.titleOf(ctx, it, paths)
		}
		return data
	})
}

// watchRemote reports remote.state_changed when remote access comes up,
// goes down, or its relay connection changes. Checked by the worker.
func (s *Server) watchRemote() {
	if s.Remote == nil {
		return
	}
	info := s.Remote.Info()
	relay, udp := false, false
	var latency *float64
	if st := info.Status; st != nil {
		relay, latency = st.Relay.Connected, st.Relay.LatencyMs
		if st.NetReport != nil {
			udp = st.NetReport.UDP
		}
	}
	connected := info.State == "ready"
	path := "none"
	switch {
	case connected && udp:
		path = "direct"
	case connected && relay:
		path = "relay"
	}
	sig := info.State + "|" + path
	s.integ.mu.Lock()
	prev := s.integ.remote
	s.integ.remote = sig
	s.integ.mu.Unlock()
	if prev == "" || prev == sig {
		return
	}
	data := map[string]any{"connected": connected, "state": info.State, "path": path}
	if latency != nil {
		data["relayLatencyMs"] = math.Round(*latency)
	}
	if info.Error != "" {
		data["error"] = info.Error
	}
	s.emit("remote.state_changed", eventScope{}, data)
}

// --- Library ---

// maxItemEvents caps title events from one scan: a first scan of a big
// library would otherwise send thousands. scan_finished has the counts.
const maxItemEvents = 100

// scanChanged reports what a finished scan added and removed, titles that
// need matching by hand, and library.scan_finished.
func (s *Server) scanChanged(libraryID int64, p scanner.Progress, c scanner.Changes) {
	s.libraryChanged()
	ctx := s.baseCtx()
	lib, err := s.Store.GetLibrary(ctx, libraryID)
	if err != nil {
		return
	}
	libRef := libraryRef{ID: lib.ID, Name: lib.Name}
	scope := eventScope{LibraryID: libraryID}
	itemEvent := func(event string, ids []string) {
		if !s.wants(event) {
			return
		}
		for i, id := range ids {
			if i >= maxItemEvents {
				s.Log.Info("integrations: more titles changed than events sent", "event", event, "titles", len(ids))
				break
			}
			it, err := s.Store.GetItem(ctx, id)
			if err != nil {
				continue // merged into another copy meanwhile
			}
			s.emitWith(event, scope, func(paths bool) any { return map[string]any{"title": s.titleOf(ctx, it, paths)} })
		}
	}
	itemEvent("library.item_added", c.Added)
	itemEvent("library.item_removed", c.Removed)

	if s.wants("library.match_needed") {
		sent := 0
		for _, id := range c.Added {
			it, err := s.Store.GetItem(ctx, id)
			if err != nil || it.MatchState != store.MatchUnmatched || sent >= maxItemEvents {
				continue
			}
			sent++
			s.emitWith("library.match_needed", scope, func(paths bool) any {
				file := ""
				if vs := s.itemFiles(ctx, it); len(vs) > 0 {
					file = vs[0].RelPath
					if paths {
						file = vs[0].Path
					}
				}
				guess := map[string]any{"title": it.Title}
				if it.Year > 0 {
					guess["year"] = it.Year
				}
				return map[string]any{"library": libRef, "file": file, "guess": guess,
					"title": s.titleOf(ctx, it, paths)}
			})
		}
	}

	data := map[string]any{"library": libRef, "added": len(c.Added), "removed": len(c.Removed),
		"unmatched": p.Unmatched, "tookSec": c.TookSec}
	if c.Folder != "" {
		data["folder"] = c.Folder
	}
	s.emit("library.scan_finished", scope, data)
}

// itemFiles are a movie's files, or for a show its first episode's.
func (s *Server) itemFiles(ctx context.Context, it store.Item) []store.Version {
	if it.Kind == store.ItemShow {
		seasons, _ := s.Store.Children(ctx, it.ID)
		for _, sn := range seasons {
			eps, _ := s.Store.Children(ctx, sn.ID)
			for _, ep := range eps {
				if vs, err := s.Store.VersionsForItem(ctx, ep.ID); err == nil && len(vs) > 0 {
					return vs
				}
			}
		}
		return nil
	}
	vs, _ := s.Store.VersionsForItem(ctx, it.ID)
	return vs
}

// --- Incoming updates ---

// noteStatus reports status.updated and status.done.
func (s *Server) noteStatus(event string, src statusSource, st store.TitleStatus, m *titleMatch, scan string) {
	if !s.wants(event) {
		return
	}
	scope := eventScope{}
	if m != nil && m.Item != nil {
		scope.LibraryID = m.Item.LibraryID
	}
	s.emitWith(event, scope, func(paths bool) any {
		ctx := s.baseCtx()
		status := map[string]any{"state": st.State}
		if event != "status.done" {
			if st.Progress != nil {
				status["progress"] = *st.Progress
			}
			if st.Message != "" {
				status["message"] = st.Message
			}
			if st.EtaSec > 0 {
				status["etaSec"] = st.EtaSec
			}
		}
		source := map[string]string{}
		if src.hook != nil {
			source["hook"] = src.name
		} else {
			source["key"] = src.name
		}
		data := map[string]any{"title": s.statusTitle(ctx, st, m, paths), "status": status, "source": source}
		if scan != "" {
			data["scan"] = scan
		}
		return data
	})
}

// statusTitle describes what a status is about: the library's episode or
// title when it has it, else what the update said.
func (s *Server) statusTitle(ctx context.Context, st store.TitleStatus, m *titleMatch, paths bool) titleObj {
	if m != nil && m.Item != nil {
		it := *m.Item
		if it.Kind == store.ItemShow && st.Season >= 0 {
			if sid, err := s.Store.FindSeason(ctx, it.ID, st.Season); err == nil && sid != "" {
				if st.Episode >= 0 {
					if eid, err := s.Store.FindEpisode(ctx, sid, st.Episode); err == nil && eid != "" {
						if ep, err := s.Store.GetItem(ctx, eid); err == nil {
							return s.titleOf(ctx, ep, paths)
						}
					}
				}
			}
		}
		t := s.titleOf(ctx, it, paths)
		t.Season, t.Episode = numberOrNil(st.Season), numberOrNil(st.Episode)
		return t
	}
	t := titleObj{Kind: st.Kind, Title: st.Title, Year: st.Year, IDs: titleIDs{TMDB: st.TMDBID},
		Season: numberOrNil(st.Season), Episode: numberOrNil(st.Episode)}
	if st.Episode >= 0 && st.Kind == "show" {
		t.Kind, t.Show = "episode", st.Title
	}
	return t
}
