// Command kinara-server runs the Kinara media server.
//
//	kinara-server [serve]           run the server (default)
//	kinara-server user add          create a user
//	kinara-server user passwd       change a password
//	kinara-server library add       add a library folder
//	kinara-server library list      list libraries
//	kinara-server scan              scan libraries now and wait
//	kinara-server service           run under the Windows service manager (the installer sets this up)
//
// Settings come from flags or environment variables:
//
//	-data     KINARA_DATA      data folder (database, settings)      ./kinara-data
//	-cache    KINARA_CACHE     cache folder (images, conversions,    DATA/cache
//	                           preview thumbnails); can grow large
//	-listen   KINARA_LISTEN    address to listen on                  :8420
//	-tmdb-key KINARA_TMDB_KEY  your own TMDB API key or read access   the key built into release builds
//	                           token, used instead of the built-in one
//	-ffprobe  KINARA_FFPROBE   path to ffprobe                       ffmpeg\ffprobe next to the server, else
//	                                                                  Jellyfin's FFmpeg (Linux), else on PATH
//	-ffmpeg   KINARA_FFMPEG    path to ffmpeg                        next to ffprobe, or on PATH
//
// Background features (on unless set to 0 or off):
//
//	KINARA_TRANSCODE   convert files the TV can't play
//	KINARA_TRICKPLAY   preview thumbnails for skipping (about 5 MB per movie)
//	KINARA_INTROS      find TV intros for "Skip intro"
//	KINARA_WATCH       seconds between checks of library folders for new files (default 60)
//
// Limits:
//
//	KINARA_MAX_TRANSCODES  conversions at once (default 6 with a graphics card encoder, else 3)
//	KINARA_HWACCEL         graphics card for converting: auto (default), nvenc, qsv,
//	                       vaapi, amf, videotoolbox, or none for the CPU only
//	KINARA_HW_DEVICE       Linux render node to use (default: each in turn)
//
// Behind a reverse proxy (Caddy, nginx) on a public address:
//
//	KINARA_TRUSTED_PROXIES  addresses or ranges of the proxies (such as 127.0.0.1),
//	                        so the visitor's own address is read from
//	                        X-Forwarded-For. Visitors from public addresses can
//	                        sign in but can't set the server up or make accounts.
//
// Remote access (turned on in the web manager, under Remote access):
//
//	KINARA_CLOUD_URL       the Kinara cloud for accounts and remote play (default
//	                       https://cloud.kinara.media; off turns it off)
//	KINARA_IROH            path to kinara-iroh (default: next to the server, or on PATH)
//	KINARA_TUNNEL_LISTEN   loopback address kinara-iroh delivers connections to (default 127.0.0.1:8421)
//
// Subtitles (also set in the web manager, under Subtitles):
//
//	KINARA_OPENSUBTITLES_KEY  an OpenSubtitles.com API key, used instead of the one built into releases
//
// Updates (checking can also be turned off in the web manager, under Updates):
//
//	KINARA_UPDATE_URL      the list of releases (default https://dl.kinara.media/server/latest.json;
//	                       off turns update checks off)
package main

import (
	"bufio"
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"kinara/internal/api"
	"kinara/internal/auth"
	"kinara/internal/backup"
	"kinara/internal/cloud"
	"kinara/internal/ffmpeg"
	"kinara/internal/images"
	"kinara/internal/jobs"
	"kinara/internal/match"
	"kinara/internal/metrics"
	"kinara/internal/pgs"
	"kinara/internal/probe"
	"kinara/internal/remote"
	"kinara/internal/scanner"
	"kinara/internal/store"
	"kinara/internal/tmdb"
	"kinara/internal/transcode"
	"kinara/internal/trickplay"
	"kinara/internal/update"
	"kinara/internal/watch"
	"kinara/internal/watcher"
)

// version is what the server reports. Release builds set it from the tag
// (-ldflags "-X main.version=0.2.0"); build.ps1 reads this line.
var version = "0.1.0-dev"

