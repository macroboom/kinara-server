package cloud

import (
	"bufio"
	"bytes"
	"context"
	"encoding/binary"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// Mapping is an open port on the router.
type Mapping struct {
	Method     string // "upnp", "natpmp" or "manual"
	ExternalIP string
	External   int
	Internal   int
	Lifetime   time.Duration
}

// portMapper opens an inbound TCP port on the home router.
type portMapper interface {
	Map(ctx context.Context, internal, external int, lifetime time.Duration) (*Mapping, error)
	Unmap(ctx context.Context, m *Mapping)
}

// autoMapper tries NAT-PMP (quick, one packet) then UPnP.
type autoMapper struct {
	gateway func() (net.IP, error)
	localIP func() (net.IP, error)
}

func (a autoMapper) Map(ctx context.Context, internal, external int, lifetime time.Duration) (*Mapping, error) {
	var errs []string
	if gw, err := a.gateway(); err == nil {
		m, err := natpmpMap(ctx, gw, internal, external, lifetime)
		if err == nil {
			return m, nil
		}
		errs = append(errs, "NAT-PMP: "+err.Error())
	} else {
		errs = append(errs, "NAT-PMP: "+err.Error())
	}
	local, err := a.localIP()
	if err != nil {
		return nil, err
	}
	m, err := upnpMap(ctx, local, internal, external, lifetime)
	if err == nil {
		return m, nil
	}
	errs = append(errs, "UPnP: "+err.Error())
	return nil, errors.New(strings.Join(errs, "; "))
}

func (a autoMapper) Unmap(ctx context.Context, m *Mapping) {
	switch m.Method {
	case "natpmp":
		if gw, err := a.gateway(); err == nil {
			natpmpMap(ctx, gw, m.Internal, 0, 0)
		}
	case "upnp":
		if local, err := a.localIP(); err == nil {
			upnpUnmap(ctx, local, m.External)
		}
	}
}

// --- NAT-PMP (RFC 6886) -------------------------------------------------------------

func natpmpMap(ctx context.Context, gw net.IP, internal, external int, lifetime time.Duration) (*Mapping, error) {
	c, err := net.DialUDP("udp4", nil, &net.UDPAddr{IP: gw, Port: 5351})
	if err != nil {
		return nil, err
	}
	defer c.Close()
	exch := func(req []byte, want byte, size int) ([]byte, error) {
		buf := make([]byte, 16)
		wait := 250 * time.Millisecond
		for i := 0; i < 4; i++ {
			if ctx.Err() != nil {
				return nil, ctx.Err()
			}
			c.Write(req)
			c.SetReadDeadline(time.Now().Add(wait))
			n, err := c.Read(buf)
			if err == nil && n >= size && buf[1] == want {
				if code := binary.BigEndian.Uint16(buf[2:4]); code != 0 {
					return nil, fmt.Errorf("router refused (code %d)", code)
				}
				return buf[:n], nil
			}
			wait *= 2
		}
		return nil, errors.New("no answer from the router")
	}
	// External address.
	resp, err := exch([]byte{0, 0}, 128, 12)
	if err != nil {
		return nil, err
	}
	ext := net.IP(resp[8:12]).String()
	req := make([]byte, 12)
	req[1] = 2 // TCP
	binary.BigEndian.PutUint16(req[4:], uint16(internal))
	binary.BigEndian.PutUint16(req[6:], uint16(external))
	binary.BigEndian.PutUint32(req[8:], uint32(lifetime.Seconds()))
	resp, err = exch(req, 130, 16)
	if err != nil {
		return nil, err
	}
	return &Mapping{
		Method: "natpmp", ExternalIP: ext,
		Internal: int(binary.BigEndian.Uint16(resp[8:10])),
		External: int(binary.BigEndian.Uint16(resp[10:12])),
		Lifetime: time.Duration(binary.BigEndian.Uint32(resp[12:16])) * time.Second,
	}, nil
}

// --- UPnP IGD -----------------------------------------------------------------------

type igd struct {
	control string // control URL
	service string // service type
}

var igdServices = []string{
	"urn:schemas-upnp-org:service:WANIPConnection:2",
	"urn:schemas-upnp-org:service:WANIPConnection:1",
	"urn:schemas-upnp-org:service:WANPPPConnection:1",
}

// discoverIGD finds the router's port-mapping service by SSDP.
func discoverIGD(ctx context.Context, local net.IP) (*igd, error) {
	c, err := net.ListenUDP("udp4", &net.UDPAddr{IP: local})
	if err != nil {
		return nil, err
	}
	defer c.Close()
	dst := &net.UDPAddr{IP: net.IPv4(239, 255, 255, 250), Port: 1900}
	for _, st := range []string{"urn:schemas-upnp-org:device:InternetGatewayDevice:1", "urn:schemas-upnp-org:service:WANIPConnection:1"} {
		msg := "M-SEARCH * HTTP/1.1\r\nHOST: 239.255.255.250:1900\r\nMAN: \"ssdp:discover\"\r\nMX: 2\r\nST: " + st + "\r\n\r\n"
		c.WriteTo([]byte(msg), dst)
	}
	deadline := time.Now().Add(3 * time.Second)
	buf := make([]byte, 2048)
	seen := map[string]bool{}
	for time.Now().Before(deadline) && ctx.Err() == nil {
		c.SetReadDeadline(deadline)
		n, _, err := c.ReadFrom(buf)
		if err != nil {
			break
		}
		resp, err := http.ReadResponse(bufio.NewReader(bytes.NewReader(buf[:n])), nil)
		if err != nil {
			continue
		}
		loc := resp.Header.Get("Location")
		if loc == "" || seen[loc] {
			continue
		}
		seen[loc] = true
		if g, err := readDescription(ctx, loc); err == nil {
			return g, nil
		}
	}
	return nil, errors.New("no UPnP router found (UPnP may be turned off)")
}

type upnpDevice struct {
	Services []struct {
		Type    string `xml:"serviceType"`
		Control string `xml:"controlURL"`
	} `xml:"serviceList>service"`
	Devices []upnpDevice `xml:"deviceList>device"`
}

func readDescription(ctx context.Context, loc string) (*igd, error) {
	req, _ := http.NewRequestWithContext(ctx, "GET", loc, nil)
	hc := &http.Client{Timeout: 3 * time.Second}
	resp, err := hc.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	var root struct {
		URLBase string     `xml:"URLBase"`
		Device  upnpDevice `xml:"device"`
	}
	if err := xml.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&root); err != nil {
		return nil, err
	}
	base, err := url.Parse(loc)
	if err != nil {
		return nil, err
	}
	if root.URLBase != "" {
		if b, err := url.Parse(root.URLBase); err == nil {
			base = b
		}
	}
	var walk func(d upnpDevice) *igd
	walk = func(d upnpDevice) *igd {
		for _, want := range igdServices {
			for _, s := range d.Services {
				if s.Type == want {
					u, err := base.Parse(strings.TrimSpace(s.Control))
					if err == nil {
						return &igd{control: u.String(), service: want}
					}
				}
			}
		}
		for _, sub := range d.Devices {
			if g := walk(sub); g != nil {
				return g
			}
		}
		return nil
	}
	if g := walk(root.Device); g != nil {
		return g, nil
	}
	return nil, errors.New("router has no port-mapping service")
}

