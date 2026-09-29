package session

import (
	"context"
	"errors"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/caoer/mega-asr/internal/asr"
	"github.com/caoer/mega-asr/internal/audio"
	"github.com/caoer/mega-asr/internal/post"
	"github.com/caoer/mega-asr/internal/store"
)

// fakeSource streams n samples of room noise when started (blocks instead,
// when set); with cut it then ends on its own, Err saying cutErr ("stream
// cut" when nil). note and device answer the optional Note and Device. With
// stopGate set, Stop closes the stream only once the gate is closed, as a
// microphone streams its tail after the stop.
type fakeSource struct {
	n        int
	blocks   [][]int16
	cut      bool
	cutErr   error
	startErr error
	note     string
	device   string
	stopGate chan struct{}
	ch       chan []int16
	stopOnce sync.Once
}

func (f *fakeSource) clone() *fakeSource {
	return &fakeSource{n: f.n, blocks: f.blocks, cut: f.cut, cutErr: f.cutErr, startErr: f.startErr, note: f.note, device: f.device, stopGate: f.stopGate}
}

func (f *fakeSource) Note() string   { return f.note }
func (f *fakeSource) Device() string { return f.device }

func (f *fakeSource) Start(context.Context) (<-chan []int16, error) {
	if f.startErr != nil {
		return nil, f.startErr
	}
	blocks := f.blocks
	if blocks == nil {
		blocks = [][]int16{noise(f.n)}
	}
	f.ch = make(chan []int16, len(blocks))
	for _, b := range blocks {
		f.ch <- b
	}
	if f.cut {
		close(f.ch)
	}
	return f.ch, nil
}

func (f *fakeSource) Stop() {
	if f.stopGate != nil {
		<-f.stopGate
	}
	if !f.cut {
		f.stopOnce.Do(func() { close(f.ch) })
	}
}

func (f *fakeSource) Err() error {
	switch {
	case f.cut && f.cutErr != nil:
		return f.cutErr
	case f.cut:
		return errors.New("stream cut")
	}
	return nil
}

// fakeASR answers the texts in order (the last one repeats), or, when byTake
// is set, the text of the chunk's take; calls listed in fail return an error.
type fakeASR struct {
	mu     sync.Mutex
	texts  []string
	byTake map[string]string // take base name → text
	// answer, when set, gives each chunk's text from its samples
	answer func(s []int16) string
	fail   map[int]bool
	calls  int
	wholes []bool        // each call's whole flag
	block  chan struct{} // when set, Transcribe waits for it
}