// defaultCloudURL is the Kinara cloud a server links to unless
// KINARA_CLOUD_URL says otherwise ("off" turns accounts and remote play off).
// Nothing is sent to it until the owner links the server under Remote access.
const defaultCloudURL = "https://cloud.kinara.media"

type config struct {
	dataDir  string
	cacheDir string
	listen   string
	tmdbKey  string
	ffprobe  string
	ffmpeg   string
	debug    bool
	rescan   time.Duration
	workers  int
}

// cache is a folder inside the cache folder (KINARA_CACHE, else DATA/cache).
func (c config) cache(name string) string {
	root := c.cacheDir
	if root == "" {
		root = filepath.Join(c.dataDir, "cache")
	}
	return filepath.Join(root, name)
}

// bundledTool finds a tool shipped with the server: ffmpeg\ffprobe.exe (or
// ffmpeg/ffprobe) in the server's own folder, as the Windows installer lays
// it out, else beside the server, else (Linux) Jellyfin's FFmpeg build, which
// the Docker image and the .deb use for its HDR tone mapping and hardware
// drivers. "" means look on PATH.
func bundledTool(name string) string {
	exe, err := os.Executable()
	if err != nil {
		return ""
	}
	if runtime.GOOS == "windows" {
		name += ".exe"
	}
	dir := filepath.Dir(exe)
	candidates := []string{filepath.Join(dir, "ffmpeg", name), filepath.Join(dir, name)}
	if runtime.GOOS == "linux" {
		candidates = append(candidates, filepath.Join("/usr/lib/jellyfin-ffmpeg", name))
	}
	for _, p := range candidates {
		if fi, err := os.Stat(p); err == nil && !fi.IsDir() {
			return p
		}
	}
	return ""
}

