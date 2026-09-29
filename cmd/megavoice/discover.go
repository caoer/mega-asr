//go:build darwin

package main

import (
	"bytes"
	"cmp"
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net"
	"net/netip"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
	"unicode"

	"github.com/caoer/mega-asr/internal/app"
	"github.com/caoer/mega-asr/internal/audio"
	"github.com/caoer/mega-asr/internal/mac"
	"github.com/caoer/mega-asr/internal/micserver"
)

// The mic servers Add Remote Mic… and `mic scan` offer, and pairing one over
// ssh: a box the Mac can ssh to without a prompt hands this Mac a token and
// its certificate's fingerprint over that authenticated channel, so no PIN.

// micService is the DNS-SD type a mic server advertises itself as.
const micService = "_megavoice-mic._tcp"

// The seams a test replaces: ssh, the Bonjour browse, name lookup and the
// TLS probe.
var (
	// sshCmd runs ssh with args: its stdout, its stderr's last line, and its
	// exit status (-1 when it did not run).
	sshCmd = func(ctx context.Context, args ...string) ([]byte, string, int) {
		c := exec.CommandContext(ctx, "ssh", args...)
		var stderr bytes.Buffer
		c.Stderr = &stderr
		out, err := c.Output()
		var ee *exec.ExitError
		switch {
		case errors.As(err, &ee):
			return out, lastLine(stderr.Bytes()), ee.ExitCode()
		case err != nil:
			return out, err.Error(), -1
		}
		return out, lastLine(stderr.Bytes()), 0
	}
	browseMics = func(ctx context.Context) ([]mac.Service, error) { return mac.Browse(ctx, micService) }
	lookupIP   = func(ctx context.Context, host string) ([]netip.Addr, error) {
		return net.DefaultResolver.LookupNetIP(ctx, "ip", host)
	}
	probeMic = micserver.Probe
)

func lastLine(b []byte) string {
	lines := strings.Split(strings.TrimSpace(string(b)), "\n")
	return strings.TrimSpace(lines[len(lines)-1])
}

// sshConfigPath is the user's ssh config.
func sshConfigPath() string {
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".ssh", "config")
}

// sshHosts are the Host names in the ssh config at path and the files it
// Includes, in order, without patterns (*, ?, !) or repeats.
func sshHosts(path string) []string {
	home, _ := os.UserHomeDir()
	var out []string
	seen := map[string]bool{}
	var read func(string, int)
	read = func(path string, depth int) {
		b, err := os.ReadFile(path)
		if err != nil || depth > 16 {
			return
		}
		for _, line := range strings.Split(string(b), "\n") {
			key, args := sshLine(line)
			switch strings.ToLower(key) {
			case "host":
				for _, h := range args {
					if !strings.ContainsAny(h, "*?!") && !seen[h] {
						seen[h] = true
						out = append(out, h)
					}
				}
			case "include":
				for _, p := range args {
					if rest, ok := strings.CutPrefix(p, "~/"); ok {
						p = filepath.Join(home, rest)
					} else if !filepath.IsAbs(p) {
						p = filepath.Join(home, ".ssh", p)
					}
					ms, _ := filepath.Glob(p)
					for _, m := range ms {
						read(m, depth+1)
					}
				}
			}
		}
	}
	read(path, 0)
	return out
}

// sshLine is an ssh_config line's keyword and arguments ("Key args" or
// "Key=args"; double quotes group).
func sshLine(line string) (string, []string) {
	line = strings.TrimSpace(line)
	if line == "" || line[0] == '#' {
		return "", nil
	}
	i := strings.IndexAny(line, " \t=")
	if i < 0 {
		return line, nil
	}
	key, rest := line[:i], strings.TrimLeft(line[i:], " \t")
	rest = strings.TrimLeft(strings.TrimPrefix(rest, "="), " \t")
	var args []string
	for ; rest != ""; rest = strings.TrimLeft(rest, " \t") {
		if rest[0] == '"' {
			end := strings.IndexByte(rest[1:], '"')
			if end < 0 {
				return key, append(args, rest[1:])
			}
			args, rest = append(args, rest[1:1+end]), rest[2+end:]
			continue
		}
		end := strings.IndexAny(rest, " \t")
		if end < 0 {
			end = len(rest)
		}
		args, rest = append(args, rest[:end]), rest[end:]
	}
	return key, args
}

