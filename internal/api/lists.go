package api

import (
	"errors"
	"net/http"
	"strconv"

	"kinora/internal/store"
)

// Lists belong to the profile watching on this session: "My List" (made on
// first use) and any lists it named.

func listErr(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, store.ErrNotFound):
		writeErr(w, http.StatusNotFound, "list or title not found")
	case errors.Is(err, store.ErrBadInput):
		writeErr(w, http.StatusBadRequest, "that can't be done with this list")
	default:
		writeErr(w, http.StatusInternalServerError, err.Error())
	}
}

func listID(r *http.Request) (int64, bool) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	return id, err == nil && id > 0
}

func (s *Server) getLists(w http.ResponseWriter, r *http.Request, u store.User) {
	lists, err := s.Store.Lists(r.Context(), u.ProfileID)
	if err != nil {
		listErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, lists)
}

type listRequest struct {
	Name string `json:"name"`
}

func (s *Server) createList(w http.ResponseWriter, r *http.Request, u store.User) {
	var req listRequest
	if err := readJSON(r, &req); err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	l, err := s.Store.CreateList(r.Context(), u.ProfileID, req.Name)
	if err != nil {
		if errors.Is(err, store.ErrBadInput) {
			writeErr(w, http.StatusBadRequest, "a list name must be 1 to 80 characters")
			return
		}
		listErr(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, l)
}

func (s *Server) renameList(w http.ResponseWriter, r *http.Request, u store.User) {
	id, ok := listID(r)
	if !ok {
		writeErr(w, http.StatusBadRequest, "bad list id")
		return
	}
	var req listRequest
	if err := readJSON(r, &req); err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	if err := s.Store.RenameList(r.Context(), u.ProfileID, id, req.Name); err != nil {
		listErr(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) deleteList(w http.ResponseWriter, r *http.Request, u store.User) {
	id, ok := listID(r)
	if !ok {
		writeErr(w, http.StatusBadRequest, "bad list id")
		return
	}
	if err := s.Store.DeleteList(r.Context(), u.ProfileID, id); err != nil {
		listErr(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// listItems answers {"list": ..., "items": [...]} with watch state.
func (s *Server) listItemsHandler(w http.ResponseWriter, r *http.Request, u store.User) {
	id, ok := s.resolveListID(w, r, u)
	if !ok {
		return
	}
	items, err := s.Store.ItemsInList(r.Context(), u.ProfileID, id)
	if err != nil {
		listErr(w, err)
		return
	}
	views, err := s.views(r.Context(), u.ProfileID, items)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"listId": id, "items": views})
}

// resolveListID reads {id} from the path; "default" means the profile's My List.
func (s *Server) resolveListID(w http.ResponseWriter, r *http.Request, u store.User) (int64, bool) {
	if r.PathValue("id") == "default" {
		id, err := s.Store.DefaultList(r.Context(), u.ProfileID)
		if err != nil {
			listErr(w, err)
			return 0, false
		}
		return id, true
	}
	id, ok := listID(r)
	if !ok {
		writeErr(w, http.StatusBadRequest, "bad list id")
	}
	return id, ok
}

func (s *Server) addToList(w http.ResponseWriter, r *http.Request, u store.User) {
	id, ok := s.resolveListID(w, r, u)
	if !ok {
		return
	}
	if err := s.Store.AddToList(r.Context(), u.ProfileID, id, r.PathValue("item")); err != nil {
		listErr(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) removeFromList(w http.ResponseWriter, r *http.Request, u store.User) {
	id, ok := s.resolveListID(w, r, u)
	if !ok {
		return
	}
	if err := s.Store.RemoveFromList(r.Context(), u.ProfileID, id, r.PathValue("item")); err != nil {
		listErr(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
