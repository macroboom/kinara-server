// Command kinora-server runs the Kinora media server.
//
//	kinora-server [serve]           run the server (default)
//	kinora-server user add          create a user
//	kinora-server user passwd       change a password
//	kinora-server library add       add a library folder
//	kinora-server library list      list libraries
//	kinora-server scan              scan libraries now and wait
//	kinora-server service           run under the Windows service manager (the installer sets this up)
//
// Settings come from flags or environment variables:
//
//	-data     KINORA_DATA      data folder (database, settings)      ./kinora-data
//	-cache    KINORA_CACHE     cache folder (images, conversions,    DATA/cache
//	                           preview thumbnails); can grow large
//	-listen   KINORA_LISTEN    address to listen on                  :8420
//	-tmdb-key KINORA_TMDB_KEY  your own TMDB API key or read access   the key built into release builds
//	                           token, used instead of the built-in one
//	-ffprobe  KINORA_FFPROBE   path to ffprobe                       ffmpeg\ffprobe next to the server, else on PATH
//	-ffmpeg   KINORA_FFMPEG    path to ffmpeg                        next to ffprobe, or on PATH
//
// Background features (on unless set to 0 or off):
//
//	KINORA_TRANSCODE   convert files the TV can't play
//	KINORA_TRICKPLAY   preview thumbnails for skipping (about 5 MB per movie)
//	KINORA_INTROS      find TV intros for "Skip intro"
//	KINORA_WATCH       seconds between checks of library folders for new files (default 60)
//
// Limits:
//
//	KINORA_MAX_TRANSCODES  conversions at once (default 6 with a graphics card encoder, else 3)
//	KINORA_HWACCEL         graphics card for converting: auto (default), nvenc, qsv,
//	                       vaapi, amf, videotoolbox, or none for the CPU only
//	KINORA_HW_DEVICE       Linux render node to use (default: each in turn)
//
// Remote access (turned on in the web manager, under Remote access):
//
//	KINORA_IROH            path to kinora-iroh (default: next to the server, or on PATH)
//	KINORA_TUNNEL_LISTEN   loopback address kinora-iroh delivers connections to (default 127.0.0.1:8421)
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

	"kinora/internal/api"
	"kinora/internal/auth"
	"kinora/internal/cloud"
	"kinora/internal/ffmpeg"
	"kinora/internal/images"
	"kinora/internal/jobs"
	"kinora/internal/match"
	"kinora/internal/metrics"
	"kinora/internal/pgs"
	"kinora/internal/probe"
	"kinora/internal/remote"
	"kinora/internal/scanner"
	"kinora/internal/store"
	"kinora/internal/tmdb"
	"kinora/internal/transcode"
	"kinora/internal/trickplay"
	"kinora/internal/watch"
	"kinora/internal/watcher"
)

const version = "0.1.0-dev"

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

// cache is a folder inside the cache folder (KINORA_CACHE, else DATA/cache).
func (c config) cache(name string) string {
	root := c.cacheDir
	if root == "" {
		root = filepath.Join(c.dataDir, "cache")
	}
	return filepath.Join(root, name)
}

