//go:build darwin

package audio

import (
	"context"
	"errors"
	"fmt"
	"math"
	"os"
	"runtime"
	"slices"
	"sync"
	"sync/atomic"
	"time"
	"unsafe"

	"github.com/ebitengine/purego"
	"github.com/ebitengine/purego/objc"
)

// Core Audio from pure Go: the HAL's C API through purego, the one
// Objective-C class (CATapDescription) and the property dictionaries
// through objc_msgSend.

type propAddr struct{ sel, scope, elem uint32 }

func fourcc(s string) uint32 {
	return uint32(s[0])<<24 | uint32(s[1])<<16 | uint32(s[2])<<8 | uint32(s[3])
}

func osStatus(what string, st int32) error {
	return fmt.Errorf("tap: %s: %s", what, statusText(st))
}

// statusText shows an OSStatus as its four-char code when it is one.
func statusText(st int32) string {
	u := uint32(st)
	b := []byte{byte(u >> 24), byte(u >> 16), byte(u >> 8), byte(u)}
	for _, c := range b {
		if c < 0x20 || c >= 0x7f {
			return fmt.Sprintf("OSStatus %d", st)
		}
	}
	return fmt.Sprintf("OSStatus '%s'", b)
}

const sysObj = 1 // kAudioObjectSystemObject

var (
	caOnce sync.Once
	caErr  error

	getPropData     func(obj uint32, a *propAddr, qsz uint32, q unsafe.Pointer, size *uint32, data unsafe.Pointer) int32
	getPropDataSize func(obj uint32, a *propAddr, qsz uint32, q unsafe.Pointer, size *uint32) int32
	createTap       func(desc objc.ID, out *uint32) int32
	destroyTap      func(tap uint32) int32
	createAgg       func(dict objc.ID, out *uint32) int32
	destroyAgg      func(dev uint32) int32
	createIOProcID  func(dev uint32, proc uintptr, client uintptr, out *uintptr) int32
	destroyIOProcID func(dev uint32, proc uintptr) int32
	devStart        func(dev uint32, proc uintptr) int32
	devStop         func(dev uint32, proc uintptr) int32
	procName        func(pid int32, buf *byte, size uint32) int32

	ioCallback   uintptr  // one purego callback for every Tap: callbacks are never freed
	taps         sync.Map // client data → *Tap
	tapSeq       atomic.Uintptr
	micCallback  uintptr // one purego callback for every Mic
	keepCallback uintptr // the no-op IOProc that keeps a Bluetooth input warm

	halOnce sync.Once
	halErr  error
	caCA    uintptr // the CoreAudio image
)

// loadHAL binds the HAL's device calls, which Mic and Inputs need on any
// macOS.
func loadHAL() error {
	halOnce.Do(func() {
		ca, err := purego.Dlopen("/System/Library/Frameworks/CoreAudio.framework/CoreAudio", purego.RTLD_NOW|purego.RTLD_GLOBAL)
		if err != nil {
			halErr = fmt.Errorf("audio: %w", err)
			return
		}
		if _, err := purego.Dlopen("/System/Library/Frameworks/Foundation.framework/Foundation", purego.RTLD_NOW|purego.RTLD_GLOBAL); err != nil {
			halErr = fmt.Errorf("audio: %w", err)
			return
		}
		caCA = ca
		purego.RegisterLibFunc(&getPropData, ca, "AudioObjectGetPropertyData")
		purego.RegisterLibFunc(&getPropDataSize, ca, "AudioObjectGetPropertyDataSize")
		purego.RegisterLibFunc(&createIOProcID, ca, "AudioDeviceCreateIOProcID")
		purego.RegisterLibFunc(&destroyIOProcID, ca, "AudioDeviceDestroyIOProcID")
		purego.RegisterLibFunc(&devStart, ca, "AudioDeviceStart")
		purego.RegisterLibFunc(&devStop, ca, "AudioDeviceStop")
		micCallback = purego.NewCallback(micProc)
		keepCallback = purego.NewCallback(keepProc)
	})
	return halErr
}

