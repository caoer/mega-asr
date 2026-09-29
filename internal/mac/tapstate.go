package mac

import (
	"fmt"
	"sync/atomic"
	"time"
)

// TapKey is a modifier used as a toggle: tapped alone, it starts or stops a
// recording; used with another key it stays a plain modifier.
type TapKey struct {
	Name      string
	Code      int    // virtual keycode
	DeviceBit uint64 // NX_DEVICE*KEYMASK: this side of the modifier is down
	Class     uint64 // kCGEventFlagMask* of the modifier's class
}

const (
	flagShift   = 0x20000
	flagControl = 0x40000
	flagOption  = 0x80000
	flagCommand = 0x100000
	flagFn      = 0x800000
	modMask     = flagShift | flagControl | flagOption | flagCommand | flagFn

	keyEscape      = 53
	keyReturn      = 36
	keyKeypadEnter = 76
)

var tapKeys = map[string]TapKey{
	"right_shift":  {"right_shift", 60, 0x04, flagShift},
	"right_option": {"right_option", 61, 0x40, flagOption},
}

// LookupTapKey resolves a TAP_KEY name.
func LookupTapKey(name string) (TapKey, error) {
	k, ok := tapKeys[name]
	if !ok {
		return TapKey{}, fmt.Errorf("unknown tap key %q (right_shift, right_option)", name)
	}
	return k, nil
}

// Phase is what the controller is doing, as far as the tap cares.
type Phase int32

const (
	PhaseIdle       Phase = iota
	PhaseRecording        // Esc cancels; Enter stops and sends
	PhaseProcessing       // transcribing or delivering; Enter upgrades to send
	// PhaseWindow: a window of megavoice's own has the keys; the tap takes
	// nothing, and a tap of the key starts or stops nothing.
	PhaseWindow
)

// windowWhy is a key event's reason while a megavoice window has the keys.
const windowWhy = "a megavoice window has the keys"

// Kind classifies an event for the detector.
type Kind int

const (
	KindFlags   Kind = iota // flagsChanged
	KindKeyDown             // keyDown, autorepeat included
	KindKeyUp
	KindPointer // mouse down or scroll
)

// Input is one event seen by the tap.
type Input struct {
	Kind   Kind
	Code   int
	Flags  uint64
	At     time.Time
	Repeat bool  // a key-down the keyboard repeats while the key is held
	Take   int64 // the recording the event came in (tapCore's count), 0 before any
}

// Result says what an event means and whether the tap drops it.
type Result struct {
	Tap     bool // the tap key went down and up alone within the window
	Cancel  bool // Esc while recording
	Enter   bool // a bare Return while recording or processing
	Chord   bool // Esc went down with the tap key held: the resend hotkey
	Swallow bool
	Why     string // for a tap-key event: why it is, or is not, a tap
}

// Detector turns the event stream into taps. A tap is the key going down with
// no other modifier held and up again within Window, with no other event in
// between. A tap that would start a recording also needs Quiet since the last
// key press, so a Shift pressed and let go mid-typing stays a Shift; a tap
// that stops one does not. Esc with the tap key held is the chord, taken in
// every phase. At the Esc's key-down, flags carrying the key's device bit
// say it is held; flags without it are not proof either way, since an Esc
// from another keyboard need not carry it, so the system's own key state is
// asked: a key-up the tap missed never turns a plain Esc into the chord, and
// a key-down it never saw never turns the chord into a cancel. Between Escs
// the key's own events keep its state, and a modifier event whose flags lack
// its device bit says it is up. A lone Esc is taken only while a recording
// runs; a bare Return (no modifier) while a recording runs or its text is
// still on the way. A taken key's repeats and key-up are taken with it.
type Detector struct {
	Key    TapKey
	Window time.Duration
	Quiet  time.Duration
	// Held is the system's state of the key with the code given, asked at
	// an Esc's key-down whose flags lack the tap key's device bit, and at a
	// Reset; nil trusts the key's events alone.
	Held func(code int) bool

	armed    bool
	down     bool   // the tap key's key-down came and its key-up has not
	downRec  bool   // and it came while a take recorded
	downTake int64  // which one
	why      string // why the tap key is not armed
	downAt   time.Time
	lastKey  time.Time
	escOut   bool // an Esc down was swallowed; swallow its up too
	enterOut int  // keycode of a swallowed Return down; 0 = none
}

