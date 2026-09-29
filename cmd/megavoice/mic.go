//go:build darwin

package main

import (
	"bufio"
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"os"
	"regexp"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/caoer/mega-asr/internal/app"
	"github.com/caoer/mega-asr/internal/micserver"
)

// micPort is a mic server's port when HOST names none (mic_server.listen's
// default).
const micPort = "7866"

// micRemote is `megavoice mic add|scan|list|forget`: this Mac's side of a
// remote mic, paired over ssh when BatchMode ssh reaches the box, else by the
// PIN the box's `megavoice mic pair` shows.
func micRemote(o app.LoadOpts, args []string, in io.Reader, out io.Writer) error {
	if len(args) == 0 {
		fmt.Fprint(os.Stderr, usage)
		return exitCode(2)
	}
	path, _ := app.Path(o.Path)
	switch args[0] {
	case "add":
		host, name, err := parseAdd(args[1:])
		if err != nil {
			fmt.Fprintln(os.Stderr, "megavoice mic add HOST[:PORT] [--name N]:", err)
			return exitCode(2)
		}
		dest, port := splitDest(host)
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		r, err := sshPair(ctx, path, dest, port, name)
		cancel()
		var noSSH sshFailed
		switch {
		case err == nil:
			fmt.Fprintf(out, "paired with %s (%s) as %s over ssh; dictation records from it from the next take\n", r.Name, r.Addr, r.Client)
			return nil
		case !errors.As(err, &noSSH):
			return err
		}
		fmt.Fprintf(out, "not over ssh (%v): pairing by PIN\n", err)
		addr := pinAddr(context.Background(), dest, port)
		if err := micserver.Probe(context.Background(), addr); err != nil {
			return err // before the PIN: the box's PIN is single-use
		}
		fmt.Fprintf(out, "PIN (run `megavoice mic pair` on %s): ", hostOf(addr))
		line, _ := bufio.NewReader(in).ReadString('\n')
		ctx, cancel = context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		r, err = pairRemote(ctx, path, addr, strings.TrimSpace(line), name)
		if err != nil {
			return err
		}
		fmt.Fprintf(out, "paired with %s (%s) as %s; dictation records from it from the next take\n", r.Name, r.Addr, r.Client)
		return nil
	case "scan":
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		found, note, public := scanMics(ctx, sshConfigPath())
		return printFound(out, found, note, public)
	case "list":
		return micList(o, out)
	case "forget":
		if len(args) != 2 {
			fmt.Fprintln(os.Stderr, "megavoice mic forget NAME")
			return exitCode(2)
		}
		r, local, err := forgetRemote(path, args[1])
		if err != nil {
			return err
		}
		if local {
			fmt.Fprintln(out, "capture.source is local again")
		}
		fmt.Fprintf(out, "forgot %s; the box keeps this Mac paired until `megavoice mic revoke %s` there\n", r.Name, r.Client)
		return nil
	}
	fmt.Fprint(os.Stderr, usage)
	return exitCode(2)
}

