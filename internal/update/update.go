// Package update checks for new Kinara releases and installs them.
//
// Releases are listed in latest.json on the downloads site (see
// packaging/publish-downloads.sh):
//
//	{ "version": "0.2.0", "date": "2026-10-20", "notes": "https://...",
//	  "files": { "windows-x64": { "name", "url", "latest", "size", "sha256" }, ... } }
//
// The server reads latest-signed.json, the same list with an Ed25519
// signature, so the list (and so each installer's checksum) is known to
// come from Kinara's release key, not just from whoever controls the
// downloads site:
//
//	{ "payload": "<latest.json, base64>", "signature": "<base64>" }
//
// The public half of the key is update-key.pub in this folder, built into
// the server. A build without one can check for updates but not install
// them.
//
// Installing downloads the installer for this computer into the updates
// folder (which only this account and administrators can change), checks
// its size and SHA-256 against the signed list (and, on macOS, that
// Macroboom LLC signed it and Apple notarized it), checks it again right
// before starting it, then starts it:
// silently on Windows (the Kinara installer stops the server and starts the
// new one), with macOS's own password prompt on a Mac. The new server
// empties the updates folder when it starts, so no installer is left behind.
//
// The .deb and Docker installs are updated by apt and docker; for those
// Status says how instead.
package update

import (
	"context"
	"crypto/ed25519"
	"crypto/sha256"
	_ "embed"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"time"

	"kinara/internal/fsacl"
)

// DefaultURL is where releases are listed, signed.
const DefaultURL = "https://dl.kinara.media/server/latest-signed.json"

//go:embed update-key.pub
var keyFile string

// publicKey checks the release list's signature (nil: this build has no
// key, so it can't install updates). A variable for tests.
var publicKey = parseKey(keyFile)

// parseKey reads update-key.pub: the raw 32-byte Ed25519 public key in
// base64, on the first line that isn't a # comment.
func parseKey(s string) ed25519.PublicKey {
	for _, line := range strings.Split(s, "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		b, err := base64.StdEncoding.DecodeString(line)
		if err != nil || len(b) != ed25519.PublicKeySize {
			return nil
		}
		return ed25519.PublicKey(b)
	}
	return nil
}

// errNoKey is installing from a build with no release key.
var errNoKey = errors.New("this build of Kinara has no release key, so it can't check that an update is genuine")

// Signed is latest-signed.json.
type Signed struct {
	Payload   string `json:"payload"`
	Signature string `json:"signature"`
}

// maxInstaller caps a download: a bigger file is not one of ours.
const maxInstaller = 1 << 30

// Release is latest.json.
type Release struct {
	Version string          `json:"version"`
	Date    string          `json:"date"`
	Notes   string          `json:"notes"`
	Files   map[string]File `json:"files"`
}

// File is one installer in a release.
type File struct {
	Name   string `json:"name"`
	URL    string `json:"url"`
	Latest string `json:"latest"`
	Size   int64  `json:"size"`
	SHA256 string `json:"sha256"`
}

// Kinds of install, which decide how an update is put in.
const (
	KindWindows = "windows" // the Kinara installer (Inno Setup)
	KindMacOS   = "macos"   // Kinara.pkg
	KindDeb     = "deb"     // the apt package
	KindDocker  = "docker"  // the container image
	KindOther   = "other"   // built from source, or copied by hand
)

// Install states.
const (
	StateIdle        = "idle"
	StateDownloading = "downloading"
	StateVerifying   = "verifying"
	StateInstalling  = "installing"
	StateFailed      = "failed"
)

// Status is what the manager page shows.
type Status struct {
	Current   string `json:"current"`
	Latest    string `json:"latest,omitempty"`
	Available bool   `json:"available"`
	Date      string `json:"date,omitempty"`
	Notes     string `json:"notes,omitempty"`
	// CheckedAt is Unix seconds of the last check that answered (0: never).
	CheckedAt int64  `json:"checkedAt"`
	Error     string `json:"error,omitempty"`
	// Kind is how this copy was installed; CanInstall says whether Kinara
	// can install an update itself, How what to do otherwise.
	Kind       string `json:"kind"`
	CanInstall bool   `json:"canInstall"`
	How        string `json:"how,omitempty"`
	// State is the install in progress: idle, downloading, verifying,
	// installing or failed (Message says why).
	State   string `json:"state"`
	Message string `json:"message,omitempty"`
	// Downloaded and Size are bytes, while downloading.
	Downloaded int64 `json:"downloaded,omitempty"`
	Size       int64 `json:"size,omitempty"`
}

