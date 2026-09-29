package main

// The Linux megavoice: `mic serve` records this host's input and serves it
// to paired Macs (internal/micserver); `mic pair|token|status|clients|revoke`
// ask the running server over its control socket, which admits root, the
// server's own user and members of mic_server.admin_group (ctlGate). Parsing, config lookup and the
// capture rule are here, build-tag free, so every platform tests them;
// main_other.go runs them.

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net"
	"os"
	"os/user"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/caoer/mega-asr/internal/app"
	"github.com/caoer/mega-asr/internal/audio"
	"github.com/caoer/mega-asr/internal/ctl"
	"github.com/caoer/mega-asr/internal/micserver"
)

const hostUsage = `usage: megavoice [--config FILE] [--set KEY=VALUE]... <command> [args]

  --config FILE             settings file (default: $MEGAVOICE_CONFIG, else
                            ~/.config/megavoice/config.toml, else
                            /etc/megavoice/config.toml, the system service's)
  --set section.key=value   override one setting for this run; repeatable

  mic serve                 record [capture] and serve it to paired Macs on
                            mic_server.listen (the service runs this)
  mic pair                  print a single-use PIN for a Mac to pair with:
                            MegaVoice › Input › Add remote mic…
  mic token NAME            pair the Mac NAME and print its token as one JSON
                            line (a Mac runs this over ssh); re-pairs a NAME
                            already paired
  mic status                the running server: address, name, capture, clients
  mic clients               each paired Mac: paired, last seen, live streams
  mic revoke NAME           unpair a Mac and end its live streams
                            (pair, token, status, clients and revoke take root
                            or a member of mic_server.admin_group)
  config show | check [FILE] | init [FILE]
                            the effective settings and their sources; validate
                            a file; write the commented default config
`

// systemConfig is the file the system service runs with (a host's install
// module, packaging/megavoice-mic.service's README).
const systemConfig = "/etc/megavoice/config.toml"

// hostTables are the config tables the Linux build reads.
var hostTables = []string{"capture", "mic_server"}

// hostCmd is one Linux command: group (mic, config), verb, and its one
// argument (revoke's and token's NAME, check's and init's FILE).
type hostCmd struct{ Group, Verb, Arg string }

var errUsage = errors.New("usage")

func parseHost(args []string) (hostCmd, error) {
	if len(args) < 2 {
		return hostCmd{}, errUsage
	}
	c := hostCmd{Group: args[0], Verb: args[1]}
	rest := args[2:]
	var ok bool
	switch c.Group {
	case "mic":
		switch c.Verb {
		case "serve", "pair", "status", "clients":
			ok = len(rest) == 0
		case "revoke", "token":
			ok = len(rest) == 1 && rest[0] != ""
		}
	case "config":
		switch c.Verb {
		case "show":
			ok = len(rest) == 0
		case "check", "init":
			ok = len(rest) <= 1
		}
	}
	if !ok {
		return hostCmd{}, errUsage
	}
	if len(rest) == 1 {
		c.Arg = rest[0]
	}
	return c, nil
}

// hostConfigPath is the file to read: --config, else MEGAVOICE_CONFIG, else
// the user's config.toml when it exists, else the system service's when it
// does. "" reads the defaults. A user with no file of their own — root
// under sudo — so reads the service's file and finds its socket.
func hostConfigPath(flag string, exists func(string) bool) string {
	path, explicit := app.Path(flag)
	switch {
	case explicit, exists(path):
		return path
	case exists(systemConfig):
		return systemConfig
	}
	return ""
}

func fileExists(p string) bool { _, err := os.Stat(p); return err == nil }

// hostCheck is what the Linux build adds to the config's own validation.
func hostCheck(c app.Config) []string {
	var errs []string
	switch c.Capture.Source {
	case "local", "ssh":
	default:
		errs = append(errs, fmt.Sprintf("capture.source: %s is a Mac's; a mic server records local or ssh", c.Capture.Source))
	}
	if c.Capture.Source == "local" && c.Capture.MicChannel > audio.Channels {
		errs = append(errs, fmt.Sprintf("capture.mic_channel: %d is outside 0..%d (0 is mono, n a channel of the XVF3800's six)", c.Capture.MicChannel, audio.Channels))
	}
	return errs
}

