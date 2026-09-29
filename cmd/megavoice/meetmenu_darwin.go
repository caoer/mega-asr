//go:build darwin

package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/caoer/mega-asr/internal/app"
	"github.com/caoer/mega-asr/internal/audio"
	"github.com/caoer/mega-asr/internal/ctl"
	"github.com/caoer/mega-asr/internal/mac"
	"github.com/caoer/mega-asr/internal/micserver"
)

// meetMenu polls megameet serve — status every second, five times a second
// while the menu is open for the level meter, the last recording's page
// record every 30 s — and renders meetState on a status item.
type meetMenu struct {
	config   string                 // the config file the pickers write
	global   []string               // the global flags a megavoice command from the menu gets
	sock     string                 // megameet serve's control socket
	listener func() (string, error) // the listener this process holds, or why none
	take     func() voiceTake       // the dictation take recording now
	count    func() int             // the takes undelivered and not seen
	item     *mac.StatusItem
	wake     chan struct{}

	mu     sync.Mutex
	st     meetState
	open   bool
	lastAt time.Time
	scanAt time.Time // the last search for mic servers ended
}

// meetMenuOf is the menu over the agent's state, not yet on the menu bar:
// show puts it there.
func meetMenuOf(o app.LoadOpts, running app.Config, take func() voiceTake, count func() int, listener func() (string, error)) *meetMenu {
	path, _ := app.Path(o.Path)
	m := &meetMenu{
		config:   path,
		take:     take,
		count:    count,
		sock:     filepath.Join(filepath.Dir(stateDir()), "megameet", "ctl.sock"), // megameet's stateDir()/ctl.sock
		wake:     make(chan struct{}, 1),
		listener: listener,
	}
	if o.Path != "" {
		m.global = []string{"--config", o.Path}
	}
	m.st.Voice.Running, m.st.Voice.Store = voiceOf(running), running.Store.Data
	return m
}

// show puts the menu on the menu bar and keeps it current.
func (m *meetMenu) show() {
	m.item = mac.NewStatusItem(m.opened, m.closed)
	m.readLocal()
	m.poll()
	go m.loop()
}

func (m *meetMenu) call(cmd string, args any, timeout time.Duration, v any) error {
	req := ctl.Request{Cmd: cmd}
	if args != nil {
		b, err := json.Marshal(args)
		if err != nil {
			return err
		}
		req.Args = b
	}
	resp, err := ctl.Call(m.sock, req, timeout)
	if errors.Is(err, ctl.ErrNotRunning) {
		return fmt.Errorf("no answer on %s", m.sock)
	}
	if err != nil {
		return err
	}
	if !resp.OK {
		return errors.New(resp.Error)
	}
	if v == nil || len(resp.Data) == 0 {
		return nil
	}
	return json.Unmarshal(resp.Data, v)
}

func (m *meetMenu) loop() {
	for {
		m.mu.Lock()
		every := time.Second
		if m.open {
			every = 200 * time.Millisecond
		}
		m.mu.Unlock()
		select {
		case <-time.After(every):
		case <-m.wake:
		}
		m.poll()
	}
}

func (m *meetMenu) poke() {
	select {
	case m.wake <- struct{}{}:
	default:
	}
}

// poll reads status, and the last recording when it is due or a recording
// just ended, then renders.
func (m *meetMenu) poll() {
	var s meetStatus
	err := m.call("status", nil, 2*time.Second, &s)
	tk, n := m.take(), m.count()
	m.mu.Lock()
	m.st.Voice.Take, m.st.Voice.Undelivered = tk, n
	was := m.st.Status.Recording
	if err != nil {
		m.st.Down, m.st.Status = err.Error(), meetStatus{}
	} else {
		m.st.Down, m.st.Status = "", s
	}
	due := err == nil && (time.Since(m.lastAt) > 30*time.Second || (was && !s.Recording))
	if due {
		m.lastAt = time.Now()
	}
	m.mu.Unlock()
	if due {
		go m.readLast()
	}
	m.render()
}

func (m *meetMenu) readLast() {
	var l *meetLast
	if err := m.call("last", nil, 15*time.Second, &l); err != nil {
		log.Printf("menu: last: %v", err)
		return
	}
	m.mu.Lock()
	m.st.Last = l
	m.mu.Unlock()
	m.render()
}

func voiceOf(c app.Config) voiceSettings {
	return voiceSettings{TapKey: c.Tap.Key, WholeMax: time.Duration(c.Take.WholeMax), LLM: c.ASR.FunASR.LLM, Engine: c.ASR.Engine}
}

