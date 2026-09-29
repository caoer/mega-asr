//go:build darwin

package mac

import (
	"errors"
	"log"
	"sync"
	"sync/atomic"
	"time"

	"github.com/ebitengine/purego"
)

const (
	cgSessionEventTap   = 1
	cgHeadInsertTap     = 0
	cgTapOptionDefault  = 0  // active: the callback may drop events
	cgKeyboardAutorep   = 8  // kCGKeyboardEventAutorepeat
	cgKeyboardKeycode   = 9  // kCGKeyboardEventKeycode
	cgKeyboardType      = 10 // kCGKeyboardEventKeyboardType
	cgSourcePID         = 41 // kCGEventSourceUnixProcessID
	cgSourceStateID     = 45 // kCGEventSourceStateID: -1 private, 0 session, 1 hid
	cgSourceUserData    = 42 // kCGEventSourceUserData: marks megavoice's own events
	cgTapDisabledTimout = 0xFFFFFFFE
	cgTapDisabledInput  = 0xFFFFFFFF

	evLeftMouseDown  = 1
	evRightMouseDown = 3
	evKeyDown        = 10
	evKeyUp          = 11
	evFlagsChanged   = 12
	evScrollWheel    = 22
	evOtherMouseDown = 25
)

var (
	cgEventTapCreate              func(tap, place, options uint32, mask uint64, cb, info uintptr) uintptr
	cgEventTapEnable              func(port uintptr, enable bool)
	cgEventGetIntegerValueField   func(ev uintptr, field uint32) int64
	cgEventGetFlags               func(ev uintptr) uint64
	cfMachPortCreateRunLoopSource func(alloc, port uintptr, order int) uintptr
	cfRunLoopGetMain              func() uintptr
	cfRunLoopAddSource            func(rl, src, mode uintptr)
	cfRunLoopCommonModes          uintptr
	cgEventSourceKeyState         func(state int32, key uint16) bool
)

// cgHIDSystemState is kCGEventSourceStateHIDSystemState: the keys every
// keyboard holds down, as the HID system has them.
const cgHIDSystemState = 1

func init() {
	purego.RegisterLibFunc(&cgEventTapCreate, libCG, "CGEventTapCreate")
	purego.RegisterLibFunc(&cgEventTapEnable, libCG, "CGEventTapEnable")
	purego.RegisterLibFunc(&cgEventGetIntegerValueField, libCG, "CGEventGetIntegerValueField")
	purego.RegisterLibFunc(&cgEventGetFlags, libCG, "CGEventGetFlags")
	purego.RegisterLibFunc(&cfMachPortCreateRunLoopSource, libCF, "CFMachPortCreateRunLoopSource")
	purego.RegisterLibFunc(&cfRunLoopGetMain, libCF, "CFRunLoopGetMain")
	purego.RegisterLibFunc(&cfRunLoopAddSource, libCF, "CFRunLoopAddSource")
	purego.RegisterLibFunc(&cgEventSourceKeyState, libCG, "CGEventSourceKeyState")
	cfRunLoopCommonModes = symbolValue(libCF, "kCFRunLoopCommonModes")
}

// Tap is the active session event tap. It passes every event through except
// the keys the detector takes in the current Phase, and never touches the
// events megavoice posts itself.
type Tap struct {
	Taps    atomic.Int64 // taps detected since start
	lastKey atomic.Int64 // unix ns of the last key-down or modifier change
	port    uintptr
	core    tapCore
	on      func(Result, time.Time)
}

// LastKey is when the tap last saw a key go down or a modifier change;
// zero before the first.
func (t *Tap) LastKey() time.Time {
	if ns := t.lastKey.Load(); ns != 0 {
		return time.Unix(0, ns)
	}
	return time.Time{}
}

// SetPhase tells the tap the controller's phase.
func (t *Tap) SetPhase(p Phase) { t.core.setPhase(p) }

// appActive reports whether megavoice is the active app. The tap's callback
// runs on the main thread, where AppKit may be asked.
func appActive() bool {
	return sendInt(send(class("NSApplication"), "sharedApplication"), "isActive") != 0
}

// ErrTapRefused means CGEventTapCreate returned NULL: Accessibility is off or
// Input Monitoring is explicitly denied.
var ErrTapRefused = errors.New("event tap refused (Accessibility off or Input Monitoring denied)")

var (
	// installed is the tap the one callback serves; set on the main thread.
	installed *Tap
	// tapCallback is made once: purego allows 2000 callbacks a process, and
	// installTap retries every 2 s while Accessibility is off.
	tapCallback = sync.OnceValue(func() uintptr {
		return purego.NewCallback(func(proxy uintptr, typ uint32, ev uintptr, info uintptr) uintptr {
			return installed.callback(proxy, typ, ev, info)
		})
	})
	tapOn bool // a tap is on the run loop; main thread only
)

