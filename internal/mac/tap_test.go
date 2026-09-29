package mac

import (
	"slices"
	"strings"
	"testing"
	"time"
)

func TestDetector(t *testing.T) {
	rs := tapKeys["right_shift"]
	down := Input{Kind: KindFlags, Code: 60, Flags: flagShift | rs.DeviceBit}
	up := Input{Kind: KindFlags, Code: 60}
	key := Input{Kind: KindKeyDown, Code: 0}
	keyUp := Input{Kind: KindKeyUp, Code: 0}
	click := Input{Kind: KindPointer}
	cmdDown := Input{Kind: KindFlags, Code: 55, Flags: flagCommand | 0x08}
	cmdHeldShiftDown := Input{Kind: KindFlags, Code: 60, Flags: flagCommand | flagShift | rs.DeviceBit}
	leftShiftUp := Input{Kind: KindFlags, Code: 56, Flags: flagShift | rs.DeviceBit}
	esc := Input{Kind: KindKeyDown, Code: keyEscape}
	escUp := Input{Kind: KindKeyUp, Code: keyEscape}
	ret := Input{Kind: KindKeyDown, Code: keyReturn}
	retUp := Input{Kind: KindKeyUp, Code: keyReturn}
	shiftRet := Input{Kind: KindKeyDown, Code: keyReturn, Flags: flagShift | 0x02}
	shiftRetUp := Input{Kind: KindKeyUp, Code: keyReturn, Flags: flagShift | 0x02}
	padEnter := Input{Kind: KindKeyDown, Code: keyKeypadEnter, Flags: 0x200000} // NumericPad flag
	aDown := Input{Kind: KindKeyDown, Code: 0}                                  // 'a' has keycode 0
	const idle, rec, proc = PhaseIdle, PhaseRecording, PhaseProcessing

	type step struct {
		in    Input
		ms    int // time since the sequence start
		phase Phase
		want  Result
	}
	for _, c := range []struct {
		name  string
		steps []step
	}{
		{"tap", []step{{down, 0, idle, Result{}}, {up, 120, idle, Result{Tap: true}}}},
		{"tap at the window edge", []step{{down, 0, idle, Result{}}, {up, 400, idle, Result{Tap: true}}}},
		{"held too long", []step{{down, 0, idle, Result{}}, {up, 401, idle, Result{}}}},
		{"shift+letter", []step{{down, 0, idle, Result{}}, {key, 50, idle, Result{}}, {up, 100, idle, Result{}}}},
		{"rolling key-up", []step{{down, 0, idle, Result{}}, {keyUp, 30, idle, Result{}}, {up, 90, idle, Result{}}}},
		{"shift+click", []step{{down, 0, idle, Result{}}, {click, 50, idle, Result{}}, {up, 100, idle, Result{}}}},
		{"chord with cmd held", []step{{cmdDown, 0, idle, Result{}}, {cmdHeldShiftDown, 10, idle, Result{}}, {up, 80, idle, Result{}}}},
		{"other shift during hold", []step{{down, 0, idle, Result{}}, {leftShiftUp, 40, idle, Result{}}, {up, 90, idle, Result{}}}},
		{"esc idle passes", []step{{esc, 0, idle, Result{}}, {escUp, 50, idle, Result{}}}},
		{"esc recording cancels and is swallowed", []step{
			{esc, 0, rec, Result{Cancel: true, Swallow: true}},
			{escUp, 60, idle, Result{Swallow: true}}, // its up follows even once idle
		}},
		{"tap while recording stops", []step{{down, 0, rec, Result{}}, {up, 100, rec, Result{Tap: true}}}},
		{"key during recording is not a tap and passes", []step{{key, 0, rec, Result{}}}},
		{"tap after a pause in typing", []step{{key, 0, idle, Result{}}, {down, 500, idle, Result{}}, {up, 600, idle, Result{Tap: true}}}},
		{"lone tap while typing", []step{{key, 0, idle, Result{}}, {down, 200, idle, Result{}}, {up, 300, idle, Result{}}}},
		{"shift+letter then a quick tap", []step{{down, 0, idle, Result{}}, {key, 40, idle, Result{}}, {up, 90, idle, Result{}}, {down, 250, idle, Result{}}, {up, 330, idle, Result{}}}},
		{"stop tap right after a key", []step{{key, 0, rec, Result{}}, {down, 100, rec, Result{}}, {up, 180, rec, Result{Tap: true}}}},
		{"enter idle passes", []step{{ret, 0, idle, Result{}}, {retUp, 50, idle, Result{}}}},
		{"enter recording stops and is swallowed with its repeat and up", []step{
			{ret, 0, rec, Result{Enter: true, Swallow: true}},
			{ret, 500, proc, Result{Swallow: true}}, // autorepeat
			{retUp, 600, idle, Result{Swallow: true}},
			{aDown, 700, idle, Result{}}, // keycode 0 is not mistaken for the taken Return
		}},
		{"enter while processing upgrades", []step{{ret, 0, proc, Result{Enter: true, Swallow: true}}, {retUp, 60, proc, Result{Swallow: true}}}},
		{"keypad enter counts", []step{{padEnter, 0, rec, Result{Enter: true, Swallow: true}}}},
		{"shift+enter passes while recording", []step{{shiftRet, 0, rec, Result{}}, {shiftRetUp, 40, rec, Result{}}}},
		{"esc while processing passes", []step{{esc, 0, proc, Result{}}, {escUp, 40, proc, Result{}}}},
		{"enter between down and up of the tap key is not a tap", []step{{down, 0, rec, Result{}}, {ret, 50, rec, Result{Enter: true, Swallow: true}}, {up, 100, proc, Result{}}}},
	} {
		d := Detector{Key: rs, Window: 400 * time.Millisecond, Quiet: 500 * time.Millisecond}
		t0 := time.Now()
		for i, s := range c.steps {
			s.in.At = t0.Add(time.Duration(s.ms) * time.Millisecond)
			if got := d.Handle(s.in, s.phase); !sameResult(got, s.want) {
				t.Errorf("%s: step %d: got %+v, want %+v", c.name, i, got, s.want)
			}
		}
	}
}

