package api

import (
	"context"
	"errors"
	"math/rand/v2"
	"net/http"
	"time"

	"kinara/internal/store"
	"kinara/internal/update"
)

// Updates: the server checks the downloads site for a newer release twice
// a day (unless the owner turns it off), tells the Manage page, the tray
// and the menu bar item, and fires server.update_available once per
// version. On Windows and macOS it installs the release itself at night
// when nothing is playing (unless the owner turns that off), or with one
// click on the Manage page (see internal/update); other installs are told
// the command to run.

func (s *Server) updateRoutes(mux *http.ServeMux) {
	mux.Handle("GET /api/v1/update", s.admin(s.updateStatus))
	mux.Handle("POST /api/v1/update/check", s.admin(s.updateCheck))
	mux.Handle("POST /api/v1/update/install", s.admin(s.updateInstall))
}

func (s *Server) updateStatus(w http.ResponseWriter, r *http.Request, u store.User) {
	if s.Updater == nil {
		writeJSON(w, http.StatusOK, update.Status{Current: s.Version, Kind: update.KindOther, State: update.StateIdle})
		return
	}
	writeJSON(w, http.StatusOK, s.Updater.Status())
}

func (s *Server) updateCheck(w http.ResponseWriter, r *http.Request, u store.User) {
	if s.Updater == nil {
		writeErr(w, http.StatusNotImplemented, "update checks are off on this server (KINARA_UPDATE_URL=off)")
		return
	}
	s.checkForUpdate(r.Context())
	writeJSON(w, http.StatusOK, s.Updater.Status())
}

func (s *Server) updateInstall(w http.ResponseWriter, r *http.Request, u store.User) {
	if s.Updater == nil {
		writeErr(w, http.StatusNotImplemented, "update checks are off on this server (KINARA_UPDATE_URL=off)")
		return
	}
	// Downloading outlives the request: it runs under the server's context.
	if err := s.Updater.Install(s.baseCtx()); err != nil {
		status := http.StatusConflict
		if errors.Is(err, update.ErrBusy) {
			status = http.StatusTooManyRequests
		}
		writeErr(w, status, err.Error())
		return
	}
	s.Log.Info("update requested", "by", u.Username, "version", s.Updater.Status().Latest)
	writeJSON(w, http.StatusAccepted, s.Updater.Status())
}

// RunUpdates empties the updates folder from an earlier update, then
// checks for new releases: a couple of minutes after starting, then about
// every 12 hours. Every few minutes it also sees whether a release it
// found can be installed on its own now (see autoInstall).
func (s *Server) RunUpdates(ctx context.Context) {
	if s.Updater == nil {
		return
	}
	go s.Updater.Cleanup(ctx)
	next := time.Now().Add(2 * time.Minute)
	tick := time.NewTicker(autoEvery)
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
		}
		st, err := s.Store.GetSettings(ctx)
		if err != nil {
			continue
		}
		if st.UpdateCheck && !time.Now().Before(next) {
			s.checkForUpdate(ctx)
			// Spread out, so servers don't all ask at the same minute.
			next = time.Now().Add(11*time.Hour + time.Duration(rand.Int64N(int64(2*time.Hour))))
		}
		if st.UpdateCheck && st.AutoUpdate {
			s.autoInstall(ctx, time.Now())
		}
	}
}

// autoEvery is how often RunUpdates wakes (a variable for tests).
var autoEvery = 5 * time.Minute

// Quiet hours for installing on its own, in the server's local time: the
// installer restarts the server, so someone watching would be cut off.
const (
	quietFrom = 2 // 2:00
	quietTo   = 5 // until 5:00
)

// autoInstall puts a release in on its own. As soon as one is out it's
// downloaded and checked, ready; then, in the quiet hours with nobody
// watching, installed, where that needs no prompt on the server's computer
// (otherwise it waits, ready, for a click on the Manage page). A download or
// install that failed is tried again the next day.
func (s *Server) autoInstall(ctx context.Context, now time.Time) {
	st := s.Updater.Status()
	if !st.CanInstall {
		return
	}
	once := func(key string) bool {
		day := st.Latest + "@" + now.Format("2006-01-02")
		if v, _ := s.Store.Value(ctx, key); v == day {
			return false
		}
		_ = s.Store.SetValue(ctx, key, day)
		return true
	}
	switch st.State {
	case update.StateIdle, update.StateFailed:
		if !once("update_auto_fetched") {
			return
		}
		if err := s.Updater.Prepare(s.baseCtx()); err != nil {
			s.Log.Warn("update: couldn't download ahead", "version", st.Latest, "err", err)
		}
	case update.StateReady:
		if !st.Unattended || st.NeedsPrompt {
			return
		}
		if h := now.Hour(); h < quietFrom || h >= quietTo {
			return
		}
		if s.watching(now) || !once("update_auto_tried") {
			return
		}
		if err := s.Updater.Install(s.baseCtx(), true); err != nil {
			s.Log.Warn("update: couldn't install on its own", "version", st.Latest, "err", err)
			return
		}
		s.Log.Info("update: installing on its own", "version", st.Latest)
	}
}

// watching is whether anyone is playing something, or a conversion runs.
func (s *Server) watching(now time.Time) bool {
	if len(s.playing.list(now)) > 0 {
		return true
	}
	return s.Transcoder != nil && len(s.Transcoder.Live()) > 0
}

// checkForUpdate asks for the newest release and, the first time a
// version is seen, logs it and tells the webhooks.
func (s *Server) checkForUpdate(ctx context.Context) {
	rel, err := s.Updater.Check(ctx)
	if err != nil {
		s.Log.Debug("update check", "err", err)
		return
	}
	if !update.Newer(rel.Version, s.Version) {
		return
	}
	seen, _ := s.Store.Value(ctx, "update_notified")
	if seen == rel.Version {
		return
	}
	_ = s.Store.SetValue(ctx, "update_notified", rel.Version)
	s.Log.Info("a newer version of Kinara is out", "current", s.Version, "latest", rel.Version, "notes", rel.Notes)
	s.emit("server.update_available", eventScope{}, map[string]any{
		"current": s.Version, "version": rel.Version, "date": rel.Date, "notes": rel.Notes,
	})
}

// updateForHealth is the newer version for /health ("" when there's none),
// which the tray and the menu bar item read on the server's own computer.
func (s *Server) updateForHealth(r *http.Request) string {
	if s.Updater == nil || fromInternet(r) {
		return ""
	}
	if st := s.Updater.Status(); st.Available {
		return st.Latest
	}
	return ""
}