func env(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

// builtinTMDBKey is Kinara's own TMDB key, so posters and descriptions work
// without anyone signing up for one. It's set when release builds are made,
//
//	go build -ldflags "-X main.builtinTMDBKey=<key>" ./cmd/kinara-server
//
// and empty in the source, so the key never lands in the repository.
// KINARA_TMDB_KEY (or -tmdb-key) replaces it with your own.
var builtinTMDBKey string

// builtinOpenSubtitlesKey is Kinara's OpenSubtitles.com API key, for
// finding subtitles, set like builtinTMDBKey
// (-X main.builtinOpenSubtitlesKey=<key>). KINARA_OPENSUBTITLES_KEY, or a
// key the owner enters under Manage, Subtitles, replaces it.
var builtinOpenSubtitlesKey string

func commonFlags(fs *flag.FlagSet, c *config) {
	fs.StringVar(&c.dataDir, "data", env("KINARA_DATA", "./kinara-data"), "data folder")
	fs.StringVar(&c.cacheDir, "cache", env("KINARA_CACHE", ""), "cache folder (default: the data folder's cache)")
	fs.StringVar(&c.tmdbKey, "tmdb-key", env("KINARA_TMDB_KEY", builtinTMDBKey), "your own TMDB API key or read access token (default: the one built in)")
	fs.StringVar(&c.ffprobe, "ffprobe", env("KINARA_FFPROBE", bundledTool("ffprobe")), "path to ffprobe")
	fs.StringVar(&c.ffmpeg, "ffmpeg", env("KINARA_FFMPEG", ""), "path to ffmpeg (default: next to ffprobe, or on PATH)")
	fs.BoolVar(&c.debug, "debug", false, "verbose logging")
	workers, _ := strconv.Atoi(env("KINARA_SCAN_WORKERS", "0"))
	fs.IntVar(&c.workers, "scan-workers", workers, "files probed at once (0 = pick by drive type: SSD up to 8, hard drive 2)")
}

func main() {
	if err := loadEnvFile(); err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
	args := os.Args[1:]
	cmd := "serve"
	if len(args) > 0 && !strings.HasPrefix(args[0], "-") {
		cmd, args = args[0], args[1:]
	}
	var err error
	switch cmd {
	case "serve":
		err = serve(args)
	case "service":
		err = runService(args)
	case "user":
		err = userCmd(args)
	case "library":
		err = libraryCmd(args)
	case "scan":
		err = scanCmd(args)
	case "version":
		fmt.Println(version)
	case "help", "-h", "--help":
		fmt.Println(usage)
	default:
		err = fmt.Errorf("unknown command %q\n\n%s", cmd, usage)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}

const usage = `Usage:
  kinara-server [serve] [-listen :8420] [-data DIR] [-tmdb-key KEY]
  kinara-server user add -username NAME [-password PASS] [-admin]
  kinara-server user passwd -username NAME [-password PASS]
  kinara-server library add -kind movies|tv [-name NAME] PATH
  kinara-server library list
  kinara-server scan [-library ID]
  kinara-server service [serve flags]   (Windows: started by the service manager)
  kinara-server version`

type deps struct {
	store   *store.Store
	ffmpeg  *ffmpeg.Tool
	tmdb    *tmdb.Client
	prober  *probe.Prober
	matcher *match.Matcher
	scanner *scanner.Scanner
	images  *images.Cache
	log     *slog.Logger
	logs    *metrics.LogRing
}

func open(c config) (*deps, error) {
	level := slog.LevelInfo
	if c.debug {
		level = slog.LevelDebug
	}
	// Warnings and errors are also kept for the metrics page.
	logs := &metrics.LogRing{}
	log := slog.New(logs.Handler(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: level})))
	if err := os.MkdirAll(c.dataDir, 0o755); err != nil {
		return nil, err
	}
	st, err := store.Open(filepath.Join(c.dataDir, "kinara.db"))
	if err != nil {
		return nil, err
	}
	d := &deps{store: st, log: log, logs: logs}
	d.prober = &probe.Prober{Binary: c.ffprobe}
	// KINARA_HWACCEL picks the graphics card backend (auto, nvenc, qsv,
	// vaapi, amf, videotoolbox, none); KINARA_HW_DEVICE the Linux render
	// node (/dev/dri/renderD128).
	d.ffmpeg = &ffmpeg.Tool{Binary: ffmpegPath(c), HWAccel: env("KINARA_HWACCEL", "auto"), Device: env("KINARA_HW_DEVICE", ""), Log: log}
	if c.tmdbKey != "" {
		d.tmdb = tmdb.New(c.tmdbKey)
		d.matcher = &match.Matcher{Store: st, Src: d.tmdb, Log: log}
		source := "your own key"
		if builtinTMDBKey != "" && c.tmdbKey == builtinTMDBKey {
			source = "Kinara's built-in key"
		}
		log.Info("metadata: TMDB, using " + source)
	}
	d.scanner = &scanner.Scanner{Store: st, Prober: d.prober, Matcher: d.matcher, Log: log, Workers: c.workers}
	d.images = images.New(c.cache("images"))
	return d, nil
}

// ffmpegPath is -ffmpeg if set, else ffmpeg in the same folder as a given
// ffprobe (they come together), else "" for ffmpeg on PATH.
func ffmpegPath(c config) string {
	if c.ffmpeg != "" {
		return c.ffmpeg
	}
	if c.ffprobe == "" {
		return ""
	}
	dir, file := filepath.Split(c.ffprobe)
	name := strings.Replace(strings.ToLower(file), "ffprobe", "ffmpeg", 1)
	if name == strings.ToLower(file) {
		return ""
	}
	candidate := filepath.Join(dir, name)
	if _, err := os.Stat(candidate); err == nil {
		return candidate
	}
	return ""
}

// feature reads an on/off environment setting (on unless "0", "off",
// "false" or "no").
func feature(key string) bool {
	switch strings.ToLower(strings.TrimSpace(os.Getenv(key))) {
	case "0", "off", "false", "no":
		return false
	}
	return true
}

