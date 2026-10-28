package mac

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"
)

type composerFake struct {
	mu                                  sync.Mutex
	focus                               int
	fields                              map[int]string
	clipboard, title                    string
	version                             int
	prepareHook, activateHook, postHook func()
	delay                               time.Duration
	posts, enters, releases             int
	unreadable                          bool
}

func newComposerFake() *composerFake {
	return &composerFake{focus: 1, fields: map[int]string{1: "draft ", 2: ""}, title: "Window"}
}
func (f *composerFake) ops() composerOps {
	return composerOps{
		prepare: func(text string) error {
			f.mu.Lock()
			f.clipboard = text
			f.version++
			f.mu.Unlock()
			if f.prepareHook != nil {
				f.prepareHook()
			}
			return nil
		},
		activate: func() error {
			f.mu.Lock()
			f.focus = 1
			f.mu.Unlock()
			if f.activateHook != nil {
				f.activateHook()
			}
			return nil
		},
		focused: func() bool { f.mu.Lock(); defer f.mu.Unlock(); return f.focus == 1 },
		read: func(context.Context) (string, bool) {
			f.mu.Lock()
			defer f.mu.Unlock()
			return f.fields[1], !f.unreadable
		},
		post: func() error {
			f.mu.Lock()
			f.posts++
			f.mu.Unlock()
			if f.postHook != nil {
				f.postHook()
			}
			consume := func() { f.mu.Lock(); f.fields[f.focus] += f.clipboard; f.title = "Window (modified)"; f.mu.Unlock() }
			if f.delay > 0 {
				time.AfterFunc(f.delay, consume)
			} else {
				consume()
			}
			return nil
		},
		enter: func() error { f.mu.Lock(); defer f.mu.Unlock(); f.enters++; f.fields[f.focus] += "[ENTER]"; return nil },

		release: func() { f.mu.Lock(); f.releases++; f.mu.Unlock() },
	}
}
func (f *composerFake) move(to int) { f.mu.Lock(); f.focus = to; f.mu.Unlock() }