func hostLoad(o app.LoadOpts) (app.Loaded, error) {
	o.Path = hostConfigPath(o.Path, fileExists)
	o.Check = hostCheck
	return app.Load(o)
}

// hostSource is one stream's input: arecord on this machine for local
// (capture.mic the PCM, mic_channel 0 mono or n of the XVF3800's six), or
// over ssh; the server opens one per connection.
func hostSource(c app.Config) audio.Source {
	cc := c.Capture
	if cc.Source == "ssh" {
		return &audio.XVF{Host: cc.Host, Device: cc.Device, Channel: cc.Channel, CtlPath: filepath.Join(c.MicServer.State, "cm")}
	}
	return &audio.XVF{Device: cc.Mic, Channel: cc.MicChannel}
}

// captureLabel names the input in logs and status: "hw:Array,0 ch 2", "default mono".
func captureLabel(c app.CaptureConfig) string {
	if c.Source == "ssh" {
		return fmt.Sprintf("%s:%s ch %d", c.Host, c.Device, c.Channel)
	}
	if c.MicChannel == 0 {
		return c.Mic + " mono"
	}
	return fmt.Sprintf("%s ch %d", c.Mic, c.MicChannel)
}

// hostConfig runs config show, check and init.
func hostConfig(w io.Writer, o app.LoadOpts, c hostCmd) error {
	switch c.Verb {
	case "show":
		l, err := hostLoad(o)
		if err != nil {
			return err
		}
		l.WriteEffective(w, "megavoice", hostTables...)
		return nil
	case "check":
		if c.Arg != "" {
			o = app.LoadOpts{Path: c.Arg}
		}
		l, err := hostLoad(o)
		if err != nil {
			return err
		}
		if l.Path == "" {
			fmt.Fprintln(w, "no config file; the defaults are valid")
			return nil
		}
		fmt.Fprintf(w, "%s: ok\n", l.Path)
		return nil
	case "init":
		path, _ := app.Path(o.Path)
		if c.Arg != "" {
			path = c.Arg
		}
		if path == "-" {
			_, err := w.Write(app.Template())
			return err
		}
		if _, err := os.Stat(path); err == nil {
			return fmt.Errorf("%s exists; kept as it is", path)
		} else if !errors.Is(err, fs.ErrNotExist) {
			return err
		}
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			return err
		}
		if err := os.WriteFile(path, app.Template(), 0o644); err != nil {
			return err
		}
		fmt.Fprintln(w, path)
		return nil
	}
	return errUsage
}

// micControl is what the control socket asks of the running server.
type micControl interface {
	NewPIN() (micserver.PIN, error)
	Clients() []micserver.Client
	Revoke(name string) error
	Token(name string, uid int) (string, error)
	Fingerprint() string
}

// peer is a control-socket caller as the kernel reports it (peerCred:
// SO_PEERCRED and SO_PEERGROUPS on Linux), never as the caller says.
type peer struct {
	UID, GID int
	Groups   []int // supplementary groups
}

// ctlGate is who may use the control socket: root, the server's own user
// (who can read its key anyway) and members of the admin group.
type ctlGate struct {
	self  int    // the server's uid
	group string // mic_server.admin_group
	gid   int    // its gid; -1 when the group does not exist
}

func newCtlGate(group string) (ctlGate, error) {
	g := ctlGate{self: os.Getuid(), group: group, gid: -1}
	gr, err := user.LookupGroup(group)
	if err != nil {
		return g, err
	}
	g.gid, err = strconv.Atoi(gr.Gid)
	if err != nil {
		g.gid = -1
	}
	return g, err
}

func (g ctlGate) admit(p peer) error {
	if p.UID == 0 || p.UID == g.self || (g.gid >= 0 && (p.GID == g.gid || slices.Contains(p.Groups, g.gid))) {
		return nil
	}
	return fmt.Errorf("uid %d refused: the mic server's control socket takes root or a member of group %s", p.UID, g.group)
}

// listenCtl makes the control socket at path, mode 0660 and, for gid >= 0,
// owned by that group, so the admin group can connect where it can reach
// the directory. A stale socket is removed: the caller checked none answers.
// The directory must be root's or this user's and writable by no one else,
// else another local user could replace the socket with their own.
func listenCtl(path string, gid int) (net.Listener, error) {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	if err := safeDir(dir); err != nil {
		return nil, err
	}
	_ = os.Remove(path)
	l, err := net.Listen("unix", path)
	if err != nil {
		return nil, err
	}
	err = os.Chmod(path, 0o660)
	if err == nil && gid >= 0 {
		err = os.Chown(path, -1, gid)
	}
	if err != nil {
		l.Close()
		return nil, err
	}
	return l, nil
}

