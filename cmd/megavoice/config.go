package main

import (
	"context"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"time"

	"github.com/caoer/mega-asr/internal/app"
	"github.com/caoer/mega-asr/internal/audio"
	"github.com/caoer/mega-asr/internal/mac"
)

// load resolves the shared config (internal/app) and checks what only the
// Mac can: the tap key.
func load(o app.LoadOpts) (app.Loaded, error) {
	o.Check = macCheck
	return app.Load(o)
}

func macCheck(c app.Config) []string {
	var errs []string
	if _, err := mac.LookupTapKey(c.Tap.Key); err != nil {
		errs = append(errs, "tap.key: "+err.Error())
	}
	if c.Capture.Source == "remote" && c.Capture.Remote != "" {
		if _, err := pairedRemote(c.Capture.Remote); err != nil {
			errs = append(errs, "capture.remote: "+err.Error())
		}
	}
	return errs
}

// pairedRemote is the remote mic called name in remotes.toml.
func pairedRemote(name string) (app.Remote, error) {
	rs, err := app.LoadRemotes(app.RemotesPath())
	if err != nil {
		return app.Remote{}, err
	}
	r, ok := app.FindRemote(rs, name)
	if !ok {
		return r, fmt.Errorf("no remote mic %q is paired (megavoice mic list; megavoice mic add HOST)", name)
	}
	return r, nil
}

// set writes one key of the config file, refused when serve could not start
// on the result.
func set(path, key, val string) error { return app.Set(path, key, val, macCheck) }

func stateDir() string {
	if d := os.Getenv("XDG_STATE_HOME"); d != "" {
		return filepath.Join(d, "megavoice")
	}
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".local", "state", "megavoice")
}

func sockPath() string { return filepath.Join(stateDir(), "ctl.sock") }

// chunkDir holds the chunk WAVs of takes on their way through the ASR.
func chunkDir() string { return filepath.Join(stateDir(), "chunks") }

// source builds each take's input from the config file as the take starts,
// so a pick in the menu or a `config set capture.*` applies at the next
// take without a restart; a file that no longer loads keeps c's capture.
func source(o app.LoadOpts, c app.Config) func() audio.Source {
	o.Check = nil // the tap key is serve's, checked at its start
	return func() audio.Source {
		cc := c.Capture
		if l, err := app.Load(o); err != nil {
			log.Printf("capture: %v; keeping serve's capture", err)
		} else {
			cc = l.Capture
		}
		return captureSource(cc)
	}
}

// backupSource builds a take's backup track (session.Controller.NewBackup)
// from capture.backup, read from the config file as the take starts: a
// second input of this Mac beside a local main input. The ssh and remote
// sources record on another machine, so this Mac has no second input to fall
// back to: they get no backup track, only the no-signal notice.
func backupSource(o app.LoadOpts, c app.Config) func(main audio.Source) audio.Source {
	o.Check = nil
	return func(main audio.Source) audio.Source {
		m, ok := main.(*audio.Mic)
		if !ok {
			return nil
		}
		uid := c.Capture.Backup
		if l, err := app.Load(o); err == nil {
			uid = l.Capture.Backup
		}
		if uid == "" || uid == "off" {
			return nil
		}
		return &audio.Mic{UID: uid, Not: m.Input().UID, Backup: true, Tail: 300 * time.Millisecond}
	}
}

// captureSource is the input capture.source names: this Mac's input, the
// array over ssh, or a paired remote mic.
func captureSource(c app.CaptureConfig) audio.Source {
	switch c.Source {
	case "ssh":
		return &audio.XVF{
			Host:    c.Host,
			Device:  c.Device,
			Channel: c.Channel,
			CtlPath: filepath.Join(stateDir(), "cm"),
			Tail:    300 * time.Millisecond,
		}
	case "remote":
		r, err := pairedRemote(c.Remote)
		if err != nil {
			return unstartable{fmt.Errorf("remote mic %s: %w", c.Remote, err)}
		}
		return &audio.Remote{Name: r.Name, Addr: r.Addr, Fingerprint: r.Fingerprint, Token: r.Token, Tail: 300 * time.Millisecond}
	}
	return &audio.Mic{UID: c.Mic, Name: c.MicName, Channel: c.MicChannel, Tail: 300 * time.Millisecond, Warm: time.Duration(c.BluetoothWarm)}
}

// unstartable is a source whose Start fails with err: a take that names a
// remote no longer paired.
type unstartable struct{ err error }

func (u unstartable) Start(context.Context) (<-chan []int16, error) { return nil, u.err }
func (unstartable) Stop()                                           {}
func (unstartable) Err() error                                      { return nil }
