//go:build darwin

package main

import (
	"errors"
	"flag"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/caoer/mega-asr/internal/app"
	"github.com/caoer/mega-asr/internal/asr"
	"github.com/caoer/mega-asr/internal/audio"
	"github.com/caoer/mega-asr/internal/ctl"
	"github.com/caoer/mega-asr/internal/deliver"
	"github.com/caoer/mega-asr/internal/mac"
	"github.com/caoer/mega-asr/internal/session"
	"github.com/caoer/mega-asr/internal/store"
	"github.com/caoer/mega-asr/internal/takes"
)

type status struct {
	State        string     `json:"state"`
	Build        string     `json:"build"`
	Grants       mac.Grants `json:"grants"`
	TapInstalled bool       `json:"tap_installed"`
	TapKey       string     `json:"tap_key"`
	Taps         int64      `json:"taps"`
	Executable   string     `json:"executable"`
	PID          int        `json:"pid"`
	Data         string     `json:"data"`
	Frontmost    mac.App    `json:"frontmost"`

	// RestartPending: restart --when-idle waits for nothing in flight and
	// no key for quietFor
	RestartPending bool `json:"restart_pending,omitempty"`
}

type tapEvent struct {
	At     time.Time `json:"at"`
	Action string    `json:"action"`
}

type server struct {
	cfg  app.Config
	opts app.LoadOpts
	flag serveFlags
	key  mac.TapKey
	ctrl *session.Controller
	dlv  deliver.Deliverer // the controller's, for the targets a resend names
	ov   *mac.Overlay
	taps chan tapIn

	mu     sync.Mutex
	tap    *mac.Tap
	events []tapEvent

	cmds        *takeCommands
	takes       *takes.Handler // the Takes page, whose index the menu's count reads
	undelivered atomic.Int64   // the takes undelivered and not seen, every 5 s
	panel       atomic.Pointer[panelState]
}

// panelState is the loopback listener servePanel holds, or why it holds none.
type panelState struct {
	addr string // the address bound for compare.addr, once this process holds its listener
	err  error  // why it does not
}

// lockFile holds the single-instance lock for the process life; an
// unreferenced *os.File would be closed by its finalizer, dropping the lock.
var lockFile *os.File

// lock takes the single-instance lock.
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
		return fmt.Errorf("another megavoice serve holds %s", path)
	}
	lockFile = f
	return nil
}

// serveFlags are serve's own flags, for a scratch instance run for checks.
type serveFlags struct {
	noTap      bool   // no key tap and no Accessibility prompt
	file       string // every take replays this WAV in place of capture.source
	backupFile string // every take's backup track replays this WAV in place of capture.backup
	pane       string // every take delivers to this herdr pane in place of what is frontmost
}

func parseServe(args []string) (serveFlags, error) {
	var f serveFlags
	fs := flag.NewFlagSet("serve", flag.ContinueOnError)
	fs.Usage = func() {
		fmt.Fprint(os.Stderr, "usage: megavoice serve [--no-tap] [--file WAV] [--backup-file WAV] [--pane ID]\n\n"+
			"The four flags are for a scratch instance run for checks, with its own\n"+
			"XDG_STATE_HOME, XDG_DATA_HOME, XDG_CONFIG_HOME and compare.addr:\n\n")
		fs.PrintDefaults()
	}
	fs.BoolVar(&f.noTap, "no-tap", false, "install no key tap and ask for no Accessibility grant; drive it with toggle and cancel")
	fs.StringVar(&f.file, "file", "", "every take replays this 16 kHz WAV at real time in place of capture.source")
	fs.StringVar(&f.backupFile, "backup-file", "", "every take's backup track replays this 16 kHz WAV at real time in place of capture.backup")
	fs.StringVar(&f.pane, "pane", "", "every take delivers to this herdr pane in place of what is frontmost")
	if err := fs.Parse(args); err != nil {
		return f, exitCode(2)
	}
	if fs.NArg() > 0 {
		fs.Usage()
		return f, exitCode(2)
	}
	return f, nil
}

