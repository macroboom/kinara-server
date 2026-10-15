# Kinora remote access sidecar (iroh)

`kinora-iroh` lets the Kinora apps reach a home server from anywhere without
port forwarding. It runs next to `kinora-server`, which starts and supervises
it. The same Rust code is the client side the apps will embed later.

```
 phone / TV app                              home PC
 ExoPlayer, AVPlayer, fetch                  kinora-server (Go)
   |  http://127.0.0.1:<port>                  ^  127.0.0.1:8421, PROXY v2 + HTTP/1.1
   v                                           |
 kinora-tunnel client  ==== iroh QUIC ====  kinora-iroh
 (one stream per TCP connection; direct over IPv6 or IPv4, relay as last resort)
```

The tunnel only moves bytes. HTTP auth (Bearer header, cookie, `?token=`)
stays the security boundary for all content.

## Versions

| Crate | Version | Notes |
|---|---|---|
| `iroh` | `=1.3.0` (2026-09-28) | features `unstable-net-report` for the net report in status |
| `iroh-tickets` | `=1.0.0` | `EndpointTicket` for pairing |
| `iroh-mdns-address-lookup` | `=0.6.0` | LAN discovery |

All three are pinned exactly. `unstable-net-report` is outside iroh's semver
promise, so treat any iroh bump as a code change: rebuild, run the tests,
read the iroh changelog.

**Build status.** This workspace was written in an environment without
crates.io access, against the iroh 1.3.0 API on docs.rs. The dependency-free
parts (PROXY v2 encoder and parser, address scopes, rate limiter, size
parsing) were compiled and unit tested there; the Go PROXY snippet below was
compiled and tested against bytes produced by the Rust encoder. Everything
that touches iroh, tokio, hyper or clap has not been compiled yet. Expect a
few compile fixes on the first `cargo build`; see "First build" below.

## Layout

```
sidecar/
  Cargo.toml                    workspace, pinned versions, release profile
  rustfmt.toml
  .cargo/config.toml            static MSVC runtime for kinora-iroh.exe
  crates/kinora-tunnel/         library: protocol, both ends, config
    src/lib.rs                  ALPN, close codes
    src/server.rs               accept loop, per-peer accounting, block, status
    src/client.rs               local listener, lazy connect, reconnect, status
    src/pump.rs                 TCP <-> QUIC stream copy with half-close
    src/proxy_v2.rs             PROXY v2 encoder and parser
    src/path.rs                 direct-v4 / direct-v6 / relay / mixed
    src/transport.rs            endpoint builder, QUIC windows and timeouts
    src/events.rs               JSON Lines types (stdout and stdin)
    src/config.rs               clap flags and env vars for both binaries
    src/keys.rs                 persistent secret key
    src/addr.rs, ratelimit.rs, units.rs, hexutil.rs
  crates/kinora-iroh/           binary: the sidecar
  crates/kinora-connect/        binary: test client CLI over the library API
  crates/kinora-testserver/     binary: stand-in for the Go 8421 listener
  crates/kinora-e2e/            binary: runs the end-to-end test list
  crates/kinora-tunnel-ffi/     library for the apps: C ABI (iOS) and JNI (Android)
    include/kinora_tunnel.h     the C interface
```

## Protocol

* **ALPN** `kinora/tunnel/1`. The endpoint only offers this ALPN, and the
  accept path checks it again.
* **One iroh connection per device**, one QUIC bidirectional stream per TCP
  connection the app opens. Bytes are copied raw; the sidecar never parses
  HTTP.
* **Half-close.** EOF on a TCP read becomes `finish()` on that QUIC send
  stream, and the end of a QUIC receive stream becomes a TCP write shutdown.
  The other direction keeps flowing.
* **Backpressure.** Each direction is a loop over a 64 KiB buffer whose
  writes wait for QUIC flow control or the TCP socket. A slow reader at either
  end slows the writer at the other end; nothing buffers a file.
* **Errors.** If a copy fails partway, the QUIC stream is reset (code 1) so
  the far side tears down its TCP connection too.
* **Transport tuning** (both ends, all configurable): idle timeout 60 s,
  keepalive 15 s, stream window 4 MiB, connection window 16 MiB, send window
  16 MiB. 150 Mbps at 100 ms RTT is about 1.9 MB in flight, so a single stream
  has headroom and several parallel range requests share the connection window.
  iroh's own per-path keepalive (5 s) and multipath settings are left as iroh
  sets them.