// Handle classifies one event in the controller's current phase.
func (d *Detector) Handle(in Input, phase Phase) Result {
	typing := !d.lastKey.IsZero() && in.At.Sub(d.lastKey) < d.Quiet
	if in.Kind == KindKeyDown {
		d.lastKey = in.At
	}
	recording := phase == PhaseRecording
	if phase == PhaseWindow {
		return d.window(in)
	}
	if in.Kind == KindFlags && in.Code != d.Key.Code && in.Flags&d.Key.DeviceBit == 0 {
		d.down = false // the system's modifier state says the key is up
	}
	if in.Kind == KindKeyDown && in.Code == keyEscape && !in.Repeat {
		d.heldAtEsc(in)
	}
	switch {
	case in.Kind == KindFlags && in.Code == d.Key.Code:
		if in.Flags&d.Key.DeviceBit != 0 {
			other := in.Flags&modMask != d.Key.Class
			d.armed, d.down, d.downRec, d.downTake, d.downAt = !other && (recording || !typing), true, recording, in.Take, in.At
			switch {
			case other:
				d.why = "another modifier was held at its key-down"
			case !d.armed:
				d.why = "typing: a key went down within the quiet time"
			default:
				return Result{Why: "armed"}
			}
			return Result{Why: "not armed: " + d.why}
		}
		held := in.At.Sub(d.downAt).Milliseconds()
		r := Result{Tap: d.armed && in.At.Sub(d.downAt) <= d.Window}
		switch {
		case !d.down:
			r.Why = "no key-down came before it"
		case r.Tap:
			r.Why = fmt.Sprintf("up after %dms", held)
		case recording && (!d.downRec || d.downTake != in.Take):
			r.Why = "its key-down came before this take" // no reason or time from before the take
		case d.armed:
			r.Why = fmt.Sprintf("held %dms, past the %dms window", held, d.Window.Milliseconds())
		default:
			r.Why = "not armed: " + d.why
		}
		d.armed, d.down = false, false
		return r
	case in.Kind == KindKeyDown && in.Code == keyEscape && in.Repeat && d.escOut:
		return Result{Swallow: true} // autorepeat of a taken Esc
	case in.Kind == KindKeyDown && in.Code == keyEscape && !in.Repeat && d.down:
		d.disarm("Esc went down while it was held")
		d.escOut = true
		return Result{Chord: true, Swallow: true}
	case in.Kind == KindKeyDown && in.Code == keyEscape && recording:
		d.disarm("Esc went down while it was held")
		d.escOut = true
		return Result{Cancel: true, Swallow: true}
	case in.Kind == KindKeyUp && in.Code == keyEscape && d.escOut:
		d.escOut = false
		return Result{Swallow: true}
	case in.Kind == KindKeyDown && d.enterOut != 0 && in.Code == d.enterOut:
		return Result{Swallow: true} // autorepeat of a taken Return
	case in.Kind == KindKeyDown && isReturn(in.Code) && in.Flags&modMask&^flagFn == 0 && phase != PhaseIdle:
		d.disarm("Return went down while it was held")
		d.enterOut = in.Code
		return Result{Enter: true, Swallow: true}
	case in.Kind == KindKeyUp && d.enterOut != 0 && in.Code == d.enterOut:
		d.enterOut = 0
		return Result{Swallow: true}
	}
	d.disarm("another key or a click came while it was held")
	return Result{}
}

