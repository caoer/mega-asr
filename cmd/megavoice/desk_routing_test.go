package main

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/caoer/mega-asr/internal/audio"
	"github.com/caoer/mega-asr/internal/deliver"
	"github.com/caoer/mega-asr/internal/keypad"
	"github.com/caoer/mega-asr/internal/session"
	"github.com/caoer/mega-asr/internal/store"
)

type deskRouteTarget struct {
	name     string
	released atomic.Bool
}

func (t *deskRouteTarget) String() string       { return t.name }
func (t *deskRouteTarget) Release()             { t.released.Store(true) }
func (t *deskRouteTarget) Record() store.Target { return store.Target{Kind: "app", App: t.name} }

type deskRouteMac struct {
	mu       sync.Mutex
	front    string
	target   *deskRouteTarget
	captured chan struct{}
	calls    []string
}

func (m *deskRouteMac) Capture() session.Target {
	m.mu.Lock()
	defer m.mu.Unlock()
	first := m.target == nil
	m.target = &deskRouteTarget{name: m.front}
	if first {
		close(m.captured)
	}
	return m.target
}
func (m *deskRouteMac) Deliver(t session.Target, text string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.calls = append(m.calls, "paste "+t.String()+" "+text)
	return nil
}
func (m *deskRouteMac) Submit(t session.Target) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.calls = append(m.calls, "enter "+t.String())
	return nil
}

type deskRouteUI struct {
	target chan string
	views  chan session.View
}

func (u deskRouteUI) Show(v session.View) {
	if u.views != nil {
		select {
		case u.views <- v:
		default:
		}
	}
	if v.Target != "" {
		select {
		case u.target <- v.Target:
		default:
		}
	}
}
func (deskRouteUI) Flash(string) {}
func (deskRouteUI) Alert(string) {}

