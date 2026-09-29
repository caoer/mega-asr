package main

import (
	"cmp"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/caoer/mega-asr/internal/ctl"
	"github.com/caoer/mega-asr/internal/mac"
	"github.com/caoer/mega-asr/internal/session"
)

// ender is what restart or quit from the menu needs of the controller.
type ender interface {
	StopFrom(via, id string) bool
	CancelFrom(via, id string) bool
	Drain(max time.Duration) []string
}

// exitGrace is how long past its bound the menu's exit waits for a
// controller that does not answer at all.
const exitGrace = time.Second

// endAndExit is restart or quit chosen from the menu, which ends in an exit
// in every state. The exit is armed first; then the recording take, if any,
// is ended from the menu and what is in flight is given until max. end
// cancel cancels take, the take the offer was shown for; when that take no
// longer records, what records is stopped to paste, never cancelled. Any
// other end stops what records to paste. exit is called once: with what is
// left once nothing is in flight or max has passed, or with answered false
// when the controller has not answered by max plus exitGrace.
func endAndExit(c ender, end, take string, max time.Duration, exit func(left []string, answered bool)) {
	var once sync.Once
	fire := func(left []string, answered bool) { once.Do(func() { exit(left, answered) }) }
	deadline := time.Now().Add(max)
	time.AfterFunc(max+exitGrace, func() { fire(nil, false) })
	go func() {
		if end != "cancel" || !c.CancelFrom("menu", take) {
			c.StopFrom("menu", "")
		}
		fire(c.Drain(time.Until(deadline)), true)
	}()
}

// takeRequest is the control socket's stop or cancel. stop is the menu's
// 停止并粘贴: it stops the recording take to paste and never starts one.
// Args {"via": "menu"} name the menu on the stop line (without them it is
// ctl), and {"take": id} the take the menu showed: another take is left
// recording. It fails when it ended nothing.
func takeRequest(c ender, cmd string, raw json.RawMessage) error {
	var a struct {
		Via  string `json:"via"`
		Take string `json:"take"`
	}
	if len(raw) > 0 {
		if err := json.Unmarshal(raw, &a); err != nil {
			return err
		}
	}
	switch a.Via {
	case "":
		a.Via = "ctl"
	case "menu", "ctl":
	default:
		return fmt.Errorf("via %q: menu or ctl", a.Via)
	}
	var ok bool
	if cmd == "stop" {
		ok = c.StopFrom(a.Via, a.Take)
	} else {
		ok = c.CancelFrom(a.Via, a.Take)
	}
	switch {
	case !ok && a.Take != "":
		return fmt.Errorf("take %s is not recording", a.Take)
	case !ok:
		return errors.New("no take is recording")
	}
	return nil
}

// exitEnd is restart's or quit's {"end": "paste"|"cancel", "take": id},
// which only the menu sends; end "" when absent.
func exitEnd(raw json.RawMessage) (end, take string, err error) {
	var a struct {
		End  string `json:"end"`
		Take string `json:"take"`
	}
	if len(raw) > 0 {
		if err := json.Unmarshal(raw, &a); err != nil {
			return "", "", err
		}
	}
	switch a.End {
	case "", "paste", "cancel":
		return a.End, a.Take, nil
	}
	return "", "", fmt.Errorf("end %q: paste or cancel", a.End)
}

// menuExit is restart or quit chosen from the menu (endAndExit at drainMax):
// it logs what it leaves and calls exit with 75 for restart, 0 for quit.
func menuExit(c ender, cmd, end, take string, exit func(code int)) {
	code := 0
	if cmd == "restart" {
		code = 75
	}
	log.Printf("serve: %s from the menu (a recording take ends: %s); waiting up to %v for what is in flight", cmd, end, drainMax)
	endAndExit(c, end, take, drainMax, func(left []string, answered bool) {
		switch {
		case !answered:
			log.Printf("serve: %s: the controller did not answer within %v; the audio on disk is what the next start finds", cmd, drainMax+exitGrace)
		case len(left) > 0:
			log.Printf("serve: %s: %v passed with %s in flight; the audio on disk is what the next start finds", cmd, drainMax, strings.Join(left, ", "))
		}
		log.Printf("serve: %s requested; exiting %d", cmd, code)
		exit(code)
	})
}