* **Limits.** The server allows 64 concurrent streams per device (enforced by
  QUIC stream credit, so the app's 65th connection waits instead of failing)
  and 30 new connections per device per minute (checked in the handshake hook).
  A new connection from a device that is already connected replaces the old
  one.
* **When the tunnel cannot carry a request** the client answers the local TCP
  connection with `HTTP/1.1 502 Bad Gateway` and a JSON body
  `{"error":"tunnel_unavailable","message":...}` plus the header
  `X-Kinora-Tunnel: error`, instead of hanging. The sidecar does the same when
  it cannot reach the Go listener. Apps can tell a tunnel 502 from a server
  502 by that header.

## Contract with kinora-server

### 1. Launch and supervision

```
kinora-iroh --data-dir <KINORA_DATA> --target 127.0.0.1:8421 [--udp-port N] [--block <nodeId>,...]
```

* Pipe stdin, stdout and stderr. stdout is JSON Lines only; stderr is logs.
* **Closing stdin stops the sidecar** (within about 1.5 s: up to 1 s for open
  streams, then a bounded close). If kinora-server dies, its end of the pipe
  closes and the sidecar exits the same way, so it is never orphaned, also on
  Windows. Ctrl-C, SIGTERM, Ctrl-Break and console close also stop it.
* Restart it with backoff when it exits (for example 1 s doubling to 30 s,
  reset after a minute of uptime). Exit code 2 means a bad flag or config,
  which a restart will not fix: show the `error` line and stop retrying.
* Pass blocked devices on the command line (`--block a,b,c`) or in
  `KINORA_IROH_BLOCK`, so a restart never has an unblocked window.
* Read stdout with a large line buffer (a status line with many peers can be
  tens of KB; `bufio.Scanner` needs `scanner.Buffer(make([]byte, 0, 64<<10), 1<<20)`).

### 2. stdout events

Every line is one JSON object with `"type"` and `"v": 1`. Times (`ts`,
`connectedSince`) are Unix milliseconds. Field names are camelCase. New
fields may appear without a version bump; renames or removals bump `v`. The
Rust types in `crates/kinora-tunnel/src/events.rs` are the schema.

**ready**, once the endpoint is up and a relay is connected (or after 10 s
without one, or at once when relays are disabled):

```json
{"type":"ready","v":1,"ts":1760480000000,
 "nodeId":"ae58ff8833241ac82d6ff7611046ed67b5072d142c588d0063e942d9a75502b6",
 "ticket":"endpoint...",
 "relay":"https://use1-1.relay.n0.iroh.link./",
 "addrs":[{"addr":"192.168.1.20:52114","family":"ipv4","scope":"lan"},
          {"addr":"[2601:1c2:4000::17]:52115","family":"ipv6","scope":"global"}]}
```

**status**, every `--status-interval-secs` (default 5), right after `ready`,
after any `block`/`unblock`/`status` command, and within about a second of a
change in addresses, relay, UDP reachability, warnings, the blocked list, or
any peer's path:

```json
{"type":"status","v":1,"ts":1760480005000,
 "nodeId":"ae58...02b6","ticket":"endpoint...","uptimeSecs":5,
 "relay":{"mode":"default","url":"https://use1-1.relay.n0.iroh.link./","connected":true,
          "latencyMs":18.4,"lastError":null},
 "addrs":[{"addr":"192.168.1.20:52114","family":"ipv4","scope":"lan"}],
 "netReport":{"udp":true,"udpV4":true,"udpV6":true,
              "globalV4":"203.0.113.7:52114","globalV6":"[2601:1c2:4000::17]:52115",
              "mappingVariesByDest":false,"captivePortal":null,
              "preferredRelay":"https://use1-1.relay.n0.iroh.link./"},
 "warnings":[],
 "blocked":[],
 "activeStreams":3,
 "peers":[{"nodeId":"5c1e...9a0f","path":"direct-v6","remoteAddr":"[2607:fb90::1]:41000",
           "relayUrl":"https://use1-1.relay.n0.iroh.link./","rttMs":42.1,
           "bytesIn":18233,"bytesOut":734003200,"openStreams":3,"totalStreams":57,
           "connectedSince":1760479990000}]}
```

* `addrs` scope is `global`, `lan`, `link-local` or `loopback`.
* `netReport` is `null` until iroh's first network check finishes.
* `peers[].path` is `direct-v4`, `direct-v6`, `relay`, `mixed` (several paths
  in use, or iroh is switching) or `none`. `bytesIn` is received from the
  device, `bytesOut` sent to it.
* `ticket` can change when addresses or the home relay change; always show
  the latest one.

**peer_connected / peer_disconnected**

```json
{"type":"peer_connected","v":1,"ts":1760479990000,"nodeId":"5c1e...9a0f","path":"relay","remoteAddr":null,"reason":null}
{"type":"peer_disconnected","v":1,"ts":1760480100000,"nodeId":"5c1e...9a0f","path":"none","remoteAddr":null,"reason":"blocked"}
```

The path in `peer_connected` is often `relay`: iroh starts on the relay and
moves to a direct path a moment later, which shows up in the next status line.
A device that reconnects (for example after Wi-Fi to cellular) does not
produce a disconnect for its old connection.

**error**, for the manager UI:

```json
{"type":"error","v":1,"ts":1760480000000,"code":"udp_blocked","message":"UDP blocked, relay only. Remote playback will go through the relay."}
```

| code | when |
|---|---|
| `udp_blocked` | the net report found no working UDP; every device will use the relay |
| `symmetric_nat` | symmetric NAT and no global IPv6, so hole punching will mostly fail |
| `relay_unreachable` | relays are enabled but none connected 20 s after start |
| `mdns_unavailable` | LAN discovery could not start (multicast blocked) |
| `target_unreachable` | the Go 8421 listener refused or timed out; devices get 502s |
| `bad_command` | a stdin line could not be parsed or had a bad node ID |
| `config` | invalid configuration, exit code 2 follows |
| `bind_failed` | could not create the endpoint (for example the UDP port is taken), exit code 1 follows |

Condition codes (`udp_blocked`, `symmetric_nat`, `relay_unreachable`,
`mdns_unavailable`, `target_unreachable`) are emitted once when they appear
and stay listed in `status.warnings` while true, so the UI can show and clear
them from status alone.

### 3. stdin commands

One JSON object per line:

```json
{"cmd":"block","nodeId":"5c1e...9a0f"}
{"cmd":"unblock","nodeId":"5c1e...9a0f"}
{"cmd":"status"}
{"cmd":"shutdown"}
```

`block` closes the device's connection at once (close code 2), and the
handshake hook refuses it from then on; a fresh `status` line follows. Node IDs
are 64 hex characters. The block list lives in memory only, so kinora-server
owns the persistent list and passes it with `--block` on every launch.

QUIC close codes the device may see: 0 normal, 1 server shutting down,
2 blocked, 3 superseded by a newer connection, 4 too many connections, 5
wrong ALPN.

### 4. PROXY protocol v2 on 127.0.0.1:8421

Every TCP connection the sidecar opens to `--target` starts with a PROXY v2
header, then the app's raw HTTP/1.1 bytes.

```
offset  size  field
0       12    signature 0D 0A 0D 0A 00 0D 0A 51 55 49 54 0A
12      1     0x21  version 2, command PROXY
13      1     family: 0x11 TCP/IPv4, 0x21 TCP/IPv6, 0x00 AF_UNSPEC (relayed)
14      2     length of everything after byte 16, big endian
16      12|36|0  address block (absent for AF_UNSPEC)
              IPv4: src addr(4) dst addr(4) src port(2) dst port(2)
              IPv6: src addr(16) dst addr(16) src port(2) dst port(2)
...     TLVs: type(1) length(2, big endian) value
              0xE0  device node ID, 64 lowercase hex ASCII
              0xE1  path type ASCII: direct-v4 | direct-v6 | relay | mixed
              0xE2  shared secret (only when KINORA_IROH_SECRET is set)
```

* **src** is the device's current UDP address on the direct path (IPv4-mapped
  IPv6 is folded to IPv4). On a relayed connection there is no address and the
  family is AF_UNSPEC.
