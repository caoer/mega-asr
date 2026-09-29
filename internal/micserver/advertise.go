package micserver

import (
	"context"
	"net"
	"strings"

	"github.com/brutella/dnssd"
)

// ServiceType is the DNS-SD service a mic server advertises in local.: the
// instance is the server's name, the port its listen port, TXT v=1. A Mac's
// Add Remote Mic… lists what answers a browse for it.
const ServiceType = "_megavoice-mic._tcp"

// netIface is an interface as advertIfaces weighs it.
type netIface struct {
	Name  string
	Flags net.Flags
	Addrs []net.IP
}

func listIfaces() ([]netIface, error) {
	ifs, err := net.Interfaces()
	if err != nil {
		return nil, err
	}
	out := make([]netIface, 0, len(ifs))
	for _, i := range ifs {
		ni := netIface{Name: i.Name, Flags: i.Flags}
		addrs, _ := i.Addrs()
		for _, a := range addrs {
			if ipn, ok := a.(*net.IPNet); ok {
				ni.Addrs = append(ni.Addrs, ipn.IP)
			}
		}
		out = append(out, ni)
	}
	return out, nil
}

// advertIfaces are the interfaces to advertise on for a server bound to
// host (an IP, as the listener reports it): every up, multicast,
// non-loopback interface with an address when host is unspecified, else the
// one holding host. Loopback never: a Mac is on the LAN.
func advertIfaces(host string, ifs []netIface) []string {
	ip := net.ParseIP(host)
	if host != "" && ip == nil {
		return nil
	}
	var out []string
	for _, i := range ifs {
		if i.Flags&net.FlagUp == 0 || i.Flags&net.FlagMulticast == 0 || i.Flags&net.FlagLoopback != 0 || len(i.Addrs) == 0 {
			continue
		}
		if host == "" || ip.IsUnspecified() || containsIP(i.Addrs, ip) {
			out = append(out, i.Name)
		}
	}
	return out
}

func containsIP(ips []net.IP, ip net.IP) bool {
	for _, a := range ips {
		if a.Equal(ip) {
			return true
		}
	}
	return false
}

// advertise starts answering DNS-SD for the server bound at a, on the
// interfaces it listens on, until ctx ends. A failure is logged: serving
// paired Macs does not need it.
func (s *Server) advertise(ctx context.Context, a *net.TCPAddr) {
	ifs, err := listIfaces()
	if err != nil {
		s.logf("mDNS: %v", err)
		return
	}
	on := advertIfaces(a.IP.String(), ifs)
	if len(on) == 0 {
		s.logf("mDNS: not advertised, %s is on no LAN interface", a)
		return
	}
	s.logf("mDNS: advertising %s as %s on %s", ServiceType, s.cfg.Name, strings.Join(on, ", "))
	go func() {
		if err := advertise(ctx, s.cfg.Name, a.Port, on); err != nil && ctx.Err() == nil {
			s.logf("mDNS: %v", err)
		}
	}()
}

// advertise answers DNS-SD for name at port on ifaces until ctx ends, then
// withdraws it.
func advertise(ctx context.Context, name string, port int, ifaces []string) error {
	sv, err := dnssd.NewService(dnssd.Config{
		Name:   name,
		Type:   ServiceType,
		Domain: "local",
		Port:   port,
		Text:   map[string]string{"v": "1"},
		Ifaces: ifaces,
	})
	if err != nil {
		return err
	}
	rp, err := dnssd.NewResponder()
	if err != nil {
		return err
	}
	if _, err := rp.Add(sv); err != nil {
		return err
	}
	return rp.Respond(ctx)
}
