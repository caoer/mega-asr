package main

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/caoer/mega-asr/internal/app"
	"github.com/caoer/mega-asr/internal/deliver"
	"github.com/caoer/mega-asr/internal/keypad"
	"github.com/caoer/mega-asr/internal/session"
)

// fakeLink is a keypad.Link that keeps the snapshot and takes events from
// the test.
type fakeLink struct {
	mu         sync.Mutex
	snap       keypad.Snapshot
	ev         chan keypad.Event
	offline    bool
	noKey1     bool
	connection uint64
}

func (l *fakeLink) Key1State() (uint64, bool) {
	l.mu.Lock()
	defer l.mu.Unlock()
	connection := l.connection
	if connection == 0 {
		connection = 1
	}
	return connection, !l.offline && !l.noKey1
}

func (l *fakeLink) Update(f func(*keypad.Snapshot)) {
	l.mu.Lock()
	defer l.mu.Unlock()
	f(&l.snap)
}
func (l *fakeLink) Events() <-chan keypad.Event { return l.ev }
func (l *fakeLink) Run(ctx context.Context)     {}
func (l *fakeLink) Connected() bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	return !l.offline
}
func (l *fakeLink) get() keypad.Snapshot {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.snap
}

const deskTargets = `{"groups": [
  {"chat": -1001, "label": "报到群", "topics": [{"id": 7, "label": "ccc-status"}, {"id": 9, "label": "test"}]},
  {"chat": -1002, "label": "蓝蟹"}
]}`

func newTestDesk(t *testing.T) (*desk, *fakeLink) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "keypad.json")
	if err := os.WriteFile(path, []byte(deskTargets), 0o600); err != nil {
		t.Fatal(err)
	}
	l := &fakeLink{ev: make(chan keypad.Event)}
	return newDesk(l, path), l
}