// loadCoreAudio adds what a Tap needs: aggregates and process taps.
func loadCoreAudio() error {
	caOnce.Do(func() {
		if caErr = loadHAL(); caErr != nil {
			caErr = fmt.Errorf("tap: %w", caErr)
			return
		}
		sys, err := purego.Dlopen("/usr/lib/libSystem.B.dylib", purego.RTLD_NOW|purego.RTLD_GLOBAL)
		if err != nil {
			caErr = fmt.Errorf("tap: %w", err)
			return
		}
		purego.RegisterLibFunc(&createAgg, caCA, "AudioHardwareCreateAggregateDevice")
		purego.RegisterLibFunc(&destroyAgg, caCA, "AudioHardwareDestroyAggregateDevice")
		purego.RegisterLibFunc(&procName, sys, "proc_name")
		if objc.GetClass("CATapDescription") == 0 {
			caErr = ErrTapUnsupported
			return
		}
		purego.RegisterLibFunc(&createTap, caCA, "AudioHardwareCreateProcessTap")
		purego.RegisterLibFunc(&destroyTap, caCA, "AudioHardwareDestroyProcessTap")
		ioCallback = purego.NewCallback(ioProc)
	})
	return caErr
}

// property readers

func propRaw(obj uint32, sel, scope string, out unsafe.Pointer, size uint32) bool {
	a := propAddr{fourcc(sel), fourcc(scope), 0}
	return getPropData(obj, &a, 0, nil, &size, out) == 0
}

func propU32(obj uint32, sel, scope string) (v uint32, ok bool) {
	ok = propRaw(obj, sel, scope, unsafe.Pointer(&v), 4)
	return
}

func propF64(obj uint32, sel, scope string) (v float64) {
	propRaw(obj, sel, scope, unsafe.Pointer(&v), 8)
	return
}

func propIDs(obj uint32, sel, scope string) []uint32 {
	a := propAddr{fourcc(sel), fourcc(scope), 0}
	var size uint32
	if getPropDataSize(obj, &a, 0, nil, &size) != 0 || size < 4 {
		return nil
	}
	v := make([]uint32, size/4)
	if getPropData(obj, &a, 0, nil, &size, unsafe.Pointer(&v[0])) != 0 {
		return nil
	}
	return v[:size/4]
}

// propString reads a CFString property; the HAL hands over a +1 reference.
func propString(obj uint32, sel, scope string) string {
	var ref objc.ID
	if !propRaw(obj, sel, scope, unsafe.Pointer(&ref), 8) || ref == 0 {
		return ""
	}
	defer ref.Send(objc.RegisterName("release"))
	return objc.Send[string](ref, objc.RegisterName("UTF8String"))
}

func processName(pid int32) string {
	buf := make([]byte, 256)
	n := procName(pid, &buf[0], uint32(len(buf)))
	if n <= 0 {
		return ""
	}
	return string(buf[:n])
}

type procObj struct {
	id uint32
	App
}

func processObjects() []procObj {
	var ps []procObj
	for _, o := range propIDs(sysObj, "prs#", "glob") {
		pid, _ := propU32(o, "ppid", "glob")
		out, _ := propU32(o, "piro", "glob")
		in, _ := propU32(o, "piri", "glob")
		ps = append(ps, procObj{o, App{
			PID:    int(int32(pid)),
			Bundle: propString(o, "pbid", "glob"),
			Name:   processName(int32(pid)),
			Out:    out != 0,
			In:     in != 0,
		}})
	}
	slices.SortFunc(ps, func(a, b procObj) int { return a.PID - b.PID })
	return ps
}

// Apps lists the processes Core Audio knows as clients, by pid. The one
// playing a meeting's audio has Out set while the other side speaks.
func Apps() ([]App, error) {
	if err := loadCoreAudio(); err != nil {
		return nil, err
	}
	var apps []App
	for _, p := range processObjects() {
		apps = append(apps, p.App)
	}
	return apps, nil
}

// objc helpers

func cls(name string) objc.ID { return objc.ID(objc.GetClass(name)) }

func msg(id objc.ID, sel string, args ...any) objc.ID {
	return id.Send(objc.RegisterName(sel), args...)
}

func nsstr(s string) objc.ID { return msg(cls("NSString"), "stringWithUTF8String:", s) }

func nsarray(items ...objc.ID) objc.ID {
	a := msg(cls("NSMutableArray"), "array")
	for _, it := range items {
		msg(a, "addObject:", it)
	}
	return a
}

