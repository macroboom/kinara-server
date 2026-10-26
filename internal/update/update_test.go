package update

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestNewer(t *testing.T) {
	cases := []struct {
		a, b string
		want bool
	}{
		{"0.2.0", "0.1.9", true},
		{"0.1.10", "0.1.9", true},
		{"0.1.9", "0.1.10", false},
		{"1.0.0", "1.0.0", false},
		{"v1.0.1", "1.0.0", true},
		{"1.0.0", "1.0.0-rc.1", true},
		{"1.0.0-rc.1", "1.0.0", false},
		{"1.0.0-rc.2", "1.0.0-rc.1", true},
		{"1.0.0-rc.10", "1.0.0-rc.9", true},
		{"0.2.0", "0.2.0-dev", true},
		{"0.1.0", "0.1.0-dev", true},
		{"0.1", "0.0.9", true},
		{"junk", "0.1.0", false},
		{"0.1.0", "junk", true},
	}
	for _, c := range cases {
		if got := Newer(c.a, c.b); got != c.want {
			t.Errorf("Newer(%q, %q) = %v, want %v", c.a, c.b, got, c.want)
		}
	}
}

func site(t *testing.T, body []byte, mutate func(*Release)) (*httptest.Server, *Release) {
	t.Helper()
	sum := sha256.Sum256(body)
	var srv *httptest.Server
	rel := &Release{Version: "9.0.0", Date: "2026-10-20", Notes: "https://example.com/notes"}
	srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/server/latest.json":
			json.NewEncoder(w).Encode(rel)
		case "/server/9.0.0/Kinara-Setup-9.0.0-x64.exe":
			w.Write(body)
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)
	rel.Files = map[string]File{"windows-x64": {
		Name: "Kinara-Setup-9.0.0-x64.exe", URL: srv.URL + "/server/9.0.0/Kinara-Setup-9.0.0-x64.exe",
		Size: int64(len(body)), SHA256: hex.EncodeToString(sum[:]),
	}}
	if mutate != nil {
		mutate(rel)
	}
	return srv, rel
}

func TestCheckAndStatus(t *testing.T) {
	srv, _ := site(t, []byte("installer"), nil)
	u := &Updater{URL: srv.URL + "/server/latest.json", Current: "0.1.0", Dir: t.TempDir(), Kind: KindDeb}
	if _, err := u.Check(context.Background()); err != nil {
		t.Fatal(err)
	}
	st := u.Status()
	if !st.Available || st.Latest != "9.0.0" || st.CheckedAt == 0 {
		t.Fatalf("status %+v", st)
	}
	// A .deb is updated by apt: Kinara says how instead of installing.
	if st.CanInstall || st.How == "" {
		t.Fatalf("deb should not install itself: %+v", st)
	}
	if err := u.Install(context.Background()); err == nil {
		t.Fatal("install should refuse on a deb")
	}

	u2 := &Updater{URL: srv.URL + "/server/latest.json", Current: "9.0.0", Dir: t.TempDir(), Kind: KindWindows}
	u2.Check(context.Background())
	if st := u2.Status(); st.Available {
		t.Fatalf("same version shown as an update: %+v", st)
	}
}

func TestCheckError(t *testing.T) {
	u := &Updater{URL: "http://127.0.0.1:1/latest.json", Current: "0.1.0", Dir: t.TempDir(), Kind: KindOther}
	if _, err := u.Check(context.Background()); err == nil {
		t.Fatal("expected an error")
	}
	if st := u.Status(); st.Error == "" || st.Available {
		t.Fatalf("status %+v", st)
	}
}

func TestDownloadChecksum(t *testing.T) {
	body := []byte("the real installer")
	srv, rel := site(t, body, nil)
	dir := t.TempDir()
	u := &Updater{URL: srv.URL + "/server/latest.json", Current: "0.1.0", Dir: dir}
	f := rel.Files["windows-x64"]
	if err := u.checkFile(f); err != nil {
		t.Fatal(err)
	}
	path, err := u.download(context.Background(), f)
	if err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(path); string(b) != string(body) {
		t.Fatalf("got %q", b)
	}

	bad := f
	bad.SHA256 = hex.EncodeToString(make([]byte, 32))
	os.Remove(path)
	if _, err := u.download(context.Background(), bad); err == nil {
		t.Fatal("a checksum mismatch should fail")
	}
	if left, _ := os.ReadDir(dir); len(left) != 0 {
		t.Fatalf("files left after a failed download: %v", left)
	}

	short := f
	short.Size = 3
	if _, err := u.download(context.Background(), short); err == nil {
		t.Fatal("a download bigger than listed should fail")
	}
}

func TestCheckFileRefusesOtherHosts(t *testing.T) {
	u := &Updater{URL: "https://dl.kinara.media/server/latest.json"}
	good := File{Name: "Kinara-macOS.pkg", URL: "https://dl.kinara.media/server/1.0.0/Kinara-1.0.0-macOS.pkg", Size: 10, SHA256: hex.EncodeToString(make([]byte, 32))}
	if err := u.checkFile(good); err != nil {
		t.Fatal(err)
	}
	for _, f := range []File{
		{Name: good.Name, URL: "https://evil.example/x.pkg", Size: 10, SHA256: good.SHA256},
		{Name: good.Name, URL: "http://dl.kinara.media/x.pkg", Size: 10, SHA256: good.SHA256},
		{Name: "../x.pkg", URL: good.URL, Size: 10, SHA256: good.SHA256},
		{Name: good.Name, URL: good.URL, Size: 0, SHA256: good.SHA256},
		{Name: good.Name, URL: good.URL, Size: 10, SHA256: "abc"},
	} {
		if err := u.checkFile(f); err == nil {
			t.Errorf("accepted %+v", f)
		}
	}
}

func TestCleanupKeepsLog(t *testing.T) {
	dir := t.TempDir()
	for _, n := range []string{"Kinara-Setup-1.0.0-x64.exe", "Kinara-1.0.0-macOS.pkg.part", "install.log"} {
		os.WriteFile(filepath.Join(dir, n), []byte("x"), 0o644)
	}
	u := &Updater{Dir: dir}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	u.Cleanup(ctx)
	left, _ := os.ReadDir(dir)
	if len(left) != 1 || left[0].Name() != "install.log" {
		t.Fatalf("left %v", left)
	}
}