// The real controller chooses at start by trigger and at delivery by end
// mode. The chat and cursor both stay fixed as focus and the lock change.
func TestDeskControllerRouting(t *testing.T) {
	for _, tc := range []struct {
		trigger, end string
	}{
		{"tap", "paste"}, {"tap", "enter"},
		{"menu", "enter"}, {"ctl", "enter"}, {"iphone", "enter"}, {"watch", "enter"},
		{"keypad", "paste"}, {"keypad", "enter"}, {"keypad", "enter later"}, {"keypad", "cancel"},
		{"keypad", "lost up then tap"},
		{"keypad", "lost up then auto"},
		{"keypad", "cold enter"},
	} {
		t.Run(tc.trigger+"/"+tc.end, func(t *testing.T) {
			d, link := newTestDesk(t)
			d.event(keypad.Event{Kind: keypad.Key4})
			mac := &deskRouteMac{front: "first window", captured: make(chan struct{})}
			sent := filepath.Join(t.TempDir(), "sent.txt")
			sender := deliver.Telegram{Argv: []string{"sh", "-c", `cat >"$1"; echo "Sent → message 31"`, "fixture", sent}}
			ui := deskRouteUI{target: make(chan string, 16), views: make(chan session.View, 32)}
			asrGate := make(gate)
			router := d.router(mac, sender)
			ctrl := &session.Controller{
				NewSource: func() audio.Source { return &heldMacInput{} },
				ASR:       gatedASR{asrGate}, Deliver: router, UI: ui,
				Store: store.Store{Dir: t.TempDir()}, ChunkDir: t.TempDir(), WholeMax: time.Minute,
				OnView: d.view,
			}
			if tc.end == "lost up then auto" {
				ctrl.MaxDuration = 100 * time.Millisecond
			}
			d.bind(ctrl, 400*time.Millisecond, func(at time.Time, target *deliver.TelegramTarget, recording string) {
				ctrl.ToggleKeypadAt(at, func() session.Target { return router.CaptureKeypad(target) }, recording)
			})
			if tc.end == "cold enter" {
				link.noKey1 = true
				d.view(session.Idle, session.View{})
				link.noKey1 = false // the first edge proves capability, before any lock frame
			}
			t0 := time.Now()
			tap := func(ms int) {
				shown := link.get()
				if tc.end == "cold enter" {
					shown.Lock = false
				}
				d.key1At(true, t0.Add(time.Duration(ms)*time.Millisecond), shown)
				d.key1At(false, t0.Add(time.Duration(ms+100)*time.Millisecond), link.get())
			}
			if tc.trigger == "keypad" {
				tap(0)
			} else {
				ctrl.ToggleFrom(tc.trigger)
			}
			select {
			case <-mac.captured:
			case <-time.After(2 * time.Second):
				t.Fatal("target capture stalled")
			}
			id, _, recording := ctrl.Recording()
			if !recording {
				t.Fatal("take did not start")
			}
			if tc.trigger == "keypad" && tc.end != "cold enter" {
				select {
				case label := <-ui.target:
					if label != "报到群 / ccc-status" {
						t.Fatalf("overlay target %q", label)
					}
				case <-time.After(2 * time.Second):
					t.Fatal("locked target never reached the view")
				}
			}
			if got := link.get(); !got.Lock {
				t.Fatalf("recording face lock %v for %s/%s", got.Lock, tc.trigger, tc.end)
			}
			mac.mu.Lock()
			mac.front = "later window"
			mac.mu.Unlock()
			d.event(keypad.Event{Kind: keypad.Key4}) // unlock and change the pick mid-take
			d.event(keypad.Event{Kind: keypad.Knob, N: 1})
			switch tc.end {
			case "paste":
				if tc.trigger == "keypad" {
					tap(1000)
				} else {
					ctrl.Toggle()
				}
			case "lost up then tap":
				d.key1At(true, t0.Add(time.Second), link.get()) // its up is lost on the link
				if _, _, ok := ctrl.Recording(); !ok {
					t.Fatal("a down alone ended the take")
				}
				tap(3000) // same recovery as a lost Right Shift up
				if _, _, ok := ctrl.Recording(); ok {
					t.Fatal("the next tap did not stop the take")
				}
			case "lost up then auto":
				d.key1At(true, t0.Add(time.Second), link.get())
				link.mu.Lock()
				link.offline = true
				link.mu.Unlock()
				for deadline := time.Now().Add(2 * time.Second); ctrl.KeyPhase() == session.Recording; {
					if time.Now().After(deadline) {
						t.Fatal("lost key1 up defeated the take limit")
					}
					time.Sleep(time.Millisecond)
				}
			case "enter later":
				tap(1000)
				if !ctrl.Enter() {
					t.Fatal("Enter while processing was not accepted")
				}
			case "enter", "cold enter":
				if !ctrl.Enter() {
					t.Fatal("Enter while recording was not accepted")
				}
			case "cancel":
				ctrl.Cancel()
			}
			if tc.end == "enter later" && link.get().Lock {
				t.Fatal("cursor-bound processing still shows the group lock")
			}
			if tc.end == "enter later" {
				processing := false
				for len(ui.views) > 0 {
					v := <-ui.views
					if v.Quality == nil && !v.Started.IsZero() {
						processing = true
						if v.Target != "" {
							t.Fatalf("cursor-bound processing names a group: %+v", v)
						}
					}
				}
				if !processing {
					t.Fatal("no processing view after paste end")
				}
			}
			close(asrGate)
			ctrl.Wait()
			b, err := os.ReadFile(sent)
			wantChat := tc.trigger == "keypad" && tc.end == "enter"
			if wantChat {
				if err != nil || string(b) != "words for the test" {
					t.Fatalf("chat text %q, %v", b, err)
				}
			} else if !os.IsNotExist(err) {
				t.Fatalf("unexpected chat send: %q, %v", b, err)
			}
			mac.mu.Lock()
			calls, released := strings.Join(mac.calls, ","), mac.target.released.Load()
			mac.mu.Unlock()
			wantCalls := "paste first window words for the test"
			if strings.Contains(tc.end, "enter") && !wantChat {
				wantCalls += ",enter first window"
			}
			if wantChat || tc.end == "cancel" {
				wantCalls = ""
			}
			if calls != wantCalls || !released {
				t.Fatalf("Mac calls %q, want %q; released %v", calls, wantCalls, released)
			}
			events, err := store.Read(filepath.Join(ctrl.Store.Dir, id))
			if err != nil {
				t.Fatal(err)
			}
			for _, e := range events {
				if e.Ev == "start" && e.Trigger != tc.trigger {
					t.Errorf("trigger %q, want %q", e.Trigger, tc.trigger)
				}
				if e.Ev == "deliver" && (e.Target == nil || (e.Target.Kind == "telegram") != wantChat) {
					t.Errorf("delivery target %+v, chat %v", e.Target, wantChat)
				}
			}
			wantDone := keypad.DonePasted
			if strings.Contains(tc.end, "enter") {
				wantDone = keypad.DoneSent
			}
			if tc.end != "cancel" && link.get().Done != wantDone {
				t.Errorf("done %q, want %q", link.get().Done, wantDone)
			}
			if tc.trigger != "keypad" || tc.end == "cold enter" {
				select {
				case label := <-ui.target:
					t.Errorf("Mac take gained a target label %q", label)
				default:
				}
			}
		})
	}
}

