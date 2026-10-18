package remote

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Sidecar states.
const (
	StateDisabled = "disabled"
	// StateWaiting: turned on, but the server has no admin account yet, so
	// nothing is opened to the internet (the first-run page would be).
	StateWaiting  = "waiting"
	StateMissing  = "missing" // kinora-iroh not found
	StateStarting = "starting"
	StateReady    = "ready"
	StateError    = "error"
)

const (
	backoffMin = time.Second
	backoffMax = 30 * time.Second
	// A run this long resets the backoff.
	stableRun = time.Minute
	stopGrace = 3 * time.Second
	keepNotes = 20
)

// Sidecar starts and supervises kinora-iroh: restarts it with backoff,
// reads its status lines, and sends it block commands.
type Sidecar struct {
	// Bin is kinora-iroh's path ("" finds it; see FindBinary).
	Bin string
	// DataDir is the server's data folder; the sidecar keeps its key in
	// <DataDir>/iroh, so the server's address survives restarts.
	DataDir string
	// Target is the tunnel listener, normally 127.0.0.1:8421.
	Target string
	// Secret goes to the sidecar (environment, not the command line) and
	// comes back in every connection's header.
	Secret string
	// Blocked returns the persisted refused list, passed on every launch.
	Blocked func(ctx context.Context) ([]string, error)
	// Allowed says whether it may run now (false until an admin exists).
	Allowed func(ctx context.Context) bool
	Log     *slog.Logger

	// relayURLs gives the self-hosted iroh relays to use instead of the
	// public ones (nil or empty: the public ones). Set with SetRelays; read
	// at each launch.
	relayURLs func() []string
	// launchedRelays are the relays the running process was started with.
	launchedRelays string
	// offRelay counts status reports in a row from a relay that isn't ours;
	// lastHeal is when that last made us restart.
	offRelay int
	lastHeal time.Time

	mu       sync.Mutex
	enabled  bool
	udpPort  int
	state    string
	lastErr  string
	status   *Status
	nodeID   string
	ticket   string
	started  time.Time
	restarts int
	notes    []Note
	stdin    io.WriteCloser
	// stop ends the running process (set while one runs).
	stop context.CancelFunc
	wake chan struct{}
}

// Info is the sidecar's state for the manager page and /health.
type Info struct {
	Enabled  bool    `json:"enabled"`
	UDPPort  int     `json:"udpPort"`
	State    string  `json:"state"`
	Error    string  `json:"error,omitempty"`
	NodeID   string  `json:"nodeId,omitempty"`
	Ticket   string  `json:"ticket,omitempty"`
	Since    int64   `json:"since,omitempty"` // Unix seconds the current run started
	Restarts int     `json:"restarts"`
	Status   *Status `json:"status,omitempty"`
	Notes    []Note  `json:"notes"`
}

func (s *Sidecar) init() {
	s.mu.Lock()
	if s.wake == nil {
		s.wake = make(chan struct{}, 1)
	}
	if s.state == "" {
		s.state = StateDisabled
	}
	s.mu.Unlock()
}

func (s *Sidecar) poke() {
	select {
	case s.wake <- struct{}{}:
	default:
	}
}

// Configure turns remote access on or off and sets the UDP port. A change
// restarts the sidecar.
func (s *Sidecar) Configure(enabled bool, udpPort int) {
	s.init()
	s.mu.Lock()
	changed := s.enabled != enabled || s.udpPort != udpPort
	s.enabled, s.udpPort = enabled, udpPort
	if changed {
		s.restarts = 0
		if !enabled {
			s.state, s.lastErr, s.status = StateDisabled, "", nil
		}
		if s.stop != nil {
			s.stop()
		}
	}
	s.mu.Unlock()
	s.poke()
}

// SetRelays sets where the relay list comes from (safe while it runs).
func (s *Sidecar) SetRelays(f func() []string) {
	s.mu.Lock()
	s.relayURLs = f
	s.mu.Unlock()
}

// wantedRelays is the relay list to launch with, joined ("" for the public ones).
func (s *Sidecar) wantedRelays() string {
	s.mu.Lock()
	f := s.relayURLs
	s.mu.Unlock()
	if f == nil {
		return ""
	}
	return strings.Join(f(), ",")
}