func serve(args []string) error {
	ctx := context.Background()
	// KINARA_STOP_ON_EOF=1: a parent (the Windows tray, in "at sign-in"
	// mode) holds our stdin open; when it closes it, or the parent dies, we
	// shut down cleanly. Windows has no SIGTERM to send a windowless child.
	if env("KINARA_STOP_ON_EOF", "") == "1" {
		var cancel context.CancelFunc
		ctx, cancel = context.WithCancel(ctx)
		defer cancel()
		go func() {
			_, _ = io.Copy(io.Discard, os.Stdin)
			cancel()
		}()
	}
	err := serveUntil(ctx, args)
	if errors.Is(err, errRestart) {
		// Start again in place (see restart_*.go).
		return restartSelf()
	}
	return err
}

// errRestart is what serveUntil returns when the server asked to be
// restarted (to restore a backup). The Windows service reports it as a
// failure, so the service manager starts it again.
var errRestart = errors.New("restarting")

// serveUntil runs the server until parent ends (the Windows service manager
// stopping it) or the process is interrupted.
func serveUntil(parent context.Context, args []string) error {
	var c config
	fs := flag.NewFlagSet("serve", flag.ExitOnError)
	commonFlags(fs, &c)
	fs.StringVar(&c.listen, "listen", env("KINARA_LISTEN", ":8420"), "address to listen on")
	fs.DurationVar(&c.rescan, "rescan", 6*time.Hour, "rescan libraries this often (0 to turn off)")
	fs.Parse(args)

	// A backup staged for restoring goes in place before the database opens.
	envFile := os.Getenv("KINARA_ENV_FILE")
	restored, restoreErr := backup.ApplyPending(c.dataDir, envFile, slog.Default())

	d, err := open(c)
	if err != nil {
		return err
	}
	defer d.store.Close()

	ctx, stop := signal.NotifyContext(parent, os.Interrupt, syscall.SIGTERM)
	defer stop()

	var warnings []string
	if err := d.prober.Available(); err != nil {
		warnings = append(warnings, "ffprobe was not found. Install ffmpeg (which includes ffprobe) or pass -ffprobe /path/to/ffprobe; files can't be read without it.")
	}
	if d.matcher == nil {
		warnings = append(warnings, "No TMDB key: this build has none built in and KINARA_TMDB_KEY isn't set, so titles come from file names and there's no artwork. Set KINARA_TMDB_KEY to your own key.")
	}
	ffmpegOK := d.ffmpeg.Available() == nil
	if !ffmpegOK {
		warnings = append(warnings, "ffmpeg was not found, so files the TV can't play won't be converted, and there are no preview thumbnails or Skip intro. Install ffmpeg or pass -ffmpeg /path/to/ffmpeg.")
	}
	if restoreErr != nil {
		warnings = append(warnings, "Restoring a backup failed, so the library is as it was: "+restoreErr.Error())
	} else if restored != nil {
		d.log.Info("this server was restored from a backup", "made", time.Unix(restored.CreatedAt, 0).Format(time.RFC3339), "on", restored.Host)
	}
	for _, w := range warnings {
		d.log.Warn(w)
	}

	// The web app: KINARA_WEB, or a folder named web next to the server.
	webDir := env("KINARA_WEB", "")
	if webDir == "" {
		if exe, err := os.Executable(); err == nil {
			if fi, err := os.Stat(filepath.Join(filepath.Dir(exe), "web", "index.html")); err == nil && !fi.IsDir() {
				webDir = filepath.Join(filepath.Dir(exe), "web")
			}
		}
	}

	srv := &api.Server{
		Store: d.store, Scanner: d.scanner, Matcher: d.matcher, Images: d.images,
		Watch: &watch.Service{Store: d.store}, Log: d.log, Version: version,
		BaseCtx: ctx, Warnings: warnings, TMDB: d.tmdb, FFprobe: c.ffprobe, WebDir: webDir,
		LANPort: lanPortOf(c.listen),
		Logs:    d.logs, DataDir: c.dataDir, CacheDir: c.cache(""),
	}
	// Backups: nightly, and from the manager page, which also restores them.
	srv.Backups = &backup.Manager{Store: d.store, DataDir: c.dataDir, EnvFile: envFile, Version: version, Log: d.log}
	restart := make(chan string, 1)
	srv.Restart = func(reason string) {
		select {
		case restart <- reason:
		default:
		}
	}
	go srv.RunBackups(ctx)
	// New releases: checked twice a day, installed from the manager page
	// (after a backup).
	if u := env("KINARA_UPDATE_URL", update.DefaultURL); u != "off" {
		srv.Updater = &update.Updater{URL: u, Current: version, Dir: c.cache("updates"), Log: d.log, Before: srv.BackupBeforeUpdate}
		go srv.RunUpdates(ctx)
	}
	// Readings for the metrics page: every 5 s, the last day in memory.
	srv.Metrics = &metrics.Collector{Streams: srv.StreamCounts}
	go srv.Metrics.Run(ctx)
	if ffmpegOK {
		// Check hardware encoders now rather than on the first stream.
		go d.ffmpeg.Caps()
		if feature("KINARA_TRANSCODE") {
			srv.Transcoder = &transcode.Transcoder{Tool: d.ffmpeg, FFprobe: c.ffprobe, Log: d.log}
			// KINARA_MAX_TRANSCODES caps conversions at once (default 6 with
			// a graphics card encoder, else 3).
			if n, err := strconv.Atoi(env("KINARA_MAX_TRANSCODES", "0")); err == nil && n > 0 {
				srv.Transcoder.MaxJobs = n
			}
			srv.Transcoder.Init()
			// HLS for iPhones and Safari, written under the cache (left over
			// sessions from a previous run are cleared first).
			hlsDir := c.cache("hls")
			_ = os.RemoveAll(hlsDir)
			srv.HLS = &transcode.HLS{T: srv.Transcoder, Dir: hlsDir}
		}
		srv.Subtitles = &transcode.Subtitles{Tool: d.ffmpeg, Dir: c.cache("subtitles")}
		srv.PGS = &pgs.Extractor{Tool: d.ffmpeg, FFprobe: c.ffprobe, Dir: c.cache("pgs"), Log: d.log}
		worker := &jobs.Worker{Store: d.store, Tool: d.ffmpeg, Log: d.log, Intros: feature("KINARA_INTROS"),
			ThumbDir: c.cache("thumbs")}
		if feature("KINARA_TRICKPLAY") {
			worker.Trickplay = &trickplay.Maker{Tool: d.ffmpeg, Dir: c.cache("trickplay")}
			srv.Trickplay = worker.Trickplay
		}
		// Always on: it also makes pictures for "Other videos" titles.
		{
			srv.Jobs = worker
			d.scanner.OnDone = func(int64) { worker.Kick() }
			go worker.Run(ctx)
		}
	}
	// Subtitles from OpenSubtitles (with Kinara's key, the owner's own, or
	// KINARA_OPENSUBTITLES_KEY), and how DVD subtitles look.
	srv.ApplySubtitleSettings(ctx, env("KINARA_OPENSUBTITLES_KEY", builtinOpenSubtitlesKey))
	if secs := env("KINARA_WATCH", "60"); secs != "0" && secs != "off" {
		every, err := strconv.Atoi(secs)
		if err != nil || every < 10 {
			every = 60
		}
		w := &watcher.Watcher{
			Interval: time.Duration(every) * time.Second,
			Log:      d.log,
			Roots: func(ctx context.Context) ([]watcher.Root, error) {
				libs, err := d.store.ListLibraries(ctx)
				if err != nil {
					return nil, err
				}
				roots := make([]watcher.Root, 0, len(libs))
				for _, l := range libs {
					for _, f := range l.Folders {
						roots = append(roots, watcher.Root{LibraryID: l.ID, Path: f})
					}
				}
				return roots, nil
			},
			OnChange: func(id int64) { go scanWhenFree(ctx, d, id) },
		}
		go w.Run(ctx)
	}
	// One handler for both listeners, so they share sign-in limits.
	handler := srv.Handler()
	startRemote(ctx, d, srv, handler, c.dataDir)
	startCloud(ctx, d, srv, handler, c)

	httpSrv := &http.Server{
		Addr:              c.listen,
		Handler:           handler,
		ReadHeaderTimeout: 10 * time.Second,
		IdleTimeout:       2 * time.Minute,
		// No write timeout: streams run for hours.
	}

	go housekeeping(ctx, d, c.rescan)

	errc := make(chan error, 1)
	go func() { errc <- httpSrv.ListenAndServe() }()
	if n, _ := d.store.CountUsers(ctx); n == 0 {
		d.log.Info("first run: open the server in a browser to create the admin account", "url", "http://localhost"+portOf(c.listen))
	}
	d.log.Info("Kinara server running", "version", version, "listen", c.listen, "data", c.dataDir, "cache", c.cache(""))

	select {
	case err := <-errc:
		if !errors.Is(err, http.ErrServerClosed) {
			return err
		}
	case <-ctx.Done():
		d.log.Info("shutting down")
		shutCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		return httpSrv.Shutdown(shutCtx)
	case reason := <-restart:
		d.log.Info("restarting", "why", reason)
		// Background work (the remote access sidecar, scans) ends with ctx.
		stop()
		shutCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_ = httpSrv.Shutdown(shutCtx)
		return errRestart
	}
	return nil
}