func TestDeskLegacyFirmwareHidesLock(t *testing.T) {
	d, link := newTestDesk(t)
	d.event(keypad.Event{Kind: keypad.Key4})
	link.noKey1 = true
	d.view(session.Idle, session.View{})
	d.event(keypad.Event{Kind: keypad.Key4})
	if link.get().Lock {
		t.Fatal("a HID-only keypad shows the lock")
	}
	if _, locked := d.locked(); locked {
		t.Fatal("a HID-only keypad enabled the locked route")
	}
	link.noKey1 = false
	d.event(keypad.Event{Kind: keypad.Key1Down})
	if !link.get().Lock {
		t.Fatal("link-capable key 1 did not enable the lock face")
	}
	if _, locked := d.locked(); !locked {
		t.Fatal("lock face has no matching route")
	}
}

func TestDeskKey1OrphanAndConnectionReset(t *testing.T) {
	d, link := newTestDesk(t)
	var taps int
	bind := func() {
		d.bind(&session.Controller{}, 400*time.Millisecond, func(time.Time, *deliver.TelegramTarget, string) { taps++ })
	}
	bind()
	d.event(keypad.Event{Kind: keypad.Key1Up, Connection: 1})
	if taps != 0 {
		t.Fatal("orphan up toggled a take")
	}
	d.event(keypad.Event{Kind: keypad.Key1Down, Connection: 1})
	link.connection = 2
	d.event(keypad.Event{Kind: keypad.Key1Down, Connection: 1}) // queued before the port closed
	d.event(keypad.Event{Kind: keypad.Key1Up, Connection: 2})
	if taps != 0 {
		t.Fatal("an edge pair crossed a port gap")
	}
	d.event(keypad.Event{Kind: keypad.Key1Down, Connection: 2})
	bind() // a restarted controller/detector has no remembered down
	d.event(keypad.Event{Kind: keypad.Key1Up, Connection: 2})
	if taps != 0 {
		t.Fatal("an up after restart toggled a take")
	}
	d.event(keypad.Event{Kind: keypad.Key1Down, Connection: 2})
	d.event(keypad.Event{Kind: keypad.Key1Up, Connection: 2})
	if taps != 1 {
		t.Fatal("the next complete tap did not recover")
	}
}

func TestDeskKey1RespectsModalWindow(t *testing.T) {
	d, _ := newTestDesk(t)
	var taps int
	d.bind(&session.Controller{}, 400*time.Millisecond, func(time.Time, *deliver.TelegramTarget, string) { taps++ })
	modal := true
	d.windowHasKeys = func() bool { return modal }
	d.event(keypad.Event{Kind: keypad.Key1Down})
	d.event(keypad.Event{Kind: keypad.Key1Up})
	if taps != 0 {
		t.Fatal("a keypad tap toggled while the modal had the keys")
	}
	d.event(keypad.Event{Kind: keypad.Key1Down})
	modal = false
	d.event(keypad.Event{Kind: keypad.Key1Up})
	if taps != 0 {
		t.Fatal("an edge pair crossed the modal guard")
	}
	d.event(keypad.Event{Kind: keypad.Key1Down})
	d.event(keypad.Event{Kind: keypad.Key1Up})
	if taps != 1 {
		t.Fatal("keypad tap did not recover after the modal")
	}
}