func serve(o app.LoadOpts, args []string) error {
	sf, err := parseServe(args)
	if err != nil {
		return err
	}
	log.SetFlags(log.LstdFlags | log.Lmicroseconds)
	l, err := load(o)
	if err != nil {
		return err
	}
	cfg := l.Config
	if l.Path == "" {
		log.Printf("serve: config: defaults (no %s)", filepath.Join(app.ConfigDir(), "config.toml"))
	} else {
		log.Printf("serve: config: %s", l.Path)
	}
	key, err := mac.LookupTapKey(cfg.Tap.Key)
	if err != nil {
		return err
	}
	if err := lock(stateDir()); err != nil {
		return err
	}
	app.Seed()
	s := &server{cfg: cfg, opts: o, flag: sf, key: key, taps: make(chan tapIn, 16)}
	mac.Run(s.ready)
	return nil
}

func (s *server) ready() {
	exe, _ := os.Executable()
	g := mac.CheckGrants()
	log.Printf("serve: pid %d, %s, tap key %s, data %s, grants %+v", os.Getpid(), exe, s.key.Name, s.cfg.Store.Data, g)
	s.ov = mac.NewOverlay()
	if s.cfg.Capture.Source == "local" && mac.MicrophoneAuth() == mac.MicNotDetermined {
		// ask for the Microphone grant now, not at the first take
		go func() {
			err := audio.PrimeMic()
			log.Printf("serve: microphone grant asked at launch: %v; now %v", err, mac.MicrophoneAuth())
		}()
	}
	tr := s.cfg.Transcriber()
	if fb, ok := tr.(*app.Fallback); ok {
		go fb.Warm(stateDir())
	}
	f := s.cfg.ASR.FunASR
	if s.cfg.ASR.Engine == "doubao" {
		d := s.cfg.ASR.Doubao
		log.Printf("serve: asr doubao (%s, two-pass %v, ddc %v, hotwords %v); funasr stands in when a take's stream fails", d.URL, d.TwoPass, d.DDC, d.Hotwords)
	}
	log.Printf("serve: asr funasr %s (llm %s, gpu %d, enc %s, vad %s, threads %d), takes up to %v decoded whole, longer ones in chunks at %v pauses, %v max; takes stop after %v",
		f.Mode, asr.LLMPath(f.Root, f.LLM), f.GPULayers, f.Encoder, f.VAD, f.Threads, s.cfg.Take.WholeMax, s.cfg.Take.ChunkPause, s.cfg.Take.ChunkMax, s.cfg.Take.MaxDuration)
	newSource := source(s.opts, s.cfg)
	if s.flag.file != "" {
		newSource = func() audio.Source { return &audio.File{Path: s.flag.file} }
	}
	newBackup := backupSource(s.opts, s.cfg)
	if s.flag.backupFile != "" {
		newBackup = func(audio.Source) audio.Source { return &audio.File{Path: s.flag.backupFile} }
	}
	engine, model := s.cfg.ASR.Engine, filepath.Base(asr.LLMPath(f.Root, f.LLM))
	if engine == "doubao" {
		model = s.cfg.ASR.Doubao.ResourceID
	}
	s.dlv = deliver.Deliverer{
		Pane:      s.flag.pane,
		Herdr:     deliver.Herdr{Bin: "herdr"},
		HerdrApps: s.cfg.Deliver.HerdrApps,
		FocusWait: time.Second,
		Settle:    150 * time.Millisecond,
	}
	s.ctrl = &session.Controller{
		Engine:      engine,
		Model:       model,
		ChunkEngine: "funasr",
		Build:       app.BuildID(),
		NewSource:   newSource,
		NewBackup:   newBackup,
		ASR:         tr,
		Stream:      s.cfg.Streamer(),
		Tee:         s.cfg.Fanout(s.opts, tr).Tee,
		Post:        s.cfg.Post(),
		Deliver:     s.dlv,
		UI:          &overlayUI{ov: s.ov, after: time.AfterFunc},
		Store:       store.Store{Dir: s.cfg.Store.Data},
		NewChunker:  s.cfg.Chunker,
		ChunkDir:    chunkDir(),
		MaxDuration: time.Duration(s.cfg.Take.MaxDuration),
		WholeMax:    time.Duration(s.cfg.Take.WholeMax),
		Sound:       mac.Beep,
		HotkeyKey:   chordKey(s.key.Name),
		OnState: func(st session.State) {
			s.mu.Lock()
			defer s.mu.Unlock()
			if s.tap != nil {
				s.tap.SetPhase(phaseOf(st))
			}
		},
	}
	s.cmds = &takeCommands{ctrl: s.ctrl, flash: s.ctrl.UI.Flash, exit: os.Exit, lastKey: s.lastKey}
	sig := make(chan os.Signal, 1)
	signal.Notify(sig, syscall.SIGTERM, syscall.SIGINT)
	go onSignal(sig, s.ctrl, signalWait, os.Exit)
	s.takes = takes.New(s.cfg.Store.Data, s.cfg.Labels(), s.cfg.TakesEngines(s.opts))
	go s.countUndelivered()
	s.menu().show()
	if !g.Accessibility && !s.flag.noTap {
		mac.PromptAccessibility()
		log.Printf("serve: Accessibility is off — System Settings › Privacy & Security › Accessibility › MegaVoice")
		s.ov.Flash("MegaVoice needs Accessibility — System Settings › Privacy & Security", 6*time.Second)
	}
	s.ctrl.Recover()
	if _, err := ctl.Listen(sockPath(), s.handle); err != nil {
		log.Fatalf("serve: control socket: %v", err)
	}
	go s.dispatch(s.ctrl)
	if s.flag.noTap {
		log.Printf("serve: --no-tap: no key tap; drive it with toggle and cancel")
	} else {
		go s.installTap()
	}
	s.servePanel()
}

