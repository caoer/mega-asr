package micserver

import (
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"slices"
	"testing"
	"time"

	"github.com/miekg/dns"
	"golang.org/x/net/ipv4"
)

// The server advertises on the interfaces it listens on, never loopback:
// every up multicast interface with an address for an unspecified host,
// else the interface that holds the host's address.
func TestAdvertIfaces(t *testing.T) {
	ifs := []netIface{
		{Name: "wg0", Flags: net.FlagUp | net.FlagPointToPoint, Addrs: []net.IP{net.ParseIP("203.0.113.9")}}, // no multicast
		{Name: "wlan0", Flags: net.FlagUp | net.FlagMulticast, Addrs: []net.IP{net.ParseIP("198.51.100.40"), net.ParseIP("2001:db8:5::40")}},
		{Name: "lo", Flags: net.FlagUp | net.FlagLoopback | net.FlagMulticast, Addrs: []net.IP{net.ParseIP("127.0.0.1"), net.ParseIP("::1")}},
		{Name: "usb0", Flags: net.FlagMulticast, Addrs: []net.IP{net.ParseIP("192.0.2.200")}}, // down
		{Name: "eth1", Flags: net.FlagUp | net.FlagMulticast, Addrs: []net.IP{net.ParseIP("192.0.2.77")}},
		{Name: "dummy0", Flags: net.FlagUp | net.FlagMulticast}, // no address
	}
	for _, tc := range []struct {
		host string
		want []string
	}{
		{"", []string{"wlan0", "eth1"}},
		{"::", []string{"wlan0", "eth1"}},
		{"0.0.0.0", []string{"wlan0", "eth1"}},
		{"2001:db8:5::40", []string{"wlan0"}},
		{"192.0.2.77", []string{"eth1"}},
		{"192.0.2.200", nil},
		{"203.0.113.9", nil},
		{"::1", nil},
		{"localhost", nil},
	} {
		if got := advertIfaces(tc.host, ifs); !slices.Equal(got, tc.want) {
			t.Errorf("%q: %v, want %v", tc.host, got, tc.want)
		}
	}
}

// The advertiser answers a DNS-SD browse for _megavoice-mic._tcp with the
// server's name, its port and TXT v=1. Needs a multicast interface: skipped
// on a host with none.
func TestAdvertiseAnswersABrowse(t *testing.T) {
	ifs, err := listIfaces()
	if err != nil {
		t.Fatal(err)
	}
	on := advertIfaces("", ifs)
	v4 := "" // the query goes out over IPv4
	for _, i := range ifs {
		if slices.Contains(on, i.Name) && slices.ContainsFunc(i.Addrs, func(ip net.IP) bool { return ip.To4() != nil }) {
			v4 = i.Name
			break
		}
	}
	if v4 == "" {
		t.Skip("no multicast interface with an IPv4 address")
	}
	if err := multicastLoops(v4); err != nil {
		t.Skipf("multicast on %s does not come back to this host (%v): a host firewall that drops it hides any answer; run in a network namespace with a dummy interface", v4, err)
	}
	name := fmt.Sprintf("megavoice-test-%d", os.Getpid())
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	actx, stop := context.WithCancel(ctx)
	done := make(chan error, 1)
	go func() { done <- advertise(actx, name, 17866, on) }()
	got, err := browse(ctx, v4, name)
	if err != nil {
		select {
		case aerr := <-done:
			t.Fatalf("advertiser ended: %v", aerr)
		default:
		}
		t.Fatalf("browse on %s: %v", v4, err)
	}
	if got.port != 17866 || !slices.Contains(got.txt, "v=1") {
		t.Errorf("answer for %s: port %d, TXT %q; want 17866, v=1", name, got.port, got.txt)
	}
	stop()
	if err := <-done; err != nil && err != context.Canceled {
		t.Errorf("advertiser: %v", err)
	}
}

// multicastLoops checks that a datagram multicast on iface reaches a
// listener on this host, as the browse's query must reach the advertiser.
func multicastLoops(iface string) error {
	ifi, err := net.InterfaceByName(iface)
	if err != nil {
		return err
	}
	group := &net.UDPAddr{IP: net.IPv4(224, 0, 0, 251), Port: 5399}
	l, err := net.ListenMulticastUDP("udp4", ifi, group)
	if err != nil {
		return err
	}
	defer l.Close()
	c, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4zero})
	if err != nil {
		return err
	}
	defer c.Close()
	pc := ipv4.NewPacketConn(c)
	if err := pc.SetMulticastInterface(ifi); err != nil {
		return err
	}
	_ = pc.SetMulticastLoopback(true)
	buf := make([]byte, 16)
	for range 3 {
		if _, err := c.WriteTo([]byte("megavoice-loop"), group); err != nil {
			return err
		}
		_ = l.SetReadDeadline(time.Now().Add(300 * time.Millisecond))
		if n, _, err := l.ReadFrom(buf); err == nil && string(buf[:n]) == "megavoice-loop" {
			return nil
		}
	}
	return errors.New("no datagram came back")
}

type answer struct {
	port int
	txt  []string
}

// browse asks as `dns-sd -B` does — a PTR query for the service type,
// multicast on iface — until an answer names instance, with its SRV and TXT.
// It asks from an ephemeral port, so the responder answers it by unicast.
func browse(ctx context.Context, iface, instance string) (answer, error) {
	ifi, err := net.InterfaceByName(iface)
	if err != nil {
		return answer{}, err
	}
	c, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4zero})
	if err != nil {
		return answer{}, err
	}
	defer c.Close()
	pc := ipv4.NewPacketConn(c)
	if err := pc.SetMulticastInterface(ifi); err != nil {
		return answer{}, err
	}
	_ = pc.SetMulticastLoopback(true)
	q := new(dns.Msg)
	q.SetQuestion(ServiceType+".local.", dns.TypePTR)
	q.RecursionDesired = false
	b, _ := q.Pack()
	fqdn := instance + "." + ServiceType + ".local."
	buf := make([]byte, 9000)
	for ctx.Err() == nil {
		if _, err := c.WriteTo(b, &net.UDPAddr{IP: net.IPv4(224, 0, 0, 251), Port: 5353}); err != nil {
			return answer{}, err
		}
		_ = c.SetReadDeadline(time.Now().Add(500 * time.Millisecond))
		for {
			n, _, err := c.ReadFrom(buf)
			if err != nil {
				break
			}
			var m dns.Msg
			if m.Unpack(buf[:n]) != nil {
				continue
			}
			var a answer
			ptr := false
			for _, rr := range append(m.Answer, m.Extra...) {
				switch r := rr.(type) {
				case *dns.PTR:
					ptr = ptr || r.Ptr == fqdn
				case *dns.SRV:
					if r.Hdr.Name == fqdn {
						a.port = int(r.Port)
					}
				case *dns.TXT:
					if r.Hdr.Name == fqdn {
						a.txt = r.Txt
					}
				}
			}
			if ptr {
				return a, nil
			}
		}
	}
	return answer{}, fmt.Errorf("no answer naming %s", fqdn)
}
