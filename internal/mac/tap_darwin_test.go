//go:build darwin

package mac

import (
	"bytes"
	"log"
	"reflect"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"
)

// A refused start leaves nothing running: installTap retries every 2 s
// while Accessibility is off.
func TestStartTapRefusedLeavesNothing(t *testing.T) {
	was := install
	defer func() { install = was }()
	install = func(*Tap) error { return ErrTapRefused }
	det := Detector{Key: tapKeys["right_shift"], Window: 400 * time.Millisecond}
	before := runtime.NumGoroutine()
	for range 2 {
		if _, err := StartTap(det, func(Result, time.Time) {}); err != ErrTapRefused {
			t.Fatalf("StartTap: %v", err)
		}
	}
	time.Sleep(50 * time.Millisecond)
	if after := runtime.NumGoroutine(); after != before {
		t.Fatalf("%d goroutines after two refused starts, %d before", after, before)
	}
}

// syncBuf is a log output read while the log writes to it.
type syncBuf struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuf) Write(p []byte) (int, error) { s.mu.Lock(); defer s.mu.Unlock(); return s.b.Write(p) }
func (s *syncBuf) String() string              { s.mu.Lock(); defer s.mu.Unlock(); return s.b.String() }

// An installed tap writes its key lines to the log.
func TestStartTapLogsKeyLines(t *testing.T) {
	was := install
	defer func() { install = was }()
	install = func(*Tap) error { return nil }
	var buf syncBuf
	out := log.Writer()
	log.SetOutput(&buf)
	defer log.SetOutput(out)
	tp, err := StartTap(Detector{Key: tapKeys["right_shift"], Window: 400 * time.Millisecond}, func(Result, time.Time) {})
	if err != nil {
		t.Fatal(err)
	}
	tp.SetPhase(PhaseRecording)
	tp.core.event(Input{Kind: KindKeyDown, Code: keyReturn, At: time.Now()}, func() Device { return Device{Source: "hid"} })
	for deadline := time.Now().Add(time.Second); !strings.Contains(buf.String(), "tap: return down → enter, swallowed"); {
		if time.Now().After(deadline) {
			t.Fatalf("log %q", buf.String())
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// The callback reads a key-down's autorepeat, never a key-up's, and leaves
// megavoice's own events alone.
func TestInputOf(t *testing.T) {
	fields := func(m map[uint32]int64) func(uint32) int64 { return func(f uint32) int64 { return m[f] } }
	flags := func() uint64 { return 0x20104 }
	in, own := inputOf(evKeyDown, fields(map[uint32]int64{cgKeyboardKeycode: keyReturn, cgKeyboardAutorep: 1}), flags)
	if own || in.Kind != KindKeyDown || in.Code != keyReturn || in.Flags != 0x20104 || !in.Repeat {
		t.Errorf("a repeated Return: %+v own %v", in, own)
	}
	if in, _ := inputOf(evKeyDown, fields(map[uint32]int64{cgKeyboardKeycode: keyReturn}), flags); in.Repeat {
		t.Errorf("a first Return read as a repeat: %+v", in)
	}
	if in, _ := inputOf(evKeyUp, fields(map[uint32]int64{cgKeyboardKeycode: keyEscape, cgKeyboardAutorep: 1}), flags); in.Kind != KindKeyUp || in.Repeat {
		t.Errorf("an Esc key-up: %+v", in)
	}
	if in, _ := inputOf(evFlagsChanged, fields(map[uint32]int64{cgKeyboardKeycode: 60}), flags); in.Kind != KindFlags || in.Code != 60 {
		t.Errorf("Right Shift: %+v", in)
	}
	if _, own := inputOf(evKeyDown, fields(map[uint32]int64{cgSourceUserData: ownEvent, cgKeyboardKeycode: keyReturn}), flags); !own {
		t.Error("megavoice's own Return read as a key")
	}
	if in, own := inputOf(evScrollWheel, fields(nil), flags); own || in.Kind != KindPointer {
		t.Errorf("a scroll: %+v", in)
	}
}

// StartTap's tap gives a megavoice window the keys only while megavoice is
// the active app: without the active check the exit offer's Return would
// send the take.
func TestNewTapAsksWhetherActive(t *testing.T) {
	if tp := newTap(Detector{Key: tapKeys["right_shift"]}, func(Result, time.Time) {}); tp.core.active == nil || tp.core.keys == nil || tp.core.det.Held == nil {
		t.Fatalf("active set %v, keys set %v, held set %v", tp.core.active != nil, tp.core.keys != nil, tp.core.det.Held != nil)
	}
}

// keyHeld asks the HID system for the key's state: a key no one holds (F18,
// on no built-in keyboard) is up, and newTap wires it as the detector's Held.
func TestKeyHeldReadsTheSystem(t *testing.T) {
	if keyHeld(79) {
		t.Fatal("keyHeld(F18) = true for a key no one holds")
	}
	tp := newTap(Detector{Key: tapKeys["right_option"]}, func(Result, time.Time) {})
	if reflect.ValueOf(tp.core.det.Held).Pointer() != reflect.ValueOf(keyHeld).Pointer() {
		t.Fatal("newTap does not wire keyHeld")
	}
}

// A tap the system disabled is re-enabled with the detector reset: a Right
// Shift held before it no longer makes a plain Esc the chord.
func TestTapReenableForgetsKeys(t *testing.T) {
	enable := cgEventTapEnable
	defer func() { cgEventTapEnable = enable }()
	var enabled int
	cgEventTapEnable = func(port uintptr, on bool) { enabled++ }
	tp := newTap(Detector{Key: tapKeys["right_shift"], Window: 400 * time.Millisecond}, func(Result, time.Time) {})
	tp.core.det.Held = nil
	dev := func() Device { return Device{} }
	t0 := time.Now()
	tp.core.event(Input{Kind: KindFlags, Code: 60, Flags: flagShift | tapKeys["right_shift"].DeviceBit, At: t0}, dev)
	if ev := tp.callback(0, cgTapDisabledTimout, 9, 0); ev != 9 || enabled != 1 {
		t.Fatalf("disabled: event %d, enabled %d times", ev, enabled)
	}
	if got := tp.core.event(Input{Kind: KindKeyDown, Code: keyEscape, At: t0.Add(time.Second)}, dev); got != (Result{}) {
		t.Fatalf("a plain Esc after the re-enable: %+v", got)
	}
}

// installOnMain uses the one callback for every attempt, and after a tap is
// on the run loop a second install is refused.
func TestInstallOnMain(t *testing.T) {
	create, source, add, getMain, enable := cgEventTapCreate, cfMachPortCreateRunLoopSource, cfRunLoopAddSource, cfRunLoopGetMain, cgEventTapEnable
	defer func() {
		cgEventTapCreate, cfMachPortCreateRunLoopSource, cfRunLoopAddSource, cfRunLoopGetMain, cgEventTapEnable = create, source, add, getMain, enable
		tapOn, installed = false, nil
	}()
	var cbs []uintptr
	port := uintptr(0)
	cgEventTapCreate = func(tap, place, options uint32, mask uint64, cb, info uintptr) uintptr {
		cbs = append(cbs, cb)
		return port
	}
	cfMachPortCreateRunLoopSource = func(alloc, port uintptr, order int) uintptr { return 1 }
	cfRunLoopAddSource = func(rl, src, mode uintptr) {}
	cfRunLoopGetMain = func() uintptr { return 1 }
	cgEventTapEnable = func(port uintptr, enable bool) {}
	det := Detector{Key: tapKeys["right_shift"]}
	for range 2 {
		if err := installOnMain(newTap(det, func(Result, time.Time) {})); err != ErrTapRefused {
			t.Fatalf("refused install: %v", err)
		}
	}
	port = 7
	if err := installOnMain(newTap(det, func(Result, time.Time) {})); err != nil {
		t.Fatalf("install: %v", err)
	}
	if len(cbs) != 3 || cbs[0] != cbs[1] || cbs[1] != cbs[2] || cbs[0] == 0 {
		t.Fatalf("callbacks %v: one for every attempt", cbs)
	}
	if err := installOnMain(newTap(det, func(Result, time.Time) {})); err == nil || len(cbs) != 3 {
		t.Fatalf("a second install after a success: %v, %d attempts", err, len(cbs))
	}
}

// The callback hands a tap, a cancel and an Enter to on, and drops a
// swallowed event.
func TestTapAfter(t *testing.T) {
	var got []Result
	tp := &Tap{on: func(r Result, _ time.Time) { got = append(got, r) }}
	for _, c := range []struct {
		r  Result
		ev uintptr
		on bool
	}{
		{Result{Tap: true}, 9, true},
		{Result{Cancel: true, Swallow: true}, 0, true},
		{Result{Enter: true, Swallow: true}, 0, true},
		{Result{Chord: true, Swallow: true}, 0, true},
		{Result{Swallow: true}, 0, false},
		{Result{Why: "armed"}, 9, false},
	} {
		got = nil
		if ev := tp.after(c.r, time.Now(), 9); ev != c.ev || (len(got) == 1) != c.on {
			t.Errorf("%+v: event %d, on %v; want %d %v", c.r, ev, got, c.ev, c.on)
		}
	}
	if tp.Taps.Load() != 1 {
		t.Errorf("taps %d", tp.Taps.Load())
	}
}
