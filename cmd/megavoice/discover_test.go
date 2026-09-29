//go:build darwin

package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http/httptest"
	"net/netip"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"

	"github.com/caoer/mega-asr/internal/app"
	"github.com/caoer/mega-asr/internal/audio"
	"github.com/caoer/mega-asr/internal/mac"
	"github.com/caoer/mega-asr/internal/micserver"
)

// ssh config Hosts: in order, through Include (relative to ~/.ssh, ~, a
// glob), without wildcard patterns, Match blocks or repeats.
func TestSSHHosts(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	dir := filepath.Join(home, ".ssh")
	os.MkdirAll(filepath.Join(dir, "conf.d"), 0o700)
	write := func(name, s string) { os.WriteFile(filepath.Join(dir, name), []byte(s), 0o600) }
	write("config", `# hosts
Include conf.d/*.conf
Host north pier*
  HostName 192.0.2.33
Host=eq "quoted one" ?x !neg
Match host foo
  User bar
  include ~/.ssh/extra
Host github.com gitlab.com *
`)
	write("conf.d/a.conf", "Host alpha\nHost north\n")
	write("conf.d/b.conf", "HOST beta\n")
	write("extra", "Host gamma\n")
	got := sshHosts(filepath.Join(dir, "config"))
	want := []string{"alpha", "north", "beta", "eq", "quoted one", "gamma", "github.com", "gitlab.com"}
	if !slices.Equal(got, want) {
		t.Errorf("sshHosts = %q, want %q", got, want)
	}
	if got := sshHosts(filepath.Join(dir, "absent")); got != nil {
		t.Errorf("absent config: %q", got)
	}
}

// fakeSSH answers `ssh -G HOST` from hostnames and every other ssh command
// with run, recording the argv. It may be called from several goroutines at
// once, as sshBoxes calls ssh.
func fakeSSH(t *testing.T, hostnames map[string]string, run func(args []string) ([]byte, string, int)) *[][]string {
	t.Helper()
	var (
		mu    sync.Mutex
		calls [][]string
	)
	old := sshCmd
	sshCmd = func(_ context.Context, args ...string) ([]byte, string, int) {
		mu.Lock()
		calls = append(calls, args)
		mu.Unlock()
		if args[0] == "-G" {
			h := args[len(args)-1]
			if hn, ok := hostnames[h]; ok {
				h = hn
			}
			return []byte("user root\nhostname " + h + "\nport 22\n"), "", 0
		}
		if run == nil {
			return nil, "ssh: connect to host: Connection refused", 255
		}
		return run(args)
	}
	t.Cleanup(func() { sshCmd = old })
	return &calls
}