// readLocal reads the inputs, the config file and the files the menu offers.
func (m *meetMenu) readLocal() {
	in, err := audio.Inputs()
	if err != nil {
		log.Printf("menu: inputs: %v", err)
	}
	pick, page, fileErr := "default", "", ""
	l, err := load(app.LoadOpts{Path: m.config})
	if err == nil {
		pick, page = l.Meeting.Capture.Mic, meetingsPage(l.Meeting.Page)
	} else {
		fileErr = err.Error()
	}
	var models []model
	ggufs, _ := filepath.Glob(filepath.Join(app.FinetuneDir(), "*", "*.gguf"))
	for _, g := range ggufs {
		models = append(models, model{Name: filepath.Base(filepath.Dir(g)), Path: g})
	}
	remotes, err := app.LoadRemotes(app.RemotesPath())
	if err != nil {
		log.Printf("menu: remotes: %v", err)
	}
	home, _ := os.UserHomeDir()
	var logs []string
	for _, name := range []string{"megavoice.log", "megameet.log"} {
		p := filepath.Join(home, "Library", "Logs", name)
		if _, err := os.Stat(p); err == nil {
			logs = append(logs, p)
		}
	}
	m.mu.Lock()
	m.st.Inputs, m.st.MicPick, m.st.Page = in, pick, page
	v := &m.st.Voice
	v.Inputs = v.Inputs[:0]
	for _, i := range in {
		v.Inputs = append(v.Inputs, inputView{UID: i.UID, Name: i.Name, Channels: i.Channels, Default: i.Default})
	}
	was := v.Remotes
	v.Remotes = nil
	for _, r := range remotes {
		rv := remoteView{Name: r.Name, Addr: r.Addr}
		for _, o := range was {
			if o.Name == r.Name && o.Addr == r.Addr {
				rv.Offline = o.Offline // until the next probe
			}
		}
		v.Remotes = append(v.Remotes, rv)
	}
	v.File, v.FileErr, v.Models, v.Logs = voiceOf(l.Config), fileErr, models, logs
	v.Hotwords, v.Corrections = app.HotwordsPath(), app.CorrectionsPath()
	v.Takes, v.TakesNote = pageURL(m.listener()) // read at every open: a rotated key applies at once
	if fileErr == "" {
		v.Store = l.Store.Data
		v.Compare = compareSettings{On: l.Compare.On, Engines: l.Compare.Engines}
		v.Capture = l.Capture
	}
	m.mu.Unlock()
}

func (m *meetMenu) opened() {
	m.mu.Lock()
	m.open = true
	m.lastAt = time.Time{}
	m.mu.Unlock()
	m.readLocal()
	go m.probeRemotes()
	m.mu.Lock()
	every := 30 * time.Second
	if len(m.st.Voice.Found) == 0 || m.st.Voice.FoundNote != "" {
		every = 5 * time.Second // e.g. Local Network access was just allowed
	}
	scan := !m.st.Voice.Scanning && time.Since(m.scanAt) > every
	m.st.Voice.Scanning = m.st.Voice.Scanning || scan
	m.mu.Unlock()
	if scan {
		go m.scanMics()
	}
	m.poll()
}

// scanMics searches for the mic servers Add Remote Mic… offers, off the
// main thread; the menu shows the last search's until this one ends.
func (m *meetMenu) scanMics() {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	t0 := time.Now()
	found, note, public := scanMics(ctx, sshConfigPath())
	log.Printf("menu: mic servers: %d found in %v (%d public ssh Hosts not probed)", len(found), time.Since(t0).Round(time.Millisecond), public)
	m.mu.Lock()
	m.st.Voice.Found, m.st.Voice.FoundNote, m.st.Voice.Scanning = found, note, false
	m.scanAt = time.Now()
	m.mu.Unlock()
	m.render()
}

// probeRemotes marks each paired remote mic offline that a TCP connect
// does not reach within 300 ms, all at once, then renders.
func (m *meetMenu) probeRemotes() {
	m.mu.Lock()
	rs := slices.Clone(m.st.Voice.Remotes)
	m.mu.Unlock()
	var wg sync.WaitGroup
	for i := range rs {
		wg.Go(func() {
			c, err := net.DialTimeout("tcp", rs[i].Addr, 300*time.Millisecond)
			if err == nil {
				c.Close()
			}
			rs[i].Offline = err != nil
		})
	}
	wg.Wait()
	m.mu.Lock()
	for i := range m.st.Voice.Remotes {
		for _, r := range rs {
			if m.st.Voice.Remotes[i].Name == r.Name && m.st.Voice.Remotes[i].Addr == r.Addr {
				m.st.Voice.Remotes[i].Offline = r.Offline
			}
		}
	}
	m.mu.Unlock()
	m.render()
}

