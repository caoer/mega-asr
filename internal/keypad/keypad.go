// Package keypad links Mega Voice with a desk keypad with a screen on each
// key — a LILYGO T-Keyboard-S3-Pro running the "MegaVoice Keys" firmware;
// its source is the desktop-displays repo, tkeyboard/firmware.
//
// The Mac owns all state and pushes it whole as a Snapshot, at every change
// and every Every, so the keypad shows megavoice as offline when the lines
// stop; the keypad sends back knob turns and key 4 presses as Events. The
// line protocol (protocol.go) rides any byte stream a Transport dials: Serial
// is the USB one.
package keypad

import (
	"context"
	"errors"
	"io"
	"log"
	"os/exec"
	"regexp"
	"sync"
	"syscall"
	"time"
)

// Product is the USB product name the firmware enumerates with.
const Product = "MegaVoice Keys"

// Link is the keypad as the rest of Mega Voice sees it, whatever carries it.
type Link interface {
	// Update changes the snapshot in place and pushes it if it changed; it
	// never blocks on the keypad.
	Update(func(*Snapshot))
	// Events delivers what the keypad reports, across reconnects.
	Events() <-chan Event
	// Run keeps the keypad connected and fed until ctx ends.
	Run(ctx context.Context)
	// Connected reports whether a keypad is connected now: its port open
	// and taking the snapshot.
	Connected() bool
}

// Key1Link reports whether this connection has supplied key-1 edges. A
// link without that evidence cannot show or use a locked recording target.
type Key1Link interface {
	Key1State() (connection uint64, ready bool)
}

// Transport reaches the keypad's byte stream.
type Transport interface {
	// Dial connects to the keypad: a nil port and nil error when none is
	// present now. name labels the connection in logs. The port's Read
	// blocks until bytes arrive and returns an error once the port is gone
	// or closed.
	Dial() (port io.ReadWriteCloser, name string, err error)
}

// Serial is the keypad's USB serial port. The port is found by the USB
// product name, never by opening ports to probe them: opening an ESP32's
// USB-Serial-JTAG port resets that board.
type Serial struct {
	// Find returns the device path, "" when none is plugged in. Nil means
	// FindDarwin.
	Find func() (string, error)
	// Open opens the device. Nil means openPort.
	Open func(path string) (io.ReadWriteCloser, error)
}

func (s Serial) Dial() (io.ReadWriteCloser, string, error) {
	find, open := s.Find, s.Open
	if find == nil {
		find = FindDarwin
	}
	if open == nil {
		open = openPort
	}
	path, err := find()
	if err != nil || path == "" {
		return nil, "", err
	}
	p, err := open(path)
	if err != nil {
		return nil, "", err
	}
	return p, path, nil
}

// Outlet is the Link over a Transport, dialling it again as the keypad comes
// and goes.
type Outlet struct {
	// Transport nil means Serial{}.
	Transport Transport
	// Every is how often the snapshot is repeated and a missing keypad is
	// looked for again; zero means 2 s.
	Every time.Duration
	// DoneHold is how long a Done other than none shows before it reads
	// none again; zero means 2 s.
	DoneHold time.Duration

	mu            sync.Mutex
	snap          Snapshot
	doneGen       int
	wake          chan struct{}
	events        chan Event
	port          bool // Run holds an open port
	connection    uint64
	key1          bool     // this port session has supplied a key-1 edge
	shownSnapshot Snapshot // the last fully written snapshot on this connection
}

var _ Link = (*Outlet)(nil)

// Connected reports whether Run holds an open port to the keypad.
func (o *Outlet) Connected() bool {
	o.mu.Lock()
	defer o.mu.Unlock()
	return o.port
}

// connected records whether Run holds an open port.
func (o *Outlet) connected(on bool) {
	o.mu.Lock()
	o.port = on
	o.connection++
	o.key1 = false
	o.shownSnapshot = Snapshot{}
	o.mu.Unlock()
}

func (o *Outlet) Key1State() (uint64, bool) {
	o.mu.Lock()
	defer o.mu.Unlock()
	return o.connection, o.port && o.key1
}

func (o *Outlet) key1Seen(connection uint64) Snapshot {
	o.mu.Lock()
	defer o.mu.Unlock()
	var shown Snapshot
	if o.port && o.connection == connection {
		shown = o.shownSnapshot
		if !o.key1 {
			o.key1 = true
			poke(o.wakeChan())
		}
	}
	return shown
}

func (o *Outlet) shown(connection uint64, snapshot Snapshot) {
	o.mu.Lock()
	defer o.mu.Unlock()
	if o.port && o.connection == connection {
		o.shownSnapshot = snapshot
	}
}

// Set changes only the take state; see Update.
func (o *Outlet) Set(state string) {
	o.Update(func(s *Snapshot) { s.State = state })
}

// Update changes the snapshot and wakes the writer when it changed. An
// Update that leaves Done other than none (re)starts its DoneHold and pushes,
// so the same word twice flashes twice in full; after the hold Done reads
// none.
func (o *Outlet) Update(f func(*Snapshot)) {
	o.mu.Lock()
	old := o.snap
	f(&o.snap)
	now := o.snap
	flash := now.Done != "" && now.Done != DoneNone
	if flash || now.Done != old.Done {
		o.doneGen++
	}
	if flash {
		gen, hold := o.doneGen, o.DoneHold
		if hold <= 0 {
			hold = 2 * time.Second
		}
		time.AfterFunc(hold, func() { o.clearDone(gen) })
	}
	w := o.wakeChan()
	o.mu.Unlock()
	if flash || now != old {
		poke(w)
	}
}

