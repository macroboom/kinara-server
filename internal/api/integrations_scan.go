package api

import (
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"strings"

	"kinara/internal/scanner"
	"kinara/internal/store"
)

// scanFolder scans one folder of a library: POST
// /api/v1/libraries/{id}/scan?path=<folder>. The path is relative to one
// of the library's folders ("Northern Lights/Season 02"), or a full path
// inside one. Scripts call it when they've put new files somewhere.
func (s *Server) scanFolder(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(r)
	if !ok {
		writeErr(w, http.StatusBadRequest, "bad library id")
		return
	}
	lib, err := s.Store.GetLibrary(r.Context(), id)
	if err != nil {
		writeErr(w, http.StatusNotFound, "library not found")
		return
	}
	dir, err := folderInLibrary(lib, r.URL.Query().Get("path"))
	if err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	if err := s.Scanner.StartFolder(s.BaseCtx, id, dir); err != nil {
		if errors.Is(err, scanner.ErrBusy) {
			writeErr(w, http.StatusConflict, err.Error())
			return
		}
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	p, _ := s.Scanner.Progress(id)
	writeJSON(w, http.StatusAccepted, p)
}

// folderInLibrary finds the folder a ?path= names inside a library's
// folders. It must exist and may not climb out of them.
func folderInLibrary(lib store.Library, raw string) (string, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return "", errors.New("path is empty")
	}
	for _, root := range lib.Folders {
		cand := filepath.FromSlash(raw)
		if !filepath.IsAbs(cand) {
			cand = filepath.Join(root, cand)
		}
		cand = filepath.Clean(cand)
		rel, err := filepath.Rel(root, cand)
		if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
			continue
		}
		if fi, err := os.Stat(cand); err == nil && fi.IsDir() {
			return cand, nil
		}
	}
	return "", errors.New("that folder isn't inside this library, or doesn't exist")
}
