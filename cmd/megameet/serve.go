package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/caoer/mega-asr/internal/app"
	"github.com/caoer/mega-asr/internal/audio"
	"github.com/caoer/mega-asr/internal/ctl"
	"github.com/caoer/mega-asr/internal/pages"
)

// lockFile holds the single-instance lock for the process life; an
// unreferenced *os.File would be closed by its finalizer, dropping the lock.
var lockFile *os.File

func lock(dir string) error {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	path := filepath.Join(dir, "serve.lock")
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return err
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		f.Close()
		return fmt.Errorf("another megameet serve holds %s", path)
	}
	lockFile = f
	return nil
}

func hostName() string {
	h, _ := os.Hostname()
	h, _, _ = strings.Cut(strings.ToLower(h), ".")
	if h == "" {
		h = "mac"
	}
	return h
}

// tapSources records apps through one process tap with the mic
// meeting.capture.mic names at that start: the config file is read again, so
// `megavoice config set meeting.capture.mic` (MegaVoice's mic picker) takes
// effect without a restart; an unreadable file keeps boot's mic (logged).
func tapSources(o app.LoadOpts, boot string) func(apps []string) Sources {
	return func(apps []string) Sources {
		mic := boot
		if l, err := app.Load(o); err != nil {
			log.Printf("start: config: %v; mic %q from serve's start", err, boot)
		} else {
			mic = l.Meeting.Capture.Mic
		}
		if mic == "default" {
			mic = ""
		}
		t := &audio.Tap{Apps: apps, MicUID: mic}
		return Sources{Remote: t.Remote(), Mic: t.Mic(), Info: t.Info}
	}
}

func serve(o app.LoadOpts) error {
	log.SetFlags(log.LstdFlags | log.Lmicroseconds)
	l, err := app.Load(o)
	if err != nil {
		return err
	}
	c := l.Meeting
	if err := lock(stateDir()); err != nil {
		return err
	}
	r := &recorder{
		dir:     filepath.Join(c.Data, "recordings"),
		host:    hostName(),
		sources: tapSources(o, c.Capture.Mic),
		now:     time.Now,
	}
	pend := ledger(filepath.Join(c.Data, "pending"))
	repaired, err := repair(r.dir)
	if err != nil {
		log.Printf("serve: repair: %v", err)
	}
	for _, dir := range repaired {
		if m, err := readMeta(dir); err == nil {
			err = pend.put(macJob(dir, m))
		}
		if err != nil {
			log.Printf("serve: ledger %s: %v", dir, err)
		}
	}
	// Every stopped recording enters the ledger; it goes to the page when
	// one is configured, now or at a later start.
	up := newUploader(c, pend, r.setUpload)
	r.onStop = func(dir string, m Meta) {
		if err := pend.put(macJob(dir, m)); err != nil {
			log.Printf("stop %s: ledger: %v", m.ID, err)
			return
		}
		if up != nil {
			go up.run(context.Background(), m.ID)
		}
	}
	resume := func() {
		if up != nil {
			if err := up.resume(context.Background()); err != nil {
				log.Printf("upload: %v", err)
			}
		}
		prune(r.dir, pend, c.Upload.RetentionDays, time.Now())
	}
	go resume()
	var reg *pages.Client
	if c.Page.Slug != "" {
		reg, _ = pages.FromConfig(c) // newUploader logged why not
	}
	ln, err := ctl.Listen(sockPath(), menuHandler(handler(r, c.Capture.Apps, func() { go resume() }), r, reg, c.Ingest.Wiki))
	if err != nil {
		return err
	}
	log.Printf("serve: %s, recordings in %s, default apps %v", sockPath(), r.dir, c.Capture.Apps)
	sig := make(chan os.Signal, 1)
	signal.Notify(sig, syscall.SIGTERM, syscall.SIGINT)
	s := <-sig
	ln.Close()
	if _, err := r.stop(); err == nil {
		log.Printf("serve: %v: recording stopped", s)
	}
	return nil
}

// newUploader is the page's uploader, or nil when no page is configured
// or its credential cannot be read (logged).
func newUploader(c app.MeetingConfig, l ledger, status func(id, s string)) *uploader {
	if c.Page.Slug == "" {
		log.Printf("upload: meeting.page.slug is empty; recordings stay in %s", l)
		return nil
	}
	cl, err := pages.FromConfig(c)
	if err != nil {
		log.Printf("upload: %v; recordings stay in %s", err, l)
		return nil
	}
	return &uploader{reg: cl, ledger: l, now: time.Now, status: status}
}

// handler answers the control socket: start, stop, status. onStart runs
// after each start (the ledger's resume).
func handler(r *recorder, defaultApps []string, onStart func()) func(ctl.Request) ctl.Response {
	return func(req ctl.Request) ctl.Response {
		switch req.Cmd {
		case "start":
			var a StartArgs
			if len(req.Args) > 0 {
				if err := json.Unmarshal(req.Args, &a); err != nil {
					return ctl.Fail(fmt.Errorf("start: %w", err))
				}
			}
			if len(a.Apps) == 0 {
				a.Apps = defaultApps
			}
			m, err := r.start(a)
			if err != nil {
				return ctl.Fail(err)
			}
			if onStart != nil {
				onStart()
			}
			return ctl.Reply(m)
		case "stop":
			m, err := r.stop()
			if err != nil {
				return ctl.Fail(err)
			}
			return ctl.Reply(struct {
				Meta
				Dir string `json:"dir"`
			}{m, filepath.Join(r.dir, m.ID)})
		case "status":
			return ctl.Reply(r.status())
		}
		return ctl.Fail(fmt.Errorf("unknown command %q", req.Cmd))
	}
}
