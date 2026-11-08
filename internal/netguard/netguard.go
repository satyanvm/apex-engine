// Package netguard builds HTTP clients for calling URLs that users supply. The
// address is checked after DNS resolution, at connect time, so a public hostname
// that resolves to an internal address is refused too.
package netguard

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"net/netip"
	"os"
	"syscall"
	"time"
)

var cgnat = netip.MustParsePrefix("100.64.0.0/10")

// Blocked reports whether addr is somewhere user-supplied URLs must not reach.
func Blocked(addr netip.Addr) bool {
	addr = addr.Unmap()
	return !addr.IsValid() || addr.IsLoopback() || addr.IsPrivate() || addr.IsLinkLocalUnicast() ||
		addr.IsLinkLocalMulticast() || addr.IsInterfaceLocalMulticast() || addr.IsMulticast() ||
		addr.IsUnspecified() || cgnat.Contains(addr)
}

func control(_, address string, _ syscall.RawConn) error {
	ap, err := netip.ParseAddrPort(address)
	if err != nil {
		return fmt.Errorf("netguard: unparseable address %q: %w", address, err)
	}
	if Blocked(ap.Addr()) {
		return fmt.Errorf("refusing to connect to private address %s", ap.Addr())
	}
	return nil
}

// AllowPrivate reports whether private addresses are permitted (local dev only).
func AllowPrivate() bool { return os.Getenv("APEX_ALLOW_PRIVATE_NETWORK") == "1" }

// Client returns a client for user-supplied URLs. When followRedirects is false
// the first response is returned as is; credentialed calls use that so custom
// auth headers cannot follow a redirect to another host.
func Client(timeout time.Duration, followRedirects bool) *http.Client {
	d := &net.Dialer{Timeout: 10 * time.Second}
	if !AllowPrivate() {
		d.Control = control
	}
	c := &http.Client{
		Timeout: timeout,
		Transport: &http.Transport{
			DialContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
				return d.DialContext(ctx, network, addr)
			},
			TLSHandshakeTimeout:   10 * time.Second,
			ResponseHeaderTimeout: timeout,
			MaxIdleConnsPerHost:   4,
		},
	}
	if !followRedirects {
		c.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	}
	return c
}
