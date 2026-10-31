// Package backup makes and restores backups of a Kinara server.
//
// A backup is one zip file, kinara-backup-<date>-<time>.zip:
//
//	manifest.json      what made it, when, and what's in it
//	kinara.db          the database: libraries, titles and their matches,
//	                   accounts, profiles, watch history, lists, requests,
//	                   integrations and the owner's settings
//	kinara.env         the settings file, when there is one (kept for
//	                   reference; restoring doesn't put it back unless asked)
//	cloud/...          the link to the owner's Kinara account
//	iroh/secret.key    the remote access identity, so devices keep finding it
//	artwork/...        pictures the owner uploaded for titles
//	subtitles/...      subtitles downloaded from OpenSubtitles
//
// The cache (artwork, previews, conversions) isn't in it: the server
// rebuilds that. Media files never are.
//
// Restoring can't swap the database under a running server, so it's staged:
// the backup is checked and copied to <data>/restore-pending.zip, the server
// restarts, and before opening the database ApplyPending moves the current
// files to <data>/restore-old/ and unpacks the backup in their place.
package backup

import (
	"archive/zip"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"os"
	"path"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"sync"
	"time"

	"kinara/internal/store"
)

// Manifest is manifest.json.
type Manifest struct {
	// Format is 1.
	Format int `json:"format"`
	// Version is the Kinara version that made it.
	Version string `json:"version"`
	// Schema is the database's newest migration; a server older than that
	// can't open it.
	Schema    int    `json:"schema"`
	CreatedAt int64  `json:"createdAt"`
	Host      string `json:"host"`
	// Reason is scheduled, manual, before-restore or before-update.
	Reason string       `json:"reason"`
	Counts store.Counts `json:"counts"`
	// Files lists what's in the zip besides the manifest.
	Files []string `json:"files"`
}

// Info is a backup in the backups folder.
type Info struct {
	Name string `json:"name"`
	Size int64  `json:"size"`
	Manifest
}

// Reasons a backup was made.
const (
	ReasonScheduled     = "scheduled"
	ReasonManual        = "manual"
	ReasonBeforeRestore = "before-restore"
	ReasonBeforeUpdate  = "before-update"
)

const (
	dbName      = "kinara.db"
	envName     = "kinara.env"
	pendingName = "restore-pending.zip"
	optionsName = "restore-pending.json"
	oldDir      = "restore-old"
	prefix      = "kinara-backup-"
)

// Manager makes and lists backups.
type Manager struct {
	Store *store.Store
	// DataDir holds kinara.db, cloud/ and iroh/.
	DataDir string
	// EnvFile is the settings file ("" for none).
	EnvFile string
	Version string
	Log     *slog.Logger

	// mu makes one backup at a time.
	mu sync.Mutex
}

func (m *Manager) log() *slog.Logger {
	if m.Log != nil {
		return m.Log
	}
	return slog.New(slog.DiscardHandler)
}

// identity is what besides the database a backup keeps, relative to the
// data folder: the account link, the remote access key, uploaded artwork
// and downloaded subtitles.
var identity = []string{"cloud", "iroh", "artwork", "subtitles"}

