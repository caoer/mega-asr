// Package session is megavoice's controller. A tap starts a take and a second
// tap stops it to paste; Enter stops it to paste and send, or, while its text
// is still on the way, turns a paste into a send. The delivery target is
// locked when the take starts.
//
// Audio comes first at a start: Deliverer.Capture reads the focus on its own
// goroutine from the tap while the source opens, so a slow answer from herdr
// delays neither the first block nor the controller's lock. The target is
// awaited only where it is read, off the lock; the start line waits for it,
// and no other line of the take is written before the start line.
//
// While a take records, every block goes to its WAV as it arrives and the
// stream is cut into chunks at pauses. A take no longer than WholeMax is
// decoded at stop as one window, its silent head and tail trimmed: the model
// sees the whole context, and no text is stitched across a cut. A longer take
// streams: once it passes WholeMax, one ASR worker transcribes its chunks in
// order, in the background, so a stop waits only for the last chunk. A new
// take can start while earlier ones transcribe; each take keeps its own
// target and text, and takes deliver in the order they were recorded.
//
// A streaming primary engine (Stream) instead gets every block as it is
// recorded and answers once the take ends; the chunk ASR then transcribes
// only a take whose stream failed. A Tee gets a copy of each take for a
// comparison and never holds up its delivery.
//
// Every take writes its record, <base>.events.jsonl (store.Append): a line at
// each transition, and one final state on disk before the overlay reports it
// — sent, pasted, cancelled, empty, or undelivered with its reason. Every
// path that delivers (a take's own turn, a command, the hotkey, the page)
// holds the one delivery lock from the focus change through the Enter, since
// they share the pasteboard and the focus.
package session

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"slices"
	"sync"
	"sync/atomic"
	"time"

	"github.com/caoer/mega-asr/internal/asr"
	"github.com/caoer/mega-asr/internal/audio"
	"github.com/caoer/mega-asr/internal/post"
	"github.com/caoer/mega-asr/internal/store"
)

// State is the controller's phase, as far as the key tap cares.
type State int

const (
	Idle         State = iota
	Recording          // a take records (earlier ones may still transcribe)
	Transcribing       // no take records; a stopped take awaits its text
	Delivering         // no take records; a take's text is being delivered
)

func (s State) String() string {
	return [...]string{"idle", "recording", "transcribing", "delivering"}[s]
}

// Target is where a transcript goes, captured at start.
type Target interface{ String() string }

// Deliverer captures the target at start and later delivers text to it. A
// Deliver error means the text was left on the clipboard instead. Submit
// presses Enter in the target after a successful Deliver.
type Deliverer interface {
	Capture() Target
	Deliver(t Target, text string) error
	Submit(t Target) error
}

// ReadBacker is a Deliverer that can read a target back after a delivery:
// whole, short or unknown, and why; "" when the target has nothing to read.
type ReadBacker interface {
	Received(t Target, text string) (verdict, why string)
}

// View is what the overlay shows; the zero View hides it.
type View struct {
	// Quality is the recording take's waveform; nil when nothing records.
	Quality func() []audio.Quality
	// Warming says the recording take's Bluetooth input has delivered only
	// digital silence so far: the headset is still switching into call mode
	// and what is said now is lost.
	Warming bool
	// NoSignal says the recording take's input has delivered digital silence
	// for audio.DeadAfter (audio.BluetoothWarmup while warming) up to now; it
	// holds until a live block arrives.
	NoSignal bool
	// Device names the recording take's input ("Handheld BT"); "" when the
	// source does not say.
	Device string
	// Started is when the newest take still on its way was stopped; zero
	// when there is none.
	Started time.Time
	// Send says that take presses Enter after its paste.
	Send bool
	// Estimate is the time left until that take's text is ready; 0 when it
	// cannot be estimated yet.
	Estimate func() time.Duration
}

// UI shows the controller's state; the overlay implements it. Flash shows a
// message for a moment over whatever the last View shows; Alert shows a
// failure the same way, for longer.
type UI interface {
	Show(v View)
	Flash(msg string)
	Alert(msg string)
}

// Chunker cuts a stream into chunks; audio.Chunker is the real one.
type Chunker interface {
	Push(s []int16) []audio.Cut
}

// Controller owns the takes.
type Controller struct {
	NewSource  func() audio.Source
	NewChunker func() Chunker // nil: audio.Chunker with its defaults
	ASR        asr.Transcriber
	// Stream, when set, is the primary engine: each take streams to it as it
	// records, and its text is the take's. ASR then transcribes a take whose
	// stream failed, from the take's WAV, and recovered takes.
	Stream asr.Streamer
	// Tee, when set, is called as a take starts; the TakeTee it returns (nil:
	// none) gets a copy of the take.
	Tee         func(base string) TakeTee
	Post        post.Chain
	Deliver     Deliverer
	UI          UI
	Store       store.Store
	ChunkDir    string           // chunk WAVs, removed once a take's text is saved
	MaxDuration time.Duration    // auto-stop per take; 0 = none
	WholeMax    time.Duration    // a take this long or shorter is decoded whole at stop; 0 streams every take
	OnState     func(State)      // tells the key tap which keys to take
	Now         func() time.Time // clock for file names
	// NewBackup, when set, gives a take's backup track for its main source,
	// which has started; nil when that source has none. The backup records
	// beside the main track into Store's backup/<name>.wav, which no listing
	// of the store's takes reads, and stops with it. At stop it is removed
	// unless the main track delivered nothing for a while: over the whole
	// take, digital silence that turned the no-signal notice on, a Bluetooth
	// input's warm-up, or the time after its last block when its stream
	// ended on its own. Where the backup holds a voice over such a span, the
	// take is decoded with the backup's audio in that span's place; what the
	// main track recorded elsewhere is decoded as it is.
	NewBackup func(main audio.Source) audio.Source
	// OnChunks, when set, gets a finished take's chunks in order, before
	// the take is delivered: a meeting keeps each chunk's time and text.
	OnChunks func(take string, chunks []Chunk)
	// Ctx is the ASR calls' context; nil is context.Background. Once it is
	// done, each chunk still queued fails with its error at once.
	Ctx context.Context
	// Engine and Model name the primary engine, ChunkEngine the chunk ASR
	// ("" is Engine), Build this build: the start and text lines carry them.
	Engine, Model, ChunkEngine, Build string
	// Sound, when set, plays one system sound; it is called once for each
	// take that becomes undelivered, and never for a cancel.
	Sound func()
	// HotkeyKey is the tap key's name in the resend hotkey's hint on a
	// flash; empty names the default tap key, 右Option.
	HotkeyKey string

	mu       sync.Mutex
	state    State
	rec      *take         // the recording take
	finished []*take       // stopped, not yet delivered, oldest first; cancelled ones too
	last     chan struct{} // closed when the newest take is done delivering
	seq      int           // takes started
	shown    viewKey
	wg       sync.WaitGroup // takes and recoveries in flight
	inflight atomic.Int64   // recoveries, resends, re-transcriptions and delivery-lock holders in flight
	// recovering: takes Recover transcribes whose text line is not on disk
	// yet, by base, with when each started
	recovering map[string]time.Time
	pub        atomic.Pointer[published]
	prev       string    // the newest take's base
	prevStop   time.Time // when it stopped
	// cancelled: the newest take to end was cancelled; a Return with no
	// take recording is then the app's
	cancelled bool

	resending      map[string]bool // takes a resend is delivering, by base
	retranscribing map[string]bool // takes a re-transcription is decoding, by base
	hotkeying      atomic.Bool     // a hotkey paste is under way
	rmu            sync.Mutex      // a re-transcription's count of the record's lines and its append

	// closing, once set, refuses every take start, resend and
	// re-transcription, and is the words a refused start flashes (Close)
	closing atomic.Pointer[string]
	omu     sync.Mutex
	open    map[*take]bool // takes started and not yet done, for Interrupt

	lockOnce sync.Once
	dlock    chan struct{} // the delivery lock: one token

	qmu        sync.Mutex
	live, bg   []*chunk // ASR queue; background chunks wait while a take is on its way
	wake       chan struct{}
	workerOnce sync.Once
	// yield cancels the background chunk the worker decodes, nil when it
	// decodes none; yielded: a take started meanwhile and cancelled it.
	// Guarded by qmu.
	yield   context.CancelFunc
	yielded bool

	est estimator
}

