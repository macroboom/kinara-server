// Package remote runs remote access: it supervises the kinora-iroh sidecar
// (which carries apps' connections over iroh from anywhere) and serves the
// tunnel listener those connections arrive on.
package remote

import (
	"bufio"
	"context"
	"crypto/subtle"
	"encoding/binary"
	"errors"
	"io"
	"net"
)

var proxySig = []byte("\r\n\r\n\x00\r\nQUIT\n")

// PROXY v2 TLVs kinora-iroh writes.
const (
	tlvNodeID = 0xE0
	tlvPath   = 0xE1
	tlvSecret = 0xE2
)

// Peer is the remote device behind a tunnel connection.
type Peer struct {
	// NodeID is the device's iroh key, 64 lowercase hex characters. It
	// names the device; it never signs anyone in.
	NodeID string
	// Path is direct-v4, direct-v6, relay or mixed.
	Path string
	// Addr is the device's UDP address on a direct path (nil when relayed).
	Addr *net.UDPAddr
}

var (
	errNotProxy  = errors.New("proxy: not a PROXY v2 header")
	errBadSecret = errors.New("proxy: missing or wrong tunnel secret")
)

// ReadProxyV2 reads the header kinora-iroh writes first on every
// connection. With a secret set, the header must carry it, so other
// programs on this computer can't pose as the sidecar.
func ReadProxyV2(r *bufio.Reader, secret string) (*Peer, error) {
	hdr := make([]byte, 16)
	if _, err := io.ReadFull(r, hdr); err != nil {
		return nil, err
	}
	if string(hdr[:12]) != string(proxySig) || hdr[12] != 0x21 {
		return nil, errNotProxy
	}
	body := make([]byte, binary.BigEndian.Uint16(hdr[14:16]))
	if _, err := io.ReadFull(r, body); err != nil {
		return nil, err
	}
	var n int
	switch hdr[13] {
	case 0x00:
		n = 0
	case 0x11:
		n = 12
	case 0x21:
		n = 36
	default:
		return nil, errors.New("proxy: unsupported address family")
	}
	if len(body) < n {
		return nil, errors.New("proxy: short address block")
	}
	p := &Peer{}
	switch hdr[13] {
	case 0x11: // src(4) dst(4) srcport(2) dstport(2)
		p.Addr = &net.UDPAddr{IP: net.IP(append([]byte(nil), body[0:4]...)), Port: int(binary.BigEndian.Uint16(body[8:10]))}
	case 0x21: // src(16) dst(16) srcport(2) dstport(2)
		p.Addr = &net.UDPAddr{IP: net.IP(append([]byte(nil), body[0:16]...)), Port: int(binary.BigEndian.Uint16(body[32:34]))}
	}
	var gotSecret []byte
	for t := body[n:]; len(t) > 0; {
		if len(t) < 3 {
			return nil, errors.New("proxy: malformed TLV")
		}
		l := int(binary.BigEndian.Uint16(t[1:3]))
		if len(t) < 3+l {
			return nil, errors.New("proxy: malformed TLV")
		}
		v := t[3 : 3+l]
		switch t[0] {
		case tlvNodeID:
			p.NodeID = string(v)
		case tlvPath:
			p.Path = string(v)
		case tlvSecret:
			gotSecret = v
		}
		t = t[3+l:]
	}
	if !validNodeID(p.NodeID) {
		return nil, errors.New("proxy: missing or bad node ID")
	}
	if secret != "" && subtle.ConstantTimeCompare(gotSecret, []byte(secret)) != 1 {
		return nil, errBadSecret
	}
	return p, nil
}

// validNodeID: 64 lowercase hex characters.
func validNodeID(s string) bool {
	if len(s) != 64 {
		return false
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		if !(c >= '0' && c <= '9' || c >= 'a' && c <= 'f') {
			return false
		}
	}
	return true
}

// ValidNodeID reports whether s looks like an iroh node ID.
func ValidNodeID(s string) bool { return validNodeID(s) }

type peerKey struct{}

// WithPeer marks a context as belonging to a tunnel connection.
func WithPeer(ctx context.Context, p *Peer) context.Context {
	return context.WithValue(ctx, peerKey{}, p)
}

// PeerFrom returns the remote device behind a request that came through the
// tunnel, or nil for requests on the home network.
func PeerFrom(ctx context.Context) *Peer {
	p, _ := ctx.Value(peerKey{}).(*Peer)
	return p
}

// RelayedHeader is the PROXY v2 header kinora-iroh writes for a device on
// a relayed path (no address). For tests and tools that stand in for the
// sidecar.
func RelayedHeader(nodeID, secret string) []byte {
	var body []byte
	add := func(kind byte, v string) {
		body = append(body, kind, byte(len(v)>>8), byte(len(v)))
		body = append(body, v...)
	}
	add(tlvNodeID, nodeID)
	add(tlvPath, "relay")
	if secret != "" {
		add(tlvSecret, secret)
	}
	out := append([]byte(nil), proxySig...)
	out = append(out, 0x21, 0x00, byte(len(body)>>8), byte(len(body)))
	return append(out, body...)
}