// TestDetectorWhileRecording: what the detector returns for the sequences a
// key that does not stop a recording take can come from — a late key-up, a
// modifier held with the tap key, a lost key-down or key-up.
func TestDetectorWhileRecording(t *testing.T) {
	rs := tapKeys["right_shift"]
	down := Input{Kind: KindFlags, Code: 60, Flags: flagShift | rs.DeviceBit}
	up := Input{Kind: KindFlags, Code: 60}
	with := func(f uint64) Input { return Input{Kind: KindFlags, Code: 60, Flags: flagShift | rs.DeviceBit | f} }
	bothShifts := with(0x02)          // left Shift held too: same class
	capsLock := with(0x10000)         // alpha shift is not a modifier here
	fnHeld := with(flagFn)            // fn held
	ctrlHeld := with(flagControl | 1) // left Control held
	esc := Input{Kind: KindKeyDown, Code: keyEscape}
	escUp := Input{Kind: KindKeyUp, Code: keyEscape}
	chordEsc := Input{Kind: KindKeyDown, Code: keyEscape, Flags: flagShift | rs.DeviceBit | 0x100}
	shiftRet := Input{Kind: KindKeyDown, Code: keyReturn, Flags: flagShift | rs.DeviceBit}
	retUp := Input{Kind: KindKeyUp, Code: keyReturn}
	const rec = PhaseRecording

	type step struct {
		in   Input
		ms   int
		want Result
	}
	for _, c := range []struct {
		name  string
		steps []step
	}{
		{"key-up 401 ms after its key-down", []step{{down, 0, Result{Why: "armed"}}, {up, 401, Result{Why: "held 401ms, past the 400ms window"}}}},
		{"key-up 900 ms after its key-down", []step{{down, 0, Result{}}, {up, 900, Result{Why: "held 900ms, past the 400ms window"}}}},
		{"both Shifts down", []step{{bothShifts, 0, Result{}}, {up, 100, Result{Tap: true, Why: "up after 100ms"}}}},
		{"caps lock on", []step{{capsLock, 0, Result{}}, {up, 100, Result{Tap: true}}}},
		{"fn flag set at key-down", []step{{fnHeld, 0, Result{Why: "not armed: another modifier was held at its key-down"}}, {up, 100, Result{Why: "not armed: another modifier was held at its key-down"}}}},
		{"control flag set at key-down", []step{{ctrlHeld, 0, Result{}}, {up, 100, Result{Why: "not armed: another modifier was held at its key-down"}}}},
		{"key-down lost: a key-up alone", []step{{up, 0, Result{Why: "no key-down came before it"}}}},
		{"key-up lost: the next tap still stops", []step{{down, 0, Result{}}, {down, 2000, Result{}}, {up, 2100, Result{Tap: true}}}},
		{"two key-downs then a key-up in the window", []step{{down, 0, Result{}}, {down, 50, Result{}}, {up, 100, Result{Tap: true}}}},
		{"Esc key-down lost: a key-up alone passes", []step{{escUp, 0, Result{}}}},
		{"Esc with Right Shift held is the chord, not a cancel", []step{{down, 0, Result{}}, {chordEsc, 150, Result{Chord: true, Swallow: true}}, {escUp, 200, Result{Swallow: true}}, {up, 300, Result{Why: "not armed: Esc went down while it was held"}}}},
		{"Return with Right Shift held passes", []step{{down, 0, Result{}}, {shiftRet, 100, Result{}}, {retUp, 150, Result{}}, {up, 200, Result{Why: "not armed: another key or a click came while it was held"}}}},
		{"Esc alone cancels", []step{{esc, 0, Result{Cancel: true, Swallow: true}}}},
	} {
		d := Detector{Key: rs, Window: 400 * time.Millisecond, Quiet: 500 * time.Millisecond}
		t0 := time.Now()
		for i, s := range c.steps {
			s.in.At = t0.Add(time.Duration(s.ms) * time.Millisecond)
			if got := d.Handle(s.in, rec); !sameResult(got, s.want) {
				t.Errorf("%s: step %d: got %+v, want %+v", c.name, i, got, s.want)
			}
		}
	}
}