func TestDeskKey1UsesTapWindow(t *testing.T) {
	d, _ := newTestDesk(t)
	ctrl := &session.Controller{}
	var taps int
	d.bind(ctrl, 250*time.Millisecond, func(time.Time, *deliver.TelegramTarget, string) { taps++ })
	t0 := time.Now()
	d.key1At(true, t0, keypad.Snapshot{})
	d.key1At(false, t0.Add(251*time.Millisecond), keypad.Snapshot{})
	if taps != 0 {
		t.Fatal("long hold toggled a take")
	}
	d.key1At(true, t0.Add(time.Second), keypad.Snapshot{})
	d.key1At(false, t0.Add(time.Second+250*time.Millisecond), keypad.Snapshot{})
	if taps != 1 {
		t.Fatal("tap at the window edge did not toggle")
	}
}

func TestDeskHiddenControlsKeepSavedPick(t *testing.T) {
	for _, locked := range []bool{false, true} {
		d, link := newTestDesk(t)
		if locked {
			d.event(keypad.Event{Kind: keypad.Key4})
		}
		var before, after keypad.Snapshot
		d.targets.Fill(&before)
		link.noKey1 = true
		d.view(session.Idle, session.View{})
		d.event(keypad.Event{Kind: keypad.Knob, N: 1})
		d.event(keypad.Event{Kind: keypad.Key4})
		d.targets.Fill(&after)
		if before != after {
			t.Fatalf("hidden controls changed saved pick: %+v -> %+v", before, after)
		}
	}
}

func TestDeskKey1KeepsShownTargetAtDown(t *testing.T) {
	d, link := newTestDesk(t)
	d.event(keypad.Event{Kind: keypad.Key4})
	var got []*deliver.TelegramTarget
	d.bind(&session.Controller{}, time.Second, func(_ time.Time, target *deliver.TelegramTarget, _ string) { got = append(got, target) })
	t0 := time.Now()
	shown := link.get()
	// A first down cannot use a lock that appears only before its up.
	d.key1At(true, t0, keypad.Snapshot{})
	d.key1At(false, t0.Add(100*time.Millisecond), shown)
	if len(got) != 1 || got[0] != nil {
		t.Fatalf("cold tap target %+v", got)
	}
	// A queued tap keeps the target actually shown at down, even if the
	// next-take pick changes before its capture goroutine runs.
	d.key1At(true, t0.Add(time.Second), shown)
	d.event(keypad.Event{Kind: keypad.Key4})
	d.event(keypad.Event{Kind: keypad.Knob, N: 1})
	d.key1At(false, t0.Add(time.Second+100*time.Millisecond), link.get())
	if len(got) != 2 || got[1] == nil || got[1].Chat != "-1001" || got[1].Topic != 7 {
		t.Fatalf("shown target lost: %+v", got)
	}
}

func TestDeskViewKeepsNextTakePick(t *testing.T) {
	d, link := newTestDesk(t)
	d.event(keypad.Event{Kind: keypad.Key4})
	wave := func() []audio.Quality { return nil }
	d.view(session.Recording, session.View{Quality: wave})
	if s := link.get(); !s.Lock || s.Group != "报到群" || s.Chat != "-1001" || s.Send {
		t.Fatalf("Mac take face %+v", s)
	}
	d.view(session.Transcribing, session.View{Started: time.Now(), Send: true, Target: "Earlier group / Earlier topic"})
	if s := link.get(); !s.Lock || s.Group != "报到群" || s.Chat != "-1001" || s.TopicID != 7 || !s.Send {
		t.Fatalf("chat take face %+v", s)
	}
	d.event(keypad.Event{Kind: keypad.Key4})
	d.event(keypad.Event{Kind: keypad.Knob, N: 1})
	if s := link.get(); s.Lock || s.Group != "蓝蟹" {
		t.Fatalf("next pick was hidden by earlier take %+v", s)
	}
	d.view(session.Idle, session.View{})
	if s := link.get(); s.Lock || s.Group != "蓝蟹" {
		t.Fatalf("idle did not restore next pick %+v", s)
	}
}