// safeDir refuses a socket directory that is a symlink, or is not root's or
// this user's, or that group or others may write.
func safeDir(dir string) error {
	st, err := os.Lstat(dir)
	if err != nil {
		return err
	}
	if !st.IsDir() {
		return fmt.Errorf("%s: not a directory", dir)
	}
	if st.Mode().Perm()&0o022 != 0 {
		return fmt.Errorf("%s: mode %v lets others replace the control socket; want 0700 or 0750", dir, st.Mode().Perm())
	}
	if uid, ok := fileUID(st); ok && uid != 0 && uid != os.Getuid() {
		return fmt.Errorf("%s: owned by uid %d, not root or this user (%d)", dir, uid, os.Getuid())
	}
	return nil
}

// serveCtl answers the control socket on l until it closes: each caller's
// credentials come from cred, and a caller g does not admit gets one
// refusal and runs nothing.
func serveCtl(l net.Listener, cred func(net.Conn) (peer, error), g ctlGate, h func(peer, ctl.Request) ctl.Response, logf func(string, ...any)) {
	for {
		c, err := l.Accept()
		if err != nil {
			return
		}
		go func() {
			_ = c.SetDeadline(time.Now().Add(10 * time.Second))
			p, err := cred(c)
			if err == nil {
				err = g.admit(p)
			}
			if err != nil {
				logf("control socket: %v", err)
				ctl.ServeConn(c, func(ctl.Request) ctl.Response { return ctl.Fail(err) })
				return
			}
			ctl.ServeConn(c, func(r ctl.Request) ctl.Response { return h(p, r) })
		}()
	}
}

// tokenReply is `mic token`'s one line: what a Mac pins for this box.
type tokenReply struct {
	Box         string `json:"box"`
	Client      string `json:"client"`
	Token       string `json:"token"`
	Fingerprint string `json:"fingerprint"`
	Port        int    `json:"port"`
}

// pairReply is a PIN and the address a Mac types beside it: IP, this box's
// LAN address, when it has one, and Addr, its name.
type pairReply struct {
	micserver.PIN
	IP   string `json:"ip,omitempty"`
	Addr string `json:"addr"`
}

// lanIP is the address of the interface this box's default route leaves by:
// the one a Mac on the same LAN reaches. A name can resolve elsewhere on the
// Mac (a proxy with fake-IP DNS maps it to an address that accepts and drops), so
// the hint leads with this. The UDP dial sends nothing.
var lanIP = func() string {
	c, err := net.Dial("udp4", "192.0.2.1:9")
	if err != nil {
		return ""
	}
	defer c.Close()
	return c.LocalAddr().(*net.UDPAddr).IP.String()
}

// micStatus is `mic status`: the server as it runs.
type micStatus struct {
	Name    string             `json:"name"`
	Listen  string             `json:"listen"`
	Capture string             `json:"capture"`
	Config  string             `json:"config"`
	Clients []micserver.Client `json:"clients"`
}

// nameReq is revoke's or token's request for a client name.
func nameReq(cmd, name string) ctl.Request {
	b, _ := json.Marshal(map[string]string{"name": name})
	return ctl.Request{Cmd: cmd, Args: b}
}