// Reload restarts a running sidecar so it picks up settings it only reads
// when it starts (the relays to use). It does nothing when the running one
// already has them, so calling it whenever something might have changed is
// cheap.
func (s *Sidecar) Reload() {
	s.init()
	want := s.wantedRelays()
	s.mu.Lock()
	if s.enabled && s.stop != nil && s.launchedRelays != want {
		s.Log.Info("remote access: restarting kinora-iroh for new relays", "from", relayLabel(s.launchedRelays), "to", relayLabel(want))
		s.restarts = 0
		s.stop()
	}
	s.mu.Unlock()
	s.poke()
}

func relayLabel(joined string) string {
	if joined == "" {
		return "public relays"
	}
	return joined
}

// How many status reports in a row from someone else's relay before
// restarting onto ours, and how often that may happen at most.
const (
	offRelayReports = 3
	healEvery       = 5 * time.Minute
)

// checkRelay restarts the sidecar when it reports a relay that isn't one of
// ours (it can only have been started without our list). Called with s.mu
// held; reports whether to restart.
func (s *Sidecar) checkRelay(st *Status) bool {
	if s.relayURLs == nil || st.Relay.URL == nil || *st.Relay.URL == "" {
		s.offRelay = 0
		return false
	}
	want := s.relayURLs()
	if len(want) == 0 {
		s.offRelay = 0
		return false
	}
	got := relayKey(*st.Relay.URL)
	for _, u := range want {
		if relayKey(u) == got {
			s.offRelay = 0
			return false
		}
	}
	s.offRelay++
	if s.offRelay < offRelayReports || time.Since(s.lastHeal) < healEvery {
		return false
	}
	s.offRelay, s.lastHeal = 0, time.Now()
	s.launchedRelays = "?" // whatever it was started with, it's wrong
	return true
}

// relayKey is a relay URL reduced to host and port, so "https://a:8443/" and
// "https://a:8443" match.
func relayKey(u string) string {
	k := strings.ToLower(strings.TrimSpace(u))
	k = strings.TrimPrefix(strings.TrimPrefix(k, "https://"), "http://")
	k = strings.TrimSuffix(k, "/")
	host, port := k, "443"
	if i := strings.LastIndex(k, ":"); i >= 0 {
		host, port = k[:i], k[i+1:]
	}
	// iroh writes names fully qualified ("relay.example.com.").
	return strings.TrimSuffix(host, ".") + ":" + port
}

// Info returns the current state.
func (s *Sidecar) Info() Info {
	s.init()
	s.mu.Lock()
	defer s.mu.Unlock()
	in := Info{Enabled: s.enabled, UDPPort: s.udpPort, State: s.state, Error: s.lastErr, NodeID: s.nodeID,
		Ticket: s.ticket, Restarts: s.restarts, Notes: append([]Note{}, s.notes...)}
	if !s.started.IsZero() && (s.state == StateReady || s.state == StateStarting) {
		in.Since = s.started.Unix()
	}
	if s.status != nil {
		st := *s.status
		in.Status = &st
	}
	return in
}

// Block refuses a device now (if the sidecar runs). The caller persists it.
func (s *Sidecar) Block(nodeID string) { s.send(map[string]string{"cmd": "block", "nodeId": nodeID}) }

// Unblock lets a device back in (if the sidecar runs).
func (s *Sidecar) Unblock(nodeID string) {
	s.send(map[string]string{"cmd": "unblock", "nodeId": nodeID})
}

func (s *Sidecar) send(cmd map[string]string) {
	b, _ := json.Marshal(cmd)
	s.mu.Lock()
	w := s.stdin
	s.mu.Unlock()
	if w != nil {
		_, _ = w.Write(append(b, '\n'))
	}
}

func (s *Sidecar) setState(state, err string) {
	s.mu.Lock()
	s.state, s.lastErr = state, err
	s.mu.Unlock()
}

func (s *Sidecar) note(code, msg string) {
	s.mu.Lock()
	s.notes = append([]Note{{At: time.Now().Unix(), Code: code, Message: msg}}, s.notes...)
	if len(s.notes) > keepNotes {
		s.notes = s.notes[:keepNotes]
	}
	s.mu.Unlock()
}

