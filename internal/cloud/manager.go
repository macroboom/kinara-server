package cloud

import (
	"bufio"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Settings come from the environment (see main).
type Settings struct {
	// CloudURL is kinara-cloud's API, "https://cloud.<domain>". Empty turns
	// the cloud off.
	CloudURL string
	// DataDir is the server's data folder; state lives in <DataDir>/cloud.
	DataDir string
	// LANPort is the plain HTTP port the server listens on.
	LANPort int
	// HTTPSPort is the port the HTTPS listener opens (and asks the router
	// to forward). 0 turns direct connections off; the relay still works.
	HTTPSPort int
	// ManualPort: the router already forwards this external port to
	// HTTPSPort (for routers without UPnP or NAT-PMP).
	ManualPort int
	// NoPortMap turns off UPnP and NAT-PMP.
	NoPortMap bool
	// RelayConns is how many relay connections wait at once.
	RelayConns int
	Version    string
	Name       func() string
}

// Iroh reports the sidecar's address for presence (nil: no sidecar).
type Iroh func() (ticket, nodeID string)

// Manager runs everything; it is safe to call from handlers.
type Manager struct {
	S       Settings
	Handler http.Handler // the server's API, served over HTTPS and the relay
	Log     *slog.Logger
	Iroh    Iroh
	// HTTPClient talks to the cloud (tests set one).
	HTTPClient *http.Client
	// RelaysChanged is called (in its own goroutine) when the iroh relays to
	// use change: on linking, unlinking, or a new list from the cloud. The
	// sidecar only reads them when it starts, so it restarts on this.
	RelaysChanged func()

	file   stateFile
	client *client
	mapper portMapper

	mu       sync.Mutex
	state    *State
	link     *pendingLink
	cert     *tls.Certificate
	certErr  string
	mapping  *Mapping
	mapErr   string
	last     *serverInfo
	lastErr  string
	reported time.Time
	relayUp  int
	wake     chan struct{}
	stopLink context.CancelFunc
}

type pendingLink struct {
	Code      string
	VerifyURL string
	Complete  string
	Expires   time.Time
	Err       string
	userID    int64
}

// New makes a manager; call Run to start it.
func New(s Settings, h http.Handler, log *slog.Logger) *Manager {
	if s.RelayConns <= 0 {
		s.RelayConns = 4
	}
	m := &Manager{
		S: s, Handler: h, Log: log,
		file: stateFile{path: filepath.Join(s.DataDir, "cloud", "state.json")},
		wake: make(chan struct{}, 1),
	}
	if !s.NoPortMap {
		m.mapper = autoMapper{
			gateway: guessGateway,
			localIP: func() (net.IP, error) { return outboundIP("udp4", "8.8.8.8:53") },
		}
	}
	return m
}

// Enabled reports whether a cloud URL is configured.
func (m *Manager) Enabled() bool { return m != nil && m.S.CloudURL != "" }

func (m *Manager) http() *http.Client {
	if m.HTTPClient != nil {
		return m.HTTPClient
	}
	return &http.Client{Timeout: 5 * time.Minute}
}

func (m *Manager) poke() {
	select {
	case m.wake <- struct{}{}:
	default:
	}
}

// State is the linked state, or nil.
func (m *Manager) State() *State {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.state == nil {
		return nil
	}
	s := *m.state
	return &s
}

// Run loads the saved link and keeps the server reachable until ctx ends.
func (m *Manager) Run(ctx context.Context) {
	if !m.Enabled() {
		return
	}
	m.client = &client{base: strings.TrimSuffix(m.S.CloudURL, "/"), hc: m.http()}
	st, err := m.file.load()
	if err != nil {
		m.Log.Error("cloud: reading saved link", "err", err)
	}
	m.mu.Lock()
	m.state = st
	m.mu.Unlock()
	m.loadCert()
	// Remote access may have started before the saved link was read, and so
	// without our relays: let it check (it restarts only if they differ).
	if st != nil && len(st.IrohRelays) > 0 {
		m.relaysChanged()
	}

	if m.S.HTTPSPort > 0 {
		go m.serveHTTPS(ctx)
	}
	go m.relayLoop(ctx)
	go m.certLoop(ctx)
	m.presenceLoop(ctx)
}

// --- linking ------------------------------------------------------------------------

// LinkStatus is shown on the manager's Remote access page.
type LinkStatus struct {
	Enabled    bool      `json:"enabled"`
	Linked     bool      `json:"linked"`
	ServerID   string    `json:"serverId,omitempty"`
	Owner      *Owner    `json:"owner,omitempty"`
	Code       string    `json:"code,omitempty"`
	VerifyURL  string    `json:"verifyUrl,omitempty"`
	LinkURL    string    `json:"linkUrl,omitempty"`
	Expires    time.Time `json:"codeExpires,omitempty"`
	LinkError  string    `json:"linkError,omitempty"`
	Reachable  []string  `json:"reachable"`
	RelayUp    bool      `json:"relayConnected"`
	CertReady  bool      `json:"certReady"`
	CertError  string    `json:"certError,omitempty"`
	PortMap    string    `json:"portMap,omitempty"`
	PortError  string    `json:"portMapError,omitempty"`
	PublicV4   string    `json:"publicV4,omitempty"`
	HTTPSPort  int       `json:"httpsPort"`
	LastReport time.Time `json:"lastReport,omitempty"`
	CloudError string    `json:"cloudError,omitempty"`
	RelayURL   string    `json:"relayUrl,omitempty"`
}

// Status is the current picture.
func (m *Manager) Status() LinkStatus {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := LinkStatus{Enabled: m.Enabled(), Reachable: []string{}, HTTPSPort: m.S.HTTPSPort,
		CertReady: m.cert != nil, CertError: m.certErr, PortError: m.mapErr,
		LastReport: m.reported, CloudError: m.lastErr, RelayUp: m.relayUp > 0}
	if m.state != nil {
		out.Linked = true
		out.ServerID = m.state.ServerID
		o := m.state.Owner
		out.Owner = &o
		out.RelayURL = "https://" + m.state.RelayHost
	}
	if m.link != nil {
		out.Code, out.VerifyURL, out.LinkURL, out.Expires, out.LinkError = m.link.Code, m.link.VerifyURL, m.link.Complete, m.link.Expires, m.link.Err
	}
	if m.mapping != nil {
		out.PortMap = fmt.Sprintf("%s: %s:%d", m.mapping.Method, m.mapping.ExternalIP, m.mapping.External)
	}
	if l := m.last; l != nil {
		out.PublicV4 = l.PublicV4
		if l.Direct4OK {
			out.Reachable = append(out.Reachable, "ipv4")
		}
		if len(l.Direct6OK) > 0 {
			out.Reachable = append(out.Reachable, "ipv6")
		}
		if l.RelayConnected || m.relayUp > 0 {
			out.Reachable = append(out.Reachable, "relay")
		}
	}
	return out
}

// StartLink asks the cloud for a code; userID is the admin linking, whom
// the account's owner will sign in as.
func (m *Manager) StartLink(ctx context.Context, userID int64) (LinkStatus, error) {
	if !m.Enabled() {
		return LinkStatus{}, errors.New("this server has no cloud address set (KINARA_CLOUD_URL)")
	}
	if m.State() != nil {
		return m.Status(), errors.New("already linked; unlink first")
	}
	name := "Kinara server"
	if m.S.Name != nil {
		name = m.S.Name()
	}
	var ls linkStart
	if _, err := m.client.do(ctx, "POST", "/v1/link/start", "", map[string]string{"kind": "server", "name": name}, &ls); err != nil {
		return LinkStatus{}, err
	}
	m.mu.Lock()
	if m.stopLink != nil {
		m.stopLink()
	}
	lctx, cancel := context.WithTimeout(context.Background(), time.Duration(ls.ExpiresIn)*time.Second)
	m.stopLink = cancel
	m.link = &pendingLink{Code: ls.Code, VerifyURL: ls.VerifyURL, Complete: ls.VerifyURLComplete,
		Expires: time.Now().Add(time.Duration(ls.ExpiresIn) * time.Second), userID: userID}
	m.mu.Unlock()
	go m.pollLink(lctx, ls, userID)
	return m.Status(), nil
}

func (m *Manager) pollLink(ctx context.Context, ls linkStart, userID int64) {
	every := time.Duration(ls.Interval) * time.Second
	if every <= 0 {
		every = 3 * time.Second
	}
	for {
		select {
		case <-ctx.Done():
			m.mu.Lock()
			if m.link != nil && m.link.Code == ls.Code {
				m.link = nil
			}
			m.mu.Unlock()
			return
		case <-time.After(every):
		}
		var got serverInfo
		st, err := m.client.do(ctx, "POST", "/v1/link/poll", "", map[string]string{"pollToken": ls.PollToken}, &got)
		if st == http.StatusAccepted {
			continue
		}
		if err != nil {
			var ae *apiError
			if errors.As(err, &ae) && ae.Status == http.StatusGone {
				m.mu.Lock()
				m.link = nil
				m.mu.Unlock()
				return
			}
			continue // network trouble: keep trying until the code runs out
		}
		s := &State{
			ServerID: got.ServerID, ServerSecret: got.ServerSecret, Owner: got.Account, OwnerUserID: userID,
			DirectZone: got.DirectZone, CertName: got.CertName, RelayHost: got.RelayHost,
			IrohRelays: got.IrohRelays, CloudKeyID: got.CloudKeyID, CloudKey: got.CloudKey, Linked: time.Now(),
		}
		if err := m.file.save(s); err != nil {
			m.mu.Lock()
			m.link.Err = "Couldn't save the link: " + err.Error()
			m.mu.Unlock()
			return
		}
		m.mu.Lock()
		m.state, m.link = s, nil
		m.mu.Unlock()
		m.Log.Info("cloud: linked", "server", s.ServerID, "account", s.Owner.Email)
		m.relaysChanged()
		m.poke()
		return
	}
}

// CancelLink stops waiting for a code.
func (m *Manager) CancelLink() {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.stopLink != nil {
		m.stopLink()
	}
	m.link = nil
}

// Unlink forgets the account (the owner can also remove it in the cloud).
func (m *Manager) Unlink(ctx context.Context) error {
	m.mu.Lock()
	m.state, m.cert, m.last = nil, nil, nil
	mp := m.mapping
	m.mapping = nil
	m.mu.Unlock()
	if mp != nil && m.mapper != nil && mp.Method != "manual" {
		m.mapper.Unmap(ctx, mp)
	}
	os.Remove(filepath.Join(m.S.DataDir, "cloud", "cert.pem"))
	os.Remove(filepath.Join(m.S.DataDir, "cloud", "key.pem"))
	m.relaysChanged()
	return m.file.save(nil)
}

func (m *Manager) relaysChanged() {
	if m.RelaysChanged != nil {
		go m.RelaysChanged()
	}
}

// VerifyAccess checks an app's account token for this server and returns
// the local user it signs in as.
func (m *Manager) VerifyAccess(token string) (int64, Owner, error) {
	st := m.State()
	if st == nil {
		return 0, Owner{}, errors.New("this server isn't linked to a Kinara account")
	}
	c, err := verifyAccess(token, st.CloudKey, time.Now())
	if err != nil {
		return 0, Owner{}, err
	}
	if c.Server != st.ServerID {
		return 0, Owner{}, errors.New("that token is for another server")
	}
	if c.Role != "owner" || c.Subject != st.Owner.ID {
		return 0, Owner{}, errors.New("that account doesn't have access to this server")
	}
	return st.OwnerUserID, Owner{ID: c.Subject, Email: c.Email, Name: c.Name}, nil
}

// --- presence -----------------------------------------------------------------------

func (m *Manager) presenceLoop(ctx context.Context) {
	for {
		every := m.report(ctx)
		select {
		case <-ctx.Done():
			return
		case <-m.wake:
		case <-time.After(every):
		}
	}
}

// report tells the cloud how to reach us; returns when to report next.
func (m *Manager) report(ctx context.Context) time.Duration {
	st := m.State()
	if st == nil {
		return time.Minute
	}
	m.refreshMapping(ctx)
	lan, v6 := localAddrs()
	m.mu.Lock()
	p := presenceOut{
		Version: m.S.Version, LocalAddrs: lan, Port: m.S.LANPort, IPv6: v6,
		HTTPSPort: m.S.HTTPSPort, CertReady: m.cert != nil,
	}
	if mp := m.mapping; mp != nil {
		p.MappedPort, p.PublicV4 = mp.External, mp.ExternalIP
	}
	m.mu.Unlock()
	if m.S.Name != nil {
		p.Name = m.S.Name()
	}
	if m.Iroh != nil {
		p.IrohTicket, p.IrohNode = m.Iroh()
	}
	var got serverInfo
	rctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	_, err := m.client.do(rctx, "PUT", "/v1/server/presence", st.ServerSecret, p, &got)
	m.mu.Lock()
	defer m.mu.Unlock()
	if err != nil {
		m.lastErr = err.Error()
		if isUnlinked(err) {
			m.Log.Warn("cloud: this server was removed from its account; unlinking")
			m.state, m.cert = nil, nil
			go m.file.save(nil)
			m.relaysChanged()
		}
		return 30 * time.Second
	}
	m.lastErr, m.last, m.reported = "", &got, time.Now()
	// Pick up a new cloud key or relay list.
	if got.CloudKey != "" && (got.CloudKey != m.state.CloudKey || strings.Join(got.IrohRelays, ",") != strings.Join(m.state.IrohRelays, ",")) {
		relays := strings.Join(got.IrohRelays, ",") != strings.Join(m.state.IrohRelays, ",")
		m.state.CloudKey, m.state.CloudKeyID, m.state.IrohRelays = got.CloudKey, got.CloudKeyID, got.IrohRelays
		s := *m.state
		go m.file.save(&s)
		if relays {
			m.Log.Info("cloud: new iroh relays", "relays", got.IrohRelays)
			m.relaysChanged()
		}
	}
	return time.Minute
}

// refreshMapping keeps a router port open for the HTTPS listener.
func (m *Manager) refreshMapping(ctx context.Context) {
	if m.S.HTTPSPort == 0 {
		return
	}
	if m.S.ManualPort > 0 {
		m.mu.Lock()
		m.mapping = &Mapping{Method: "manual", External: m.S.ManualPort, Internal: m.S.HTTPSPort}
		m.mu.Unlock()
		return
	}
	if m.mapper == nil {
		return
	}
	m.mu.Lock()
	cur := m.mapping
	m.mu.Unlock()
	// NAT-PMP leases are renewed at half-life; UPnP every 20 minutes.
	if cur != nil && cur.Lifetime > 0 && time.Since(m.reported) < cur.Lifetime/2 {
		return
	}
	mctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	ext := m.S.HTTPSPort
	if cur != nil {
		ext = cur.External
	}
	mp, err := m.mapper.Map(mctx, m.S.HTTPSPort, ext, time.Hour)
	m.mu.Lock()
	defer m.mu.Unlock()
	if err != nil {
		if m.mapErr != err.Error() {
			m.Log.Info("cloud: couldn't open a port on the router; the relay will be used", "err", err)
		}
		m.mapErr, m.mapping = err.Error(), nil
		return
	}
	if m.mapping == nil {
		m.Log.Info("cloud: router port open", "method", mp.Method, "external", mp.ExternalIP, "port", mp.External)
	}
	m.mapErr, m.mapping = "", mp
}

// --- certificate --------------------------------------------------------------------

func (m *Manager) certPaths() (string, string) {
	d := filepath.Join(m.S.DataDir, "cloud")
	return filepath.Join(d, "cert.pem"), filepath.Join(d, "key.pem")
}

func (m *Manager) loadCert() {
	cp, kp := m.certPaths()
	c, err := tls.LoadX509KeyPair(cp, kp)
	if err != nil {
		return
	}
	st := m.State()
	leaf, err := x509.ParseCertificate(c.Certificate[0])
	if err != nil || st == nil || leaf.VerifyHostname("x."+st.DirectZone) != nil {
		return
	}
	c.Leaf = leaf
	m.mu.Lock()
	m.cert = &c
	m.mu.Unlock()
}

func (m *Manager) certDue() bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.state == nil {
		return false
	}
	return m.cert == nil || m.cert.Leaf == nil || time.Until(m.cert.Leaf.NotAfter) < 30*24*time.Hour
}