// TakeTee receives a take beside the primary engine — a comparison. Its
// methods are called on the take's path and must return at once.
type TakeTee interface {
	Write(s []int16) // each block as it is recorded
	// End: the take's audio is complete, stopped at stopped (a tap, Enter,
	// Esc, auto-stop, or the stream's end); wav is its closed WAV, "" when
	// the take was too short and is removed — Done then never comes.
	End(stopped time.Time, wav string, samples int)
	Done(r TakeResult) // the primary's text is known
}

// TakeResult is the primary engine's answer for a take.
type TakeResult struct {
	Raw, Text string        // the primary's text, before and after the post chain
	Failed    int           // chunks without text: Raw is partial or empty
	Latency   time.Duration // from stop to text
	StreamErr error         // the streaming primary failed; Raw is the chunk ASR's
}

// take is one recording from start to delivery.
type take struct {
	target  *pending // what had focus at the tap
	tapAt   time.Time
	src     audio.Source
	stream  asr.Stream // the streaming primary's; nil with a chunk primary
	tee     TakeTee
	levels  *audio.Levels
	file    *store.Take
	chunker Chunker
	seq     int           // start order; the ASR takes older takes' chunks first
	base    string        // the take's path without extension
	dir     string        // its chunk WAVs
	chunks  []*chunk      // appended by the take's own goroutine only
	pending atomic.Int64  // samples queued and not yet transcribed
	turn    chan struct{} // closed when the take before is done delivering
	next    chan struct{} // closed when this take is done delivering
	auto    *time.Timer
	backup  *backup // nil without NewBackup
	// ctx is a re-transcription's: once it is done, the take's chunks fail
	// with its error. nil for any other take.
	ctx context.Context
	// first is when the main track's first block arrived, zero until then;
	// the backup is lined up by it. Written and read by record's goroutine.
	first time.Time
	// fromBackup: the backup's audio is queued in place of the main track's
	// dead spans; set on the take's own goroutine before finish.
	fromBackup bool
	// interrupted: Interrupt closed its WAV and wrote its last lines; the
	// recording goroutine ends without a line of its own
	interrupted atomic.Bool
	// stopLine is the stop line it writes once its start line is on disk,
	// published as it is stopped. Interrupt, which never waits for
	// Controller.mu, reads it.
	stopLine atomic.Pointer[store.Event]
	// paste orders the take's paste against Interrupt: finish moves it from 0
	// to pasteBegun just before Deliver, and back to 0 when Deliver fails;
	// Interrupt moves it from 0 to pasteBarred. Whichever moves it first
	// decides: a barred take is never pasted, and Interrupt marks a take
	// whose paste has begun delivery_cut.
	paste atomic.Int32

	jmu     sync.Mutex    // orders the record's first lines
	startEv store.Event   // the start line, less its target
	started bool          // the start line is written; guarded by jmu
	early   []store.Event // lines noted before the start line; guarded by jmu
	logged  chan struct{} // closed once the start line and early are on disk

	// guarded by Controller.mu
	kind       string    // what stopped it: a stop line's kind
	via        string    // where the stop came from: a stop line's via ("" a key)
	recAt      time.Time // when it started recording
	warming    bool      // a Bluetooth input with no live sample yet (View.Warming)
	noSignal   bool      // View.NoSignal
	device     string    // View.Device
	send       bool      // press Enter after delivery
	sealed     bool      // delivery has decided on send
	delivering bool
	cancelled  bool // Esc: transcribed and kept as cancelled, never delivered
	stopAt     time.Time
	why        string // non-empty when the stream ended on its own
}

// A take's paste state (take.paste).
const (
	pasteBegun  = 1 // Deliver is called: the text may land
	pasteBarred = 2 // Interrupt marked the take: it is never pasted
)

// Chunk is one piece of a finished take as the ASR returned it.
type Chunk struct {
	Offset time.Duration // where it starts in the take
	Length time.Duration
	// Start, End: the chunk is the take's samples [Start, End).
	Start, End int
	// From, To: the kinds of its first and last edge (audio.EdgeStart …
	// audio.EdgeWhole).
	From, To string
	Raw      string // the ASR's text
	Text     string // Raw after the post chain
	Err      error  // the ASR failed on it; Raw and Text are empty
}

// chunk is a piece of a take on its way through the ASR.
type chunk struct {
	t        *take
	n        int
	wav      string
	offset   int // samples from the take's start
	samples  int
	from, to string // edge kinds, as Chunk's
	whole    bool   // the whole take, decoded as one window
	text     string
	err      error
	done     chan struct{}
}

// State reports the current phase.
func (c *Controller) State() State {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.state
}

// Toggle starts a take when none records, else stops the recording one to
// paste. Earlier takes still transcribing carry on. It is the key tap's;
// ToggleFrom names another trigger.
func (c *Controller) Toggle() { c.ToggleFrom("tap") }

// ToggleFrom is Toggle for a take started by trigger (tap, ctl or replay),
// which its start line records.
func (c *Controller) ToggleFrom(trigger string) { c.ToggleAt(trigger, time.Now()) }

// ToggleAt is ToggleFrom with the time of the key event that asked for it,
// which a take's start line records as tap_at.
func (c *Controller) ToggleAt(trigger string, at time.Time) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.rec == nil {
		c.startLocked(trigger, at)
		return
	}
	via := ""
	if trigger == "ctl" {
		via = "ctl"
	}
	c.stopFromLocked(via)
}

