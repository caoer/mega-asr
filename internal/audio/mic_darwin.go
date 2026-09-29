//go:build darwin

package audio

import (
	"context"
	"errors"
	"fmt"
	"log"
	"strings"
	"sync"
	"sync/atomic"
	"time"
	"unsafe"

	"github.com/ebitengine/purego"
)

type micState struct {
	mu      sync.Mutex
	started bool
	dev     inputDev
	note    string
	e       error
	id      uintptr
	proc    uintptr
	raw     chan []float32 // mono blocks at the device's rate
	done    chan struct{}  // closed once the output channel is

	held     bool        // Start holds the device warm (see holdWarm); halt releases it
	stopReq  atomic.Bool // Stop was called: a later failure is not the take's
	stopping atomic.Bool // the device is going down: micProc returns at once
	haltOnce sync.Once
	overruns atomic.Int64
}

var (
	mics   sync.Map // client data → *Mic
	micSeq atomic.Uintptr
)

var (
	ioKitOnce sync.Once
	ioKitErr  error

	ioServiceMatching               func(name string) uintptr
	ioServiceGetMatchingService     func(mainPort uint32, matching uintptr) uint32
	ioRegistryEntryCreateCFProperty func(entry uint32, key, allocator uintptr, options uint32) uintptr
	ioObjectRelease                 func(obj uint32) int32
	cfStringCreateWithCString       func(allocator uintptr, s string, encoding uint32) uintptr
	cfGetTypeID                     func(cf uintptr) uintptr
	cfBooleanGetTypeID              func() uintptr
	cfBooleanGetValue               func(b uintptr) bool
	cfRelease                       func(cf uintptr)
)

func loadIOKit() error {
	ioKitOnce.Do(func() {
		iokit, err := purego.Dlopen("/System/Library/Frameworks/IOKit.framework/IOKit", purego.RTLD_NOW|purego.RTLD_GLOBAL)
		if err != nil {
			ioKitErr = err
			return
		}
		cf, err := purego.Dlopen("/System/Library/Frameworks/CoreFoundation.framework/CoreFoundation", purego.RTLD_NOW|purego.RTLD_GLOBAL)
		if err != nil {
			ioKitErr = err
			return
		}
		purego.RegisterLibFunc(&ioServiceMatching, iokit, "IOServiceMatching")
		purego.RegisterLibFunc(&ioServiceGetMatchingService, iokit, "IOServiceGetMatchingService")
		purego.RegisterLibFunc(&ioRegistryEntryCreateCFProperty, iokit, "IORegistryEntryCreateCFProperty")
		purego.RegisterLibFunc(&ioObjectRelease, iokit, "IOObjectRelease")
		purego.RegisterLibFunc(&cfStringCreateWithCString, cf, "CFStringCreateWithCString")
		purego.RegisterLibFunc(&cfGetTypeID, cf, "CFGetTypeID")
		purego.RegisterLibFunc(&cfBooleanGetTypeID, cf, "CFBooleanGetTypeID")
		purego.RegisterLibFunc(&cfBooleanGetValue, cf, "CFBooleanGetValue")
		purego.RegisterLibFunc(&cfRelease, cf, "CFRelease")
	})
	return ioKitErr
}

// lidClosed reports a MacBook's lid shut (IOPMrootDomain's
// AppleClamshellState). The HAL cannot tell: the disconnected built-in mic
// stays listed, alive, with its data source. A Mac without a lid, or an
// IOKit that does not answer, reports false.
func lidClosed() bool {
	if loadIOKit() != nil {
		return false
	}
	// IOServiceGetMatchingService consumes the matching dictionary.
	root := ioServiceGetMatchingService(0, ioServiceMatching("IOPMrootDomain"))
	if root == 0 {
		return false
	}
	defer ioObjectRelease(root)
	key := cfStringCreateWithCString(0, "AppleClamshellState", 0x08000100) // kCFStringEncodingUTF8
	defer cfRelease(key)
	v := ioRegistryEntryCreateCFProperty(root, key, 0, 0)
	if v == 0 {
		return false
	}
	defer cfRelease(v)
	return cfGetTypeID(v) == cfBooleanGetTypeID() && cfBooleanGetValue(v)
}

