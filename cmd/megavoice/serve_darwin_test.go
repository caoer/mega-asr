//go:build darwin

package main

import (
	"encoding/json"
	"errors"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/caoer/mega-asr/internal/ctl"
	"github.com/caoer/mega-asr/internal/mac"
	"github.com/caoer/mega-asr/internal/store"
	"github.com/caoer/mega-asr/internal/takes"
)

// The control socket's handler hands the take commands to takeCommands.
func TestServerHandleDelegates(t *testing.T) {
	f := &fakeCtl{fakeEnder: fakeEnder{ok: true}}
	s := &server{cmds: &takeCommands{ctrl: f, flash: func(string) {}, exit: func(int) {}}}
	for _, cmd := range []string{"toggle", "stop", "cancel"} {
		if resp := s.handle(ctl.Request{Cmd: cmd}); !resp.OK {
			t.Errorf("%s: %+v", cmd, resp)
		}
	}
	if got := strings.Join(f.calls, ","); got != "toggle ctl,stop ctl ,cancel ctl " {
		t.Fatalf("calls %q", got)
	}
}

// The resend picker leaves out the app RunningApps marks active: the
// browser showing the Takes page.
func TestTargetsLeaveOutActiveApp(t *testing.T) {
	th := takes.New(t.TempDir(), nil, func() []takes.Engine { return nil })
	th.Panes = func() ([]takes.Pane, error) { return nil, errors.New("herdr: not running") }
	th.Apps = appsFrom(func() []mac.App {
		return []mac.App{
			{PID: 700, BundleID: "org.example.browser", Name: "Browser", Title: "Takes", Active: true},
			{PID: 501, BundleID: "org.example.notes", Name: "Notes", Title: "Shopping list"},
		}
	})
	w := httptest.NewRecorder()
	th.ServeHTTP(w, httptest.NewRequest("GET", "/takes/api/targets", nil))
	var out takes.Targets
	if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil {
		t.Fatalf("%d %s: %v", w.Code, w.Body, err)
	}
	var to []string
	for _, x := range out.Targets {
		to = append(to, x.To)
	}
	if got := strings.Join(to, " "); got != "app:501:org.example.notes clipboard" {
		t.Fatalf("targets %q, want the notes app and the clipboard", got)
	}
}

// The tap's results go through onTap and dispatch to the controller in
// order, each noted with its action: the chord as hotkey.
func TestDispatch(t *testing.T) {
	s := &server{taps: make(chan tapIn, 16), cmds: &takeCommands{ctrl: &fakeCtl{}, flash: func(string) {}}}
	s.onTap(mac.Result{Chord: true, Swallow: true}, time.Now())
	s.onTap(mac.Result{Tap: true}, time.Now())
	close(s.taps)
	f := &fakeTaker{}
	s.dispatch(f)
	var actions []string
	for _, e := range s.events {
		actions = append(actions, e.Action)
	}
	if got, want := strings.Join(f.calls, ","), "hotkey,toggle tap"; got != want {
		t.Errorf("calls %q, want %q", got, want)
	}
	if got := strings.Join(actions, ","); got != "hotkey,tap" {
		t.Errorf("actions %q", got)
	}
}

// onTap runs inside the tap's callback and never blocks it: with the queue
// full, a result is dropped.
func TestOnTapNeverBlocks(t *testing.T) {
	s := &server{taps: make(chan tapIn, 1)}
	s.onTap(mac.Result{Tap: true}, time.Now())
	done := make(chan struct{})
	go func() { s.onTap(mac.Result{Chord: true, Swallow: true}, time.Now()); close(done) }()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("onTap blocked on a full queue")
	}
	if in := <-s.taps; !in.r.Tap || len(s.taps) != 0 {
		t.Fatalf("queued %+v and %d more, want the tap alone", in.r, len(s.taps))
	}
}

// recount stores the Takes index's count of undelivered takes for the
// menu; a store it cannot read keeps the last count and says why once.
func TestRecount(t *testing.T) {
	dir := t.TempDir()
	base := filepath.Join(dir, "20200605-141500")
	at := time.Date(2020, 6, 5, 14, 15, 0, 0, time.Local)
	for i, e := range []store.Event{{Ev: "start", Trigger: "tap"}, {Ev: "stop", Kind: "tap", DurS: 2}, {Ev: "text", Chars: 4}, {Ev: "hold", Why: "deliver_failed"}} {
		e.At = at.Add(time.Duration(i) * time.Second)
		if err := store.Append(base, e); err != nil {
			t.Fatal(err)
		}
	}
	s := &server{takes: takes.New(dir, nil, nil)}
	if failed := s.recount(""); failed != "" || s.undelivered.Load() != 1 {
		t.Fatalf("failed %q, count %d; want 1", failed, s.undelivered.Load())
	}
	notDir := filepath.Join(dir, "not-a-directory")
	if err := os.WriteFile(notDir, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	s.takes = takes.New(notDir, nil, nil)
	if failed := s.recount(""); failed == "" || s.undelivered.Load() != 1 {
		t.Fatalf("an unreadable store: failed %q, count %d; want an error and the last count", failed, s.undelivered.Load())
	}
}

// The hotkey's hint names the configured tap key.
func TestChordKey(t *testing.T) {
	if a, b := chordKey("right_shift"), chordKey("right_option"); a != "右Shift" || b != "右Option" {
		t.Fatalf("right_shift %q, right_option %q", a, b)
	}
}