// The chord is Esc going down while the tap key is held. Each phase gets
// one continuous stream of events (gap: milliseconds since the previous
// one), checked event by event against Handle's branches: the key's hold
// comes from its own flag events or its device bit on the Esc, the chord is taken with its Esc's repeats
// and key-up, the key's release after it is no tap, a flags event of
// another modifier without the key's bit ends the hold, and a megavoice
// window takes nothing.
func TestChord(t *testing.T) {
	ro := tapKeys["right_option"]
	press := Input{Kind: KindFlags, Code: ro.Code, Flags: flagOption | ro.DeviceBit}
	release := Input{Kind: KindFlags, Code: ro.Code}
	esc := Input{Kind: KindKeyDown, Code: keyEscape, Flags: flagOption | ro.DeviceBit}
	plainEsc := Input{Kind: KindKeyDown, Code: keyEscape}
	escAgain := Input{Kind: KindKeyDown, Code: keyEscape, Repeat: true}
	escUp := Input{Kind: KindKeyUp, Code: keyEscape}
	ctrl := Input{Kind: KindFlags, Code: 59, Flags: flagControl | 0x01}
	letter := Input{Kind: KindKeyDown, Code: 4}
	chord, taken, none := Result{Chord: true, Swallow: true}, Result{Swallow: true}, Result{}
	afterChord := Result{Why: "not armed: Esc went down while it was held"}

	type ev struct {
		gap  int
		in   Input
		want Result
	}
	streams := map[Phase][]ev{
		PhaseIdle: {
			{0, esc, chord}, {35, escUp, taken}, // the key's device bit on an Esc, no press seen: the chord
			{620, press, none}, {145, plainEsc, chord}, {410, escAgain, taken}, {25, escUp, taken}, {90, release, afterChord},
			{840, press, none}, {115, release, Result{Tap: true}},
			{730, plainEsc, none}, {45, press, none}, {215, escAgain, none}, {35, escUp, none}, {55, release, none},
			{1100, press, none}, {1450, ctrl, none}, {420, plainEsc, none},
		},
		PhaseRecording: {
			{0, press, none}, {255, esc, chord}, {35, escUp, taken}, {140, release, afterChord},
			{520, plainEsc, Result{Cancel: true, Swallow: true}}, {45, escUp, taken},
		},
		PhaseProcessing: {
			{0, letter, none}, {45, press, none}, {215, esc, chord}, {35, escUp, taken},
		},
		PhaseWindow: {
			{0, press, Result{Why: windowWhy}}, {125, plainEsc, Result{Why: windowWhy}}, {35, escUp, Result{Why: windowWhy}},
		},
	}
	for phase, evs := range streams {
		d := Detector{Key: ro, Window: 400 * time.Millisecond, Quiet: 500 * time.Millisecond}
		at := time.Now()
		for i, e := range evs {
			at = at.Add(time.Duration(e.gap) * time.Millisecond)
			e.in.At = at
			if got := d.Handle(e.in, phase); !sameResult(got, e.want) {
				t.Errorf("phase %d, event %d: got %+v, want %+v", phase, i, got, e.want)
			}
		}
	}
}

