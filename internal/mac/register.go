//go:build darwin

package mac

import (
	"context"
	"errors"
	"fmt"
	"syscall"
	"time"

	"github.com/ebitengine/purego"
)

var (
	dnsRegister func(ref *uintptr, flags, iface uint32, name, regtype, domain, host string, port, txtLen uint16, txt *byte, cb, ctx uintptr) int32
	registerCB  uintptr
)

func init() {
	purego.RegisterLibFunc(&dnsRegister, libSystem, "DNSServiceRegister")
	registerCB = purego.NewCallback(func(_, _, code uintptr, name, _, _ *byte, ctx uintptr) {
		if r, ok := lookup(ctx).(*registration); ok {
			r.answer(int32(uint32(code)), cstr(name))
		}
	})
}

// registration is one service registered with mDNSResponder; its callback
// runs on the goroutine that serves it.
type registration struct {
	onName func(string)
	err    error
}

func (r *registration) answer(code int32, name string) {
	if code != 0 {
		r.err = dnsErr("register", code)
		return
	}
	if r.onName != nil {
		r.onName(name)
	}
}

// Register advertises service (e.g. "_megavoice-ctl._tcp") in local. at port
// under this Mac's computer name, with the TXT strings txt, until ctx ends;
// mDNSResponder renames it on a conflict. onName gets the name it holds,
// each time that changes. It returns once ctx ends, or with the error that
// ended the registration (ErrLocalNetwork when Local Network privacy
// refuses this app).
func Register(ctx context.Context, service string, port int, txt []string, onName func(string)) error {
	return registerOn(ctx, 0, "", "", service, port, txt, onName)
}

// registerOn is Register on interface iface (0: every one) under name and
// host ("": this Mac's).
func registerOn(ctx context.Context, iface uint32, name, host, service string, port int, txt []string, onName func(string)) error {
	rec, err := txtRecord(txt)
	if err != nil {
		return err
	}
	r := &registration{onName: onName}
	id := register(r)
	defer unregister(id)
	var ref uintptr
	var p *byte
	if len(rec) > 0 {
		p = &rec[0]
	}
	if c := dnsRegister(&ref, 0, iface, name, service, "", host, htons(uint16(port)), uint16(len(rec)), p, registerCB, id); c != 0 {
		return dnsErr("register "+service, c)
	}
	defer dnsDeallocate(ref) // withdraws the service
	return serveUntil(ctx, ref, &r.err)
}

// serveUntil reads mDNSResponder's answers on ref until ctx ends or *failed
// is set by a callback.
func serveUntil(ctx context.Context, ref uintptr, failed *error) error {
	fd := int(dnsSockFD(ref))
	if fd < 0 || fd >= syscall.FD_SETSIZE {
		return fmt.Errorf("mDNSResponder socket %d", fd)
	}
	for ctx.Err() == nil {
		var rs syscall.FdSet
		rs.Bits[fd/32] |= 1 << (uint(fd) % 32)
		tv := syscall.NsecToTimeval((250 * time.Millisecond).Nanoseconds())
		if err := syscall.Select(fd+1, &rs, nil, nil, &tv); err != nil {
			if errors.Is(err, syscall.EINTR) {
				continue
			}
			return err
		}
		if rs.Bits[fd/32]&(1<<(uint(fd)%32)) == 0 {
			continue
		}
		if c := dnsProcessResult(ref); c != 0 {
			return dnsErr("mDNSResponder", c)
		}
		if *failed != nil {
			return *failed
		}
	}
	return nil
}

// txtRecord is TXT strings as a record's length-prefixed bytes.
func txtRecord(txt []string) ([]byte, error) {
	var b []byte
	for _, s := range txt {
		if len(s) > 255 {
			return nil, fmt.Errorf("TXT string of %d bytes", len(s))
		}
		b = append(b, byte(len(s)))
		b = append(b, s...)
	}
	if len(b) > 0xffff {
		return nil, errors.New("TXT record over 64 KiB")
	}
	return b, nil
}

func htons(v uint16) uint16 { return v<<8 | v>>8 }
