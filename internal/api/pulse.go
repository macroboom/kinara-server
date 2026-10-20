package api

import (
	"net/http"
	"time"

	"kinara/internal/store"
)

// The pulse is how the apps stay current without a connection held open:
// they ask every few seconds, and reload only the parts whose number moved.
// It's tiny and answered from memory, so asking often is cheap even over
// remote access.

type pulseView struct {
	// Library moves when a scan finishes or a title is matched by hand:
	// Home, the library grids and title pages reload.
	Library int64 `json:"library"`
	// Requests moves when a request or an on-the-way status changes:
	// request markers and progress reload.
	Requests int64 `json:"requests"`
	// Active is true while something is on its way, so apps ask more often.
	Active bool `json:"active"`
	// Party moves when any watch party or invite changes: apps then ask
	// /parties/mine whether it concerns them.
	Party int64 `json:"party"`
}

func (s *Server) libraryChanged() {
	s.libraryRevision()
	s.libraryRev.Add(1)
}

// libraryRevision starts from the clock, so a restarted server never hands
// out a number an app already has.
func (s *Server) libraryRevision() int64 {
	if v := s.libraryRev.Load(); v != 0 {
		return v
	}
	s.libraryRev.CompareAndSwap(0, time.Now().UnixMilli())
	return s.libraryRev.Load()
}

func (s *Server) pulse(w http.ResponseWriter, r *http.Request, u store.User) {
	live, _ := s.Store.LiveStatuses()
	s.markOnline(r)
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, http.StatusOK, pulseView{
		Library:  s.libraryRevision(),
		Requests: s.Store.RequestsRevision(),
		Active:   live,
		Party:    s.parties().Rev(),
	})
}