// window handles an event while a megavoice window has the keys: nothing is
// taken and nothing stops or cancels, but the up of a key already taken is
// taken with it.
func (d *Detector) window(in Input) Result {
	switch {
	case in.Kind == KindKeyUp && in.Code == keyEscape && d.escOut:
		d.escOut = false
		return Result{Swallow: true}
	case in.Kind == KindKeyUp && d.enterOut != 0 && in.Code == d.enterOut:
		d.enterOut = 0
		return Result{Swallow: true}
	case in.Kind == KindFlags && in.Code == d.Key.Code:
		d.armed, d.why = false, windowWhy
		d.down, d.downRec, d.downTake = in.Flags&d.Key.DeviceBit != 0, true, in.Take
		return Result{Why: windowWhy}
	case (in.Kind == KindKeyDown || in.Kind == KindKeyUp) && (in.Code == keyEscape || isReturn(in.Code)):
		d.disarm(windowWhy)
		return Result{Why: windowWhy}
	}
	d.disarm(windowWhy)
	return Result{}
}

// heldAtEsc settles whether the tap key is held as an Esc goes down. The
// Esc's own flags carrying the key's device bit say so as of the Esc; without
// the bit the system's key state is asked, so a key-up the tap missed and a
// key-down it never saw (held before the tap, or across a Reset) both come
// out right.
func (d *Detector) heldAtEsc(in Input) {
	held := d.down
	switch {
	case in.Flags&d.Key.DeviceBit != 0:
		held = true
	case d.Held != nil:
		held = d.Held(d.Key.Code)
	}
	switch {
	case !held:
		d.down, d.armed = false, false // its key-up never reached the tap
	case !d.down:
		d.down, d.armed, d.why = true, false, "its key-down never reached the tap"
	}
}

// Reset forgets every key the detector holds as taken, and takes the tap
// key's held state from the system: the tap was disabled and the events in
// between are lost.
func (d *Detector) Reset() {
	d.armed, d.escOut, d.enterOut = false, false, 0
	d.down = d.Held != nil && d.Held(d.Key.Code)
	if d.down {
		d.why = "its key-down came before the tap was reset"
	}
}

// disarm ends a tap in the making, saying why.
func (d *Detector) disarm(why string) {
	if d.armed {
		d.why = why
	}
	d.armed = false
}

// Device is where a key event came from, as far as the event says: a
// CGEvent carries no device, only the keyboard type, its source state (hid
// for a key from hardware; session or private for a posted one) and the
// process that posted it (0 for hardware).
type Device struct {
	Keyboard int64
	Source   string
	PID      int64
}

// KeyLine is the log line for a key event received while a take records:
// the tap key, Esc and Return only, with what the detector returned. ok is
// false for any other key and for every key outside a recording.
func KeyLine(key TapKey, in Input, phase Phase, r Result, dev Device) (line string, ok bool) {
	if !keyLogged(key, in, phase) {
		return "", false
	}
	var name, dir string
	switch {
	case in.Kind == KindFlags && in.Code == key.Code:
		name, dir = key.Name, "up"
		if in.Flags&key.DeviceBit != 0 {
			dir = "down"
		}
	case in.Kind != KindKeyDown && in.Kind != KindKeyUp:
		return "", false
	case in.Code == keyEscape:
		name = "esc"
	case in.Code == keyReturn:
		name = "return"
	case in.Code == keyKeypadEnter:
		name = "keypad_enter"
	default:
		return "", false
	}
	if dir == "" {
		dir = "down"
		if in.Kind == KindKeyUp {
			dir = "up"
		}
	}
	did := "none"
	switch {
	case r.Tap:
		did = "tap"
	case r.Cancel:
		did = "cancel"
	case r.Enter:
		did = "enter"
	case r.Chord:
		did = "chord"
	}
	if r.Swallow {
		did += ", swallowed"
	}
	switch {
	case r.Why != "":
		did += ": " + r.Why
	case !r.Swallow && did == "none" && in.Flags&modMask&^flagFn != 0:
		did += ": passed on, a modifier is held"
	case !r.Swallow && did == "none":
		did += ": passed on"
	}
	return fmt.Sprintf("tap: %s %s → %s flags=%#x kbd=%d src=%s pid=%d", name, dir, did, in.Flags, dev.Keyboard, dev.Source, dev.PID), true
}

