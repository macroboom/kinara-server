package remote

import (
	"bufio"
	"context"
	"errors"
	"log/slog"
	"net"
	"net/http"
	"sync"
	"time"
)

// headerTimeout bounds how long a new connection may take to send its
// PROXY header. The sidecar sends it at once.
const headerTimeout = 5 * time.Second

// Listener wraps the loopback tunnel listener: every connection must start
// with kinara-iroh's PROXY v2 header (and secret); anything else is dropped.
// Headers are read off the accept loop, so a stray connection that never
// speaks can't hold up the others.
type Listener struct {
	net.Listener
	Secret string
	Log    *slog.Logger

	once  sync.Once
	ready chan net.Conn
	done  chan struct{}
	err   error
}

// Conn is a tunnel connection with its device.
type Conn struct {
	net.Conn
	r    *bufio.Reader
	Peer *Peer
}

// Read goes through the reader that already consumed the header.
func (c *Conn) Read(b []byte) (int, error) { return c.r.Read(b) }

// RemoteAddr is "[<nodeId>]:0", so code keyed on the client's address (the
// sign-in limiter, request logs) sees the device rather than 127.0.0.1.
func (c *Conn) RemoteAddr() net.Addr { return peerAddr(c.Peer.NodeID) }

type peerAddr string

func (a peerAddr) Network() string { return "iroh" }
func (a peerAddr) String() string  { return "[" + string(a) + "]:0" }

func (l *Listener) start() {
	l.once.Do(func() {
		l.ready = make(chan net.Conn)
		l.done = make(chan struct{})
		go l.acceptLoop()
	})
}

func (l *Listener) acceptLoop() {
	defer close(l.done)
	for {
		c, err := l.Listener.Accept()
		if err != nil {
			var ne net.Error
			if errors.As(err, &ne) && ne.Timeout() {
				time.Sleep(50 * time.Millisecond)
				continue
			}
			l.err = err
			return
		}
		go l.handshake(c)
	}
}

func (l *Listener) handshake(c net.Conn) {
	_ = c.SetReadDeadline(time.Now().Add(headerTimeout))
	r := bufio.NewReader(c)
	p, err := ReadProxyV2(r, l.Secret)
	if err != nil {
		if l.Log != nil {
			l.Log.Warn("tunnel: dropping a connection without a valid header", "err", err)
		}
		c.Close()
		return
	}
	_ = c.SetReadDeadline(time.Time{})
	select {
	case l.ready <- &Conn{Conn: c, r: r, Peer: p}:
	case <-l.done:
		c.Close()
	}
}

// Accept returns the next connection whose header checked out.
func (l *Listener) Accept() (net.Conn, error) {
	l.start()
	select {
	case c := <-l.ready:
		return c, nil
	case <-l.done:
		if l.err != nil {
			return nil, l.err
		}
		return nil, net.ErrClosed
	}
}

// Serve runs handler on the tunnel listener at addr (loopback only: anyone
// who can reach it could claim to be any device, which the secret guards
// against) until ctx ends.
func Serve(ctx context.Context, addr, secret string, handler http.Handler, log *slog.Logger) error {
	ln, err := Listen(addr)
	if err != nil {
		return err
	}
	return ServeOn(ctx, ln, secret, handler, log)
}

// Listen opens the tunnel listener (loopback only). Open it before starting
// the sidecar: if the port is taken, whatever holds it must not receive the
// tunnel's connections (and the secret in their PROXY header).
func Listen(addr string) (net.Listener, error) {
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		return nil, err
	}
	if ip := net.ParseIP(host); ip == nil || !ip.IsLoopback() {
		return nil, errors.New("the tunnel listener must be on a loopback address, such as 127.0.0.1:8421")
	}
	return net.Listen("tcp", addr)
}

// ServeOn runs handler on a listener from Listen until ctx ends.
func ServeOn(ctx context.Context, ln net.Listener, secret string, handler http.Handler, log *slog.Logger) error {
	srv := &http.Server{
		Handler:           handler,
		ReadHeaderTimeout: 30 * time.Second,
		IdleTimeout:       2 * time.Minute,
		// No write timeout: streams run for hours. The tunnel's own idle
		// timeout ends connections to devices that went away.
		ConnContext: func(ctx context.Context, c net.Conn) context.Context {
			if tc, ok := c.(*Conn); ok {
				return WithPeer(ctx, tc.Peer)
			}
			return ctx
		},
	}
	go func() {
		<-ctx.Done()
		sctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = srv.Shutdown(sctx)
	}()
	err := srv.Serve(&Listener{Listener: ln, Secret: secret, Log: log})
	if errors.Is(err, http.ErrServerClosed) {
		return nil
	}
	return err
}