// controller is what the control socket's take commands need.
type controller interface {
	ender
	ToggleFrom(trigger string)
	Busy() bool
	State() session.State
	Recording() (id string, d time.Duration, ok bool)
	Close(msg string) bool
	Reopen()
}

// takeCommands are the control socket's toggle, stop, cancel, restart and
// quit, and the refusal of a start while an exit waits.
type takeCommands struct {
	ctrl    controller
	flash   func(msg string)
	exit    func(code int)
	lastKey func() time.Time       // the last key event the tap saw; nil: no tap
	quiet   time.Duration          // restart --when-idle's quiet; 0 is quietFor
	exiting atomic.Pointer[string] // restart or quit, once one is under way
	pending atomic.Bool            // restart --when-idle waits for quiet
}

// quietFor is how long restart --when-idle waits with nothing in flight and
// no key pressed before it exits.
const quietFor = 3 * time.Second

// exitMsg is the flash of a start refused while cmd exits.
func exitMsg(cmd string) string {
	if cmd == "quit" {
		return "正在退出"
	}
	return "正在重启"
}

// handle answers req when it is one of the take commands; ok false leaves it
// to the caller.
func (k *takeCommands) handle(req ctl.Request) (resp ctl.Response, ok bool) {
	state := func() ctl.Response { return ctl.Reply(map[string]string{"state": k.ctrl.State().String()}) }
	switch req.Cmd {
	case "toggle":
		if k.refuseStart() {
			return ctl.Fail(fmt.Errorf("megavoice is exiting (%s)", *k.exiting.Load())), true
		}
		k.ctrl.ToggleFrom("ctl")
		return state(), true
	case "stop", "cancel":
		if err := takeRequest(k.ctrl, req.Cmd, req.Args); err != nil {
			return ctl.Fail(err), true
		}
		return state(), true
	case "restart", "quit":
		// The agent's KeepAlive restarts it on a failed exit only: restart
		// exits 75 (EX_TEMPFAIL) and launchd starts the same signed app
		// again, so the grants hold and the config file is read anew; quit
		// exits 0 and it stays down until `megavoice start` or the next login.
		end, take, err := exitEnd(req.Args)
		if err != nil {
			return ctl.Fail(err), true
		}
		if end != "" {
			// from the menu: an exit in every state (menuExit), armed once;
			// no take starts meanwhile
			cmd := req.Cmd
			if !k.exiting.CompareAndSwap(nil, &cmd) {
				return ctl.Reply(map[string]string{"state": *k.exiting.Load()}), true
			}
			menuExit(k.ctrl, cmd, end, take, k.exit)
			return ctl.Reply(map[string]string{"state": cmd}), true
		}
		if req.Cmd == "restart" && whenIdle(req.Args) {
			if k.pending.CompareAndSwap(false, true) {
				quiet := cmp.Or(k.quiet, quietFor)
				log.Printf("serve: restart pending: exiting 75 once nothing is in flight and no key was pressed for %v", quiet)
				go k.restartWhenIdle(quiet, min(quiet/10, 100*time.Millisecond))
			}
			return ctl.Reply(map[string]string{"state": "pending"}), true
		}
		// a plain restart or quit exits now or not at all: from the idle
		// check on, no take starts
		cmd := req.Cmd
		if !k.exiting.CompareAndSwap(nil, &cmd) {
			return ctl.Reply(map[string]string{"state": *k.exiting.Load()}), true
		}
		if !k.ctrl.Close(exitMsg(cmd)) {
			k.ctrl.Reopen()
			k.exiting.Store(nil)
			return ctl.Fail(fmt.Errorf("a take is in flight; %s once it is kept or delivered", req.Cmd)), true
		}
		code := 0
		if req.Cmd == "restart" {
			code = 75
		}
		log.Printf("serve: %s requested; exiting %d", req.Cmd, code)
		time.AfterFunc(200*time.Millisecond, func() { k.exit(code) })
		return ctl.Reply(map[string]string{"state": req.Cmd}), true
	}
	return ctl.Response{}, false
}