// At an Esc's key-down the chord needs the tap key held. The Esc's flags
// carrying the key's device bit settle it without asking the system; flags
// without the bit leave it to the system's key state, whether the key's
// events say it is down (a key-up the tap missed) or not (a key-down it
// never saw).
func TestChordHeldAsked(t *testing.T) {
	ro := tapKeys["right_option"]
	press := Input{Kind: KindFlags, Code: ro.Code, Flags: flagOption | ro.DeviceBit}
	release := Input{Kind: KindFlags, Code: ro.Code}
	key := Input{Kind: KindKeyDown, Code: 7}
	plainEsc := Input{Kind: KindKeyDown, Code: keyEscape, Flags: 0x100}
	bitEsc := Input{Kind: KindKeyDown, Code: keyEscape, Flags: flagOption | ro.DeviceBit | 0x100}
	escUp := Input{Kind: KindKeyUp, Code: keyEscape}
	chord, cancel, taken, none := Result{Chord: true, Swallow: true}, Result{Cancel: true, Swallow: true}, Result{Swallow: true}, Result{}
	type ev struct {
		gap  int
		in   Input
		want Result
	}
	for _, c := range []struct {
		name  string
		held  bool // the system's answer
		phase Phase
		asked int // times the system is asked
		evs   []ev
	}{
		{"its key-up lost, idle: the Esc passes", false, PhaseIdle, 1,
			[]ev{{0, press, none}, {1300, key, none}, {2100, plainEsc, none}, {90, escUp, none}}},
		{"its key-up lost, recording: the Esc cancels", false, PhaseRecording, 1,
			[]ev{{0, press, none}, {1300, key, none}, {2100, plainEsc, cancel}, {90, escUp, taken}}},
		{"its events and the system agree: the chord", true, PhaseIdle, 1,
			[]ev{{0, press, none}, {260, plainEsc, chord}, {70, escUp, taken}}},
		{"its key-down never seen, recording: the chord, not a cancel", true, PhaseRecording, 1,
			[]ev{{0, plainEsc, chord}, {70, escUp, taken}, {140, release, none}}},
		{"its key-down never seen, idle: the chord, and its release is no tap", true, PhaseIdle, 1,
			[]ev{{0, plainEsc, chord}, {70, escUp, taken}, {140, release, Result{Why: "not armed: its key-down never reached the tap"}}}},
		{"not held, recording: a lone Esc cancels", false, PhaseRecording, 1,
			[]ev{{0, plainEsc, cancel}, {70, escUp, taken}}},
		{"the device bit on the Esc: the chord, the system not asked", false, PhaseRecording, 0,
			[]ev{{0, bitEsc, chord}, {70, escUp, taken}}},
		{"the device bit after its key-down: the chord, the system not asked", false, PhaseIdle, 0,
			[]ev{{0, press, none}, {180, bitEsc, chord}, {70, escUp, taken}}},
	} {
		asked := 0
		d := Detector{Key: ro, Window: 400 * time.Millisecond, Quiet: 500 * time.Millisecond,
			Held: func(code int) bool {
				if code != ro.Code {
					t.Errorf("%s: asked for key %d", c.name, code)
				}
				asked++
				return c.held
			}}
		at := time.Now()
		for i, e := range c.evs {
			at = at.Add(time.Duration(e.gap) * time.Millisecond)
			e.in.At = at
			if got := d.Handle(e.in, c.phase); !sameResult(got, e.want) {
				t.Errorf("%s: event %d: got %+v, want %+v", c.name, i, got, e.want)
			}
		}
		if asked != c.asked {
			t.Errorf("%s: the system asked %d times, want %d", c.name, asked, c.asked)
		}
	}
}