// Start opens the device and streams it; see Mic.
func (m *Mic) Start(ctx context.Context) (<-chan []int16, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.started {
		return nil, errors.New("mic: started twice")
	}
	m.started = true
	if err := loadHAL(); err != nil {
		return nil, err
	}
	var d inputDev
	var note string
	var err error
	if m.Backup {
		d, err = pickBackup(m.UID, m.Not, inputDevs(), lidClosed())
	} else {
		d, note, err = pickInput(m.UID, m.Name, inputDevs(), lidClosed())
	}
	if err != nil {
		return nil, err
	}
	if err := checkChannel(d, m.Channel); err != nil {
		return nil, err
	}
	for _, s := range propIDs(d.id, "stm#", "inpt") {
		if err := streamFormat(s).float32(); err != nil {
			return nil, fmt.Errorf("%s: %w", d.name, err)
		}
	}
	rate := propF64(d.id, "nsrt", "glob")
	if rate <= 0 {
		return nil, fmt.Errorf("%s reports no sample rate", d.name)
	}
	m.dev, m.note = d, note
	m.raw = make(chan []float32, 1024) // ≈ 10 s of 512-frame blocks
	m.id = micSeq.Add(1)
	mics.Store(m.id, m)
	if st := createIOProcID(d.id, micCallback, m.id, &m.proc); st != 0 {
		mics.Delete(m.id)
		return nil, fmt.Errorf("%s: AudioDeviceCreateIOProcID: %s", d.name, statusText(st))
	}
	if st := devStart(d.id, m.proc); st != 0 {
		destroyIOProcID(d.id, m.proc)
		mics.Delete(m.id)
		return nil, fmt.Errorf("%s: AudioDeviceStart: %s", d.name, statusText(st))
	}
	switch warmFor(m.Backup, m.Warm, d) {
	case warmHold:
		m.held = true
		holdWarm(d)
	case warmDrop:
		dropWarm()
	}
	out := make(chan []int16, 256)
	m.done = make(chan struct{})
	p := &micPump{name: d.name, raw: m.raw, out: out, rs: newResampler(rate), first: micFirstBlock, stall: MicStall, halt: m.halt}
	go func() {
		p.run()
		close(m.done)
	}()
	go func() {
		select {
		case <-ctx.Done():
			m.halt(ctx.Err())
		case <-m.done:
		}
	}()
	return out, nil
}

// Stop keeps the device streaming for Tail, then closes it; the channel
// closes once what it delivered has been drained.
func (m *Mic) Stop() {
	m.mu.Lock()
	done := m.done
	m.mu.Unlock()
	if done == nil || m.stopReq.Swap(true) {
		return
	}
	select {
	case <-done: // the stream already ended on its own
		return
	case <-time.After(m.Tail):
	}
	m.halt(nil)
}

// halt closes the device once, recording err as the take's unless Stop
// came first.
func (m *Mic) halt(err error) {
	m.mu.Lock()
	if err != nil && m.e == nil && !m.stopReq.Load() {
		m.e = err
	}
	m.mu.Unlock()
	m.haltOnce.Do(func() {
		m.stopping.Store(true)
		devStop(m.dev.id, m.proc) // returns once the IO thread has left micProc
		destroyIOProcID(m.dev.id, m.proc)
		mics.Delete(m.id)
		close(m.raw)
		switch {
		case !m.held:
		case err != nil && !m.stopReq.Load():
			dropWarm() // a stalled or vanished device is not kept
		default:
			releaseWarm(m.dev.id, m.Warm)
		}
	})
}

// warm is the Bluetooth input kept running between takes by a no-op IOProc
// of its own. macOS holds a headset in call mode (eSCO) while any IOProc
// runs on its input, and tears the link down soon after the last one stops;
// a take that opens it after that records digital silence until the link is
// back up (BluetoothWarmup). A take's own IOProc is added to the running
// device, so its first block carries the voice.
var warm struct {
	mu    sync.Mutex
	dev   uint32
	proc  uintptr
	holds int         // takes on dev not yet ended
	timer *time.Timer // lets dev go once no take holds it
}

// holdWarm keeps d running until releaseWarm's window lapses; another
// device held is let go. It is called after the take's own IOProc has
// started d, so it never starts the device itself.
func holdWarm(d inputDev) {
	warm.mu.Lock()
	defer warm.mu.Unlock()
	if warm.proc != 0 && warm.dev == d.id {
		warm.holds++
		if warm.timer != nil {
			warm.timer.Stop()
			warm.timer = nil
		}
		return
	}
	dropWarmLocked()
	var proc uintptr
	if st := createIOProcID(d.id, keepCallback, 0, &proc); st != 0 {
		log.Printf("mic: keep %s warm: AudioDeviceCreateIOProcID: %s", d.name, statusText(st))
		return
	}
	if st := devStart(d.id, proc); st != 0 {
		destroyIOProcID(d.id, proc)
		log.Printf("mic: keep %s warm: AudioDeviceStart: %s", d.name, statusText(st))
		return
	}
	warm.dev, warm.proc, warm.holds = d.id, proc, 1
}

