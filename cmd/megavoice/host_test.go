package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/caoer/mega-asr/internal/app"
	"github.com/caoer/mega-asr/internal/audio"
	"github.com/caoer/mega-asr/internal/ctl"
	"github.com/caoer/mega-asr/internal/micserver"
)

func TestParseHost(t *testing.T) {
	for _, tc := range []struct {
		args []string
		want hostCmd
	}{
		{[]string{"mic", "serve"}, hostCmd{"mic", "serve", ""}},
		{[]string{"mic", "pair"}, hostCmd{"mic", "pair", ""}},
		{[]string{"mic", "status"}, hostCmd{"mic", "status", ""}},
		{[]string{"mic", "clients"}, hostCmd{"mic", "clients", ""}},
		{[]string{"mic", "revoke", "studio"}, hostCmd{"mic", "revoke", "studio"}},
		{[]string{"mic", "token", "studio"}, hostCmd{"mic", "token", "studio"}},
		{[]string{"config", "show"}, hostCmd{"config", "show", ""}},
		{[]string{"config", "check"}, hostCmd{"config", "check", ""}},
		{[]string{"config", "check", "/etc/megavoice/config.toml"}, hostCmd{"config", "check", "/etc/megavoice/config.toml"}},
		{[]string{"config", "init", "-"}, hostCmd{"config", "init", "-"}},
	} {
		got, err := parseHost(tc.args)
		if err != nil || got != tc.want {
			t.Errorf("%q: got %+v, %v; want %+v", tc.args, got, err, tc.want)
		}
	}
	for _, args := range [][]string{
		nil, {"mic"}, {"serve"}, {"mic", "revoke"}, {"mic", "revoke", "a", "b"}, {"mic", "pair", "x"}, {"mic", "token"}, {"mic", "token", "a", "b"},
		{"mic", "list"}, {"config"}, {"config", "show", "x"}, {"config", "set", "a", "b"}, {"config", "init", "a", "b"},
	} {
		if got, err := parseHost(args); err == nil {
			t.Errorf("%q parsed as %+v", args, got)
		}
	}
}

// With no --config and no MEGAVOICE_CONFIG, the user's own file wins; a
// user without one (root under sudo) reads the system service's file, so
// `sudo megavoice mic pair` reaches the running service.
func TestHostConfigPath(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", dir)
	t.Setenv("MEGAVOICE_CONFIG", "")
	user := filepath.Join(dir, "megavoice", "config.toml")
	has := map[string]bool{}
	exists := func(p string) bool { return has[p] }
	if got := hostConfigPath("", exists); got != "" {
		t.Errorf("no files: %q, want the defaults", got)
	}
	has[systemConfig] = true
	if got := hostConfigPath("", exists); got != systemConfig {
		t.Errorf("system file only: %q", got)
	}
	has[user] = true
	if got := hostConfigPath("", exists); got != user {
		t.Errorf("user file: %q", got)
	}
	if got := hostConfigPath("/x.toml", exists); got != "/x.toml" {
		t.Errorf("--config: %q", got)
	}
	t.Setenv("MEGAVOICE_CONFIG", "/env.toml")
	if got := hostConfigPath("", exists); got != "/env.toml" {
		t.Errorf("MEGAVOICE_CONFIG: %q", got)
	}
}

// On Linux, local capture is arecord on this machine: capture.mic names the
// PCM, mic_channel 0 is mono and n picks the XVF3800's channel n; ssh is
// the same over ssh. Tail is 0: the Mac applies the take's tail.
func TestHostSource(t *testing.T) {
	c := app.Default()
	c.MicServer.State = "/var/lib/megavoice-mic"
	x, ok := hostSource(c).(*audio.XVF)
	if !ok || x.Host != "" || x.Device != "default" || x.Channel != 0 || x.Tail != 0 {
		t.Fatalf("default: %#v", hostSource(c))
	}
	c.Capture.Mic, c.Capture.MicChannel = "hw:Array,0", 2
	if x := hostSource(c).(*audio.XVF); x.Host != "" || x.Device != "hw:Array,0" || x.Channel != 2 {
		t.Fatalf("local: %#v", x)
	}
	c.Capture = app.CaptureConfig{Source: "ssh", Host: "box", Device: "plughw:1,0", Channel: 3}
	if x := hostSource(c).(*audio.XVF); x.Host != "box" || x.Device != "plughw:1,0" || x.Channel != 3 || x.CtlPath != "/var/lib/megavoice-mic/cm" {
		t.Fatalf("ssh: %#v", x)
	}
}

