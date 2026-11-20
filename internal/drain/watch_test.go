package drain

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/caoer/mega-asr/internal/pages"
)

// counted is a registry that counts its List calls.
type counted struct {
	*mem
	lists atomic.Int32
}

func (c *counted) List(ctx context.Context, prefix string) ([]pages.Object, error) {
	c.lists.Add(1)
	return c.mem.List(ctx, prefix)
}

// sock is a live socket the test drives: each Watch call takes the next
// session; a session sends its frames, then holds until stopped or ended.
type sock struct {
	mu       sync.Mutex
	sessions []func(ctx context.Context, on func(pages.Frame)) error
	dials    atomic.Int32
	prefixes []string
}

func (s *sock) Watch(ctx context.Context, prefixes []string, on func(pages.Frame)) error {
	s.dials.Add(1)
	s.mu.Lock()
	s.prefixes = prefixes
	var next func(ctx context.Context, on func(pages.Frame)) error
	if len(s.sessions) > 0 {
		next, s.sessions = s.sessions[0], s.sessions[1:]
	}
	s.mu.Unlock()
	if next == nil {
		return errors.New("refused")
	}
	return next(ctx, on)
}

// held is a session that says hello, sends push after a pause, and holds.
func held(push <-chan pages.Frame) func(ctx context.Context, on func(pages.Frame)) error {
	return func(ctx context.Context, on func(pages.Frame)) error {
		on(pages.Frame{Type: "hello", Seq: 1})
		for {
			select {
			case <-ctx.Done():
				return nil
			case f := <-push:
				on(f)
			}
		}
	}
}

func watchFor(t *testing.T, d *Drain, w Watch, for_ time.Duration) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), for_)
	defer cancel()
	if err := d.Watch(ctx, w); err != nil {
		t.Fatalf("Watch: %v", err)
	}
}

func TestWatchIdleSocketTicksOnce(t *testing.T) {
	reg := &counted{mem: newMem()}
	l := &logs{}
	d := newDrain(reg, (&fake{}).stages(false), "ws:1", l)
	s := &sock{}
	s.sessions = append(s.sessions, held(nil))
	var ticks atomic.Int32
	watchFor(t, d, Watch{Socket: s, Settle: time.Millisecond, Fallback: 20 * time.Millisecond, Report: func(Run, error) { ticks.Add(1) }}, 300*time.Millisecond)
	// The first tick, and one for the hello. An idle socket adds none — the
	// next timed work is the sweep, an hour away — though the fallback is 20 ms.
	if n := ticks.Load(); n < 1 || n > 2 {
		t.Errorf("an idle socket ticked %d times in 300 ms; want the first tick and the hello's", n)
	}
	if got := s.prefixes; len(got) != 2 || got[0] != "q." || got[1] != "rec." {
		t.Errorf("selected %v, want [q. rec.]", got)
	}
}

func TestWatchTicksOnAPush(t *testing.T) {
	reg := &counted{mem: newMem()}
	l := &logs{}
	d := newDrain(reg, (&fake{}).stages(false), "ws:1", l)
	push := make(chan pages.Frame, 1)
	s := &sock{}
	s.sessions = append(s.sessions, held(push))
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	var before int32
	go func() {
		time.Sleep(150 * time.Millisecond)
		before = reg.lists.Load()
		uploaded(reg.mem, t, "m1", nil)
		reg.mem.put(t, "q.m1", map[string]any{})
		push <- pages.Frame{Type: "put", Key: "q.m1", Seq: 2}
		time.Sleep(300 * time.Millisecond)
		cancel()
	}()
	if err := d.Watch(ctx, Watch{Socket: s, Settle: time.Millisecond, Fallback: time.Hour}); err != nil {
		t.Fatal(err)
	}
	if reg.lists.Load() <= before {
		t.Fatal("a put under q. ticked nothing")
	}
	if reg.has("q.m1") && reg.rec(t, "m1")["state"] == "uploaded" {
		t.Error("the pushed record was not claimed")
	}
}

