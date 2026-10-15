package api

import (
	"net/http"
	"os"
	"path"
	"path/filepath"
	"strings"
)

// webApp serves the web player and manager (a built single-page app) from
// WebDir at /app/. Paths that aren't files get index.html, so the app's own
// routes work on reload. Built files (with hashed names under assets/) are
// cached for a long time; index.html never is.
func (s *Server) webApp() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if s.WebDir == "" {
			http.Error(w, "The web app isn't installed on this server. Put its built files in a folder named web next to the server, or set KINORA_WEB.", http.StatusNotFound)
			return
		}
		rel := strings.TrimPrefix(path.Clean("/"+strings.TrimPrefix(r.URL.Path, "/app")), "/")
		full := filepath.Join(s.WebDir, filepath.FromSlash(rel))
		if fi, err := os.Stat(full); rel == "" || err != nil || fi.IsDir() {
			w.Header().Set("Cache-Control", "no-cache")
			http.ServeFile(w, r, filepath.Join(s.WebDir, "index.html"))
			return
		}
		if strings.HasPrefix(rel, "assets/") {
			w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
		}
		http.ServeFile(w, r, full)
	})
}
