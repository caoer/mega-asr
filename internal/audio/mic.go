package audio

import (
	"errors"
	"fmt"
	"time"
)

// ErrMicUnsupported is what Mic.Start returns off macOS.
var ErrMicUnsupported = errors.New("local mic capture needs macOS")

// Mic records one of this Mac's input devices through an IOProc on the
// device itself — no aggregate and no process tap, so macOS asks for the
// Microphone grant only. A pinned device that is not connected falls back
// to a wired input (see pickInput) and Note says so. A device that stops
// calling back for MicStall ends the stream with an error naming it.
type Mic struct {
	UID string // input device UID; "" or "default" is the system default input
	// Name is the pinned device's display name, for the note when it is not
	// connected; empty names it by UID.
	Name    string
	Channel int           // 0 mixes every channel to mono; n records channel n alone, 1-based as XVF
	Tail    time.Duration // kept streaming after Stop: the last syllable is in flight
	// Warm keeps a Bluetooth input's device running this long after the
	// take ends (see holdWarm on macOS), so the next take on it starts at
	// once; 0 closes it with the take.
	Warm time.Duration
	// Backup makes this Mic a take's backup track, opened beside the main
	// input: UID is capture.backup ("auto" or an input's UID) and Not the
	// UID of the input the main track opened; pickBackup chooses. A backup
	// never touches the main input's warm hold.
	Backup bool
	Not    string

	micState // per-OS
}

// MicStall is how long a device may stop calling back before the stream
// ends; the first block gets micFirstBlock, since a Bluetooth headset
// switches profile before it delivers.
const (
	MicStall      = 2 * time.Second
	micFirstBlock = 5 * time.Second
)

// BluetoothWarmup bounds the digital silence a Bluetooth input opened cold
// may deliver before the voice: macOS moves the headset to its call profile
// only when an input opens, and that switch is not instant. Silence past the
// bound counts as no signal.
const BluetoothWarmup = 6 * time.Second

// inputDev is an input device as Mic chooses among them.
type inputDev struct {
	id        uint32
	uid       string
	name      string
	channels  int
	transport string // kAudioDevicePropertyTransportType as its four chars: "bltn", "usb ", "blue"…
	def       bool   // the system default input
}

// builtIn is the Mac's own microphone.
func (d inputDev) builtIn() bool { return d.transport == "bltn" }

// bluetooth is a Bluetooth (Classic or LE) input: opening it switches the
// headset into call mode.
func (d inputDev) bluetooth() bool { return d.transport == "blue" || d.transport == "blea" }

// wired is an input a fallback may open: not Bluetooth (opening a headset's
// mic switches it into call mode), not wireless (AirPlay, an iPhone over
// Continuity), not virtual or an aggregate (another app's device).
func (d inputDev) wired() bool {
	switch d.transport {
	case "blue", "blea", "airp", "ccwl", "virt", "grup", "fgrp":
		return false
	}
	return true
}

// pickInput chooses the device for uid among devs: the default for "" or
// "default"; the one with that UID when connected; else, with a note naming
// both (pin names the absent device; empty uses the UID), the first of: the
// built-in input unless the lid is closed (Apple silicon then disconnects it
// and it delivers zeros while still listed), the default if it is wired, the
// first other wired input.
func pickInput(uid, pin string, devs []inputDev, lidClosed bool) (inputDev, string, error) {
	find := func(ok func(inputDev) bool) (inputDev, bool) {
		for _, d := range devs {
			if ok(d) {
				return d, true
			}
		}
		return inputDev{}, false
	}
	if uid == "" || uid == "default" {
		if d, ok := find(func(d inputDev) bool { return d.def }); ok {
			return d, "", nil
		}
		return inputDev{}, "", errors.New("no input device")
	}
	if d, ok := find(func(d inputDev) bool { return d.uid == uid }); ok {
		return d, "", nil
	}
	if pin == "" {
		pin = uid
	}
	usable := func(d inputDev) bool { return d.wired() && !(lidClosed && d.builtIn()) }
	for _, ok := range []func(inputDev) bool{
		func(d inputDev) bool { return d.builtIn() && usable(d) },
		func(d inputDev) bool { return d.def && usable(d) },
		usable,
	} {
		if d, found := find(ok); found {
			return d, fmt.Sprintf("%s not connected — using %s", pin, d.name), nil
		}
	}
	return inputDev{}, "", fmt.Errorf("%s not connected — no wired mic", pin)
}