func TestWatchIgnoresWritesOutsideTheQueueAndRecords(t *testing.T) {
	reg := &counted{mem: newMem()}
	l := &logs{}
	d := newDrain(reg, (&fake{}).stages(false), "ws:1", l)
	push := make(chan pages.Frame, 4)
	s := &sock{}
	s.sessions = append(s.sessions, held(push))
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	var before, after int32
	go func() {
		time.Sleep(150 * time.Millisecond)
		before = reg.lists.Load()
		push <- pages.Frame{Type: "put", Key: "watcher", Seq: 2}
		push <- pages.Frame{Type: "put", Key: "people.index", Seq: 3}
		time.Sleep(200 * time.Millisecond)
		after = reg.lists.Load()
		cancel()
	}()
	if err := d.Watch(ctx, Watch{Socket: s, Settle: time.Millisecond, Fallback: time.Hour}); err != nil {
		t.Fatal(err)
	}
	if after != before {
		t.Errorf("writes outside q. and rec. ticked: %d → %d lists", before, after)
	}
}

func TestWatchWithoutASocketPacesTicksByTheFallback(t *testing.T) {
	reg := &counted{mem: newMem()}
	l := &logs{}
	d := newDrain(reg, (&fake{}).stages(false), "ws:1", l)
	s := &sock{} // every dial refused
	var ticks atomic.Int32
	watchFor(t, d, Watch{Socket: s, Settle: time.Millisecond, Fallback: 100 * time.Millisecond, Redial: 10 * time.Millisecond, Report: func(Run, error) { ticks.Add(1) }}, 450*time.Millisecond)
	// Ticks at 0, 100, 200, 300, 400 ms.
	if n := ticks.Load(); n < 4 || n > 5 {
		t.Errorf("ticked %d times in 450 ms with a 100 ms fallback and no socket; want 5", n)
	}
	if s.dials.Load() < 2 {
		t.Errorf("dialled %d times; want redials while the socket is refused", s.dials.Load())
	}
}

