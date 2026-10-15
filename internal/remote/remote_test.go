package remote

import (
	"bufio"
	"bytes"
	"context"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"strings"
	"testing"
	"time"
)

const testNode = "ae58ff8833241ac82d6ff7611046ed67b5072d142c588d0063e942d9a75502b6"

// header builds a PROXY v2 header the way kinora-iroh's encoder does.
func header(family byte, addr []byte, tlvs ...[]byte) []byte {
	body := append([]byte(nil), addr...)
	for _, t := range tlvs {
		body = append(body, t...)
	}
	out := append([]byte(nil), proxySig...)
	out = append(out, 0x21, family, byte(len(body)>>8), byte(len(body)))
	return append(out, body...)
}

func tlv(kind byte, v string) []byte {
	b := []byte{kind, 0, 0}
	binary.BigEndian.PutUint16(b[1:], uint16(len(v)))
	return append(b, v...)
}

// The relayed example from the sidecar README, byte for byte.
const readmeRelay = "0d0a0d0a000d0a515549540a2100004b" +
	"e0004061653538666638383333323431616338326436666637363131303436656436376235303732643134326335383864303036336539343264396137353530326236" +
	"e1000572656c6179"

func TestReadProxyV2(t *testing.T) {
	raw, _ := hex.DecodeString(readmeRelay)
	p, err := ReadProxyV2(bufio.NewReader(bytes.NewReader(append(raw, "GET / HTTP/1.1\r\n"...))), "")
	if err != nil || p.NodeID != testNode || p.Path != "relay" || p.Addr != nil {
		t.Fatalf("relay: %+v %v", p, err)
	}

	v4 := []byte{203, 0, 113, 7, 0, 0, 0, 0, 0xca, 0x6c, 0x1e, 0x61}
	h := header(0x11, v4, tlv(tlvNodeID, testNode), tlv(tlvPath, "direct-v4"), tlv(tlvSecret, "s3cret"))
	p, err = ReadProxyV2(bufio.NewReader(bytes.NewReader(h)), "s3cret")
	if err != nil || p.Addr.String() != "203.0.113.7:51820" || p.Path != "direct-v4" {
		t.Fatalf("v4: %+v %v", p, err)
	}
	if _, err := ReadProxyV2(bufio.NewReader(bytes.NewReader(h)), "other"); err != errBadSecret {
		t.Fatalf("wrong secret accepted: %v", err)
	}
	if _, err := ReadProxyV2(bufio.NewReader(bytes.NewReader(raw)), "s3cret"); err != errBadSecret {
		t.Fatalf("missing secret accepted: %v", err)
	}

	bad := [][]byte{
		[]byte("GET / HTTP/1.1\r\nHost: x\r\n\r\n"),
		header(0x11, v4[:4], tlv(tlvNodeID, testNode)),               // short address block... then TLV parse
		header(0x00, nil, tlv(tlvNodeID, strings.ToUpper(testNode))), // node ID must be lowercase hex
		header(0x00, nil, []byte{0xE0, 0x00, 0x40, 'a'}),             // TLV longer than the body
		header(0x31, nil, tlv(tlvNodeID, testNode)),                  // unix sockets
	}
	for i, b := range bad {
		if _, err := ReadProxyV2(bufio.NewReader(bytes.NewReader(b)), ""); err == nil {
			t.Errorf("bad header %d accepted", i)
		}
	}
}