// install puts t on the main run loop; a test replaces it.
var install = func(t *Tap) error {
	var err error
	DoSync(func() { err = installOnMain(t) })
	return err
}

// installOnMain creates the event tap for t and adds it to the main run
// loop; it runs on the main thread. One tap at a time, with the one callback.
func installOnMain(t *Tap) error {
	if tapOn {
		return errors.New("tap already installed")
	}
	mask := uint64(1<<evFlagsChanged | 1<<evKeyDown | 1<<evKeyUp |
		1<<evLeftMouseDown | 1<<evRightMouseDown | 1<<evOtherMouseDown | 1<<evScrollWheel)
	installed = t
	port := cgEventTapCreate(cgSessionEventTap, cgHeadInsertTap, cgTapOptionDefault, mask, tapCallback(), 0)
	if port == 0 {
		return ErrTapRefused
	}
	tapOn = true
	t.port = port
	src := cfMachPortCreateRunLoopSource(0, port, 0)
	cfRunLoopAddSource(cfRunLoopGetMain(), src, cfRunLoopCommonModes)
	cgEventTapEnable(port, true)
	return nil
}

// StartTap installs the tap on the main run loop with the detector's
// settings. on gets each Result with the time of the key event that made it;
// it runs on the main thread and must not block. The key lines' writer
// starts once the tap is installed: a refused start leaves nothing running.
func StartTap(det Detector, on func(Result, time.Time)) (*Tap, error) {
	t := newTap(det, on)
	if err := install(t); err != nil {
		return nil, err
	}
	go t.core.keys.Run(logLine)
	return t, nil
}

// newTap is the tap StartTap installs: its key lines queued, a megavoice
// window given the keys only while megavoice is the active app, and the
// chord's hold confirmed by the HID system's key state.
func newTap(det Detector, on func(Result, time.Time)) *Tap {
	if det.Held == nil {
		det.Held = keyHeld
	}
	return &Tap{core: tapCore{det: det, keys: NewKeyLog(det.Key, 64), active: appActive}, on: on}
}

// keyHeld reports whether the HID system holds the key with code down.
func keyHeld(code int) bool { return cgEventSourceKeyState(cgHIDSystemState, uint16(code)) }

// logLine writes a key line to the log.
func logLine(line string) { log.Print(line) }

func (t *Tap) callback(proxy uintptr, typ uint32, ev uintptr, info uintptr) uintptr {
	if typ == cgTapDisabledTimout || typ == cgTapDisabledInput {
		log.Printf("tap: disabled by the system (type %#x), re-enabling; the keys held are forgotten", typ)
		t.core.det.Reset()
		cgEventTapEnable(t.port, true)
		return ev
	}
	in, own := inputOf(typ, func(f uint32) int64 { return cgEventGetIntegerValueField(ev, f) }, func() uint64 { return cgEventGetFlags(ev) })
	if own {
		return ev
	}
	in.At = time.Now()
	if in.Kind == KindKeyDown || in.Kind == KindFlags {
		t.lastKey.Store(in.At.UnixNano())
	}
	return t.after(t.core.event(in, func() Device { return deviceOf(ev) }), in.At, ev)
}

// after acts on an event's Result: a tap, cancel, Enter or chord goes to on, and a
// swallowed event is dropped (0); any other goes on as ev.
func (t *Tap) after(r Result, at time.Time, ev uintptr) uintptr {
	if r.Tap {
		t.Taps.Add(1)
	}
	if r.Tap || r.Cancel || r.Enter || r.Chord {
		t.on(r, at)
	}
	if r.Swallow {
		return 0
	}
	return ev
}

// deviceOf is where a key event came from, as far as the event says.
func deviceOf(ev uintptr) Device {
	src := "private"
	switch cgEventGetIntegerValueField(ev, cgSourceStateID) {
	case 0:
		src = "session"
	case 1:
		src = "hid"
	}
	return Device{
		Keyboard: cgEventGetIntegerValueField(ev, cgKeyboardType),
		Source:   src,
		PID:      cgEventGetIntegerValueField(ev, cgSourcePID),
	}
}

// inputOf reads an event of type typ through its fields and flags; own is
// true for an event megavoice posted itself.
func inputOf(typ uint32, field func(uint32) int64, flags func() uint64) (in Input, own bool) {
	switch typ {
	case evFlagsChanged:
		in.Kind = KindFlags
	case evKeyDown:
		in.Kind = KindKeyDown
	case evKeyUp:
		in.Kind = KindKeyUp
	default:
		in.Kind = KindPointer
		return in, false
	}
	if field(cgSourceUserData) == ownEvent {
		return in, true
	}
	in.Code = int(field(cgKeyboardKeycode))
	in.Flags = flags()
	in.Repeat = in.Kind == KindKeyDown && field(cgKeyboardAutorep) != 0
	return in, false
}
