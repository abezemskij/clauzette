package tools

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"time"
)

// Shared, SSRF-hardened HTTP machinery used by the network tools (fetch_url and
// web search). The guarantee: the host is resolved and every address a request
// would connect to is checked before the dial — on the initial request and on
// each redirect — refusing private/loopback/link-local/metadata ranges unless
// allowPrivate is set. The checked IP is dialled directly, so the address
// cannot change between the check and the dial.

func guardedDial(allowPrivate bool, ctx context.Context, network, addr string) (net.Conn, error) {
	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		return nil, err
	}
	ips, err := net.DefaultResolver.LookupIPAddr(ctx, host)
	if err != nil {
		return nil, err
	}
	d := &net.Dialer{Timeout: 10 * time.Second}
	var firstErr error
	for _, ipa := range ips {
		if !allowPrivate && isBlockedIP(ipa.IP) {
			if firstErr == nil {
				firstErr = fmt.Errorf("refusing to connect to %s: %s is a private, loopback, link-local or metadata address", host, ipa.IP)
			}
			continue
		}
		conn, derr := d.DialContext(ctx, network, net.JoinHostPort(ipa.IP.String(), port))
		if derr == nil {
			return conn, nil
		}
		firstErr = derr
	}
	if firstErr == nil {
		firstErr = fmt.Errorf("no usable address for %s", host)
	}
	return nil, firstErr
}

// guardedClient builds an http.Client whose connections go through guardedDial.
// checkRedirect may be nil (the default redirect behaviour then applies).
func guardedClient(allowPrivate bool, timeout time.Duration, checkRedirect func(*http.Request, []*http.Request) error) *http.Client {
	return &http.Client{
		Timeout: timeout,
		Transport: &http.Transport{
			DialContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
				return guardedDial(allowPrivate, ctx, network, addr)
			},
			TLSHandshakeTimeout: 10 * time.Second,
			DisableKeepAlives:   true,
		},
		CheckRedirect: checkRedirect,
	}
}

// isBlockedIP reports whether ip is in a range the network tools must not
// reach: loopback, link-local (incl. 169.254.169.254 metadata), multicast, the
// unspecified address, RFC 1918 / ULA private ranges, and CGNAT 100.64.0.0/10.
func isBlockedIP(ip net.IP) bool {
	if ip == nil {
		return true
	}
	if ip.IsLoopback() || ip.IsLinkLocalUnicast() || ip.IsLinkLocalMulticast() ||
		ip.IsMulticast() || ip.IsUnspecified() || ip.IsPrivate() {
		return true
	}
	if ip4 := ip.To4(); ip4 != nil && ip4[0] == 100 && ip4[1]&0xc0 == 64 {
		return true // 100.64.0.0/10 (carrier-grade NAT)
	}
	return false
}