func (o *Outlet) clearDone(gen int) {
	o.mu.Lock()
	if o.doneGen != gen {
		o.mu.Unlock()
		return
	}
	o.snap.Done = DoneNone
	o.doneGen++
	w := o.wakeChan()
	o.mu.Unlock()
	poke(w)
}

func poke(w chan struct{}) {
	select {
	case w <- struct{}{}:
	default:
	}
}

// Snapshot returns what the keypad is being sent. The lock stays hidden
// until key 1 has identified itself over this connection's link.
func (o *Outlet) Snapshot() Snapshot {
	o.mu.Lock()
	defer o.mu.Unlock()
	s := o.snap
	s.Lock = s.Lock && o.port && o.key1
	return s
}

// Events delivers the keypad's events. A full buffer drops the event rather
// than stall the reader.
func (o *Outlet) Events() <-chan Event {
	o.mu.Lock()
	defer o.mu.Unlock()
	return o.eventChan()
}

func (o *Outlet) eventChan() chan Event {
	if o.events == nil {
		o.events = make(chan Event, 64)
	}
	return o.events
}

func (o *Outlet) wakeChan() chan struct{} {
	if o.wake == nil {
		o.wake = make(chan struct{}, 1)
	}
	return o.wake
}

// Run writes the snapshot until ctx ends: at once on a change, and every
// Every; a reader turns the keypad's lines into Events. A failed write or a
// port the reader saw end closes the port. Dialling waits for the tick, so a
// keypad that keeps dropping, or a burst of changes while it is away, costs
// one dial per Every.
func (o *Outlet) Run(ctx context.Context) {
	tr, every := o.Transport, o.Every
	if tr == nil {
		tr = Serial{}
	}
	if every <= 0 {
		every = 2 * time.Second
	}
	o.mu.Lock()
	wake, events := o.wakeChan(), o.eventChan()
	o.mu.Unlock()
	t := time.NewTicker(every)
	defer t.Stop()

	var (
		port       io.ReadWriteCloser
		connection uint64
		name       string
		lost       chan struct{} // closed by the reader when the port ends
		torn       bool          // a short write left half a line on the wire
		lastErr    string
		dial       = true // the tick allows a dial
	)
	drop := func(why string) {
		log.Printf("keypad: %s: %s; looking for it again", name, why)
		port.Close()
		port, lost = nil, nil
		o.connected(false)
	}
	defer func() {
		if port != nil {
			port.Close()
			o.connected(false)
		}
	}()
	for {
		if port == nil && dial {
			dial = false
			p, n, err := tr.Dial()
			if err != nil {
				if err.Error() != lastErr {
					log.Printf("keypad: %v", err)
					lastErr = err.Error()
				}
			} else if p != nil {
				port, name, lost, torn, lastErr = p, n, make(chan struct{}), false, ""
				o.connected(true)
				connection, _ = o.Key1State()
				go read(p, events, lost, connection, o.key1Seen)
				log.Printf("keypad: connected on %s", n)
			}
		}
		if port != nil {
			snapshot := o.Snapshot()
			if b := snapshot.Encode(); b != nil {
				if torn {
					b = append([]byte{'\n'}, b...) // end the half line; the firmware drops it
				}
				n, err := port.Write(b)
				if n == len(b) && err == nil {
					o.shown(connection, snapshot)
				}
				torn = n > 0 && n < len(b) || errors.Is(err, syscall.EAGAIN) && torn
				if err != nil && !errors.Is(err, syscall.EAGAIN) {
					drop(err.Error())
				}
			}
		}
		select {
		case <-ctx.Done():
			return
		case <-wake:
		case <-t.C:
			dial = true
		case <-lost:
			drop("port ended")
		}
	}
}

// read parses the keypad's lines into events until the port ends.
func read(p io.Reader, events chan<- Event, lost chan<- struct{}, connection uint64, key1Seen func(uint64) Snapshot) {
	defer close(lost)
	var ls lines
	buf := make([]byte, 256)
	emit := func(line string) {
		ev, ok := ParseEvent(line)
		if !ok {
			return
		}
		if ev.Kind == Key1Down || ev.Kind == Key1Up {
			ev.Connection = connection
			ev.Shown = key1Seen(connection)
		}
		select {
		case events <- ev:
		default:
			log.Printf("keypad: event dropped, nobody reading: %q", line)
		}
	}
	for {
		n, err := p.Read(buf)
		ls.feed(buf[:n], emit)
		if err != nil {
			return
		}
	}
}

var calloutRE = regexp.MustCompile(`"IOCalloutDevice" = "([^"]+)"`)

// FindDarwin asks the IORegistry for the serial port under the USB device
// named Product; "" when it is not plugged in.
func FindDarwin() (string, error) {
	out, err := exec.Command("ioreg", "-r", "-n", Product, "-l", "-w0").Output()
	if err != nil {
		return "", err
	}
	return parseCallout(out), nil
}

// parseCallout returns the first IOCalloutDevice in ioreg output.
func parseCallout(out []byte) string {
	if m := calloutRE.FindSubmatch(out); m != nil {
		return string(m[1])
	}
	return ""
}