// Create makes a backup in dir and returns it.
func (m *Manager) Create(ctx context.Context, dir, reason string) (Info, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if err := os.MkdirAll(dir, 0o750); err != nil {
		return Info{}, fmt.Errorf("backups folder: %w", err)
	}
	now := time.Now()
	name := prefix + now.Format("2006-01-02-150405")
	if reason != ReasonScheduled && reason != ReasonManual {
		name += "-" + reason
	}
	name += ".zip"
	final := filepath.Join(dir, name)
	part := final + ".part"

	// The database first, as a consistent copy taken while it's in use.
	tmp, err := os.MkdirTemp(dir, ".backup-")
	if err != nil {
		return Info{}, err
	}
	defer os.RemoveAll(tmp)
	snap := filepath.Join(tmp, dbName)
	if err := m.Store.Snapshot(ctx, snap); err != nil {
		return Info{}, fmt.Errorf("copying the database: %w", err)
	}
	counts, _ := m.Store.CountAll(ctx)
	host, _ := os.Hostname()
	man := Manifest{Format: 1, Version: m.Version, Schema: store.SchemaVersion(), CreatedAt: now.Unix(),
		Host: host, Reason: reason, Counts: counts}

	out, err := os.OpenFile(part, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
	if err != nil {
		return Info{}, err
	}
	zw := zip.NewWriter(out)
	fail := func(err error) (Info, error) {
		zw.Close()
		out.Close()
		os.Remove(part)
		return Info{}, err
	}
	add := func(name, src string) error {
		f, err := os.Open(src)
		if err != nil {
			return err
		}
		defer f.Close()
		fi, err := f.Stat()
		if err != nil {
			return err
		}
		h, err := zip.FileInfoHeader(fi)
		if err != nil {
			return err
		}
		h.Name, h.Method = name, zip.Deflate
		w, err := zw.CreateHeader(h)
		if err != nil {
			return err
		}
		if _, err := io.Copy(w, f); err != nil {
			return err
		}
		man.Files = append(man.Files, name)
		return nil
	}
	if err := add(dbName, snap); err != nil {
		return fail(err)
	}
	if m.EnvFile != "" {
		if _, err := os.Stat(m.EnvFile); err == nil {
			if err := add(envName, m.EnvFile); err != nil {
				return fail(err)
			}
		}
	}
	for _, d := range identity {
		root := filepath.Join(m.DataDir, d)
		err := filepath.WalkDir(root, func(p string, e fs.DirEntry, err error) error {
			if err != nil || e.IsDir() {
				return nil
			}
			rel, err := filepath.Rel(m.DataDir, p)
			if err != nil {
				return nil
			}
			return add(filepath.ToSlash(rel), p)
		})
		if err != nil {
			return fail(err)
		}
	}
	mw, err := zw.Create("manifest.json")
	if err != nil {
		return fail(err)
	}
	enc := json.NewEncoder(mw)
	enc.SetIndent("", "  ")
	if err := enc.Encode(man); err != nil {
		return fail(err)
	}
	if err := zw.Close(); err != nil {
		out.Close()
		os.Remove(part)
		return Info{}, err
	}
	if err := out.Close(); err != nil {
		os.Remove(part)
		return Info{}, err
	}
	if err := os.Rename(part, final); err != nil {
		os.Remove(part)
		return Info{}, err
	}
	fi, _ := os.Stat(final)
	m.log().Info("backup made", "file", final, "reason", reason)
	return Info{Name: name, Size: fi.Size(), Manifest: man}, nil
}

// List returns the backups in dir, newest first. Files that aren't
// readable backups are left out.
func List(dir string) ([]Info, error) {
	entries, err := os.ReadDir(dir)
	if errors.Is(err, fs.ErrNotExist) {
		return []Info{}, nil
	}
	if err != nil {
		return nil, err
	}
	out := []Info{}
	for _, e := range entries {
		if e.IsDir() || !ValidName(e.Name()) {
			continue
		}
		man, err := readManifest(filepath.Join(dir, e.Name()))
		if err != nil {
			continue
		}
		fi, err := e.Info()
		if err != nil {
			continue
		}
		out = append(out, Info{Name: e.Name(), Size: fi.Size(), Manifest: man})
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].CreatedAt != out[j].CreatedAt {
			return out[i].CreatedAt > out[j].CreatedAt
		}
		return out[i].Name > out[j].Name
	})
	return out, nil
}

// ValidName is a backup file name Kinara makes (so a name from a request
// can't reach outside the backups folder).
func ValidName(name string) bool {
	if !strings.HasPrefix(name, prefix) || !strings.HasSuffix(name, ".zip") {
		return false
	}
	for _, r := range name {
		if !(r >= 'a' && r <= 'z' || r >= '0' && r <= '9' || r == '-' || r == '.') {
			return false
		}
	}
	return !strings.Contains(name, "..")
}