// addRemote is Add Remote Mic › Other…: it asks for a mic server's host,
// then pairs as a found box does.
func (m *meetMenu) addRemote() {
	vals, ok := mac.Prompt("Add Remote Mic",
		"The machine that runs `megavoice mic serve`, e.g. the Linux box your mic array is plugged into.", "Next",
		[]mac.Field{{Label: "Host", Placeholder: "micbox or micbox:7866"}})
	if !ok || strings.TrimSpace(vals[0]) == "" {
		return
	}
	dest, port := splitDest(strings.TrimSpace(vals[0]))
	m.pair(dest, port, "")
}

// pair pairs with a mic server off the main thread: over ssh when dest is
// set and BatchMode ssh reaches it, with no prompt; otherwise by the PIN its
// `mic pair` shows, at addr (dest's ssh address when addr is "").
func (m *meetMenu) pair(dest, port, addr string) {
	go func() {
		var why error
		if dest != "" {
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			r, err := sshPair(ctx, m.config, dest, port, clientName())
			cancel()
			var noSSH sshFailed
			switch {
			case err == nil:
				log.Printf("menu: paired with %s (%s) as %s over ssh %s", r.Name, r.Addr, r.Client, dest)
				m.readLocal()
				m.note(nil)
				return
			case !errors.As(err, &noSSH):
				log.Printf("menu: pair %s over ssh: %v", dest, err)
				m.readLocal()
				m.note(err)
				mac.Later(func() { mac.Alert("Pairing with "+dest+" failed", err.Error()) })
				return
			}
			log.Printf("menu: pair %s over ssh: %v; asking for the PIN", dest, err)
			why = err
			if addr == "" {
				addr = pinAddr(context.Background(), dest, port)
			}
		}
		m.pinPair(addr, why)
	}()
}

// pinPair checks the mic server at addr answers, then asks for the PIN its
// `mic pair` shows and pairs; the prompt runs on the main thread, the
// network off it. why is the ssh pairing's failure, if one was tried.
func (m *meetMenu) pinPair(addr string, why error) {
	// a TLS answer, not only a TCP one: a proxy's fake IP accepts TCP
	// and closes, and the PIN prompt would come before the failure
	if err := micserver.Probe(context.Background(), addr); err != nil {
		mac.Later(func() { mac.Alert(addr+" unreachable", err.Error()+"\n\nIs `megavoice mic serve` running there?") })
		return
	}
	msg := "Run `megavoice mic pair` on " + hostOf(addr) + " and type the 8-digit PIN it shows. It works once, for 2 minutes."
	if why != nil {
		msg = "Pairing over ssh did not work (" + trim(why.Error(), 160) + ").\n\n" + msg
	}
	mac.Later(func() {
		vals, ok := mac.Prompt("Pair with "+hostOf(addr), msg, "Pair", []mac.Field{{Label: "PIN", Placeholder: "8 digits"}})
		if !ok {
			return
		}
		pin := strings.ReplaceAll(strings.TrimSpace(vals[0]), " ", "")
		go func() {
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()
			r, err := pairRemote(ctx, m.config, addr, pin, clientName())
			m.readLocal()
			m.note(err)
			if err != nil {
				log.Printf("menu: pair %s: %v", addr, err)
				mac.Later(func() { mac.Alert("Pairing with "+hostOf(addr)+" failed", err.Error()) })
				return
			}
			log.Printf("menu: paired with %s (%s) as %s", r.Name, r.Addr, r.Client)
			mac.Later(func() {
				mac.Alert("Paired with "+r.Name, "Dictation records from "+r.Name+"'s mic from the next take. On "+r.Name+", `megavoice mic revoke "+r.Client+"` ends it.")
			})
		}()
	})
}

func (m *meetMenu) closed() {
	m.mu.Lock()
	m.open = false
	m.mu.Unlock()
}

func (m *meetMenu) render() {
	m.mu.Lock()
	st := m.st
	m.mu.Unlock()
	b := st.bar()
	m.item.SetBar(mac.Bar{Symbol: b.symbol, Tint: b.tint, Title: b.title})
	m.item.Set(m.items(st.rows()))
}

