package keypad

import (
	"context"
	"errors"
	"io"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"
)

func TestParseCallout(t *testing.T) {
	out := `+-o MegaVoice Keys@01121000  <class IOUSBHostDevice, id 0x1>
    | |   "IOTTYDevice" = "usbmodem11211"
    | |   "IOCalloutDevice" = "/dev/cu.usbmodem11211"
    | |   "IODialinDevice" = "/dev/tty.usbmodem11211"`
	if got := parseCallout([]byte(out)); got != "/dev/cu.usbmodem11211" {
		t.Fatalf("parseCallout = %q", got)
	}
	if got := parseCallout([]byte("")); got != "" {
		t.Fatalf("parseCallout(empty) = %q", got)
	}
}

// fakePort records writes and feeds reads from in; closing in (hang) or Close
// ends Read the way a real port does.
type fakePort struct {
	mu      sync.Mutex
	written strings.Builder
	fail    bool
	short   int // the next write takes only this many bytes, then EAGAIN
	in      chan []byte
	done    chan struct{}
	once    sync.Once
}

func newFakePort() *fakePort {
	return &fakePort{in: make(chan []byte, 8), done: make(chan struct{})}
}

func (p *fakePort) Write(b []byte) (int, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.fail {
		return 0, errors.New("device gone")
	}
	if p.short > 0 {
		n := p.short
		p.short = 0
		p.written.Write(b[:n])
		return n, syscall.EAGAIN
	}
	p.written.Write(b)
	return len(b), nil
}

func (p *fakePort) Read(b []byte) (int, error) {
	select {
	case chunk, ok := <-p.in:
		if !ok {
			return 0, io.EOF
		}
		return copy(b, chunk), nil
	case <-p.done:
		return 0, errors.New("closed")
	}
}

func (p *fakePort) Close() error { p.once.Do(func() { close(p.done) }); return nil }

func (p *fakePort) text() string {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.written.String()
}

func (p *fakePort) set(f func(*fakePort)) {
	p.mu.Lock()
	defer p.mu.Unlock()
	f(p)
}

// fakeTransport hands out ports in order, then none.
type fakeTransport struct {
	mu    sync.Mutex
	ports []*fakePort
	dials int
}

func (f *fakeTransport) Dial() (io.ReadWriteCloser, string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.dials >= len(f.ports) {
		return nil, "", nil
	}
	f.dials++
	return f.ports[f.dials-1], "/dev/cu.fake", nil
}

func run(t *testing.T, o *Outlet) {
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	go o.Run(ctx)
}

// The whole snapshot reaches the keypad at once on a change; after a failed
// write the outlet dials again and resends the current snapshot.
func TestRunPushesSnapshotAndReconnectsAfterWriteFailure(t *testing.T) {
	first, second := newFakePort(), newFakePort()
	o := &Outlet{Transport: &fakeTransport{ports: []*fakePort{first, second}}, Every: 20 * time.Millisecond}
	o.Set("idle")
	run(t, o)
	waitFor(t, func() bool {
		return strings.Contains(first.text(), "state idle\nend paste\nlock 0\ngroup \ntopic \ndone none\n")
	})
	o.Update(func(s *Snapshot) {
		s.State, s.Send, s.Lock, s.Group, s.Topic = "recording", true, true, "报到群", "ccc-status"
	})
	first.in <- []byte("key1 down\n")
	full := "state recording\nend send\nlock 1\ngroup 报到群\ntopic ccc-status\ndone none\n"
	waitFor(t, func() bool { return strings.Contains(first.text(), full) })
	first.set(func(p *fakePort) { p.fail = true })
	reset := strings.Replace(full, "lock 1", "lock 0", 1)
	waitFor(t, func() bool { return strings.Contains(second.text(), reset) })
	second.in <- []byte("key1 down\n")
	waitFor(t, func() bool { return strings.Contains(second.text(), full) })
}