// Add Remote Mic…'s list. An ssh Host is resolved through ssh -G and probed
// only at a nearby address: a public one is counted, one that resolves to a
// proxy's fake IP is dropped, one that does not answer TLS is not a row by
// itself. Hosts sharing an address are one row, named by the first. An mDNS
// answer only labels a row an ssh Host has (no label when the names agree);
// at such a Host's address on another port it is a row paired over that
// Host; alone it pairs by PIN, its name reduced to graphic characters, or
// its address when nothing is left. Rows sort by name.
func TestScanMics(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	cfg := filepath.Join(os.Getenv("HOME"), "config")
	os.WriteFile(cfg, []byte("Host ferry pumice culvert boathouse\nHost quince kiosk plover skiff\n"), 0o600)
	fakeSSH(t, map[string]string{
		"ferry": "203.0.113.33", "pumice": "172.16.3.3", "culvert": "culvert.example", "boathouse": "10.3.3.3",
		"quince": "172.16.3.3", "kiosk": "kiosk.example", "plover": "192.0.2.44", "skiff": "100.111.3.3",
	}, nil)
	oldLookup, oldProbe, oldBrowse := lookupIP, probeMic, browseMics
	t.Cleanup(func() { lookupIP, probeMic, browseMics = oldLookup, oldProbe, oldBrowse })
	lookupIP = func(_ context.Context, host string) ([]netip.Addr, error) {
		switch host {
		case "culvert.example":
			return []netip.Addr{netip.MustParseAddr("198.18.3.3")}, nil
		case "kiosk.example":
			return []netip.Addr{netip.MustParseAddr("2001:db8::33"), netip.MustParseAddr("198.51.100.33")}, nil
		}
		return nil, errors.New("no such host")
	}
	answers := func(ok ...string) func(context.Context, string) error {
		return func(_ context.Context, addr string) error {
			if slices.Contains(ok, addr) {
				return nil
			}
			return errors.New("unreachable")
		}
	}

	// Browsing refused: the ssh Hosts that answer are still offered.
	browseMics = func(context.Context) ([]mac.Service, error) { return nil, mac.ErrLocalNetwork }
	probeMic = answers("172.16.3.3:7866", "100.111.3.3:7866")
	got, note, _ := scanMics(context.Background(), cfg)
	if !strings.Contains(note, "Local Network") || len(got) != 2 || got[0].SSH != "pumice" || got[1].SSH != "skiff" {
		t.Errorf("browse refused: %+v %q", got, note)
	}

	var (
		mu     sync.Mutex
		probed []string
	)
	ok := answers("172.16.3.3:7866", "100.111.3.3:7866")
	probeMic = func(ctx context.Context, addr string) error { // called from sshBoxes' goroutines
		mu.Lock()
		probed = append(probed, addr)
		mu.Unlock()
		return ok(ctx, addr)
	}
	browseMics = func(context.Context) ([]mac.Service, error) {
		return []mac.Service{
			{Name: "pumice", Port: 7866, Addrs: []netip.Addr{netip.MustParseAddr("172.16.3.3")}},
			{Name: "\x07\u200b", Port: 7866, Addrs: []netip.Addr{netip.MustParseAddr("198.51.100.44")}},
			{Name: "hallway", Port: 7912, Addrs: []netip.Addr{netip.MustParseAddr("fe80::33"), netip.MustParseAddr("10.3.3.3")}},
			{Name: "veranda\t\x1b]0;x\x07", Port: 7866, Addrs: []netip.Addr{netip.MustParseAddr("192.0.2.55")}},
			{Name: "lantern", Port: 7931, Addrs: []netip.Addr{netip.MustParseAddr("172.16.3.3")}},
		}, nil
	}
	got, note, public := scanMics(context.Background(), cfg)
	want := []foundView{
		{Name: "198.51.100.44:7866", Addr: "198.51.100.44:7866"},
		{Name: "boathouse", Advertised: "hallway", Addr: "10.3.3.3:7912", SSH: "boathouse"},
		{Name: "pumice", Addr: "172.16.3.3:7866", SSH: "pumice"},
		{Name: "pumice", Advertised: "lantern", Addr: "172.16.3.3:7931", SSH: "pumice"},
		{Name: "skiff", Addr: "100.111.3.3:7866", SSH: "skiff"},
		{Name: "veranda]0;x", Addr: "192.0.2.55:7866"},
	}
	if !slices.Equal(got, want) || note != "" || public != 3 {
		t.Errorf("scanMics = %+v %q %d public, want %+v, 3 public", got, note, public, want)
	}
	for _, a := range probed {
		if ip := netip.MustParseAddrPort(a).Addr(); !nearby(ip) || audio.IsFakeIP(ip) {
			t.Errorf("probed %s: only nearby addresses that are not a proxy's are probed", a)
		}
	}

	// Local Network access: one LAN Host refusing the connection shows it
	// is on; every LAN Host unreachable with nothing browsed shows it is off.
	browseMics = func(context.Context) ([]mac.Service, error) { return nil, nil }
	probeMic = func(_ context.Context, addr string) error {
		if addr == "10.3.3.3:7866" {
			return syscall.ECONNREFUSED
		}
		return syscall.EHOSTUNREACH
	}
	if _, note, _ := scanMics(context.Background(), cfg); note != "" {
		t.Errorf("a LAN Host refused, yet: %q", note)
	}
	probeMic = func(_ context.Context, addr string) error {
		return fmt.Errorf("%s unreachable: %w", addr, &net.OpError{Op: "dial", Err: syscall.EHOSTUNREACH})
	}
	if got, note, _ := scanMics(context.Background(), cfg); got != nil || !strings.Contains(note, "Local Network") {
		t.Errorf("LAN unreachable: %+v %q", got, note)
	}
}

