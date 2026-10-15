package api

import (
	"io"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestWebApp(t *testing.T) {
	dir := t.TempDir()
	os.MkdirAll(filepath.Join(dir, "assets"), 0o755)
	os.WriteFile(filepath.Join(dir, "index.html"), []byte("<html>app</html>"), 0o644)
	os.WriteFile(filepath.Join(dir, "assets", "main-abc.js"), []byte("console.log(1)"), 0o644)
	os.WriteFile(filepath.Join(filepath.Dir(dir), "secret.txt"), []byte("nope"), 0o644)
	s := &Server{WebDir: dir}
	h := s.webApp()

	get := func(path string) (int, string, string) {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest("GET", path, nil))
		b, _ := io.ReadAll(rec.Body)
		return rec.Code, string(b), rec.Header().Get("Cache-Control")
	}
	if code, body, cache := get("/app/"); code != 200 || body != "<html>app</html>" || cache != "no-cache" {
		t.Errorf("/app/ = %d %q %q", code, body, cache)
	}
	if code, body, _ := get("/app/movies/123"); code != 200 || body != "<html>app</html>" {
		t.Errorf("app route = %d %q", code, body)
	}
	if code, body, cache := get("/app/assets/main-abc.js"); code != 200 || body != "console.log(1)" || !strings.Contains(cache, "immutable") {
		t.Errorf("asset = %d %q %q", code, body, cache)
	}
	if _, body, _ := get("/app/../secret.txt"); strings.Contains(body, "nope") {
		t.Error("escaped the web folder")
	}
	if code, _, _ := (&Server{}).webAppStatus(); code != 404 {
		t.Errorf("not installed = %d", code)
	}
}

// webAppStatus is what /app/ answers when no web app is installed.
func (s *Server) webAppStatus() (int, string, string) {
	rec := httptest.NewRecorder()
	s.webApp().ServeHTTP(rec, httptest.NewRequest("GET", "/app/", nil))
	return rec.Code, rec.Body.String(), ""
}