// Updater checks for and installs releases. Its zero value is not usable:
// set Current and Dir.
type Updater struct {
	// URL is latest.json (DefaultURL when empty).
	URL string
	// Current is this server's version.
	Current string
	// Dir is the updates folder (the installer is downloaded here).
	Dir string
	// Kind is how this copy was installed (Detect() when empty).
	Kind   string
	Client *http.Client
	Log    *slog.Logger
	// Before runs right before the installer starts (a backup, say). If
	// it fails the update still goes ahead; the failure is logged.
	Before func(ctx context.Context) error

	mu      sync.Mutex
	release *Release
	// signed is whether release came with a good signature.
	signed    bool
	checkedAt time.Time
	checkErr  string
	state     string
	message   string
	got, size int64
}

func (u *Updater) kind() string {
	if u.Kind == "" {
		u.Kind = Detect()
	}
	return u.Kind
}

func (u *Updater) client() *http.Client {
	if u.Client != nil {
		return u.Client
	}
	return &http.Client{Timeout: 30 * time.Minute}
}

func (u *Updater) log() *slog.Logger {
	if u.Log != nil {
		return u.Log
	}
	return slog.New(slog.DiscardHandler)
}

// fileKey is this computer's entry in latest.json ("" when there is none).
func fileKey(kind string) string {
	switch kind {
	case KindWindows:
		if runtime.GOARCH == "amd64" {
			return "windows-x64"
		}
	case KindMacOS:
		return "macos"
	case KindDeb:
		return "linux-" + runtime.GOARCH + "-deb"
	}
	return ""
}

// how says how to update an install Kinara can't update itself.
func how(kind string) string {
	switch kind {
	case KindDeb:
		return "sudo apt update && sudo apt install --only-upgrade kinara-server"
	case KindDocker:
		return "docker compose pull && docker compose up -d"
	}
	return "Download the new version from https://kinara.media/#download and install it over this one. Your library and settings stay."
}

// Status reports what's known.
func (u *Updater) Status() Status {
	u.mu.Lock()
	defer u.mu.Unlock()
	k := u.kind()
	st := Status{Current: u.Current, Kind: k, State: u.state, Message: u.message, Error: u.checkErr}
	if st.State == "" {
		st.State = StateIdle
	}
	if !u.checkedAt.IsZero() {
		st.CheckedAt = u.checkedAt.Unix()
	}
	if r := u.release; r != nil {
		st.Latest, st.Date, st.Notes = r.Version, r.Date, r.Notes
		st.Available = Newer(r.Version, u.Current)
		_, has := r.Files[fileKey(k)]
		st.CanInstall = st.Available && has && canLaunch(k) && !isDev(u.Current) && u.signed
	}
	if !st.CanInstall {
		st.How = how(k)
	}
	if st.State == StateDownloading {
		st.Downloaded, st.Size = u.got, u.size
	}
	return st
}

// Check reads latest.json. It returns the release (nil on error).
func (u *Updater) Check(ctx context.Context) (*Release, error) {
	r, signed, err := u.fetch(ctx)
	u.mu.Lock()
	defer u.mu.Unlock()
	if err != nil {
		u.checkErr = "Couldn't check for updates: " + err.Error()
		return nil, err
	}
	u.release, u.signed, u.checkErr, u.checkedAt = r, signed, "", time.Now()
	return r, nil
}

func (u *Updater) manifestURL() string {
	if u.URL != "" {
		return u.URL
	}
	return DefaultURL
}