// The server records local or ssh; a channel beyond the XVF3800's six is
// refused at load, not at the first stream.
func TestHostCheck(t *testing.T) {
	c := app.Default()
	if errs := hostCheck(c); len(errs) != 0 {
		t.Fatalf("defaults: %v", errs)
	}
	c.Capture.MicChannel = 7
	if errs := hostCheck(c); len(errs) != 1 || !strings.HasPrefix(errs[0], "capture.mic_channel: 7") {
		t.Errorf("channel 7: %v", errs)
	}
	c.Capture.MicChannel = 0
	c.Capture.Source = "remote"
	if errs := hostCheck(c); len(errs) != 1 || !strings.HasPrefix(errs[0], "capture.source: remote") {
		t.Errorf("remote: %v", errs)
	}
}

// config show on Linux prints the tables the host uses.
func TestHostConfigShow(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", dir)
	t.Setenv("MEGAVOICE_CONFIG", "")
	path := filepath.Join(dir, "box.toml")
	if err := os.WriteFile(path, []byte("[capture]\nmic = \"hw:Array,0\"\nmic_channel = 2\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	var out strings.Builder
	if err := hostConfig(&out, app.LoadOpts{Path: path}, hostCmd{"config", "show", ""}); err != nil {
		t.Fatal(err)
	}
	s := out.String()
	if !strings.Contains(s, "\n[capture]\n") || !strings.Contains(s, "\n[mic_server]\n") || strings.Contains(s, "[asr]") || !strings.Contains(s, `mic = "hw:Array,0"`) {
		t.Fatalf("show:\n%s", s)
	}
}

type fakeMic struct {
	pin     micserver.PIN
	clients []micserver.Client
	revoked []string
	tokens  []string // name@uid, per Token
}

func (f *fakeMic) NewPIN() (micserver.PIN, error) { return f.pin, nil }
func (f *fakeMic) Clients() []micserver.Client    { return f.clients }
func (f *fakeMic) Fingerprint() string            { return "ab12" }
func (f *fakeMic) Token(name string, uid int) (string, error) {
	if name == "bad name" {
		return "", errors.New("client name \"bad name\": letters, digits")
	}
	f.tokens = append(f.tokens, fmt.Sprintf("%s@%d", name, uid))
	return "tok-" + name, nil
}
func (f *fakeMic) Revoke(name string) error {
	for _, c := range f.clients {
		if c.Name == name {
			f.revoked = append(f.revoked, name)
			return nil
		}
	}
	return micserver.ErrUnknownClient
}

// The control socket's commands, answered by the server and printed by the
// CLI as the user reads them.
func TestMicControl(t *testing.T) {
	exp := time.Date(2020, 3, 14, 2, 0, 0, 0, time.Local)
	seen := exp.Add(-time.Hour)
	f := &fakeMic{pin: micserver.PIN{Code: "12345678", Expires: exp}, clients: []micserver.Client{
		{Name: "studio", Paired: seen, LastSeen: seen, Streams: []micserver.Stream{{Addr: "203.0.113.77:51234", Since: seen}}},
		{Name: "host-b", Paired: seen},
	}}
	c := app.Default()
	c.Capture.Mic, c.Capture.MicChannel = "hw:Array,0", 2
	c.MicServer.Name, c.MicServer.Listen = "micbox", ":7866"
	hp := micHandler(f, app.Loaded{Config: c, Path: "/etc/megavoice/config.toml"})
	h := func(r ctl.Request) ctl.Response { return hp(peer{UID: 0}, r) }
	run := func(req ctl.Request) string {
		t.Helper()
		var out strings.Builder
		if err := printMic(&out, req.Cmd, h(req)); err != nil {
			t.Fatalf("%s: %v", req.Cmd, err)
		}
		return out.String()
	}
	// the hint leads with this box's LAN address: a Mac whose resolver maps
	// the box's name elsewhere (a proxy's fake IP) still pairs by typing it
	lanIP = func() string { return "203.0.113.140" }
	if s := run(ctl.Request{Cmd: "pair"}); !strings.Contains(s, "12345678") || !strings.Contains(s, "02:00:00") ||
		!strings.Contains(s, "→ 203.0.113.140:7866") || !strings.Contains(s, "micbox:7866") {
		t.Errorf("pair:\n%s", s)
	}
	lanIP = func() string { return "" }
	if s := run(ctl.Request{Cmd: "pair"}); !strings.Contains(s, "→ micbox:7866") {
		t.Errorf("pair without a LAN address:\n%s", s)
	}
	s := run(ctl.Request{Cmd: "clients"})
	if !strings.Contains(s, "studio") || !strings.Contains(s, "203.0.113.77:51234") || !strings.Contains(s, "host-b") || !strings.Contains(s, "never") {
		t.Errorf("clients:\n%s", s)
	}
	s = run(ctl.Request{Cmd: "status"})
	for _, w := range []string{"micbox", ":7866", "hw:Array,0 ch 2", "/etc/megavoice/config.toml", "2 paired", "1 live"} {
		if !strings.Contains(s, w) {
			t.Errorf("status lacks %q:\n%s", w, s)
		}
	}
	if s := run(nameReq("revoke", "studio")); !strings.Contains(s, "studio") || len(f.revoked) != 1 {
		t.Errorf("revoke: %q %v", s, f.revoked)
	}
	var out strings.Builder
	if err := printMic(&out, "revoke", h(nameReq("revoke", "nobody"))); err == nil || !strings.Contains(err.Error(), "no paired client") {
		t.Errorf("revoke nobody: %v", err)
	}
	if r := h(ctl.Request{Cmd: "toggle"}); r.OK {
		t.Errorf("unknown command answered: %+v", r)
	}
}

// `mic token NAME` prints exactly one JSON line: what the Mac pins, with the
// uid the socket reported reaching the server's log.
func TestMicToken(t *testing.T) {
	f := &fakeMic{}
	c := app.Default()
	c.MicServer.Name, c.MicServer.Listen = "micbox", ":7866"
	h := micHandler(f, app.Loaded{Config: c})
	var out strings.Builder
	if err := printMic(&out, "token", h(peer{UID: 1001, GID: 100}, nameReq("token", "studio"))); err != nil {
		t.Fatal(err)
	}
	s := out.String()
	if strings.Count(s, "\n") != 1 || !strings.HasSuffix(s, "\n") {
		t.Fatalf("not one line: %q", s)
	}
	var got map[string]any
	if err := json.Unmarshal([]byte(s), &got); err != nil {
		t.Fatalf("%q: %v", s, err)
	}
	want := map[string]any{"box": "micbox", "client": "studio", "token": "tok-studio", "fingerprint": "ab12", "port": float64(7866)}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("token line %v, want %v", got, want)
	}
	if !slices.Equal(f.tokens, []string{"studio@1001"}) {
		t.Errorf("Token calls %v", f.tokens)
	}
	out.Reset()
	if err := printMic(&out, "token", h(peer{}, nameReq("token", "bad name"))); err == nil || out.Len() != 0 {
		t.Errorf("bad name: %v, printed %q", err, out.String())
	}
}

// The control socket admits root, the server's own user and members of the
// admin group — by the primary or a supplementary group — as the kernel
// reports the caller; anyone else gets one refusal line and nothing runs.
func TestCtlGate(t *testing.T) {
	g := ctlGate{self: 990, group: "megavoice-mic", gid: 991}
	for _, tc := range []struct {
		name string
		p    peer
		ok   bool
	}{
		{"root", peer{UID: 0, GID: 0}, true},
		{"the server", peer{UID: 990, GID: 990}, true},
		{"primary member", peer{UID: 1000, GID: 991}, true},
		{"supplementary member", peer{UID: 1001, GID: 100, Groups: []int{100, 27, 991}}, true},
		{"non-member", peer{UID: 1002, GID: 100, Groups: []int{100, 27}}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			sock := filepath.Join(t.TempDir(), "ctl.sock")
			l, err := listenCtl(sock, -1)
			if err != nil {
				t.Fatal(err)
			}
			defer l.Close()
			var got []int
			var mu sync.Mutex
			go serveCtl(l, func(net.Conn) (peer, error) { return tc.p, nil }, g, func(p peer, r ctl.Request) ctl.Response {
				mu.Lock()
				got = append(got, p.UID)
				mu.Unlock()
				return ctl.Reply(r.Cmd)
			}, t.Logf)
			resp, err := ctl.Call(sock, ctl.Request{Cmd: "token"}, 2*time.Second)
			if err != nil {
				t.Fatal(err)
			}
			mu.Lock()
			defer mu.Unlock()
			if resp.OK != tc.ok || (len(got) == 1) != tc.ok {
				t.Fatalf("%+v: response %+v, handler saw %v", tc.p, resp, got)
			}
			if tc.ok && got[0] != tc.p.UID {
				t.Errorf("handler saw uid %v, want %d", got, tc.p.UID)
			}
			if !tc.ok && !strings.Contains(resp.Error, "root or a member of group megavoice-mic") {
				t.Errorf("refusal %q", resp.Error)
			}
		})
	}
	// credentials the socket cannot report refuse too
	sock := filepath.Join(t.TempDir(), "ctl.sock")
	l, err := listenCtl(sock, -1)
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	go serveCtl(l, func(net.Conn) (peer, error) { return peer{}, errors.New("no peer credentials") }, g, func(peer, ctl.Request) ctl.Response { return ctl.Reply("ran") }, t.Logf)
	if resp, err := ctl.Call(sock, ctl.Request{Cmd: "status"}, 2*time.Second); err != nil || resp.OK {
		t.Errorf("no credentials: %+v %v", resp, err)
	}
}