func (m *meetMenu) items(rs []row) []mac.Item {
	out := make([]mac.Item, len(rs))
	for i, r := range rs {
		// A coloured row is enabled, with no action, so AppKit does not dim it.
		it := mac.Item{Title: r.title, Key: r.key, Enabled: r.enabled || r.color != "", Checked: r.checked, Mono: r.mono, Color: r.color, Indent: r.indent, Sep: r.sep}
		if r.sub != nil {
			it.Sub = m.items(r.sub)
		}
		if a := r.act; a != nil {
			it.Do = func() { m.do(*a) }
		}
		out[i] = it
	}
	return out
}

// do runs a row's action on the main thread; the socket calls go to a
// goroutine.
func (m *meetMenu) do(a act) {
	switch a.kind {
	case actStart:
		m.start()
	case actStop:
		m.busy("stopping…")
		go func() {
			err := m.call("stop", nil, 30*time.Second, nil)
			m.done(err)
			m.mu.Lock()
			m.lastAt = time.Time{}
			m.mu.Unlock()
			m.poke()
		}()
	case actMic:
		err := set(m.config, "meeting.capture.mic", strconv.Quote(a.arg))
		if err != nil {
			log.Printf("menu: mic: %v", err)
		} else {
			log.Printf("menu: meeting.capture.mic = %q", a.arg)
		}
		m.readLocal()
		m.done(err)
	case actOpen:
		mac.Open(a.arg)
	case actSet:
		err := set(m.config, a.key, a.arg)
		if err != nil {
			log.Printf("menu: %s: %v", a.key, err)
		} else {
			log.Printf("menu: %s = %s", a.key, a.arg)
		}
		m.readLocal()
		m.note(err)
	case actExec:
		go func() {
			out, err := m.command(a.argv).CombinedOutput()
			if err != nil {
				log.Printf("menu: %s: %v: %s", strings.Join(a.argv, " "), err, out)
				m.note(fmt.Errorf("%s: %v", strings.Join(a.argv, " "), err))
			}
		}()
	case actCapture:
		var err error
		for _, kv := range a.sets {
			if err = set(m.config, kv[0], kv[1]); err != nil {
				log.Printf("menu: %s: %v", kv[0], err)
				break
			}
			log.Printf("menu: %s = %s", kv[0], kv[1])
		}
		if err == nil && picksLocal(a.sets) && mac.MicrophoneAuth() == mac.MicNotDetermined {
			go func() {
				err := audio.PrimeMic()
				log.Printf("menu: microphone grant asked at the pick: %v; now %v", err, mac.MicrophoneAuth())
			}()
		}
		m.readLocal()
		m.note(err)
	case actAddRemote:
		m.addRemote()
	case actPair:
		m.pair(a.key, "", a.arg)
	case actForget:
		_, _, err := forgetRemote(m.config, a.arg)
		if err != nil {
			log.Printf("menu: forget %s: %v", a.arg, err)
		} else {
			log.Printf("menu: forgot remote mic %s", a.arg)
		}
		m.readLocal()
		m.note(err)
	case actTake:
		go func() {
			err := m.voiceCall(a.arg, takeArgs(a))
			if err != nil {
				log.Printf("menu: %s the take: %v", a.arg, err)
			}
			m.note(err)
			m.poke()
		}()
	case actExit:
		m.exit(a.arg)
	case actRun:
		m.check(a.arg, "", a.argv)
	case actLast:
		m.mu.Lock()
		dir := m.st.Voice.Store
		m.mu.Unlock()
		wav, err := lastTake(dir)
		if err != nil {
			m.note(err)
			return
		}
		saved, _ := os.ReadFile(strings.TrimSuffix(wav, ".wav") + ".txt")
		m.check(a.arg, "Take "+strings.TrimSuffix(filepath.Base(wav), ".wav")+"\nSaved: "+strings.TrimSpace(string(saved))+"\n\nNow:", []string{"megavoice", "transcribe", wav})
	}
}