// nearby is loopback, private (RFC 1918, ULA), CGNAT and mesh
// (100.64.0.0/10) and link-local, in either IP form; anything else is
// public. The edges of 172.16.0.0/12 and 100.64.0.0/10 are forced by those
// ranges.
func TestNearby(t *testing.T) {
	for a, want := range map[string]bool{
		"3.3.3.3": false, "::ffff:3.3.3.3": false, "198.51.100.33": false, "2001:db8::33": false,
		"172.15.255.255": false, "172.32.0.0": false, "100.63.255.255": false, "100.128.0.0": false,
		"fd33::33": true, "169.254.3.3": true, "fe80::44": true, "127.3.3.3": true, "::1": true,
		"100.64.0.0": true, "100.127.255.255": true, "100.111.3.3": true,
		"10.33.33.33": true, "::ffff:10.33.33.33": true, "172.16.0.0": true, "172.31.255.255": true, "192.168.33.33": true,
	} {
		if got := nearby(netip.MustParseAddr(a)); got != want {
			t.Errorf("nearby(%s) = %v, want %v", a, got, want)
		}
	}
}

// micBox is a mic server on loopback for the Mac side to pair with.
func micBox(t *testing.T) (*micserver.Server, string) {
	t.Helper()
	srv, err := micserver.New(micserver.Config{State: filepath.Join(t.TempDir(), "mic"), Name: "micbox", Logf: t.Logf})
	if err != nil {
		t.Fatal(err)
	}
	ts := httptest.NewUnstartedServer(srv.Handler())
	ts.TLS = srv.TLSConfig()
	ts.StartTLS()
	t.Cleanup(ts.Close)
	return srv, ts.Listener.Addr().String()
}

func scratchConfig(t *testing.T) {
	t.Helper()
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	t.Setenv("MEGAVOICE_CONFIG", "")
}

// tokenOp is the box's `megavoice mic token NAME` as ssh runs it: srv
// mints and the line carries fp (the box's own unless a test lies).
func tokenOp(srv *micserver.Server, port string, fp string) func([]string) ([]byte, string, int) {
	return func(args []string) ([]byte, string, int) {
		name := args[len(args)-1]
		tok, err := srv.Token(name, 0)
		if err != nil {
			return nil, "megavoice: " + err.Error(), 1
		}
		if fp == "" {
			fp = srv.Fingerprint()
		}
		b, _ := json.Marshal(map[string]any{"box": "micbox", "client": name, "token": tok, "fingerprint": fp, "port": mustAtoi(port)})
		return append([]byte("\n"), append(b, '\n')...), "", 0
	}
}

func mustAtoi(s string) int {
	n, err := strconv.Atoi(s)
	if err != nil {
		panic(err)
	}
	return n
}

