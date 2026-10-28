package main

import (
	"context"
	"errors"
	"log"
	"strconv"
	"sync/atomic"
	"time"

	"github.com/caoer/mega-asr/internal/app"
	"github.com/caoer/mega-asr/internal/deliver"
	"github.com/caoer/mega-asr/internal/keypad"
	"github.com/caoer/mega-asr/internal/mac"
	"github.com/caoer/mega-asr/internal/session"
)

// desk is the desk keypad joined to the controller: the knob moves the pick
// and key 4 locks key 1 to it (keypad.Targets); the keypad draws the take
// state, the end mode, the pick and the last delivery's outcome; and a take
// started by key 1 keeps the locked Telegram target (deliver.Router).
type desk struct {
	link           keypad.Link
	targets        *keypad.Targets
	offline        atomic.Bool // a locked take went to the Mac with the keypad away; logged once
	ctrl           deskController
	key1           mac.Detector
	onTap          func(time.Time, *deliver.TelegramTarget, string)
	key1Connection uint64
	windowHasKeys  func() bool
	downTarget     *deliver.TelegramTarget
	downRecording  string
	ensureSend     func()
}

type deskController interface {
	KeyPhase() session.State
	Recording() (string, time.Duration, bool)
}

// bind connects key 1 before run starts, using the Mac tap's hold window.
func (d *desk) bind(c deskController, window time.Duration, onTap func(time.Time, *deliver.TelegramTarget, string)) {
	d.ctrl = c
	d.onTap = onTap
	key, _ := mac.LookupTapKey("right_shift")
	d.key1 = mac.Detector{Key: key, Window: window}
}

// newDesk is the desk over link with the targets in path; a missing or
// unreadable file is no targets.
func newDesk(link keypad.Link, path string) *desk {
	t, err := keypad.LoadTargets(path)
	if err != nil {
		log.Printf("keypad: %s: %v; no targets", path, err)
	}
	d := &desk{link: link, targets: t}
	d.update(func(s *keypad.Snapshot) {
		s.State = session.Idle.String()
		t.Fill(s)
	})
	return d
}

// run keeps the keypad fed and acts on its events until ctx ends.
func (d *desk) run(ctx context.Context) {
	go d.link.Run(ctx)
	ev := d.link.Events()
	for {
		select {
		case <-ctx.Done():
			return
		case e, ok := <-ev:
			if !ok {
				return
			}
			d.event(e)
		}
	}
}

// event turns the knob's pick or key 4's lock, then shows the result.
func (d *desk) event(e keypad.Event) {
	switch e.Kind {
	case keypad.Key1Down, keypad.Key1Up:
		connection, _ := d.key1State()
		if !d.link.Connected() || e.Connection != 0 && e.Connection != connection {
			return
		}
		d.key1At(e.Kind == keypad.Key1Down, time.Now(), e.Shown)
		d.update(d.targets.Fill)
		return
	case keypad.Knob:
		if _, ready := d.key1State(); !ready {
			return
		}
		d.targets.Turn(e.N, e.Held)
	case keypad.Key4:
		if _, ready := d.key1State(); !ready {
			return
		}
		if locked, ok := d.targets.ToggleLock(); !ok {
			log.Printf("keypad: key 4: no targets to lock to")
		} else {
			log.Printf("keypad: locked %v", locked)
		}
	}
	d.update(d.targets.Fill)
}

// key1At feeds the link's button edges through the same detector as Right
// Shift. Each new down replaces a missed up, so the next tap still stops.
func (d *desk) key1At(down bool, at time.Time, shown keypad.Snapshot) {
	if d.ctrl == nil {
		return
	}
	connection, _ := d.key1State()
	if connection != d.key1Connection {
		d.key1.Reset()
		d.downTarget = nil
		d.key1Connection = connection
	}
	phase := phaseOf(d.ctrl.KeyPhase())
	if d.windowHasKeys != nil && d.windowHasKeys() {
		phase = mac.PhaseWindow
	}
	in := mac.Input{Kind: mac.KindFlags, Code: d.key1.Key.Code, At: at}
	if down {
		d.downTarget = nil
		if shown.Lock && shown.Chat != "" {
			d.downTarget = &deliver.TelegramTarget{Chat: shown.Chat, Topic: shown.TopicID, Group: shown.Group, TopicLabel: shown.Topic}
		}
		in.Flags = d.key1.Key.Class | d.key1.Key.DeviceBit
		d.downRecording, _, _ = d.ctrl.Recording()
		if d.downTarget != nil && d.downRecording == "" && d.ensureSend != nil {
			d.ensureSend()
		}
	} else if recording, _, _ := d.ctrl.Recording(); recording != d.downRecording {
		d.key1.Reset()
		d.downTarget = nil
	}
	if r := d.key1.Handle(in, phase); r.Tap && d.onTap != nil {
		d.onTap(at, d.downTarget, d.downRecording)
	}
}

func (d *desk) key1State() (uint64, bool) {
	if link, ok := d.link.(keypad.Key1Link); ok {
		return link.Key1State()
	}
	return 0, false
}

func (d *desk) update(f func(*keypad.Snapshot)) {
	_, ready := d.key1State()
	d.link.Update(func(s *keypad.Snapshot) {
		f(s)
		d.targets.Fill(s)
		if !ready {
			s.Lock = false
		}
	})
}

// view shows the current state/end mode. Lock and labels always describe
// the saved destination for the next key-1 take, including while processing.
func (d *desk) view(st session.State, v session.View) {
	d.update(func(s *keypad.Snapshot) { s.State = st.String(); s.Send = v.Send })
}

// outcome shows how the last delivery ended.
func (d *desk) outcome(o deliver.Outcome) {
	d.update(func(s *keypad.Snapshot) { s.Done = keypad.Done(o) })
}

// locked is the Telegram target key 1 is locked to, while the keypad is
// connected and shows the lock on key 1. With the keypad away (unplugged,
// its battery dead, not found yet) a take goes to the Mac; the saved lock
// holds for when it is back.
func (d *desk) locked() (deliver.TelegramTarget, bool) {
	p, ok := d.targets.Locked()
	if !ok {
		return deliver.TelegramTarget{}, false
	}
	if !d.link.Connected() {
		if !d.offline.Swap(true) {
			log.Printf("keypad: locked to %s but not connected; takes go to the Mac until it is back", p.Group)
		}
		return deliver.TelegramTarget{}, false
	}
	if _, ready := d.key1State(); !ready {
		return deliver.TelegramTarget{}, false
	}
	d.offline.Store(false)
	return deliver.TelegramTarget{Chat: strconv.FormatInt(p.Chat, 10), Topic: p.Topic,
		Group: p.Group, TopicLabel: p.TopicLabel}, true
}

// router captures the lock for keypad takes; Enter sends, a key-1 end pastes.
func (d *desk) router(mac session.Deliverer, send deliver.Telegram) *deliver.Router {
	return &deliver.Router{Mac: mac, Telegram: send, Outcome: d.outcome}
}

// keypadSend is the Telegram deliverer [keypad] names: its send argv, or
// ccc-telegram over ssh on via as the sender.
func keypadSend(c app.KeypadConfig) (deliver.Telegram, error) {
	if len(c.Send) > 0 {
		return deliver.Telegram{Argv: c.Send}, nil
	}
	if c.Via == "" {
		return deliver.Telegram{}, errors.New("[keypad] sets neither via nor send")
	}
	return deliver.TelegramCommand(c.Sender, c.Via)
}