// whenIdle reports restart's {"when_idle": true}, which
// `megavoice restart --when-idle` sends.
func whenIdle(raw json.RawMessage) bool {
	var a struct {
		WhenIdle bool `json:"when_idle"`
	}
	return len(raw) > 0 && json.Unmarshal(raw, &a) == nil && a.WhenIdle
}

// restartWhenIdle is restart --when-idle's wait, polled every poll: once
// nothing has been in flight and no key was pressed for quiet, it refuses
// every start and exits 75. It never gives up; while another exit is under
// way it waits on.
func (k *takeCommands) restartWhenIdle(quiet, poll time.Duration) {
	since := time.Now()
	for {
		time.Sleep(poll)
		now := time.Now()
		if k.exiting.Load() != nil || k.ctrl.Busy() {
			since = now
			continue
		}
		if k.lastKey != nil {
			if key := k.lastKey(); key.After(since) {
				since = key
			}
		}
		if now.Sub(since) < quiet {
			continue
		}
		cmd := "restart"
		if !k.exiting.CompareAndSwap(nil, &cmd) {
			continue
		}
		if k.ctrl.Close(exitMsg(cmd)) {
			log.Printf("serve: restart: nothing in flight and no key for %v; exiting 75", quiet)
			k.exit(75)
			return
		}
		k.ctrl.Reopen()
		k.exiting.Store(nil)
		since = now
	}
}

// refuseStart reports whether a tap or toggle that would start a take is
// refused because an exit is under way, and flashes why; one that stops a
// recording take is never refused.
func (k *takeCommands) refuseStart() bool {
	cmd := k.exiting.Load()
	if cmd == nil {
		return false
	}
	if _, _, rec := k.ctrl.Recording(); rec {
		return false
	}
	k.flash(exitMsg(*cmd))
	return true
}

// signalWait is how long a termination signal gives Interrupt before the
// exit, whatever launchd allows.
const signalWait = 3 * time.Second

// interrupter is what a termination signal needs of the controller.
type interrupter interface{ Interrupt() []string }

// onSignal waits for a termination signal on sig, then exits 75 — the
// agent's KeepAlive starts it again on a failed exit only — once c.Interrupt
// has closed and marked the takes in flight, or when wait has passed,
// whichever comes first. It never waits for the controller's mutex.
func onSignal(sig <-chan os.Signal, c interrupter, wait time.Duration, exit func(code int)) {
	s := <-sig
	log.Printf("serve: %v: closing the takes in flight; exiting 75 within %v", s, wait)
	var once sync.Once
	fire := func() { once.Do(func() { exit(75) }) }
	time.AfterFunc(wait, func() {
		log.Printf("serve: %v: %v passed; exiting 75", s, wait)
		fire()
	})
	marked := c.Interrupt()
	log.Printf("serve: %v: marked interrupted: %d takes %s; exiting 75", s, len(marked), strings.Join(marked, " "))
	fire()
}

// tapTaker is what a key tap's Result needs of the controller.
type tapTaker interface {
	Cancel()
	Hotkey()
	EnterWhy() (used bool, why string)
	ToggleAt(trigger string, at time.Time)
}

// takeTap acts on one Result of the key tap: Esc cancels; the chord (the
// tap key held, Esc pressed) is the resend hotkey; a tap is refused while a
// menu exit waits, else it starts or stops a take; a Return no take uses
// goes back to the app through pressReturn.
func takeTap(r mac.Result, at time.Time, c tapTaker, k *takeCommands, pressReturn func()) {
	switch {
	case r.Cancel:
		c.Cancel()
	case r.Chord:
		c.Hotkey()
	case r.Tap && k.refuseStart():
	case r.Enter:
		if used, why := c.EnterWhy(); !used {
			log.Printf("tap: Return used by no take (%s); passing it on", why)
			pressReturn()
		}
	default:
		c.ToggleAt("tap", at)
	}
}
