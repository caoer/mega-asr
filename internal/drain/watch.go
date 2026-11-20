package drain

import (
	"context"
	"strings"
	"sync/atomic"
	"time"

	"github.com/caoer/mega-asr/internal/pages"
)

// Socket is the page's live socket as the watch uses it (*pages.Client).
type Socket interface {
	Watch(ctx context.Context, prefixes []string, on func(pages.Frame)) error
}

// watched are the prefixes whose writes can owe the drain work: the queue,
// and the records (a speaker named by hand, an action set on the page).
var watched = []string{"q.", "rec."}

// Watch is how a long-running drain decides when to tick.
type Watch struct {
	Socket Socket
	// Settle is how long a push waits for the writes that follow it (a page
	// action writes the record and its queue key) before the tick.
	Settle time.Duration
	// Fallback is the least interval between ticks the clock starts: while
	// no socket is open (the page's rule for a reactive reader that lost its
	// socket), and when timed work is already overdue.
	Fallback time.Duration
	// Redial is the first wait after a socket ended; it doubles to 15 s.
	Redial time.Duration
	// Report receives each tick's run.
	Report func(Run, error)
}

// Watch ticks once, then whenever the socket reports a write under q. or
// rec., when it opens (a write may have landed while it was down), and when
// timed work falls due (Run.Due). With no socket open it ticks no more often
// than Fallback. An idle page costs one socket and no reads. It returns when
// ctx ends.
func (d *Drain) Watch(ctx context.Context, w Watch) error {
	if w.Settle <= 0 {
		w.Settle = 2 * time.Second
	}
	if w.Fallback <= 0 {
		w.Fallback = 2 * time.Minute
	}
	if w.Redial <= 0 {
		w.Redial = time.Second
	}
	wake := make(chan struct{}, 1)
	signal := func(ch chan struct{}) {
		select {
		case ch <- struct{}{}:
		default:
		}
	}
	var up atomic.Bool
	moved := make(chan struct{}, 1) // the socket opened or closed
	go func() {
		wait := w.Redial
		for ctx.Err() == nil {
			opened := false
			err := w.Socket.Watch(ctx, watched, func(f pages.Frame) {
				switch {
				case f.Type == "hello":
					opened = true
					up.Store(true)
					signal(moved)
					signal(wake)
				case strings.HasPrefix(f.Key, "q.") || strings.HasPrefix(f.Key, "rec."):
					signal(wake)
				}
			})
			if up.Swap(false) {
				signal(moved)
			}
			if ctx.Err() != nil {
				return
			}
			if err != nil {
				d.logf("watch: %v", err)
			}
			if opened {
				wait = w.Redial // a session that held redials at once
			}
			select {
			case <-ctx.Done():
				return
			case <-time.After(wait):
			}
			if !opened {
				wait = min(2*wait, 15*time.Second)
			}
		}
	}()

	tick := func() Run {
		run, err := d.Tick(ctx)
		if w.Report != nil && ctx.Err() == nil {
			w.Report(run, err)
		}
		return run
	}
	run := tick()
	last := time.Now()
	for {
		// The clock's next tick: when timed work falls due. Work already due
		// (overdue, or a store call that failed) and a page with no socket to
		// report a change tick no sooner than Fallback after the last tick.
		var next time.Time
		switch floor := last.Add(w.Fallback); {
		case !up.Load(), !run.Due.After(run.Ended):
			next = floor
		default:
			next = last.Add(run.Due.Sub(run.Ended))
		}
		timer := time.NewTimer(time.Until(next))
		select {
		case <-ctx.Done():
			timer.Stop()
			return nil
		case <-moved:
			timer.Stop()
			continue
		case <-wake:
			timer.Stop()
			select {
			case <-ctx.Done():
				return nil
			case <-time.After(w.Settle):
			}
			select { // the writes the settle covered are this tick's
			case <-wake:
			default:
			}
		case <-timer.C:
		}
		run = tick()
		last = time.Now()
	}
}