func nsdict(kv ...any) objc.ID {
	d := msg(cls("NSMutableDictionary"), "dictionary")
	for i := 0; i < len(kv); i += 2 {
		var v objc.ID
		switch x := kv[i+1].(type) {
		case string:
			v = nsstr(x)
		case bool:
			v = msg(cls("NSNumber"), "numberWithBool:", x)
		case objc.ID:
			v = x
		}
		msg(d, "setObject:forKey:", v, nsstr(kv[i].(string)))
	}
	return d
}

// the device

type tapState struct {
	mu      sync.Mutex
	opened  bool
	openErr error
	e       error
	info    TapInfo

	outs       [2]chan []int16 // mic, remote
	raw        chan []float32  // one IOProc block: mic mono then remote mono, same length
	id         uintptr
	tap, agg   uint32
	proc       uintptr
	micStreams int
	stopping   atomic.Bool
	closeOnce  sync.Once
	done       chan struct{} // closed when the device is torn down
	padded     atomic.Int64  // input frames
	overruns   atomic.Int64
}

func (t *Tap) start(ctx context.Context, track int) (<-chan []int16, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if !t.opened {
		t.opened = true
		if t.openErr = t.open(); t.openErr == nil {
			go func() {
				select {
				case <-ctx.Done():
					t.fail(ctx.Err())
					t.stop()
				case <-t.done:
				}
			}()
		}
	}
	if t.openErr != nil {
		return nil, t.openErr
	}
	return t.outs[track], nil
}

func (t *Tap) open() error {
	if err := loadCoreAudio(); err != nil {
		return err
	}
	runtime.LockOSThread() // the autorelease pool belongs to this thread
	defer runtime.UnlockOSThread()
	pool := msg(msg(cls("NSAutoreleasePool"), "alloc"), "init")
	defer msg(pool, "drain")

	// Mic: the aggregate's main sub-device, so its clock runs the take.
	var mic uint32
	if t.MicUID == "" {
		mic, _ = propU32(sysObj, "dIn ", "glob")
		if mic == 0 {
			return errors.New("tap: no default input device")
		}
	} else {
		for _, d := range propIDs(sysObj, "dev#", "glob") {
			if propString(d, "uid ", "glob") == t.MicUID {
				mic = d
			}
		}
		if mic == 0 {
			return fmt.Errorf("tap: no input device with UID %q", t.MicUID)
		}
	}
	micUID := propString(mic, "uid ", "glob")
	t.micStreams = len(propIDs(mic, "stm#", "inpt"))
	if t.micStreams == 0 {
		return fmt.Errorf("tap: %s has no input streams", micUID)
	}

	// Tap description: by bundle id where macOS 26 allows it (the app need
	// not be running, and a relaunched app is re-attached), else the
	// process objects running now.
	var desc objc.ID
	switch probe := msg(cls("CATapDescription"), "alloc"); {
	case len(t.Apps) == 0:
		desc = msg(probe, "initMonoGlobalTapButExcludeProcesses:", nsarray())
	case objc.Send[bool](probe, objc.RegisterName("respondsToSelector:"), objc.RegisterName("setBundleIDs:")):
		desc = msg(probe, "initMonoMixdownOfProcesses:", nsarray())
		var ids []objc.ID
		for _, b := range t.Apps {
			ids = append(ids, nsstr(b))
		}
		msg(desc, "setBundleIDs:", nsarray(ids...))
		msg(desc, "setProcessRestoreEnabled:", true)
	default:
		var objs []objc.ID
		for _, p := range processObjects() {
			if slices.Contains(t.Apps, p.Bundle) {
				objs = append(objs, msg(cls("NSNumber"), "numberWithUnsignedInt:", p.id))
			}
		}
		if len(objs) == 0 {
			msg(probe, "release")
			return fmt.Errorf("tap: none of %v is running", t.Apps)
		}
		desc = msg(probe, "initMonoMixdownOfProcesses:", nsarray(objs...))
	}
	defer msg(desc, "release")
	msg(desc, "setName:", nsstr("megameet"))
	msg(desc, "setPrivate:", true)
	msg(desc, "setMuteBehavior:", 0) // the user keeps hearing the app
	tapUID := objc.Send[string](msg(msg(desc, "UUID"), "UUIDString"), objc.RegisterName("UTF8String"))
	if st := createTap(desc, &t.tap); st != 0 {
		return osStatus("AudioHardwareCreateProcessTap", st)
	}

	t.id = tapSeq.Add(1)
	dict := nsdict(
		"name", "megameet",
		"uid", fmt.Sprintf("app.0xdao.megameet.tap.%d.%d", os.Getpid(), t.id),
		"private", true,
		// tapautostart would hold the whole device, mic included, until a
		// tapped process plays: a take started before the call connects
		// would record nothing.
		"tapautostart", false,
		"master", micUID,
		"subdevices", nsarray(nsdict("uid", micUID)),
		"taps", nsarray(nsdict("uid", tapUID, "drift", true)),
	)
	if st := createAgg(dict, &t.agg); st != 0 {
		destroyTap(t.tap)
		return osStatus("AudioHardwareCreateAggregateDevice", st)
	}
	fail := func(err error) error {
		destroyAgg(t.agg)
		destroyTap(t.tap)
		return err
	}
	streams := propIDs(t.agg, "stm#", "inpt")
	if len(streams) != t.micStreams+1 {
		return fail(fmt.Errorf("tap: aggregate has %d input streams, want the mic's %d plus the tap", len(streams), t.micStreams))
	}
	for _, s := range []uint32{streams[0], streams[t.micStreams]} {
		if err := streamFormat(s).float32(); err != nil {
			return fail(fmt.Errorf("tap: %w", err))
		}
	}
	t.info = TapInfo{Mic: propString(mic, "lnam", "glob"), MicUID: micUID, InputRate: propF64(t.agg, "nsrt", "glob")}
	if t.info.InputRate <= 0 {
		return fail(errors.New("tap: aggregate reports no sample rate"))
	}

	t.raw = make(chan []float32, 1024) // ≈ 10 s of 512-frame blocks
	t.outs = [2]chan []int16{make(chan []int16, 256), make(chan []int16, 256)}
	t.done = make(chan struct{})
	taps.Store(t.id, t)
	if st := createIOProcID(t.agg, ioCallback, t.id, &t.proc); st != 0 {
		taps.Delete(t.id)
		return fail(osStatus("AudioDeviceCreateIOProcID", st))
	}
	// Blocks until the user answers the first "System Audio Recording" prompt.
	if st := devStart(t.agg, t.proc); st != 0 {
		destroyIOProcID(t.agg, t.proc)
		taps.Delete(t.id)
		return fail(osStatus("AudioDeviceStart", st))
	}
	go t.pump()
	return nil
}