func (m *Manager) certLoop(ctx context.Context) {
	wait := 5 * time.Second
	for {
		select {
		case <-ctx.Done():
			return
		case <-time.After(wait):
		}
		if m.S.HTTPSPort == 0 {
			return
		}
		if m.State() == nil {
			wait = 10 * time.Second // not linked yet
			continue
		}
		wait = 6 * time.Hour
		if !m.certDue() {
			continue
		}
		if err := m.renewCert(ctx); err != nil {
			m.mu.Lock()
			m.certErr = err.Error()
			m.mu.Unlock()
			m.Log.Warn("cloud: certificate", "err", err)
			wait = 30 * time.Minute
			var ae *apiError
			if errors.As(err, &ae) && ae.Code == "cert_recent" {
				wait = 6 * time.Hour
			}
			continue
		}
		m.mu.Lock()
		m.certErr = ""
		m.mu.Unlock()
		m.poke()
	}
}

func (m *Manager) renewCert(ctx context.Context) error {
	st := m.State()
	if st == nil {
		return nil
	}
	key, csr, err := newKeyAndCSR(st.CertName)
	if err != nil {
		return err
	}
	var out struct {
		Chain string `json:"chain"`
	}
	cctx, cancel := context.WithTimeout(ctx, 5*time.Minute)
	defer cancel()
	if _, err := m.client.do(cctx, "POST", "/v1/server/cert", st.ServerSecret,
		map[string]string{"csr": string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE REQUEST", Bytes: csr}))}, &out); err != nil {
		return err
	}
	c, err := tls.X509KeyPair([]byte(out.Chain), key)
	if err != nil {
		return err
	}
	c.Leaf, _ = x509.ParseCertificate(c.Certificate[0])
	cp, kp := m.certPaths()
	os.MkdirAll(filepath.Dir(cp), 0o700)
	if err := os.WriteFile(kp, key, 0o600); err != nil {
		return err
	}
	if err := os.WriteFile(cp, []byte(out.Chain), 0o644); err != nil {
		return err
	}
	m.mu.Lock()
	m.cert = &c
	m.mu.Unlock()
	m.Log.Info("cloud: certificate ready", "name", st.CertName, "expires", c.Leaf.NotAfter.Format(time.DateOnly))
	return nil
}

// --- HTTPS listener -----------------------------------------------------------------

func (m *Manager) serveHTTPS(ctx context.Context) {
	cfg := &tls.Config{
		MinVersion: tls.VersionTLS12,
		NextProtos: []string{"h2", "http/1.1"},
		GetCertificate: func(*tls.ClientHelloInfo) (*tls.Certificate, error) {
			m.mu.Lock()
			defer m.mu.Unlock()
			if m.cert == nil {
				return nil, errors.New("no certificate yet")
			}
			return m.cert, nil
		},
	}
	srv := &http.Server{
		Handler:           httpsHandler(m.Handler),
		TLSConfig:         cfg,
		ReadHeaderTimeout: 10 * time.Second,
		IdleTimeout:       2 * time.Minute,
		ErrorLog:          nil,
	}
	go func() { <-ctx.Done(); srv.Close() }()
	for ctx.Err() == nil {
		ln, err := net.Listen("tcp", ":"+strconv.Itoa(m.S.HTTPSPort))
		if err != nil {
			m.Log.Error("cloud: HTTPS listener", "port", m.S.HTTPSPort, "err", err)
			select {
			case <-ctx.Done():
				return
			case <-time.After(time.Minute):
				continue
			}
		}
		m.Log.Info("cloud: HTTPS listening", "port", m.S.HTTPSPort)
		srv.ServeTLS(ln, "", "")
	}
}

// --- relay --------------------------------------------------------------------------

type relayListener struct {
	conns chan net.Conn
	done  chan struct{}
	once  sync.Once
}

func (l *relayListener) Accept() (net.Conn, error) {
	select {
	case c := <-l.conns:
		return c, nil
	case <-l.done:
		return nil, net.ErrClosed
	}
}
func (l *relayListener) Close() error   { l.once.Do(func() { close(l.done) }); return nil }
func (l *relayListener) Addr() net.Addr { return &net.TCPAddr{} }

// bufConn reads through the bufio.Reader that already holds the request.
type bufConn struct {
	net.Conn
	r *bufio.Reader
}

func (c *bufConn) Read(p []byte) (int, error) { return c.r.Read(p) }

// relayLoop keeps RelayConns connections waiting at the cloud. Each one
// carries a single request; a new one is opened as soon as one is used.
func (m *Manager) relayLoop(ctx context.Context) {
	ln := &relayListener{conns: make(chan net.Conn), done: make(chan struct{})}
	srv := &http.Server{Handler: relayHandler(m.Handler), ReadHeaderTimeout: 30 * time.Second}
	go srv.Serve(ln)
	go func() { <-ctx.Done(); ln.Close(); srv.Close() }()

	slots := make(chan struct{}, m.S.RelayConns)
	for {
		select {
		case <-ctx.Done():
			return
		case slots <- struct{}{}:
		}
		go func() {
			defer func() { <-slots }()
			if err := m.relayOnce(ctx, ln); err != nil && ctx.Err() == nil {
				// Spread retries so a cloud restart isn't hit all at once.
				time.Sleep(time.Duration(2+time.Now().UnixNano()%5) * time.Second)
			}
		}()
	}
}

func (m *Manager) relayOnce(ctx context.Context, ln *relayListener) error {
	st := m.State()
	if st == nil {
		time.Sleep(10 * time.Second)
		return nil
	}
	u := m.client.base + "/v1/server/relay"
	req, _ := http.NewRequestWithContext(ctx, "GET", u, nil)
	req.Header.Set("Authorization", "Bearer "+st.ServerSecret)
	req.Header.Set("Connection", "Upgrade")
	req.Header.Set("Upgrade", "kinara-relay")
	// A plain transport, so the connection can be taken over after 101.
	tr := &http.Transport{Proxy: nil, ForceAttemptHTTP2: false, TLSClientConfig: &tls.Config{NextProtos: []string{"http/1.1"}}}
	if m.HTTPClient != nil {
		if t, ok := m.HTTPClient.Transport.(*http.Transport); ok {
			tr = t.Clone()
			tr.ForceAttemptHTTP2 = false
		}
	}
	defer tr.CloseIdleConnections()
	resp, err := (&http.Client{Transport: tr}).Do(req)
	if err != nil {
		return err
	}
	if resp.StatusCode != http.StatusSwitchingProtocols {
		resp.Body.Close()
		if resp.StatusCode == http.StatusUnauthorized {
			time.Sleep(time.Minute)
		}
		return fmt.Errorf("relay answered %d", resp.StatusCode)
	}
	rwc, ok := resp.Body.(io.ReadWriteCloser)
	if !ok {
		resp.Body.Close()
		return errors.New("relay connection can't be taken over")
	}
	m.mu.Lock()
	m.relayUp++
	m.mu.Unlock()
	defer func() {
		m.mu.Lock()
		m.relayUp--
		m.mu.Unlock()
	}()
	br := bufio.NewReader(rwc)
	// Heartbeats are zero bytes; anything else starts a request.
	for {
		b, err := br.ReadByte()
		if err != nil {
			rwc.Close()
			return err
		}
		if b != 0 {
			br.UnreadByte()
			break
		}
	}
	conn := &bufConn{Conn: rwcConn{rwc}, r: br}
	select {
	case ln.conns <- conn:
	case <-ln.done:
		rwc.Close()
	}
	return nil
}

// rwcConn turns the upgraded body into a net.Conn for http.Server.
type rwcConn struct{ io.ReadWriteCloser }

func (rwcConn) LocalAddr() net.Addr                { return &net.TCPAddr{} }
func (rwcConn) RemoteAddr() net.Addr               { return &net.TCPAddr{IP: net.IPv4(127, 0, 0, 2)} }
func (rwcConn) SetDeadline(t time.Time) error      { return nil }
func (rwcConn) SetReadDeadline(t time.Time) error  { return nil }
func (rwcConn) SetWriteDeadline(t time.Time) error { return nil }

type viaRelayKey struct{}
type internetKey struct{}

// ViaRelay reports whether a request came through the cloud relay.
func ViaRelay(r *http.Request) bool {
	v, _ := r.Context().Value(viaRelayKey{}).(bool)
	return v
}

// FromInternet reports whether a request came through the relay or from
// outside the home network to the HTTPS listener.
func FromInternet(r *http.Request) bool {
	v, _ := r.Context().Value(internetKey{}).(bool)
	return v
}

// httpsHandler marks requests to the HTTPS listener from outside the home
// network (the same listener also serves the LAN with a trusted name).
func httpsHandler(h http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		host, _, _ := net.SplitHostPort(r.RemoteAddr)
		ip := net.ParseIP(host)
		local := ip != nil && (ip.IsPrivate() || ip.IsLoopback() || ip.IsLinkLocalUnicast() ||
			(ip.To4() != nil && ip.To4()[0] == 100 && ip.To4()[1]&0xc0 == 64))
		if !local {
			r = r.WithContext(context.WithValue(r.Context(), internetKey{}, true))
		}
		h.ServeHTTP(w, r)
	})
}

// relayHandler gives relayed requests the viewer's real address (the
// cloud is trusted to set it) and marks them as remote.
func relayHandler(h http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if ip := net.ParseIP(strings.TrimSpace(strings.Split(r.Header.Get("X-Forwarded-For"), ",")[0])); ip != nil {
			r.RemoteAddr = net.JoinHostPort(ip.String(), "0")
		}
		ctx := context.WithValue(r.Context(), viaRelayKey{}, true)
		r = r.WithContext(context.WithValue(ctx, internetKey{}, true))
		h.ServeHTTP(w, r)
	})
}