// Reset forgets a taken Esc and a taken Return, and takes the tap key's
// held state from the system: after it a plain Esc passes when idle and
// cancels while recording, a taken key's up passes, and a release reads as
// the release of a key the system held.
func TestDetectorReset(t *testing.T) {
	ro := tapKeys["right_option"]
	t0 := time.Now()
	at := func(ms int) time.Time { return t0.Add(time.Duration(ms) * time.Millisecond) }
	press := Input{Kind: KindFlags, Code: ro.Code, Flags: flagOption | ro.DeviceBit, At: at(0)}
	release := Input{Kind: KindFlags, Code: ro.Code, At: at(3000)}
	esc := Input{Kind: KindKeyDown, Code: keyEscape, At: at(1000)}
	escUp := Input{Kind: KindKeyUp, Code: keyEscape, At: at(2000)}
	ret := Input{Kind: KindKeyDown, Code: keyReturn, At: at(1000)}
	retUp := Input{Kind: KindKeyUp, Code: keyReturn, At: at(2000)}

	d := Detector{Key: ro, Window: 400 * time.Millisecond}
	d.Handle(press, PhaseIdle)
	d.Reset()
	if got := d.Handle(esc, PhaseIdle); got != (Result{}) {
		t.Errorf("idle Esc after Reset: %+v", got)
	}
	d.Handle(press, PhaseRecording)
	d.Reset()
	if got := d.Handle(esc, PhaseRecording); got != (Result{Cancel: true, Swallow: true}) {
		t.Errorf("recording Esc after Reset: %+v", got)
	}
	d.Reset()
	if got := d.Handle(escUp, PhaseRecording); got.Swallow {
		t.Errorf("a taken Esc's up after Reset: %+v", got)
	}
	if got := d.Handle(ret, PhaseRecording); got != (Result{Enter: true, Swallow: true}) {
		t.Fatalf("a bare Return while recording: %+v", got)
	}
	d.Reset()
	if got := d.Handle(retUp, PhaseRecording); got.Swallow {
		t.Errorf("a taken Return's up after Reset: %+v", got)
	}

	for _, c := range []struct {
		name string
		held bool
		why  string
	}{
		{"the system holds the key", true, "not armed: its key-down came before the tap was reset"},
		{"the system has it up", false, "no key-down came before it"},
	} {
		asked := 0
		d := Detector{Key: ro, Window: 400 * time.Millisecond, Held: func(int) bool { asked++; return c.held }}
		d.Handle(press, PhaseIdle)
		d.Reset()
		if asked != 1 {
			t.Errorf("%s: the system asked %d times at Reset, want 1", c.name, asked)
		}
		if got := d.Handle(release, PhaseIdle); got != (Result{Why: c.why}) {
			t.Errorf("%s: the release after Reset: %+v, want Why %q", c.name, got, c.why)
		}
	}
}

// sameResult compares a result with a wanted one, and its Why only when
// the wanted one states it.
func sameResult(got, want Result) bool {
	if want.Why == "" {
		got.Why = ""
	}
	return got == want
}