// Prune deletes the oldest scheduled and manual backups beyond keep. The
// ones made before a restore or an update are kept for 30 days whatever.
func Prune(dir string, keep int, log *slog.Logger) {
	if keep < 1 {
		return
	}
	list, err := List(dir)
	if err != nil {
		return
	}
	n := 0
	cutoff := time.Now().Add(-30 * 24 * time.Hour).Unix()
	for _, b := range list {
		special := b.Reason == ReasonBeforeRestore || b.Reason == ReasonBeforeUpdate
		if special {
			if b.CreatedAt >= cutoff {
				continue
			}
		} else if n++; n <= keep {
			continue
		}
		if err := os.Remove(filepath.Join(dir, b.Name)); err == nil && log != nil {
			log.Info("old backup removed", "file", b.Name)
		}
	}
}

func readManifest(p string) (Manifest, error) {
	zr, err := zip.OpenReader(p)
	if err != nil {
		return Manifest{}, err
	}
	defer zr.Close()
	return manifestOf(&zr.Reader)
}

func manifestOf(zr *zip.Reader) (Manifest, error) {
	f, err := zr.Open("manifest.json")
	if err != nil {
		return Manifest{}, errors.New("this isn't a Kinara backup (no manifest.json)")
	}
	defer f.Close()
	var man Manifest
	if err := json.NewDecoder(io.LimitReader(f, 1<<20)).Decode(&man); err != nil {
		return Manifest{}, fmt.Errorf("unreadable manifest.json: %w", err)
	}
	if man.Format != 1 {
		return Manifest{}, fmt.Errorf("a backup format this Kinara doesn't know (%d); update Kinara first", man.Format)
	}
	return man, nil
}

// Check reads a backup and makes sure it can be restored here: a Kinara
// backup with a database this version of Kinara (current) can open.
func Check(ctx context.Context, p, current string) (Manifest, error) {
	zr, err := zip.OpenReader(p)
	if err != nil {
		return Manifest{}, errors.New("this isn't a Kinara backup (not a zip file)")
	}
	defer zr.Close()
	man, err := manifestOf(&zr.Reader)
	if err != nil {
		return man, err
	}
	if man.Schema > store.SchemaVersion() {
		return man, fmt.Errorf("this backup was made by Kinara %s, which is newer than this server (%s); update this server first", man.Version, current)
	}
	for _, f := range zr.File {
		if !safeEntry(f.Name) {
			return man, fmt.Errorf("unexpected file in the backup: %s", f.Name)
		}
	}
	// Unpack the database to a temporary file and open it.
	tmp, err := os.MkdirTemp(filepath.Dir(p), ".check-")
	if err != nil {
		return man, err
	}
	defer os.RemoveAll(tmp)
	db := filepath.Join(tmp, dbName)
	if err := extract(&zr.Reader, dbName, db); err != nil {
		return man, errors.New("the backup has no database")
	}
	if _, _, err := store.CountFile(ctx, db); err != nil {
		return man, fmt.Errorf("the database in the backup can't be read: %w", err)
	}
	return man, nil
}

