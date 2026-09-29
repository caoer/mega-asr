package main

import (
	"context"
	"encoding/binary"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/caoer/mega-asr/internal/audio"
	"github.com/caoer/mega-asr/internal/ctl"
	"github.com/caoer/mega-asr/internal/session"
	"github.com/caoer/mega-asr/internal/store"
)

// liveSource streams n samples of room noise at once, then a 20 ms block
// every 20 ms until Stop.
type liveSource struct {
	n    int
	stop chan struct{}
	once sync.Once
}

func (s *liveSource) Start(context.Context) (<-chan []int16, error) {
	ch := make(chan []int16, 1)
	s.stop = make(chan struct{})
	ch <- room(s.n)
	go func() {
		defer close(ch)
		tick := time.NewTicker(audio.BlockDuration)
		defer tick.Stop()
		for {
			select {
			case <-s.stop:
				return
			case <-tick.C:
				select {
				case ch <- room(audio.Block):
				case <-s.stop:
					return
				}
			}
		}
	}()
	return ch, nil
}
func (s *liveSource) Stop()      { s.once.Do(func() { close(s.stop) }) }
func (s *liveSource) Err() error { return nil }

// room is n samples of a live input's room noise, above audio.DeadLevel.
func room(n int) []int16 {
	s := make([]int16, n)
	for i := range s {
		s[i] = int16(i%32 - 16)
	}
	return s
}

// gate holds a call until it is opened.
type gate chan struct{}

func (g gate) wait() {
	if g != nil {
		<-g
	}
}

type gatedASR struct{ g gate }

func (a gatedASR) Transcribe(context.Context, string, bool) (string, error) {
	a.g.wait()
	return "words for the test", nil
}

type pane string

func (p pane) String() string { return string(p) }

type gatedDeliverer struct{ g gate }

func (gatedDeliverer) Capture() session.Target                { return pane("scratch") }
func (d gatedDeliverer) Deliver(session.Target, string) error { d.g.wait(); return nil }
func (gatedDeliverer) Submit(session.Target) error            { return nil }

type quietUI struct{}

func (quietUI) Show(session.View) {}
func (quietUI) Flash(string)      {}
func (quietUI) Alert(string)      {}

// realCtl is a controller whose takes stream n samples and whose ASR and
// delivery wait on their gates.
func realCtl(t *testing.T, n int, asrGate, deliverGate gate) *session.Controller {
	return &session.Controller{
		NewSource: func() audio.Source { return &liveSource{n: n} },
		ASR:       gatedASR{asrGate},
		Deliver:   gatedDeliverer{deliverGate},
		UI:        quietUI{},
		Store:     store.Store{Dir: t.TempDir()},
		ChunkDir:  t.TempDir(),
		WholeMax:  10 * time.Second,
		Now:       func() time.Time { return time.Date(2020, 4, 17, 10, 42, 0, 0, time.Local) },
	}
}

// eventually waits up to 2 s for ok.
func eventually(t *testing.T, what string, ok func() bool) {
	t.Helper()
	for end := time.Now().Add(2 * time.Second); !ok(); time.Sleep(5 * time.Millisecond) {
		if time.Now().After(end) {
			t.Fatalf("%s: not within 2 s", what)
		}
	}
}

// recorded reports whether the take at base has n samples in its WAV.
func recorded(base string, n int) bool {
	st, err := os.Stat(base + ".wav")
	return err == nil && st.Size() >= int64(44+2*n)
}

func pendingRestart(t *testing.T, c controller) (*takeCommands, chan int) {
	t.Helper()
	codes := make(chan int, 2)
	k := &takeCommands{ctrl: c, flash: func(string) {}, exit: func(n int) { codes <- n }, quiet: 100 * time.Millisecond}
	resp, _ := k.handle(ctl.Request{Cmd: "restart", Args: json.RawMessage(`{"when_idle":true}`)})
	if !resp.OK || string(resp.Data) != `{"state":"pending"}` {
		t.Fatalf("restart --when-idle: %+v", resp)
	}
	return k, codes
}

func noExit(t *testing.T, codes chan int, d time.Duration) {
	t.Helper()
	select {
	case n := <-codes:
		t.Fatalf("exited %d with work in flight", n)
	case <-time.After(d):
	}
}

