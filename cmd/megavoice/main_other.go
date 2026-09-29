//go:build !darwin

// megavoice off macOS is the paired mic server (host.go): `mic serve`
// records this host's input for the Macs paired with it, and the other
// commands ask it over its control socket.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io/fs"
	"log"
	"net"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/caoer/mega-asr/internal/app"
	"github.com/caoer/mega-asr/internal/audio"
	"github.com/caoer/mega-asr/internal/ctl"
	"github.com/caoer/mega-asr/internal/micserver"
)

func main() {
	var o app.LoadOpts
	gfs := flag.NewFlagSet("megavoice", flag.ContinueOnError)
	gfs.Usage = func() { fmt.Fprint(os.Stderr, hostUsage) }
	gfs.StringVar(&o.Path, "config", "", "settings file")
	gfs.Func("set", "section.key=value", func(s string) error { o.Sets = append(o.Sets, s); return nil })
	if err := gfs.Parse(os.Args[1:]); err != nil {
		os.Exit(2)
	}
	c, err := parseHost(gfs.Args())
	if err == nil {
		if c.Group == "config" {
			err = hostConfig(os.Stdout, o, c)
		} else {
			err = micCmd(o, c)
		}
	}
	if errors.Is(err, errUsage) {
		fmt.Fprint(os.Stderr, hostUsage)
		os.Exit(2)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "megavoice:", err)
		os.Exit(1)
	}
}

func micCmd(o app.LoadOpts, c hostCmd) error {
	l, err := hostLoad(o)
	if err != nil {
		return err
	}
	if c.Verb == "serve" {
		return micServe(l)
	}
	sock := l.MicServer.CtlPath()
	req := ctl.Request{Cmd: c.Verb}
	if c.Arg != "" {
		req = nameReq(c.Verb, c.Arg)
	}
	resp, err := ctl.Call(sock, req, 5*time.Second)
	if errors.Is(err, ctl.ErrNotRunning) {
		if c, derr := net.Dial("unix", sock); derr == nil {
			c.Close()
		} else if errors.Is(derr, fs.ErrPermission) {
			return fmt.Errorf("%s: permission denied; the control socket takes root (sudo) or a member of group %s", sock, l.MicServer.AdminGroup)
		}
		return fmt.Errorf("no mic server answers on %s (config %s): is `megavoice mic serve` running?", sock, firstNonEmpty(l.Path, "none"))
	}
	if err != nil {
		return err
	}
	return printMic(os.Stdout, c.Verb, resp)
}

// micServe runs the server until SIGINT or SIGTERM. Its capture is fixed at
// start: a config change takes a restart.
func micServe(l app.Loaded) error {
	cfg := l.Config
	ms := cfg.MicServer
	sock := ms.CtlPath()
	if _, err := ctl.Call(sock, ctl.Request{Cmd: "status"}, time.Second); err == nil {
		return fmt.Errorf("a mic server already answers on %s", sock)
	}
	log.Printf("mic serve: %s on %s, capture %s, config %s, state %s", ms.HostName(), ms.Listen, captureLabel(cfg.Capture), firstNonEmpty(l.Path, "none"), ms.State)
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	probe(ctx, cfg)
	s, err := micserver.New(micserver.Config{
		Listen: ms.Listen,
		State:  ms.State,
		Name:   ms.HostName(),
		Source: func() audio.Source { return hostSource(cfg) },
		Logf:   log.Printf,
	})
	if err != nil {
		return err
	}
	gate, err := newCtlGate(ms.AdminGroup)
	if err != nil {
		log.Printf("admin group %s: %v; the control socket takes root and uid %d only", ms.AdminGroup, err, gate.self)
	}
	ln, err := listenCtl(sock, gate.gid)
	if err != nil {
		return fmt.Errorf("control socket: %w", err)
	}
	defer ln.Close()
	go serveCtl(ln, peerCred, gate, micHandler(s, l), log.Printf)
	return s.Serve(ctx)
}

// probe reads the capture for half a second at start and logs what it got,
// so the service's log says whether the mic works before any Mac asks; a
// failure is logged and serving goes on (the device may come back).
func probe(ctx context.Context, cfg app.Config) {
	src := hostSource(cfg)
	ch, err := src.Start(ctx)
	if err != nil {
		log.Printf("capture %s: %v", captureLabel(cfg.Capture), err)
		return
	}
	var got []int16
	timer := time.AfterFunc(500*time.Millisecond, src.Stop)
	defer timer.Stop()
	for s := range ch {
		got = append(got, s...)
	}
	if err := src.Err(); err != nil || len(got) == 0 {
		log.Printf("capture %s: no audio: %v", captureLabel(cfg.Capture), err)
		return
	}
	rms, peak := audio.Stats(got)
	log.Printf("capture %s: ok, %d samples in 0.5 s, rms %.0f dBFS, peak %.0f dBFS", captureLabel(cfg.Capture), len(got), rms, peak)
}