// exit is Restart or Quit: while a take records it offers to end the take
// first, and the choice made either ends it and exits or keeps it recording.
func (m *meetMenu) exit(cmd string) {
	tk := m.take()
	o := exitOffer(cmd, tk.Elapsed)
	choice := -1
	if tk.Recording {
		labels := make([]string, len(o.choices))
		for i, c := range o.choices {
			labels[i] = c.label
		}
		choice = mac.Choose(o.title, o.info, labels...)
	}
	args := exitArgs(tk, o, choice)
	if args == nil {
		log.Printf("menu: %s: not chosen (choice %d); the take records on", cmd, choice)
		return
	}
	log.Printf("menu: %s, a recording take ends: %s", cmd, args["end"])
	go func() {
		if err := m.voiceCall(cmd, args); err != nil {
			log.Printf("menu: %s: %v", cmd, err)
			m.note(err)
		}
	}()
}

// voiceCall sends cmd to this megavoice serve's own control socket; call it
// off the main thread.
func (m *meetMenu) voiceCall(cmd string, args any) error {
	b, err := json.Marshal(args)
	if err != nil {
		return err
	}
	resp, err := ctl.Call(sockPath(), ctl.Request{Cmd: cmd, Args: b}, 5*time.Second)
	if err != nil {
		return err
	}
	if !resp.OK {
		return errors.New(resp.Error)
	}
	return nil
}

// command is argv as a process: megavoice is this app's own binary with the
// menu's global flags, anything else is looked up on PATH.
func (m *meetMenu) command(argv []string) *exec.Cmd {
	name, args := argv[0], argv[1:]
	if name == "megavoice" {
		name, _ = os.Executable()
		args = append(slices.Clone(m.global), args...)
	}
	return exec.Command(name, args...)
}

// check runs one command at a time and shows what it printed, after head,
// with the command line to run it from a shell.
func (m *meetMenu) check(title, head string, argv []string) {
	m.mu.Lock()
	m.st.Voice.Check, m.st.Voice.Note = title, ""
	m.mu.Unlock()
	m.render()
	go func() {
		t0 := time.Now()
		out, err := m.command(argv).CombinedOutput()
		body := strings.TrimRight(string(out), "\n")
		if lines := strings.Split(body, "\n"); len(lines) > 40 {
			body = strings.Join(lines[:40], "\n") + fmt.Sprintf("\n… %d more lines", len(lines)-40)
		}
		if err != nil {
			body += "\n(" + err.Error() + ")"
		}
		if head != "" {
			body = head + "\n" + body
		}
		body += fmt.Sprintf("\n\n$ %s   (%.1f s)", strings.Join(argv, " "), time.Since(t0).Seconds())
		log.Printf("menu: %s: %v in %v", strings.Join(argv, " "), err, time.Since(t0).Round(time.Millisecond))
		m.mu.Lock()
		m.st.Voice.Check = ""
		m.mu.Unlock()
		m.render()
		mac.Later(func() { mac.Alert(title, body) })
	}()
}

func (m *meetMenu) note(err error) {
	m.mu.Lock()
	m.st.Voice.Note = ""
	if err != nil {
		m.st.Voice.Note = trim(err.Error(), 90)
	}
	m.mu.Unlock()
	m.render()
}

func (m *meetMenu) start() {
	m.mu.Lock()
	mic := m.st.micName()
	m.mu.Unlock()
	vals, ok := mac.Prompt("Start meeting",
		"Records the meeting app's audio and your mic ("+mic+"). Stop it from this menu.", "Start",
		[]mac.Field{{Label: "Title", Placeholder: "e.g. Supplier call"}, {Label: "Speakers (optional)", Placeholder: "leave empty to estimate"}})
	if !ok {
		return
	}
	a := struct {
		Title    string `json:"title,omitempty"`
		Speakers int    `json:"speakers,omitempty"`
	}{Title: strings.TrimSpace(vals[0])}
	if s := strings.TrimSpace(vals[1]); s != "" {
		n, err := strconv.Atoi(s)
		if err != nil || n < 0 {
			mac.Alert("Speakers must be a number", fmt.Sprintf("%q is not; the meeting was not started.", s))
			return
		}
		a.Speakers = n
	}
	m.busy("starting…")
	go func() {
		// The first recording under launchd waits on the permission prompts.
		err := m.call("start", a, 2*time.Minute, nil)
		m.done(err)
		m.poke()
	}()
}

func (m *meetMenu) busy(s string) {
	m.mu.Lock()
	m.st.Busy, m.st.Note = s, ""
	m.mu.Unlock()
	m.render()
}

func (m *meetMenu) done(err error) {
	m.mu.Lock()
	m.st.Busy, m.st.Note = "", ""
	if err != nil {
		m.st.Note = trim(err.Error(), 90)
	}
	m.mu.Unlock()
	m.render()
}