func TestServeTunnel(t *testing.T) {
	ln, _ := net.Listen("tcp", "127.0.0.1:0")
	addr := ln.Addr().String()
	ln.Close()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	h := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		p := PeerFrom(r.Context())
		if p == nil {
			http.Error(w, "no peer", 500)
			return
		}
		fmt.Fprintf(w, "%s %s %s", p.NodeID[:8], p.Path, r.RemoteAddr)
	})
	go Serve(ctx, addr, "s3cret", h, log)
	var c net.Conn
	var err error
	for i := 0; i < 50; i++ {
		if c, err = net.Dial("tcp", addr); err == nil {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if err != nil {
		t.Fatal(err)
	}
	// A stray connection that never speaks must not hold up the next one.
	stray, _ := net.Dial("tcp", addr)
	defer stray.Close()

	c.Write(header(0x00, nil, tlv(tlvNodeID, testNode), tlv(tlvPath, "relay"), tlv(tlvSecret, "s3cret")))
	c.Write([]byte("GET / HTTP/1.1\r\nHost: x\r\nConnection: close\r\n\r\n"))
	c.SetReadDeadline(time.Now().Add(3 * time.Second))
	resp, err := http.ReadResponse(bufio.NewReader(c), nil)
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	if want := "ae58ff88 relay [" + testNode + "]:0"; string(body) != want {
		t.Fatalf("got %q, want %q", body, want)
	}

	// Without the secret the connection is dropped.
	c2, _ := net.Dial("tcp", addr)
	c2.Write(header(0x00, nil, tlv(tlvNodeID, testNode)))
	c2.Write([]byte("GET / HTTP/1.1\r\nHost: x\r\n\r\n"))
	c2.SetReadDeadline(time.Now().Add(3 * time.Second))
	if n, _ := c2.Read(make([]byte, 10)); n != 0 {
		t.Fatal("a connection without the secret got an answer")
	}

	if err := Serve(ctx, "0.0.0.0:0", "", h, log); err == nil {
		t.Fatal("non-loopback tunnel listener allowed")
	}
}

// --- A stand-in sidecar: this test binary run with KINORA_FAKE_IROH set. ---

func TestMain(m *testing.M) {
	switch os.Getenv("KINORA_FAKE_IROH") {
	case "":
		os.Exit(m.Run())
	case "bad-config":
		fmt.Println(`{"type":"error","v":1,"ts":0,"code":"config","message":"invalid node ID in --block"}`)
		os.Exit(2)
	default:
		fakeIroh()
	}
}

func fakeIroh() {
	args := strings.Join(os.Args[1:], " ")
	blocked := []string{}
	for i, a := range os.Args {
		if a == "--block" && i+1 < len(os.Args) {
			blocked = strings.Split(os.Args[i+1], ",")
		}
	}
	emit := func(v any) {
		b, _ := json.Marshal(v)
		fmt.Println(string(b))
	}
	fmt.Fprintln(os.Stderr, "fake kinora-iroh starting: "+args)
	emit(map[string]any{"type": "ready", "v": 1, "nodeId": testNode, "ticket": "endpointfake"})
	status := func() {
		emit(map[string]any{"type": "status", "v": 1, "nodeId": testNode, "ticket": "endpointfake",
			"relay": map[string]any{"mode": "default", "connected": true},
			"addrs": []any{}, "warnings": []any{}, "blocked": blocked, "activeStreams": 0, "peers": []any{},
			"secret": os.Getenv("KINORA_IROH_SECRET"), "args": args})
	}
	status()
	sc := bufio.NewScanner(os.Stdin)
	for sc.Scan() {
		var cmd map[string]string
		_ = json.Unmarshal(sc.Bytes(), &cmd)
		switch cmd["cmd"] {
		case "block":
			blocked = append(blocked, cmd["nodeId"])
		case "unblock":
			out := blocked[:0]
			for _, b := range blocked {
				if b != cmd["nodeId"] {
					out = append(out, b)
				}
			}
			blocked = out
		}
		status()
	}
	os.Exit(0) // stdin closed: stop
}

func waitFor(t *testing.T, what string, ok func() bool) {
	t.Helper()
	for i := 0; i < 200; i++ {
		if ok() {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

func TestSidecarLifecycle(t *testing.T) {
	exe, _ := os.Executable()
	t.Setenv("KINORA_FAKE_IROH", "1")
	other := strings.Repeat("b", 64)
	s := &Sidecar{
		Bin: exe, DataDir: t.TempDir(), Target: "127.0.0.1:8421", Secret: "s3cret",
		Blocked: func(context.Context) ([]string, error) { return []string{other}, nil },
		Log:     slog.New(slog.NewTextHandler(io.Discard, nil)),
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan struct{})
	go func() { s.Run(ctx); close(done) }()

	if st := s.Info().State; st != StateDisabled {
		t.Fatalf("state %q before turning on", st)
	}
	s.Configure(true, 7777)
	waitFor(t, "ready", func() bool { in := s.Info(); return in.State == StateReady && in.Status != nil })
	in := s.Info()
	if in.NodeID != testNode || in.Ticket != "endpointfake" || len(in.Status.Blocked) != 1 || in.Status.Blocked[0] != other {
		t.Fatalf("info %+v", in)
	}

	s.Block(testNode)
	waitFor(t, "block applied", func() bool { st := s.Info().Status; return st != nil && len(st.Blocked) == 2 })
	s.Unblock(testNode)
	waitFor(t, "unblock applied", func() bool { st := s.Info().Status; return st != nil && len(st.Blocked) == 1 })

	// Turning it off stops the process (stdin closed) without a restart.
	s.Configure(false, 7777)
	waitFor(t, "disabled", func() bool { return s.Info().State == StateDisabled })
	time.Sleep(200 * time.Millisecond)
	if in := s.Info(); in.State != StateDisabled || in.Restarts != 0 {
		t.Fatalf("after off: %+v", in)
	}

	cancel()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Run didn't return")
	}
}

func TestSidecarBadConfigStops(t *testing.T) {
	exe, _ := os.Executable()
	t.Setenv("KINORA_FAKE_IROH", "bad-config")
	s := &Sidecar{Bin: exe, DataDir: t.TempDir(), Target: "127.0.0.1:8421", Log: slog.New(slog.NewTextHandler(io.Discard, nil))}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go s.Run(ctx)
	s.Configure(true, 0)
	waitFor(t, "error", func() bool { return s.Info().State == StateError })
	in := s.Info()
	if !strings.Contains(in.Error, "invalid node ID") {
		t.Fatalf("error %q", in.Error)
	}
	time.Sleep(1500 * time.Millisecond)
	if in := s.Info(); in.Restarts != 0 || in.State != StateError {
		t.Fatalf("restarted after exit code 2: %+v", in)
	}
}

func TestSidecarWaitsForAdmin(t *testing.T) {
	s := &Sidecar{Bin: "/nonexistent", Log: slog.New(slog.NewTextHandler(io.Discard, nil)),
		Allowed: func(context.Context) bool { return false }}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go s.Run(ctx)
	s.Configure(true, 0)
	waitFor(t, "waiting", func() bool { return s.Info().State == StateWaiting })
}