// releaseWarm ends a take's hold on dev; the last one lets it go after
// window, unless a take holds it again first.
func releaseWarm(dev uint32, window time.Duration) {
	warm.mu.Lock()
	defer warm.mu.Unlock()
	if warm.proc == 0 || warm.dev != dev {
		return
	}
	if warm.holds--; warm.holds > 0 {
		return
	}
	if warm.timer != nil {
		warm.timer.Stop()
	}
	var t *time.Timer
	t = time.AfterFunc(window, func() {
		warm.mu.Lock()
		defer warm.mu.Unlock()
		if warm.timer == t {
			dropWarmLocked()
		}
	})
	warm.timer = t
}

// dropWarm lets the warm device go now.
func dropWarm() {
	warm.mu.Lock()
	defer warm.mu.Unlock()
	dropWarmLocked()
}

func dropWarmLocked() {
	if warm.timer != nil {
		warm.timer.Stop()
		warm.timer = nil
	}
	if warm.proc != 0 {
		devStop(warm.dev, warm.proc)
		destroyIOProcID(warm.dev, warm.proc)
		warm.dev, warm.proc, warm.holds = 0, 0, 0
	}
}

// keepProc is the warm device's IOProc: it reads nothing.
func keepProc(dev uint32, now, inData unsafe.Pointer, inTime, outData, outTime unsafe.Pointer, client uintptr) int32 {
	return 0
}

// Err is why the stream ended on its own; nil after a clean Stop.
func (m *Mic) Err() error {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.e
}

// Note is what the user should know about a successful Start — the pinned
// device is not connected and another one records — or empty.
func (m *Mic) Note() string {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.note
}

// Device is the name of the input Start opened; empty before.
func (m *Mic) Device() string {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.dev.name
}

// Input is the device Start opened: its name, UID and transport, the
// channel recorded, and the fallback note when the pinned device was absent.
func (m *Mic) Input() TakeInput {
	m.mu.Lock()
	defer m.mu.Unlock()
	return TakeInput{Source: "local", Name: m.dev.name, UID: m.dev.uid, Transport: strings.TrimSpace(m.dev.transport), Channel: m.Channel, Note: m.note}
}

// micProc runs on Core Audio's IO thread: it copies one mono block out and
// never waits. AudioBufferList: uint32 mNumberBuffers, pad, then 16-byte
// AudioBuffers {uint32 mNumberChannels, uint32 mDataByteSize, void *mData},
// one per input stream.
func micProc(dev uint32, now, inData unsafe.Pointer, inTime, outData, outTime unsafe.Pointer, client uintptr) int32 {
	v, ok := mics.Load(client)
	if !ok {
		return 0
	}
	m := v.(*Mic)
	if m.stopping.Load() || inData == nil {
		return 0
	}
	nb := int(*(*uint32)(inData))
	bufs := make([]ioBuf, nb)
	for i := range bufs {
		b := unsafe.Add(inData, 8+i*16)
		ch := int(*(*uint32)(b))
		size := *(*uint32)(unsafe.Add(b, 4))
		data := *(*unsafe.Pointer)(unsafe.Add(b, 8))
		if ch == 0 || data == nil {
			return 0
		}
		bufs[i] = ioBuf{ch, unsafe.Slice((*float32)(data), size/4)}
	}
	block := monoOf(bufs, m.Channel)
	if len(block) == 0 {
		return 0
	}
	select {
	case m.raw <- block:
	default:
		m.overruns.Add(1)
	}
	return 0
}

// PrimeMic opens the built-in input for a moment and closes it, so macOS
// asks for the Microphone grant at launch rather than at the first take. It
// never opens the system default — a Bluetooth headset there would switch
// into call mode — and returns an error on a Mac without a built-in input.
// A closed lid does not matter: the built-in stays listed and opens, it
// only delivers zeros. It may block until the user answers the prompt:
// call it off the main thread.
func PrimeMic() error {
	if err := loadHAL(); err != nil {
		return err
	}
	var uid string
	for _, d := range inputDevs() {
		if d.builtIn() {
			uid = d.uid
			break
		}
	}
	if uid == "" {
		return errors.New("no built-in input")
	}
	m := &Mic{UID: uid}
	ch, err := m.Start(context.Background())
	if err != nil {
		return err
	}
	select {
	case <-ch:
	case <-time.After(time.Second):
	}
	m.Stop()
	for range ch {
	}
	return m.Err()
}
