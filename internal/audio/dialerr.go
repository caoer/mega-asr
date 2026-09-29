package audio

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/netip"
	"runtime"
	"slices"
	"syscall"
	"time"
)

// fakeIPs is the range proxies (Surge, Clash, sing-box) answer intercepted
// names with: a connection there reaches the proxy, never the box.
var fakeIPs = netip.MustParsePrefix("198.18.0.0/15")

// LocalNetworkHint is where macOS lets an app reach the local network: until
// the user allows it there, every LAN connection the app makes, its ssh
// child's included, fails "no route to host".
const LocalNetworkHint = "if MegaVoice is not allowed Local Network access, allow it in System Settings › Privacy & Security › Local Network"

// IsLAN is whether ip is on a local network: private or link-local.
func IsLAN(ip netip.Addr) bool { ip = ip.Unmap(); return ip.IsPrivate() || ip.IsLinkLocalUnicast() }

// IsFakeIP is whether ip is in a proxy's fake-IP range.
func IsFakeIP(ip netip.Addr) bool { return fakeIPs.Contains(ip.Unmap()) }

// lookupIP resolves a host as this Mac does; a test replaces it.
var lookupIP = func(ctx context.Context, host string) ([]netip.Addr, error) {
	return net.DefaultResolver.LookupNetIP(ctx, "ip", host)
}

// DialError explains a failed connection to a mic server at addr when the
// cause is one the user can act on, wrapping err: a host that resolves into
// a proxy's fake-IP range, a LAN address macOS refuses this app (Local
// Network access), or a peer that closed or reset before TLS finished
// (something other than `megavoice mic serve` answers). Anything else is
// err unchanged.
func DialError(addr string, err error) error {
	host, _, splitErr := net.SplitHostPort(addr)
	if splitErr != nil {
		host = addr
	}
	var ips []netip.Addr
	if ip, perr := netip.ParseAddr(host); perr == nil {
		ips = []netip.Addr{ip}
	} else {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		ips, _ = lookupIP(ctx, host)
	}
	for _, ip := range ips {
		if IsFakeIP(ip) {
			return fmt.Errorf("%s is %s, a proxy's fake IP (%s), not the box: use its LAN address, which `megavoice mic pair` prints there (%w)", host, ip, fakeIPs, err)
		}
	}
	if runtime.GOOS == "darwin" && errors.Is(err, syscall.EHOSTUNREACH) && slices.ContainsFunc(ips, IsLAN) {
		return fmt.Errorf("%s: no route to host — %s (%w)", host, LocalNetworkHint, err)
	}
	if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) || errors.Is(err, syscall.ECONNRESET) {
		return fmt.Errorf("%s closed the connection before TLS finished — something other than `megavoice mic serve` answers there (a proxy?): use the box's LAN address, which `megavoice mic pair` prints there (%w)", host, err)
	}
	return err
}
