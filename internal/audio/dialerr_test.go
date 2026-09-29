package audio

import (
	"context"
	"errors"
	"io"
	"net/netip"
	"runtime"
	"strings"
	"syscall"
	"testing"
)

// A proxy that answers DNS with addresses from its fake-IP range accepts the
// dial and hangs up, so the bare error is an EOF. DialError says the host
// resolved into that range, whatever error the dial returned.
func TestDialErrorNamesAProxyFakeIP(t *testing.T) {
	was := lookupIP
	defer func() { lookupIP = was }()
	lookupIP = func(_ context.Context, host string) ([]netip.Addr, error) {
		if host == "recorder" {
			return []netip.Addr{netip.MustParseAddr("198.19.117.52")}, nil
		}
		return []netip.Addr{netip.MustParseAddr("192.0.2.140")}, nil
	}
	err := DialError("recorder:7866", io.EOF)
	for _, w := range []string{"recorder is 198.19.117.52", "fake IP", "LAN address", "megavoice mic pair"} {
		if !strings.Contains(err.Error(), w) {
			t.Errorf("%q lacks %q", err, w)
		}
	}
	if !errors.Is(err, io.EOF) {
		t.Errorf("%v does not wrap the dial error", err)
	}
	if err := DialError("198.19.250.96:7866", io.EOF); !strings.Contains(err.Error(), "fake IP") {
		t.Errorf("an address typed inside the range: %v", err)
	}
	plain := errors.New("connection refused")
	if err := DialError("box:7866", plain); err != plain {
		t.Errorf("an ordinary failure is rewritten: %v", err)
	}
	if err := DialError("box:7866", syscall.ECONNRESET); !strings.Contains(err.Error(), "closed the connection before TLS finished") {
		t.Errorf("reset during TLS: %v", err)
	}
	if err := DialError("box:7866", io.ErrUnexpectedEOF); !strings.Contains(err.Error(), "closed the connection before TLS finished") {
		t.Errorf("EOF during TLS: %v", err)
	}
}

// On macOS an app not allowed Local Network access gets "no route to host"
// for every LAN address, its ssh child's included: the error says where the
// switch is. A public address's is left alone.
func TestDialErrorNamesLocalNetworkAccess(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Skip("Local Network privacy is macOS's")
	}
	err := DialError("10.4.4.4:7866", syscall.EHOSTUNREACH)
	if !strings.Contains(err.Error(), "Local Network") || !errors.Is(err, syscall.EHOSTUNREACH) {
		t.Errorf("LAN address: %v", err)
	}
	if err := DialError("203.0.113.184:7866", syscall.EHOSTUNREACH); err != syscall.EHOSTUNREACH {
		t.Errorf("public address rewritten: %v", err)
	}
}