// fetch reads the release list. With a release key built in, the list must
// be signed by it; without one, a signed or plain list is read but signed
// is false (it can be shown, not installed).
func (u *Updater) fetch(ctx context.Context) (r *Release, signed bool, err error) {
	if err := secureURL(u.manifestURL()); err != nil {
		return nil, false, err
	}
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.manifestURL(), nil)
	if err != nil {
		return nil, false, err
	}
	req.Header.Set("User-Agent", "kinara-server/"+u.Current+" ("+runtime.GOOS+"; "+runtime.GOARCH+")")
	resp, err := u.client().Do(req)
	if err != nil {
		return nil, false, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, false, fmt.Errorf("the downloads site answered %s", resp.Status)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return nil, false, err
	}
	payload := body
	var env Signed
	if json.Unmarshal(body, &env) == nil && env.Payload != "" {
		p, perr := base64.StdEncoding.DecodeString(env.Payload)
		sig, serr := base64.StdEncoding.DecodeString(env.Signature)
		if perr != nil || serr != nil {
			return nil, false, errors.New("unreadable signed release list")
		}
		payload = p
		signed = publicKey != nil && ed25519.Verify(publicKey, p, sig)
		if publicKey != nil && !signed {
			return nil, false, errors.New("the release list's signature doesn't match Kinara's release key")
		}
	} else if publicKey != nil {
		return nil, false, errors.New("the release list isn't signed")
	}
	var rel Release
	if err := json.Unmarshal(payload, &rel); err != nil {
		return nil, false, fmt.Errorf("unreadable release list: %w", err)
	}
	if _, ok := parse(rel.Version); !ok {
		return nil, false, fmt.Errorf("unreadable version %q in the release list", rel.Version)
	}
	return &rel, signed, nil
}

// secureURL refuses a plain http address, except on this computer (tests,
// a local mirror).
func secureURL(raw string) error {
	p, err := url.Parse(raw)
	if err != nil {
		return err
	}
	if p.Scheme == "https" || (p.Scheme == "http" && loopback(p.Hostname())) {
		return nil
	}
	return fmt.Errorf("updates are only fetched over https, not from %s", raw)
}

