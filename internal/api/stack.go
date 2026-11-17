package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"time"

	"kinara/internal/arr"
	"kinara/internal/store"
)

// The download stack, made easy: Prowlarr (indexers), Radarr, Sonarr and
// qBittorrent. For someone starting from nothing, Kinara writes a setup
// script for the server's system that starts all four in Docker with
// secrets Kinara knows, then "Connect everything" wires them together and
// to Kinara. For someone who has them already, the Prowlarr and
// qBittorrent connections link them to Radarr and Sonarr.

func (s *Server) stackRoutes(mux *http.ServeMux) {
	mux.Handle("GET /api/v1/arr/stack", s.admin(s.getStack))
	mux.Handle("POST /api/v1/arr/stack", s.admin(s.makeStack))
	mux.Handle("POST /api/v1/arr/stack/connect", s.admin(s.connectStack))
	mux.Handle("POST /api/v1/arr/link", s.admin(s.linkArr))
}

// --- What this server is ----------------------------------------------------------

type stackInfo struct {
	// OS and Arch are the server's (linux, darwin, windows).
	OS   string `json:"os"`
	Arch string `json:"arch"`
	// InDocker: Kinara itself runs in a container, so the paths it sees
	// may not be the host's.
	InDocker bool `json:"inDocker"`
	// Docker and Compose: found on this computer (unknown inside Docker).
	Docker  bool `json:"docker"`
	Compose bool `json:"compose"`
	// Install is how to get Docker on this system.
	Install []arr.InstallStep `json:"install"`
	// Suggested folders, from the libraries.
	Suggested stackFolders `json:"suggested"`
	// Warnings about the suggestion.
	Warnings []string `json:"warnings"`
	// Saved is the stack made before (its folders).
	Saved *stackFolders `json:"saved,omitempty"`
	// QbitLogin is how to sign in to the stack's qBittorrent in a browser
	// (owners only see this page).
	QbitLogin *qbitLogin `json:"qbitLogin,omitempty"`
	// Ports the stack uses on this computer.
	Ports map[string]int `json:"ports"`
}

type qbitLogin struct {
	URL      string `json:"url"`
	Username string `json:"username"`
	Password string `json:"password"`
}

type stackFolders struct {
	MediaDir     string `json:"mediaDir"`
	MoviesDir    string `json:"moviesDir"`
	TVDir        string `json:"tvDir"`
	DownloadsDir string `json:"downloadsDir"`
	// OwnQbit: use the owner's own qBittorrent, not one in the stack.
	OwnQbit   bool  `json:"ownQbit"`
	CreatedAt int64 `json:"createdAt,omitempty"`
}

func inDocker() bool {
	if _, err := os.Stat("/.dockerenv"); err == nil {
		return true
	}
	b, _ := os.ReadFile("/proc/1/cgroup")
	return strings.Contains(string(b), "docker") || strings.Contains(string(b), "containerd")
}

func (s *Server) getStack(w http.ResponseWriter, r *http.Request, u store.User) {
	writeJSON(w, http.StatusOK, s.stackInfo(r.Context()))
}

func (s *Server) stackInfo(ctx context.Context) stackInfo {
	out := stackInfo{OS: runtime.GOOS, Arch: runtime.GOARCH, InDocker: inDocker(), Install: arr.DockerInstall(runtime.GOOS),
		Warnings: []string{}, Ports: map[string]int{"prowlarr": arr.PortProwlarr, "radarr": arr.PortRadarr, "sonarr": arr.PortSonarr, "qbittorrent": arr.PortQbit, "flaresolverr": arr.PortFlareSolverr}}
	if _, err := exec.LookPath("docker"); err == nil {
		out.Docker = true
		cctx, cancel := context.WithTimeout(ctx, 5*time.Second)
		out.Compose = exec.CommandContext(cctx, "docker", "compose", "version").Run() == nil
		cancel()
	}
	out.Suggested, out.Warnings = s.suggestFolders(ctx)
	// Before the first script: the apps' own ports must be free, or Docker
	// can't start them (a qBittorrent already installed may use 8090 for its
	// Web UI, for example). The stack's own ports are busy once it runs.
	if s.arrSettings(ctx).Stack == nil && !out.InDocker {
		for _, svc := range []struct {
			name string
			port int
		}{{"Prowlarr", arr.PortProwlarr}, {"Radarr", arr.PortRadarr}, {"Sonarr", arr.PortSonarr}, {"qBittorrent's Web UI", arr.PortQbit}, {"FlareSolverr", arr.PortFlareSolverr}} {
			if !portFree(svc.port) {
				out.Warnings = append(out.Warnings, fmt.Sprintf("Something on this computer already uses port %d, which %s needs. If it's that app installed outside Docker, stop it first (or connect it by hand below instead).", svc.port, svc.name))
			}
		}
	}
	if out.InDocker {
		out.Warnings = append(out.Warnings, "Kinara runs in Docker here, so the folders above are as its container sees them. Type the media folder as the computer itself has it, and make sure Kinara's container mounts that same folder.")
	}
	if st := s.arrSettings(ctx).Stack; st != nil {
		out.Saved = &stackFolders{MediaDir: st.MediaDir, MoviesDir: st.MoviesDir, TVDir: st.TVDir, DownloadsDir: st.DownloadsDir, OwnQbit: st.OwnQbit, CreatedAt: st.CreatedAt}
		if !st.OwnQbit {
			out.QbitLogin = &qbitLogin{URL: localhostFor(arr.PortQbit), Username: st.QbitUser, Password: st.QbitPass}
		}
	}
	return out
}

