package api

import (
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"

	"kinora/internal/store"
)

// The folder picker: lets the manager page browse folders on the server's
// own disks, since a web browser can't hand over a path on another computer.

// maxListed caps one folder's listing; a folder with more subfolders than
// this is not a media folder anyone browses by hand.
const maxListed = 2000

type folderEntry struct {
	Name string `json:"name"`
	Path string `json:"path"`
	// Library names the library that already has this folder, or one
	// around it, so the picker can show it can't be added again.
	Library string `json:"library,omitempty"`
}

type folderListing struct {
	// Path is the folder listed; empty for the top level (drives and
	// common places).
	Path string `json:"path"`
	// Parent is one level up; empty when Path is a drive or the root, in
	// which case "up" goes to the top level.
	Parent string `json:"parent"`
	// Library is set when Path itself is already in a library.
	Library string `json:"library,omitempty"`
	// Videos counts the video files directly in Path.
	Videos    int           `json:"videos"`
	Folders   []folderEntry `json:"folders"`
	Truncated bool          `json:"truncated,omitempty"`
	// Separator is the server's path separator, for showing paths.
	Separator string `json:"separator"`
}

// listFolders answers GET /api/v1/fs/folders?path=. With no path it lists
// the places to start from: drives on Windows; the root, the server's home
// folder and the usual mount points elsewhere.
func (s *Server) listFolders(w http.ResponseWriter, r *http.Request, u store.User) {
	libs, err := s.Store.ListLibraries(r.Context())
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	owner := func(p string) string {
		for _, l := range libs {
			for _, f := range l.Folders {
				if samePath(p, f) || insidePath(p, f) {
					return l.Name
				}
			}
		}
		return ""
	}
	out := folderListing{Folders: []folderEntry{}, Separator: string(filepath.Separator)}

	raw := strings.TrimSpace(r.URL.Query().Get("path"))
	if raw == "" {
		for _, p := range places() {
			out.Folders = append(out.Folders, folderEntry{Name: p.name, Path: p.path, Library: owner(p.path)})
		}
		writeJSON(w, http.StatusOK, out)
		return
	}

	path := filepath.Clean(raw)
	if runtime.GOOS == "windows" && len(path) == 2 && path[1] == ':' {
		path += `\` // "D:" means the current folder on D; the picker means the drive
	}
	if !filepath.IsAbs(path) {
		writeErr(w, http.StatusBadRequest, "path must be an absolute folder path")
		return
	}
	entries, err := os.ReadDir(path)
	if err != nil {
		writeErr(w, http.StatusNotFound, "can't open that folder: "+err.Error())
		return
	}
	out.Path = path
	out.Library = owner(path)
	if up := filepath.Dir(path); up != path {
		out.Parent = up
	}
	for _, e := range entries {
		name := e.Name()
		if hiddenFolder(name, out.Parent == "") {
			continue
		}
		full := filepath.Join(path, name)
		isDir := e.IsDir()
		if !isDir && e.Type()&os.ModeSymlink != 0 {
			// A link to a folder (common for mounted shares) counts as one.
			if fi, err := os.Stat(full); err == nil && fi.IsDir() {
				isDir = true
			}
		}
		if !isDir {
			if e.Type().IsRegular() && videoExt[strings.ToLower(filepath.Ext(name))] {
				out.Videos++
			}
			continue
		}
		if len(out.Folders) >= maxListed {
			out.Truncated = true
			continue
		}
		out.Folders = append(out.Folders, folderEntry{Name: name, Path: full, Library: owner(full)})
	}
	sort.Slice(out.Folders, func(i, j int) bool {
		return strings.ToLower(out.Folders[i].Name) < strings.ToLower(out.Folders[j].Name)
	})
	writeJSON(w, http.StatusOK, out)
}

var videoExt = map[string]bool{
	".mkv": true, ".mp4": true, ".m4v": true, ".avi": true, ".mov": true,
	".ts": true, ".m2ts": true, ".webm": true, ".wmv": true, ".mpg": true, ".mpeg": true,
}

// hiddenFolder skips dot folders, the system folders drives and NAS boxes
// keep, and (at the top of a disk) the system's own folders.
func hiddenFolder(name string, atRoot bool) bool {
	if strings.HasPrefix(name, ".") || strings.HasPrefix(name, "$") {
		return true
	}
	switch strings.ToLower(name) {
	case "system volume information", "@eadir", "#recycle", "lost+found":
		return true
	case "proc", "sys", "dev", "run", "boot", "windows", "program files", "program files (x86)", "programdata":
		return atRoot
	}
	return false
}

type place struct{ name, path string }

// places are the starting points of the picker: ones that exist on this
// computer, without repeats.
func places() []place {
	var out []place
	seen := map[string]bool{}
	add := func(name, path string) {
		if path == "" || seen[path] {
			return
		}
		if fi, err := os.Stat(path); err != nil || !fi.IsDir() {
			return
		}
		seen[path] = true
		out = append(out, place{name, path})
	}
	if runtime.GOOS == "windows" {
		for c := 'A'; c <= 'Z'; c++ {
			d := string(c) + `:\`
			add(string(c)+":", d)
		}
		if home, err := os.UserHomeDir(); err == nil {
			add("Home", home)
		}
		return out
	}
	if home, err := os.UserHomeDir(); err == nil {
		add("Home", home)
	}
	for _, p := range []string{"/Volumes", "/media", "/mnt", "/srv", "/data", "/share"} {
		add(p, p)
	}
	// Removable drives usually sit one level down, as /media/<user>/<drive>.
	if user := os.Getenv("USER"); user != "" {
		if entries, err := os.ReadDir(filepath.Join("/media", user)); err == nil {
			for _, e := range entries {
				add(e.Name(), filepath.Join("/media", user, e.Name()))
			}
		}
	}
	add("Whole disk (/)", "/")
	return out
}