// ioProc runs on Core Audio's IO thread: it copies the block out and never
// waits. AudioBufferList: uint32 mNumberBuffers, pad, then 16-byte
// AudioBuffers {uint32 mNumberChannels, uint32 mDataByteSize, void *mData};
// the mic's streams come first, the tap's last.
func ioProc(dev uint32, now, inData unsafe.Pointer, inTime, outData, outTime unsafe.Pointer, client uintptr) int32 {
	v, ok := taps.Load(client)
	if !ok {
		return 0
	}
	t := v.(*Tap)
	if t.stopping.Load() || inData == nil {
		return 0
	}
	nb := int(*(*uint32)(inData))
	buf := func(i int) (ch int, s []float32) {
		b := unsafe.Add(inData, 8+i*16)
		ch = int(*(*uint32)(b))
		size := *(*uint32)(unsafe.Add(b, 4))
		data := *(*unsafe.Pointer)(unsafe.Add(b, 8))
		if ch == 0 || data == nil {
			return 1, nil
		}
		return ch, unsafe.Slice((*float32)(data), size/4)
	}
	if nb == 0 {
		return 0
	}
	mch, ms := buf(0)
	frames := len(ms) / mch
	block := make([]float32, 2*frames)
	downmix(block[:frames], ms, mch)
	got := 0
	if nb > t.micStreams {
		tch, ts := buf(t.micStreams)
		got = min(frames, len(ts)/tch)
		downmix(block[frames:frames+got], ts, tch)
	}
	if got < frames {
		t.padded.Add(int64(frames - got))
	}
	select {
	case t.raw <- block:
	default:
		t.overruns.Add(1)
	}
	return 0
}

func downmix(dst, src []float32, ch int) {
	for i := range dst {
		var acc float32
		for c := 0; c < ch; c++ {
			acc += src[i*ch+c]
		}
		dst[i] = acc / float32(ch)
	}
}

