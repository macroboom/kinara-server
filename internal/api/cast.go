package api

import (
	"net"
	"sort"
	"strconv"
)

// Casting: a phone starts a conversion as usual (POST /hls/{id}) and hands
// the playlist to a Chromecast, which fetches it itself. The phone may be
// talking to the server through remote access or an address the Chromecast
// can't use (the tunnel's local port, Tailscale), so the answer also lists
// the server's addresses on the home network.

// lanBases are "http://192.168.1.20:8420" for each private IPv4 address of
// this computer (not Tailscale's 100.64/10, which a Chromecast can't reach),
// the usual home ranges first.
func (s *Server) lanBases() []string {
	if s.LANPort <= 0 {
		return []string{}
	}
	var ips []net.IP
	ifaces, _ := net.Interfaces()
	for _, ifc := range ifaces {
		if ifc.Flags&net.FlagUp == 0 || ifc.Flags&net.FlagLoopback != 0 {
			continue
		}
		addrs, _ := ifc.Addrs()
		for _, a := range addrs {
			ipn, ok := a.(*net.IPNet)
			if !ok {
				continue
			}
			if v4 := ipn.IP.To4(); v4 != nil && v4.IsPrivate() {
				ips = append(ips, v4)
			}
		}
	}
	rank := func(ip net.IP) int {
		switch {
		case ip[0] == 192 && ip[1] == 168:
			return 0
		case ip[0] == 10:
			return 1
		default: // 172.16/12: often a virtual adapter (Docker, WSL)
			return 2
		}
	}
	sort.SliceStable(ips, func(i, j int) bool { return rank(ips[i]) < rank(ips[j]) })
	out := make([]string, 0, len(ips))
	for _, ip := range ips {
		out = append(out, "http://"+ip.String()+":"+strconv.Itoa(s.LANPort))
	}
	return out
}