// While a take records, each tap-key, Esc and Return event gets one line with
// what the detector returned; any other key none, and those keys none
// outside a recording.
func TestKeyLine(t *testing.T) {
	rs := tapKeys["right_shift"]
	dev := Device{Keyboard: 40, Source: "hid"}
	for _, c := range []struct {
		name  string
		in    Input
		phase Phase
		r     Result
		want  string // "" for no line
	}{
		{"right shift down", Input{Kind: KindFlags, Code: 60, Flags: 0x20104}, PhaseRecording, Result{Why: "armed"},
			"tap: right_shift down → none: armed flags=0x20104 kbd=40 src=hid pid=0"},
		{"right shift up, late", Input{Kind: KindFlags, Code: 60, Flags: 0x100}, PhaseRecording, Result{Why: "held 512ms, past the 400ms window"},
			"tap: right_shift up → none: held 512ms, past the 400ms window flags=0x100 kbd=40 src=hid pid=0"},
		{"right shift up, tap", Input{Kind: KindFlags, Code: 60, Flags: 0x100}, PhaseRecording, Result{Tap: true, Why: "up after 120ms"},
			"tap: right_shift up → tap: up after 120ms flags=0x100 kbd=40 src=hid pid=0"},
		{"esc", Input{Kind: KindKeyDown, Code: keyEscape, Flags: 0x100}, PhaseRecording, Result{Cancel: true, Swallow: true},
			"tap: esc down → cancel, swallowed flags=0x100 kbd=40 src=hid pid=0"},
		{"return", Input{Kind: KindKeyDown, Code: keyReturn, Flags: 0x100}, PhaseRecording, Result{Enter: true, Swallow: true},
			"tap: return down → enter, swallowed flags=0x100 kbd=40 src=hid pid=0"},
		{"shift+return", Input{Kind: KindKeyDown, Code: keyReturn, Flags: 0x20104}, PhaseRecording, Result{},
			"tap: return down → none: passed on, a modifier is held flags=0x20104 kbd=40 src=hid pid=0"},
		{"keypad enter up", Input{Kind: KindKeyUp, Code: keyKeypadEnter, Flags: 0x200100}, PhaseRecording, Result{Swallow: true},
			"tap: keypad_enter up → none, swallowed flags=0x200100 kbd=40 src=hid pid=0"},
		{"chord", Input{Kind: KindKeyDown, Code: keyEscape, Flags: 0x20104}, PhaseRecording, Result{Chord: true, Swallow: true},
			"tap: esc down → chord, swallowed flags=0x20104 kbd=40 src=hid pid=0"},
		{"esc up", Input{Kind: KindKeyUp, Code: keyEscape, Flags: 0x100}, PhaseRecording, Result{Swallow: true},
			"tap: esc up → none, swallowed flags=0x100 kbd=40 src=hid pid=0"},
		{"return up", Input{Kind: KindKeyUp, Code: keyReturn, Flags: 0x100}, PhaseRecording, Result{Swallow: true},
			"tap: return up → none, swallowed flags=0x100 kbd=40 src=hid pid=0"},
		{"return repeat", Input{Kind: KindKeyDown, Code: keyReturn, Repeat: true}, PhaseRecording, Result{Swallow: true}, ""},
		{"a letter", Input{Kind: KindKeyDown, Code: 0}, PhaseRecording, Result{}, ""},
		{"a letter up", Input{Kind: KindKeyUp, Code: 0}, PhaseRecording, Result{}, ""},
		{"space up", Input{Kind: KindKeyUp, Code: 49}, PhaseRecording, Result{}, ""},
		{"left shift up", Input{Kind: KindFlags, Code: 56}, PhaseRecording, Result{}, ""},
		{"right option", Input{Kind: KindFlags, Code: 61, Flags: 0x80140}, PhaseRecording, Result{}, ""},
		{"left shift", Input{Kind: KindFlags, Code: 56, Flags: 0x20102}, PhaseRecording, Result{}, ""},
		{"a click", Input{Kind: KindPointer}, PhaseRecording, Result{}, ""},
		{"right shift idle", Input{Kind: KindFlags, Code: 60, Flags: 0x20104}, PhaseIdle, Result{}, ""},
		{"esc idle", Input{Kind: KindKeyDown, Code: keyEscape}, PhaseIdle, Result{}, ""},
		{"return while transcribing", Input{Kind: KindKeyDown, Code: keyReturn}, PhaseProcessing, Result{Enter: true, Swallow: true}, ""},
	} {
		line, ok := KeyLine(rs, c.in, c.phase, c.r, dev)
		if ok != (c.want != "") || line != c.want {
			t.Errorf("%s: got %q (%v), want %q", c.name, line, ok, c.want)
		}
	}
}

// A key-up whose key-down came before the take began says nothing about
// that time.
func TestNoReasonFromBeforeTheTake(t *testing.T) {
	rs := tapKeys["right_shift"]
	d := Detector{Key: rs, Window: 400 * time.Millisecond, Quiet: 500 * time.Millisecond}
	t0 := time.Now()
	d.Handle(Input{Kind: KindKeyDown, Code: 0, At: t0}, PhaseIdle)                                                             // typing
	d.Handle(Input{Kind: KindFlags, Code: 60, Flags: flagShift | rs.DeviceBit, At: t0.Add(200 * time.Millisecond)}, PhaseIdle) // not armed: typing
	r := d.Handle(Input{Kind: KindFlags, Code: 60, At: t0.Add(900 * time.Millisecond)}, PhaseRecording)
	if r.Tap || r.Why != "its key-down came before this take" {
		t.Fatalf("got %+v", r)
	}
}