// servePanel serves the Takes page under /takes/ and the redirect to it at
// /, behind the guard, and returns the listener. The key is rotated first, at
// every start: while megavoice was down another process may have held the
// port and read the key from an open tab's requests, so no address handed
// out before opens the pages. A busy address, or a key that cannot be
// rotated, leaves the rest of the agent running and returns nil, and the
// menu and `megavoice takes url` hand out no key. The address handed out is
// the one bound, an IP literal, so a browser reaches no other listener.
func (s *server) servePanel() net.Listener {
	addr, kf := s.cfg.Compare.Addr, keyFile()
	if _, err := kf.Rotate(); err != nil {
		err = fmt.Errorf("rotating the key: %w", err)
		s.panel.Store(&panelState{err: err})
		log.Printf("serve: Takes page: %v; this process serves nothing on %s, so the menu and `megavoice takes url` hand out no key", err, addr)
		return nil
	}
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		s.panel.Store(&panelState{err: err})
		log.Printf("serve: Takes page: %v; this process holds no listener on %s, so the menu and `megavoice takes url` hand out no key, and the key is rotated: no address handed out before opens the pages", err, addr)
		return nil
	}
	th := s.takes
	if th == nil {
		th = takes.New(s.cfg.Store.Data, s.cfg.Labels(), s.cfg.TakesEngines(s.opts))
	}
	if s.ctrl != nil {
		th.Ctrl, th.Resolve, th.Engine = s.ctrl, s.dlv.Resolve, s.cfg.Retranscriber(s.opts)
		th.Panes, th.Apps = s.panes, appsFrom(mac.RunningApps)
	}
	bound := ln.Addr().String()
	log.Printf("serve: the Takes page on http://%s/takes/, labels in %s", bound, s.cfg.Store.Labels)
	s.panel.Store(&panelState{addr: bound})
	go func() {
		// http.Serve closes ln when it returns, and the port is free for
		// another process: the key is rotated, as after a failed bind
		err := http.Serve(ln, guard(th, ln))
		s.panel.Store(&panelState{err: err})
		if _, rerr := kf.Rotate(); rerr != nil {
			err = fmt.Errorf("%w; rotating the key: %v", err, rerr)
			s.panel.Store(&panelState{err: err})
		}
		log.Printf("serve: Takes page: %v; this process holds no listener on %s any more, so the menu and `megavoice takes url` hand out no key", err, bound)
	}()
	return ln
}