// suggestFolders picks one media folder holding the movie and TV libraries
// (so downloads and libraries share a drive), from the libraries there are.
func (s *Server) suggestFolders(ctx context.Context) (stackFolders, []string) {
	warn := []string{}
	var movies, tv string
	if libs, err := s.Store.ListLibraries(ctx); err == nil {
		for _, l := range libs {
			if l.Kind == store.KindMovies && movies == "" {
				movies = l.Path
			}
			if l.Kind == store.KindTV && tv == "" {
				tv = l.Path
			}
		}
	}
	sep := string(filepath.Separator)
	rel := func(base, p string) string {
		r, err := filepath.Rel(base, p)
		if err != nil || strings.HasPrefix(r, "..") {
			return ""
		}
		return filepath.ToSlash(r)
	}
	out := stackFolders{MoviesDir: "movies", TVDir: "tv", DownloadsDir: "downloads"}
	switch {
	case movies != "" && tv != "":
		common := commonDir(movies, tv)
		if common == "" || common == sep || isDriveRoot(common) {
			out.MediaDir = filepath.Dir(movies)
			out.MoviesDir = rel(out.MediaDir, movies)
			warn = append(warn, fmt.Sprintf("Your movie and TV libraries aren't in one folder, so shows will go to %s and Kinara adds that to your TV library.",
				filepath.Join(out.MediaDir, "tv")))
		} else {
			out.MediaDir = common
			out.MoviesDir, out.TVDir = rel(common, movies), rel(common, tv)
		}
	case movies != "":
		out.MediaDir = filepath.Dir(movies)
		out.MoviesDir = rel(out.MediaDir, movies)
	case tv != "":
		out.MediaDir = filepath.Dir(tv)
		out.TVDir = rel(out.MediaDir, tv)
	default:
		home, _ := os.UserHomeDir()
		out.MediaDir = filepath.Join(home, "Media")
		warn = append(warn, "There are no movie or TV libraries yet: Kinara makes them in this folder when it connects everything.")
	}
	if out.MoviesDir == "" || out.MoviesDir == "." {
		out.MoviesDir = "movies"
	}
	if out.TVDir == "" || out.TVDir == "." {
		out.TVDir = "tv"
	}
	return out, warn
}

// commonDir is the deepest folder holding both paths ("" when none).
func commonDir(a, b string) string {
	a, b = filepath.Clean(a), filepath.Clean(b)
	for d := a; ; d = filepath.Dir(d) {
		if r, err := filepath.Rel(d, b); err == nil && !strings.HasPrefix(r, "..") {
			return d
		}
		if filepath.Dir(d) == d {
			return ""
		}
	}
}

func isDriveRoot(p string) bool {
	return len(p) <= 3 && strings.Contains(p, ":")
}

// --- The setup script -------------------------------------------------------------

// timezone is the server's time zone name for the containers.
func timezone() string {
	if tz := os.Getenv("TZ"); tz != "" {
		return tz
	}
	if name := time.Local.String(); name != "" && name != "Local" {
		return name
	}
	if b, err := os.ReadFile("/etc/timezone"); err == nil && strings.TrimSpace(string(b)) != "" {
		return strings.TrimSpace(string(b))
	}
	if link, err := os.Readlink("/etc/localtime"); err == nil {
		if _, zone, ok := strings.Cut(link, "zoneinfo/"); ok {
			return zone
		}
	}
	return "Etc/UTC"
}