func loopback(host string) bool {
	if host == "localhost" {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

// ErrBusy means an install is already under way.
var ErrBusy = errors.New("an update is already being installed")

// Install downloads, checks and starts the installer for the newest release.
// It returns once the download starts; Status follows it. The server is
// stopped by the installer, so on success this process ends.
func (u *Updater) Install(ctx context.Context) error {
	u.mu.Lock()
	if u.state == StateDownloading || u.state == StateVerifying || u.state == StateInstalling {
		u.mu.Unlock()
		return ErrBusy
	}
	r := u.release
	k := u.kind()
	if r == nil || !Newer(r.Version, u.Current) {
		u.mu.Unlock()
		return errors.New("there's no newer version to install")
	}
	f, ok := r.Files[fileKey(k)]
	if !ok || !canLaunch(k) || isDev(u.Current) {
		u.mu.Unlock()
		return errors.New("this copy of Kinara can't update itself: " + how(k))
	}
	if !u.signed {
		u.mu.Unlock()
		if publicKey == nil {
			return fmt.Errorf("%w: %s", errNoKey, how(k))
		}
		return errors.New("the release list isn't signed, so the update wasn't installed")
	}
	if err := u.checkFile(f); err != nil {
		u.mu.Unlock()
		return err
	}
	u.state, u.message, u.got, u.size = StateDownloading, "", 0, f.Size
	u.mu.Unlock()

	go func() {
		path, err := u.download(ctx, f)
		if err == nil {
			u.set(StateVerifying, "")
			err = verify(ctx, k, path)
		}
		if err == nil {
			u.set(StateInstalling, "")
			if u.Before != nil {
				if berr := u.Before(ctx); berr != nil {
					u.log().Warn("update: before installing", "err", berr)
				}
			}
			// The backup can take a while: check the file is still the
			// one downloaded.
			err = checkSum(path, f.SHA256)
		}
		if err == nil {
			u.log().Info("update: starting the installer", "version", r.Version, "file", path)
			err = launch(k, path, r.Version, f.SHA256, u.Dir, func(err error) {
				// The installer ended and we're still running: it failed or
				// was cancelled before it stopped the server.
				_ = os.Remove(path)
				if err == nil {
					err = errors.New("the installer ended without updating Kinara")
				}
				u.log().Warn("update: installer", "err", err)
				u.set(StateFailed, err.Error())
			})
		}
		if err != nil {
			_ = os.Remove(path)
			u.log().Warn("update failed", "version", r.Version, "err", err)
			u.set(StateFailed, err.Error())
		}
	}()
	return nil
}

func (u *Updater) set(state, msg string) {
	u.mu.Lock()
	u.state, u.message = state, msg
	u.mu.Unlock()
}

// checkFile refuses an entry that isn't a plain https download from the
// same site as latest.json, with a size and checksum.
func (u *Updater) checkFile(f File) error {
	base, err := url.Parse(u.manifestURL())
	if err != nil {
		return err
	}
	dl, err := url.Parse(f.URL)
	if err != nil || dl.Host != base.Host || secureURL(f.URL) != nil {
		return fmt.Errorf("the release list points somewhere unexpected: %s", f.URL)
	}
	if f.Size <= 0 || f.Size > maxInstaller {
		return errors.New("the release list gives no usable size for the installer")
	}
	if len(f.SHA256) != 64 {
		return errors.New("the release list gives no checksum for the installer")
	}
	name := filepath.Base(f.Name)
	if name != f.Name || name == "." || strings.ContainsAny(name, `/\:`) {
		return fmt.Errorf("unexpected installer name %q", f.Name)
	}
	return nil
}

// download fetches the installer into Dir, checking its size and SHA-256.
func (u *Updater) download(ctx context.Context, f File) (string, error) {
	if err := os.MkdirAll(u.Dir, 0o700); err != nil {
		return "", err
	}
	// Other accounts on this computer mustn't be able to swap the
	// installer before it runs (as an administrator).
	if err := fsacl.Private(u.Dir); err != nil {
		return "", fmt.Errorf("couldn't protect the updates folder: %w", err)
	}
	final := filepath.Join(u.Dir, f.Name)
	part := final + ".part"
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, f.URL, nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("User-Agent", "kinara-server/"+u.Current)
	resp, err := u.client().Do(req)
	if err != nil {
		return "", fmt.Errorf("download: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("download: the downloads site answered %s", resp.Status)
	}
	out, err := os.Create(part)
	if err != nil {
		return "", err
	}
	h := sha256.New()
	buf := make([]byte, 256<<10)
	var n int64
	for {
		m, rerr := resp.Body.Read(buf)
		if m > 0 {
			n += int64(m)
			if n > f.Size {
				out.Close()
				os.Remove(part)
				return "", errors.New("the download is bigger than the release list says")
			}
			h.Write(buf[:m])
			if _, err := out.Write(buf[:m]); err != nil {
				out.Close()
				os.Remove(part)
				return "", err
			}
			u.mu.Lock()
			u.got = n
			u.mu.Unlock()
		}
		if rerr == io.EOF {
			break
		}
		if rerr != nil {
			out.Close()
			os.Remove(part)
			return "", fmt.Errorf("download: %w", rerr)
		}
	}
	if err := out.Close(); err != nil {
		os.Remove(part)
		return "", err
	}
	if n != f.Size {
		os.Remove(part)
		return "", fmt.Errorf("the download stopped early (%d of %d bytes)", n, f.Size)
	}
	if got := hex.EncodeToString(h.Sum(nil)); !strings.EqualFold(got, f.SHA256) {
		os.Remove(part)
		return "", errors.New("the download doesn't match its checksum, so it wasn't installed")
	}
	if err := os.Rename(part, final); err != nil {
		os.Remove(part)
		return "", err
	}
	return final, nil
}

// checkSum checks a downloaded installer against its SHA-256 again.
func checkSum(path, want string) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return err
	}
	if !strings.EqualFold(hex.EncodeToString(h.Sum(nil)), want) {
		return errors.New("the installer changed after it was downloaded, so it wasn't installed")
	}
	return nil
}

// Cleanup empties the updates folder: installers and partial downloads
// from an earlier update (the installer's log is kept). Run it when the
// server starts. An installer still running holds its file open on
// Windows, so it tries again for a few minutes.
func (u *Updater) Cleanup(ctx context.Context) {
	left := u.sweep()
	for i := 0; left > 0 && i < 10; i++ {
		select {
		case <-ctx.Done():
			return
		case <-time.After(30 * time.Second):
		}
		left = u.sweep()
	}
}

// sweep removes what it can and returns how many files are left.
func (u *Updater) sweep() int {
	entries, err := os.ReadDir(u.Dir)
	if err != nil {
		return 0
	}
	left := 0
	for _, e := range entries {
		if e.IsDir() || strings.HasSuffix(e.Name(), ".log") {
			continue
		}
		p := filepath.Join(u.Dir, e.Name())
		if err := os.Remove(p); err != nil {
			left++
		} else {
			u.log().Info("update: removed the installer of an earlier update", "file", e.Name())
		}
	}
	return left
}

// --- Versions ---------------------------------------------------------------------

type version struct {
	nums [3]int
	pre  string
}

func parse(v string) (version, bool) {
	v = strings.TrimPrefix(strings.TrimSpace(v), "v")
	if i := strings.IndexByte(v, '+'); i >= 0 {
		v = v[:i]
	}
	var out version
	core := v
	if i := strings.IndexByte(v, '-'); i >= 0 {
		core, out.pre = v[:i], v[i+1:]
	}
	parts := strings.Split(core, ".")
	if len(parts) == 0 || len(parts) > 3 {
		return out, false
	}
	for i, p := range parts {
		n, err := strconv.Atoi(p)
		if err != nil || n < 0 {
			return out, false
		}
		out.nums[i] = n
	}
	return out, true
}

// Newer reports whether a is a later version than b. Unreadable versions
// are never newer. A pre-release (0.2.0-rc.1) comes before its release.
func Newer(a, b string) bool {
	va, ok := parse(a)
	if !ok {
		return false
	}
	vb, ok := parse(b)
	if !ok {
		return true
	}
	for i := range va.nums {
		if va.nums[i] != vb.nums[i] {
			return va.nums[i] > vb.nums[i]
		}
	}
	switch {
	case va.pre == vb.pre:
		return false
	case va.pre == "":
		return true
	case vb.pre == "":
		return false
	}
	return comparePre(va.pre, vb.pre) > 0
}

// comparePre orders pre-release labels as semver does: dot-separated parts,
// numbers numerically and below words.
func comparePre(a, b string) int {
	pa, pb := strings.Split(a, "."), strings.Split(b, ".")
	for i := 0; i < len(pa) && i < len(pb); i++ {
		na, ea := strconv.Atoi(pa[i])
		nb, eb := strconv.Atoi(pb[i])
		switch {
		case ea == nil && eb == nil:
			if na != nb {
				if na < nb {
					return -1
				}
				return 1
			}
		case ea == nil:
			return -1
		case eb == nil:
			return 1
		default:
			if c := strings.Compare(pa[i], pb[i]); c != 0 {
				return c
			}
		}
	}
	return len(pa) - len(pb)
}

// isDev is a version made from the source, not a release.
func isDev(v string) bool { return strings.Contains(v, "dev") }

// --- How this copy was installed ------------------------------------------------------

// Detect works out how this copy of Kinara was installed, from where its
// program file is.
func Detect() string {
	exe, err := os.Executable()
	if err != nil {
		return KindOther
	}
	if p, err := filepath.EvalSymlinks(exe); err == nil {
		exe = p
	}
	dir := filepath.Dir(exe)
	switch runtime.GOOS {
	case "windows":
		// Inno Setup puts its uninstaller next to the programs.
		if found, _ := filepath.Glob(filepath.Join(dir, "unins*.exe")); len(found) > 0 {
			if _, err := os.Stat(filepath.Join(dir, "kinara-tray.exe")); err == nil {
				return KindWindows
			}
		}
	case "darwin":
		if exe == "/usr/local/kinara/kinara-server" {
			return KindMacOS
		}
	case "linux":
		// Docker, and Podman.
		for _, f := range []string{"/.dockerenv", "/run/.containerenv"} {
			if _, err := os.Stat(f); err == nil {
				return KindDocker
			}
		}
		if exe == "/usr/bin/kinara-server" {
			return KindDeb
		}
	}
	return KindOther
}