// While a megavoice modal window has the keys — one is open (modal) and
// megavoice is the active app — the tap takes nothing: Return and Esc go to
// the window, a tap of the key stops nothing, and the key lines say so. A
// modal left open behind another app takes no key: the recording still
// stops on Return and on the tap key. In Idle a tap starts nothing while the
// window has the keys.
// An Esc or Return taken before the window opened has its key-up taken too.
func TestTapCoreWindow(t *testing.T) {
	rs := tapKeys["right_shift"]
	dev := func() Device { return Device{Keyboard: 40, Source: "hid"} }
	active := true
	newCore := func(p Phase) *tapCore {
		c := &tapCore{det: Detector{Key: rs, Window: 400 * time.Millisecond, Quiet: 500 * time.Millisecond}, keys: NewKeyLog(rs, 16), active: func() bool { return active }}
		c.setPhase(p)
		return c
	}
	t0 := time.Now()
	at := func(ms int) time.Time { return t0.Add(time.Duration(ms) * time.Millisecond) }
	ret := func(ms int) Input { return Input{Kind: KindKeyDown, Code: keyReturn, At: at(ms)} }
	retUp := func(ms int) Input { return Input{Kind: KindKeyUp, Code: keyReturn, At: at(ms)} }
	down := func(ms int) Input {
		return Input{Kind: KindFlags, Code: 60, Flags: flagShift | rs.DeviceBit, At: at(ms)}
	}
	up := func(ms int) Input { return Input{Kind: KindFlags, Code: 60, At: at(ms)} }

	var c *tapCore
	modal(func() {
		c = newCore(PhaseRecording)
		for i, s := range []struct {
			in   Input
			want Result
		}{
			{ret(0), Result{Why: windowWhy}},
			{retUp(50), Result{Why: windowWhy}},
			{Input{Kind: KindKeyDown, Code: keyEscape, At: at(100)}, Result{Why: windowWhy}},
			{down(200), Result{Why: windowWhy}},
			{up(260), Result{Why: windowWhy}},
		} {
			if got := c.event(s.in, dev); got != s.want {
				t.Errorf("window step %d: got %+v, want %+v", i, got, s.want)
			}
		}

		active = false // the modal is left open behind another app
		b := newCore(PhaseRecording)
		if got := b.event(ret(0), dev); !got.Enter || !got.Swallow {
			t.Errorf("Return with the modal behind another app: %+v", got)
		}
		b = newCore(PhaseRecording)
		b.event(down(0), dev)
		if got := b.event(up(100), dev); !got.Tap {
			t.Errorf("a tap with the modal behind another app: %+v", got)
		}
		active = true
		i := newCore(PhaseIdle)
		i.event(down(0), dev)
		if got := i.event(up(100), dev); got.Tap || got.Why != windowWhy {
			t.Errorf("a tap in Idle with the modal active started a take: %+v", got)
		}

		e := newCore(PhaseRecording)
		active = false
		e.event(Input{Kind: KindKeyDown, Code: keyEscape, At: at(0)}, dev)
		e.event(ret(10), dev)
		active = true
		if got := e.event(Input{Kind: KindKeyUp, Code: keyEscape, At: at(80)}, dev); !got.Swallow {
			t.Errorf("the up of an Esc taken before the window: %+v", got)
		}
		if got := e.event(retUp(90), dev); !got.Swallow {
			t.Errorf("the up of a Return taken before the window: %+v", got)
		}
	})
	close(c.keys.ch)
	var lines []string
	c.keys.Run(func(l string) { lines = append(lines, l) })
	if len(lines) != 5 || lines[0] != "tap: return down → none: "+windowWhy+" flags=0x0 kbd=40 src=hid pid=0" {
		t.Errorf("lines %q", lines)
	}

	c = newCore(PhaseRecording) // no modal open
	if got := c.event(ret(0), dev); !got.Enter || !got.Swallow {
		t.Errorf("Return with no window: %+v", got)
	}
	c = newCore(PhaseRecording)
	c.event(ret(0), dev)
	c.setPhase(PhaseProcessing)
	c.event(Input{Kind: KindKeyDown, Code: keyReturn, At: at(500), Repeat: true}, dev)
	if n := len(c.keys.ch); n != 1 {
		t.Errorf("%d key lines queued for a Return and its repeat, want 1", n)
	}
}

