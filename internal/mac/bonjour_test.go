//go:build darwin

package mac

import (
	"context"
	"fmt"
	"os"
	"slices"
	"testing"
	"time"

	"github.com/ebitengine/purego"
)

// A service registered on this Mac alone (the local-only interface, nothing
// on the network) is found with its port, TXT and host's address.
func TestBrowseLocalOnly(t *testing.T) {
	var register func(ref *uintptr, flags, iface uint32, name, regtype, domain, host string, port, txtLen uint16, txt *byte, cb, ctx uintptr) int32
	purego.RegisterLibFunc(&register, libSystem, "DNSServiceRegister")
	name := fmt.Sprintf("megavoice-test-%d", os.Getpid())
	txt := []byte("\x03v=1")
	var ref uintptr
	if c := register(&ref, 0, ifaceLocalOnly, name, "_megavoice-test._tcp", "", "localhost", ntohs(7866), uint16(len(txt)), &txt[0], 0, 0); c != 0 {
		t.Fatalf("DNSServiceRegister: %d", c)
	}
	defer dnsDeallocate(ref)

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	got, err := browse(ctx, "_megavoice-test._tcp", ifaceLocalOnly)
	if err != nil {
		t.Fatal(err)
	}
	i := slices.IndexFunc(got, func(s Service) bool { return s.Name == name })
	if i < 0 {
		t.Fatalf("%s not among %+v", name, got)
	}
	s := got[i]
	if s.Port != 7866 || !slices.Equal(s.TXT, []string{"v=1"}) || len(s.Addrs) == 0 || !s.Addrs[0].IsLoopback() {
		t.Errorf("found %+v; want port 7866, TXT v=1, a loopback address first", s)
	}
}

func TestParseTXT(t *testing.T) {
	for in, want := range map[string][]string{
		"\x03v=1\x05a=bcd": {"v=1", "a=bcd"},
		"\x00":             nil,
		"\x09v=1":          nil, // runs past the end
		"":                 nil,
	} {
		if got := parseTXT([]byte(in)); !slices.Equal(got, want) {
			t.Errorf("parseTXT(%q) = %q, want %q", in, got, want)
		}
	}
}