// safeEntry is a file a backup may hold: the database, the settings file,
// the manifest, or a file under cloud/ or iroh/.
func safeEntry(name string) bool {
	if name == dbName || name == envName || name == "manifest.json" {
		return true
	}
	clean := path.Clean(name)
	if clean != name || strings.HasPrefix(clean, "/") || strings.Contains(clean, "..") || strings.Contains(clean, `\`) {
		return false
	}
	for _, d := range identity {
		if strings.HasPrefix(clean, d+"/") {
			return true
		}
	}
	return false
}

func extract(zr *zip.Reader, name, dst string) error {
	f, err := zr.Open(name)
	if err != nil {
		return err
	}
	defer f.Close()
	if err := os.MkdirAll(filepath.Dir(dst), 0o750); err != nil {
		return err
	}
	out, err := os.OpenFile(dst, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, f); err != nil {
		out.Close()
		return err
	}
	return out.Close()
}

// Options say what a restore puts back besides the database.
type Options struct {
	// Settings replaces the settings file (kinara.env) with the backup's.
	// Off by default: it may name folders that don't exist on this computer.
	Settings bool `json:"settings"`
}

// Stage copies a checked backup to the data folder, to be restored when
// the server next starts.
func Stage(dataDir, src string, opts Options) error {
	dst := filepath.Join(dataDir, pendingName)
	if err := copyFile(src, dst+".part"); err != nil {
		return err
	}
	if err := os.Rename(dst+".part", dst); err != nil {
		return err
	}
	b, _ := json.Marshal(opts)
	return os.WriteFile(filepath.Join(dataDir, optionsName), b, 0o600)
}

// Pending reports whether a restore waits for the next start.
func Pending(dataDir string) bool {
	_, err := os.Stat(filepath.Join(dataDir, pendingName))
	return err == nil
}

// ApplyPending restores a staged backup. Call it before opening the
// database. The current database, cloud/ and iroh/ go to restore-old/
// (replacing an earlier one) rather than being deleted. It returns the
// manifest of what was restored (nil when nothing was waiting).
func ApplyPending(dataDir, envFile string, log *slog.Logger) (*Manifest, error) {
	if b, err := os.ReadFile(filepath.Join(dataDir, brokenName)); err == nil {
		return nil, fmt.Errorf("%w (%s)", ErrRestoreBroken, strings.TrimSpace(string(b)))
	}
	pending := filepath.Join(dataDir, pendingName)
	if _, err := os.Stat(pending); err != nil {
		return nil, nil
	}
	var opts Options
	if b, err := os.ReadFile(filepath.Join(dataDir, optionsName)); err == nil {
		_ = json.Unmarshal(b, &opts)
	}
	zr, err := zip.OpenReader(pending)
	if err != nil {
		os.Rename(pending, pending+".bad")
		return nil, fmt.Errorf("the backup to restore can't be read (left as %s): %w", pendingName+".bad", err)
	}
	defer zr.Close()
	man, err := manifestOf(&zr.Reader)
	if err != nil {
		zr.Close()
		os.Rename(pending, pending+".bad")
		return nil, err
	}
	// Staged by a newer server, then this older one started: its database
	// couldn't be opened here, so nothing is touched.
	if man.Schema > store.SchemaVersion() {
		zr.Close()
		os.Rename(pending, pending+".bad")
		return nil, fmt.Errorf("the backup was made by Kinara %s, which is newer than this server; update the server and restore it again (left as %s)", man.Version, pendingName+".bad")
	}

	// Unpack into a new folder first, so a failure leaves everything as it was.
	in := filepath.Join(dataDir, ".restore-new")
	os.RemoveAll(in)
	for _, f := range zr.File {
		if f.FileInfo().IsDir() || f.Name == "manifest.json" || !safeEntry(f.Name) {
			continue
		}
		if f.Name == envName && !opts.Settings {
			continue
		}
		if err := extract(&zr.Reader, f.Name, filepath.Join(in, filepath.FromSlash(f.Name))); err != nil {
			os.RemoveAll(in)
			return nil, fmt.Errorf("unpacking the backup: %w", err)
		}
	}
	if _, err := os.Stat(filepath.Join(in, dbName)); err != nil {
		os.RemoveAll(in)
		return nil, errors.New("the backup has no database")
	}

	// Move the current files aside. The -wal and -shm files go too: left
	// behind, SQLite would apply them to the restored database.
	old := filepath.Join(dataDir, oldDir)
	os.RemoveAll(old)
	if err := os.MkdirAll(old, 0o750); err != nil {
		os.RemoveAll(in)
		return nil, err
	}
	moves := []string{dbName, dbName + "-wal", dbName + "-shm"}
	moves = append(moves, identity...)
	var moved, placed []string
	// undo puts the current files back after a failure past this point,
	// so the server opens the library it had. When that fails too, the
	// error is ErrRestoreBroken: the server must not open (it would make
	// a new, empty library over the real one).
	undo := func(cause error) error {
		os.RemoveAll(in)
		var bad []string
		for _, n := range placed {
			if err := os.RemoveAll(filepath.Join(dataDir, n)); err != nil {
				bad = append(bad, n)
			}
		}
		for _, n := range moved {
			if err := os.Rename(filepath.Join(old, n), filepath.Join(dataDir, n)); err != nil {
				bad = append(bad, n)
			}
		}
		failed(pending)
		if len(bad) > 0 {
			err := fmt.Errorf("%w: %v; putting the previous files back failed too (%s): move them back from %s by hand, then delete %s", ErrRestoreBroken, cause, strings.Join(bad, ", "), old, filepath.Join(dataDir, brokenName))
			// Remembered, so a restart (the Windows service restarts on
			// failure) doesn't open an empty library either.
			os.WriteFile(filepath.Join(dataDir, brokenName), []byte(err.Error()+"\n"), 0o600)
			return err
		}
		return cause
	}
	for _, n := range moves {
		src := filepath.Join(dataDir, n)
		if _, err := os.Stat(src); err == nil {
			if err := os.Rename(src, filepath.Join(old, n)); err != nil {
				return nil, undo(fmt.Errorf("moving %s aside: %w", n, err))
			}
			moved = append(moved, n)
		}
	}
	if opts.Settings && envFile != "" {
		if _, err := os.Stat(envFile); err == nil {
			copyFile(envFile, filepath.Join(old, envName))
		}
	}

	// Put the backup's files in place.
	err = filepath.WalkDir(in, func(p string, e fs.DirEntry, err error) error {
		if err != nil || e.IsDir() {
			return err
		}
		rel, _ := filepath.Rel(in, p)
		if rel == envName {
			return nil // below
		}
		top := strings.SplitN(filepath.ToSlash(rel), "/", 2)[0]
		if !slices.Contains(placed, top) {
			placed = append(placed, top)
		}
		dst := filepath.Join(dataDir, rel)
		if err := os.MkdirAll(filepath.Dir(dst), 0o750); err != nil {
			return err
		}
		return placeFile(p, dst)
	})
	if err != nil {
		return nil, undo(fmt.Errorf("restoring: %w", err))
	}
	// The settings file, when asked. It may live where the server can't
	// write (/etc/kinara on Linux): then it's left for the owner to copy.
	if src := filepath.Join(in, envName); opts.Settings && envFile != "" {
		if _, err := os.Stat(src); err == nil {
			if err := copyFile(src, envFile); err != nil {
				keep := filepath.Join(dataDir, envName+".from-backup")
				copyFile(src, keep)
				if log != nil {
					log.Warn("restore: couldn't replace the settings file; the backup's copy is next to the database", "file", keep, "err", err)
				}
			}
		}
	}
	os.RemoveAll(in)
	os.Remove(pending)
	os.Remove(filepath.Join(dataDir, optionsName))
	if log != nil {
		log.Info("restored a backup", "made", time.Unix(man.CreatedAt, 0).Format(time.RFC3339),
			"version", man.Version, "host", man.Host, "previous files", old)
	}
	return &man, nil
}

// ErrRestoreBroken is ApplyPending failing in a way that left the data
// folder without its database. The server must not start on it.
var ErrRestoreBroken = errors.New("restoring a backup failed partway")

// placeFile moves a restored file into place (a variable for tests).
var placeFile = os.Rename

// brokenName marks a data folder ApplyPending left without its database.
const brokenName = "restore-broken.txt"

// failed renames a staged backup that couldn't be restored, so the next
// start doesn't try it again.
func failed(pending string) {
	os.Remove(pending + ".failed")
	os.Rename(pending, pending+".failed")
}

func copyFile(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.OpenFile(dst, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		out.Close()
		os.Remove(dst)
		return err
	}
	return out.Close()
}
