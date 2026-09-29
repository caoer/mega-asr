//go:build darwin

package mac

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/netip"
	"sync"
	"syscall"
	"time"
	"unsafe"

	"github.com/ebitengine/purego"
)

// Bonjour browsing goes through the system's mDNSResponder (dns_sd), which
// owns UDP 5353 on macOS, sends on every interface (the default route may be
// a proxy's tunnel) and is the path Local Network privacy judges.

// Service is a DNS-SD service instance and where it answers.
type Service struct {
	Name  string       // the instance name, e.g. "micbox"
	Host  string       // its host target, e.g. "micbox.local."
	Port  int          // its port
	TXT   []string     // its TXT strings, e.g. "v=1"
	Addrs []netip.Addr // its host's addresses, IPv4 first
}

// ErrLocalNetwork is Browse's error when Local Network privacy refuses this
// app the network (System Settings › Privacy & Security › Local Network).
var ErrLocalNetwork = errors.New("Local Network access is off for this app (System Settings › Privacy & Security › Local Network)")

const (
	dnsFlagMoreComing      = 0x1
	dnsFlagAdd             = 0x2
	dnsFlagShareConnection = 0x4000
	dnsProtoIPv4           = 0x1
	dnsProtoIPv6           = 0x2
	dnsErrPolicyDenied     = -65570
	ifaceLocalOnly         = ^uint32(0) // kDNSServiceInterfaceIndexLocalOnly
)

var (
	dnsCreateConnection func(ref *uintptr) int32
	dnsBrowse           func(ref *uintptr, flags, iface uint32, regtype, domain string, cb, ctx uintptr) int32
	dnsResolve          func(ref *uintptr, flags, iface uint32, name, regtype, domain string, cb, ctx uintptr) int32
	dnsGetAddrInfo      func(ref *uintptr, flags, iface, proto uint32, host string, cb, ctx uintptr) int32
	dnsSockFD           func(ref uintptr) int32
	dnsProcessResult    func(ref uintptr) int32
	dnsDeallocate       func(ref uintptr)

	browseCB, resolveCB, addrCB uintptr
)

func init() {
	purego.RegisterLibFunc(&dnsCreateConnection, libSystem, "DNSServiceCreateConnection")
	purego.RegisterLibFunc(&dnsBrowse, libSystem, "DNSServiceBrowse")
	purego.RegisterLibFunc(&dnsResolve, libSystem, "DNSServiceResolve")
	purego.RegisterLibFunc(&dnsGetAddrInfo, libSystem, "DNSServiceGetAddrInfo")
	purego.RegisterLibFunc(&dnsSockFD, libSystem, "DNSServiceRefSockFD")
	purego.RegisterLibFunc(&dnsProcessResult, libSystem, "DNSServiceProcessResult")
	purego.RegisterLibFunc(&dnsDeallocate, libSystem, "DNSServiceRefDeallocate")
	// Integer arguments arrive as uintptr and are narrowed here: the callee
	// may not rely on a register's upper bits.
	browseCB = purego.NewCallback(func(_, flags, iface, code uintptr, name, regtype, domain *byte, ctx uintptr) {
		if b, ok := lookup(ctx).(*browser); ok {
			b.found(uint32(flags), uint32(iface), int32(uint32(code)), cstr(name), cstr(regtype), cstr(domain))
		}
	})
	resolveCB = purego.NewCallback(func(_, _, _, code uintptr, _, host *byte, port, txtLen uintptr, txt *byte, ctx uintptr) {
		if s, ok := lookup(ctx).(*inst); ok && int32(uint32(code)) == 0 {
			s.resolved(cstr(host), int(ntohs(uint16(port))), parseTXT(unsafe.Slice(txt, int(uint16(txtLen)))))
		}
	})
	addrCB = purego.NewCallback(func(_, _, _, code uintptr, _, sa *byte, _, ctx uintptr) {
		if s, ok := lookup(ctx).(*inst); ok && int32(uint32(code)) == 0 && sa != nil {
			if a, ok := sockaddrIP(sa); ok {
				s.addr(a)
			}
		}
	})
}

// Browse finds the instances of service (e.g. "_megavoice-mic._tcp") in
// local. until ctx ends, and returns those resolved to a port and at least
// one address by then.
func Browse(ctx context.Context, service string) ([]Service, error) {
	return browse(ctx, service, 0)
}

// reg holds the browses and instances in flight by the context id their
// callbacks carry: a Go pointer never goes to C. The callbacks run inside
// dnsProcessResult, on the goroutine that serves the browse.
var (
	regMu  sync.Mutex
	reg    = map[uintptr]any{}
	lastID uintptr
)

func register(v any) uintptr {
	regMu.Lock()
	defer regMu.Unlock()
	lastID++
	reg[lastID] = v
	return lastID
}

func lookup(id uintptr) any {
	regMu.Lock()
	defer regMu.Unlock()
	return reg[id]
}

func unregister(ids ...uintptr) {
	regMu.Lock()
	defer regMu.Unlock()
	for _, id := range ids {
		delete(reg, id)
	}
}

type browser struct {
	main    uintptr
	service string
	err     error
	seen    map[string]bool
	insts   []*inst
}

type inst struct {
	b       *browser
	id      uintptr
	svc     Service
	iface   uint32
	sub     uintptr // the resolve, then the address query, in flight
	hasPort bool
}