// FindBinary finds kinora-iroh: KINORA_IROH, next to the server, or on PATH.
func FindBinary() string {
	if p := os.Getenv("KINORA_IROH"); p != "" {
		return p
	}
	name := "kinora-iroh"
	if runtime.GOOS == "windows" {
		name += ".exe"
	}
	if exe, err := os.Executable(); err == nil {
		p := filepath.Join(filepath.Dir(exe), name)
		if fi, err := os.Stat(p); err == nil && !fi.IsDir() {
			return p
		}
	}
	if p, err := exec.LookPath(name); err == nil {
		return p
	}
	return ""
}

// Run supervises the sidecar until ctx ends, starting it whenever remote
// access is on.
func (s *Sidecar) Run(ctx context.Context) {
	s.init()
	backoff := backoffMin
	for {
		s.mu.Lock()
		enabled, port := s.enabled, s.udpPort
		s.mu.Unlock()

		wait := time.Duration(0)
		switch {
		case !enabled:
			s.setState(StateDisabled, "")
			wait = -1
		case s.Allowed != nil && !s.Allowed(ctx):
			s.setState(StateWaiting, "Create the admin account first.")
			wait = 10 * time.Second
		default:
			bin := s.Bin
			if bin == "" {
				bin = FindBinary()
			}
			if bin == "" {
				s.setState(StateMissing, "kinora-iroh wasn't found next to the server or on PATH (or set KINORA_IROH).")
				wait = time.Minute
				break
			}
			ranFor, code, err := s.runOnce(ctx, bin, port)
			if ctx.Err() != nil {
				return
			}
			s.mu.Lock()
			stillOn := s.enabled && s.udpPort == port
			s.mu.Unlock()
			if !stillOn {
				// Turned off or changed: go round at once.
				backoff = backoffMin
				continue
			}
			if code == 2 {
				// Bad configuration; a restart won't fix it.
				msg := "kinora-iroh refused its configuration"
				if err != nil {
					msg += ": " + err.Error()
				}
				s.setState(StateError, msg)
				s.Log.Error("remote access stopped", "err", msg)
				wait = -1
				break
			}
			if ranFor >= stableRun {
				backoff = backoffMin
			}
			msg := "kinora-iroh stopped"
			if err != nil {
				msg += ": " + err.Error()
			}
			s.setState(StateError, msg+"; restarting")
			s.mu.Lock()
			s.restarts++
			s.mu.Unlock()
			s.Log.Warn("remote access: sidecar exited, restarting", "err", err, "in", backoff)
			wait = backoff
			backoff = min(backoff*2, backoffMax)
		}

		var timer <-chan time.Time
		if wait >= 0 {
			timer = time.After(wait)
		}
		select {
		case <-ctx.Done():
			return
		case <-s.wake:
		case <-timer:
		}
	}
}