func soap(ctx context.Context, g *igd, action string, args [][2]string) (map[string]string, error) {
	var b strings.Builder
	b.WriteString(`<?xml version="1.0"?><s:Envelope xmlns:s="http://schemas.xmlsoap.org/soap/envelope/" s:encodingStyle="http://schemas.xmlsoap.org/soap/encoding/"><s:Body>`)
	fmt.Fprintf(&b, `<u:%s xmlns:u="%s">`, action, g.service)
	for _, a := range args {
		fmt.Fprintf(&b, "<%s>", a[0])
		xml.EscapeText(&b, []byte(a[1]))
		fmt.Fprintf(&b, "</%s>", a[0])
	}
	fmt.Fprintf(&b, `</u:%s></s:Body></s:Envelope>`, action)
	req, err := http.NewRequestWithContext(ctx, "POST", g.control, strings.NewReader(b.String()))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", `text/xml; charset="utf-8"`)
	req.Header.Set("SOAPAction", `"`+g.service+"#"+action+`"`)
	resp, err := (&http.Client{Timeout: 5 * time.Second}).Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<16))
	out := map[string]string{}
	dec := xml.NewDecoder(bytes.NewReader(body))
	var cur string
	for {
		t, err := dec.Token()
		if err != nil {
			break
		}
		switch v := t.(type) {
		case xml.StartElement:
			cur = v.Name.Local
		case xml.CharData:
			if cur != "" {
				out[cur] += string(v)
			}
		case xml.EndElement:
			cur = ""
		}
	}
	if resp.StatusCode != 200 {
		if d := out["errorDescription"]; d != "" {
			return nil, fmt.Errorf("%s (%s)", d, out["errorCode"])
		}
		return nil, fmt.Errorf("router answered %d", resp.StatusCode)
	}
	return out, nil
}