* **dst** is the sidecar's bound UDP socket of the same family (address
  `0.0.0.0` / `::`, real port). Go can ignore it.
* The path is sampled when each TCP connection opens, so it reflects that
  request.

Example: a device on a direct IPv4 path from 203.0.113.7:51820, sidecar UDP
port 7777 (107 bytes):

```
0000  0d 0a 0d 0a 00 0d 0a 51 55 49 54 0a 21 11 00 5b  .......QUIT.!..[
0010  cb 00 71 07 00 00 00 00 ca 6c 1e 61 e0 00 40 61  ..q......l.a..@a
0020  65 35 38 66 66 38 38 33 33 32 34 31 61 63 38 32  e58ff8833241ac82
0030  64 36 66 66 37 36 31 31 30 34 36 65 64 36 37 62  d6ff7611046ed67b
0040  35 30 37 32 64 31 34 32 63 35 38 38 64 30 30 36  5072d142c588d006
0050  33 65 39 34 32 64 39 61 37 35 35 30 32 62 36 e1  3e942d9a75502b6.
0060  00 09 64 69 72 65 63 74 2d 76 34                 ..direct-v4
```

`21` PROXY, `11` TCP/IPv4, `00 5b` = 91 bytes follow; `cb 00 71 07` source
203.0.113.7, `00 00 00 00` destination 0.0.0.0, `ca 6c` source port 51820,
`1e 61` destination port 7777; TLV `e0 00 40` (64 bytes of node ID), TLV
`e1 00 09` "direct-v4".

The same device on a direct IPv6 path from [2001:db8:1234::42]:40000 (131 bytes):

```
0000  0d 0a 0d 0a 00 0d 0a 51 55 49 54 0a 21 21 00 73  .......QUIT.!!.s
0010  20 01 0d b8 12 34 00 00 00 00 00 00 00 00 00 42   ....4.........B
0020  00 00 00 00 00 00 00 00 00 00 00 00 00 00 00 00  ................
0030  9c 40 1e 61 e0 00 40 61 65 35 38 66 66 38 38 33  .@.a..@ae58ff883
0040  33 32 34 31 61 63 38 32 64 36 66 66 37 36 31 31  3241ac82d6ff7611
0050  30 34 36 65 64 36 37 62 35 30 37 32 64 31 34 32  046ed67b5072d142
0060  63 35 38 38 64 30 30 36 33 65 39 34 32 64 39 61  c588d0063e942d9a
0070  37 35 35 30 32 62 36 e1 00 09 64 69 72 65 63 74  75502b6...direct
0080  2d 76 36                                         -v6
```