// sshHostname is the address ssh reaches dest at (ssh -G's hostname):
// on a Mac behind a proxy the name can resolve to a fake IP where the ssh
// config names the box's LAN address.
func sshHostname(ctx context.Context, dest string) string {
	out, _, code := sshCmd(ctx, "-G", "--", dest)
	if code == 0 {
		for _, l := range strings.Split(string(out), "\n") {
			if h, ok := strings.CutPrefix(l, "hostname "); ok && strings.TrimSpace(h) != "" {
				return strings.TrimSpace(h)
			}
		}
	}
	return dest
}

// sshBox is an ssh Host, the address ssh -G gives it, and why no mic
// server answers TLS there on micPort (nil: one does).
type sshBox struct {
	dest, host string
	ips        []netip.Addr
	err        error
}

// sshBoxes takes each Host's address from ssh -G (a bounded number at a
// time), then resolves and probes them all at once for up to probeFor. Only
// a nearby address is probed (a scan at every menu open must not knock on
// internet servers); public counts the Hosts left unprobed for that. A name
// that resolves to a proxy's fake IP is left out, as it never reaches a box.
func sshBoxes(ctx context.Context, hosts []string, probeFor time.Duration) (boxes []sshBox, public int) {
	names := make([]string, len(hosts))
	sem := make(chan struct{}, 16)
	var wg sync.WaitGroup
	for i, dest := range hosts {
		wg.Go(func() {
			sem <- struct{}{}
			defer func() { <-sem }()
			names[i] = sshHostname(ctx, dest)
		})
	}
	wg.Wait()
	ctx, cancel := context.WithTimeout(ctx, probeFor)
	defer cancel()
	found := make([]*sshBox, len(hosts))
	far := make([]bool, len(hosts))
	for i, dest := range hosts {
		wg.Go(func() {
			ips := resolve(ctx, names[i])
			if len(ips) == 0 || slices.ContainsFunc(ips, audio.IsFakeIP) {
				return
			}
			near := slices.IndexFunc(ips, nearby)
			if near < 0 {
				far[i] = true
				return
			}
			found[i] = &sshBox{dest: dest, host: names[i], ips: ips, err: probeMic(ctx, net.JoinHostPort(ips[near].String(), micPort))}
		})
	}
	wg.Wait()
	for i, b := range found {
		if b != nil {
			boxes = append(boxes, *b)
		}
		if far[i] {
			public++
		}
	}
	return boxes, public
}

// cgnat is 100.64.0.0/10: carrier-grade NAT, and the mesh VPNs' addresses.
var cgnat = netip.MustParsePrefix("100.64.0.0/10")

// nearby is whether a scan may probe ip: loopback, private (RFC 1918, ULA),
// CGNAT or mesh, link-local — never a public server.
func nearby(ip netip.Addr) bool {
	ip = ip.Unmap()
	return ip.IsLoopback() || ip.IsPrivate() || ip.IsLinkLocalUnicast() || cgnat.Contains(ip)
}

func resolve(ctx context.Context, host string) []netip.Addr {
	if ip, err := netip.ParseAddr(host); err == nil {
		return []netip.Addr{ip}
	}
	ctx, cancel := context.WithTimeout(ctx, time.Second)
	defer cancel()
	ips, _ := lookupIP(ctx, host)
	return ips
}

// scanMics is what Add Remote Mic… offers: every mic server advertised on
// the network and every nearby ssh Host that answers TLS on micPort, one row per
// address and port, named as the box advertises itself, else by its Host;
// a row pairs over ssh when an ssh Host has its address. note says why the
// network was not searched, when it was not; public counts the ssh Hosts at
// public addresses, which are not probed.
func scanMics(ctx context.Context, sshConfig string) (found []foundView, note string, public int) {
	var (
		wg    sync.WaitGroup
		svcs  []mac.Service
		boxes []sshBox
	)
	wg.Go(func() {
		bctx, cancel := context.WithTimeout(ctx, time.Second)
		defer cancel()
		var err error
		if svcs, err = browseMics(bctx); err != nil {
			log.Printf("mic scan: %v", err)
			note = "not searched on the network: " + err.Error()
			if errors.Is(err, mac.ErrLocalNetwork) {
				note = mac.ErrLocalNetwork.Error()
			}
		}
	})
	wg.Go(func() { boxes, public = sshBoxes(ctx, sshHosts(sshConfig), time.Second) }) // a LAN box answers in milliseconds
	wg.Wait()
	if note == "" && len(svcs) == 0 && lanRefused(boxes) {
		note = "no LAN host answers: " + audio.LocalNetworkHint
	}
	return mergeFound(boxes, svcs), note, public
}