// mic add HOST pairs over ssh with no PIN when BatchMode ssh reaches the
// box: the token and fingerprint `mic token` prints are pinned, the address
// is ssh -G's, and the pairing is the source.
func TestMicAddOverSSH(t *testing.T) {
	scratchConfig(t)
	srv, addr := micBox(t)
	_, port, _ := net.SplitHostPort(addr)
	calls := fakeSSH(t, map[string]string{"micbox": "127.0.0.1"}, tokenOp(srv, port, ""))
	var out bytes.Buffer
	if err := micRemote(app.LoadOpts{}, []string{"add", "micbox", "--name", "studio"}, strings.NewReader(""), &out); err != nil {
		t.Fatalf("mic add: %v\n%s", err, out.String())
	}
	if strings.Contains(out.String(), "PIN") || !strings.Contains(out.String(), "paired with micbox") {
		t.Errorf("output: %s", out.String())
	}
	token := []string{"-o", "BatchMode=yes", "-o", "ConnectTimeout=3", "-T", "--", "micbox", "megavoice", "mic", "token", "studio"}
	if !slices.ContainsFunc(*calls, func(c []string) bool { return slices.Equal(c, token) }) {
		t.Errorf("ssh calls %q, want one %q", *calls, token)
	}
	rs, _ := app.LoadRemotes(app.RemotesPath())
	if len(rs) != 1 || rs[0].Name != "micbox" || rs[0].Addr != "127.0.0.1:"+port || rs[0].Fingerprint != srv.Fingerprint() || rs[0].Client != "studio" || rs[0].Token == "" {
		t.Fatalf("remotes.toml: %+v", rs)
	}
	if st, _ := os.Stat(app.RemotesPath()); st.Mode().Perm() != 0o600 {
		t.Errorf("remotes.toml mode %v", st.Mode().Perm())
	}
	if l, err := load(app.LoadOpts{}); err != nil || l.Capture.Source != "remote" || l.Capture.Remote != "micbox" {
		t.Errorf("config after mic add: %+v %v", l.Capture, err)
	}
	if cs := srv.Clients(); len(cs) != 1 || cs[0].Name != "studio" {
		t.Errorf("the box's clients: %+v", cs)
	}
}

// A fingerprint over ssh that is not the certificate the address presents
// pins nothing.
func TestMicAddOverSSHNotPinned(t *testing.T) {
	scratchConfig(t)
	srv, addr := micBox(t)
	_, port, _ := net.SplitHostPort(addr)
	fakeSSH(t, map[string]string{"micbox": "127.0.0.1"}, tokenOp(srv, port, strings.Repeat("ab", 32)))
	err := micRemote(app.LoadOpts{}, []string{"add", "micbox", "--name", "studio"}, strings.NewReader(""), &bytes.Buffer{})
	if !errors.Is(err, audio.ErrNotPinned) {
		t.Fatalf("mic add: %v, want ErrNotPinned", err)
	}
	if _, err := os.Stat(app.RemotesPath()); err == nil {
		t.Error("a pairing that did not pin wrote remotes.toml")
	}
}

// When BatchMode ssh does not reach the box, or its megavoice refuses,
// mic add says why and asks for the PIN, at ssh -G's address.
func TestMicAddFallsBackToPIN(t *testing.T) {
	for name, run := range map[string]func([]string) ([]byte, string, int){
		"no ssh": nil,
		"refused": func([]string) ([]byte, string, int) {
			return nil, "megavoice: token: uid 501 is not root or in megavoice-mic", 1
		},
		"garbled": func([]string) ([]byte, string, int) { return []byte("usage: megavoice …\n"), "", 0 },
	} {
		t.Run(name, func(t *testing.T) {
			scratchConfig(t)
			srv, addr := micBox(t)
			_, port, _ := net.SplitHostPort(addr)
			fakeSSH(t, map[string]string{"box": "127.0.0.1"}, run)
			pin, _ := srv.NewPIN()
			var out bytes.Buffer
			if err := micRemote(app.LoadOpts{}, []string{"add", "box:" + port, "--name", "studio"}, strings.NewReader(pin.Code+"\n"), &out); err != nil {
				t.Fatalf("mic add: %v\n%s", err, out.String())
			}
			if !strings.Contains(out.String(), "pairing by PIN") || !strings.Contains(out.String(), "PIN (run `megavoice mic pair` on 127.0.0.1)") {
				t.Errorf("output: %s", out.String())
			}
			if rs, _ := app.LoadRemotes(app.RemotesPath()); len(rs) != 1 || rs[0].Addr != "127.0.0.1:"+port {
				t.Errorf("remotes.toml: %+v", rs)
			}
		})
	}
}