The same device through the relay (91 bytes):

```
0000  0d 0a 0d 0a 00 0d 0a 51 55 49 54 0a 21 00 00 4b  .......QUIT.!..K
0010  e0 00 40 61 65 35 38 66 66 38 38 33 33 32 34 31  ..@ae58ff8833241
0020  61 63 38 32 64 36 66 66 37 36 31 31 30 34 36 65  ac82d6ff7611046e
0030  64 36 37 62 35 30 37 32 64 31 34 32 63 35 38 38  d67b5072d142c588
0040  64 30 30 36 33 65 39 34 32 64 39 61 37 35 35 30  d0063e942d9a7550
0050  32 62 36 e1 00 05 72 65 6c 61 79                 2b6...relay
```

These dumps were produced by the Rust encoder in `proxy_v2.rs`, and the Go
code below was tested against exactly these bytes.

#### Go: parse the header (stdlib only)

```go
package tunnel

import (
	"bufio"
	"encoding/binary"
	"errors"
	"io"
	"net"
)

var proxySig = []byte("\r\n\r\n\x00\r\nQUIT\n")

// TunnelPeer is who a tunnel connection belongs to, from kinora-iroh's PROXY v2 header.
type TunnelPeer struct {
	NodeID string       // iroh node ID, 64 lowercase hex chars (TLV 0xE0)
	Path   string       // direct-v4, direct-v6, relay or mixed (TLV 0xE1)
	Addr   *net.UDPAddr // device's UDP address on a direct path, nil when relayed
}

// ReadProxyV2 reads the header kinora-iroh writes first on every connection to 8421.
func ReadProxyV2(r *bufio.Reader) (*TunnelPeer, error) {
	hdr := make([]byte, 16)
	if _, err := io.ReadFull(r, hdr); err != nil {
		return nil, err
	}
	if string(hdr[:12]) != string(proxySig) || hdr[12] != 0x21 {
		return nil, errors.New("proxy: not a PROXY v2 header")
	}
	body := make([]byte, binary.BigEndian.Uint16(hdr[14:16]))
	if _, err := io.ReadFull(r, body); err != nil {
		return nil, err
	}
	addrLen := map[byte]int{0x00: 0, 0x11: 12, 0x21: 36}
	n, ok := addrLen[hdr[13]]
	if !ok || len(body) < n {
		return nil, errors.New("proxy: unsupported family or short address block")
	}
	p := &TunnelPeer{}
	switch hdr[13] {
	case 0x11: // TCP over IPv4: src(4) dst(4) srcport(2) dstport(2)
		p.Addr = &net.UDPAddr{IP: net.IP(append([]byte(nil), body[0:4]...)), Port: int(binary.BigEndian.Uint16(body[8:10]))}
	case 0x21: // TCP over IPv6: src(16) dst(16) srcport(2) dstport(2)
		p.Addr = &net.UDPAddr{IP: net.IP(append([]byte(nil), body[0:16]...)), Port: int(binary.BigEndian.Uint16(body[32:34]))}
	}
	for t := body[n:]; len(t) > 0; {
		if len(t) < 3 || len(t) < 3+int(binary.BigEndian.Uint16(t[1:3])) {
			return nil, errors.New("proxy: malformed TLV")
		}
		l := int(binary.BigEndian.Uint16(t[1:3]))
		switch t[0] {
		case 0xE0:
			p.NodeID = string(t[3 : 3+l])
		case 0xE1:
			p.Path = string(t[3 : 3+l])
		}
		t = t[3+l:]
	}
	if len(p.NodeID) != 64 {
		return nil, errors.New("proxy: missing or bad node ID")
	}
	return p, nil
}
```

#### Go: the 8421 listener

This wraps a normal `net.Listener` so the existing `http.Handler` serves the
tunnel unchanged. Tested with `net/http` against the bytes above:
`r.RemoteAddr` becomes `[<nodeId>]:0`, so code that runs
`net.SplitHostPort(r.RemoteAddr)` (such as the sign-in rate limiter) keys on
the device, and `PeerFrom(r)` gives the full details.