// micHandler answers the control socket's pair, token, status, clients and
// revoke for a caller serveCtl admitted.
func micHandler(s micControl, l app.Loaded) func(peer, ctl.Request) ctl.Response {
	name := l.MicServer.HostName()
	return func(p peer, r ctl.Request) ctl.Response {
		switch r.Cmd {
		case "token":
			var a struct {
				Name string `json:"name"`
			}
			if err := json.Unmarshal(r.Args, &a); err != nil || a.Name == "" {
				return ctl.Fail(errors.New("token: no client name"))
			}
			tok, err := s.Token(a.Name, p.UID)
			if err != nil {
				return ctl.Fail(err)
			}
			_, port, _ := net.SplitHostPort(l.MicServer.Listen)
			n, _ := strconv.Atoi(port)
			return ctl.Reply(tokenReply{Box: name, Client: a.Name, Token: tok, Fingerprint: s.Fingerprint(), Port: n})
		case "pair":
			p, err := s.NewPIN()
			if err != nil {
				return ctl.Fail(err)
			}
			_, port, _ := net.SplitHostPort(l.MicServer.Listen)
			r := pairReply{PIN: p, Addr: net.JoinHostPort(name, port)}
			if ip := lanIP(); ip != "" {
				r.IP = net.JoinHostPort(ip, port)
			}
			return ctl.Reply(r)
		case "status":
			return ctl.Reply(micStatus{Name: name, Listen: l.MicServer.Listen, Capture: captureLabel(l.Capture), Config: l.Path, Clients: s.Clients()})
		case "clients":
			return ctl.Reply(s.Clients())
		case "revoke":
			var a struct {
				Name string `json:"name"`
			}
			if err := json.Unmarshal(r.Args, &a); err != nil || a.Name == "" {
				return ctl.Fail(errors.New("revoke: no client name"))
			}
			if err := s.Revoke(a.Name); err != nil {
				return ctl.Fail(fmt.Errorf("%s: %w", a.Name, err))
			}
			return ctl.Reply(a.Name)
		}
		return ctl.Fail(fmt.Errorf("unknown command %q", r.Cmd))
	}
}

// printMic prints a control reply as the user reads it.
func printMic(w io.Writer, cmd string, r ctl.Response) error {
	if !r.OK {
		return errors.New(r.Error)
	}
	switch cmd {
	case "token":
		var t tokenReply
		if err := json.Unmarshal(r.Data, &t); err != nil {
			return err
		}
		b, err := json.Marshal(t)
		if err != nil {
			return err
		}
		_, err = w.Write(append(b, '\n'))
		return err
	case "pair":
		var p pairReply
		if err := json.Unmarshal(r.Data, &p); err != nil {
			return err
		}
		fmt.Fprintf(w, "PIN %s — once, until %s (%s)\n", p.Code, p.Expires.Local().Format("15:04:05"), micserver.PINLife)
		if p.IP != "" {
			fmt.Fprintf(w, "On the Mac: MegaVoice › Input › Add remote mic… → %s (or %s where that name reaches this box), then the PIN.\n", p.IP, p.Addr)
		} else {
			fmt.Fprintf(w, "On the Mac: MegaVoice › Input › Add remote mic… → %s, then the PIN.\n", p.Addr)
		}
	case "status":
		var st micStatus
		if err := json.Unmarshal(r.Data, &st); err != nil {
			return err
		}
		live := 0
		for _, c := range st.Clients {
			live += len(c.Streams)
		}
		fmt.Fprintf(w, "%s on %s, capture %s, config %s\n", st.Name, st.Listen, firstNonEmpty(st.Capture, "?"), firstNonEmpty(st.Config, "none (defaults)"))
		fmt.Fprintf(w, "%d paired, %d live stream(s)\n", len(st.Clients), live)
		if len(st.Clients) > 0 {
			writeClients(w, st.Clients)
		}
	case "clients":
		var cs []micserver.Client
		if err := json.Unmarshal(r.Data, &cs); err != nil {
			return err
		}
		if len(cs) == 0 {
			fmt.Fprintln(w, "no paired Macs; `megavoice mic pair` prints a PIN for one")
			return nil
		}
		writeClients(w, cs)
	case "revoke":
		var name string
		_ = json.Unmarshal(r.Data, &name)
		fmt.Fprintf(w, "revoked %s: unpaired, its live streams closed\n", name)
	}
	return nil
}

func writeClients(w io.Writer, cs []micserver.Client) {
	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "NAME\tPAIRED\tLAST SEEN\tLIVE")
	for _, c := range cs {
		var live []string
		for _, st := range c.Streams {
			live = append(live, st.Addr+" since "+stamp(st.Since))
		}
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\n", c.Name, stamp(c.Paired), stamp(c.LastSeen), firstNonEmpty(strings.Join(live, ", "), "-"))
	}
	_ = tw.Flush()
}

// stamp is a time as the log shows it; "never" for the zero time.
func stamp(t time.Time) string {
	if t.IsZero() {
		return "never"
	}
	return t.Local().Format("2006-01-02 15:04:05")
}

func firstNonEmpty(s ...string) string {
	for _, v := range s {
		if v != "" {
			return v
		}
	}
	return ""
}