// A keypad that speaks only the HID key protocol never gets a locked
// face. Evidence of link edges belongs to one port session alone.
func TestKey1CapabilityIsPerConnection(t *testing.T) {
	first, second := newFakePort(), newFakePort()
	o := &Outlet{Transport: &fakeTransport{ports: []*fakePort{first, second}}, Every: 20 * time.Millisecond}
	o.Update(func(s *Snapshot) { s.State, s.Lock = "idle", true })
	run(t, o)
	waitFor(t, func() bool { return strings.Contains(first.text(), "lock 0\n") })
	first.in <- []byte("key4\n")
	if e := next(t, o.Events()); e.Kind != Key4 {
		t.Fatalf("event %+v", e)
	}
	if _, ready := o.Key1State(); ready || o.Snapshot().Lock {
		t.Fatal("legacy firmware enabled the lock")
	}
	first.in <- []byte("key1 down\n")
	e := next(t, o.Events())
	connection, ready := o.Key1State()
	if e.Kind != Key1Down || e.Connection != connection || !ready {
		t.Fatalf("key1 event %+v, connection %d ready %v", e, connection, ready)
	}
	waitFor(t, func() bool { return strings.Contains(first.text(), "lock 1\n") })
	close(first.in)
	waitFor(t, func() bool { return strings.Contains(second.text(), "lock 0\n") })
	o.key1Seen(connection) // a reader from the previous port cannot re-arm the lock
	if _, ready := o.Key1State(); ready || o.Snapshot().Lock {
		t.Fatal("reconnect kept the previous capability")
	}
	second.in <- []byte("key1 up\n")
	e = next(t, o.Events())
	if e.Kind != Key1Up || e.Connection == connection {
		t.Fatalf("reconnected key1 event %+v", e)
	}
}

func TestKey1HandshakeAndShownSnapshot(t *testing.T) {
	p := newFakePort()
	o := &Outlet{Transport: &fakeTransport{ports: []*fakePort{p}}, Every: time.Hour}
	o.Update(func(s *Snapshot) {
		s.State, s.Lock, s.Chat, s.TopicID, s.Group, s.Topic = "idle", true, "-1009", 23, "Saved", "Topic"
	})
	run(t, o)
	waitFor(t, func() bool { return strings.Contains(p.text(), "lock 0\n") })
	p.in <- []byte("key1 up\n") // firmware's opening state; no press required
	up := next(t, o.Events())
	if up.Kind != Key1Up || up.Shown.Lock {
		t.Fatalf("opening state %+v", up)
	}
	waitFor(t, func() bool { return strings.Contains(p.text(), "lock 1\n") })
	waitFor(t, func() bool { return o.key1Seen(up.Connection).Lock })
	p.in <- []byte("key1 down\n")
	down := next(t, o.Events())
	if !down.Shown.Lock || down.Shown.Chat != "-1009" || down.Shown.TopicID != 23 {
		t.Fatalf("down did not carry shown lock: %+v", down)
	}
}

// Knob and key 4 lines from the keypad come out as events, and keep coming
// after the port hangs up and the outlet dials a new one.
func TestRunReadsEventsAcrossReconnect(t *testing.T) {
	first, second := newFakePort(), newFakePort()
	o := &Outlet{Transport: &fakeTransport{ports: []*fakePort{first, second}}, Every: 20 * time.Millisecond}
	o.Set("idle")
	run(t, o)
	ev := o.Events()
	first.in <- []byte("knob 2\nkno")
	first.in <- []byte("b -1 held\nnoise\nkey4\n")
	for _, want := range []Event{{Kind: Knob, N: 2}, {Kind: Knob, N: -1, Held: true}, {Kind: Key4}} {
		if got := next(t, ev); got != want {
			t.Fatalf("event = %+v, want %+v", got, want)
		}
	}
	close(first.in) // the keypad hangs up
	waitFor(t, func() bool { return strings.Contains(second.text(), "state idle\n") })
	second.in <- []byte("knob -3\n")
	if got := next(t, ev); got != (Event{Kind: Knob, N: -3}) {
		t.Fatalf("event after reconnect = %+v", got)
	}
}

// A done word shows for DoneHold, then reads none again; a newer one restarts
// the hold.
func TestDoneClearsAfterHold(t *testing.T) {
	p := newFakePort()
	o := &Outlet{Transport: &fakeTransport{ports: []*fakePort{p}}, Every: time.Hour, DoneHold: 300 * time.Millisecond}
	o.Set("idle")
	run(t, o)
	o.Update(func(s *Snapshot) { s.Done = DonePasted })
	waitFor(t, func() bool { return strings.Contains(p.text(), "done pasted\n") })
	time.Sleep(150 * time.Millisecond)
	o.Update(func(s *Snapshot) { s.Done = DoneSent })
	time.Sleep(200 * time.Millisecond) // past the first hold, inside the second
	if d := o.Snapshot().Done; d != DoneSent {
		t.Fatalf("Done = %q 350ms in, want sent (the newer hold)", d)
	}
	waitFor(t, func() bool { return o.Snapshot().Done == DoneNone })
	waitFor(t, func() bool { return strings.HasSuffix(p.text(), "done none\n") })
}