func browse(ctx context.Context, service string, iface uint32) ([]Service, error) {
	var main uintptr
	if c := dnsCreateConnection(&main); c != 0 {
		return nil, dnsErr("connect to mDNSResponder", c)
	}
	defer dnsDeallocate(main) // ends the browse and every resolve under it
	b := &browser{main: main, service: service, seen: map[string]bool{}}
	id := register(b)
	defer func() {
		unregister(id)
		for _, s := range b.insts {
			unregister(s.id)
		}
	}()
	ref := main
	if c := dnsBrowse(&ref, dnsFlagShareConnection, iface, service, "local.", browseCB, id); c != 0 {
		return nil, dnsErr("browse "+service, c)
	}
	if err := serve(ctx, main); err != nil {
		return nil, err
	}
	var out []Service
	for _, s := range b.insts {
		if s.hasPort && len(s.svc.Addrs) > 0 {
			out = append(out, s.svc)
		}
	}
	if len(out) == 0 && b.err != nil {
		return nil, b.err
	}
	return out, nil
}

// serve reads mDNSResponder's answers on the connection until ctx ends;
// the callbacks run here, on this goroutine.
func serve(ctx context.Context, main uintptr) error {
	fd := int(dnsSockFD(main))
	if fd < 0 || fd >= syscall.FD_SETSIZE {
		return fmt.Errorf("mDNSResponder socket %d", fd)
	}
	end, ok := ctx.Deadline()
	if !ok {
		end = time.Now().Add(time.Second)
	}
	for {
		left := time.Until(end)
		if left <= 0 || ctx.Err() != nil {
			return nil
		}
		var r syscall.FdSet
		r.Bits[fd/32] |= 1 << (uint(fd) % 32)
		tv := syscall.NsecToTimeval(min(left, 100*time.Millisecond).Nanoseconds())
		if err := syscall.Select(fd+1, &r, nil, nil, &tv); err != nil {
			if errors.Is(err, syscall.EINTR) {
				continue
			}
			return err
		}
		if r.Bits[fd/32]&(1<<(uint(fd)%32)) == 0 {
			continue
		}
		if c := dnsProcessResult(main); c != 0 {
			return dnsErr("mDNSResponder", c)
		}
	}
}

// found is the browse's answer: a new instance starts its resolve.
func (b *browser) found(flags, iface uint32, code int32, name, regtype, domain string) {
	if code != 0 {
		b.err = dnsErr("browse "+b.service, code)
		return
	}
	if flags&dnsFlagAdd == 0 || b.seen[name] {
		return // a removal, or the same box on another interface
	}
	b.seen[name] = true
	s := &inst{b: b, svc: Service{Name: name}, iface: iface}
	s.id = register(s)
	b.insts = append(b.insts, s)
	s.sub = b.main
	if c := dnsResolve(&s.sub, dnsFlagShareConnection, iface, name, regtype, domain, resolveCB, s.id); c != 0 {
		b.err = dnsErr("resolve "+name, c)
		s.sub = 0
	}
}

// resolved is an instance's host and port: its address query starts.
func (s *inst) resolved(host string, port int, txt []string) {
	if s.hasPort {
		return
	}
	s.svc.Host, s.svc.Port, s.svc.TXT, s.hasPort = host, port, txt, true
	if s.sub != 0 {
		dnsDeallocate(s.sub) // one answer is enough
	}
	iface := s.iface
	if iface == ifaceLocalOnly {
		iface = 0 // a local-only service's host is this Mac's name
	}
	s.sub = s.b.main
	if c := dnsGetAddrInfo(&s.sub, dnsFlagShareConnection, iface, dnsProtoIPv4|dnsProtoIPv6, host, addrCB, s.id); c != 0 {
		s.b.err = dnsErr("address of "+host, c)
		s.sub = 0
	}
}

func (s *inst) addr(a netip.Addr) {
	for _, o := range s.svc.Addrs {
		if o == a {
			return
		}
	}
	if a.Is4() {
		s.svc.Addrs = append([]netip.Addr{a}, s.svc.Addrs...)
	} else {
		s.svc.Addrs = append(s.svc.Addrs, a)
	}
}

func dnsErr(what string, code int32) error {
	if code == dnsErrPolicyDenied {
		return fmt.Errorf("%s: %w", what, ErrLocalNetwork)
	}
	return fmt.Errorf("%s: DNSServiceErrorType %d", what, code)
}

// cstr is a C string as Go's.
func cstr(p *byte) string {
	if p == nil {
		return ""
	}
	n := 0
	for *(*byte)(unsafe.Add(unsafe.Pointer(p), n)) != 0 {
		n++
	}
	return string(unsafe.Slice(p, n))
}

func ntohs(v uint16) uint16 { return v<<8 | v>>8 }

// parseTXT splits a TXT record's length-prefixed strings.
func parseTXT(b []byte) []string {
	var out []string
	for len(b) > 0 {
		n := int(b[0])
		if n+1 > len(b) {
			break
		}
		if n > 0 {
			out = append(out, string(b[1:n+1]))
		}
		b = b[n+1:]
	}
	return out
}

// sockaddrIP reads a BSD sockaddr_in or sockaddr_in6; a link-local IPv6
// address gets its interface as the zone.
func sockaddrIP(sa *byte) (netip.Addr, bool) {
	hdr := unsafe.Slice(sa, 2) // sa_len, sa_family
	switch hdr[1] {
	case syscall.AF_INET:
		b := unsafe.Slice(sa, syscall.SizeofSockaddrInet4)
		return netip.AddrFrom4([4]byte(b[4:8])), true
	case syscall.AF_INET6:
		b := unsafe.Slice(sa, syscall.SizeofSockaddrInet6)
		a := netip.AddrFrom16([16]byte(b[8:24]))
		if scope := *(*uint32)(unsafe.Pointer(&b[24])); scope != 0 && a.IsLinkLocalUnicast() {
			if ifi, err := net.InterfaceByIndex(int(scope)); err == nil {
				a = a.WithZone(ifi.Name)
			}
		}
		return a, true
	}
	return netip.Addr{}, false
}
