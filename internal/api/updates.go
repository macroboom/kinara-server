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
// version. Installing is one click on Windows and macOS (see
// internal/update); other installs are told the command to run.

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
// every 12 hours.
func (s *Server) RunUpdates(ctx context.Context) {
	if s.Updater == nil {
		return
	}
	go s.Updater.Cleanup(ctx)
	wait := 2 * time.Minute
	for {
		select {
		case <-ctx.Done():
			return
		case <-time.After(wait):
		}
		if st, err := s.Store.GetSettings(ctx); err == nil && st.UpdateCheck {
			s.checkForUpdate(ctx)
		}
		// Spread out, so servers don't all ask at the same minute.
		wait = 11*time.Hour + time.Duration(rand.Int64N(int64(2*time.Hour)))
	}
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