func exited(t *testing.T, codes chan int) {
	t.Helper()
	select {
	case n := <-codes:
		if n != 75 {
			t.Fatalf("exit %d, want 75", n)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("the pending restart never exited")
	}
}

// A pending restart and Esc on a 2 s take: no exit until the cancelled
// take's text is saved; once it fires, no take starts.
func TestRestartWhenIdleWaitsForCancelledTake(t *testing.T) {
	asr := make(gate)
	c := realCtl(t, 2*audio.Rate, asr, nil)
	base := filepath.Join(c.Store.Dir, "20200417-104200")
	c.ToggleFrom("ctl")
	eventually(t, "2 s recorded", func() bool { return recorded(base, 2*audio.Rate) })
	k, codes := pendingRestart(t, c)
	c.Cancel()
	noExit(t, codes, 500*time.Millisecond)
	close(asr)
	exited(t, codes)
	if _, err := os.Stat(base + ".txt"); err != nil {
		t.Fatalf("exited before the cancelled take's text was saved: %v", err)
	}
	evs, _ := store.Read(base)
	if st := store.State(evs); st.State != "cancelled" {
		t.Fatalf("state at the exit %+v", st)
	}
	if resp, _ := k.handle(ctl.Request{Cmd: "toggle"}); resp.OK {
		t.Fatalf("a toggle after the restart fired: %+v", resp)
	}
	c.ToggleFrom("tap") // a tap already past the key's check
	if st := c.State(); st != session.Idle {
		t.Fatalf("a take started after the restart fired: %v", st)
	}
}

// A plain restart on an idle controller closes it as it answers: in the
// moment before the exit a take cannot start.
func TestPlainRestartClosesFirst(t *testing.T) {
	c := realCtl(t, audio.Rate, nil, nil)
	codes := make(chan int, 1)
	k := &takeCommands{ctrl: c, flash: func(string) {}, exit: func(n int) { codes <- n }}
	if resp, _ := k.handle(ctl.Request{Cmd: "restart"}); !resp.OK {
		t.Fatalf("restart: %+v", resp)
	}
	c.ToggleFrom("tap")
	if st := c.State(); st != session.Idle {
		t.Fatalf("a take started before the exit: %v", st)
	}
	if code := <-codes; code != 75 {
		t.Fatalf("exit %d, want 75", code)
	}
}

// A pending restart during a resend waits for it; once it fires a resend
// is refused.
func TestRestartWhenIdleWaitsForResend(t *testing.T) {
	deliver := make(gate)
	c := realCtl(t, audio.Rate, nil, deliver)
	base := filepath.Join(c.Store.Dir, "20200101-090000")
	for _, e := range []store.Event{{Ev: "start"}, {Ev: "stop", Kind: "tap"}, {Ev: "text", Chars: 5}, {Ev: "hold", Why: "deliver_failed"}} {
		store.Append(base, e)
	}
	store.Store{}.SaveText(base, "kept", "kept")
	done := make(chan store.Event)
	go func() {
		de, err := c.Resend(session.Resend{ID: "20200101-090000", To: pane("scratch"), Via: "cli"})
		if err != nil {
			t.Error(err)
		}
		done <- de
	}()
	eventually(t, "the resend in flight", c.Busy)
	_, codes := pendingRestart(t, c)
	noExit(t, codes, 500*time.Millisecond)
	close(deliver)
	if de := <-done; !de.OK {
		t.Fatalf("resend %+v", de)
	}
	t0 := time.Now()
	exited(t, codes)
	if d := time.Since(t0); d < 90*time.Millisecond {
		t.Fatalf("exited %v after the resend; the quiet is 100 ms", d)
	}
	if _, err := c.Resend(session.Resend{ID: "20200101-090000", To: pane("scratch"), Via: "cli"}); err == nil || err.Error() != "正在重启" {
		t.Fatalf("a resend after the restart fired: %v", err)
	}
}

// A termination signal during a recording: the WAV's header gives the
// samples written, the record ends hold interrupted, the exit is 75; an
// Interrupt that does not return is cut at the deadline.
func TestSignalDuringRecording(t *testing.T) {
	c := realCtl(t, audio.Rate, nil, nil)
	c.Sound = func() { t.Error("a sound as megavoice exits") }
	base := filepath.Join(c.Store.Dir, "20200417-104200")
	c.ToggleFrom("ctl")
	eventually(t, "1 s recorded", func() bool { return recorded(base, audio.Rate) })
	sig := make(chan os.Signal, 1)
	codes := make(chan int, 2)
	go onSignal(sig, c, time.Second, func(n int) { codes <- n })
	sig <- syscall.SIGTERM
	if n := <-codes; n != 75 {
		t.Fatalf("exit %d, want 75", n)
	}
	time.Sleep(200 * time.Millisecond) // the take's own goroutines, which the exit would end, write nothing more
	b, _ := os.ReadFile(base + ".wav")
	if data := binary.LittleEndian.Uint32(b[40:]); len(b) < 44+2*audio.Rate || int(data) != len(b)-44 || binary.LittleEndian.Uint32(b[4:]) != data+36 {
		t.Fatalf("wav %d bytes, header data %d", len(b), data)
	}
	evs, _ := store.Read(base)
	var names []string
	for _, e := range evs {
		names = append(names, e.Ev+" "+e.Kind+e.Why)
	}
	if got := strings.Join(names, ","); got != "start ,stop shutdown,hold interrupted" {
		t.Fatalf("record %s", got)
	}

	codes = make(chan int, 2)
	sig <- syscall.SIGINT
	t0 := time.Now()
	go onSignal(sig, stuck{}, 200*time.Millisecond, func(n int) { codes <- n })
	if n := <-codes; n != 75 || time.Since(t0) > time.Second {
		t.Fatalf("stuck interrupt: exit %d after %v", n, time.Since(t0))
	}
}

// stuck is an Interrupt that never returns.
type stuck struct{}

func (stuck) Interrupt() []string { select {} }