// StopFrom stops the recording take to paste, as a tap does, and never
// starts one. via names where the stop came from on its stop line ("menu",
// "ctl"); id, when set, is the take the caller was shown: another take is
// left recording. It reports false when it stopped nothing. The take
// delivers to the target it locked at its start.
func (c *Controller) StopFrom(via, id string) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	if !c.recordingLocked(id) {
		return false
	}
	c.stopFromLocked(via)
	return true
}

// recordingLocked reports whether a take records, and is id when id is set.
func (c *Controller) recordingLocked(id string) bool {
	return c.rec != nil && (id == "" || filepath.Base(c.rec.base) == id)
}

func (c *Controller) stopFromLocked(via string) {
	c.rec.via = via
	c.stopLocked("tap", "")
}

// published is what the controller shows to callers that must never wait
// for its mutex — the main thread, where the menu and the key tap run, and
// an exit that must happen whatever the controller is doing. updateLocked
// writes it.
type published struct {
	takes bool      // a take records or is on its way
	id    string    // the recording take; "" when none records
	recAt time.Time // when it started
}

func (c *Controller) published() published {
	if p := c.pub.Load(); p != nil {
		return *p
	}
	return published{}
}

// Recording reports the recording take's id and how long it has recorded,
// or ok false. It never waits for the controller's mutex.
func (c *Controller) Recording() (id string, d time.Duration, ok bool) {
	p := c.published()
	if p.id == "" {
		return "", 0, false
	}
	return p.id, time.Since(p.recAt), true
}

// Enter handles a Return the key tap took from the app: while a take records
// it stops that take to paste and send; otherwise it turns the newest take
// still on its way from a paste into a send. It reports false when no take
// can use it (no take on its way, delivery past the point of sending, or a
// cancel with no take stopped since), so the caller passes it on: a Return
// after a cancel is the app's, and never turns an earlier take into a send.
func (c *Controller) Enter() bool {
	ok, _ := c.EnterWhy()
	return ok
}

// EnterWhy is Enter, saying why no take used the Return: "after a cancel",
// "no take on its way" or "past delivery".
func (c *Controller) EnterWhy() (used bool, why string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.rec == nil && c.cancelled {
		return false, "after a cancel"
	}
	if c.rec != nil {
		c.rec.send = true
		c.stopLocked("enter", "")
		return true, ""
	}
	why = "no take on its way"
	for i := len(c.finished) - 1; i >= 0; i-- {
		t := c.finished[i]
		if t.sealed || t.cancelled {
			why = "past delivery"
			continue
		}
		if !t.send {
			t.send = true
			log.Printf("session: Enter while %s — %s will send", c.state, filepath.Base(t.base))
			c.note(t, store.Event{Ev: "send_on"})
			c.updateLocked()
		}
		return true, ""
	}
	return false, why
}

// Cancel ends the recording take without delivering it; its audio and text
// are still kept, and it ends cancelled once its stream has drained. It is
// in flight (Busy) until its text is saved, but not in State: no key is
// taken for it. Earlier takes carry on. It is the key's; CancelFrom names
// another source.
func (c *Controller) Cancel() { c.CancelFrom("", "") }

// CancelFrom is Cancel from via ("menu", "ctl"), which its stop line
// records; id, when set, is the take the caller was shown, as StopFrom's.
// It reports false when it cancelled nothing.
func (c *Controller) CancelFrom(via, id string) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	if !c.recordingLocked(id) {
		return false
	}
	r := c.rec
	c.rec = nil
	c.cancelled = true
	r.cancelled = true
	r.kind, r.via = "cancel", via
	r.stopLine.Store(&store.Event{Ev: "stop", Kind: "cancel", Via: via})
	r.stopAt = time.Now()
	c.prevStop = r.stopAt
	if r.auto != nil {
		r.auto.Stop()
	}
	c.finished = append(c.finished, r)
	go r.src.Stop()
	r.backup.stop()
	log.Printf("session: cancelled %s%s", filepath.Base(r.base), fromVia(via))
	c.updateLocked()
	return true
}

// fromVia is a log line's note of where a stop came from.
func fromVia(via string) string {
	if via == "" {
		return ""
	}
	return " from the " + via
}