// A key-up in a later recording than its key-down gives no reason or hold
// time from the earlier one, however the phase got there.
func TestNoReasonFromAnEarlierTake(t *testing.T) {
	rs := tapKeys["right_shift"]
	c := &tapCore{det: Detector{Key: rs, Window: 400 * time.Millisecond, Quiet: 500 * time.Millisecond}}
	dev := func() Device { return Device{} }
	t0 := time.Now()
	c.setPhase(PhaseRecording)
	c.event(Input{Kind: KindFlags, Code: 60, Flags: flagShift | rs.DeviceBit, At: t0}, dev)
	c.setPhase(PhaseProcessing) // take A stopped from the menu
	c.setPhase(PhaseRecording)  // take B started by ctl; no key event in between
	r := c.event(Input{Kind: KindFlags, Code: 60, At: t0.Add(2 * time.Second)}, dev)
	if r.Tap || r.Why != "its key-down came before this take" {
		t.Fatalf("got %+v", r)
	}
	if n := c.takes.Load(); n != 2 {
		t.Fatalf("%d recordings counted, want 2", n)
	}
}

// The key log never blocks the tap: a full queue drops lines and counts
// them, and the next line written says how many.
func TestKeyLogNeverBlocks(t *testing.T) {
	rs := tapKeys["right_shift"]
	k := NewKeyLog(rs, 2)
	rec := keyRec{in: Input{Kind: KindKeyDown, Code: keyEscape}, phase: PhaseRecording, r: Result{Cancel: true, Swallow: true}}
	done := make(chan struct{})
	go func() {
		for range 5 {
			k.Add(rec)
		}
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("Add blocked with no reader")
	}
	close(k.ch)
	var lines []string
	k.Run(func(l string) { lines = append(lines, l) })
	want := []string{"tap: 3 key lines dropped: the log fell behind", "tap: esc down → cancel, swallowed flags=0x0 kbd=0 src= pid=0", "tap: esc down → cancel, swallowed flags=0x0 kbd=0 src= pid=0"}
	if !slices.Equal(lines, want) {
		t.Fatalf("lines %q, want %q", lines, want)
	}
}

// Choose's answer names a button only when runModal's answer is one of them.
func TestChoiceIndex(t *testing.T) {
	for resp, want := range map[int]int{1000: 0, 1002: 2, 1003: -1, 999: -1, -1000: -1} {
		if got := choiceIndex(resp, 3); got != want {
			t.Errorf("choiceIndex(%d, 3) = %d, want %d", resp, got, want)
		}
	}
}

// Through tapCore.event, the chord while a take records is taken and
// queues its key line: `tap: esc down → chord, swallowed`.
func TestTapCoreChordLine(t *testing.T) {
	rs := tapKeys["right_shift"]
	c := &tapCore{det: Detector{Key: rs, Window: 400 * time.Millisecond}, keys: NewKeyLog(rs, 16)}
	c.setPhase(PhaseRecording)
	t0 := time.Now()
	dev := func() Device { return Device{Keyboard: 40, Source: "hid"} }
	c.event(Input{Kind: KindFlags, Code: 60, Flags: flagShift | rs.DeviceBit, At: t0}, dev)
	if got := c.event(Input{Kind: KindKeyDown, Code: keyEscape, Flags: flagShift | rs.DeviceBit, At: t0.Add(150 * time.Millisecond)}, dev); got != (Result{Chord: true, Swallow: true}) {
		t.Fatalf("the chord: %+v", got)
	}
	close(c.keys.ch)
	var lines []string
	c.keys.Run(func(l string) { lines = append(lines, l) })
	if len(lines) != 2 || !strings.HasPrefix(lines[1], "tap: esc down → chord, swallowed") {
		t.Fatalf("lines %q", lines)
	}
}