// lanRefused is whether the LAN Hosts probed got "no route to host" and
// none answered: macOS refuses an app the local network until it is
// allowed.
func lanRefused(boxes []sshBox) bool {
	refused := false
	for _, b := range boxes {
		switch {
		case !audio.IsLAN(b.ips[0]):
		case errors.Is(b.err, syscall.EHOSTUNREACH):
			refused = true
		case b.err == nil, !errors.Is(b.err, context.DeadlineExceeded) && !errors.Is(b.err, os.ErrDeadlineExceeded) && !errors.Is(b.err, syscall.EHOSTDOWN):
			return false // something on the LAN answered
		}
	}
	return refused
}

func mergeFound(boxes []sshBox, svcs []mac.Service) []foundView {
	overlap := func(a, b []netip.Addr) bool {
		return slices.ContainsFunc(a, func(x netip.Addr) bool { return slices.Contains(b, x) })
	}
	type seen struct {
		ips  []netip.Addr
		port int
	}
	var out []foundView
	var at []seen // each row's addresses and port
	row := func(ips []netip.Addr, port int) int {
		return slices.IndexFunc(at, func(s seen) bool { return s.port == port && overlap(s.ips, ips) })
	}
	std, _ := strconv.Atoi(micPort)
	for _, b := range boxes {
		if b.err == nil && row(b.ips, std) < 0 {
			out = append(out, foundView{Name: b.dest, Addr: net.JoinHostPort(b.host, micPort), SSH: b.dest})
			at = append(at, seen{b.ips, std})
		}
	}
	// An mDNS answer is anyone's on the LAN: it never names a row an ssh Host
	// reaches, only labels it, and its name is stripped for display.
	label := func(host, name string) string {
		if name == host {
			return ""
		}
		return name
	}
	for _, s := range svcs {
		name := printable(s.Name)
		if i := row(s.Addrs, s.Port); i >= 0 {
			out[i].Advertised = label(out[i].Name, name)
			continue
		}
		port := strconv.Itoa(s.Port)
		f := foundView{Name: name, Addr: net.JoinHostPort(s.Addrs[0].String(), port)}
		if i := slices.IndexFunc(boxes, func(b sshBox) bool { return overlap(b.ips, s.Addrs) }); i >= 0 {
			b := boxes[i]
			f = foundView{Name: b.dest, Advertised: label(b.dest, name), Addr: net.JoinHostPort(b.host, port), SSH: b.dest}
		}
		if f.Name == "" {
			f.Name = f.Addr
		}
		out = append(out, f)
		at = append(at, seen{s.Addrs, s.Port})
	}
	slices.SortStableFunc(out, func(a, b foundView) int { return strings.Compare(a.Name, b.Name) })
	return out
}

// printable is a name from the network as a menu or a terminal may show it:
// graphic characters only (no control or format characters), at most 64.
func printable(s string) string {
	s = strings.TrimSpace(strings.Map(func(r rune) rune {
		if unicode.IsGraphic(r) {
			return r
		}
		return -1
	}, s))
	if r := []rune(s); len(r) > 64 {
		s = string(r[:64])
	}
	return s
}

// tokenLine is the one line `megavoice mic token NAME` prints on the box.
type tokenLine struct {
	Box         string `json:"box"`
	Client      string `json:"client"`
	Token       string `json:"token"`
	Fingerprint string `json:"fingerprint"`
	Port        int    `json:"port"`
}

