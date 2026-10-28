//go:build darwin

package mac

import (
	"context"
	"fmt"
	"os"
	"slices"
	"testing"
	"time"
)

// A service Register holds is found with its port and TXT, onName hears the
// name it holds, and Register returns nil once its context ends.
func TestRegisterLocalOnly(t *testing.T) {
	name := fmt.Sprintf("megavoice-reg-%d", os.Getpid())
	ctx, cancel := context.WithCancel(context.Background())
	named := make(chan string, 4)
	ended := make(chan error, 1)
	go func() {
		ended <- registerOn(ctx, ifaceLocalOnly, name, "localhost", "_megavoice-test._tcp", 7867, []string{"v=1", "fp=0123456789abcdef"}, func(n string) { named <- n })
	}()
	select {
	case n := <-named:
		if n != name {
			t.Fatalf("registered as %q, want %q", n, name)
		}
	case err := <-ended:
		t.Fatalf("Register ended: %v", err)
	case <-time.After(2 * time.Second):
		t.Fatal("no name from mDNSResponder")
	}
	bctx, bcancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer bcancel()
	got, err := browse(bctx, "_megavoice-test._tcp", ifaceLocalOnly)
	if err != nil {
		t.Fatal(err)
	}
	i := slices.IndexFunc(got, func(s Service) bool { return s.Name == name })
	if i < 0 {
		t.Fatalf("%s not among %+v", name, got)
	}
	if s := got[i]; s.Port != 7867 || !slices.Equal(s.TXT, []string{"v=1", "fp=0123456789abcdef"}) {
		t.Errorf("found %+v", s)
	}
	cancel()
	select {
	case err := <-ended:
		if err != nil {
			t.Fatalf("Register returned %v after its context ended", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Register did not return after its context ended")
	}
}

func TestTXTRecord(t *testing.T) {
	b, err := txtRecord([]string{"v=1", "fp=ab"})
	if err != nil || string(b) != "\x03v=1\x05fp=ab" {
		t.Fatalf("txtRecord = %q, %v", b, err)
	}
	if _, err := txtRecord([]string{string(make([]byte, 256))}); err == nil {
		t.Fatal("a 256-byte TXT string passed")
	}
}