func isReturn(code int) bool { return code == keyReturn || code == keyKeypadEnter }

// keyLogged reports whether an event gets a key line: the tap key, Esc,
// Return or keypad Enter while a take records; a repeat of a held key none.
func keyLogged(key TapKey, in Input, phase Phase) bool {
	switch {
	case phase != PhaseRecording || in.Repeat:
		return false
	case in.Kind == KindFlags:
		return in.Code == key.Code
	case in.Kind == KindKeyDown || in.Kind == KindKeyUp:
		return in.Code == keyEscape || isReturn(in.Code)
	}
	return false
}

// keyRec is one key line's material, formatted off the tap's thread.
type keyRec struct {
	in    Input
	phase Phase
	r     Result
	dev   Device
}

// KeyLog writes key lines off the tap's thread. Add never blocks: a line
// that finds the queue full is dropped and counted, and the next line
// written is preceded by the count.
type KeyLog struct {
	key     TapKey
	ch      chan keyRec
	dropped atomic.Int64
}

// NewKeyLog makes a KeyLog for key that holds up to n lines not yet written.
func NewKeyLog(key TapKey, n int) *KeyLog {
	return &KeyLog{key: key, ch: make(chan keyRec, n)}
}

// Add queues a line; it never blocks.
func (k *KeyLog) Add(rec keyRec) {
	select {
	case k.ch <- rec:
	default:
		k.dropped.Add(1)
	}
}

// Run writes the queued lines through logf until the queue is closed.
func (k *KeyLog) Run(logf func(string)) {
	for rec := range k.ch {
		if n := k.dropped.Swap(0); n > 0 {
			logf(fmt.Sprintf("tap: %d key lines dropped: the log fell behind", n))
		}
		if line, ok := KeyLine(k.key, rec.in, rec.phase, rec.r, rec.dev); ok {
			logf(line)
		}
	}
}

// windowUp counts megavoice's own modal windows open now.
var windowUp atomic.Int32

// modal runs a modal window's loop, counted in windowUp.
func modal(run func()) {
	windowUp.Add(1)
	defer windowUp.Add(-1)
	run()
}

// tapCore is what the tap's callback does with one event, apart from the
// event system: classify it in the controller's phase and queue its key
// line. While a megavoice modal window has the keys — one is open and
// megavoice is the active app — the phase is PhaseWindow, whatever the
// controller's: no key is taken and none starts, stops or cancels a take. A
// modal left open behind another app takes no key.
type tapCore struct {
	det    Detector
	keys   *KeyLog
	active func() bool // megavoice is the active app
	phase  atomic.Int32
	takes  atomic.Int64 // recordings the phase has entered
}

// setPhase sets the controller's phase; entering PhaseRecording counts a
// new recording.
func (c *tapCore) setPhase(p Phase) {
	if old := Phase(c.phase.Swap(int32(p))); p == PhaseRecording && old != PhaseRecording {
		c.takes.Add(1)
	}
}

// windowHasKeys reports whether a megavoice modal window has the keys.
func (c *tapCore) windowHasKeys() bool {
	return windowUp.Load() > 0 && c.active != nil && c.active()
}

func (c *tapCore) event(in Input, dev func() Device) Result {
	phase := Phase(c.phase.Load())
	in.Take = c.takes.Load()
	eff := phase
	if c.windowHasKeys() {
		eff = PhaseWindow
	}
	r := c.det.Handle(in, eff)
	if c.keys != nil && keyLogged(c.det.Key, in, phase) {
		c.keys.Add(keyRec{in: in, phase: phase, r: r, dev: dev()})
	}
	return r
}