func portOf(listen string) string {
	if i := strings.LastIndex(listen, ":"); i >= 0 {
		return listen[i:]
	}
	return ":" + listen
}

// waitingScans holds libraries with a scan already queued by scanWhenFree,
// so changes seen during a long scan add up to one more scan, not many.
var waitingScans sync.Map

// scanWhenFree starts a scan for a library, waiting for a running one to
// finish first so a change seen mid-scan isn't missed.
func scanWhenFree(ctx context.Context, d *deps, id int64) {
	if _, queued := waitingScans.LoadOrStore(id, true); queued {
		return
	}
	defer waitingScans.Delete(id)
	for range 120 {
		err := d.scanner.Start(ctx, id)
		if !errors.Is(err, scanner.ErrBusy) {
			if err != nil {
				d.log.Warn("scan after change", "library", id, "err", err)
			}
			return
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(15 * time.Second):
		}
	}
}

// housekeeping rescans libraries on a schedule (first pass shortly after
// startup, to catch changes made while the server was off) and expires
// old sessions.
func housekeeping(ctx context.Context, d *deps, every time.Duration) {
	first := time.NewTimer(30 * time.Second)
	defer first.Stop()
	var tick <-chan time.Time
	if every > 0 {
		t := time.NewTicker(every)
		defer t.Stop()
		tick = t.C
	}
	run := func() {
		_ = d.store.ExpireSessions(ctx)
		if every <= 0 {
			return
		}
		libs, err := d.store.ListLibraries(ctx)
		if err != nil {
			d.log.Warn("rescan", "err", err)
			return
		}
		for _, l := range libs {
			if err := d.scanner.Start(ctx, l.ID); err != nil && !errors.Is(err, scanner.ErrBusy) {
				d.log.Warn("rescan", "library", l.Name, "err", err)
			}
		}
	}
	for {
		select {
		case <-ctx.Done():
			return
		case <-first.C:
			run()
		case <-tick:
			run()
		}
	}
}