// BackupAuto is capture.backup's value that lets pickBackup choose.
const BackupAuto = "auto"

// pickBackup chooses a take's backup input among devs. It is never the main
// input (UID not), never an input that is not wired (a Bluetooth headset
// would switch into call mode, an iPhone over Continuity is not at the
// desk), and never the built-in of a closed lid, which delivers zeros. uid
// "auto" takes the first such input other than the built-in, else the
// built-in; another uid is that input alone, and an error when it is absent
// or not allowed.
func pickBackup(uid, not string, devs []inputDev, lidClosed bool) (inputDev, error) {
	ok := func(d inputDev) bool { return d.uid != not && d.wired() && !(lidClosed && d.builtIn()) }
	if uid != BackupAuto {
		for _, d := range devs {
			switch {
			case d.uid != uid:
			case ok(d):
				return d, nil
			case d.uid == not:
				return inputDev{}, fmt.Errorf("backup %s is the main input", d.name)
			default:
				return inputDev{}, fmt.Errorf("backup %s is not a wired input", d.name)
			}
		}
		return inputDev{}, fmt.Errorf("backup %s not connected", uid)
	}
	for _, want := range []func(inputDev) bool{
		func(d inputDev) bool { return ok(d) && !d.builtIn() },
		ok,
	} {
		for _, d := range devs {
			if want(d) {
				return d, nil
			}
		}
	}
	return inputDev{}, errors.New("no wired input beside the main one for a backup")
}

// warmAct is what a Mic's Start does with the warm Bluetooth input.
type warmAct int

const (
	warmDrop  warmAct = iota // let the held device go: this take is on another input
	warmHold                 // hold this take's Bluetooth input warm
	warmLeave                // a backup track: the hold is the main input's
)

// warmFor is Start's warmAct for input d.
func warmFor(backup bool, warm time.Duration, d inputDev) warmAct {
	switch {
	case backup:
		return warmLeave
	case warm > 0 && d.bluetooth():
		return warmHold
	}
	return warmDrop
}

func checkChannel(d inputDev, ch int) error {
	if ch < 0 || ch > d.channels {
		return fmt.Errorf("%s has channels 1..%d, not %d", d.name, d.channels, ch)
	}
	return nil
}

// ioBuf is one AudioBuffer of an IOProc's input: ch interleaved channels.
type ioBuf struct {
	ch int
	s  []float32
}

// monoOf makes one mono block of bufs, whose channels are numbered across
// the buffers in order from 1: channel 0 averages them all, n takes channel
// n alone. The shortest buffer bounds the block.
func monoOf(bufs []ioBuf, channel int) []float32 {
	frames := -1
	total := 0
	for _, b := range bufs {
		if n := len(b.s) / b.ch; frames < 0 || n < frames {
			frames = n
		}
		total += b.ch
	}
	if frames <= 0 {
		return nil
	}
	out := make([]float32, frames)
	if channel > 0 {
		for _, b := range bufs {
			if channel > b.ch {
				channel -= b.ch
				continue
			}
			for i := range out {
				out[i] = b.s[i*b.ch+channel-1]
			}
			return out
		}
		return out
	}
	for _, b := range bufs {
		for i := range out {
			for c := range b.ch {
				out[i] += b.s[i*b.ch+c]
			}
		}
	}
	for i := range out {
		out[i] /= float32(total)
	}
	return out
}

// micPump resamples a device's mono blocks to Rate and delivers them on
// out, which it closes once raw is closed and drained. No block within
// first, or none for stall after one, halts the device with an error naming
// it; halt closes raw.
type micPump struct {
	name         string
	raw          <-chan []float32
	out          chan<- []int16
	rs           *resampler
	first, stall time.Duration
	halt         func(error)
}

func (p *micPump) run() {
	defer close(p.out)
	timer := time.NewTimer(p.first)
	defer timer.Stop()
	stalled := timer.C
	for {
		select {
		case b, ok := <-p.raw:
			if !ok {
				return
			}
			if s := p.rs.push(b); len(s) > 0 {
				p.out <- s
			}
			if stalled != nil {
				timer.Reset(p.stall)
			}
		case <-stalled:
			stalled = nil // halt once; what raw still holds drains
			p.halt(fmt.Errorf("%s stopped delivering audio", p.name))
		}
	}
}