// parseAdd reads `mic add`'s HOST[:PORT] and --name, before or after it.
func parseAdd(args []string) (host, name string, err error) {
	fs := flag.NewFlagSet("mic add", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	n := fs.String("name", clientName(), "this Mac's name on the box")
	if err := fs.Parse(args); err != nil {
		return "", "", err
	}
	if fs.NArg() == 0 {
		return "", "", errors.New("no HOST")
	}
	host = fs.Arg(0)
	if err := fs.Parse(fs.Args()[1:]); err != nil {
		return "", "", err
	}
	if fs.NArg() != 0 {
		return "", "", fmt.Errorf("unexpected %q", fs.Args())
	}
	return host, *n, nil
}

// pairRemote pairs this Mac as name with the mic server at addr, keeps the
// pairing in remotes.toml and makes it the capture source: `mic add` and
// the Input menu's "Add Remote Mic…".
func pairRemote(ctx context.Context, config, addr, pin, name string) (app.Remote, error) {
	if !pinRE.MatchString(pin) {
		return app.Remote{}, errors.New("the PIN is the 8 digits `megavoice mic pair` shows")
	}
	p, err := micserver.Pair(ctx, addr, pin, name)
	if err != nil {
		return app.Remote{}, err
	}
	r := app.Remote{Name: p.Box, Addr: addr, Fingerprint: p.Fingerprint, Token: p.Token, Client: name}
	return r, keepRemote(config, r)
}

// keepRemote keeps a pairing in remotes.toml and makes it the capture
// source; the box's name, which both files carry, must be a name.
func keepRemote(config string, r app.Remote) error {
	if !nameRE.MatchString(r.Name) {
		return fmt.Errorf("box name %q: %s", trim(r.Name, 70), nameRule)
	}
	if err := app.SaveRemote(app.RemotesPath(), r); err != nil {
		return err
	}
	for _, kv := range remotePick(r.Name) {
		if err := set(config, kv[0], kv[1]); err != nil {
			return err
		}
	}
	return nil
}

// printFound is `mic scan`: what Add Remote Mic… offers.
func printFound(out io.Writer, found []foundView, note string, public int) error {
	w := tabwriter.NewWriter(out, 0, 0, 2, ' ', 0)
	fmt.Fprintln(w, "NAME\tADDR\tPAIRS")
	for _, f := range found {
		how := "by PIN (megavoice mic pair there)"
		if f.SSH != "" {
			how = "over ssh " + f.SSH + ", else by PIN"
		}
		name := f.Name
		if f.Advertised != "" {
			name += " (advertised as " + f.Advertised + ")"
		}
		fmt.Fprintf(w, "%s\t%s\t%s\n", name, f.Addr, how)
	}
	if err := w.Flush(); err != nil {
		return err
	}
	if len(found) == 0 {
		fmt.Fprintln(out, "none found: no mic server advertised on the network, and no nearby ssh config Host answers on port "+micPort)
	}
	if public > 0 {
		fmt.Fprintf(out, "%d ssh config Hosts at public addresses not probed (only loopback, LAN, mesh and link-local ones are); megavoice mic add HOST pairs one\n", public)
	}
	if note != "" {
		fmt.Fprintln(out, note)
	}
	return nil
}

// forgetRemote drops the remote called name; when it is the capture
// source, the source goes back to local first, so the file stays valid.
func forgetRemote(config, name string) (r app.Remote, local bool, err error) {
	if r, err = pairedRemote(name); err != nil {
		return r, false, err
	}
	if l, err := load(app.LoadOpts{Path: config}); err == nil && l.Capture.Source == "remote" && l.Capture.Remote == name {
		if err := set(config, "capture.source", `"local"`); err != nil {
			return r, false, err
		}
		local = true
	}
	return r, local, app.ForgetRemote(app.RemotesPath(), name)
}

func micList(o app.LoadOpts, out io.Writer) error {
	rs, err := app.LoadRemotes(app.RemotesPath())
	if err != nil {
		return err
	}
	c := app.Default().Capture
	if l, err := load(o); err == nil {
		c = l.Capture
	}
	w := tabwriter.NewWriter(out, 0, 0, 2, ' ', 0)
	fmt.Fprintln(w, "USE\tNAME\tADDR\tAS\tCERTIFICATE")
	for _, r := range rs {
		use := "-"
		if c.Source == "remote" && c.Remote == r.Name {
			use = "Y"
		}
		fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%.16s…\n", use, r.Name, r.Addr, r.Client, r.Fingerprint)
	}
	if err := w.Flush(); err != nil {
		return err
	}
	if len(rs) == 0 {
		fmt.Fprintln(out, "none paired: megavoice mic add HOST, with the PIN `megavoice mic pair` shows on HOST")
	}
	return nil
}

var (
	pinRE    = regexp.MustCompile(`^[0-9]{8}$`)
	notNameC = regexp.MustCompile(`[^A-Za-z0-9._-]+`)
)

// clientName is this Mac's name on a box by default: the hostname's first
// label, in the characters a client name takes.
func clientName() string {
	h, _ := os.Hostname()
	h, _, _ = strings.Cut(h, ".")
	h = strings.Trim(notNameC.ReplaceAllString(h, "-"), "-._")
	if len(h) > 64 {
		h = h[:64]
	}
	if h == "" {
		return "mac"
	}
	return h
}

// withPort is host with micPort when it names no port.
func withPort(host string) string {
	if _, _, err := net.SplitHostPort(host); err == nil {
		return host
	}
	return net.JoinHostPort(strings.Trim(host, "[]"), micPort)
}

func hostOf(addr string) string {
	h, _, _ := net.SplitHostPort(addr)
	return h
}