// runOnce starts the sidecar and waits for it to end. code is its exit
// code (-1 when unknown).
func (s *Sidecar) runOnce(ctx context.Context, bin string, port int) (time.Duration, int, error) {
	var blocked []string
	if s.Blocked != nil {
		b, err := s.Blocked(ctx)
		if err != nil {
			return 0, -1, err
		}
		blocked = b
	}
	args := []string{"--data-dir", s.DataDir, "--target", s.Target}
	if port > 0 {
		args = append(args, "--udp-port", strconv.Itoa(port))
	}
	if len(blocked) > 0 {
		args = append(args, "--block", strings.Join(blocked, ","))
	}
	cmd := exec.Command(bin, args...)
	cmd.Env = append(os.Environ(), "KINORA_IROH_SECRET="+s.Secret)
	relays := s.wantedRelays()
	if relays != "" {
		cmd.Env = append(cmd.Env, "KINORA_IROH_RELAY_MODE=custom", "KINORA_IROH_RELAY_URLS="+relays)
	}
	hideWindow(cmd)
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return 0, -1, err
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return 0, -1, err
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		return 0, -1, err
	}
	s.setState(StateStarting, "")
	if err := cmd.Start(); err != nil {
		return 0, -1, err
	}
	start := time.Now()
	runCtx, stop := context.WithCancel(ctx)
	s.mu.Lock()
	s.stdin, s.stop, s.started, s.status = stdin, stop, start, nil
	s.launchedRelays, s.offRelay = relays, 0
	s.mu.Unlock()
	s.Log.Info("remote access: started kinora-iroh", "pid", cmd.Process.Pid, "udpPort", port, "blocked", len(blocked), "relays", relayLabel(relays))

	var wg sync.WaitGroup
	wg.Add(2)
	go func() { defer wg.Done(); s.readEvents(stdout) }()
	go func() { defer wg.Done(); s.readLogs(stderr) }()

	exited := make(chan error, 1)
	go func() {
		// Wait after the pipes are drained, as exec requires.
		wg.Wait()
		exited <- cmd.Wait()
	}()

	var werr error
	select {
	case werr = <-exited:
	case <-runCtx.Done():
		// Closing stdin is the stop signal; kill it if it lingers.
		_ = stdin.Close()
		select {
		case werr = <-exited:
		case <-time.After(stopGrace):
			_ = cmd.Process.Kill()
			werr = <-exited
		}
	}
	stop()
	s.mu.Lock()
	s.stdin, s.stop = nil, nil
	s.mu.Unlock()
	_ = stdin.Close()

	code := -1
	if cmd.ProcessState != nil {
		code = cmd.ProcessState.ExitCode()
	}
	if werr != nil {
		var ee *exec.ExitError
		if !errors.As(werr, &ee) {
			return time.Since(start), code, werr
		}
	}
	// Prefer the sidecar's own last error line as the reason.
	s.mu.Lock()
	var reason error
	if len(s.notes) > 0 && s.notes[0].At >= start.Unix() && isFatal(s.notes[0].Code) {
		reason = errors.New(s.notes[0].Message)
	}
	s.mu.Unlock()
	if reason == nil && code != 0 {
		reason = errors.New("exit code " + strconv.Itoa(code))
	}
	return time.Since(start), code, reason
}

func isFatal(code string) bool {
	return code == "config" || code == "bind_failed" || code == "runtime"
}

func (s *Sidecar) readEvents(r io.Reader) {
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 0, 64<<10), 4<<20)
	for sc.Scan() {
		line := sc.Bytes()
		var ev event
		if err := json.Unmarshal(line, &ev); err != nil {
			s.Log.Warn("remote access: unreadable line from kinora-iroh", "err", err)
			continue
		}
		switch ev.Type {
		case "ready":
			s.mu.Lock()
			s.state, s.lastErr, s.nodeID, s.ticket = StateReady, "", ev.NodeID, ev.Ticket
			s.mu.Unlock()
			s.Log.Info("remote access ready", "nodeId", ev.NodeID)
		case "status":
			var st Status
			if err := json.Unmarshal(line, &st); err != nil {
				continue
			}
			s.mu.Lock()
			s.status = &st
			if st.NodeID != "" {
				s.nodeID = st.NodeID
			}
			if st.Ticket != "" {
				s.ticket = st.Ticket
			}
			heal := s.checkRelay(&st)
			s.mu.Unlock()
			if heal {
				s.Log.Warn("remote access: kinora-iroh is on a relay that isn't ours; restarting it", "relay", *st.Relay.URL)
				s.note("relay_switched_back", "Remote access was using a public relay instead of yours. It was restarted on your relay.")
				s.Reload()
			}
		case "error":
			s.note(ev.Code, ev.Message)
			s.Log.Warn("remote access", "code", ev.Code, "message", ev.Message)
		case "peer_connected":
			s.Log.Info("remote device connected", "nodeId", ev.NodeID, "path", ev.Path)
		case "peer_disconnected":
			s.Log.Info("remote device disconnected", "nodeId", ev.NodeID, "reason", ev.Reason)
		}
	}
}

func (s *Sidecar) readLogs(r io.Reader) {
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 0, 16<<10), 1<<20)
	for sc.Scan() {
		s.Log.Debug("kinora-iroh: " + sc.Text())
	}
}
