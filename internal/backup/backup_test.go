//go:build !nosqlite

package backup

import (
	"archive/zip"
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"kinara/internal/auth"
	"kinara/internal/store"
)

func setup(t *testing.T) (*Manager, string) {
	t.Helper()
	data := t.TempDir()
	st, err := store.Open(filepath.Join(data, "kinara.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	hash, _ := auth.HashPassword("password123")
	if _, err := st.CreateUser(context.Background(), "alex", hash, true); err != nil {
		t.Fatal(err)
	}
	os.MkdirAll(filepath.Join(data, "iroh"), 0o755)
	os.WriteFile(filepath.Join(data, "iroh", "secret.key"), []byte("node-key"), 0o600)
	os.MkdirAll(filepath.Join(data, "cloud"), 0o755)
	os.WriteFile(filepath.Join(data, "cloud", "state.json"), []byte(`{"serverId":"s1"}`), 0o600)
	env := filepath.Join(data, "kinara.env")
	os.WriteFile(env, []byte("KINARA_LISTEN=:8420\n"), 0o600)
	return &Manager{Store: st, DataDir: data, EnvFile: env, Version: "0.3.0"}, data
}

func TestCreateListCheck(t *testing.T) {
	m, data := setup(t)
	ctx := context.Background()
	dir := filepath.Join(data, "backups")
	info, err := m.Create(ctx, dir, ReasonManual)
	if err != nil {
		t.Fatal(err)
	}
	if info.Counts.Users != 1 || info.Schema != store.SchemaVersion() || info.Version != "0.3.0" {
		t.Fatalf("manifest %+v", info.Manifest)
	}
	want := map[string]bool{"kinara.db": true, "kinara.env": true, "iroh/secret.key": true, "cloud/state.json": true}
	for _, f := range info.Files {
		delete(want, f)
	}
	if len(want) != 0 {
		t.Fatalf("missing from the backup: %v (has %v)", want, info.Files)
	}
	list, err := List(dir)
	if err != nil || len(list) != 1 || list[0].Name != info.Name {
		t.Fatalf("list %v %v", list, err)
	}
	if _, err := Check(ctx, filepath.Join(dir, info.Name), "0.3.0"); err != nil {
		t.Fatal(err)
	}
	// No leftovers next to it.
	entries, _ := os.ReadDir(dir)
	if len(entries) != 1 {
		t.Fatalf("backups folder has %d entries", len(entries))
	}
}

func writeZip(t *testing.T, p string, files map[string]string) {
	t.Helper()
	f, _ := os.Create(p)
	zw := zip.NewWriter(f)
	for name, body := range files {
		w, _ := zw.Create(name)
		w.Write([]byte(body))
	}
	zw.Close()
	f.Close()
}

func TestCheckRefuses(t *testing.T) {
	dir := t.TempDir()
	ctx := context.Background()
	notZip := filepath.Join(dir, "a.zip")
	os.WriteFile(notZip, []byte("hello"), 0o644)
	if _, err := Check(ctx, notZip, "1"); err == nil {
		t.Error("accepted a file that isn't a zip")
	}
	noManifest := filepath.Join(dir, "b.zip")
	writeZip(t, noManifest, map[string]string{"kinara.db": "x"})
	if _, err := Check(ctx, noManifest, "1"); err == nil {
		t.Error("accepted a zip without a manifest")
	}
	newer := filepath.Join(dir, "c.zip")
	writeZip(t, newer, map[string]string{"manifest.json": `{"format":1,"version":"99.0.0","schema":9999}`, "kinara.db": "x"})
	if _, err := Check(ctx, newer, "1"); err == nil {
		t.Error("accepted a backup from a newer schema")
	}
	escape := filepath.Join(dir, "d.zip")
	writeZip(t, escape, map[string]string{"manifest.json": `{"format":1,"schema":1}`, "kinara.db": "x", "../evil": "x"})
	if _, err := Check(ctx, escape, "1"); err == nil {
		t.Error("accepted a path outside the data folder")
	}
	junkDB := filepath.Join(dir, "e.zip")
	writeZip(t, junkDB, map[string]string{"manifest.json": `{"format":1,"schema":1}`, "kinara.db": "not a database"})
	if _, err := Check(ctx, junkDB, "1"); err == nil {
		t.Error("accepted a broken database")
	}
}

func TestStageAndApply(t *testing.T) {
	m, data := setup(t)
	ctx := context.Background()
	dir := filepath.Join(data, "backups")
	info, err := m.Create(ctx, dir, ReasonManual)
	if err != nil {
		t.Fatal(err)
	}
	// Change things after the backup: a second account, a new node key.
	hash, _ := auth.HashPassword("password123")
	m.Store.CreateUser(ctx, "sam", hash, false)
	os.WriteFile(filepath.Join(data, "iroh", "secret.key"), []byte("new-key"), 0o600)
	os.WriteFile(m.EnvFile, []byte("KINARA_LISTEN=:9000\n"), 0o600)

	if err := Stage(data, filepath.Join(dir, info.Name), Options{}); err != nil {
		t.Fatal(err)
	}
	if !Pending(data) {
		t.Fatal("nothing pending")
	}
	m.Store.Close()

	man, err := ApplyPending(data, m.EnvFile, nil)
	if err != nil || man == nil {
		t.Fatalf("apply: %v %v", man, err)
	}
	if Pending(data) {
		t.Fatal("still pending after applying")
	}
	st, err := store.Open(filepath.Join(data, "kinara.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	if n, _ := st.CountUsers(ctx); n != 1 {
		t.Fatalf("restored database has %d users, want 1", n)
	}
	if b, _ := os.ReadFile(filepath.Join(data, "iroh", "secret.key")); string(b) != "node-key" {
		t.Fatalf("node key %q", b)
	}
	// The settings file stays unless asked for.
	if b, _ := os.ReadFile(m.EnvFile); string(b) != "KINARA_LISTEN=:9000\n" {
		t.Fatalf("settings file replaced: %q", b)
	}
	// The previous files are kept aside.
	if b, _ := os.ReadFile(filepath.Join(data, "restore-old", "iroh", "secret.key")); string(b) != "new-key" {
		t.Fatalf("previous key %q", b)
	}
	if _, err := os.Stat(filepath.Join(data, "restore-old", "kinara.db")); err != nil {
		t.Fatal("previous database not kept")
	}

	// Nothing pending: nothing happens.
	if man, err := ApplyPending(data, m.EnvFile, nil); man != nil || err != nil {
		t.Fatalf("second apply: %v %v", man, err)
	}
}

func TestApplyRestoresSettingsWhenAsked(t *testing.T) {
	m, data := setup(t)
	ctx := context.Background()
	dir := filepath.Join(data, "backups")
	info, err := m.Create(ctx, dir, ReasonManual)
	if err != nil {
		t.Fatal(err)
	}
	os.WriteFile(m.EnvFile, []byte("KINARA_LISTEN=:9000\n"), 0o600)
	Stage(data, filepath.Join(dir, info.Name), Options{Settings: true})
	m.Store.Close()
	if _, err := ApplyPending(data, m.EnvFile, nil); err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(m.EnvFile); string(b) != "KINARA_LISTEN=:8420\n" {
		t.Fatalf("settings file %q", b)
	}
}

func TestPruneAndNames(t *testing.T) {
	m, data := setup(t)
	ctx := context.Background()
	dir := filepath.Join(data, "backups")
	var names []string
	for i := 0; i < 4; i++ {
		info, err := m.Create(ctx, dir, ReasonScheduled)
		if err != nil {
			t.Fatal(err)
		}
		names = append(names, info.Name)
		time.Sleep(1100 * time.Millisecond) // names have seconds
	}
	before, _ := m.Create(ctx, dir, ReasonBeforeRestore)
	Prune(dir, 2, nil)
	list, _ := List(dir)
	got := map[string]bool{}
	for _, b := range list {
		got[b.Name] = true
	}
	if len(list) != 3 || !got[names[3]] || !got[names[2]] || !got[before.Name] {
		t.Fatalf("after prune: %v", got)
	}

	for _, n := range []string{"kinara-backup-2026-10-25-033000.zip", "kinara-backup-2026-10-25-033000-before-restore.zip"} {
		if !ValidName(n) {
			t.Errorf("%s should be valid", n)
		}
	}
	for _, n := range []string{"../kinara-backup-x.zip", "kinara-backup-..zip/../../x.zip", "other.zip", "kinara-backup-x.exe", `kinara-backup-a\b.zip`} {
		if ValidName(n) {
			t.Errorf("%s should be refused", n)
		}
	}
}

// A restore that fails while putting the backup's files in place puts the
// current ones back and isn't tried again; one that can't even do that
// stops the server from starting until the owner has sorted it out.
func TestApplyFailureRollsBack(t *testing.T) {
	m, data := setup(t)
	ctx := context.Background()
	dir := filepath.Join(data, "backups")
	info, err := m.Create(ctx, dir, ReasonManual)
	if err != nil {
		t.Fatal(err)
	}
	hash, _ := auth.HashPassword("password123")
	m.Store.CreateUser(ctx, "sam", hash, false)
	os.WriteFile(filepath.Join(data, "iroh", "secret.key"), []byte("new-key"), 0o600)
	m.Store.Close()

	defer func() { placeFile = os.Rename }()
	calls := 0
	placeFile = func(src, dst string) error {
		if calls++; calls == 2 {
			return errors.New("disk full")
		}
		return os.Rename(src, dst)
	}
	if err := Stage(data, filepath.Join(dir, info.Name), Options{}); err != nil {
		t.Fatal(err)
	}
	_, err = ApplyPending(data, m.EnvFile, nil)
	if err == nil || errors.Is(err, ErrRestoreBroken) {
		t.Fatalf("apply: %v", err)
	}
	if Pending(data) {
		t.Fatal("a failed restore is tried again on the next start")
	}
	st, err := store.Open(filepath.Join(data, "kinara.db"))
	if err != nil {
		t.Fatal(err)
	}
	if n, _ := st.CountUsers(ctx); n != 2 {
		t.Fatalf("database after a failed restore has %d users, want the current 2", n)
	}
	st.Close()
	if b, _ := os.ReadFile(filepath.Join(data, "iroh", "secret.key")); string(b) != "new-key" {
		t.Fatalf("node key after a failed restore %q", b)
	}

	// Now the files can't be put back either.
	if err := Stage(data, filepath.Join(dir, info.Name), Options{}); err != nil {
		t.Fatal(err)
	}
	calls = 0
	old := filepath.Join(data, oldDir)
	placeFile = func(src, dst string) error {
		if calls++; calls == 2 {
			os.RemoveAll(old) // the previous files are gone from restore-old
			return errors.New("disk full")
		}
		return os.Rename(src, dst)
	}
	if _, err := ApplyPending(data, m.EnvFile, nil); !errors.Is(err, ErrRestoreBroken) {
		t.Fatalf("apply: %v", err)
	}
	// Still refused on the next start.
	if _, err := ApplyPending(data, m.EnvFile, nil); !errors.Is(err, ErrRestoreBroken) {
		t.Fatalf("second start: %v", err)
	}
}

// A backup staged by a newer server isn't applied by an older one.
func TestApplyRefusesNewerSchema(t *testing.T) {
	data := t.TempDir()
	p := filepath.Join(data, "new.zip")
	writeZip(t, p, map[string]string{"manifest.json": `{"format":1,"schema":9999,"version":"9.9"}`, "kinara.db": "x"})
	if err := Stage(data, p, Options{}); err != nil {
		t.Fatal(err)
	}
	os.WriteFile(filepath.Join(data, "kinara.db"), []byte("current"), 0o600)
	if _, err := ApplyPending(data, "", nil); err == nil {
		t.Fatal("applied a newer backup")
	}
	if b, _ := os.ReadFile(filepath.Join(data, "kinara.db")); string(b) != "current" {
		t.Fatal("current database was moved")
	}
}