// bundledTool finds a tool shipped with the server: ffmpeg\ffprobe.exe (or
// ffmpeg/ffprobe) in the server's own folder, as the Windows installer lays
// it out, else beside the server. "" means look on PATH.
func bundledTool(name string) string {
	exe, err := os.Executable()
	if err != nil {
		return ""
	}
	if runtime.GOOS == "windows" {
		name += ".exe"
	}
	dir := filepath.Dir(exe)
	for _, p := range []string{filepath.Join(dir, "ffmpeg", name), filepath.Join(dir, name)} {
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
//	go build -ldflags "-X main.builtinTMDBKey=<key>" ./cmd/kinora-server
//
// and empty in the source, so the key never lands in the repository.
// KINORA_TMDB_KEY (or -tmdb-key) replaces it with your own.
var builtinTMDBKey string

func commonFlags(fs *flag.FlagSet, c *config) {
	fs.StringVar(&c.dataDir, "data", env("KINORA_DATA", "./kinora-data"), "data folder")
	fs.StringVar(&c.cacheDir, "cache", env("KINORA_CACHE", ""), "cache folder (default: the data folder's cache)")
	fs.StringVar(&c.tmdbKey, "tmdb-key", env("KINORA_TMDB_KEY", builtinTMDBKey), "your own TMDB API key or read access token (default: the one built in)")
	fs.StringVar(&c.ffprobe, "ffprobe", env("KINORA_FFPROBE", bundledTool("ffprobe")), "path to ffprobe")
	fs.StringVar(&c.ffmpeg, "ffmpeg", env("KINORA_FFMPEG", ""), "path to ffmpeg (default: next to ffprobe, or on PATH)")
	fs.BoolVar(&c.debug, "debug", false, "verbose logging")
	workers, _ := strconv.Atoi(env("KINORA_SCAN_WORKERS", "0"))
	fs.IntVar(&c.workers, "scan-workers", workers, "files probed at once (0 = pick by drive type: SSD up to 8, hard drive 2)")
}

func main() {
	envFile := env("KINORA_ENV_FILE", ".env")
	if err := loadDotEnv(envFile); err != nil {
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
  kinora-server [serve] [-listen :8420] [-data DIR] [-tmdb-key KEY]
  kinora-server user add -username NAME [-password PASS] [-admin]
  kinora-server user passwd -username NAME [-password PASS]
  kinora-server library add -kind movies|tv [-name NAME] PATH
  kinora-server library list
  kinora-server scan [-library ID]
  kinora-server service [serve flags]   (Windows: started by the service manager)
  kinora-server version`

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
	st, err := store.Open(filepath.Join(c.dataDir, "kinora.db"))
	if err != nil {
		return nil, err
	}
	d := &deps{store: st, log: log, logs: logs}
	d.prober = &probe.Prober{Binary: c.ffprobe}
	// KINORA_HWACCEL picks the graphics card backend (auto, nvenc, qsv,
	// vaapi, amf, videotoolbox, none); KINORA_HW_DEVICE the Linux render
	// node (/dev/dri/renderD128).
	d.ffmpeg = &ffmpeg.Tool{Binary: ffmpegPath(c), HWAccel: env("KINORA_HWACCEL", "auto"), Device: env("KINORA_HW_DEVICE", ""), Log: log}
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
	// KINORA_STOP_ON_EOF=1: a parent (the Windows tray, in "at sign-in"
	// mode) holds our stdin open; when it closes it, or the parent dies, we
	// shut down cleanly. Windows has no SIGTERM to send a windowless child.
	if env("KINORA_STOP_ON_EOF", "") == "1" {
		var cancel context.CancelFunc
		ctx, cancel = context.WithCancel(ctx)
		defer cancel()
		go func() {
			_, _ = io.Copy(io.Discard, os.Stdin)
			cancel()
		}()
	}
	return serveUntil(ctx, args)
}

// serveUntil runs the server until parent ends (the Windows service manager
// stopping it) or the process is interrupted.
func serveUntil(parent context.Context, args []string) error {
	var c config
	fs := flag.NewFlagSet("serve", flag.ExitOnError)
	commonFlags(fs, &c)
	fs.StringVar(&c.listen, "listen", env("KINORA_LISTEN", ":8420"), "address to listen on")
	fs.DurationVar(&c.rescan, "rescan", 6*time.Hour, "rescan libraries this often (0 to turn off)")
	fs.Parse(args)

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
		warnings = append(warnings, "No TMDB key: this build has none built in and KINORA_TMDB_KEY isn't set, so titles come from file names and there's no artwork. Set KINORA_TMDB_KEY to your own key.")
	}
	ffmpegOK := d.ffmpeg.Available() == nil
	if !ffmpegOK {
		warnings = append(warnings, "ffmpeg was not found, so files the TV can't play won't be converted, and there are no preview thumbnails or Skip intro. Install ffmpeg or pass -ffmpeg /path/to/ffmpeg.")
	}
	for _, w := range warnings {
		d.log.Warn(w)
	}

	// The web app: KINORA_WEB, or a folder named web next to the server.
	webDir := env("KINORA_WEB", "")
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
	// Readings for the metrics page: every 5 s, the last day in memory.
	srv.Metrics = &metrics.Collector{Streams: srv.StreamCounts}
	go srv.Metrics.Run(ctx)
	if ffmpegOK {
		// Check hardware encoders now rather than on the first stream.
		go d.ffmpeg.Caps()
		if feature("KINORA_TRANSCODE") {
			srv.Transcoder = &transcode.Transcoder{Tool: d.ffmpeg, FFprobe: c.ffprobe, Log: d.log}
			// KINORA_MAX_TRANSCODES caps conversions at once (default 6 with
			// a graphics card encoder, else 3).
			if n, err := strconv.Atoi(env("KINORA_MAX_TRANSCODES", "0")); err == nil && n > 0 {
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
		worker := &jobs.Worker{Store: d.store, Tool: d.ffmpeg, Log: d.log, Intros: feature("KINORA_INTROS"),
			ThumbDir: c.cache("thumbs")}
		if feature("KINORA_TRICKPLAY") {
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
	if secs := env("KINORA_WATCH", "60"); secs != "0" && secs != "off" {
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
	d.log.Info("Kinora server running", "version", version, "listen", c.listen, "data", c.dataDir, "cache", c.cache(""))

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
	if v := os.Getenv("KINORA_PASSWORD"); v != "" {
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
	password := fs.String("password", "", "password (or KINORA_PASSWORD, or typed)")
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
		fmt.Printf("Added library %d: %s (%s) at %s\nRun \"kinora-server scan\" or start the server to scan it.\n", lib.ID, lib.Name, lib.Kind, lib.Path)
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

// startCloud links the server to a Kinara account (when KINORA_CLOUD_URL
// is set) and keeps it reachable from anywhere: direct HTTPS with a
// certificate for its own names, a router port opened by UPnP or NAT-PMP,
// and the cloud's relay for players that can't connect directly.
func startCloud(ctx context.Context, d *deps, srv *api.Server, handler http.Handler, c config) {
	url := env("KINORA_CLOUD_URL", "")
	if url == "" || url == "off" {
		return
	}
	httpsPort, _ := strconv.Atoi(env("KINORA_HTTPS_PORT", "32443"))
	manual, _ := strconv.Atoi(env("KINORA_PUBLIC_PORT", "0"))
	lanPort, _ := strconv.Atoi(strings.TrimPrefix(portOf(c.listen), ":"))
	m := cloud.New(cloud.Settings{
		CloudURL:   url,
		DataDir:    c.dataDir,
		LANPort:    lanPort,
		HTTPSPort:  httpsPort,
		ManualPort: manual,
		NoPortMap:  env("KINORA_PORT_MAPPING", "on") == "off",
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

// startRemote serves the tunnel listener and supervises kinora-iroh, which
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
	target := env("KINORA_TUNNEL_LISTEN", "127.0.0.1:8421")
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