func readPassword(flagValue string) (string, error) {
	if flagValue != "" {
		return flagValue, nil
	}
	if v := os.Getenv("KINARA_PASSWORD"); v != "" {
		return v, nil
	}
	fmt.Fprint(os.Stderr, "Password (shown as you type): ")
	line, err := bufio.NewReader(os.Stdin).ReadString('\n')
	if err != nil && line == "" {
		return "", err
	}
	return strings.TrimRight(line, "\r\n"), nil
}

func userCmd(args []string) error {
	if len(args) == 0 {
		return errors.New("user needs a subcommand: add or passwd")
	}
	sub, args := args[0], args[1:]
	var c config
	fs := flag.NewFlagSet("user "+sub, flag.ExitOnError)
	commonFlags(fs, &c)
	username := fs.String("username", "", "username")
	password := fs.String("password", "", "password (or KINARA_PASSWORD, or typed)")
	admin := fs.Bool("admin", false, "make the user an admin (the first user always is)")
	fs.Parse(args)
	if *username == "" {
		return errors.New("-username is required")
	}
	d, err := open(c)
	if err != nil {
		return err
	}
	defer d.store.Close()
	ctx := context.Background()
	pass, err := readPassword(*password)
	if err != nil {
		return err
	}
	hash, err := auth.HashPassword(pass)
	if err != nil {
		return err
	}
	switch sub {
	case "add":
		n, err := d.store.CountUsers(ctx)
		if err != nil {
			return err
		}
		u, err := d.store.CreateUser(ctx, *username, hash, *admin || n == 0)
		if err != nil {
			return err
		}
		fmt.Printf("Created user %s (admin: %v)\n", u.Username, u.IsAdmin)
	case "passwd":
		u, err := d.store.UserByName(ctx, *username)
		if err != nil {
			return fmt.Errorf("user %s not found", *username)
		}
		if err := d.store.SetPassword(ctx, u.ID, hash); err != nil {
			return err
		}
		fmt.Println("Password changed; the user was signed out everywhere.")
	default:
		return fmt.Errorf("unknown user subcommand %q", sub)
	}
	return nil
}

