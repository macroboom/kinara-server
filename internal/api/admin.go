package api

import (
	"fmt"
	"net/http"
	"strconv"

	"kinora/internal/ffmpeg"
	"kinora/internal/jobs"
	"kinora/internal/parse"
	"kinora/internal/store"
)

// What the web manager needs beyond the browsing API: a library's file
// problems and titles to review, and how the server itself is doing.

type problemFile struct {
	VersionID string `json:"versionId"`
	RelPath   string `json:"relPath"`
	Reason    string `json:"reason"`
}

// libraryProblems lists files that couldn't be read or placed, and titles
// that didn't match, so the owner can fix names or matches.
func (s *Server) libraryProblems(w http.ResponseWriter, r *http.Request, u store.User) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		writeErr(w, http.StatusBadRequest, "bad library id")
		return
	}
	lib, err := s.Store.GetLibrary(r.Context(), id)
	if err != nil {
		writeErr(w, http.StatusNotFound, "library not found")
		return
	}
	probeErrs, err := s.Store.ProbeErrors(r.Context(), id, 500)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	unplaced, err := s.Store.UnlinkedVersions(r.Context(), id, 500)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	unmatched, err := s.Store.UnmatchedItems(r.Context(), id)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	out := map[string]any{"unreadable": []problemFile{}, "unplaced": []problemFile{}, "unmatched": []ItemView{}}
	ur := []problemFile{}
	for _, v := range probeErrs {
		ur = append(ur, problemFile{VersionID: v.ID, RelPath: v.RelPath, Reason: v.ProbeError})
	}
	up := []problemFile{}
	for _, v := range unplaced {
		up = append(up, problemFile{VersionID: v.ID, RelPath: v.RelPath, Reason: unplacedReason(lib.Kind, v.RelPath)})
	}
	out["unreadable"], out["unplaced"] = ur, up
	if views, err := s.views(r.Context(), u.ProfileID, unmatched); err == nil {
		out["unmatched"] = views
	}
	notes := map[string]unmatchedNote{}
	for _, it := range unmatched {
		if n, ok := s.unmatchedNote(r, lib, it); ok {
			notes[it.ID] = n
		}
	}
	out["notes"] = notes
	writeJSON(w, http.StatusOK, out)
}

// unmatchedNote is what the manager needs to fix an unmatched title: where
// its files are, and a hint when the title is probably in the wrong library.
type unmatchedNote struct {
	Path  string `json:"path"`
	Files int    `json:"files"`
	Hint  string `json:"hint,omitempty"`
}

func (s *Server) unmatchedNote(r *http.Request, lib store.Library, it store.Item) (unmatchedNote, bool) {
	vs, err := s.Store.VersionsForItem(r.Context(), it.ID)
	if err != nil || len(vs) == 0 {
		return unmatchedNote{}, false
	}
	n := unmatchedNote{Path: vs[0].RelPath, Files: len(vs)}
	if lib.Kind == store.KindMovies {
		episodes := 0
		for _, v := range vs {
			if parse.Parse(v.RelPath).Kind == parse.Episode {
				episodes++
			}
		}
		switch {
		case episodes > 0:
			n.Hint = "These files are named like TV episodes. Move the folder into a TV shows library."
		case len(vs) > 2:
			n.Hint = fmt.Sprintf("%d files share this title, so it may be a show or a season. If so, move it into a TV shows library.", len(vs))
		}
	}
	return n, true
}

type serverStatus struct {
	Version  string       `json:"version"`
	Warnings []string     `json:"warnings"`
	Metadata bool         `json:"metadata"`
	FFmpeg   ffmpeg.Caps  `json:"ffmpeg"`
	Jobs     *jobs.Status `json:"jobs,omitempty"`
	Convert  bool         `json:"convert"`
	HLS      bool         `json:"hls"`
	Web      bool         `json:"web"`
}

// status says how the server is set up: warnings, what ffmpeg can do, and
// what the background worker is busy with.
func (s *Server) status(w http.ResponseWriter, r *http.Request, u store.User) {
	st := serverStatus{Version: s.Version, Warnings: s.Warnings, Metadata: s.Matcher != nil,
		Convert: s.Transcoder != nil, HLS: s.HLS != nil, Web: s.WebDir != ""}
	if st.Warnings == nil {
		st.Warnings = []string{}
	}
	if s.Transcoder != nil {
		st.FFmpeg = s.Transcoder.Tool.Caps()
	}
	if s.Jobs != nil {
		js := s.Jobs.Status()
		st.Jobs = &js
	}
	writeJSON(w, http.StatusOK, st)
}