func TestPasteFocusChangesDuringClipboardPreparation(t *testing.T) {
	f := newComposerFake()
	f.prepareHook = func() { f.move(2) }
	p, err := pasteComposer(&clipboardBoard{}, f.ops(), "text", time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer p.Release()
	if err := p.Submit(); err != nil {
		t.Fatal(err)
	}
	if f.fields[1] != "draft text[ENTER]" || f.fields[2] != "" {
		t.Fatalf("original=%q second=%q", f.fields[1], f.fields[2])
	}
}

func TestPasteFocusChangesAfterFinalCheckBeforeEvent(t *testing.T) {
	f := newComposerFake()
	f.postHook = func() { f.move(2) }
	b := &clipboardBoard{limit: 80 * time.Millisecond}
	p, err := pasteComposer(b, f.ops(), "text", 20*time.Millisecond)
	if !errors.Is(err, ErrPasteUnconfirmed) {
		t.Fatal(err)
	}
	if err := p.Submit(); !errors.Is(err, ErrPasteUnconfirmed) {
		t.Fatal(err)
	}
	f.mu.Lock()
	if f.posts != 1 || f.enters != 0 || f.fields[1] != "draft " || f.fields[2] != "text" || f.clipboard != "text" {
		t.Fatalf("posts=%d enters=%d fields=%v", f.posts, f.enters, f.fields)
	}
	f.mu.Unlock()
	remaining := time.Until(p.expires)
	start := time.Now()
	if err := b.copy(nil, func() error { f.mu.Lock(); f.clipboard = "next"; f.mu.Unlock(); return nil }); err != nil {
		t.Fatal(err)
	}
	if elapsed := time.Since(start); elapsed+20*time.Millisecond < remaining || elapsed > time.Second {
		t.Fatalf("reservation wait %v", elapsed)
	}
}

func TestPasteFocusLostAsActivationReturns(t *testing.T) {
	f := newComposerFake()
	f.activateHook = func() { f.move(2) }
	_, err := pasteComposer(&clipboardBoard{}, f.ops(), "text", time.Second)
	if !errors.Is(err, ErrPasteUnconfirmed) || f.posts != 0 || f.enters != 0 {
		t.Fatalf("err=%v posts=%d enters=%d", err, f.posts, f.enters)
	}
}

func TestPasteWaitsForConsumerBeyondOneHundredMilliseconds(t *testing.T) {
	f := newComposerFake()
	f.delay = 250 * time.Millisecond
	b := &clipboardBoard{}
	start := time.Now()
	p, err := pasteComposer(b, f.ops(), "first", time.Second)
	if err != nil {
		t.Fatal(err)
	}
	p.Release()
	elapsed := time.Since(start)
	if elapsed < 200*time.Millisecond {
		t.Fatalf("returned before consumer: %v", elapsed)
	}
	p, err = pasteComposer(b, f.ops(), "second", time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer p.Release()
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.fields[1] != "draft firstsecond" || f.posts != 2 {
		t.Fatalf("field=%q posts=%d", f.fields[1], f.posts)
	}
	t.Logf("first consumption acknowledged after %v", elapsed)
}

func TestUnreadableComposerWaitsForDelayedPasteBeforeEnter(t *testing.T) {
	f := newComposerFake()
	f.unreadable, f.delay = true, 200*time.Millisecond
	start := time.Now()
	p, err := pasteComposer(&clipboardBoard{}, f.ops(), "text", time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer p.Release()
	time.Sleep(150 * time.Millisecond) // Deliverer's send settle
	if err := p.Submit(); err != nil {
		t.Fatal(err)
	}
	elapsed := time.Since(start)
	f.mu.Lock()
	defer f.mu.Unlock()
	if elapsed < unreadableReturnDelay || f.fields[1] != "draft text[ENTER]" || f.enters != 1 {
		t.Fatalf("Return after %v, fields=%v enters=%d", elapsed, f.fields, f.enters)
	}
	t.Logf("unreadable 200 ms consumer received Return after %v", elapsed)
}

func TestPasteConfirmsSelectionReplacement(t *testing.T) {
	for _, tc := range []struct {
		name, before, after string
		start, length       int
		selection           bool
	}{
		{"whole selection", "old text", "text", 0, 8, true},
		{"partial UTF16 selection", "🙂 old text suffix", "🙂 text suffix", 3, 8, true},
		{"whole value without selection metadata", "old text", "text", 0, 0, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newComposerFake()
			f.fields[1] = tc.before
			ops := f.ops()
			if tc.selection {
				ops.selection = func(context.Context) (int, int, bool) { return tc.start, tc.length, true }
			}
			ops.post = func() error {
				f.mu.Lock()
				defer f.mu.Unlock()
				f.posts++
				f.fields[1] = tc.after
				return nil
			}
			p, err := pasteComposer(&clipboardBoard{}, ops, "text", time.Second)
			if err != nil {
				t.Fatal(err)
			}
			defer p.Release()
			if err := p.Submit(); err != nil {
				t.Fatal(err)
			}
			if f.fields[1] != tc.after+"[ENTER]" || f.posts != 1 || f.enters != 1 {
				t.Fatalf("fields=%v posts=%d enters=%d", f.fields, f.posts, f.enters)
			}
		})
	}
}

func TestPasteSelectionContextRejectsUnchangedOrWrongReplacement(t *testing.T) {
	for _, after := range []string{"old text", "old texttext"} {
		f := newComposerFake()
		f.fields[1] = "old text"
		ops := f.ops()
		ops.selection = func(context.Context) (int, int, bool) { return 0, 8, true }
		ops.post = func() error { f.mu.Lock(); defer f.mu.Unlock(); f.fields[1] = after; return nil }
		b := &clipboardBoard{}
		p, err := pasteComposer(b, ops, "text", 20*time.Millisecond)
		if !errors.Is(err, ErrPasteUnconfirmed) {
			t.Fatalf("after=%q err=%v", after, err)
		}
		if err := p.Submit(); !errors.Is(err, ErrPasteUnconfirmed) || f.enters != 0 {
			t.Fatalf("after=%q submit=%v enters=%d", after, err, f.enters)
		}
		b.clear(p)
	}
}

func TestPasteTimeoutRetainsClipboardUntilLateConsumption(t *testing.T) {
	f := newComposerFake()
	f.delay = 150 * time.Millisecond
	b := &clipboardBoard{limit: time.Second}
	_, err := pasteComposer(b, f.ops(), "first", 20*time.Millisecond)
	if !errors.Is(err, ErrPasteUnconfirmed) {
		t.Fatal(err)
	}
	start := time.Now()
	if err := b.copy(nil, func() error { f.mu.Lock(); f.clipboard = "second"; f.mu.Unlock(); return nil }); err != nil {
		t.Fatal(err)
	}
	elapsed := time.Since(start)
	f.mu.Lock()
	defer f.mu.Unlock()
	if elapsed < 100*time.Millisecond || elapsed > time.Second || f.fields[1] != "draft first" || f.releases != 1 {
		t.Fatalf("wait=%v field=%q releases=%d", elapsed, f.fields[1], f.releases)
	}
}

func TestPasteDoesNotUsePreexistingTextAsAcknowledgement(t *testing.T) {
	f := newComposerFake()
	f.fields[1] = "draft text"
	f.postHook = func() { f.move(2) }
	_, err := pasteComposer(&clipboardBoard{}, f.ops(), "text", 20*time.Millisecond)
	if !errors.Is(err, ErrPasteUnconfirmed) || f.enters != 0 {
		t.Fatalf("err=%v enters=%d", err, f.enters)
	}
}

func TestPasteReceiptRechecksOriginalTextBeforeEnter(t *testing.T) {
	f := newComposerFake()
	p, err := pasteComposer(&clipboardBoard{}, f.ops(), "text", time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer p.Release()
	f.fields[1] = "a different draft"
	if err := p.Submit(); !errors.Is(err, ErrPasteUnconfirmed) || f.enters != 0 {
		t.Fatalf("err=%v enters=%d", err, f.enters)
	}
}

func TestUnreadableComposerStillPastesAndSends(t *testing.T) {
	f := newComposerFake()
	f.unreadable = true
	b := &clipboardBoard{limit: 30 * time.Millisecond}
	p, err := pasteComposer(b, f.ops(), "text", time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer p.Release()
	if err := p.Submit(); err != nil {
		t.Fatal(err)
	}
	if _, ok := p.Value(); ok {
		t.Fatal("unreadable field claimed content confirmation")
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.posts != 1 || f.enters != 1 || f.fields[1] != "draft text[ENTER]" {
		t.Fatalf("posts=%d enters=%d fields=%v", f.posts, f.enters, f.fields)
	}
}

func TestUnreadableComposerFocusMovePreventsEnter(t *testing.T) {
	f := newComposerFake()
	f.unreadable = true
	f.postHook = func() { f.move(2) }
	p, err := pasteComposer(&clipboardBoard{limit: 30 * time.Millisecond}, f.ops(), "text", time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer p.Release()
	if err := p.Submit(); !errors.Is(err, ErrPasteUnconfirmed) {
		t.Fatal(err)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.posts != 1 || f.enters != 0 || f.fields[2] != "text" || f.clipboard != "text" {
		t.Fatalf("posts=%d enters=%d fields=%v", f.posts, f.enters, f.fields)
	}
}

func TestUnreadableComposerReservationExpiresWithoutUserCopy(t *testing.T) {
	f := newComposerFake()
	f.unreadable = true
	b := &clipboardBoard{}
	p, err := pasteComposer(b, f.ops(), "first", time.Second)
	if err != nil {
		t.Fatal(err)
	}
	p.Release()
	start := time.Now()
	if err := b.copy(nil, func() error { f.mu.Lock(); f.clipboard = "second"; f.mu.Unlock(); return nil }); err != nil {
		t.Fatal(err)
	}
	elapsed := time.Since(start)
	if elapsed < 1800*time.Millisecond || elapsed > 3*time.Second {
		t.Fatalf("production reservation bound %v", elapsed)
	}
	t.Logf("unreadable composer reservation released after %v without user copy", elapsed)
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.clipboard != "second" || f.releases != 1 {
		t.Fatalf("clipboard=%q releases=%d", f.clipboard, f.releases)
	}
}

func TestClipboardCopyWaitsForReservationTimerCriticalSection(t *testing.T) {
	b := &clipboardBoard{}
	b.mu.Lock()
	started := make(chan struct{})
	done := make(chan error, 1)
	go func() { close(started); done <- b.copy(nil, func() error { return nil }) }()
	<-started
	b.mu.Unlock()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("copy stalled behind reservation timer")
	}
}

func TestClipboardMainThreadNeverWaitsOnClipboardMutex(t *testing.T) {
	b := &clipboardBoard{onMain: func() bool { return true }}
	b.mu.Lock()
	defer b.mu.Unlock()
	if err := b.copy(nil, func() error { t.Fatal("clipboard replaced while busy"); return nil }); !errors.Is(err, ErrClipboardPending) {
		t.Fatal(err)
	}
}