type deskTakeASR struct{ gate <-chan struct{} }

func (a deskTakeASR) Transcribe(_ context.Context, wav string, _ bool) (string, error) {
	<-a.gate
	return filepath.Base(filepath.Dir(wav)), nil
}

func TestDeskNextKeypadTakeWhileTranscribing(t *testing.T) {
	for _, unlock := range []bool{false, true} {
		name := "lock stays on after Mac take"
		if unlock {
			name = "unlock during earlier chat take"
		}
		t.Run(name, func(t *testing.T) {
			d, link := newTestDesk(t)
			d.event(keypad.Event{Kind: keypad.Key4})
			mac := &deskRouteMac{front: "window", captured: make(chan struct{})}
			sent := filepath.Join(t.TempDir(), "sent.txt")
			sender := deliver.Telegram{Argv: []string{"sh", "-c", `cat >>"$1"; printf '\n' >>"$1"; echo "Sent → message 31"`, "fixture", sent}}
			router := d.router(mac, sender)
			gate := make(chan struct{})
			ctrl := &session.Controller{
				NewSource: func() audio.Source { return &heldMacInput{} },
				ASR:       deskTakeASR{gate}, Deliver: router, UI: phoneUI{},
				Store: store.Store{Dir: t.TempDir()}, ChunkDir: t.TempDir(), WholeMax: time.Minute, OnView: d.view,
			}
			d.bind(ctrl, 400*time.Millisecond, func(at time.Time, target *deliver.TelegramTarget, recording string) {
				ctrl.ToggleKeypadAt(at, func() session.Target { return router.CaptureKeypad(target) }, recording)
			})
			tap := func() {
				at := time.Now()
				d.key1At(true, at, link.get())
				d.key1At(false, at.Add(100*time.Millisecond), link.get())
			}
			if unlock {
				tap()
			} else {
				ctrl.ToggleFrom("tap")
			}
			first, _, ok := ctrl.Recording()
			if !ok {
				t.Fatal("earlier take did not start")
			}
			if unlock {
				if !ctrl.Enter() {
					t.Fatal("earlier chat take did not stop")
				}
			} else {
				ctrl.Toggle()
			}
			if ctrl.KeyPhase() != session.Transcribing {
				t.Fatal("earlier take is not processing")
			}
			if unlock {
				d.event(keypad.Event{Kind: keypad.Key4})
				d.event(keypad.Event{Kind: keypad.Knob, N: 1})
			}
			if s := link.get(); s.Lock == unlock {
				t.Fatalf("next-take lock %+v", s)
			}
			tap()
			second, _, ok := ctrl.Recording()
			if !ok || first == second {
				t.Fatal("next keypad take did not start while earlier take processed")
			}
			if !ctrl.Enter() {
				t.Fatal("next keypad take did not stop")
			}
			close(gate)
			ctrl.Wait()
			posted, err := os.ReadFile(sent)
			wantPost, wantCalls := second+"\n", "paste window "+first
			if unlock {
				wantPost, wantCalls = first+"\n", "paste window "+second+",enter window"
			}
			if err != nil || string(posted) != wantPost {
				t.Fatalf("posted %q, want %q (%v)", posted, wantPost, err)
			}
			mac.mu.Lock()
			calls := strings.Join(mac.calls, ",")
			mac.mu.Unlock()
			if calls != wantCalls {
				t.Fatalf("Mac calls %q, want %q", calls, wantCalls)
			}
			events, err := store.Read(filepath.Join(ctrl.Store.Dir, second))
			if err != nil {
				t.Fatal(err)
			}
			for _, e := range events {
				if e.Ev == "deliver" && (e.Target == nil || (e.Target.Kind == "telegram") == unlock) {
					t.Fatalf("next take target %+v, unlock %v", e.Target, unlock)
				}
			}
		})
	}
}