// The keypad starts idle with the pick on its face; knob and key 4 events
// move the pick and the lock, and the face follows.
func TestDeskEvents(t *testing.T) {
	d, l := newTestDesk(t)
	if s := l.get(); s.State != "idle" || s.Group != "报到群" || s.Topic != "ccc-status" || s.Lock {
		t.Fatalf("start %+v", s)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { d.run(ctx); close(done) }()
	l.ev <- keypad.Event{Kind: keypad.Knob, N: 1, Held: true}
	l.ev <- keypad.Event{Kind: keypad.Key4}
	l.ev <- keypad.Event{Kind: keypad.Knob, N: 1} // locked: the pick stays
	cancel()
	<-done
	if s := l.get(); s.Group != "报到群" || s.Topic != "test" || !s.Lock {
		t.Errorf("after hold+turn, key 4, turn: %+v", s)
	}
	tt, ok := d.locked()
	if !ok || tt != (deliver.TelegramTarget{Chat: "-1001", Topic: 9, Group: "报到群", TopicLabel: "test"}) {
		t.Errorf("locked %+v %v", tt, ok)
	}
	d.event(keypad.Event{Kind: keypad.Key4})
	d.event(keypad.Event{Kind: keypad.Knob, N: -1})
	if s := l.get(); s.Group != "蓝蟹" || s.Topic != "" || s.Lock {
		t.Errorf("after unlock and turn back: %+v", s)
	}
	if _, ok := d.locked(); ok {
		t.Error("unlocked desk reports a locked target")
	}
}

func TestDeskTopicAlias(t *testing.T) {
	path := filepath.Join(t.TempDir(), "keypad.json")
	body := strings.Replace(deskTargets, `"label": "ccc-status"`, `"label": "ccc-status", "alias": "SA"`, 1)
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	l := &fakeLink{ev: make(chan keypad.Event)}
	d := newDesk(l, path)
	d.event(keypad.Event{Kind: keypad.Key4})
	shown := l.get()
	if shown.Group != "SA" || shown.Topic != "" || !shown.Lock {
		t.Fatalf("keypad display = %+v", shown)
	}
	var captured *deliver.TelegramTarget
	d.bind(&session.Controller{}, 400*time.Millisecond, func(_ time.Time, target *deliver.TelegramTarget, _ string) {
		captured = target
	})
	at := time.Now()
	d.key1At(true, at, shown)
	d.key1At(false, at.Add(100*time.Millisecond), shown)
	if captured == nil || *captured != (deliver.TelegramTarget{Chat: "-1001", Topic: 7, Group: "SA"}) {
		t.Fatalf("captured aliased target = %+v", captured)
	}
	target := d.router(deskMac{}, deliver.Telegram{}).CaptureKeypad(captured)
	if label := target.(interface{ TargetLabel() string }).TargetLabel(); label != "SA" {
		t.Fatalf("overlay target = %q", label)
	}
	if sent := target.(session.EndTarget).ForEnd(true).(*deliver.TelegramTarget); *sent != *captured {
		t.Fatalf("send target = %+v", sent)
	}
}

// The controller's state and view reach the face.
func TestDeskStateAndView(t *testing.T) {
	d, l := newTestDesk(t)
	d.view(session.Recording, session.View{Send: true})
	if s := l.get(); s.State != "recording" || !s.Send {
		t.Errorf("%+v", s)
	}
	d.view(session.Idle, session.View{})
	if l.get().Send {
		t.Error("the zero view left end send")
	}
}

// deskMac is a Mac deliverer for the wiring tests.
type deskMac struct{ deliverErr, submitErr error }

type deskTarget struct{}

func (deskTarget) String() string { return "front" }

func (m deskMac) Capture() session.Target                          { return deskTarget{} }
func (m deskMac) Deliver(session.Target, string) error             { return m.deliverErr }
func (m deskMac) Submit(session.Target) error                      { return m.submitErr }
func (m deskMac) Received(session.Target, string) (string, string) { return "", "" }

// Every delivery's outcome reaches the face as its done word (amendment 2):
// paste only → pasted, Enter → sent, Telegram ok → sent, the deadline →
// timeout (it may have posted), any other failure → failed.
func TestDeskOutcomeIsDone(t *testing.T) {
	sent := deliver.Telegram{Argv: []string{"sh", "-c", `cat >/dev/null; echo "Sent → message 5"`}}
	expired := deliver.Telegram{Argv: []string{"sh", "-c", `echo "session expired" >&2; exit 1`}}
	slow := deliver.Telegram{Argv: []string{"sh", "-c", `exec sleep 30`}, Wait: 200 * time.Millisecond}
	for _, c := range []struct {
		name  string
		mac   deskMac
		send  deliver.Telegram
		lock  bool
		enter bool
		want  keypad.Done
	}{
		{"paste only", deskMac{}, sent, false, false, keypad.DonePasted},
		{"Enter pressed", deskMac{}, sent, false, true, keypad.DoneSent},
		{"paste failed", deskMac{deliverErr: errors.New("no app")}, sent, false, false, keypad.DoneFailed},
		{"Enter failed", deskMac{submitErr: errors.New("lost focus")}, sent, false, true, keypad.DoneFailed},
		{"telegram sent", deskMac{deliverErr: errors.New("the Mac must not be used")}, sent, true, true, keypad.DoneSent},
		{"telegram failed", deskMac{}, expired, true, true, keypad.DoneFailed},
		{"telegram timeout", deskMac{}, slow, true, true, keypad.DoneTimeout},
		{"key 1 pastes under lock", deskMac{}, expired, true, false, keypad.DonePasted},
	} {
		t.Run(c.name, func(t *testing.T) {
			d, l := newTestDesk(t)
			if c.lock {
				d.event(keypad.Event{Kind: keypad.Key4})
			}
			r := d.router(c.mac, c.send)
			target := captureDesk(d, r)
			if et, ok := target.(session.EndTarget); ok {
				target = et.ForEnd(c.enter)
			}
			if _, tg := target.(*deliver.TelegramTarget); tg != (c.lock && c.enter) {
				t.Fatalf("captured %T with lock %v", target, c.lock)
			}
			if r.Deliver(target, "hello") == nil {
				if c.enter {
					r.Submit(target)
				}
				r.Received(target, "hello")
			}
			if got := l.get().Done; got != c.want {
				t.Errorf("done %q, want %q", got, c.want)
			}
		})
	}
}

func TestKeypadSend(t *testing.T) {
	g, err := keypadSend(app.KeypadConfig{Sender: "user", Via: "sender.example"})
	if err != nil || g.Argv[0] != "ssh" || !slices.Contains(g.Argv, "sender.example") {
		t.Errorf("default: %q %v", g.Argv, err)
	}
	g, err = keypadSend(app.KeypadConfig{Sender: "user", Send: []string{"my-send", "{chat}"}})
	if err != nil || !slices.Equal(g.Argv, []string{"my-send", "{chat}"}) {
		t.Errorf("override: %q %v", g.Argv, err)
	}
}

// While the keypad is away the lock shows nowhere, so a take goes to the
// Mac; the lock holds and routes to Telegram again once it is back.
func TestDeskLockNeedsTheKeypad(t *testing.T) {
	d, l := newTestDesk(t)
	d.event(keypad.Event{Kind: keypad.Key4})
	r := d.router(deskMac{}, deliver.Telegram{})
	l.mu.Lock()
	l.offline = true
	l.mu.Unlock()
	if _, tg := captureDesk(d, r).(session.EndTarget); tg {
		t.Fatal("keypad away: captured the locked target")
	}
	if _, tg := captureDesk(d, r).(session.EndTarget); tg {
		t.Fatal("keypad away, second take: captured the locked target")
	}
	l.mu.Lock()
	l.offline = false
	l.mu.Unlock()
	if _, tg := captureDesk(d, r).(session.EndTarget); !tg {
		t.Fatal("keypad back: the lock did not come back")
	}
}

// A saved lock on a chat keypad.json no longer lists is dropped: no take
// goes to another target.
func TestDeskDropsAGoneLock(t *testing.T) {
	path := filepath.Join(t.TempDir(), "keypad.json")
	body := `{"groups": [{"chat": -1002, "label": "蓝蟹"}], "chat": -1001, "topic": 7, "locked": true}`
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	l := &fakeLink{ev: make(chan keypad.Event)}
	d := newDesk(l, path)
	if _, ok := d.locked(); ok {
		t.Error("a lock on a gone chat survived the load")
	}
	if s := l.get(); s.Lock || s.Group != "蓝蟹" {
		t.Errorf("face %+v", s)
	}
}

// With neither via nor send set, startup says so and a locked take fails
// loudly (未送达, text on the clipboard), never silently.
func TestKeypadSendUnset(t *testing.T) {
	g, err := keypadSend(app.KeypadConfig{Sender: "user"})
	if err == nil {
		t.Error("no via and no send: want a startup error")
	}
	d, l := newTestDesk(t)
	d.event(keypad.Event{Kind: keypad.Key4})
	r := d.router(deskMac{}, g)
	if err := r.Deliver(captureDesk(d, r).(session.EndTarget).ForEnd(true), "hello"); err == nil || !strings.Contains(err.Error(), "via") {
		t.Errorf("locked take: %v, want a failure naming via", err)
	}
	if got := l.get().Done; got != keypad.DoneFailed {
		t.Errorf("done %q, want failed", got)
	}
}

func captureDesk(d *desk, r *deliver.Router) session.Target {
	if target, locked := d.locked(); locked {
		return r.CaptureKeypad(&target)
	}
	return r.Capture()
}

type warmDeskController struct{ recording string }

func (c warmDeskController) KeyPhase() session.State { return session.Idle }
func (c warmDeskController) Recording() (string, time.Duration, bool) {
	return c.recording, 0, c.recording != ""
}
func TestDeskLockedStartEnsuresSender(t *testing.T) {
	for _, tc := range []struct {
		name      string
		locked    bool
		recording string
		want      int
	}{{"locked start", true, "", 1}, {"cursor start", false, "", 0}, {"recording end", true, "take", 0}} {
		t.Run(tc.name, func(t *testing.T) {
			d, _ := newTestDesk(t)
			d.bind(warmDeskController{tc.recording}, 250*time.Millisecond, nil)
			calls := 0
			d.ensureSend = func() { calls++ }
			d.key1At(true, time.Now(), keypad.Snapshot{Lock: tc.locked, Chat: "688092930", TopicID: 1, Group: "Saved Messages"})
			if calls != tc.want {
				t.Fatalf("ensure calls=%d want=%d", calls, tc.want)
			}
		})
	}
}