```go
package tunnel

import (
	"bufio"
	"context"
	"log"
	"net"
	"net/http"
	"time"
)

// tunnelListener wraps the loopback 8421 listener. Every connection must
// start with kinora-iroh's PROXY v2 header; anything else is dropped.
type tunnelListener struct{ net.Listener }

type tunnelConn struct {
	net.Conn
	r    *bufio.Reader
	Peer *TunnelPeer
}

// Read goes through the bufio.Reader that already consumed the header.
func (c *tunnelConn) Read(b []byte) (int, error) { return c.r.Read(b) }

// RemoteAddr makes r.RemoteAddr "[<nodeId>]:0", so code that does
// net.SplitHostPort(r.RemoteAddr) keys on the device instead of 127.0.0.1.
func (c *tunnelConn) RemoteAddr() net.Addr { return peerAddr(c.Peer.NodeID) }

type peerAddr string

func (a peerAddr) Network() string { return "iroh" }
func (a peerAddr) String() string  { return "[" + string(a) + "]:0" }

func (l tunnelListener) Accept() (net.Conn, error) {
	for {
		c, err := l.Listener.Accept()
		if err != nil {
			return nil, err
		}
		// The sidecar writes the header immediately; the deadline only guards against strays.
		_ = c.SetReadDeadline(time.Now().Add(5 * time.Second))
		r := bufio.NewReader(c)
		p, err := ReadProxyV2(r)
		if err != nil {
			log.Printf("tunnel: dropping connection without a valid PROXY header: %v", err)
			c.Close()
			continue
		}
		_ = c.SetReadDeadline(time.Time{})
		return &tunnelConn{Conn: c, r: r, Peer: p}, nil
	}
}

type peerKey struct{}

// PeerFrom returns the remote device for a request that came through the tunnel, or nil for LAN requests.
func PeerFrom(r *http.Request) *TunnelPeer {
	p, _ := r.Context().Value(peerKey{}).(*TunnelPeer)
	return p
}

// ServeTunnel runs the same handler on 127.0.0.1:8421 for kinora-iroh. Bind loopback only:
// anyone who can reach this port can claim any node ID.
func ServeTunnel(handler http.Handler) error {
	ln, err := net.Listen("tcp", "127.0.0.1:8421")
	if err != nil {
		return err
	}
	srv := &http.Server{
		Handler:           handler,
		ReadHeaderTimeout: 30 * time.Second,
		ConnContext: func(ctx context.Context, c net.Conn) context.Context {
			if tc, ok := c.(*tunnelConn); ok {
				return context.WithValue(ctx, peerKey{}, tc.Peer)
			}
			return ctx
		},
	}
	return srv.Serve(tunnelListener{ln})
}
```

Two Go `net/http` behaviours to keep in mind for tunnel traffic:

* Do not set a `WriteTimeout` on the 8421 server: piped transcodes run for
  hours. The tunnel's own keepalive and idle timeout handle dead devices; when
  a device goes away its stream is reset and Go sees a write error.
* If a client half-closes after sending a request, Go's background read sees
  EOF and cancels `r.Context()`. Native players do not do this, but if a
  transcode handler ties ffmpeg's lifetime to `r.Context()`, a half-closing
  client would stop it early. Tying it to write errors avoids that.

## Go-side changes (checklist)

**Status: done in kinora-server 0032** (`internal/remote`, `internal/qr`,
`internal/api/remote.go`, migration `0007_remote.sql`, Manage → Remote access
in the web app). On top of the list below it also: never starts the sidecar
before an admin account exists and refuses the first-run setup page over the
tunnel; keeps the account list and sign-up home-network only; counts failed
remote sign-ins from all devices together (30 per 10 minutes); and checks
the 0xE2 secret on every connection. The snippets above are the reference
the Go code follows.

1. **Spawn and supervise.** At startup, if remote access is enabled, start
   `kinora-iroh` (next to the server binary: `kinora-iroh.exe` on Windows)
   with `--data-dir <KINORA_DATA> --target 127.0.0.1:8421 --block <persisted list>`.
   Keep the stdin pipe open for the sidecar's lifetime and close it to stop.
   Read stdout line by line into a `remoteState` struct guarded by a mutex,
   forward stderr into the server log, restart with backoff. On server
   shutdown close stdin and wait up to 3 s, then kill.
2. **8421 PROXY listener.** Start `ServeTunnel(handler)` on 127.0.0.1:8421
   with the same handler as 8420, using the snippets above. Leave 8420
   untouched for LAN clients.
3. **Device identity.** In the sign-in rate limiter and request logs, use
   `PeerFrom(r).NodeID` when present (or rely on the `RemoteAddr` override).
   When a device signs in through the tunnel, store its node ID on the session
   or device record. The app keeps a stable key, so the ID stays the same.
4. **Block on revocation.** When a device is revoked or signed out, send
   `{"cmd":"block","nodeId":...}` for each node ID recorded for it, and add
   them to the persisted list passed with `--block`. Note that a blocked
   device cannot reach the server remotely to sign in again. If an ordinary
   sign-out should allow a later remote sign-in, block only on "revoke
   device", or unblock when that device next signs in on the LAN.
5. **`/api/v1/health` `remote` block.** Built from the last status line. Keep
   the ticket and peer details out of this public endpoint; they reveal
   addresses.
   ```json
   "remote": {"enabled":true,"state":"ready","nodeId":"ae58...02b6",
              "relay":{"url":"https://use1-1.relay.n0.iroh.link./","connected":true,"latencyMs":18.4},
              "udp":true,"globalIpv6":true,"peers":2,"warnings":["symmetric_nat"]}
   ```
   `state` is `disabled`, `starting` (launched, no `ready` yet), `ready`, or
   `error` (exited or restarting; keep the last error message).
