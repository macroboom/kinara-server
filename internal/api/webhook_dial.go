package api

import (
	"context"
	"errors"
	"net"
	"net/http"
	"net/url"
	"os"
	"strings"
	"syscall"
	"time"
)

// Where webhooks may go. A webhook is a request the server makes to an
// address someone typed in, and the answer is shown back to them, so it
// mustn't become a way to reach what only the server can: cloud metadata
// (169.254.169.254) and other link-local addresses are never allowed.
// This computer and the home network are, by default, because that's where
// Home Assistant and the like live; KINARA_WEBHOOKS_LOCAL=off refuses them
// too. Either way, only someone on the home network can point a webhook
// (or send a test) at one of them: from the internet, through an API key
// say, it's refused.

// errWebhookTarget is a delivery to an address webhooks can't reach.
var errWebhookTarget = errors.New("webhooks can't be sent to that address")

func webhooksLocalAllowed() bool {
	return !strings.EqualFold(strings.TrimSpace(os.Getenv("KINARA_WEBHOOKS_LOCAL")), "off")
}

// forbiddenTarget: never a webhook's destination.
func forbiddenTarget(ip net.IP) bool {
	return ip.IsLinkLocalUnicast() || ip.IsLinkLocalMulticast() || ip.IsInterfaceLocalMulticast() ||
		ip.IsMulticast() || ip.IsUnspecified()
}

// localTarget: this computer or the home network.
func localTarget(ip net.IP) bool {
	return !forbiddenTarget(ip) && (ip.IsLoopback() || localAddress(ip))
}

// webhookDialer refuses connections to forbidden addresses, and to local
// ones unless allowed. It checks the address actually connected to, so a
// name that resolves differently at send time doesn't get round it.
func webhookDialer(allowLocal bool) *net.Dialer {
	return &net.Dialer{
		Timeout: 30 * time.Second,
		Control: func(network, address string, _ syscall.RawConn) error {
			host, _, err := net.SplitHostPort(address)
			if err != nil {
				return err
			}
			ip := net.ParseIP(host)
			if ip == nil || forbiddenTarget(ip) || (!allowLocal && localTarget(ip)) {
				return errWebhookTarget
			}
			return nil
		},
	}
}

// webhookClient sends deliveries: guarded dialing, redirects only on the
// same host and never from https to http.
func webhookClient() *http.Client {
	tr := http.DefaultTransport.(*http.Transport).Clone()
	tr.Proxy = nil
	tr.DialContext = webhookDialer(webhooksLocalAllowed()).DialContext
	return &http.Client{Transport: tr, CheckRedirect: sameHostRedirects}
}

// targetsLocal reports whether a webhook address points at this computer
// or the home network (any of the addresses its name resolves to).
func targetsLocal(ctx context.Context, raw string) bool {
	u, err := url.Parse(raw)
	if err != nil {
		return false
	}
	host := u.Hostname()
	if ip := net.ParseIP(host); ip != nil {
		return localTarget(ip) || forbiddenTarget(ip)
	}
	if strings.EqualFold(host, "localhost") || strings.HasSuffix(strings.ToLower(host), ".local") {
		return true
	}
	ctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	ips, err := net.DefaultResolver.LookupIPAddr(ctx, host)
	if err != nil {
		return false // can't tell now; the dialer still checks at send time
	}
	for _, a := range ips {
		if localTarget(a.IP) || forbiddenTarget(a.IP) {
			return true
		}
	}
	return false
}

// checkWebhookFrom refuses, for a request from the internet, a webhook
// address on the home network or this computer.
func checkWebhookFrom(r *http.Request, raw string) error {
	if fromInternet(r) && targetsLocal(r.Context(), raw) {
		return errors.New("a webhook to this computer or the home network can only be set up or tested from the home network")
	}
	return nil
}