// pump resamples both tracks to Rate and delivers them; a device that stops
// calling back for stallAfter ends the take with an error.
func (t *Tap) pump() {
	defer func() {
		close(t.outs[0])
		close(t.outs[1])
	}()
	mic, remote := newResampler(t.info.InputRate), newResampler(t.info.InputRate)
	const stallAfter = 5 * time.Second
	stall := time.NewTimer(stallAfter)
	defer stall.Stop()
	first := true
	for {
		select {
		case b, ok := <-t.raw:
			if !ok {
				return
			}
			stall.Reset(stallAfter)
			if first {
				first = false
				t.mu.Lock()
				t.info.First = time.Now()
				t.mu.Unlock()
			}
			n := len(b) / 2
			m, r := mic.push(b[:n]), remote.push(b[n:])
			if len(m) > 0 {
				t.outs[0] <- m
				t.outs[1] <- r
			}
		case <-stall.C:
			t.fail(errors.New("tap: the audio device stopped delivering"))
			t.stop()
		}
	}
}

func (t *Tap) fail(err error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.e == nil && !t.stopping.Load() {
		t.e = err
	}
}

func (t *Tap) stop() {
	t.mu.Lock()
	up := t.opened && t.openErr == nil
	t.mu.Unlock()
	if !up {
		return
	}
	t.closeOnce.Do(func() {
		t.stopping.Store(true)
		devStop(t.agg, t.proc) // returns once the IO thread has left ioProc
		destroyIOProcID(t.agg, t.proc)
		taps.Delete(t.id)
		destroyAgg(t.agg)
		destroyTap(t.tap)
		close(t.raw)
		close(t.done)
	})
}

func (t *Tap) err() error {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.openErr != nil {
		return t.openErr
	}
	return t.e
}

// Info describes the opened device and what the take lost so far; zero
// before the first Start.
func (t *Tap) Info() TapInfo {
	t.mu.Lock()
	i := t.info
	t.mu.Unlock()
	if i.InputRate > 0 {
		i.Padded = int(math.Round(float64(t.padded.Load()) * Rate / i.InputRate))
	}
	i.Overruns = int(t.overruns.Load())
	return i
}

// format is a stream's virtual format, an AudioStreamBasicDescription.
type format struct {
	stream                    uint32
	rate                      float64
	id, flags, channels, bits uint32
}

func streamFormat(s uint32) format {
	var f [40]byte
	propRaw(s, "sfmt", "glob", unsafe.Pointer(&f[0]), 40)
	u := func(off int) uint32 { return *(*uint32)(unsafe.Pointer(&f[off])) }
	return format{stream: s, rate: *(*float64)(unsafe.Pointer(&f[0])), id: u(8), flags: u(12), channels: u(28), bits: u(32)}
}

// float32 refuses a format other than interleaved float32 PCM, the one the
// IOProcs read.
func (f format) float32() error {
	if f.id != fourcc("lpcm") || f.flags&1 == 0 || f.flags&(1<<5) != 0 || f.bits != 32 {
		return fmt.Errorf("stream %d is not interleaved float32 (format %#x flags %#x bits %d)", f.stream, f.id, f.flags, f.bits)
	}
	return nil
}

// inputDevs lists the devices with an input stream.
func inputDevs() []inputDev {
	def, _ := propU32(sysObj, "dIn ", "glob")
	var devs []inputDev
	for _, d := range propIDs(sysObj, "dev#", "glob") {
		streams := propIDs(d, "stm#", "inpt")
		if len(streams) == 0 {
			continue
		}
		ch := 0
		for _, s := range streams {
			ch += int(streamFormat(s).channels)
		}
		tr, _ := propU32(d, "tran", "glob")
		devs = append(devs, inputDev{
			id: d, uid: propString(d, "uid ", "glob"), name: propString(d, "lnam", "glob"),
			channels: ch, transport: string([]byte{byte(tr >> 24), byte(tr >> 16), byte(tr >> 8), byte(tr)}), def: d == def,
		})
	}
	return devs
}

// Inputs lists the devices with an input stream, the default one marked.
func Inputs() ([]Input, error) {
	if err := loadHAL(); err != nil {
		return nil, err
	}
	var in []Input
	for _, d := range inputDevs() {
		in = append(in, Input{UID: d.uid, Name: d.name, Channels: d.channels, Default: d.def})
	}
	return in, nil
}