6. **Manage page.** Show: state and node ID; the latest `ticket` as a QR code
   (and copyable text) for pairing a device away from home; relay URL,
   connection and latency; direct addresses with family and scope; UDP and
   NAT findings from `netReport`; active warnings with their messages; a peer
   table (device name from your session records, path, RTT, bytes in/out, open
   streams, connected since) with a block button.

## Configuration

Every flag has an environment variable. Sizes accept `K`, `M`, `G` (binary).

| Flag | Env | Default | Meaning |
|---|---|---|---|
| `--data-dir` | `KINORA_IROH_DATA_DIR` | required | key at `<dir>/iroh/secret.key` |
| `--target` | `KINORA_IROH_TARGET` | `127.0.0.1:8421` | Go PROXY listener |
| `--udp-port` | `KINORA_IROH_UDP_PORT` | iroh picks | fixed UDP port, IPv4 and IPv6 |
| `--relay-mode` | `KINORA_IROH_RELAY_MODE` | `default` | `default`, `disabled`, `custom` |
| `--relay-url` (repeat) | `KINORA_IROH_RELAY_URLS` (comma list) | | self-hosted `iroh-relay` URLs for `custom` |
| `--no-discovery-n0` | `KINORA_IROH_NO_DISCOVERY_N0` | off | no n0 DNS/pkarr publish or lookup |
| `--no-mdns` | `KINORA_IROH_NO_MDNS` | off | no LAN discovery |
| `--status-interval-secs` | `KINORA_IROH_STATUS_INTERVAL_SECS` | 5 | periodic status lines |
| `--max-streams-per-peer` | `KINORA_IROH_MAX_STREAMS_PER_PEER` | 64 | concurrent TCP connections per device |
| `--max-conns-per-minute` | `KINORA_IROH_MAX_CONNS_PER_MINUTE` | 30 | new connections per device per minute, 0 = off |
| `--max-handshakes-per-minute` | `KINORA_IROH_MAX_HANDSHAKES_PER_MINUTE` | 120 | new connections from all devices together per minute, 0 = off (a new node ID costs nothing, so the per-device limit alone can be dodged) |
| (env only) | `KINORA_IROH_SECRET` | | sent as TLV 0xE2 in every PROXY header; kinora-server sets a fresh random one each launch and drops connections without it, so other local programs can't pose as the sidecar |
| `--stream-window` | `KINORA_IROH_STREAM_WINDOW` | `4M` | per-stream receive window |
| `--connection-window` | `KINORA_IROH_CONNECTION_WINDOW` | `16M` | per-connection receive window |
| `--send-window` | `KINORA_IROH_SEND_WINDOW` | `16M` | per-connection send buffer |
| `--idle-timeout-secs` | `KINORA_IROH_IDLE_TIMEOUT_SECS` | 60 | |
| `--keepalive-secs` | `KINORA_IROH_KEEPALIVE_SECS` | 15 | must be below the idle timeout |
| `--block` (repeat) | `KINORA_IROH_BLOCK` (comma list) | | node IDs refused from the start |
| `--shutdown-grace-ms` | `KINORA_IROH_SHUTDOWN_GRACE_MS` | 1000 | wait for open streams on shutdown |
| `--log-level` | `KINORA_IROH_LOG` | info, iroh at warn | tracing filter for stderr |

Self-hosting a relay later: run `iroh-relay` on a VPS, then
`--relay-mode custom --relay-url https://relay.example.com`. The apps need
the same relay (the ticket carries it).

**Identity.** The key is 64 hex characters in `<data-dir>/iroh/secret.key`,
created on first run with mode 0600 in a 0700 folder on Unix. On Windows
inherited permissions are removed and only the current user gets access
(through `icacls`; if that fails it keeps the data folder's ACL and logs a
warning). An unreadable key is a startup error, never silently replaced,
because a new key changes the node ID and unpairs every device. Back the file
up with the rest of `KINORA_DATA`.

**Firewall and ports.** Without `--udp-port`, iroh picks a port each run and
relies on hole punching. A fixed port lets you add one firewall rule (and an
optional forward) and makes direct paths come back faster after a restart. On
Windows, allow the program on first run, or:

```powershell
New-NetFirewallRule -DisplayName "Kinora remote (UDP)" -Direction Inbound -Program "C:\Kinora\kinora-iroh.exe" -Protocol UDP -Action Allow
```

## Building

The workspace builds with stable Rust. Release binaries go to
`sidecar/target/<target>/release/`.

**Linux x86_64** (and the native build anywhere):

```sh
cd sidecar
cargo build --release --workspace
```

**Windows x86_64 (MSVC)**, on the home PC:

1. Install Visual Studio 2022 Build Tools with "Desktop development with C++"
   (linker and the C compiler `ring` needs), then rustup with the default
   `x86_64-pc-windows-msvc` toolchain.