func (a *fakeASR) Transcribe(_ context.Context, wav string, whole bool) (string, error) {
	if a.block != nil {
		<-a.block
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	a.calls++
	a.wholes = append(a.wholes, whole)
	if _, err := os.Stat(wav); err != nil {
		return "", err
	}
	if a.fail[a.calls] {
		return "", errors.New("asr crashed")
	}
	if a.byTake != nil {
		return a.byTake[filepath.Base(filepath.Dir(wav))], nil
	}
	if a.answer != nil {
		s, err := audio.ReadWAV(wav)
		return a.answer(s), err
	}
	return a.texts[min(a.calls, len(a.texts))-1], nil
}

func (a *fakeASR) n() int { a.mu.Lock(); defer a.mu.Unlock(); return a.calls }

// target is a captured target; released records that its references were
// freed, as the real app target's window and element must be.
type target struct {
	name     string
	released bool
}

func (t *target) String() string { return t.name }
func (t *target) Release()       { t.released = true }

type fakeDeliverer struct {
	mu          sync.Mutex
	front       string // what Capture sees now
	captured    []*target
	starts      int           // takes started: each asks for one Capture
	delay       time.Duration // Capture returns this long after it reads the front
	delivered   []string
	deliverErr  error
	submitErr   error
	block       chan struct{} // when set, Deliver waits for it after entering
	entered     chan struct{} // closed when Deliver is entered
	calls       []string      // when trace is set: each call's start and end
	trace       bool
	received    string // what Received reads back, with why receivedWhy
	receivedWhy string
	readFor     time.Duration // how long a readback takes
	delivers    []time.Time   // when each Deliver was entered
}

func (d *fakeDeliverer) Received(t Target, _ string) (string, string) {
	time.Sleep(d.readFor)
	d.note(t.String() + ":received")
	return d.received, d.receivedWhy
}

func (d *fakeDeliverer) note(s string) {
	if d.trace {
		d.mu.Lock()
		d.calls = append(d.calls, s)
		d.mu.Unlock()
	}
}

// setFront moves the focus once every take started so far has read it:
// Capture runs on its own goroutine from the tap, and the focus a test moves
// is the one after the tap.
func (d *fakeDeliverer) setFront(f string) {
	for {
		d.mu.Lock()
		if len(d.captured) >= d.starts {
			d.front = f
			d.mu.Unlock()
			return
		}
		d.mu.Unlock()
		time.Sleep(time.Millisecond)
	}
}

func (d *fakeDeliverer) Capture() Target {
	d.mu.Lock()
	t := &target{name: d.front}
	d.captured = append(d.captured, t)
	delay := d.delay
	d.mu.Unlock()
	time.Sleep(delay)
	return t
}

func (d *fakeDeliverer) Deliver(t Target, text string) error {
	d.note(t.String() + ":deliver>")
	defer d.note(t.String() + ":deliver<")
	d.mu.Lock()
	d.delivers = append(d.delivers, time.Now())
	d.mu.Unlock()
	if d.entered != nil {
		close(d.entered)
		d.entered = nil
	}
	if d.block != nil {
		<-d.block
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.deliverErr != nil {
		return d.deliverErr
	}
	d.delivered = append(d.delivered, t.String()+"|"+text)
	return nil
}

func (d *fakeDeliverer) Submit(t Target) error {
	d.note(t.String() + ":submit")
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.submitErr != nil {
		return d.submitErr
	}
	d.delivered = append(d.delivered, t.String()+"|⏎")
	return nil
}

func (d *fakeDeliverer) got() string {
	d.mu.Lock()
	defer d.mu.Unlock()
	return strings.Join(d.delivered, ";")
}

// allReleased reports whether every captured target was released.
func (d *fakeDeliverer) allReleased() bool {
	d.mu.Lock()
	defer d.mu.Unlock()
	for _, t := range d.captured {
		if !t.released {
			return false
		}
	}
	return true
}

type fakeUI struct {
	mu     sync.Mutex
	events []string
	views  []View
	// probe, when set, runs at each Flash and Alert with its message; what
	// it returns is kept in probed.
	probe  func(msg string) string
	probed []string
}

func (u *fakeUI) add(s string) { u.mu.Lock(); u.events = append(u.events, s); u.mu.Unlock() }
func (u *fakeUI) Show(v View) {
	u.mu.Lock()
	u.views = append(u.views, v)
	u.mu.Unlock()
	switch {
	case v.Quality != nil && v.Started.IsZero():
		u.add("recording")
	case v.Quality != nil:
		u.add("recording+prev")
	case !v.Started.IsZero() && v.Send:
		u.add("transcribing+send")
	case !v.Started.IsZero():
		u.add("transcribing")
	default:
		u.add("hide")
	}
}
func (u *fakeUI) Flash(m string) { u.note(m); u.add("flash:" + m) }
func (u *fakeUI) Alert(m string) { u.note(m); u.add("alert:" + m) }
func (u *fakeUI) note(m string) {
	if u.probe != nil {
		p := u.probe(m)
		u.mu.Lock()
		u.probed = append(u.probed, p)
		u.mu.Unlock()
	}
}
func (u *fakeUI) String() string {
	u.mu.Lock()
	defer u.mu.Unlock()
	return strings.Join(u.events, ",")
}

// everyN is a chunker that cuts every n samples.
type everyN struct{ n, pushed int }

func (e *everyN) Push(s []int16) []audio.Cut {
	var cuts []audio.Cut
	for k := (e.pushed/e.n + 1) * e.n; k <= e.pushed+len(s); k += e.n {
		cuts = append(cuts, audio.Cut{End: k, Kind: audio.EdgePause, Reason: "every n"})
	}
	e.pushed += len(s)
	return cuts
}

type rig struct {
	c      *Controller
	src    *fakeSource // template: each take gets a copy
	asr    *fakeASR
	del    *fakeDeliverer
	ui     *fakeUI
	dir    string
	chunks string
}

func newRig(t *testing.T, src *fakeSource, texts ...string) *rig {
	if len(texts) == 0 {
		texts = []string{""}
	}
	r := &rig{src: src, asr: &fakeASR{texts: texts}, del: &fakeDeliverer{front: "telegram"}, ui: &fakeUI{},
		dir: t.TempDir(), chunks: t.TempDir()}
	r.c = &Controller{
		NewSource: func() audio.Source {
			r.del.mu.Lock()
			r.del.starts++
			r.del.mu.Unlock()
			return r.src.clone()
		},
		ASR:      r.asr,
		Post:     post.Chain{func(s string) string { return strings.ReplaceAll(s, "派森", "Python") }},
		Deliver:  r.del,
		UI:       r.ui,
		Store:    store.Store{Dir: r.dir},
		ChunkDir: r.chunks,
		Now:      func() time.Time { return time.Date(2020, 4, 17, 10, 42, 0, 0, time.Local) },
	}
	return r
}

// wait blocks until every take in flight is finished.
func (r *rig) wait(t *testing.T) {
	t.Helper()
	r.waitUpTo(t, 2*time.Second)
}

func (r *rig) waitUpTo(t *testing.T, d time.Duration) {
	t.Helper()
	done := make(chan struct{})
	go func() { r.c.wg.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(d):
		t.Fatalf("takes did not finish; state %v, ui %s", r.c.State(), r.ui)
	}
}

func files(t *testing.T, dir string) string {
	es, _ := os.ReadDir(dir)
	var out []string
	for _, e := range es {
		out = append(out, e.Name())
	}
	return strings.Join(out, ",")
}

func TestTapTapPastesIntoTargetAtStart(t *testing.T) {
	r := newRig(t, &fakeSource{n: audio.Rate}, "你好 hello 派森")
	r.del.setFront("herdr pane w1:p1")
	r.c.Toggle()
	r.del.setFront("telegram") // focus moved while speaking
	r.c.Toggle()
	r.del.setFront("safari") // and again during transcription
	r.wait(t)
	if got := r.del.got(); got != "herdr pane w1:p1|你好 hello Python" {
		t.Fatalf("delivered %q", got)
	}
	if len(r.del.captured) != 1 || !r.del.allReleased() {
		t.Fatalf("captured %d targets, all released %v", len(r.del.captured), r.del.allReleased())
	}
	if got := files(t, r.dir); got != "20200417-104200.events.jsonl,20200417-104200.raw.txt,20200417-104200.txt,20200417-104200.wav" {
		t.Fatalf("kept %s", got)
	}
	txt, _ := os.ReadFile(r.dir + "/20200417-104200.txt")
	raw, _ := os.ReadFile(r.dir + "/20200417-104200.raw.txt")
	if string(txt) != "你好 hello Python\n" || string(raw) != "你好 hello 派森\n" {
		t.Fatalf("txt %q, raw %q", txt, raw)
	}
	if s, err := audio.ReadWAV(r.dir + "/20200417-104200.wav"); err != nil || len(s) != audio.Rate {
		t.Fatalf("wav: %d samples, %v", len(s), err)
	}
	if r.c.State() != Idle || r.ui.String() != "recording,transcribing,hide" || files(t, r.chunks) != "" {
		t.Fatalf("state %v, ui %s, chunk dir %q", r.c.State(), r.ui, files(t, r.chunks))
	}
}

// described is a fakeSource that names its device.
type described struct {
	*fakeSource
	in audio.TakeInput
}

func (d described) Input() audio.TakeInput { return d.in }

// A take from a source that names its device keeps that device as
// <base>.input.json beside the WAV; the transcript is written as before.
func TestTakeRecordsItsInput(t *testing.T) {
	r := newRig(t, &fakeSource{n: audio.Rate}, "hi")
	in := audio.TakeInput{Source: "remote", Name: "micbox", Host: "198.51.100.74:7866"}
	r.c.NewSource = func() audio.Source { return described{r.src.clone(), in} }
	r.c.Toggle()
	r.c.Toggle()
	r.wait(t)
	if got := files(t, r.dir); got != "20200417-104200.events.jsonl,20200417-104200.input.json,20200417-104200.raw.txt,20200417-104200.txt,20200417-104200.wav" {
		t.Fatalf("kept %s", got)
	}
	if got, err := store.LoadInput(r.dir + "/20200417-104200"); err != nil || got != in {
		t.Fatalf("input record %+v, %v", got, err)
	}
}

// OnChunks gets a finished take's chunks in order, each with where it starts
// in the take, its length, its samples, its edges' kinds, and its text before
// and after the post chain; a chunk the ASR failed on carries the error.
func TestOnChunksReportsEachChunk(t *testing.T) {
	r := newRig(t, &fakeSource{n: 3*audio.Rate + audio.Rate/2}, "one", "two", "派森", "三")
	r.asr.fail = map[int]bool{2: true}
	r.c.NewChunker = func() Chunker { return &everyN{n: audio.Rate} }
	var got []Chunk
	var take string
	r.c.OnChunks = func(base string, cs []Chunk) { take, got = base, cs }
	r.c.Toggle()
	r.c.Toggle()
	r.wait(t)
	if filepath.Base(take) != "20200417-104200" || len(got) != 4 {
		t.Fatalf("take %q: %d chunks", take, len(got))
	}
	want := []Chunk{
		{Offset: 0, Length: time.Second, Start: 0, End: audio.Rate, From: audio.EdgeStart, To: audio.EdgePause, Raw: "one", Text: "one"},
		{Offset: time.Second, Length: time.Second, Start: audio.Rate, End: 2 * audio.Rate, From: audio.EdgePause, To: audio.EdgePause},
		{Offset: 2 * time.Second, Length: time.Second, Start: 2 * audio.Rate, End: 3 * audio.Rate, From: audio.EdgePause, To: audio.EdgePause, Raw: "派森", Text: "Python"},
		{Offset: 3 * time.Second, Length: time.Second / 2, Start: 3 * audio.Rate, End: 3*audio.Rate + audio.Rate/2, From: audio.EdgePause, To: audio.EdgeTail, Raw: "三", Text: "三"},
	}
	for i, w := range want {
		g := got[i]
		if g.Offset != w.Offset || g.Length != w.Length || g.Start != w.Start || g.End != w.End || g.From != w.From || g.To != w.To ||
			g.Raw != w.Raw || g.Text != w.Text || (g.Err != nil) != (i == 1) {
			t.Errorf("chunk %d = %+v, want %+v", i, g, w)
		}
	}
}

// A long take is cut into chunks while it records; their texts are joined in
// order and the post chain sees the whole text.
func TestChunksStitchedInOrder(t *testing.T) {
	r := newRig(t, &fakeSource{n: 3*audio.Rate + audio.Rate/2}, "one", "two", "派森", "三")
	r.c.NewChunker = func() Chunker { return &everyN{n: audio.Rate} }
	r.c.Toggle()
	r.c.Toggle()
	r.wait(t)
	if got := r.del.got(); got != "telegram|one twoPython三" || r.asr.n() != 4 {
		t.Fatalf("delivered %q after %d ASR calls", got, r.asr.n())
	}
	raw, _ := os.ReadFile(r.dir + "/20200417-104200.raw.txt")
	if string(raw) != "one two派森三\n" {
		t.Fatalf("raw %q", raw)
	}
	if s, _ := audio.ReadWAV(r.dir + "/20200417-104200.wav"); len(s) != 3*audio.Rate+audio.Rate/2 {
		t.Fatalf("wav has %d samples", len(s))
	}
}

// A take within WholeMax is decoded once, whole, at stop; a longer one is
// cut into chunks as before.
func TestWholeTakeUpToWholeMax(t *testing.T) {
	for _, c := range []struct {
		name    string
		samples int
		wholes  string
	}{
		{"at the limit", 2 * audio.Rate, "[true]"},
		{"past it", 2*audio.Rate + 1, "[false false false]"},
	} {
		t.Run(c.name, func(t *testing.T) {
			r := newRig(t, &fakeSource{n: c.samples}, "text")
			r.c.NewChunker = func() Chunker { return &everyN{n: audio.Rate} }
			r.c.WholeMax = 2 * time.Second
			r.c.Toggle()
			r.c.Toggle()
			r.wait(t)
			if got := fmt.Sprint(r.asr.wholes); got != c.wholes || !strings.HasPrefix(r.del.got(), "telegram|text") {
				t.Fatalf("ASR calls whole=%s, want %s; delivered %q", got, c.wholes, r.del.got())
			}
		})
	}
	if !Whole(audio.Rate, time.Second) || Whole(audio.Rate+1, time.Second) || Whole(1, 0) {
		t.Fatal("Whole: want n <= max, and never with max 0")
	}
}

// A tap while take 1 transcribes starts take 2 with its own target; both
// deliver, in order, each to its own target.
func TestTapWhileTranscribingStartsNextTake(t *testing.T) {
	r := newRig(t, &fakeSource{n: audio.Rate}, "first", "second")
	r.asr.byTake = map[string]string{"20200417-104200": "first", "20200417-104200-2": "second"}
	r.asr.block = make(chan struct{})
	r.del.setFront("A")
	r.c.Toggle()
	r.c.Toggle()
	r.del.setFront("B")
	r.c.Toggle() // take 2 starts while take 1 transcribes
	if r.c.State() != Recording {
		t.Fatalf("state %v", r.c.State())
	}
	r.del.setFront("C")
	r.c.Toggle()
	close(r.asr.block)
	r.wait(t)
	if got := r.del.got(); got != "A|first;B|second" {
		t.Fatalf("delivered %q", got)
	}
	if !strings.HasPrefix(r.ui.String(), "recording,transcribing,recording+prev,transcribing") || !r.del.allReleased() {
		t.Fatalf("ui %s, released %v", r.ui, r.del.allReleased())
	}
	if got := files(t, r.dir); !strings.Contains(got, "20200417-104200.txt") || !strings.Contains(got, "20200417-104200-2.txt") {
		t.Fatalf("kept %s", got)
	}
}

// Take 2 has its text first when take 1's delivery is slow: it waits.
func TestDeliveryInTakeOrder(t *testing.T) {
	r := newRig(t, &fakeSource{n: audio.Rate}, "first", "second")
	r.asr.byTake = map[string]string{"20200417-104200": "first", "20200417-104200-2": "second"}
	r.del.block, r.del.entered = make(chan struct{}), make(chan struct{})
	entered := r.del.entered
	r.del.setFront("A")
	r.c.Toggle()
	r.c.Toggle()
	<-entered // take 1 is inside Deliver
	r.del.setFront("B")
	r.c.Toggle()
	r.c.Toggle()
	for r.asr.n() < 2 {
		time.Sleep(time.Millisecond)
	}
	time.Sleep(20 * time.Millisecond)
	if got := r.del.got(); got != "" {
		t.Fatalf("delivered %q before take 1", got)
	}
	close(r.del.block)
	r.wait(t)
	if got := r.del.got(); got != "A|first;B|second" {
		t.Fatalf("delivered %q", got)
	}
}

func TestEnterWhileRecordingSendsThatTakeOnly(t *testing.T) {
	r := newRig(t, &fakeSource{n: audio.Rate}, "first", "second")
	r.asr.byTake = map[string]string{"20200417-104200": "first", "20200417-104200-2": "second"}
	r.asr.block = make(chan struct{})
	r.del.setFront("A")
	r.c.Toggle()
	r.c.Toggle() // the tap key's stop: paste only
	r.del.setFront("B")
	r.c.Toggle()
	if !r.c.Enter() {
		t.Fatal("Enter while recording was not used")
	}
	close(r.asr.block)
	r.wait(t)
	if got := r.del.got(); got != "A|first;B|second;B|⏎" {
		t.Fatalf("delivered %q", got)
	}
}

func TestEnterWhileRecordingPastesAndSends(t *testing.T) {
	r := newRig(t, &fakeSource{n: audio.Rate}, "ship it")
	var states []string
	r.c.OnState = func(s State) { states = append(states, s.String()) }
	r.c.Toggle()
	r.del.setFront("safari") // focus moved while speaking
	if !r.c.Enter() {
		t.Fatal("Enter while recording was not used")
	}
	r.wait(t)
	if got := r.del.got(); got != "telegram|ship it;telegram|⏎" {
		t.Fatalf("delivered %q", got)
	}
	if got := strings.Join(states, ","); got != "recording,transcribing,delivering,idle" {
		t.Fatalf("states %s", got)
	}
	if !strings.HasPrefix(r.ui.String(), "recording,transcribing+send") || !r.del.allReleased() {
		t.Fatalf("ui %s, released %v", r.ui, r.del.allReleased())
	}
}

func TestEnterWhileTranscribingUpgradesToSend(t *testing.T) {
	r := newRig(t, &fakeSource{n: audio.Rate}, "ship it")
	r.asr.block = make(chan struct{})
	r.c.Toggle()
	r.c.Toggle() // the tap key's stop: paste only
	if !r.c.Enter() {
		t.Fatal("Enter while transcribing was not used")
	}
	close(r.asr.block)
	r.wait(t)
	if got := r.del.got(); got != "telegram|ship it;telegram|⏎" {
		t.Fatalf("delivered %q", got)
	}
	if r.ui.String() != "recording,transcribing,transcribing+send,hide" {
		t.Fatalf("ui %s", r.ui)
	}
}

func TestEnterWhileDeliveringUpgradesToSend(t *testing.T) {
	r := newRig(t, &fakeSource{n: audio.Rate}, "ship it")
	r.del.block, r.del.entered = make(chan struct{}), make(chan struct{})
	entered := r.del.entered
	r.c.Toggle()
	r.c.Toggle()
	<-entered
	if r.c.State() != Delivering || !r.c.Enter() {
		t.Fatalf("state %v: Enter not used", r.c.State())
	}
	close(r.del.block)
	r.wait(t)
	if got := r.del.got(); got != "telegram|ship it;telegram|⏎" {
		t.Fatalf("delivered %q", got)
	}
}

func TestTapStopPastesOnly(t *testing.T) {
	r := newRig(t, &fakeSource{n: audio.Rate}, "draft")
	r.c.Toggle()
	r.c.Toggle()
	r.wait(t)
	if got := r.del.got(); got != "telegram|draft" {
		t.Fatalf("delivered %q", got)
	}
	if r.c.Enter() {
		t.Fatal("Enter while idle was used; it must pass on")
	}
}

func TestSendNotPressedWhenDeliveryFails(t *testing.T) {
	r := newRig(t, &fakeSource{n: audio.Rate}, "ship it")
	r.del.deliverErr = errors.New("pane gone")
	r.c.Toggle()
	r.c.Enter()
	r.wait(t)
	if got := r.del.got(); got != "" || !strings.Contains(r.ui.String(), "alert:未送达 · 文字在剪贴板 · 右Option+Esc 重贴") {
		t.Fatalf("delivered %q, ui %s", got, r.ui)
	}
}

// A chunk the ASR fails on is left out; the rest is delivered, the take
// flashed, and the WAV left without text for the next start.
func TestChunkFailureDeliversTheRest(t *testing.T) {
	r := newRig(t, &fakeSource{n: 3 * audio.Rate}, "one", "two", "three")
	r.asr.fail = map[int]bool{2: true}
	r.c.NewChunker = func() Chunker { return &everyN{n: audio.Rate} }
	r.c.Toggle()
	r.c.Toggle()
	r.wait(t)
	if got := r.del.got(); got != "telegram|one three" || !strings.Contains(r.ui.String(), "alert:"+MsgIncomplete) {
		t.Fatalf("delivered %q, ui %s", got, r.ui)
	}
	if got := files(t, r.dir); got != "20200417-104200.events.jsonl,20200417-104200.wav" {
		t.Fatalf("kept %s", got)
	}
}

// Esc cancels only the recording take: an earlier take still delivers, the
// cancelled one is kept with its text and never delivered.
func TestCancelOnlyTheRecordingTake(t *testing.T) {
	r := newRig(t, &fakeSource{n: audio.Rate}, "first", "second")
	r.asr.byTake = map[string]string{"20200417-104200": "first", "20200417-104200-2": "second"}
	r.asr.block = make(chan struct{})
	var states []string
	r.c.OnState = func(s State) { states = append(states, s.String()) }
	r.del.setFront("A")
	r.c.Toggle()
	r.c.Toggle()
	r.del.setFront("B")
	r.c.Toggle()
	r.c.Cancel()
	if r.c.State() != Transcribing {
		t.Fatalf("state %v", r.c.State())
	}
	close(r.asr.block)
	r.wait(t)
	if got := r.del.got(); got != "A|first" {
		t.Fatalf("delivered %q", got)
	}
	txt, _ := os.ReadFile(r.dir + "/20200417-104200-2.txt")
	if string(txt) != "second\n" || !r.del.allReleased() {
		t.Fatalf("cancelled take's text %q, released %v", txt, r.del.allReleased())
	}
	if got := strings.Join(states, ","); got != "recording,transcribing,recording,transcribing,delivering,idle" {
		t.Fatalf("states %s", got)
	}
}

func TestCancelKeepsButDoesNotDeliver(t *testing.T) {
	r := newRig(t, &fakeSource{n: audio.Rate}, "text")
	var states []string
	r.c.OnState = func(s State) { states = append(states, s.String()) }
	r.c.Toggle()
	r.c.Cancel()
	r.wait(t)
	if r.del.got() != "" || files(t, r.dir) != "20200417-104200.events.jsonl,20200417-104200.raw.txt,20200417-104200.txt,20200417-104200.wav" {
		t.Fatalf("delivered %q, files %s", r.del.got(), files(t, r.dir))
	}
	if got := strings.Join(states, ","); got != "recording,idle" || !r.del.allReleased() {
		t.Fatalf("states %s, released %v", got, r.del.allReleased())
	}
	if r.c.Enter() {
		t.Fatal("Enter after cancel was used; it must pass on")
	}
}

func TestShortClipDiscarded(t *testing.T) {
	r := newRig(t, &fakeSource{n: audio.MinSamples - 1}, "text")
	r.c.Toggle()
	r.c.Toggle()
	r.wait(t)
	if r.asr.n() != 0 || r.del.got() != "" || !strings.Contains(r.ui.String(), "flash:"+MsgEmpty) || files(t, r.dir) != "20200417-104200.events.jsonl" {
		t.Fatalf("asr %d, delivered %q, ui %s, files %s", r.asr.n(), r.del.got(), r.ui, files(t, r.dir))
	}
}

func TestStreamCutTranscribesWhatArrived(t *testing.T) {
	r := newRig(t, &fakeSource{n: audio.Rate, cut: true}, "partial")
	r.c.Toggle()
	r.wait(t) // the cut is noticed without a tap
	if r.del.got() != "telegram|partial" || !strings.Contains(r.ui.String(), "flash:stream cut") {
		t.Fatalf("delivered %q, ui %s", r.del.got(), r.ui)
	}
}

// A source that will not start leaves the controller idle and flashes the
// source's own error; the target captured for the take is released.
func TestStartFailureStaysIdle(t *testing.T) {
	r := newRig(t, &fakeSource{startErr: errors.New("Stage Interface refused to start")})
	r.c.Toggle()
	r.wait(t) // the target is released once it is captured
	if r.c.State() != Idle || r.ui.String() != "flash:Stage Interface refused to start" || !r.del.allReleased() || files(t, r.dir) != "" {
		t.Fatalf("state %v, ui %s, released %v, files %s", r.c.State(), r.ui, r.del.allReleased(), files(t, r.dir))
	}
}

// A stream that ends before its first sample flashes the source's reason.
func TestEmptyStreamFlashesWhy(t *testing.T) {
	r := newRig(t, &fakeSource{cut: true, cutErr: errors.New("array unavailable: ssh: Could not resolve hostname")})
	r.c.Toggle()
	r.wait(t)
	if !strings.Contains(r.ui.String(), "flash:array unavailable: ssh: Could not resolve hostname,") {
		t.Fatalf("ui %s", r.ui)
	}
}

// A stream that ends on its own mid-take: the take is decoded and delivered
// from what arrived, and the flash carries the source's error.
func TestMidTakeCutFlashesTheSourceError(t *testing.T) {
	r := newRig(t, &fakeSource{n: audio.Rate, cut: true, cutErr: errors.New("Boom Mic stopped delivering audio")}, "half a thought")
	r.c.Toggle()
	r.wait(t)
	if r.del.got() != "telegram|half a thought" || !strings.Contains(r.ui.String(), "flash:Boom Mic stopped delivering audio,") {
		t.Fatalf("delivered %q, ui %s", r.del.got(), r.ui)
	}
}

// The source's note is flashed once, when the take starts, and the take
// goes on as any other; a source with no note flashes nothing.
func TestSourceNoteFlashedAtStart(t *testing.T) {
	for _, note := range []string{"Podium Condenser not connected — using Boom Mic", ""} {
		r := newRig(t, &fakeSource{n: audio.Rate, note: note}, "flagged start")
		r.c.Toggle()
		r.c.Toggle()
		r.wait(t)
		flashes := strings.Count(r.ui.String(), "flash:")
		if r.del.got() != "telegram|flagged start" {
			t.Fatalf("note %q: delivered %q", note, r.del.got())
		}
		if note != "" && (flashes != 1 || !strings.Contains(r.ui.String(), "flash:"+note)) || note == "" && flashes != 0 {
			t.Fatalf("note %q: ui %s", note, r.ui)
		}
	}
}

// noSignals counts the views that turn the no-signal notice on.
func (u *fakeUI) noSignals() int {
	u.mu.Lock()
	defer u.mu.Unlock()
	n, on := 0, false
	for _, v := range u.views {
		if v.NoSignal && !on {
			n++
		}
		on = v.NoSignal
	}
	return n
}

// noticeCleared reports whether, in the order shown, a recording view
// without the no-signal notice followed one with it.
func (u *fakeUI) noticeCleared() bool {
	u.mu.Lock()
	defer u.mu.Unlock()
	on := false
	for _, v := range u.views {
		switch {
		case v.NoSignal:
			on = true
		case on && v.Quality != nil:
			return true
		}
	}
	return false
}

// tone is n samples of a live signal whose peak is about amp·16.
func tone(n, amp int) []int16 {
	s := make([]int16, n)
	for i := range s {
		s[i] = int16(amp * (i%32 - 16))
	}
	return s
}

// peak is s's largest magnitude.
func peak(s []int16) int {
	p := 0
	for _, v := range s {
		p = max(p, int(v), -int(v))
	}
	return p
}

// waitView waits until a view shows what ok looks for.
func (r *rig) waitView(t *testing.T, what string, ok func(View) bool) {
	t.Helper()
	for deadline := time.Now().Add(2 * time.Second); time.Now().Before(deadline); time.Sleep(2 * time.Millisecond) {
		r.ui.mu.Lock()
		found := slices.ContainsFunc(r.ui.views, ok)
		r.ui.mu.Unlock()
		if found {
			return
		}
	}
	t.Fatalf("no view %s: %s", what, r.ui)
}

// logBuf is the log's output while a test reads it.
type logBuf struct {
	mu sync.Mutex
	b  strings.Builder
}

func (l *logBuf) Write(p []byte) (int, error) { l.mu.Lock(); defer l.mu.Unlock(); return l.b.Write(p) }
func (l *logBuf) String() string              { l.mu.Lock(); defer l.mu.Unlock(); return l.b.String() }

// captureLog sends the log to a logBuf until the test ends.
func captureLog(t *testing.T) *logBuf {
	l := &logBuf{}
	log.SetOutput(l)
	t.Cleanup(func() { log.SetOutput(os.Stderr) })
	return l
}

// 2 s of digital silence turns the notice on, a live block turns it off; it
// is never a flash, and the take carries on and delivers. The record's
// no_signal line says when it came on (the second 1.5 s block of zeros), the
// log when it went (the tone, after 4.5 s of zeros).
func TestNoSignalLastsWhileSilent(t *testing.T) {
	zeros := make([]int16, audio.Rate*3/2)
	logs := captureLog(t)
	r, base := rig2020(t, &fakeSource{blocks: [][]int16{zeros, zeros, zeros, tone(audio.Rate, 1000)}}, "text")
	r.c.Toggle()
	for deadline := time.Now().Add(2 * time.Second); !r.ui.noticeCleared(); time.Sleep(2 * time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatalf("no recording view without the notice after one with it: %s", r.ui)
		}
	}
	r.c.Toggle()
	r.wait(t)
	if r.del.got() != "telegram|text" || r.ui.noSignals() != 1 || strings.Contains(r.ui.String(), "flash:"+NoSignal) {
		t.Fatalf("delivered %q, notice on %d times, ui %s", r.del.got(), r.ui.noSignals(), r.ui)
	}
	if on := line(base, "no_signal"); on.AfterS != 3 || !strings.Contains(logs.String(), "signal back at 4.50 s") {
		t.Fatalf("no_signal after_s %v; log:\n%s", on.AfterS, logs)
	}
	r = newRig(t, &fakeSource{blocks: [][]int16{tone(audio.Rate*3/2, 1000), tone(audio.Rate*3/2, 1000)}}, "text")
	r.c.Toggle()
	time.Sleep(50 * time.Millisecond)
	r.c.Toggle()
	r.wait(t)
	if r.ui.noSignals() != 0 {
		t.Fatalf("a live signal showed the notice: %s", r.ui)
	}
}

// A take's backup track, recording when the take stops: kept and decoded in
// the main track's place when the main track held 2 s of digital silence and
// the backup holds a voice; removed when the main track is live.
func TestBackupTrack(t *testing.T) {
	zeros := make([]int16, audio.Rate*3)
	for _, c := range []struct {
		name         string
		main, backup []int16
		text, audio  string
		kept         bool
	}{
		{"silent main, speaking backup", zeros, speech(audio.Rate*3, 2000), "backup words", "backup", true},
		{"live main, speaking backup", tone(audio.Rate*3, 1000), speech(audio.Rate*3, 2000), "main words", "main", false},
		{"silent main, silent backup", zeros, zeros, "", "main", true},
	} {
		t.Run(c.name, func(t *testing.T) {
			r := newRig(t, &fakeSource{blocks: [][]int16{c.main}})
			r.c.NewBackup = func(audio.Source) audio.Source { return &fakeSource{blocks: [][]int16{c.backup}} }
			r.asr.answer = func(s []int16) string {
				switch p := peak(s); {
				case p == 0:
					return "Thanks for listening." // a model's answer to silence
				case p < 20000:
					return "main words"
				}
				return "backup words"
			}
			r.c.Toggle()
			r.backupRecording(t)
			r.c.Toggle()
			r.wait(t)
			name := r.c.Now().Format("20060102-150405")
			base := filepath.Join(r.dir, name)
			want := ""
			if c.text != "" {
				want = "telegram|" + c.text
			}
			if r.del.got() != want {
				t.Fatalf("delivered %q, want %q", r.del.got(), want)
			}
			evs, _ := store.Read(base)
			var te store.Event
			backupOn := false
			for _, e := range evs {
				switch e.Ev {
				case "text":
					te = e
				case "backup_on":
					backupOn = true
				}
			}
			if te.Audio != c.audio || !backupOn {
				t.Fatalf("text line audio %q, backup_on %v; want %q, true", te.Audio, backupOn, c.audio)
			}
			_, err := os.Stat(filepath.Join(r.dir, store.BackupDir, name+".wav"))
			if kept := err == nil; kept != c.kept {
				t.Fatalf("backup kept %v, want %v", kept, c.kept)
			}
			if want := strings.ReplaceAll("N.events.jsonl,N.raw.txt,N.txt,N.wav,backup", "N", name); files(t, r.dir) != want {
				t.Fatalf("store holds %s", files(t, r.dir))
			}
		})
	}
}

// btSource is a fakeSource on a Bluetooth input.
type btSource struct{ *fakeSource }

func (btSource) Input() audio.TakeInput {
	return audio.TakeInput{Source: "local", Name: "Handheld BT", Transport: "blue"}
}

// A Bluetooth input opens with digital silence while the headset switches
// into call mode: the take shows warming until its first live sample, and
// silence that short is not "no signal"; silence past BluetoothWarmup is.
func TestBluetoothWarmup(t *testing.T) {
	tone := make([]int16, audio.Rate)
	for i := range tone {
		tone[i] = int16(1000 * (i%32 - 16))
	}
	for _, tc := range []struct {
		zeros    time.Duration
		noSignal int
	}{{3 * time.Second, 0}, {audio.BluetoothWarmup + time.Second, 1}} {
		zeros := make([]int16, int(tc.zeros.Seconds()*audio.Rate))
		r := newRig(t, &fakeSource{blocks: [][]int16{zeros, tone}}, "text")
		r.c.NewSource = func() audio.Source { return btSource{r.src.clone()} }
		r.c.Toggle()
		live := func() bool {
			r.ui.mu.Lock()
			defer r.ui.mu.Unlock()
			for _, v := range r.ui.views {
				if v.Quality != nil && !v.Warming {
					return true
				}
			}
			return false
		}
		for deadline := time.Now().Add(2 * time.Second); !live() && time.Now().Before(deadline); {
			time.Sleep(5 * time.Millisecond)
		}
		r.c.Toggle()
		r.wait(t)
		if v := r.ui.views[0]; !v.Warming || v.Device != "Handheld BT" || !live() {
			t.Fatalf("%v of silence: first view warming %v on %q, then live %v", tc.zeros, v.Warming, v.Device, live())
		}
		if n := r.ui.noSignals(); n != tc.noSignal || !strings.HasPrefix(r.del.got(), "telegram|text") {
			t.Fatalf("%v of silence: no-signal notice on %d times, want %d; delivered %q", tc.zeros, n, tc.noSignal, r.del.got())
		}
	}
}

// Digital silence is never decoded — a model invents a sentence for it — so
// a take that recorded nothing else ends empty and a long take's silent
// chunk adds nothing; a quiet take above audio.DeadLevel is decoded as any.
func TestSilenceNotDecoded(t *testing.T) {
	sec := audio.Rate
	dither := make([]int16, 3*sec) // ±2 LSB, -87 dBFS
	for i := range dither {
		dither[i] = int16(i%5 - 2)
	}
	for _, c := range []struct {
		name   string
		blocks [][]int16
		chunks bool // cut every 2 s, else decoded whole
		text   string
		calls  int
	}{
		{"digital silence", [][]int16{make([]int16, 3*sec)}, false, "", 0},
		{"two LSB of noise", [][]int16{dither}, false, "", 0},
		{"a quiet voice, -71 dBFS", [][]int16{tone(3*sec, 1)}, false, "words", 1},
		{"a silent chunk in a long take", [][]int16{tone(2*sec, 1000), make([]int16, 2*sec), tone(2*sec, 1000)}, true, "words words", 2},
	} {
		t.Run(c.name, func(t *testing.T) {
			r, base := rig2020(t, &fakeSource{blocks: c.blocks})
			r.c.WholeMax = time.Minute
			if c.chunks {
				r.c.WholeMax = 0
				r.c.NewChunker = func() Chunker { return &everyN{n: 2 * sec} }
			}
			r.asr.answer = func(s []int16) string {
				if peak(s) <= 2 {
					return "Thanks for listening." // a model's answer to silence
				}
				return "words"
			}
			r.take(t)
			want, st := "", "empty/"
			if c.text != "" {
				want, st = "telegram|"+c.text, "pasted/"
			}
			if r.del.got() != want || state(base) != st || r.asr.n() != c.calls {
				t.Fatalf("delivered %q, want %q; state %s; %d decodes, want %d", r.del.got(), want, state(base), r.asr.n(), c.calls)
			}
		})
	}
}

func TestNoSpeechKeepsTheClip(t *testing.T) {
	r := newRig(t, &fakeSource{n: audio.Rate}, "")
	r.c.Toggle()
	r.c.Toggle()
	r.wait(t)
	if r.del.got() != "" || files(t, r.dir) != "20200417-104200.events.jsonl,20200417-104200.raw.txt,20200417-104200.txt,20200417-104200.wav" {
		t.Fatalf("delivered %q, files %s", r.del.got(), files(t, r.dir))
	}
}

// MaxDuration stops a take; the next tap starts a new one.
func TestAutoStop(t *testing.T) {
	r := newRig(t, &fakeSource{n: audio.Rate}, "long", "next")
	r.c.MaxDuration = 30 * time.Millisecond
	r.c.Toggle()
	time.Sleep(80 * time.Millisecond)
	r.wait(t)
	if r.del.got() != "telegram|long" {
		t.Fatalf("delivered %q, ui %s", r.del.got(), r.ui)
	}
	r.c.Toggle()
	if r.c.State() != Recording {
		t.Fatalf("state %v after the next tap", r.c.State())
	}
	r.c.Toggle()
	r.wait(t)
	if r.del.got() != "telegram|long;telegram|next" {
		t.Fatalf("delivered %q", r.del.got())
	}
}

func TestEstimate(t *testing.T) {
	var e estimator
	e.add(time.Second, 10*time.Second)
	e.add(time.Second, 10*time.Second)
	e.add(5*time.Second, 500*time.Millisecond) // short: mostly fixed cost, not counted
	if got := e.left(10 * audio.Rate); got != 0 {
		t.Fatalf("estimate after 2 chunks: %v, want unknown", got)
	}
	e.add(time.Second, 10*time.Second)
	if got := e.left(10 * audio.Rate); got != time.Second {
		t.Fatalf("estimate: %v, want 1s", got)
	}
	if got := e.left(0); got != 0 {
		t.Fatalf("nothing pending: %v", got)
	}
}

// Join puts one space between two Latin or digit sides and nothing where
// either side is CJK, fullwidth punctuation included; it trims each part and
// skips the empty ones.
func TestJoin(t *testing.T) {
	for want, parts := range map[string][]string{
		"河水清澈":                   {"  ", "河水", " 清澈 "},
		"open the window please": {"open the window", "please"},
		"温度25度":                  {"温度", "25", "度"},
		"page 12。":               {"page 12", "。"},
		"7 plus 8":               {"7", "plus", "8"},
		"":                       nil,
		"x":                      {"", "x", "\t"},
	} {
		if got := Join(parts); got != want {
			t.Errorf("Join(%q) = %q, want %q", parts, got, want)
		}
	}
}

// fakeStreamer is a streaming engine: each stream counts what it was
// given and answers text, or err.
type fakeStreamer struct {
	mu      sync.Mutex
	text    string
	err     error
	opened  int
	samples int
	aborted int
}

type fakeStream struct{ s *fakeStreamer }

func (f *fakeStreamer) Open() asr.Stream {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.opened++
	return fakeStream{f}
}

func (f fakeStream) Write(s []int16) { f.s.mu.Lock(); f.s.samples += len(s); f.s.mu.Unlock() }
func (f fakeStream) Finish(context.Context) (string, error) {
	f.s.mu.Lock()
	defer f.s.mu.Unlock()
	return f.s.text, f.s.err
}
func (f fakeStream) Abort() { f.s.mu.Lock(); f.s.aborted++; f.s.mu.Unlock() }

// A streaming primary gets every block as it records and its text is the
// take's; the chunk ASR is not called.
func TestStreamPrimaryDeliversItsText(t *testing.T) {
	r := newRig(t, &fakeSource{n: 3 * audio.Rate}, "local")
	st := &fakeStreamer{text: "流 text 派森"}
	r.c.Stream = st
	r.c.NewChunker = func() Chunker { return &everyN{n: audio.Rate} }
	r.c.Toggle()
	r.c.Toggle()
	r.wait(t)
	if got := r.del.got(); got != "telegram|流 text Python" || r.asr.n() != 0 {
		t.Fatalf("delivered %q after %d ASR calls", got, r.asr.n())
	}
	if st.opened != 1 || st.samples != 3*audio.Rate {
		t.Fatalf("streams %d, samples %d", st.opened, st.samples)
	}
	raw, _ := os.ReadFile(r.dir + "/20200417-104200.raw.txt")
	if string(raw) != "流 text 派森\n" || files(t, r.chunks) != "" {
		t.Fatalf("raw %q, chunk dir %q", raw, files(t, r.chunks))
	}
}

// A stream that fails leaves the take to the chunk ASR, from its WAV, as
// recording would have cut it.
func TestStreamFailureFallsBackToASR(t *testing.T) {
	for _, c := range []struct {
		name    string
		samples int
		wholes  string
	}{{"whole", 2 * audio.Rate, "[true]"}, {"chunked", 3 * audio.Rate, "[false false false]"}} {
		t.Run(c.name, func(t *testing.T) {
			r := newRig(t, &fakeSource{n: c.samples}, "local")
			r.c.Stream = &fakeStreamer{err: errors.New("doubao: connect: no route")}
			r.c.NewChunker = func() Chunker { return &everyN{n: audio.Rate} }
			r.c.WholeMax = 2 * time.Second
			var res TakeResult
			r.c.Tee = func(string) TakeTee { return &fakeTee{done: func(tr TakeResult) { res = tr }} }
			r.c.Toggle()
			r.c.Toggle()
			r.wait(t)
			if got := r.del.got(); !strings.HasPrefix(got, "telegram|local") || fmt.Sprint(r.asr.wholes) != c.wholes {
				t.Fatalf("delivered %q, ASR whole=%v", got, r.asr.wholes)
			}
			if res.StreamErr == nil || !strings.HasPrefix(res.Raw, "local") {
				t.Fatalf("tee result %+v: want the stream's error and the fallback's text", res)
			}
		})
	}
}

type fakeTee struct {
	mu      sync.Mutex
	samples int
	stopped time.Time
	wav     string
	ended   int // samples at End
	done    func(TakeResult)
}

func (f *fakeTee) Write(s []int16) { f.mu.Lock(); f.samples += len(s); f.mu.Unlock() }
func (f *fakeTee) End(at time.Time, wav string, n int) {
	f.mu.Lock()
	f.stopped, f.wav, f.ended = at, wav, n
	f.mu.Unlock()
}
func (f *fakeTee) Done(r TakeResult) {
	if f.done != nil {
		f.done(r)
	}
}

// The tee sees every block, the take's end with its WAV, and the primary's
// result before delivery; a take too short to keep ends with no WAV and no
// Done.
func TestTeeGetsTheTake(t *testing.T) {
	r := newRig(t, &fakeSource{n: audio.Rate}, "你好 派森")
	tee := &fakeTee{}
	var res TakeResult
	var deliveredBefore string
	tee.done = func(tr TakeResult) { res, deliveredBefore = tr, r.del.got() }
	var bases []string
	r.c.Tee = func(base string) TakeTee { bases = append(bases, base); return tee }
	r.c.Toggle()
	r.c.Toggle()
	r.wait(t)
	if len(bases) != 1 || filepath.Base(bases[0]) != "20200417-104200" || tee.samples != audio.Rate || tee.stopped.IsZero() {
		t.Fatalf("bases %q, tee samples %d, stopped %v", bases, tee.samples, tee.stopped)
	}
	if tee.wav != bases[0]+".wav" || tee.ended != audio.Rate {
		t.Fatalf("End(%q, %d)", tee.wav, tee.ended)
	}
	if res.Raw != "你好 派森" || res.Text != "你好 Python" || res.Latency <= 0 || deliveredBefore != "" {
		t.Fatalf("result %+v, delivered before Done %q", res, deliveredBefore)
	}

	short := newRig(t, &fakeSource{n: audio.MinSamples - 1}, "x")
	st := &fakeTee{done: func(TakeResult) { t.Error("Done for a removed take") }}
	short.c.Tee = func(string) TakeTee { return st }
	short.c.Toggle()
	short.c.Toggle()
	short.wait(t)
	if st.wav != "" || st.ended != audio.MinSamples-1 || st.stopped.IsZero() {
		t.Fatalf("short take: End(%q, %d) at %v", st.wav, st.ended, st.stopped)
	}
}

// rig2020 is newRig and the path of its first take.
func rig2020(t *testing.T, src *fakeSource, texts ...string) (r *rig, base string) {
	r = newRig(t, src, texts...)
	return r, filepath.Join(r.dir, "20200417-104200")
}

// state is the take's state as its record gives it, "state/why".
func state(base string) string {
	evs, _ := store.Read(base)
	s := store.State(evs)
	return s.State + "/" + s.Why
}

// Each way a take ends has its final line on disk before the overlay says
// so: at the outcome's flash the record already gives the final state.
func TestFinalLineBeforeFlash(t *testing.T) {
	for _, c := range []struct {
		name  string
		setup func(r *rig)
		act   func(r *rig)
		msg   string // the outcome's flash or alert
		want  string // the record's state at that moment
	}{
		{name: "cancel", act: func(r *rig) { r.c.Toggle(); r.c.Cancel() },
			msg: "已取消 0:01 · 右Option+Esc 重贴", want: "cancelled/"},
		{name: "empty text", setup: func(r *rig) { r.asr.texts = []string{""} },
			msg: MsgEmpty, want: "empty/"},
		{name: "deliver error", setup: func(r *rig) { r.del.deliverErr = errors.New("pane gone") },
			msg: "未送达 · 文字在剪贴板 · 右Option+Esc 重贴", want: "undelivered/deliver_failed"},
		{name: "every chunk failed", setup: func(r *rig) { r.asr.fail = map[int]bool{1: true} },
			msg: MsgASRFailed, want: "undelivered/asr_failed"},
		{name: "one chunk failed", setup: func(r *rig) {
			r.src.n = 3 * audio.Rate
			r.c.NewChunker = func() Chunker { return &everyN{n: audio.Rate} }
			r.asr.fail = map[int]bool{2: true}
		}, msg: MsgIncomplete, want: "undelivered/incomplete"},
		{name: "Enter error", setup: func(r *rig) { r.del.submitErr = errors.New("lost focus") },
			act: func(r *rig) { r.c.Toggle(); r.c.Enter() },
			msg: MsgNotSent + "lost focus", want: "pasted/"},
	} {
		t.Run(c.name, func(t *testing.T) {
			r, base := rig2020(t, &fakeSource{n: audio.Rate}, "a take written for the test")
			r.ui.probe = func(msg string) string { return msg + "=" + state(base) }
			if c.setup != nil {
				c.setup(r)
			}
			if c.act == nil {
				c.act = func(r *rig) { r.c.Toggle(); r.c.Toggle() }
			}
			c.act(r)
			r.wait(t)
			if want := c.msg + "=" + c.want; !slices.Contains(r.ui.probed, want) {
				t.Fatalf("flashes and the record's state at each: %q; want %q", r.ui.probed, want)
			}
		})
	}
}

// A take's own delivery and a delivery started beside it by another path
// (as a resend would) do not interleave: each holds the delivery lock from
// its paste through its Enter.
func TestDeliveriesDoNotInterleave(t *testing.T) {
	r, base := rig2020(t, &fakeSource{n: audio.Rate}, "a take written for the test")
	r.del.trace = true
	r.del.setFront("A")
	resend := make(chan struct{})
	r.del.block, r.del.entered = resend, make(chan struct{})
	entered := r.del.entered
	other := &target{name: "B"}
	go func() {
		unlock, err := r.c.LockDelivery(time.Second)
		if err != nil {
			t.Error(err)
			return
		}
		defer unlock()
		r.del.Deliver(other, "resent text")
		r.del.Submit(other)
	}()
	<-entered // the other delivery is inside its paste
	if !r.c.Busy() {
		t.Fatal("not Busy while a delivery holds the lock")
	}
	r.c.Toggle()
	r.c.Enter()
	for !strings.Contains(fmt.Sprint(mustRead(base)), "text") {
		time.Sleep(time.Millisecond)
	}
	time.Sleep(30 * time.Millisecond) // the take's turn has come
	close(resend)
	r.wait(t)
	got := strings.Join(r.del.calls, " ")
	if got != "B:deliver> B:deliver< B:submit A:deliver> A:deliver< A:submit A:received" {
		t.Fatalf("calls %s", got)
	}
	if r.c.Busy() {
		t.Fatal("Busy after both deliveries")
	}
}

func mustRead(base string) []string {
	evs, _ := store.Read(base)
	var out []string
	for _, e := range evs {
		out = append(out, e.Ev)
	}
	return out
}

// A delivered take's record reads start, stop, text, deliver.
func TestDeliveredTakeRecord(t *testing.T) {
	r, base := rig2020(t, &fakeSource{n: audio.Rate}, "a take written for the test")
	r.c.Engine, r.c.Build = "funasr", "abc1234"
	r.c.Toggle()
	r.c.Toggle()
	r.wait(t)
	evs, err := store.Read(base)
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(mustRead(base), ","); got != "start,stop,text,deliver" {
		t.Fatalf("record %s", got)
	}
	st, sp, tx, dl := evs[0], evs[1], evs[2], evs[3]
	if st.Trigger != "tap" || st.Engine != "funasr" || st.Build != "abc1234" {
		t.Errorf("start %+v", st)
	}
	if sp.Kind != "tap" || sp.DurS != 1 {
		t.Errorf("stop %+v", sp)
	}
	if tx.Chars != len("a take written for the test") || tx.FailedChunks != 0 || tx.SHA256 == "" || tx.Audio != "main" {
		t.Errorf("text %+v", tx)
	}
	if !dl.OK || dl.N != 1 || dl.Via != "auto" || dl.Submit != "none" || dl.Text != "" {
		t.Errorf("deliver %+v", dl)
	}
	if got := state(base); got != "pasted/" {
		t.Errorf("state %s", got)
	}
}

// The readback's verdict lands on a received line after the deliver line,
// and the delivery's own result is what it is without one.
func TestDeliverRecordsReadback(t *testing.T) {
	r, base := rig2020(t, &fakeSource{n: audio.Rate}, "a take written for the test")
	r.del.received, r.del.receivedWhy = "unknown", "an agent's pane before its Enter"
	r.asr.block = make(chan struct{})
	r.c.Toggle()
	r.c.Toggle()
	if !r.c.Enter() {
		t.Fatal("Enter not used while the take transcribes")
	}
	close(r.asr.block)
	r.wait(t)
	evs, err := store.Read(base)
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(mustRead(base), ","); got != "start,send_on,stop,text,deliver,received" {
		t.Fatalf("record %s", got)
	}
	dl, rl := evs[len(evs)-2], evs[len(evs)-1]
	if !dl.OK || dl.Submit != "ok" || dl.Err != "" || dl.Received != "" {
		t.Fatalf("deliver %+v", dl)
	}
	if rl.N != 1 || rl.Received != "unknown" || rl.Why != "an agent's pane before its Enter" {
		t.Fatalf("received %+v", rl)
	}
	if got := r.del.got(); got != "telegram|a take written for the test;telegram|⏎" {
		t.Fatalf("delivered %q", got)
	}
}

// A readback holds neither the delivery lock nor the take's turn: with a
// readback of 1.5 s the next take is delivered right after the first, each
// deliver line's at is its delivery's time, and each verdict follows its
// deliver line on a line of its own.
func TestReadbackDelaysNothing(t *testing.T) {
	r, first := rig2020(t, &fakeSource{n: audio.Rate}, "a take written for the test")
	r.del.received, r.del.receivedWhy, r.del.readFor = "whole", "found", 1500*time.Millisecond
	r.asr.block = make(chan struct{})
	r.c.Toggle()
	r.c.Toggle()
	r.c.Toggle()
	r.c.Toggle()
	close(r.asr.block)
	r.waitUpTo(t, 5*time.Second)
	r.del.mu.Lock()
	delivers := slices.Clone(r.del.delivers)
	r.del.mu.Unlock()
	if len(delivers) != 2 {
		t.Fatalf("%d deliveries", len(delivers))
	}
	if gap := delivers[1].Sub(delivers[0]); gap > 500*time.Millisecond {
		t.Fatalf("the second take was delivered %v after the first: it waited for the readback", gap)
	}
	for i, base := range []string{first, first + "-2"} {
		evs, _ := store.Read(base)
		if got := strings.Join(mustRead(base), ","); got != "start,stop,text,deliver,received" {
			t.Fatalf("%s: record %s", base, got)
		}
		dl, rl := evs[3], evs[4]
		if d := dl.At.Sub(delivers[i]); d < -time.Millisecond || d > 100*time.Millisecond {
			t.Errorf("%s: deliver at %v, %v after the delivery", base, dl.At, d)
		}
		if rl.N != 1 || rl.Received != "whole" || rl.Why != "found" || rl.At.Sub(dl.At) < time.Second {
			t.Errorf("%s: received %+v after deliver at %v", base, rl, dl.At)
		}
	}
}

// The readback reads after the paste and the Enter, on a take's own
// delivery and on a resend.
func TestReadbackAfterPasteAndEnter(t *testing.T) {
	r, _ := rig2020(t, &fakeSource{n: audio.Rate}, "a take written for the test")
	r.del.trace, r.del.received = true, "whole"
	r.del.setFront("A")
	r.asr.block = make(chan struct{})
	r.c.Toggle()
	r.c.Toggle()
	if !r.c.Enter() {
		t.Fatal("Enter not used while the take transcribes")
	}
	close(r.asr.block)
	r.wait(t)
	if _, err := r.c.Resend(Resend{ID: "20200417-104200", To: &target{name: "B"}, Send: true, Via: "cli"}); err != nil {
		t.Fatal(err)
	}
	r.wait(t)
	r.del.mu.Lock()
	got := strings.Join(r.del.calls, " ")
	r.del.mu.Unlock()
	if got != "A:deliver> A:deliver< A:submit A:received B:deliver> B:deliver< B:submit B:received" {
		t.Fatalf("calls %s", got)
	}
}

// After a cancel, Return is the app's: it never turns an earlier take still
// on its way into a send. A take started after the cancel takes Return again.
func TestEnterSkipsACancelledTake(t *testing.T) {
	r, _ := rig2020(t, &fakeSource{n: audio.Rate}, "first", "second")
	r.asr.byTake = map[string]string{"20200417-104200": "first", "20200417-104200-2": "second"}
	r.asr.block = make(chan struct{})
	r.del.setFront("A")
	r.c.Toggle()
	r.c.Toggle()
	r.del.setFront("B")
	r.c.Toggle()
	r.c.Cancel()
	if r.c.Enter() {
		t.Fatal("a Return after a cancel was used by the first take")
	}
	close(r.asr.block)
	r.wait(t)
	if got := r.del.got(); got != "A|first" {
		t.Fatalf("delivered %q", got)
	}

	// a take started after the cancel takes Return again, recording and
	// once stopped
	r, _ = rig2020(t, &fakeSource{n: audio.Rate}, "text")
	r.asr.block = make(chan struct{})
	r.c.Toggle()
	r.c.Cancel()
	r.c.Toggle()
	r.c.Toggle()
	if !r.c.Enter() {
		t.Fatal("Return not used by a take stopped after the cancel")
	}
	close(r.asr.block)
	r.wait(t)

	r, _ = rig2020(t, &fakeSource{n: audio.Rate}, "text")
	r.asr.block = make(chan struct{})
	r.c.Toggle()
	r.c.Cancel()
	if r.c.State() != Idle || !r.c.Busy() || r.c.Enter() {
		t.Fatalf("a cancelled take transcribing: state %v, busy %v; Enter must pass on", r.c.State(), r.c.Busy())
	}
	close(r.asr.block)
	r.wait(t)
	if r.c.Busy() {
		t.Fatal("Busy once the cancelled take is kept")
	}
}

// A Capture that takes 2 s — herdr answering late — holds up neither the
// audio nor the controller: the take's first block is on disk within 100 ms
// of the tap, a stop is not held up either, and the take still goes to what
// was in front at the tap. Its start line waits for the target and carries
// the key's time.
func TestAudioFirstAtStart(t *testing.T) {
	r, base := rig2020(t, &fakeSource{n: audio.Rate}, "a take written for the test")
	r.del.delay = 2 * time.Second
	r.del.setFront("A")
	tap := time.Now()
	r.c.ToggleAt("tap", tap)
	for {
		if fi, err := os.Stat(base + ".wav"); err == nil && fi.Size() > 44 {
			break
		}
		if d := time.Since(tap); d > 100*time.Millisecond {
			t.Fatalf("no block on disk %v after the tap", d)
		}
		time.Sleep(time.Millisecond)
	}
	if got := mustRead(base); len(got) != 0 {
		t.Fatalf("record %v while the target is still being captured", got)
	}
	r.del.setFront("B") // focus moves while Capture is still answering
	stop := time.Now()
	r.c.Toggle()
	if d := time.Since(stop); d > 100*time.Millisecond {
		t.Fatalf("stop took %v", d)
	}
	r.waitUpTo(t, 5*time.Second)
	if got := r.del.got(); got != "A|a take written for the test" {
		t.Fatalf("delivered %q", got)
	}
	evs, err := store.Read(base)
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(mustRead(base), ","); got != "start,stop,text,deliver" {
		t.Fatalf("record %s", got)
	}
	if d := evs[0].TapAt.Sub(tap); d < -time.Millisecond || d > time.Millisecond || evs[0].At.Sub(tap) > 100*time.Millisecond {
		t.Fatalf("start at %v tap_at %v; tapped %v", evs[0].At, evs[0].TapAt, tap)
	}
}

// A line noted before the start line is written (here, Enter turning a
// stopped take into a send while its target is still being captured) follows
// the start line in the record.
func TestNoLineBeforeStart(t *testing.T) {
	r, base := rig2020(t, &fakeSource{n: audio.Rate}, "a take written for the test")
	r.del.delay = 300 * time.Millisecond
	r.asr.block = make(chan struct{})
	r.c.Toggle()
	r.c.Toggle()
	if !r.c.Enter() {
		t.Fatal("Enter not used while the take transcribes")
	}
	close(r.asr.block)
	r.wait(t)
	if got := strings.Join(mustRead(base), ","); got != "start,send_on,stop,text,deliver" {
		t.Fatalf("record %s", got)
	}
}

// stopLine is the take's stop line.
func stopLine(t *testing.T, base string) store.Event {
	t.Helper()
	evs, err := store.Read(base)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range evs {
		if e.Ev == "stop" {
			return e
		}
	}
	t.Fatalf("%s: no stop line in %+v", base, evs)
	return store.Event{}
}

// The menu's stop delivers to the target the take locked at its start: the
// click on the menu bar changes nothing about it. The stop line names the menu.
func TestMenuStopDeliversToTargetAtStart(t *testing.T) {
	r, base := rig2020(t, &fakeSource{n: audio.Rate}, "你好")
	if r.c.StopFrom("menu", "") {
		t.Fatal("StopFrom with no take recording reported a stop")
	}
	if got := files(t, r.dir); got != "" {
		t.Fatalf("StopFrom with no take recording started one: %s", got)
	}
	r.del.setFront("herdr pane w1:p1")
	r.c.Toggle()
	r.del.setFront("megavoice menu") // the click brings the menu bar forward
	if !r.c.StopFrom("menu", "") {
		t.Fatal("StopFrom did not stop the recording take")
	}
	r.wait(t)
	if got := r.del.got(); got != "herdr pane w1:p1|你好" {
		t.Fatalf("delivered %q", got)
	}
	if len(r.del.captured) != 1 {
		t.Fatalf("captured %d targets, want the start's only", len(r.del.captured))
	}
	if e := stopLine(t, base); e.Kind != "tap" || e.Via != "menu" {
		t.Fatalf("stop line kind %q via %q, want tap via menu", e.Kind, e.Via)
	}
	if got := state(base); got != "pasted/" {
		t.Fatalf("state %s", got)
	}
}

// The menu's cancel ends the take cancelled and kept: its audio and text stay.
func TestMenuCancelKeepsTake(t *testing.T) {
	r, base := rig2020(t, &fakeSource{n: audio.Rate}, "你好")
	r.c.Toggle()
	if !r.c.CancelFrom("menu", "") {
		t.Fatal("CancelFrom did not cancel the recording take")
	}
	r.wait(t)
	if got := r.del.got(); got != "" {
		t.Fatalf("a cancelled take delivered %q", got)
	}
	if e := stopLine(t, base); e.Kind != "cancel" || e.Via != "menu" {
		t.Fatalf("stop line kind %q via %q, want cancel via menu", e.Kind, e.Via)
	}
	if got := state(base); got != "cancelled/" {
		t.Fatalf("state %s", got)
	}
	if _, err := os.Stat(base + ".wav"); err != nil {
		t.Fatalf("the cancelled take's audio is gone: %v", err)
	}
}

// toggle and cancel from the control socket write via ctl; a key writes none.
func TestStopViaCtlAndKey(t *testing.T) {
	for _, c := range []struct {
		name, kind, via string
		stop            func(c *Controller)
	}{
		{"ctl toggle", "tap", "ctl", func(c *Controller) { c.ToggleFrom("ctl") }},
		{"ctl cancel", "cancel", "ctl", func(c *Controller) { c.CancelFrom("ctl", "") }},
		{"key tap", "tap", "", func(c *Controller) { c.Toggle() }},
		{"key Esc", "cancel", "", func(c *Controller) { c.Cancel() }},
	} {
		r, base := rig2020(t, &fakeSource{n: audio.Rate}, "你好")
		r.c.Toggle()
		c.stop(r.c)
		r.wait(t)
		if e := stopLine(t, base); e.Kind != c.kind || e.Via != c.via {
			t.Errorf("%s: stop line kind %q via %q, want %q via %q", c.name, e.Kind, e.Via, c.kind, c.via)
		}
	}
}

// Drain returns once the take's final line is on disk, and no later than
// its bound while a transcription hangs, naming what it leaves.
func TestDrain(t *testing.T) {
	r, base := rig2020(t, &fakeSource{n: audio.Rate}, "你好")
	r.asr.block = make(chan struct{})
	r.c.Toggle()
	r.c.StopFrom("menu", "")
	t0 := time.Now()
	left := r.c.Drain(200 * time.Millisecond)
	if d := time.Since(t0); d < 200*time.Millisecond || d > time.Second {
		t.Fatalf("Drain returned after %v with a transcription hanging", d)
	}
	if want := []string{"20200417-104200 transcribing"}; !slices.Equal(left, want) {
		t.Fatalf("left %q, want %q", left, want)
	}
	done := make(chan []string)
	go func() { done <- r.c.Drain(5 * time.Second) }()
	select {
	case <-done:
		t.Fatal("Drain returned while the take was still transcribing")
	case <-time.After(150 * time.Millisecond):
	}
	close(r.asr.block)
	select {
	case left := <-done:
		if len(left) != 0 {
			t.Fatalf("left %q", left)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("Drain did not return once the take was delivered")
	}
	if got := state(base); got != "pasted/" {
		t.Fatalf("when Drain returned the record gave %s", got)
	}
}

// The calls the main thread and the menu's exit make never wait for the
// controller's mutex: with it held by a call that does not return, they
// still answer, and Drain returns at its bound saying the controller is held.
func TestNoWaitOnHeldMutex(t *testing.T) {
	r, _ := rig2020(t, &fakeSource{n: audio.Rate}, "你好")
	r.c.Toggle()
	r.c.mu.Lock()
	defer r.c.mu.Unlock()
	done := make(chan string)
	go func() {
		id, _, ok := r.c.Recording()
		busy := r.c.Busy()
		left := r.c.Drain(100 * time.Millisecond)
		done <- fmt.Sprintf("%s %v %v %q", id, ok, busy, left)
	}()
	select {
	case got := <-done:
		if want := `20200417-104200 true true ["the controller is held: what is in flight is not known"]`; got != want {
			t.Fatalf("got %s, want %s", got, want)
		}
	case <-time.After(time.Second):
		t.Fatal("Recording, Busy or Drain waited for the held mutex")
	}
}

// A stop or cancel naming the take it was shown for leaves another take
// recording: a click on a menu drawn for a take that has ended stops nothing.
func TestStopNamesTheTake(t *testing.T) {
	r, base := rig2020(t, &fakeSource{n: audio.Rate}, "你好")
	r.c.Toggle()
	if r.c.StopFrom("menu", "20200417-104159") || r.c.CancelFrom("menu", "20200417-104159") {
		t.Fatal("a stop for another take ended the recording one")
	}
	if id, _, ok := r.c.Recording(); !ok || id != "20200417-104200" {
		t.Fatalf("recording %q %v", id, ok)
	}
	if !r.c.StopFrom("menu", "20200417-104200") {
		t.Fatal("a stop naming the recording take did not stop it")
	}
	r.wait(t)
	if e := stopLine(t, base); e.Via != "menu" {
		t.Fatalf("stop line via %q", e.Via)
	}
	if _, _, ok := r.c.Recording(); ok || r.c.Busy() {
		t.Fatal("Recording or Busy still set once the take is delivered")
	}
}

// A take starting holds the mutex before it is published: Drain answers
// idle only with the mutex taken, so a held mutex with nothing published is
// reported held at the bound, never idle.
func TestDrainNotIdleWhileHeld(t *testing.T) {
	r, _ := rig2020(t, &fakeSource{n: audio.Rate}, "你好")
	r.c.mu.Lock()
	left := r.c.Drain(100 * time.Millisecond)
	r.c.mu.Unlock()
	if want := []string{"the controller is held: what is in flight is not known"}; !slices.Equal(left, want) {
		t.Fatalf("left %q, want %q", left, want)
	}
	if left := r.c.Drain(100 * time.Millisecond); left != nil {
		t.Fatalf("idle and free: left %q", left)
	}
}

// Once a take is stopped or cancelled, Recording says so at once, while the
// take still transcribes: the menu drops its stop and cancel rows.
func TestRecordingEndsAtStopAndCancel(t *testing.T) {
	for _, end := range []func(c *Controller) bool{
		func(c *Controller) bool { return c.StopFrom("menu", "") },
		func(c *Controller) bool { return c.CancelFrom("menu", "") },
	} {
		r, _ := rig2020(t, &fakeSource{n: audio.Rate}, "你好")
		r.asr.block = make(chan struct{})
		r.c.Toggle()
		if _, _, ok := r.c.Recording(); !ok {
			t.Fatal("not recording after the start")
		}
		end(r.c)
		if id, _, ok := r.c.Recording(); ok || !r.c.Busy() {
			t.Errorf("after the stop: recording %q %v, busy %v", id, ok, r.c.Busy())
		}
		close(r.asr.block)
		r.wait(t)
	}
}

// A Return no take uses says why: after a cancel, with no take on its way,
// or past delivery.
func TestEnterWhy(t *testing.T) {
	r, _ := rig2020(t, &fakeSource{n: audio.Rate}, "text")
	if used, why := r.c.EnterWhy(); used || why != "no take on its way" {
		t.Errorf("idle: %v %q", used, why)
	}
	r.asr.block = make(chan struct{})
	r.c.Toggle()
	r.c.Cancel()
	if used, why := r.c.EnterWhy(); used || why != "after a cancel" {
		t.Errorf("after a cancel: %v %q", used, why)
	}
	close(r.asr.block)
	r.wait(t)

	// past delivery: asked at the not-sent alert, when the take has decided
	// on its send and is still on its way
	r, _ = rig2020(t, &fakeSource{n: audio.Rate}, "text")
	r.del.submitErr = errors.New("no Enter")
	r.ui.probe = func(msg string) string {
		if !strings.HasPrefix(msg, MsgNotSent) {
			return ""
		}
		used, why := r.c.EnterWhy()
		return fmt.Sprintf("%v %s", used, why)
	}
	r.asr.block = make(chan struct{})
	r.c.Toggle()
	r.c.Toggle()
	r.c.Enter()
	close(r.asr.block)
	r.wait(t)
	if !slices.Contains(r.ui.probed, "false past delivery") {
		t.Errorf("past delivery: probed %q", r.ui.probed)
	}
}