func upnpMap(ctx context.Context, local net.IP, internal, external int, lifetime time.Duration) (*Mapping, error) {
	g, err := discoverIGD(ctx, local)
	if err != nil {
		return nil, err
	}
	add := func(secs int) error {
		_, err := soap(ctx, g, "AddPortMapping", [][2]string{
			{"NewRemoteHost", ""}, {"NewExternalPort", strconv.Itoa(external)}, {"NewProtocol", "TCP"},
			{"NewInternalPort", strconv.Itoa(internal)}, {"NewInternalClient", local.String()},
			{"NewEnabled", "1"}, {"NewPortMappingDescription", "Kinara remote access"},
			{"NewLeaseDuration", strconv.Itoa(secs)},
		})
		return err
	}
	secs := int(lifetime.Seconds())
	if err := add(secs); err != nil {
		// Many routers only take permanent mappings (lease 0).
		if err2 := add(0); err2 != nil {
			return nil, err
		}
		secs = 0
	}
	ext := ""
	if r, err := soap(ctx, g, "GetExternalIPAddress", nil); err == nil {
		ext = strings.TrimSpace(r["NewExternalIPAddress"])
	}
	return &Mapping{Method: "upnp", ExternalIP: ext, Internal: internal, External: external, Lifetime: time.Duration(secs) * time.Second}, nil
}

func upnpUnmap(ctx context.Context, local net.IP, external int) {
	g, err := discoverIGD(ctx, local)
	if err != nil {
		return
	}
	soap(ctx, g, "DeletePortMapping", [][2]string{{"NewRemoteHost", ""}, {"NewExternalPort", strconv.Itoa(external)}, {"NewProtocol", "TCP"}})
}

// --- local network facts ------------------------------------------------------------

// outboundIP is the address this machine uses to reach the internet.
func outboundIP(network, probe string) (net.IP, error) {
	c, err := net.Dial(network, probe)
	if err != nil {
		return nil, err
	}
	defer c.Close()
	return c.LocalAddr().(*net.UDPAddr).IP, nil
}

// guessGateway assumes the router is .1 on the outbound interface's /24,
// which is right for nearly every home network. NAT-PMP answers only from
// the real gateway, so a wrong guess just falls through to UPnP.
func guessGateway() (net.IP, error) {
	ip, err := outboundIP("udp4", "8.8.8.8:53")
	if err != nil {
		return nil, err
	}
	v4 := ip.To4()
	return net.IPv4(v4[0], v4[1], v4[2], 1), nil
}

// localAddrs lists this machine's home network addresses (private IPv4,
// and 100.64/10 for Tailscale), and its global IPv6 addresses.
func localAddrs() (lan []string, v6 []string) {
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
			ip := ipn.IP
			if v4 := ip.To4(); v4 != nil {
				if v4.IsPrivate() || (v4[0] == 100 && v4[1]&0xc0 == 64) {
					lan = append(lan, v4.String())
				}
				continue
			}
			if ip.IsGlobalUnicast() && !ip.IsPrivate() {
				// Skip temporary (privacy) addresses where the OS says so is
				// not possible portably; the cloud's reachability check keeps
				// only the ones that answer.
				v6 = append(v6, ip.String())
			}
		}
	}
	return lan, v6
}