2. In PowerShell:
   ```powershell
   cd sidecar
   cargo build --release --workspace
   cargo test --workspace
   .\target\release\kinora-e2e.exe
   ```
3. Ship `target\release\kinora-iroh.exe` next to `kinora-server.exe`.
   `.cargo/config.toml` links the C runtime statically, so no redistributable
   is needed.

**Linux aarch64** (NAS, Raspberry Pi), from an x86_64 machine:

```sh
cargo install cross
cross build --release --target aarch64-unknown-linux-gnu -p kinora-iroh
```

or with zig: `cargo install cargo-zigbuild` then
`cargo zigbuild --release --target aarch64-unknown-linux-gnu -p kinora-iroh`
(after `rustup target add aarch64-unknown-linux-gnu`).

**macOS** (universal binary):

```sh
rustup target add aarch64-apple-darwin x86_64-apple-darwin
cargo build --release -p kinora-iroh --target aarch64-apple-darwin
cargo build --release -p kinora-iroh --target x86_64-apple-darwin
lipo -create -output kinora-iroh \
  target/aarch64-apple-darwin/release/kinora-iroh \
  target/x86_64-apple-darwin/release/kinora-iroh
```

### First build

Every iroh, iroh-tickets, iroh-mdns-address-lookup, hyper and clap call was
checked against the docs.rs signatures for the pinned versions (by a second,
independent review), but the iroh-facing code has never gone through rustc.
If the first build complains, these are the places to look:

* `iroh-tickets` and `iroh` must resolve to the same `iroh-base` 1.x, or the
  ticket's `EndpointAddr` will not convert. `cargo tree -d -i iroh-base`
  should show one version.
* Error conversions go through `Display` (`ErrStr` in `lib.rs`); if a type
  lacks it, switch that spot to `{e:?}`.

Two things only a run can confirm:

* **mDNS added after bind** (`transport::attach_mdns`). It is attached after
  `bind()` so a network without multicast only loses LAN discovery. If a phone
  on the same Wi-Fi never finds the server without the relay, move it into the
  builder with `.address_lookup(MdnsAddressLookup::builder())` before `bind()`.
* **`--udp-port`** binds `0.0.0.0:port` and `[::]:port` separately. Run
  `kinora-e2e --udp-port 7777` once on each OS to confirm the two binds do not
  clash.

## Tests

### Unit tests

```sh
cargo test --workspace
```

Covers the PROXY v2 encoder and parser (exact bytes for IPv4, layout for IPv6,
AF_UNSPEC for relay, IPv4-mapped folding, round trip, every rejection path),
flag and env parsing for both binaries (defaults, all flags, bad input),
the JSON event and command shapes, address scopes, the rate limiter, size
parsing, and the test file generator.

### End to end

```sh
cargo build --release --workspace
target/release/kinora-e2e                 # quick run
target/release/kinora-e2e --long-secs 180 # adds the multi-minute chunked stream
target/release/kinora-e2e --udp-port 7777 # sidecar with a fixed port
target/release/kinora-e2e --skip-relay    # offline machine
```

It starts `kinora-testserver` on 127.0.0.1:18421 (parses PROXY v2; serves a
seeded virtual file with Range support, a slow chunked endpoint, an echo of the
PROXY TLVs, and an upload checksum), then `kinora-iroh` and `kinora-connect`,
and runs:

| # | Test | Pass condition |
|---|---|---|
| 1 | Range and integrity | 4 edge ranges and 32 parallel random ranges (up to 8 MiB each) match sha256 of the expected bytes, with correct `Content-Range` |
| 2 | Backpressure | a reader at 2 MiB/s on a 1 GiB response for 20 s; sidecar and client RSS grow less than 64 MiB |
| 3 | Half-close | 4 MiB POST, client shuts its write side, response arrives with the right checksum |
| 4 | Persistence | sidecar restart keeps the node ID; the running client reconnects by itself within 60 s |
| 5 | Relay-only | a `--relay-only` client sees path `relay` and downloads 64 MiB intact |
| 6 | Direct | client A reaches a `direct-*` path; reports whether the machine has global IPv6 |
| 7 | Peer identity | `/echo` returns client A's node ID and path from the PROXY TLVs |
| 8 | Block | `peer_disconnected` within 1 s of the command; the client's next request gets a 502; `unblock` restores it |
| 9 | Lifetime | closing stdin stops the sidecar within 2 s |
| 10 | Status lines | every stdout line from both sidecar runs parses as a known event with `v` 1, two `ready` lines |
| 11 | Throughput | 1 GiB over the direct path on loopback, 64 MiB over the n0 relay, in Mbps |
| | Long chunked | with `--long-secs`: the whole stream arrives with no gap over 5 s |

Logs and keys go to a temp folder that is removed on success (kept with
`--keep-temp` or on failure).