// menu is the status item's menu over this agent: its take, its count of
// undelivered takes, and the listener it holds.
func (s *server) menu() *meetMenu {
	return meetMenuOf(s.opts, s.cfg, func() voiceTake {
		id, d, rec := s.ctrl.Recording()
		return voiceTake{ID: id, Recording: rec, Elapsed: d}
	}, func() int { return int(s.undelivered.Load()) }, s.listening)
}

// listening is the address of the loopback listener this process holds, or
// why it holds none; both empty until servePanel has run.
func (s *server) listening() (string, error) {
	if p := s.panel.Load(); p != nil {
		return p.addr, p.err
	}
	return "", nil
}

// listenerReply answers the control socket's listener: the address this
// process holds, which `megavoice takes url` hands out with the key.
func (s *server) listenerReply() ctl.Response {
	addr, err := s.listening()
	if addr != "" {
		return ctl.Reply(map[string]string{"addr": addr})
	}
	if err == nil {
		err = errors.New("the listener is not up yet")
	}
	return ctl.Fail(fmt.Errorf("megavoice serve holds no listener on %s: %w", s.cfg.Compare.Addr, err))
}

// countUndelivered keeps the menu's count of the takes undelivered and not
// seen, from the Takes page's index, every 5 s.
func (s *server) countUndelivered() {
	var failed string
	for {
		failed = s.recount(failed)
		time.Sleep(5 * time.Second)
	}
}

// recount stores the menu's count once. failed is the error last logged,
// and the one it returns: an error is logged once until the count reads
// again.
func (s *server) recount(failed string) string {
	n, err := s.takes.Counted()
	switch {
	case err == nil:
		s.undelivered.Store(int64(n))
		return ""
	case err.Error() != failed:
		log.Printf("serve: the count of undelivered takes: %v", err)
	}
	return err.Error()
}

// panes are herdr's panes for the Takes page's resend picker.
func (s *server) panes() ([]takes.Pane, error) {
	ps, err := s.dlv.Herdr.Panes()
	out := make([]takes.Pane, len(ps))
	for i, p := range ps {
		out[i] = takes.Pane{ID: p.ID, Workspace: p.Workspace, Title: p.Title, Agent: p.Agent, Cwd: p.Cwd}
	}
	return out, err
}

// appsFrom lists the running apps for the Takes page's resend picker from
// list (mac.RunningApps); the frontmost app is marked Front.
func appsFrom(list func() []mac.App) func() []takes.App {
	return func() []takes.App {
		as := list()
		out := make([]takes.App, len(as))
		for i, a := range as {
			out[i] = takes.App{PID: int(a.PID), BundleID: a.BundleID, Name: a.Name, Title: a.Title, Front: a.Active}
		}
		return out
	}
}

// installTap retries until the grant arrives, so a toggle in System Settings
// takes effect without a restart.
func (s *server) installTap() {
	var logged bool
	for {
		t, err := mac.StartTap(mac.Detector{Key: s.key, Window: time.Duration(s.cfg.Tap.Window), Quiet: time.Duration(s.cfg.Tap.Quiet)}, s.onTap)
		if err == nil {
			s.mu.Lock()
			s.tap = t
			s.mu.Unlock()
			log.Printf("serve: tap installed on %s", s.key.Name)
			return
		}
		if !errors.Is(err, mac.ErrTapRefused) {
			log.Fatalf("serve: %v", err)
		}
		if !logged {
			log.Printf("serve: %v; retrying every 2 s", err)
			logged = true
		}
		time.Sleep(2 * time.Second)
	}
}