var (
	nameRE = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,63}$`) // micserver's names: a client's, a box's
	fpRE   = regexp.MustCompile(`^[0-9a-f]{64}$`)
)

const nameRule = "letters, digits, '.', '_' and '-', up to 64"

// parseToken reads the token line for client from the op's stdout: its
// last non-empty line. An error names the bad field and never quotes a line
// that is JSON: it may hold a live token, and errors reach the log and
// alerts.
func parseToken(out []byte, client string) (tokenLine, error) {
	var t tokenLine
	line := lastLine(out)
	switch {
	case line == "":
		return t, errors.New("printed nothing")
	case !strings.HasPrefix(line, "{"):
		return t, fmt.Errorf("printed %q, not a token line", trim(line, 60))
	}
	if err := json.Unmarshal([]byte(line), &t); err != nil {
		return tokenLine{}, errors.New("printed a line that is not valid JSON")
	}
	switch {
	case t.Client != client:
		return tokenLine{}, fmt.Errorf("paired client %q, not %q", trim(t.Client, 70), client)
	case !nameRE.MatchString(t.Box):
		return tokenLine{}, fmt.Errorf("box name %q: %s", trim(t.Box, 70), nameRule)
	case t.Token == "":
		return tokenLine{}, errors.New("no token")
	case t.Port <= 0 || t.Port > 65535:
		return tokenLine{}, fmt.Errorf("port %d", t.Port)
	case !fpRE.MatchString(t.Fingerprint):
		return tokenLine{}, fmt.Errorf("fingerprint %q: not 64 hex digits", trim(t.Fingerprint, 70))
	}
	return t, nil
}

// sshFailed is an ssh pairing that got no token: the caller asks for the
// PIN instead.
type sshFailed struct{ err error }

func (e sshFailed) Error() string { return e.err.Error() }
func (e sshFailed) Unwrap() error { return e.err }

// sshPair pairs this Mac as name with the mic server on the ssh host dest,
// no PIN: BatchMode ssh (no prompt) runs the box's `megavoice mic token`
// ($MEGAVOICE_BOX_CMD names another megavoice there, e.g. with --config),
// whose token and certificate fingerprint arrive over ssh's authenticated
// channel; the stream dials dest's ssh address at port, else the box's
// port. The certificate there must be the one ssh named. An error before
// the token is sshFailed.
func sshPair(ctx context.Context, config, dest, port, name string) (app.Remote, error) {
	if !nameRE.MatchString(name) {
		return app.Remote{}, fmt.Errorf("client name %q: %s", name, nameRule)
	}
	host := sshHostname(ctx, dest)
	box := cmp.Or(os.Getenv("MEGAVOICE_BOX_CMD"), "megavoice") // the box's megavoice when ssh's PATH lacks it
	out, stderr, code := sshCmd(ctx, "-o", "BatchMode=yes", "-o", "ConnectTimeout=3", "-T", "--", dest, box, "mic", "token", name)
	switch {
	case code == 255 || code < 0:
		msg := cmp.Or(stderr, "failed")
		if strings.Contains(msg, "No route to host") {
			msg += " — " + audio.LocalNetworkHint
		}
		return app.Remote{}, sshFailed{fmt.Errorf("ssh %s: %s", dest, msg)}
	case code != 0:
		return app.Remote{}, sshFailed{fmt.Errorf("on %s, megavoice mic token: %s", dest, cmp.Or(stderr, "exit "+strconv.Itoa(code)))}
	}
	t, err := parseToken(out, name)
	if err != nil {
		return app.Remote{}, sshFailed{fmt.Errorf("on %s, megavoice mic token: %w", dest, err)}
	}
	addr := net.JoinHostPort(host, cmp.Or(port, strconv.Itoa(t.Port)))
	if err := pinned(ctx, addr, t.Fingerprint); err != nil {
		return app.Remote{}, err
	}
	r := app.Remote{Name: t.Box, Addr: addr, Fingerprint: t.Fingerprint, Token: t.Token, Client: name}
	return r, keepRemote(config, r)
}

// pinned checks that addr presents the certificate fp names.
func pinned(ctx context.Context, addr, fp string) error {
	ctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	d := tls.Dialer{Config: audio.PinnedTLS(fp)}
	c, err := d.DialContext(ctx, "tcp", addr)
	if errors.Is(err, audio.ErrNotPinned) {
		return fmt.Errorf("%s presents another certificate than the box named over ssh: %w", addr, err)
	}
	if err != nil {
		return fmt.Errorf("%s unreachable: %w", addr, audio.DialError(addr, err))
	}
	return c.Close()
}

// splitDest is HOST[:PORT] as an ssh destination and a port, "" when none.
func splitDest(host string) (dest, port string) {
	if h, p, err := net.SplitHostPort(host); err == nil {
		return h, p
	}
	return strings.Trim(host, "[]"), ""
}

// pinAddr is where a PIN pairing with dest dials: its ssh address, at port
// or micPort.
func pinAddr(ctx context.Context, dest, port string) string {
	return net.JoinHostPort(sshHostname(ctx, dest), cmp.Or(port, micPort))
}