**What one machine cannot show.** Loopback and LAN addresses prove the
mechanics of direct paths, not hole punching across real NATs, and `::1` says
nothing about global IPv6. For that: run `kinora-iroh` at home, then on a
laptop tethered to a phone (cellular usually has IPv6), run
`kinora-connect <ticket> --status` and watch `path` go from `relay` to
`direct-v6`.

### Manual checks

```sh
target/release/kinora-testserver --listen 127.0.0.1:8421 --verbose &
target/release/kinora-iroh --data-dir ./data            # prints ready with the ticket
target/release/kinora-connect <ticket> --port 18080 --status
curl -s http://127.0.0.1:18080/echo
curl -r 0-99 http://127.0.0.1:18080/file | xxd | head
curl --limit-rate 2M -o /dev/null http://127.0.0.1:18080/file
curl -N "http://127.0.0.1:18080/slow?secs=600&kbps=512" -o /dev/null
```

## Client library

The apps use the same crate through `kinora_tunnel::TunnelClient`:

```rust
let mut cfg = ClientConfig::new(ticket_or_node_id, app_data_dir);
cfg.local_port = Some(18080);                  // optional, falls back to a free port
let tunnel = TunnelClient::start(cfg)?;        // binds, does not dial yet
let base = format!("http://127.0.0.1:{}", tunnel.local_port());
let st = tunnel.status();                      // connected, path, rttMs, relay, bytes, lastError
tunnel.network_change();                       // call on Wi-Fi <-> cellular switches
tunnel.stop();                                 // idempotent, any thread
```

* The listener binds 127.0.0.1 only.
* The first local connection dials the server; later ones share the
  connection. A dead connection is redialled once right away, and lazily after
  that. After a failed dial, new connections get an immediate 502 for 2 s.
* The API is synchronous and uses only `String`, integers, `bool`, `Option`,
  `Vec` and plain structs, plus one error enum, so uniffi can wrap it as is.
  The handle owns its own small tokio runtime.

### The apps: kinora-tunnel-ffi

`crates/kinora-tunnel-ffi` wraps `TunnelClient` for the Kinara apps. Instead
of uniffi (which needs a code generation step and JNA), it has two small
hand-written bindings over one JSON API (`src/api.rs`):

* `src/c.rs`: a C ABI, declared in `include/kinora_tunnel.h`, for Swift.
* `src/android.rs`: JNI entry points for the Kotlin object
  `expo.modules.kinoratunnel.TunnelNative` in the app's
  `modules/kinora-tunnel`. The function names spell out that package, class
  and method names; rename both sides together.

Calls: start (JSON settings in; handle, port, url and node ID out), status
(the `ClientStatus` fields plus "ok"), network change, stop, stop all, and
drain logs (log lines kept in memory, since phones have no console; the
Android module copies them to Logcat under the tag `KinoraTunnel`). Every
answer is JSON with "ok": true, or "ok": false and "error". Non-ASCII is
written as `\uXXXX`, so the same bytes are valid UTF-8 and Java's modified
UTF-8. Panics are caught and become error answers.

Settings: `server` (ticket or node ID), `dataDir` (the Android module fills
it in: the app's files folder plus `kinora-tunnel`, where the device key
lives so its node ID stays the same), `relayUrls` (the Kinara relay from
kinora-cloud; a non-empty list means custom relays), `relayOnly`,
`discoveryN0`, `mdns`, `localPort`, `connectTimeoutMs`.

It depends on `kinora-tunnel` by path with `default-features = false,
features = ["mdns"]`, not through the workspace, so clap stays out of the
app.

**Android build.** The app runs it: `npm run tunnel` in kinora-app calls

```
cargo ndk -t arm64-v8a -t armeabi-v7a -t x86_64 -o <app>/modules/kinora-tunnel/android/src/main/jniLibs --platform 24 build -p kinora-tunnel-ffi --release
```

with this folder as the working directory (it looks for
`../kinora-server/sidecar` next to the app, or `KINORA_SIDECAR`).
`.cargo/config.toml` adds 16 KB page alignment for the Android targets,
which Google Play requires for apps targeting Android 15 and later.

Android settings the app already has: cleartext HTTP is allowed
(`usesCleartextTraffic`), which covers `http://127.0.0.1`. The module's
manifest adds ACCESS_NETWORK_STATE (network change callbacks) and
CHANGE_WIFI_MULTICAST_STATE (a multicast lock while a tunnel runs, for mDNS).

**iOS (not done yet).** Plan: build the staticlib for `aarch64-apple-ios`
and `aarch64-apple-ios-sim` in an EAS `eas-build-pre-install` hook (Rust
installed on the EAS Mac), make an xcframework with
`include/kinora_tunnel.h`, and add a Swift side to the Expo module that
calls the C functions. iOS settings: `NSAllowsLocalNetworking` (already set
by the app), `NSLocalNetworkUsageDescription` (already set), and the
Bonjour type iroh's mDNS uses (service name `irohv1`; confirm the exact type
on a device). iOS suspends background apps, which ends the tunnel; the app
finds the server again when it comes back. Apple TV (tvOS) is a tier 3 Rust
target and stays without the tunnel for now.