// A bad token line names the bad field and never echoes the line: it can
// carry a live token, and the error reaches megavoice.log and an alert. The
// box's name must be a name (it goes into remotes.toml and config.toml).
func TestParseToken(t *testing.T) {
	fp := strings.Repeat("0a", 32)
	const secret = "LIVE-TOKEN-dGhpcyBpcyBhIHNlY3JldA"
	good := `{"box":"micbox","client":"studio","token":"` + secret + `","fingerprint":"` + fp + `","port":7866}`
	if tk, err := parseToken([]byte("motd\n"+good+"\n\n"), "studio"); err != nil || tk.Box != "micbox" || tk.Port != 7866 || tk.Token != secret {
		t.Errorf("good line: %+v %v", tk, err)
	}
	for bad, field := range map[string]string{
		"":         "nothing",
		"not json": "not json",
		strings.Replace(good, `"studio"`, `"other"`, 1):                       "client",
		strings.Replace(good, fp, "abc", 1):                                   "fingerprint",
		strings.Replace(good, secret, "", 1):                                  "token",
		strings.Replace(good, `7866`, `0`, 1):                                 "port",
		strings.Replace(good, `"micbox"`, `""`, 1):                            "box",
		strings.Replace(good, `"micbox"`, `"a\nb"`, 1):                        "box",
		strings.Replace(good, `"micbox"`, `"-rf"`, 1):                         "box",
		strings.Replace(good, `"micbox"`, `"../../etc"`, 1):                   "box",
		strings.Replace(good, `"micbox"`, `"`+strings.Repeat("b", 65)+`"`, 1): "box",
		strings.TrimSuffix(good, "}"):                                         "not valid JSON",
	} {
		tk, err := parseToken([]byte(bad), "studio")
		if err == nil {
			t.Errorf("%q parsed: %+v", bad, tk)
			continue
		}
		if !strings.Contains(err.Error(), field) {
			t.Errorf("%q: %v does not name %q", bad, err, field)
		}
		if strings.Contains(err.Error(), secret[:12]) {
			t.Errorf("%q: the error echoes the token: %v", bad, err)
		}
	}
}

// Nothing goes into remotes.toml or config.toml under a box name that is
// not a name, whichever pairing gave it.
func TestKeepRemoteRefusesABadName(t *testing.T) {
	scratchConfig(t)
	for _, name := range []string{"", "a\nb", "-x", "a b", "micbox\" = 1"} {
		if err := keepRemote("", app.Remote{Name: name, Addr: "192.0.2.22:7866", Fingerprint: strings.Repeat("0a", 32), Token: "t", Client: "studio"}); err == nil {
			t.Errorf("%q kept", name)
		}
	}
	if _, err := os.Stat(app.RemotesPath()); err == nil {
		t.Error("remotes.toml written")
	}
}

// mic scan labels a row by its ssh Host and shows what it advertises, and
// counts the public Hosts it did not probe.
func TestPrintFound(t *testing.T) {
	var b bytes.Buffer
	printFound(&b, []foundView{{Name: "den", Addr: "192.0.2.22:7866"}, {Name: "annex", Advertised: "lab-recorder", Addr: "192.0.2.11:7866", SSH: "annex"}}, "", 3)
	for _, w := range []string{"annex (advertised as lab-recorder)", "over ssh annex", "den ", "by PIN", "3 ssh config Hosts at public addresses not probed"} {
		if !strings.Contains(b.String(), w) {
			t.Errorf("mic scan output lacks %q:\n%s", w, b.String())
		}
	}
}