// lastKey is when the key tap last saw a key; zero without a tap.
func (s *server) lastKey() time.Time {
	s.mu.Lock()
	t := s.tap
	s.mu.Unlock()
	if t == nil {
		return time.Time{}
	}
	return t.LastKey()
}

// phaseOf tells the key tap which keys the controller's state takes.
func phaseOf(st session.State) mac.Phase {
	switch st {
	case session.Recording:
		return mac.PhaseRecording
	case session.Transcribing, session.Delivering:
		return mac.PhaseProcessing
	}
	return mac.PhaseIdle
}

// tapIn is a Result of the key tap and the time of its key event.
type tapIn struct {
	r  mac.Result
	at time.Time
}

// onTap runs on the main thread inside the tap callback: hand off, never block.
func (s *server) onTap(r mac.Result, at time.Time) {
	select {
	case s.taps <- tapIn{r, at}:
	default:
		log.Printf("tap: dropped, controller busy")
	}
}

// dispatch hands the tap's results to c in order, each noted with its
// action; it runs on one goroutine, so a call into c holds up the next key.
func (s *server) dispatch(c tapTaker) {
	for in := range s.taps {
		r := in.r
		action := "tap"
		switch {
		case r.Cancel:
			action = "cancel"
		case r.Enter:
			action = "enter"
		case r.Chord:
			action = "hotkey"
		}
		s.mu.Lock()
		s.events = append(s.events, tapEvent{time.Now(), action})
		if len(s.events) > 50 {
			s.events = s.events[len(s.events)-50:]
		}
		s.mu.Unlock()
		takeTap(r, in.at, c, s.cmds, mac.PressReturn)
	}
}

func (s *server) handle(req ctl.Request) ctl.Response {
	if resp, ok := s.cmds.handle(req); ok {
		return resp
	}
	switch req.Cmd {
	case "status":
		return ctl.Reply(s.status())
	case "spike":
		s.mu.Lock()
		defer s.mu.Unlock()
		return ctl.Reply(s.events)
	case "paste":
		unlock, err := s.ctrl.LockDelivery(10 * time.Second)
		if err != nil {
			return ctl.Fail(err)
		}
		defer unlock()
		if req.PID != 0 {
			app, err := mac.AppOf(req.PID)
			if err != nil {
				return ctl.Fail(err)
			}
			defer app.Release()
			if err := mac.Activate(app, time.Second); err != nil {
				return ctl.Fail(err)
			}
		}
		mac.Paste(req.Text)
		return ctl.Reply(map[string]any{"pasted": len(req.Text)})
	case "takes", "events":
		return s.handleTakes(req)
	case "listener":
		return s.listenerReply()
	case "resend", "retranscribe":
		return s.handleResend(req)
	}
	return ctl.Fail(fmt.Errorf("unknown command %q", req.Cmd))
}

func (s *server) status() status {
	exe, _ := os.Executable()
	front := mac.Frontmost()
	front.Release()
	st := status{
		State:      s.ctrl.State().String(),
		Build:      app.BuildID(),
		Grants:     mac.CheckGrants(),
		TapKey:     s.key.Name,
		Executable: exe,
		PID:        os.Getpid(),
		Data:       s.cfg.Store.Data,
		Frontmost:  front,
	}
	st.RestartPending = s.cmds.pending.Load()
	s.mu.Lock()
	if s.tap != nil {
		st.TapInstalled = true
		st.Taps = s.tap.Taps.Load()
	}
	s.mu.Unlock()
	return st
}