func libraryCmd(args []string) error {
	if len(args) == 0 {
		return errors.New("library needs a subcommand: add or list")
	}
	sub, args := args[0], args[1:]
	var c config
	fs := flag.NewFlagSet("library "+sub, flag.ExitOnError)
	commonFlags(fs, &c)
	name := fs.String("name", "", "library name (default: folder name)")
	kind := fs.String("kind", "", "movies or tv")
	fs.Parse(args)
	d, err := open(c)
	if err != nil {
		return err
	}
	defer d.store.Close()
	ctx := context.Background()
	switch sub {
	case "add":
		if fs.NArg() != 1 {
			return errors.New("give exactly one folder path")
		}
		if *kind != store.KindMovies && *kind != store.KindTV {
			return errors.New(`-kind must be "movies" or "tv"`)
		}
		path, err := filepath.Abs(fs.Arg(0))
		if err != nil {
			return err
		}
		if fi, err := os.Stat(path); err != nil || !fi.IsDir() {
			return fmt.Errorf("folder not found: %s", path)
		}
		n := *name
		if n == "" {
			n = filepath.Base(path)
		}
		lib, err := d.store.CreateLibrary(ctx, n, path, *kind)
		if err != nil {
			return err
		}
		fmt.Printf("Added library %d: %s (%s) at %s\nRun \"kinara-server scan\" or start the server to scan it.\n", lib.ID, lib.Name, lib.Kind, lib.Path)
	case "list":
		libs, err := d.store.ListLibraries(ctx)
		if err != nil {
			return err
		}
		for _, l := range libs {
			st, _ := d.store.LibraryStats(ctx, l.ID)
			fmt.Printf("%d\t%s\t%s\t%s\tmovies=%d shows=%d episodes=%d unmatched=%d files=%d\n",
				l.ID, l.Name, l.Kind, strings.Join(l.Folders, ", "), st.Movies, st.Shows, st.Episodes, st.Unmatched, st.Files)
		}
	default:
		return fmt.Errorf("unknown library subcommand %q", sub)
	}
	return nil
}