// Drain waits until nothing is in flight (Busy is false: every take's final
// line is on disk) or max has passed, and returns what is still in flight
// then, each take as "<id> <state>". It never waits for the controller's
// mutex: idle is answered only with the mutex taken, since a take starting
// holds it before it is published; held at the end, the answer says so in
// place of the takes.
func (c *Controller) Drain(max time.Duration) []string {
	deadline := time.Now().Add(max)
	for {
		if !c.Busy() && c.mu.TryLock() {
			idle := !c.Busy()
			c.mu.Unlock()
			if idle {
				return nil
			}
		}
		if !time.Now().Before(deadline) {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	if !c.mu.TryLock() {
		return []string{"the controller is held: what is in flight is not known"}
	}
	defer c.mu.Unlock()
	var left []string
	if c.rec != nil {
		left = append(left, filepath.Base(c.rec.base)+" recording")
	}
	for _, t := range c.finished {
		st := "transcribing"
		switch {
		case t.cancelled:
			st = "cancelled, transcribing"
		case t.delivering:
			st = "delivering"
		}
		left = append(left, filepath.Base(t.base)+" "+st)
	}
	if n := c.inflight.Load(); n > 0 && len(left) == 0 {
		left = append(left, fmt.Sprintf("%d recovery, resend or delivery", n))
	}
	return left
}

// Busy reports whether anything is in flight: a take recording, stopped,
// transcribing (a cancelled one included), waiting for its turn or
// delivering; a recovery; a resend, from its call until its line is written;
// a re-transcription; any holder of the delivery lock. A restart waits for
// it to be false. It never waits for the controller's mutex.
func (c *Controller) Busy() bool {
	return c.published().takes || c.inflight.Load() > 0
}

// ErrDeliveryBusy is LockDelivery's error when another delivery held the
// lock for the whole wait.
var ErrDeliveryBusy = errors.New("another delivery is under way")

// LockDelivery takes the delivery lock, which every path that delivers holds
// from the focus change through the Enter: a take's own turn, a command, the
// hotkey, the page. It waits at most wait (0: until the lock is free) and
// then fails with ErrDeliveryBusy. unlock releases it; calling it twice is
// safe. While it is held the controller is Busy.
func (c *Controller) LockDelivery(wait time.Duration) (unlock func(), err error) {
	c.lockOnce.Do(func() { c.dlock = make(chan struct{}, 1) })
	var timeout <-chan time.Time
	if wait > 0 {
		t := time.NewTimer(wait)
		defer t.Stop()
		timeout = t.C
	}
	select {
	case c.dlock <- struct{}{}:
	case <-timeout:
		return func() {}, ErrDeliveryBusy
	}
	end := c.begin()
	var once sync.Once
	return func() {
		once.Do(func() {
			end()
			<-c.dlock
		})
	}, nil
}

// begin counts one piece of work in flight for Busy until the func it
// returns is called.
func (c *Controller) begin() func() {
	c.inflight.Add(1)
	return func() { c.inflight.Add(-1) }
}

// Record appends e to the record of the take at base. A line that cannot be
// written is logged and alerted; the take carries on, its state kept in
// memory. It returns the append's error.
func (c *Controller) Record(base string, e store.Event) error {
	becomes := c.becomesUndelivered(base, e)
	err := c.mark(base, e)
	if becomes {
		c.Sound()
	}
	return err
}

// mark is Record without the sound: Interrupt's process is exiting, and
// Recover sounds once for every take it marks.
func (c *Controller) mark(base string, e store.Event) error {
	err := store.Append(base, e)
	if err != nil {
		log.Printf("session: %s record: %s: %v", filepath.Base(base), e.Ev, err)
		c.UI.Alert(MsgRecordFailed)
	}
	return err
}

// StoreTarget is t as a record's target: what a Target's Record method
// gives; nil for a target without one.
func StoreTarget(t Target) *store.Target {
	rt, ok := t.(interface{ Record() store.Target })
	if !ok {
		return nil
	}
	v := rt.Record()
	return &v
}

// The overlay's words for a take's outcomes.
const (
	MsgRecordFailed = "行为记录写不进去"
	MsgEmpty        = "无语音"
	MsgCancelled    = "已取消"
	MsgASRFailed    = "识别失败 · 已存入历史"
	MsgIncomplete   = "识别不完整 · 已存入历史"
	MsgUndelivered  = "未送达 · 文字在剪贴板" // the hotkey's hint follows it
	// MsgResendFailed: a resend of a take that has reached a target failed;
	// the take keeps its state, so the hotkey does not take it.
	MsgResendFailed = "送达失败 · 文字在剪贴板"
	MsgNotSent      = "已粘贴，未发送 · "
	MsgSaveFailed   = "录音写不进硬盘"
	MsgStreamCut    = "录音中断"
)

// cancelledMsg is the flash of a cancelled take d long.
func (c *Controller) cancelledMsg(d time.Duration) string {
	return fmt.Sprintf("%s %s · %s", MsgCancelled, Clock(d), c.hint())
}

// deliverFailedMsg is the alert of a take left undelivered.
func (c *Controller) deliverFailedMsg() string { return MsgUndelivered + " · " + c.hint() }

// Clock writes d as m:ss.
func Clock(d time.Duration) string {
	s := int(d.Round(time.Second) / time.Second)
	return fmt.Sprintf("%d:%02d", s/60, s%60)
}

// pending is a target being captured on its own goroutine.
type pending struct {
	done chan struct{}
	t    Target
}

// capture starts Deliver.Capture on its own goroutine, so the focus it reads
// is the one at the tap however long it takes to answer.
func (c *Controller) capture() *pending {
	p := &pending{done: make(chan struct{})}
	go func() {
		p.t = c.Deliver.Capture()
		close(p.done)
	}()
	return p
}

// get waits for the captured target. Never call it under c.mu.
func (p *pending) get() Target {
	<-p.done
	return p.t
}

// releaseLater releases a target the take will not use once it is captured.
func (c *Controller) releaseLater(p *pending) {
	c.wg.Add(1)
	go func() {
		defer c.wg.Done()
		release(p.get())
	}()
}

// startLocked locks the target — what has focus as the tap lands — while it
// starts the stream and opens the take's WAV; the start line is written once
// the target is known (logStart). tapAt is the key event's time.
func (c *Controller) startLocked(trigger string, tapAt time.Time) {
	if msg := c.closing.Load(); msg != nil {
		log.Printf("session: start refused: %s", *msg)
		c.UI.Flash(*msg)
		return
	}
	target := c.capture()
	src := c.NewSource()
	ch, err := src.Start(context.Background())
	if err != nil {
		c.releaseLater(target)
		log.Printf("session: start: %v", err)
		c.UI.Flash(err.Error())
		return
	}
	file, err := c.Store.Begin(c.now())
	if err != nil {
		c.releaseLater(target)
		go func() {
			src.Stop()
			for range ch {
			}
		}()
		log.Printf("session: start: %v", err)
		c.UI.Alert(MsgSaveFailed)
		return
	}
	bt, device := false, ""
	if d, ok := src.(audio.Described); ok {
		in := d.Input()
		bt, device = in.Bluetooth(), in.DeviceName()
		if err := c.Store.SaveInput(file.Base(), in); err != nil {
			log.Printf("session: %s.input.json: %v", file.Base(), err)
		}
	}
	r := &take{
		target:  target,
		tapAt:   tapAt,
		logged:  make(chan struct{}),
		src:     src,
		levels:  audio.NewLevels(200, audio.FloorBlocks),
		file:    file,
		chunker: c.newChunker(),
		base:    file.Base(),
		dir:     filepath.Join(c.chunkDir(), filepath.Base(file.Base())),
		turn:    c.last,
		next:    make(chan struct{}),
		warming: bt,
		device:  device,
		recAt:   time.Now(),
	}
	if c.Stream != nil {
		r.stream = c.Stream.Open()
	}
	if c.Tee != nil {
		r.tee = c.Tee(r.base)
	}
	c.seq++
	r.seq = c.seq
	if r.turn == nil {
		r.turn = make(chan struct{})
		close(r.turn)
	}
	c.last = r.next
	c.rec = r
	if c.NewBackup != nil {
		r.backup = c.startBackup(r, src)
	}
	start := store.Event{At: time.Now(), Ev: "start", Trigger: trigger, TapAt: tapAt, Engine: c.Engine, Model: c.Model, Build: c.Build}
	if c.prev != "" {
		start.Prev = &store.Prev{ID: filepath.Base(c.prev), GapS: seconds(time.Since(c.prevStop))}
	}
	c.prev = r.base
	r.startEv = start
	c.omu.Lock()
	if c.open == nil {
		c.open = map[*take]bool{}
	}
	c.open[r] = true
	c.omu.Unlock()
	if c.MaxDuration > 0 {
		r.auto = time.AfterFunc(c.MaxDuration, func() {
			c.mu.Lock()
			defer c.mu.Unlock()
			if c.rec == r {
				log.Printf("session: auto-stop after %v", c.MaxDuration)
				c.stopLocked("auto", "")
			}
		})
	}
	c.wg.Add(1)
	go c.run(r, ch)
	from := ""
	if d, ok := src.(interface{ Device() string }); ok && d.Device() != "" {
		from = " from " + d.Device()
	}
	log.Printf("session: recording %s%s", filepath.Base(r.base), from)
	go c.logStart(r)
	c.updateLocked()
	if n, ok := src.(interface{ Note() string }); ok && n.Note() != "" {
		log.Printf("session: %s", n.Note())
		c.UI.Flash(n.Note())
	}
}

// logStart waits for the take's target, then writes its start line and the
// lines noted before it, unless Interrupt has written them.
func (c *Controller) logStart(r *take) {
	t := r.target.get()
	r.jmu.Lock()
	if !r.started {
		c.writeStartLocked(r, StoreTarget(t))
	}
	r.jmu.Unlock()
	close(r.logged)
	log.Printf("session: %s: target locked: %s, %d ms after the tap", filepath.Base(r.base), t, time.Since(r.tapAt).Milliseconds())
}

// writeStartLocked writes r's start line with target and the lines noted
// before it; r.jmu is held.
func (c *Controller) writeStartLocked(r *take, target *store.Target) {
	start := r.startEv
	start.Target = target
	c.Record(r.base, start)
	for _, e := range r.early {
		c.Record(r.base, e)
	}
	r.early, r.started = nil, true
}

// note appends e to r's record, or, before the start line is written, keeps
// it to follow that line. It never waits for the target.
func (c *Controller) note(r *take, e store.Event) {
	r.jmu.Lock()
	defer r.jmu.Unlock()
	if !r.started {
		if e.At.IsZero() {
			e.At = time.Now()
		}
		r.early = append(r.early, e)
		return
	}
	c.Record(r.base, e)
}

// NoSignal is the overlay's notice while the recording take's input
// delivers digital silence (View.NoSignal): after audio.DeadAfter of it —
// audio.BluetoothWarmup at the start of a take on a Bluetooth input, which
// opens with silence — and for as long as it lasts. A denied Microphone
// grant delivers zeros, not an error, so this is the only sign of it. The
// take carries on; the backup track, when there is one, keeps the speech.
const NoSignal = "无信号 — 麦克风只送来静音 · 录音继续"

// stopLocked moves the recording take to the finished queue. kind is what
// stopped it, as its stop line says (tap, enter, auto, stream_end,
// write_error); why is non-empty when the stream ended on its own.
func (c *Controller) stopLocked(kind, why string) {
	r := c.rec
	c.rec = nil
	c.cancelled = false
	r.kind, r.why = kind, why
	r.stopLine.Store(&store.Event{Ev: "stop", Kind: kind, Why: why, Via: r.via})
	r.stopAt = time.Now()
	c.prevStop = r.stopAt
	if r.auto != nil {
		r.auto.Stop()
	}
	c.finished = append(c.finished, r)
	go r.src.Stop()
	r.backup.stop()
	mode := "paste"
	if r.send {
		mode = "paste + Enter"
	}
	log.Printf("session: stop %s (%s)%s", filepath.Base(r.base), mode, fromVia(r.via))
	c.updateLocked()
}

// run is a take's life after its start: record until the stream ends, write
// the stop line once the start line is on disk, then wait for its text and
// deliver it in turn. The target is released once the take's delivery is
// over.
func (c *Controller) run(r *take, ch <-chan []int16) {
	defer c.wg.Done()
	defer func() { release(r.target.get()) }()
	total := c.record(r, ch)
	if r.interrupted.Load() {
		return
	}
	c.mu.Lock()
	kind, why, via := r.kind, r.why, r.via
	c.mu.Unlock()
	<-r.logged
	c.Record(r.base, store.Event{Ev: "stop", Kind: kind, Why: why, Via: via, DurS: seconds(samplesDuration(total))})
	c.finish(r, total)
}

// seconds is d in seconds, to the millisecond.
func seconds(d time.Duration) float64 { return float64(d.Milliseconds()) / 1000 }

// record writes each block to the take's WAV, feeds the level meter and the
// chunker, and queues each chunk as it is cut; the rest is queued as the
// tail once the stream has ended; a stream that ends on its own stops the
// take then, before the backup is waited for. While the take is within
// WholeMax its cuts are held, and a take that ends there is queued whole.
// It returns the take's length in samples. RAM holds the chunk being
// recorded, or the take while it is within WholeMax. With a streaming
// primary each block goes to its stream instead, and nothing is queued.
func (c *Controller) record(r *take, ch <-chan []int16) int {
	var buf []int16      // audio not yet queued, from sample offset start
	var held []audio.Cut // cuts not yet queued
	var start, total int
	from := audio.EdgeStart // the next chunk's first edge
	var werr error
	var mon audio.Monitor
	var dead []span           // where the main track delivered nothing, for the backup
	noSignal := false         // View.NoSignal
	var hearing audio.Hearing // whether the main track was heard
	c.mu.Lock()
	warming := r.warming
	c.mu.Unlock()
	for s := range ch {
		if r.first.IsZero() && len(s) > 0 {
			r.first = time.Now()
		}
		r.levels.Add(s)
		if warming && slices.ContainsFunc(s, func(v int16) bool { return v != 0 }) {
			warming = false
			log.Printf("session: %s: mic live after %.2f s", filepath.Base(r.base), float64(total)/audio.Rate)
			c.note(r, store.Event{Ev: "mic_live", AfterS: seconds(samplesDuration(total))})
			if !noSignal && total > 0 { // the warm-up delivered nothing either
				dead = append(dead, span{from: 0, to: total})
			}
			c.mu.Lock()
			r.warming = false
			c.updateLocked()
			c.mu.Unlock()
		}
		deadAfter := audio.DeadAfter
		if warming {
			deadAfter = audio.BluetoothWarmup
		}
		mon.Add(s)
		hearing.Add(s)
		silent := mon.Silent()
		if on := silent >= deadAfter; on != noSignal {
			noSignal = on
			if on {
				judged := (total + len(s)) / audio.Block // blocks the monitor has judged
				dead = openSpan(dead, (judged-int(silent/audio.BlockDuration))*audio.Block)
				log.Printf("session: %s: %s", filepath.Base(r.base), NoSignal)
				c.note(r, store.Event{Ev: "no_signal", AfterS: seconds(samplesDuration(total + len(s)))})
			} else {
				dead[len(dead)-1].to = total
				log.Printf("session: %s: signal back at %.2f s", filepath.Base(r.base), float64(total)/audio.Rate)
			}
			c.mu.Lock()
			r.noSignal = on
			c.updateLocked()
			c.mu.Unlock()
		}
		if werr == nil {
			if werr = r.file.Write(s); werr == nil && total == 0 {
				log.Printf("session: %s: first block on disk %d ms after the tap", filepath.Base(r.base), time.Since(r.tapAt).Milliseconds())
			}
			if werr != nil && r.interrupted.Load() {
				break
			}
			if werr != nil {
				log.Printf("session: write %s.wav: %v", r.base, werr)
				c.mu.Lock()
				if c.rec == r {
					c.stopLocked("write_error", MsgSaveFailed)
				}
				c.mu.Unlock()
			}
		}
		if r.tee != nil {
			r.tee.Write(s)
		}
		total += len(s)
		if r.stream != nil {
			r.stream.Write(s)
			continue
		}
		buf = append(buf, s...)
		held = append(held, r.chunker.Push(s)...)
		if Whole(total, c.WholeMax) {
			continue
		}
		for _, cut := range held {
			k := min(max(cut.End-start, 0), len(buf))
			c.queue(r, buf[:k], start, false, from, cut.Kind)
			from = cut.Kind
			buf = append([]int16(nil), buf[k:]...)
			start += k
		}
		held = nil
	}
	if r.interrupted.Load() { // Interrupt closed the WAVs and writes the last lines; Recover judges the take
		r.backup.stop()
		return total
	}
	if err := r.file.Close(); err != nil {
		log.Printf("session: close %s.wav: %v", r.base, err)
	}
	c.mu.Lock()
	cut := c.rec == r // the stream ended on its own
	if cut {
		why := MsgStreamCut
		if err := r.src.Err(); err != nil {
			why = err.Error()
		}
		log.Printf("session: %s", why)
		c.stopLocked("stream_end", why)
	}
	c.mu.Unlock()
	r.backup.stop()
	if c.takeBackup(r, total, endSpans(dead, total, hearing.Heard(), cut)) {
		return total
	}
	switch {
	case audio.TooShort(total), r.stream != nil:
	case Whole(total, c.WholeMax):
		lo, hi := audio.TrimBounds(buf)
		c.queue(r, buf[lo:hi], lo, false, audio.EdgeWhole, audio.EdgeWhole)
	case len(buf) > 0:
		c.queue(r, buf, start, false, from, audio.EdgeTail)
	}
	return total
}

// Whole reports whether a take of n samples is decoded as one window: it is
// no longer than max, and max is above 0.
func Whole(n int, max time.Duration) bool {
	return max > 0 && time.Duration(n)*time.Second <= max*audio.Rate
}

// finish waits for the take's chunks, saves its text and, in turn, delivers
// it. Each way out ends with the take's final line on disk, and only then
// the overlay says so: a take too short to keep, with nothing from its
// backup, ends empty; a cancelled take
// ends cancelled as soon as its stream has drained, before its text; the
// others decide once their turn has come and the delivery lock is held.
func (c *Controller) finish(r *take, total int) {
	defer c.done(r)
	c.mu.Lock()
	why, cancelled, stopAt := r.why, r.cancelled, r.stopAt
	c.mu.Unlock()
	if r.tee != nil {
		wav := r.base + ".wav"
		if audio.TooShort(total) {
			wav = ""
		}
		r.tee.End(stopAt, wav, total)
	}
	if audio.TooShort(total) && !r.fromBackup {
		c.stitch(r)
		if r.stream != nil {
			r.stream.Abort()
		}
		if err := c.Store.Remove(r.base); err != nil {
			log.Printf("session: remove %s.wav: %v", r.base, err)
		}
		os.RemoveAll(r.dir)
		log.Printf("session: %d samples, too short", total)
		c.Record(r.base, store.Event{Ev: "hold", Why: "empty"})
		switch {
		case cancelled:
			c.UI.Flash(MsgCancelled)
		case total == 0 && why != "":
			c.UI.Flash(why)
		default:
			c.UI.Flash(MsgEmpty)
		}
		return
	}
	if cancelled {
		c.Record(r.base, store.Event{Ev: "hold", Why: "cancelled"})
		c.UI.Flash(c.cancelledMsg(samplesDuration(total)))
	}
	var streamErr error
	var raw string
	var failed int
	engine := c.Engine
	audioOf := "main"
	if r.fromBackup {
		audioOf = "backup"
		if r.stream != nil { // it heard the main track's silence
			r.stream.Abort()
			if c.ChunkEngine != "" {
				engine = c.ChunkEngine
			}
		}
		raw, failed = c.stitch(r)
	} else if r.stream != nil {
		raw, streamErr = r.stream.Finish(c.ctx())
		if streamErr != nil {
			log.Printf("session: %s: %v; transcribing it locally", filepath.Base(r.base), streamErr)
			c.queueTake(r, false)
		}
	}
	if !r.fromBackup && (r.stream == nil || streamErr != nil) {
		raw, failed = c.stitch(r)
		if r.stream != nil && c.ChunkEngine != "" {
			engine = c.ChunkEngine
		}
	}
	if c.OnChunks != nil {
		c.OnChunks(r.base, c.pieces(r))
	}
	text := c.Post.Apply(raw)
	if failed == 0 {
		if err := c.Store.SaveText(r.base, raw, text); err != nil {
			log.Printf("session: keep text: %v", err)
		}
	}
	os.RemoveAll(r.dir)
	c.mu.Lock()
	after := time.Since(r.stopAt)
	c.mu.Unlock()
	te := store.Event{Ev: "text", Engine: engine, RawChars: chars(raw), Chars: chars(text), FailedChunks: failed,
		LatencyMS: after.Milliseconds(), SHA256: textSum(text), Audio: audioOf}
	if streamErr != nil {
		te.StreamErr = streamErr.Error()
	}
	c.Record(r.base, te)
	if r.tee != nil {
		r.tee.Done(TakeResult{Raw: raw, Text: text, Failed: failed, Latency: after, StreamErr: streamErr})
	}
	switch {
	case c.OnChunks != nil: // a meeting: its text is not for the log
		log.Printf("session: %s %.2f s, %d chunks → %d characters %.2f s after stop",
			filepath.Base(r.base), float64(total)/audio.Rate, len(r.chunks), len([]rune(text)), after.Seconds())
	default:
		log.Printf("session: %s %.2f s, %d chunks → text %.2f s after stop: %q",
			filepath.Base(r.base), float64(total)/audio.Rate, len(r.chunks), after.Seconds(), text)
		if text != raw {
			log.Printf("session: corrected from %q", raw)
		}
	}
	if failed > 0 {
		log.Printf("session: %d of %d chunks failed; megavoice retranscribe %s is the retry", failed, len(r.chunks), filepath.Base(r.base))
	}
	<-r.turn
	unlock, _ := c.LockDelivery(0)
	defer unlock()
	c.mu.Lock()
	cancelled = r.cancelled
	c.mu.Unlock()
	switch {
	case cancelled:
		return
	case text == "" && failed > 0:
		c.Record(r.base, store.Event{Ev: "hold", Why: "asr_failed"})
		c.UI.Alert(MsgASRFailed)
		return
	case text == "":
		c.Record(r.base, store.Event{Ev: "hold", Why: "empty"})
		c.UI.Flash(MsgEmpty)
		return
	}
	c.mu.Lock()
	r.delivering = true
	c.updateLocked()
	c.mu.Unlock()
	target := r.target.get()
	de := store.Event{Ev: "deliver", N: 1, Via: "auto", Target: StoreTarget(target), Chars: chars(text), Submit: "none", Source: "delivered"}
	if failed > 0 { // no .txt: the record keeps the partial text
		de.Text = text
	}
	if !r.paste.CompareAndSwap(0, pasteBegun) { // Interrupt marked it undelivered: that stays true
		return
	}
	if err := c.Deliver.Deliver(target, text); err != nil {
		r.paste.Store(0) // nothing landed
		c.seal(r)
		log.Printf("session: deliver to %s: %v; text left on the clipboard", target, err)
		de.Err = err.Error()
		c.Record(r.base, de)
		c.Record(r.base, store.Event{Ev: "hold", Why: "deliver_failed"})
		c.UI.Alert(c.deliverFailedMsg())
		return
	}
	de.OK = true
	log.Printf("session: delivered to %s", target)
	var serr error
	if c.seal(r) {
		de.Submit = "ok"
		if serr = c.Deliver.Submit(target); serr != nil {
			log.Printf("session: Enter to %s: %v", target, serr)
			de.Submit, de.Err = "err", serr.Error()
		} else {
			log.Printf("session: sent (Enter) in %s", target)
		}
	}
	c.Record(r.base, de)
	c.readBack(r.base, de.N, target, text)
	switch {
	case serr != nil:
		c.UI.Alert(MsgNotSent + serr.Error())
	case failed > 0:
		c.UI.Alert(MsgIncomplete)
	case why != "":
		c.UI.Flash(why)
	}
}

// readBack has the Deliverer read the target back after the delivery the
// deliver line n records, when it can, on its own goroutine: the delivery
// lock and the take's turn are not held for it. Its verdict, whole, short or
// unknown and why, is appended as a received line after the deliver line. It
// is recorded only; nothing acts on it. Until the line is on disk the
// controller is Busy.
func (c *Controller) readBack(base string, n int, t Target, text string) {
	rb, ok := c.Deliver.(ReadBacker)
	if !ok {
		return
	}
	c.wg.Add(1)
	end := c.begin()
	go func() {
		defer c.wg.Done()
		defer end()
		if v, why := rb.Received(t, text); v != "" {
			c.Record(base, store.Event{Ev: "received", N: n, Received: v, Why: why})
		}
	}()
}

// chars counts s's characters, as the record does.
func chars(s string) int { return len([]rune(s)) }

// textSum is the hex SHA-256 of a take's text, as the text line carries it.
func textSum(s string) string {
	h := sha256.Sum256([]byte(s))
	return hex.EncodeToString(h[:])
}

// done passes the delivery turn to the next take once this take has had
// its own, and drops it from the finished queue.
func (c *Controller) done(r *take) {
	<-r.turn
	close(r.next)
	c.mu.Lock()
	defer c.mu.Unlock()
	c.finished = slices.DeleteFunc(c.finished, func(t *take) bool { return t == r })
	c.updateLocked()
	c.omu.Lock()
	delete(c.open, r)
	c.omu.Unlock()
}

// seal ends the window in which Enter can turn a paste into a send and
// reports whether to send.
func (c *Controller) seal(r *take) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	r.sealed = true
	return r.send
}

// queue writes a chunk's WAV and hands it to the ASR worker; bg chunks
// (recovered takes) wait while live ones are queued. offset is where s
// starts in the take; from and to are its edges' kinds, and a chunk that
// ends at audio.EdgeWhole is a whole take, decoded as one window. A chunk of
// digital silence (audio.Dead) is not decoded — a model invents a sentence
// for it — and its text is empty. Any other chunk is decoded: a quiet or
// gated voice comes in runs shorter than audio.HeardAfter.
func (c *Controller) queue(t *take, s []int16, offset int, bg bool, from, to string) {
	ck := &chunk{t: t, n: len(t.chunks) + 1, offset: offset, samples: len(s), from: from, to: to, whole: to == audio.EdgeWhole, done: make(chan struct{})}
	ck.wav = filepath.Join(t.dir, fmt.Sprintf("%03d.wav", ck.n))
	t.chunks = append(t.chunks, ck)
	if audio.Dead(s) {
		log.Printf("session: %s chunk %d: %.2f s of digital silence, not decoded", filepath.Base(t.base), ck.n, samplesDuration(len(s)).Seconds())
		close(ck.done)
		return
	}
	err := os.MkdirAll(t.dir, 0o755)
	if err == nil {
		err = audio.SaveWAV(ck.wav, s)
	}
	if err != nil {
		log.Printf("session: chunk %s: %v", ck.wav, err)
		ck.err = err
		close(ck.done)
		return
	}
	t.pending.Add(int64(len(s)))
	c.workerOnce.Do(func() {
		c.qmu.Lock()
		c.wake = make(chan struct{}, 1)
		c.qmu.Unlock()
		go c.work()
	})
	c.qmu.Lock()
	if bg {
		c.bg = append(c.bg, ck)
	} else {
		c.live = append(c.live, ck)
	}
	c.qmu.Unlock()
	c.poke()
}

// poke wakes the ASR worker, when it runs, to look at the queue again.
func (c *Controller) poke() {
	c.qmu.Lock()
	w := c.wake
	c.qmu.Unlock()
	if w != nil {
		select {
		case w <- struct{}{}:
		default:
		}
	}
}

// yieldBackground cancels the background chunk the worker decodes, when it
// decodes one: a take has started, and its text does not wait behind a
// re-transcription or a recovery. The chunk goes back to the head of the
// background queue and is decoded again once no take is on its way.
func (c *Controller) yieldBackground() {
	c.qmu.Lock()
	defer c.qmu.Unlock()
	if c.yield != nil && !c.yielded {
		c.yielded = true
		c.yield()
	}
}

// drop fails t's chunks still queued with err; the one being decoded, if
// any, is cancelled through t's context.
func (c *Controller) drop(t *take, err error) {
	c.qmu.Lock()
	var out []*chunk
	c.bg = slices.DeleteFunc(c.bg, func(ck *chunk) bool {
		if ck.t == t {
			out = append(out, ck)
			return true
		}
		return false
	})
	c.qmu.Unlock()
	for _, ck := range out {
		c.settle(ck, "", err)
	}
}

// work is the one ASR worker: live chunks in take order — a take's tail is
// queued only once its stream has drained, possibly after the next take's
// first chunk — then background ones (recovered takes, re-transcriptions),
// only while no take is on its way. A take that starts while a background
// chunk decodes cancels that decode (yieldBackground); the chunk is decoded
// again later.
func (c *Controller) work() {
	for {
		c.qmu.Lock()
		var ck *chunk
		bg := false
		switch {
		case len(c.live) > 0:
			i := 0
			for j, x := range c.live {
				if x.t.seq < c.live[i].t.seq {
					i = j
				}
			}
			ck = c.live[i]
			c.live = slices.Delete(c.live, i, i+1)
		case len(c.bg) > 0 && !c.published().takes:
			ck, c.bg, bg = c.bg[0], c.bg[1:], true
		}
		if ck == nil {
			c.qmu.Unlock()
			<-c.wake
			continue
		}
		ctx, cancel := c.chunkCtx(ck)
		if bg {
			c.yield, c.yielded = cancel, false
		}
		c.qmu.Unlock()
		text, err := c.decode(ctx, ck)
		cancel()
		c.qmu.Lock()
		again := bg && c.yielded && err != nil && c.ctx().Err() == nil && (ck.t.ctx == nil || ck.t.ctx.Err() == nil)
		if bg {
			c.yield, c.yielded = nil, false
		}
		if again {
			c.bg = slices.Insert(c.bg, 0, ck)
		}
		c.qmu.Unlock()
		if again {
			log.Printf("session: %s chunk %d yields to a live take; it is decoded again after it", filepath.Base(ck.t.base), ck.n)
			continue
		}
		c.settle(ck, text, err)
	}
}

// chunkCtx is the context a chunk is decoded under: the controller's, and a
// re-transcription's own; cancel ends it.
func (c *Controller) chunkCtx(ck *chunk) (context.Context, context.CancelFunc) {
	ctx, cancel := context.WithCancel(c.ctx())
	if ck.t.ctx == nil {
		return ctx, cancel
	}
	stop := context.AfterFunc(ck.t.ctx, cancel)
	return ctx, func() { stop(); cancel() }
}

func (c *Controller) ctx() context.Context {
	if c.Ctx == nil {
		return context.Background()
	}
	return c.Ctx
}

// decode has the ASR transcribe one chunk under ctx.
func (c *Controller) decode(ctx context.Context, ck *chunk) (string, error) {
	if err := ctx.Err(); err != nil { // cancelled: no call, no log line per chunk
		return "", err
	}
	t0 := time.Now()
	text, err := c.ASR.Transcribe(ctx, ck.wav, ck.whole)
	wall := time.Since(t0)
	secs := time.Duration(ck.samples) * time.Second / audio.Rate
	switch {
	case err != nil && ctx.Err() != nil: // cancelled mid-call: said by the caller
	case err != nil:
		log.Printf("session: asr %s chunk %d: %v", filepath.Base(ck.t.base), ck.n, err)
	default:
		c.est.add(wall, secs)
		what := fmt.Sprintf("chunk %d", ck.n)
		if ck.whole {
			what = "whole take, trimmed"
		}
		log.Printf("session: %s %s: %.2f s in %.2f s", filepath.Base(ck.t.base), what, secs.Seconds(), wall.Seconds())
	}
	return text, err
}

// settle gives a chunk its text or error; stitch then reads it.
func (c *Controller) settle(ck *chunk, text string, err error) {
	ck.text, ck.err = text, err
	ck.t.pending.Add(-int64(ck.samples))
	close(ck.done)
}

// stitch waits for every chunk of t and joins their texts in order; failed
// counts the chunks that have no text.
func (c *Controller) stitch(t *take) (raw string, failed int) {
	var parts []string
	for _, ck := range t.chunks {
		<-ck.done
		if ck.err != nil {
			failed++
			continue
		}
		parts = append(parts, ck.text)
	}
	return Join(parts), failed
}

// pieces is t's chunks as a finished take reports them; stitch has waited
// for each.
func (c *Controller) pieces(t *take) []Chunk {
	out := make([]Chunk, len(t.chunks))
	for i, ck := range t.chunks {
		out[i] = Chunk{Offset: samplesDuration(ck.offset), Length: samplesDuration(ck.samples), Start: ck.offset, End: ck.offset + ck.samples,
			From: ck.from, To: ck.to, Raw: ck.text, Err: ck.err}
		if ck.err == nil {
			out[i].Text = c.Post.Apply(ck.text)
		}
	}
	return out
}

func samplesDuration(n int) time.Duration { return time.Duration(n) * time.Second / audio.Rate }

// Wait blocks until every take and recovery in flight is finished.
func (c *Controller) Wait() { c.wg.Wait() }

// cut queues a kept take's samples as recording would have: whole and
// trimmed within WholeMax, else in chunks at the chunker's cuts.
func (c *Controller) cut(t *take, s []int16, bg bool) {
	if Whole(len(s), c.WholeMax) {
		lo, hi := audio.TrimBounds(s)
		c.queue(t, s[lo:hi], lo, bg, audio.EdgeWhole, audio.EdgeWhole)
		return
	}
	start, from := 0, audio.EdgeStart
	for _, cut := range t.chunker.Push(s) {
		end := min(max(cut.End, start), len(s))
		c.queue(t, s[start:end], start, bg, from, cut.Kind)
		start, from = end, cut.Kind
	}
	if start < len(s) {
		c.queue(t, s[start:], start, bg, from, audio.EdgeTail)
	}
}

// queueTake queues a finished take from its WAV; a WAV that cannot be read
// is one failed chunk.
func (c *Controller) queueTake(t *take, bg bool) {
	s, err := audio.ReadWAV(t.base + ".wav")
	if err != nil {
		log.Printf("session: %s: %v", filepath.Base(t.base), err)
		ck := &chunk{t: t, n: len(t.chunks) + 1, err: err, done: make(chan struct{})}
		close(ck.done)
		t.chunks = append(t.chunks, ck)
		return
	}
	c.cut(t, s, bg)
}

// updateLocked derives the state and the view from the takes and reports
// what changed. A cancelled take is in neither: the key tap takes no key
// for it and the overlay shows no wait for it.
func (c *Controller) updateLocked() {
	p := &published{takes: c.rec != nil || len(c.finished) > 0}
	if c.rec != nil {
		p.id, p.recAt = filepath.Base(c.rec.base), c.rec.recAt
	}
	c.pub.Store(p)
	if p.takes {
		c.yieldBackground()
	} else {
		c.poke()
	}
	live := slices.DeleteFunc(slices.Clone(c.finished), func(t *take) bool { return t.cancelled })
	st := Idle
	switch {
	case c.rec != nil:
		st = Recording
	case len(live) > 0:
		st = Transcribing
		for _, t := range live {
			if t.delivering {
				st = Delivering
			}
		}
	}
	if st != c.state {
		c.state = st
		if c.OnState != nil {
			c.OnState(st)
		}
	}
	var v View
	if c.rec != nil {
		v.Quality = c.rec.levels.Quality
		v.Warming = c.rec.warming
		v.NoSignal = c.rec.noSignal
		v.Device = c.rec.device
	}
	if n := len(live); n > 0 {
		t := live[n-1]
		v.Started, v.Send = t.stopAt, t.send
		takes := slices.Clone(c.finished) // every one of them is ahead in the ASR queue
		v.Estimate = func() time.Duration {
			var p int64
			for _, t := range takes {
				p += t.pending.Load()
			}
			return c.est.left(p)
		}
	}
	if k := (viewKey{c.rec, v.Warming, v.NoSignal, v.Started, v.Send}); k != c.shown {
		c.shown = k
		c.UI.Show(v)
	}
}

// viewKey is what makes one View differ from another.
type viewKey struct {
	rec      *take
	warming  bool
	noSignal bool
	started  time.Time
	send     bool
}

func (c *Controller) newChunker() Chunker {
	if c.NewChunker != nil {
		return c.NewChunker()
	}
	return &audio.Chunker{}
}

func (c *Controller) chunkDir() string {
	if c.ChunkDir != "" {
		return c.ChunkDir
	}
	return filepath.Join(os.TempDir(), "megavoice-chunks")
}

func (c *Controller) now() time.Time {
	if c.Now != nil {
		return c.Now()
	}
	return time.Now()
}

// release frees a target's retained references (an app target holds its
// window and element); safe on any target, nil included.
func release(t Target) {
	if rl, ok := t.(interface{ Release() }); ok {
		rl.Release()
	}
}
