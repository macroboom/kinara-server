package api

import (
	"net"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"
)

// localAddress: loopback, private (10/8, 172.16/12, 192.168/16, fc00::/7),
// link-local, carrier-grade NAT (100.64/10, which Tailscale uses), or on one
// of the server's own networks. The last covers home networks with public
// IPv6: a laptop next to the server often connects from its global address.
func localAddress(ip net.IP) bool {
	if ip.IsLoopback() || ip.IsPrivate() || ip.IsLinkLocalUnicast() {
		return true
	}
	if v4 := ip.To4(); v4 != nil && v4[0] == 100 && v4[1]&0xc0 == 64 {
		return true
	}
	for _, n := range ownNetworks() {
		if n.Contains(ip) {
			return true
		}
	}
	return false
}

// ownNetworks are the subnets of this machine's interfaces, read at most
// once a minute (they change when Wi-Fi or a VPN does).
func ownNetworks() []*net.IPNet {
	ownNets.Lock()
	defer ownNets.Unlock()
	if time.Since(ownNets.at) > time.Minute {
		ownNets.nets = ownNets.nets[:0]
		if addrs, err := net.InterfaceAddrs(); err == nil {
			for _, a := range addrs {
				if n, ok := a.(*net.IPNet); ok && !n.IP.IsLoopback() {
					// A single address (/32, /128) says nothing about neighbours.
					if ones, bits := n.Mask.Size(); ones < bits {
						ownNets.nets = append(ownNets.nets, n)
					}
				}
			}
		}
		ownNets.at = time.Now()
	}
	return ownNets.nets
}

var ownNets struct {
	sync.Mutex
	at   time.Time
	nets []*net.IPNet
}

// trustedProxies are the reverse proxies in front of the server
// (KINARA_TRUSTED_PROXIES: comma-separated addresses or CIDR ranges, such as
// 127.0.0.1 for Caddy or nginx on the same machine). Only for these is
// X-Forwarded-For believed; anyone else could write it themselves.
var trustedProxies = sync.OnceValue(func() []*net.IPNet {
	return parseProxies(os.Getenv("KINARA_TRUSTED_PROXIES"))
})

func parseProxies(list string) []*net.IPNet {
	var out []*net.IPNet
	for _, f := range strings.Split(list, ",") {
		f = strings.TrimSpace(f)
		if f == "" {
			continue
		}
		if !strings.Contains(f, "/") {
			if ip := net.ParseIP(f); ip != nil {
				bits := 128
				if ip.To4() != nil {
					bits = 32
				}
				out = append(out, &net.IPNet{IP: ip, Mask: net.CIDRMask(bits, bits)})
			}
			continue
		}
		if _, n, err := net.ParseCIDR(f); err == nil {
			out = append(out, n)
		}
	}
	return out
}

func isTrusted(ip net.IP, proxies []*net.IPNet) bool {
	for _, n := range proxies {
		if n.Contains(ip) {
			return true
		}
	}
	return false
}

// clientIP is the address of whoever made the request. Behind a trusted
// proxy it's the nearest address in X-Forwarded-For that isn't one of the
// proxies (the ones further left were written by the client and prove
// nothing).
func clientIP(r *http.Request) string {
	return clientIPWith(r, trustedProxies())
}

func clientIPWith(r *http.Request, proxies []*net.IPNet) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		host = r.RemoteAddr
	}
	peer := net.ParseIP(host)
	if peer == nil || len(proxies) == 0 || !isTrusted(peer, proxies) {
		return host
	}
	hops := strings.Split(strings.Join(r.Header.Values("X-Forwarded-For"), ","), ",")
	for i := len(hops) - 1; i >= 0; i-- {
		ip := net.ParseIP(strings.TrimSpace(hops[i]))
		if ip == nil {
			break
		}
		if !isTrusted(ip, proxies) {
			return ip.String()
		}
	}
	return host
}