func TestTickDueIsTheEarliestTimedWork(t *testing.T) {
	m := newMem()
	l := &logs{}
	d := newDrain(m, (&fake{}).stages(false), "ws:1", l)
	run, err := d.Tick(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if want := t0.Add(d.Sweep); !run.Due.Equal(want) {
		t.Errorf("idle Due = %v, want the next sweep %v", run.Due, want)
	}

	at := t0.Add(10 * time.Minute)
	uploaded(m, t, "r1", map[string]any{"state": "aligned", "retry": map[string]any{"reason": "annex", "job": "ingest", "n": 1, "at": at.Format(time.RFC3339)}})
	m.put(t, "q.r1", map[string]any{})
	exp := t0.Add(20 * time.Minute)
	uploaded(m, t, "c1", map[string]any{"claim": map[string]any{"by": "other:1", "at": t0.Format(time.RFC3339), "expires": exp.Format(time.RFC3339)}})
	m.put(t, "q.c1", map[string]any{})
	run, err = d.Tick(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if !run.Due.Equal(at) {
		t.Errorf("Due = %v, want the retry's %v", run.Due, at)
	}
}

// flaky fails its next fails Lists of q. as a store that did not answer.
type flaky struct {
	*mem
	fails atomic.Int32
}

func (f *flaky) List(ctx context.Context, prefix string) ([]pages.Object, error) {
	if prefix == "q." && f.fails.Add(-1) >= 0 {
		return nil, errors.New("pages: 502 http_502: bad gateway")
	}
	return f.mem.List(ctx, prefix)
}

func TestTickDueIsNowAfterAStoreError(t *testing.T) {
	reg := &flaky{mem: newMem()}
	reg.fails.Store(1)
	d := newDrain(reg, (&fake{}).stages(false), "ws:1", &logs{})
	run, _ := d.Tick(context.Background())
	if run.Error == "" || !run.Due.Equal(t0) {
		t.Errorf("after a failed queue list: Error %q, Due %v; want the error and Due now (%v)", run.Error, run.Due, t0)
	}
}

func TestWatchRetriesAFailedTickAfterTheFallback(t *testing.T) {
	reg := &flaky{mem: newMem()}
	reg.fails.Store(100)
	d := newDrain(reg, (&fake{}).stages(false), "ws:1", &logs{})
	s := &sock{}
	s.sessions = append(s.sessions, held(nil))
	var ticks atomic.Int32
	watchFor(t, d, Watch{Socket: s, Settle: time.Millisecond, Fallback: 100 * time.Millisecond, Report: func(Run, error) { ticks.Add(1) }}, 450*time.Millisecond)
	// The first tick and the hello's fail; each failure is due again after
	// the fallback, not at the next sweep an hour away.
	if n := ticks.Load(); n < 4 {
		t.Errorf("ticked %d times in 450 ms while every queue list failed; want a retry every 100 ms", n)
	}
}

func TestWatchTicksAtAFutureDueInsideTheFallback(t *testing.T) {
	m := newMem()
	d := newDrain(m, (&fake{}).stages(false), "ws:1", &logs{})
	// Another drain's claim lapses 80 ms after the (fixed) now.
	uploaded(m, t, "c1", map[string]any{"claim": map[string]any{"by": "other:1", "at": t0.Format(time.RFC3339Nano), "expires": t0.Add(80 * time.Millisecond).Format(time.RFC3339Nano)}})
	m.put(t, "q.c1", map[string]any{})
	s := &sock{}
	s.sessions = append(s.sessions, held(nil))
	var ticks atomic.Int32
	watchFor(t, d, Watch{Socket: s, Settle: time.Millisecond, Fallback: time.Hour, Report: func(Run, error) { ticks.Add(1) }}, 400*time.Millisecond)
	if n := ticks.Load(); n < 3 {
		t.Errorf("ticked %d times in 400 ms with a claim due 80 ms out and a 1 h fallback; want the clock to fire at the claim's expiry", n)
	}
}

func TestWatchReconnectTicksAndStopsTheFallback(t *testing.T) {
	reg := &counted{mem: newMem()}
	d := newDrain(reg, (&fake{}).stages(false), "ws:1", &logs{})
	var mu sync.Mutex
	var ticksAt []time.Time
	var hello2 time.Time
	s := &sock{}
	s.sessions = append(s.sessions,
		func(ctx context.Context, on func(pages.Frame)) error { // up, then dropped
			on(pages.Frame{Type: "hello", Seq: 1})
			time.Sleep(30 * time.Millisecond)
			return errors.New("closed")
		},
		func(ctx context.Context, on func(pages.Frame)) error { // back
			time.Sleep(150 * time.Millisecond) // down long enough for fallback ticks
			mu.Lock()
			hello2 = time.Now()
			mu.Unlock()
			return held(nil)(ctx, on)
		})
	report := func(Run, error) { mu.Lock(); ticksAt = append(ticksAt, time.Now()); mu.Unlock() }
	watchFor(t, d, Watch{Socket: s, Settle: time.Millisecond, Fallback: 50 * time.Millisecond, Redial: 10 * time.Millisecond, Report: report}, 500*time.Millisecond)
	mu.Lock()
	defer mu.Unlock()
	if hello2.IsZero() {
		t.Fatal("the second session never said hello")
	}
	var down, after int
	for _, at := range ticksAt {
		switch {
		case at.Before(hello2) && at.After(hello2.Add(-120*time.Millisecond)):
			down++
		case !at.Before(hello2):
			after++
		}
	}
	if down < 1 {
		t.Errorf("no fallback tick while the socket was down: ticks %v, hello2 %v", ticksAt, hello2)
	}
	if after != 1 {
		t.Errorf("%d ticks after the reconnect's hello; want exactly its own tick, then none from the fallback (ticks %v, hello2 %v)", after, ticksAt, hello2)
	}
}