// cleanSub checks a folder inside the media folder ("movies", "Media/TV").
func cleanSub(name, v, def string) (string, error) {
	v = strings.Trim(strings.ReplaceAll(strings.TrimSpace(v), `\`, "/"), "/")
	if v == "" {
		v = def
	}
	if strings.Contains(v, "..") || strings.ContainsAny(v, `"'$`+"`") {
		return "", fmt.Errorf("the %s folder name can't contain quotes, $ or ..", name)
	}
	return v, nil
}

func (s *Server) makeStack(w http.ResponseWriter, r *http.Request, u store.User) {
	var in struct {
		stackFolders
		// NewSecrets makes new keys even if a stack was made before (it
		// would no longer match containers already running).
		NewSecrets bool `json:"newSecrets"`
	}
	if err := readJSON(r, &in); err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	media := strings.TrimSpace(in.MediaDir)
	if media == "" || strings.ContainsAny(media, `"'$`+"`") {
		writeErr(w, http.StatusBadRequest, "give the media folder as a full path, without quotes or $")
		return
	}
	if !filepath.IsAbs(media) && !(len(media) > 2 && media[1] == ':') {
		writeErr(w, http.StatusBadRequest, "the media folder must be a full path, like /srv/media or D:\\Media")
		return
	}
	// A drive on its own ("E:") means the current folder on that drive.
	if len(media) == 2 && media[1] == ':' {
		media += `\`
	}
	var err error
	st := arr.Stack{OS: runtime.GOOS, MediaDir: media, PUID: 1000, PGID: 1000, TZ: timezone()}
	if st.MoviesDir, err = cleanSub("movies", in.MoviesDir, "movies"); err == nil {
		if st.TVDir, err = cleanSub("TV", in.TVDir, "tv"); err == nil {
			st.DownloadsDir, err = cleanSub("downloads", in.DownloadsDir, "downloads")
		}
	}
	if err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	if runtime.GOOS != "windows" {
		// The containers write as Kinara's own user, so it can read what they make.
		if uid, gid := os.Getuid(), os.Getgid(); uid > 0 {
			st.PUID, st.PGID = uid, gid
		}
	}
	a := s.arrSettings(r.Context())
	st.OwnQbit = in.OwnQbit
	if st.OwnQbit && a.Qbit.URL == "" {
		writeErr(w, http.StatusBadRequest, "connect your qBittorrent first (its Web UI address, user name and password)")
		return
	}
	if old := a.Stack; old != nil && !in.NewSecrets && old.RadarrKey != "" {
		st.RadarrKey, st.SonarrKey, st.ProwlarrKey, st.QbitUser, st.QbitPass = old.RadarrKey, old.SonarrKey, old.ProwlarrKey, old.QbitUser, old.QbitPass
		// The port the running stack holds: it isn't free now, but it's ours.
		st.TorrentPort = old.TorrentPort
		if st.TorrentPort == 0 {
			st.TorrentPort = arr.DefaultTorrentPort
		}
	} else {
		st.NewSecrets()
		st.TorrentPort = freeTorrentPort()
	}
	a.Stack = &store.ArrStack{OS: st.OS, MediaDir: st.MediaDir, MoviesDir: st.MoviesDir, TVDir: st.TVDir, DownloadsDir: st.DownloadsDir,
		PUID: st.PUID, PGID: st.PGID, TZ: st.TZ, RadarrKey: st.RadarrKey, SonarrKey: st.SonarrKey, ProwlarrKey: st.ProwlarrKey,
		QbitUser: st.QbitUser, QbitPass: st.QbitPass, TorrentPort: st.TorrentPort, OwnQbit: st.OwnQbit, CreatedAt: time.Now().Unix()}
	if err := s.Store.PutArrSettings(r.Context(), a); err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	name, script := st.Script()
	run := "sh " + name
	if st.OS == "windows" {
		run = "powershell -ExecutionPolicy Bypass -File " + name
	}
	s.Log.Info("download stack script made", "by", u.Username, "media", st.MediaDir)
	writeJSON(w, http.StatusOK, map[string]any{"name": name, "script": script, "compose": st.Compose(), "run": run})
}

func stackOf(a *store.ArrStack) arr.Stack {
	return arr.Stack{OS: a.OS, MediaDir: a.MediaDir, MoviesDir: a.MoviesDir, TVDir: a.TVDir, DownloadsDir: a.DownloadsDir,
		PUID: a.PUID, PGID: a.PGID, TZ: a.TZ, RadarrKey: a.RadarrKey, SonarrKey: a.SonarrKey, ProwlarrKey: a.ProwlarrKey,
		QbitUser: a.QbitUser, QbitPass: a.QbitPass, TorrentPort: a.TorrentPort, OwnQbit: a.OwnQbit}
}

// portFree: nothing on this computer listens on the port (TCP and UDP).
// Kinara runs on the same computer as the stack, so it can just try.
var portFree = func(port int) bool {
	l, err := net.Listen("tcp", ":"+strconv.Itoa(port))
	if err != nil {
		return false
	}
	l.Close()
	u, err := net.ListenPacket("udp", ":"+strconv.Itoa(port))
	if err != nil {
		return false
	}
	u.Close()
	return true
}

// freeTorrentPort finds a peer port no other torrent client (a qBittorrent
// already installed, say) is using.
func freeTorrentPort() int {
	for p := arr.DefaultTorrentPort; p < arr.DefaultTorrentPort+100; p++ {
		if portFree(p) {
			return p
		}
	}
	return arr.DefaultTorrentPort
}

// --- Wiring -----------------------------------------------------------------------

// stepView is one thing "Connect everything" or "Link" did.
type stepView struct {
	Title string `json:"title"`
	// State is done, skipped (there already) or failed.
	State string `json:"state"`
	Note  string `json:"note,omitempty"`
}

type steps []stepView

func (l *steps) add(title string, added bool, err error) bool {
	switch {
	case err != nil:
		*l = append(*l, stepView{Title: title, State: "failed", Note: err.Error()})
		return false
	case added:
		*l = append(*l, stepView{Title: title, State: "done"})
	default:
		*l = append(*l, stepView{Title: title, State: "skipped", Note: "there already"})
	}
	return true
}

// localhostFor is the stack's address for a service from Kinara: the
// host's own ports (Kinara in Docker uses host networking).
func localhostFor(port int) string { return "http://localhost:" + strconv.Itoa(port) }

// stackURL is localhostFor; tests point it at fakes.
var stackURL = localhostFor

// waitFor waits until a service answers (it may still be starting). tick,
// if set, hears how long it has waited and the last answer after each try
// that failed.
func waitFor(ctx context.Context, check func(context.Context) error, tick func(time.Duration, error)) error {
	began := time.Now()
	deadline := began.Add(90 * time.Second)
	for {
		cctx, cancel := context.WithTimeout(ctx, 5*time.Second)
		err := check(cctx)
		cancel()
		if err == nil || time.Now().After(deadline) || ctx.Err() != nil {
			return err
		}
		var ae *arr.Error
		if errors.As(err, &ae) && ae.Status == http.StatusUnauthorized {
			return errors.New("it turned down Kinara's API key: was it started by Kinara's setup script? If it ran before, make a new script with new keys, or connect it by hand below")
		}
		// qBittorrent counts each failed sign-in and blocks the address
		// after a few: no point asking again.
		if errors.Is(err, arr.ErrQbitRefused) {
			return errors.New("qBittorrent turned down the password Kinara set. Make the script again and run it: it resets qBittorrent's password to Kinara's")
		}
		if errors.Is(err, arr.ErrQbitBanned) {
			return err
		}
		if tick != nil {
			tick(time.Since(began), err)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(3 * time.Second):
		}
	}
}

// shortErr is an error on one line, short enough for the log.
func shortErr(err error) string {
	m := strings.Join(strings.Fields(err.Error()), " ")
	if len(m) > 160 {
		m = m[:157] + "..."
	}
	return m
}

// connectProgress tells the page what "Connect everything" is doing as it
// goes, as one JSON object per line, when the page asks for that (Accept:
// application/x-ndjson). Otherwise the answer is the result alone, at the
// end.
type connectProgress struct {
	w      http.ResponseWriter
	rc     *http.ResponseController
	stream bool
	total  float64
	done   float64
	// partial is progress within the current step (waiting for an app).
	partial float64
	// weight is the current step's share of the bar, in steps.
	weight float64
}

// connectSteps is how many steps a full run has. The four waits for the
// apps to start take most of the time, so each counts as waitWeight steps
// on the bar.
const (
	connectSteps = 15
	waitWeight   = 2.5
)

func newConnectProgress(w http.ResponseWriter, r *http.Request) *connectProgress {
	p := &connectProgress{w: w, rc: http.NewResponseController(w), total: connectSteps + 4*(waitWeight-1), weight: 1,
		stream: strings.Contains(r.Header.Get("Accept"), "application/x-ndjson")}
	if p.stream {
		w.Header().Set("Content-Type", "application/x-ndjson")
		w.Header().Set("Cache-Control", "no-store")
		// Proxies (nginx) would hold the lines back otherwise.
		w.Header().Set("X-Accel-Buffering", "no")
		w.WriteHeader(http.StatusOK)
	}
	return p
}

func (p *connectProgress) send(v map[string]any) {
	if !p.stream {
		return
	}
	v["progress"] = p.fraction()
	b, _ := json.Marshal(v)
	_, _ = p.w.Write(append(b, '\n'))
	_ = p.rc.Flush()
}

func (p *connectProgress) fraction() float64 {
	f := (p.done + p.partial*p.weight) / p.total
	if f > 1 {
		f = 1
	}
	return math.Round(f*1000) / 1000
}

// status says what's happening now.
func (p *connectProgress) status(text string) {
	p.send(map[string]any{"event": "status", "text": text})
}

// step reports a finished step.
func (p *connectProgress) step(st stepView) {
	p.done += p.weight
	p.partial, p.weight = 0, 1
	p.send(map[string]any{"event": "step", "step": st})
}

// finish sends the result: the last line when streaming, else the answer.
func (p *connectProgress) finish(result map[string]any) {
	if !p.stream {
		writeJSON(p.w, http.StatusOK, result)
		return
	}
	p.done, p.partial = p.total, 0
	p.send(map[string]any{"event": "done", "result": result})
}

// connectStack wires the stack the setup script started: root folders,
// qBittorrent in Radarr and Sonarr, Radarr and Sonarr in Prowlarr, Kinara's
// own connections, and Kinara's libraries over the same folders.
func (s *Server) connectStack(w http.ResponseWriter, r *http.Request, u store.User) {
	a := s.arrSettings(r.Context())
	if a.Stack == nil {
		writeErr(w, http.StatusConflict, "make and run the setup script first")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 4*time.Minute)
	defer cancel()
	prog := newConnectProgress(w, r)
	st := stackOf(a.Stack)
	radarr := &arr.Client{Kind: arr.Radarr, BaseURL: stackURL(arr.PortRadarr), APIKey: st.RadarrKey}
	sonarr := &arr.Client{Kind: arr.Sonarr, BaseURL: stackURL(arr.PortSonarr), APIKey: st.SonarrKey}
	prowlarr := &arr.Client{Kind: arr.Prowlarr, BaseURL: stackURL(arr.PortProwlarr), APIKey: st.ProwlarrKey}
	qbit := &arr.Qbit{BaseURL: stackURL(arr.PortQbit), Username: st.QbitUser, Password: st.QbitPass}
	// Inside the stack the apps reach each other by name.
	qc := arr.QbitClient{Host: "qbittorrent", Port: arr.PortQbit, Username: st.QbitUser, Password: st.QbitPass}
	if st.OwnQbit {
		// The owner's own qBittorrent, on this computer: Kinara reaches it
		// at its saved address, Radarr and Sonarr through the host's name.
		qbit = &arr.Qbit{BaseURL: a.Qbit.URL, Username: a.Qbit.Username, Password: a.Qbit.Password}
		port := 8080
		if u, err := url.Parse(withScheme(a.Qbit.URL)); err == nil && u.Port() != "" {
			port, _ = strconv.Atoi(u.Port())
		}
		qc = arr.QbitClient{Host: "host.docker.internal", Port: port, Username: a.Qbit.Username, Password: a.Qbit.Password}

		prog.total += 2 // the two path mappings
	}

	var out steps
	// do runs one step: says what it's doing, then reports how it went.
	do := func(doing, title string, run func() (bool, error)) {
		prog.status(doing)
		added, err := run()
		out.add(title, added, err)
		prog.step(out[len(out)-1])
	}
	for _, svc := range []struct {
		name, addr string
		check      func(context.Context) error
	}{
		{"Radarr", radarr.BaseURL, func(c context.Context) error { _, err := radarr.Profiles(c); return err }},
		{"Sonarr", sonarr.BaseURL, func(c context.Context) error { _, err := sonarr.Profiles(c); return err }},
		{"Prowlarr", prowlarr.BaseURL, func(c context.Context) error { _, _, err := prowlarr.ProwlarrCheck(c); return err }},
		{"qBittorrent", qbit.BaseURL, func(c context.Context) error { _, err := qbit.Version(c); return err }},
	} {
		prog.weight = waitWeight
		prog.status("Checking " + svc.name + " at " + svc.addr)
		err := waitFor(ctx, svc.check, func(waited time.Duration, last error) {
			// Quick at first, slowing down: most apps are up within a minute.
			prog.partial = math.Min(1-math.Exp(-waited.Seconds()/30), 0.95)
			prog.status(fmt.Sprintf("Waiting for %s (%d s). Last answer: %s", svc.name, int(waited.Seconds()), shortErr(last)))
		})
		out.add(svc.name+" is running", true, err)
		prog.step(out[len(out)-1])
		if err != nil {
			prog.finish(map[string]any{"steps": out, "ok": false})
			return
		}
	}

	do("Adding Radarr's movie folder", "Radarr's movie folder "+st.MoviesPath(), func() (bool, error) { return radarr.EnsureRootFolder(ctx, st.MoviesPath()) })
	do("Adding Sonarr's TV folder", "Sonarr's TV folder "+st.TVPath(), func() (bool, error) { return sonarr.EnsureRootFolder(ctx, st.TVPath()) })
	if st.OwnQbit {
		// Its own default folder is left alone (it has the owner's other
		// downloads); Kinara's go in their categories' folders.
		do("Adding the radarr and sonarr categories to your qBittorrent", "radarr and sonarr categories in your qBittorrent (saving to "+st.HostDownloads()+")", func() (bool, error) {
			// The categories' folders, made now: until the first download
			// creates them, Radarr and Sonarr warn that they're missing.
			// (Kinara runs on the same computer; in Docker it can't, and
			// that's only a warning.)
			for _, cat := range []string{arr.Radarr, arr.Sonarr} {
				_ = os.MkdirAll(st.HostDownloads()+cat, 0o755)
			}
			return true, qbit.Categories(ctx, st.HostDownloads())
		})
	} else {
		do("Setting qBittorrent's download folder and categories", "qBittorrent's download folder and categories", func() (bool, error) {
			return true, qbit.Prepare(ctx, st.DownloadsPath())
		})
	}
	do("Adding qBittorrent to Radarr", "qBittorrent in Radarr", func() (bool, error) {
		c := qc
		c.Category = arr.Radarr
		return radarr.EnsureQbittorrent(ctx, c)
	})
	do("Adding qBittorrent to Sonarr", "qBittorrent in Sonarr", func() (bool, error) {
		c := qc
		c.Category = arr.Sonarr
		return sonarr.EnsureQbittorrent(ctx, c)
	})
	if st.OwnQbit {
		// Your qBittorrent says where it saved as this computer names the
		// folder (E:\downloads\...); in Docker, Radarr and Sonarr see it as
		// /data/downloads.
		do("Telling Radarr where your qBittorrent's downloads are", "Radarr finds "+st.HostDownloads()+" at /data/downloads", func() (bool, error) {
			return radarr.EnsureRemotePathMapping(ctx, qc.Host, st.HostDownloads(), st.DownloadsPath()+"/")
		})
		do("Telling Sonarr where your qBittorrent's downloads are", "Sonarr finds "+st.HostDownloads()+" at /data/downloads", func() (bool, error) {
			return sonarr.EnsureRemotePathMapping(ctx, qc.Host, st.HostDownloads(), st.DownloadsPath()+"/")
		})
	}
	do("Adding Radarr to Prowlarr", "Radarr in Prowlarr (it gets Prowlarr's indexers)", func() (bool, error) {
		return prowlarr.EnsureApp(ctx, arr.ProwlarrApp{Kind: arr.Radarr, BaseURL: "http://radarr:7878", ProwlarrURL: "http://prowlarr:9696", APIKey: st.RadarrKey})
	})
	do("Adding Sonarr to Prowlarr", "Sonarr in Prowlarr (it gets Prowlarr's indexers)", func() (bool, error) {
		return prowlarr.EnsureApp(ctx, arr.ProwlarrApp{Kind: arr.Sonarr, BaseURL: "http://sonarr:8989", ProwlarrURL: "http://prowlarr:9696", APIKey: st.SonarrKey})
	})
	do("Adding FlareSolverr to Prowlarr", "FlareSolverr in Prowlarr (for indexers tagged flaresolverr)", func() (bool, error) {
		return prowlarr.EnsureFlareSolverr(ctx, "http://flaresolverr:8191")
	})

	// Kinara's own connections.
	pick := func(c *arr.Client) int {
		ps, err := c.Profiles(ctx)
		if err != nil || len(ps) == 0 {
			return 0
		}
		for _, p := range ps {
			if strings.EqualFold(p.Name, "HD-1080p") || strings.EqualFold(p.Name, "HD - 720p/1080p") {
				return p.ID
			}
		}
		return ps[0].ID
	}
	do("Pointing Kinara's requests at Radarr and Sonarr", "Requests go to Radarr and Sonarr", func() (bool, error) {
		a := s.arrSettings(ctx)
		a.Radarr.Enabled, a.Radarr.URL, a.Radarr.APIKey, a.Radarr.RootFolder = true, radarr.BaseURL, st.RadarrKey, st.MoviesPath()
		if a.Radarr.ProfileID == 0 {
			a.Radarr.ProfileID = pick(radarr)
		}
		a.Sonarr.Enabled, a.Sonarr.URL, a.Sonarr.APIKey, a.Sonarr.RootFolder = true, sonarr.BaseURL, st.SonarrKey, st.TVPath()
		if a.Sonarr.ProfileID == 0 {
			a.Sonarr.ProfileID = pick(sonarr)
		}
		a.Prowlarr = store.ArrLink{URL: prowlarr.BaseURL, APIKey: st.ProwlarrKey}
		if !st.OwnQbit {
			a.Qbit = store.QbitLink{URL: qbit.BaseURL, Username: st.QbitUser, Password: st.QbitPass}
		}
		return true, s.Store.PutArrSettings(ctx, a)
	})

	// Kinara's libraries over the same folders, so imports show up.
	if !inDocker() {
		prog.status("Adding the movies folder to Kinara's libraries")
		out = append(out, s.libraryFor(ctx, store.KindMovies, "Movies", filepath.Join(st.MediaDir, filepath.FromSlash(st.MoviesDir))))
		prog.step(out[len(out)-1])
		prog.status("Adding the TV folder to Kinara's libraries")
		out = append(out, s.libraryFor(ctx, store.KindTV, "TV Shows", filepath.Join(st.MediaDir, filepath.FromSlash(st.TVDir))))
		prog.step(out[len(out)-1])
	} else {
		out = append(out, stepView{Title: "Kinara's libraries", State: "skipped",
			Note: "Kinara runs in Docker: add the movies and TV folders to its libraries as its container sees them"})
		prog.step(out[len(out)-1])
	}
	prog.status("Counting Prowlarr's indexers")
	_, enabled, _ := prowlarr.Indexers(ctx)
	ok := true
	for _, st := range out {
		if st.State == "failed" {
			ok = false
		}
	}
	s.Log.Info("download stack connected", "by", u.Username, "ok", ok)
	s.arrKick()
	prog.finish(map[string]any{"steps": out, "ok": ok, "indexers": enabled, "prowlarrUrl": stackURL(arr.PortProwlarr)})
}

// libraryFor makes sure a library of a kind covers a folder: added to the
// first library of that kind, or a new library.
func (s *Server) libraryFor(ctx context.Context, kind, name, folder string) stepView {
	title := "Kinara library for " + folder
	if fi, err := os.Stat(folder); err != nil || !fi.IsDir() {
		return stepView{Title: title, State: "failed", Note: "Kinara can't see that folder: was the setup script run on this computer?"}
	}
	libs, err := s.Store.ListLibraries(ctx)
	if err != nil {
		return stepView{Title: title, State: "failed", Note: err.Error()}
	}
	for _, l := range libs {
		for _, f := range append([]string{l.Path}, l.Folders...) {
			if filepath.Clean(f) == filepath.Clean(folder) {
				return stepView{Title: title, State: "skipped", Note: "in " + l.Name + " already"}
			}
		}
	}
	for _, l := range libs {
		if l.Kind == kind {
			if err := s.Store.AddLibraryFolder(ctx, l.ID, folder); err != nil {
				return stepView{Title: title, State: "failed", Note: err.Error()}
			}
			if s.Scanner != nil {
				_ = s.Scanner.Queue(ctx, l.ID)
			}
			return stepView{Title: title, State: "done", Note: "added to " + l.Name}
		}
	}
	l, err := s.Store.CreateLibrary(ctx, name, folder, kind)
	if err != nil {
		return stepView{Title: title, State: "failed", Note: err.Error()}
	}
	if s.Scanner != nil {
		_ = s.Scanner.Queue(ctx, l.ID)
	}
	return stepView{Title: title, State: "done", Note: "made " + name}
}

// linkInput is how the apps reach each other, when that isn't the address
// Kinara uses (in Docker, for example). Empty uses Kinara's.
type linkInput struct {
	RadarrFromProwlarr string `json:"radarrFromProwlarr"`
	SonarrFromProwlarr string `json:"sonarrFromProwlarr"`
	ProwlarrFromApps   string `json:"prowlarrFromApps"`
	QbitFromApps       string `json:"qbitFromApps"`
	// FlareSolverr, as Prowlarr reaches it: added as its proxy when given.
	FlareSolverr string `json:"flareSolverr"`
}

// linkArr wires a setup the owner had already: qBittorrent into Radarr and
// Sonarr, and Radarr and Sonarr into Prowlarr, using the addresses saved in
// Kinara (or the ones given, as the services see each other).
func (s *Server) linkArr(w http.ResponseWriter, r *http.Request, u store.User) {
	var in linkInput
	if r.ContentLength != 0 {
		if err := readJSON(r, &in); err != nil {
			writeErr(w, http.StatusBadRequest, err.Error())
			return
		}
	}
	ctx, cancel := context.WithTimeout(r.Context(), time.Minute)
	defer cancel()
	out := s.linkApps(ctx, in)
	if len(out) == 0 {
		writeErr(w, http.StatusConflict, "connect Radarr or Sonarr, and Prowlarr or qBittorrent, first")
		return
	}
	s.Log.Info("linked the download apps", "by", u.Username)
	writeJSON(w, http.StatusOK, map[string]any{"steps": out})
}

// linkApps adds qBittorrent to Radarr and Sonarr and both to Prowlarr, from
// Kinara's saved connections. Steps already done are skipped.
func (s *Server) linkApps(ctx context.Context, in linkInput) steps {
	a := s.arrSettings(ctx)
	or := func(v, def string) string {
		if v = strings.TrimSpace(v); v != "" {
			return v
		}
		return def
	}
	var out steps
	apps := []struct {
		kind, name string
		srv        store.ArrServer
		from       string
	}{{arr.Radarr, "Radarr", a.Radarr, in.RadarrFromProwlarr}, {arr.Sonarr, "Sonarr", a.Sonarr, in.SonarrFromProwlarr}}
	if a.Qbit.URL != "" {
		qurl, err := url.Parse(or(in.QbitFromApps, withScheme(a.Qbit.URL)))
		if err != nil || qurl.Hostname() == "" {
			out.add("qBittorrent's address", false, errors.New("not an address"))
		} else {
			port, _ := strconv.Atoi(qurl.Port())
			if port == 0 {
				port = 8080
			}
			host := qurl.Hostname()
			// Radarr and Sonarr from Kinara's Docker setup are in containers,
			// where "localhost" is the container itself: a qBittorrent on
			// this computer is host.docker.internal to them, and its
			// downloads folder needs mapping to theirs.
			viaHost := a.Stack != nil && in.QbitFromApps == "" && (host == "localhost" || host == "127.0.0.1" || host == "::1")
			if viaHost {
				host = "host.docker.internal"
			}
			for _, app := range apps {
				if app.srv.URL == "" || app.srv.APIKey == "" {
					continue
				}
				qc := arr.QbitClient{Host: host, Port: port, UseSSL: qurl.Scheme == "https",
					Username: a.Qbit.Username, Password: a.Qbit.Password, Category: app.kind}
				c := arrClient(app.kind, app.srv)
				added, err := c.EnsureQbittorrent(ctx, qc)
				out.add("qBittorrent in "+app.name, added, err)
				if viaHost && err == nil {
					st := stackOf(a.Stack)
					// Its folder, before the first download makes it (Radarr
					// and Sonarr warn it's missing until then).
					_ = os.MkdirAll(st.HostDownloads()+app.kind, 0o755)
					added, err := c.EnsureRemotePathMapping(ctx, host, st.HostDownloads(), st.DownloadsPath()+"/")
					out.add(app.name+" finds "+st.HostDownloads()+" at /data/downloads", added, err)
				}
			}
			_ = (&arr.Qbit{BaseURL: a.Qbit.URL, Username: a.Qbit.Username, Password: a.Qbit.Password}).Prepare(ctx, "")
		}
	}
	if a.Prowlarr.URL != "" && a.Prowlarr.APIKey != "" {
		p := &arr.Client{Kind: arr.Prowlarr, BaseURL: a.Prowlarr.URL, APIKey: a.Prowlarr.APIKey}
		for _, app := range apps {
			if app.srv.URL == "" || app.srv.APIKey == "" {
				continue
			}
			added, err := p.EnsureApp(ctx, arr.ProwlarrApp{Kind: app.kind, BaseURL: or(app.from, app.srv.URL),
				ProwlarrURL: or(in.ProwlarrFromApps, a.Prowlarr.URL), APIKey: app.srv.APIKey})
			out.add(app.name+" in Prowlarr (it gets Prowlarr's indexers)", added, err)
		}
		if fs := strings.TrimSpace(in.FlareSolverr); fs != "" {
			added, err := p.EnsureFlareSolverr(ctx, withScheme(fs))
			out.add("FlareSolverr in Prowlarr (for indexers tagged flaresolverr)", added, err)
		}
	}
	return out
}

func withScheme(u string) string {
	if strings.HasPrefix(u, "http://") || strings.HasPrefix(u, "https://") {
		return u
	}
	return "http://" + u
}