func scanCmd(args []string) error {
	var c config
	fs := flag.NewFlagSet("scan", flag.ExitOnError)
	commonFlags(fs, &c)
	only := fs.Int64("library", 0, "library ID (default: all)")
	fs.Parse(args)
	d, err := open(c)
	if err != nil {
		return err
	}
	defer d.store.Close()
	if err := d.prober.Available(); err != nil {
		return errors.New("ffprobe not found: install ffmpeg or pass -ffprobe")
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	libs, err := d.store.ListLibraries(ctx)
	if err != nil {
		return err
	}
	for _, l := range libs {
		if *only != 0 && l.ID != *only {
			continue
		}
		start := time.Now()
		p, err := d.scanner.Run(ctx, l.ID)
		if err != nil {
			return fmt.Errorf("%s: %w", l.Name, err)
		}
		fmt.Printf("%s: %d files, %d new, %d changed, %d skipped, %d missing, %d errors, %d matched, %d unmatched (%s)\n",
			l.Name, p.FilesSeen, p.New, p.Changed, p.Skipped, p.Missing, p.Errors, p.Matched, p.Unmatched,
			time.Since(start).Round(time.Second))
		if p.Message != "" {
			fmt.Println("  " + p.Message)
		}
	}
	return nil
}

// startCloud links the server to a Kinara account (when KINARA_CLOUD_URL
// is set) and keeps it reachable from anywhere: direct HTTPS with a
// certificate for its own names, a router port opened by UPnP or NAT-PMP,
// and the cloud's relay for players that can't connect directly.
func startCloud(ctx context.Context, d *deps, srv *api.Server, handler http.Handler, c config) {
	url := env("KINARA_CLOUD_URL", defaultCloudURL)
	if url == "" || url == "off" {
		return
	}
	httpsPort, _ := strconv.Atoi(env("KINARA_HTTPS_PORT", "32443"))
	manual, _ := strconv.Atoi(env("KINARA_PUBLIC_PORT", "0"))
	lanPort, _ := strconv.Atoi(strings.TrimPrefix(portOf(c.listen), ":"))
	m := cloud.New(cloud.Settings{
		CloudURL:   url,
		DataDir:    c.dataDir,
		LANPort:    lanPort,
		HTTPSPort:  httpsPort,
		ManualPort: manual,
		NoPortMap:  env("KINARA_PORT_MAPPING", "on") == "off",
		Version:    version,
		Name: func() string {
			h, _ := os.Hostname()
			return h
		},
	}, handler, d.log)
	if srv.Remote != nil {
		sc := srv.Remote
		m.Iroh = func() (string, string) {
			in := sc.Info()
			return in.Ticket, in.NodeID
		}
		// The sidecar uses our own iroh relay once the server is linked.
		sc.SetRelays(func() []string {
			if st := m.State(); st != nil {
				return st.IrohRelays
			}
			return nil
		})
		// Also called once the saved link is read at startup: the sidecar
		// may have started before then, on the public relays.
		m.RelaysChanged = sc.Reload
	}
	srv.Cloud = m
	go m.Run(ctx)
}

// startRemote serves the tunnel listener and supervises kinara-iroh, which
// carries apps' connections from anywhere to that listener. It runs only
// while remote access is turned on, and never before an admin account
// exists.
func startRemote(ctx context.Context, d *deps, srv *api.Server, handler http.Handler, dataDir string) {
	secretBytes := make([]byte, 32)
	if _, err := rand.Read(secretBytes); err != nil {
		d.log.Error("remote access off: no random secret", "err", err)
		return
	}
	secret := hex.EncodeToString(secretBytes)
	target := env("KINARA_TUNNEL_LISTEN", "127.0.0.1:8421")
	sc := &remote.Sidecar{
		DataDir: dataDir,
		Target:  target,
		Secret:  secret,
		Log:     d.log,
		Blocked: func(ctx context.Context) ([]string, error) {
			bs, err := d.store.BlockedNodes(ctx)
			ids := make([]string, 0, len(bs))
			for _, b := range bs {
				ids = append(ids, b.NodeID)
			}
			return ids, err
		},
		Allowed: func(ctx context.Context) bool {
			n, err := d.store.CountUsers(ctx)
			return err == nil && n > 0
		},
	}
	srv.Remote = sc
	go func() {
		if err := remote.Serve(ctx, target, secret, handler, d.log); err != nil {
			d.log.Error("remote access: tunnel listener failed", "listen", target, "err", err)
		}
	}()
	if rs, err := d.store.GetRemoteSettings(ctx); err == nil {
		sc.Configure(rs.Enabled, rs.UDPPort)
	}
	go sc.Run(ctx)
}

// lanPortOf is the port in a listen address (":8420" -> 8420).
func lanPortOf(listen string) int {
	n, _ := strconv.Atoi(strings.TrimPrefix(portOf(listen), ":"))
	return n
}