// The same done word twice within the hold flashes twice in full: the second
// restarts the hold and is pushed.
func TestSameDoneRestartsHold(t *testing.T) {
	p := newFakePort()
	o := &Outlet{Transport: &fakeTransport{ports: []*fakePort{p}}, Every: time.Hour, DoneHold: 300 * time.Millisecond}
	run(t, o)
	o.Set("idle")
	waitFor(t, func() bool { return strings.Contains(p.text(), "state idle\n") })
	time.Sleep(20 * time.Millisecond) // no push pending
	o.Update(func(s *Snapshot) { s.Done = DoneSent })
	waitFor(t, func() bool { return strings.Contains(p.text(), "done sent\n") })
	time.Sleep(150 * time.Millisecond)
	n := strings.Count(p.text(), "done sent\n")
	o.Update(func(s *Snapshot) { s.Done = DoneSent })
	waitFor(t, func() bool { return strings.Count(p.text(), "done sent\n") > n })
	time.Sleep(200 * time.Millisecond) // past the first hold, inside the second
	if d := o.Snapshot().Done; d != DoneSent {
		t.Fatalf("Done = %q 350ms in, want sent (the second flash's hold)", d)
	}
	waitFor(t, func() bool { return o.Snapshot().Done == DoneNone })
}

// A keypad that hangs up is dialled again on the next tick, not at once, and
// a change while it is away does not dial either.
func TestRedialWaitsForTick(t *testing.T) {
	first, second := newFakePort(), newFakePort()
	tr := &fakeTransport{ports: []*fakePort{first, second}}
	o := &Outlet{Transport: tr, Every: 400 * time.Millisecond}
	o.Set("idle")
	run(t, o)
	waitFor(t, func() bool { return strings.Contains(first.text(), "state idle\n") })
	close(first.in) // the keypad hangs up
	time.Sleep(50 * time.Millisecond)
	o.Set("recording")
	time.Sleep(100 * time.Millisecond)
	tr.mu.Lock()
	dials := tr.dials
	tr.mu.Unlock()
	if dials != 1 {
		t.Fatalf("dialled %d times before the next tick, want 1", dials)
	}
	waitFor(t, func() bool { return strings.Contains(second.text(), "state recording\n") })
}

// A write the port took only part of is ended by a newline before the next
// snapshot, so the firmware drops the half line instead of misreading it.
func TestTornWriteIsTerminated(t *testing.T) {
	p := newFakePort()
	p.short = 7 // "state i"
	o := &Outlet{Transport: &fakeTransport{ports: []*fakePort{p}}, Every: 20 * time.Millisecond}
	o.Set("idle")
	run(t, o)
	waitFor(t, func() bool { return strings.HasPrefix(p.text(), "state i\nstate idle\n") })
}

func next(t *testing.T, ev <-chan Event) Event {
	t.Helper()
	select {
	case e := <-ev:
		return e
	case <-time.After(2 * time.Second):
		t.Fatal("no event")
		return Event{}
	}
}

func waitFor(t *testing.T, ok func() bool) {
	t.Helper()
	for end := time.Now().Add(2 * time.Second); time.Now().Before(end); time.Sleep(5 * time.Millisecond) {
		if ok() {
			return
		}
	}
	t.Fatal("timed out")
}

// Connected follows the port: false before a dial finds one, true while it
// is open, false once it ends and no other is found.
func TestConnected(t *testing.T) {
	port := newFakePort()
	o := &Outlet{Transport: &fakeTransport{ports: []*fakePort{port}}, Every: 20 * time.Millisecond}
	if o.Connected() {
		t.Fatal("connected before Run")
	}
	o.Set("idle")
	run(t, o)
	waitFor(t, o.Connected)
	port.Close()
	waitFor(t, func() bool { return !o.Connected() })
}