// The socket is 0660 and the admin group's, so members connect where they
// can reach its directory; a directory others could write or a symlink to
// one is refused, since a local user could put their own socket there.
func TestListenCtl(t *testing.T) {
	dir := t.TempDir()
	gs, _ := os.Getgroups()
	gid := os.Getgid()
	for _, g := range gs { // a group of ours that is not the directory's default
		if g != gid {
			gid = g
			break
		}
	}
	sock := filepath.Join(dir, "run", "ctl.sock")
	l, err := listenCtl(sock, gid)
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close() // closing removes the socket
	st, err := os.Stat(sock)
	if err != nil {
		t.Fatal(err)
	}
	if st.Mode().Perm() != 0o660 || st.Mode()&os.ModeSocket == 0 {
		t.Errorf("socket mode %v, want a 0660 socket", st.Mode())
	}
	if st.Sys().(*syscall.Stat_t).Gid != uint32(gid) {
		t.Errorf("socket group %d, want %d", st.Sys().(*syscall.Stat_t).Gid, gid)
	}
	open := filepath.Join(dir, "open")
	if err := os.Mkdir(open, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(open, 0o777); err != nil {
		t.Fatal(err)
	}
	if _, err := listenCtl(filepath.Join(open, "ctl.sock"), -1); err == nil || !strings.Contains(err.Error(), "lets others replace") {
		t.Errorf("world-writable dir: %v", err)
	}
	link := filepath.Join(dir, "link")
	if err := os.Symlink(filepath.Join(dir, "run"), link); err != nil {
		t.Fatal(err)
	}
	if _, err := listenCtl(filepath.Join(link, "ctl.sock"), -1); err == nil || !strings.Contains(err.Error(), "not a directory") {
		t.Errorf("symlinked dir: %v", err)
	}
}
