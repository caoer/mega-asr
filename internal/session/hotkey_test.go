package session

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/caoer/mega-asr/internal/audio"
	"github.com/caoer/mega-asr/internal/store"
)

// fixtureTake writes a take's record, and its text when text is not "", in
// dir: started ago before now, length long, ended by the lines of end.
func fixtureTake(t *testing.T, dir, name string, now time.Time, ago, long time.Duration, text string, end ...store.Event) {
	t.Helper()
	base := filepath.Join(dir, name)
	at := now.Add(-ago)
	evs := append([]store.Event{
		{Ev: "start", Trigger: "tap"},
		{Ev: "stop", Kind: "tap", DurS: long.Seconds()},
		{Ev: "text", Chars: len([]rune(text))},
	}, end...)
	for i, e := range evs {
		e.At = at.Add(time.Duration(i) * time.Millisecond)
		if err := store.Append(base, e); err != nil {
			t.Fatal(err)
		}
	}
	if text != "" {
		if err := os.WriteFile(base+".txt", []byte(text+"\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
}

var (
	failed    = store.Event{Ev: "hold", Why: "deliver_failed"}
	asrFailed = store.Event{Ev: "hold", Why: "asr_failed"}
	cancelled = store.Event{Ev: "hold", Why: "cancelled"}
	noSpeech  = store.Event{Ev: "hold", Why: "empty"}
	pasted    = store.Event{Ev: "deliver", N: 1, Via: "auto", OK: true, Submit: "none"}
)

// The hotkey's take: the newest undelivered one with text of the last 10
// minutes, else the newest cancelled one of 1 s or more; a newest cancelled
// take without text is ErrHotkeyNoText, never a walk past it to an older
// one.
func TestHotkeyTake(t *testing.T) {
	now := time.Date(2020, 4, 17, 10, 42, 0, 0, time.Local)
	type take struct {
		name      string
		ago, long time.Duration
		text      string
		end       store.Event
	}
	min := time.Minute
	for _, c := range []struct {
		name    string
		takes   []take
		want    string
		wantErr error
	}{
		{"an empty store", nil, "", nil},
		{"undelivered before a newer cancelled one", []take{
			{"20200417-103800", 4 * min, 5 * time.Second, "the undelivered one", failed},
			{"20200417-104000", 2 * min, 5 * time.Second, "the cancelled one", cancelled},
		}, "20200417-103800", nil},
		{"the newest undelivered", []take{
			{"20200417-103500", 7 * min, 3 * time.Second, "older", failed},
			{"20200417-103900", 3 * min, 3 * time.Second, "newer", asrFailed},
		}, "20200417-103900", nil},
		{"an undelivered take without text is passed over", []take{
			{"20200417-103500", 7 * min, 3 * time.Second, "with text", failed},
			{"20200417-103900", 3 * min, 3 * time.Second, "", asrFailed},
		}, "20200417-103500", nil},
		{"undelivered past 10 minutes: the cancelled one", []take{
			{"20200417-103100", 11 * min, 3 * time.Second, "too old", failed},
			{"20200417-104000", 2 * min, 2 * time.Second, "the cancelled one", cancelled},
		}, "20200417-104000", nil},
		{"a cancelled take under 1 s is a false start", []take{
			{"20200417-103800", 4 * min, 2 * time.Second, "long enough", cancelled},
			{"20200417-104000", 2 * min, 800 * time.Millisecond, "false start", cancelled},
		}, "20200417-103800", nil},
		{"a cancelled take without text", []take{{"20200417-104000", 2 * min, 3 * time.Second, "", cancelled}}, "", ErrHotkeyNoText},
		{"the newest cancelled take without text: never the older one", []take{
			{"20200417-103700", 5 * min, 4 * time.Second, "the older take", cancelled},
			{"20200417-104000", 2 * min, 3 * time.Second, "", cancelled},
		}, "", ErrHotkeyNoText},
		{"a take whose record stops before its final line is not the one", []take{
			{"20200417-103700", 5 * min, 4 * time.Second, "the cancelled one", cancelled},
			{"20200417-104000", 2 * min, 3 * time.Second, "no final line", store.Event{}},
		}, "20200417-103700", nil},
		{"no speech, pasted: none", []take{
			{"20200417-103900", 3 * min, 3 * time.Second, "", noSpeech},
			{"20200417-104000", 2 * min, 3 * time.Second, "delivered", pasted},
		}, "", nil},
		{"cancelled past 10 minutes", []take{{"20200417-103100", 11 * min, 3 * time.Second, "too old", cancelled}}, "", nil},
		{"a cancelled take of exactly 1 s", []take{{"20200417-104000", 2 * min, time.Second, "one second", cancelled}}, "20200417-104000", nil},
		{"a cancelled take of 999 ms", []take{{"20200417-104000", 2 * min, 999 * time.Millisecond, "just short", cancelled}}, "", nil},
	} {
		t.Run(c.name, func(t *testing.T) {
			dir := t.TempDir()
			for _, tk := range c.takes {
				var end []store.Event
				if tk.end.Ev != "" {
					end = append(end, tk.end)
				}
				fixtureTake(t, dir, tk.name, now, tk.ago, tk.long, tk.text, end...)
			}
			ctl := &Controller{Store: store.Store{Dir: dir}}
			if got, err := ctl.HotkeyTake(now); got != c.want || err != c.wantErr {
				t.Fatalf("got %q %v, want %q %v", got, err, c.want, c.wantErr)
			}
		})
	}
}

// A take the user dismissed does not matter to them: the hotkey passes it
// over for an older undelivered one.
func TestHotkeyTakeSkipsDismissed(t *testing.T) {
	now := time.Date(2020, 4, 17, 10, 42, 0, 0, time.Local)
	dir := t.TempDir()
	fixtureTake(t, dir, "20200417-103800", now, 4*time.Minute, 3*time.Second, "still wanted", failed)
	fixtureTake(t, dir, "20200417-104000", now, 2*time.Minute, 3*time.Second, "dismissed", failed, store.Event{Ev: "dismiss"})
	ctl := &Controller{Store: store.Store{Dir: dir}}
	if got, err := ctl.HotkeyTake(now); got != "20200417-103800" || err != nil {
		t.Fatalf("got %q %v, want the older take", got, err)
	}
}

// A take whose recovery is decoding at a start is on its way: the hotkey
// says so, and never pastes the older cancelled take instead.
func TestHotkeyTakeDuringRecovery(t *testing.T) {
	r := newRig(t, &fakeSource{}, "recovered words")
	r.asr.block = make(chan struct{})
	now := r.c.Now()
	fixtureTake(t, r.dir, "20200417-103800", now, 4*time.Minute, 3*time.Second, "the older take", cancelled)
	leftTake(t, r.dir, now.Add(-2*time.Minute), 2*audio.Rate, "", store.Event{Ev: "start"})
	r.c.Recover()
	id, err := r.c.HotkeyTake(now)
	close(r.asr.block)
	r.wait(t)
	if id != "" || err != ErrTranscribing {
		t.Fatalf("during the recovery: %q %v, want %v", id, err, ErrTranscribing)
	}
	if id, err := r.c.HotkeyTake(now); id != "20200417-104000" || err != nil {
		t.Fatalf("after the recovery: %q %v, want the recovered take", id, err)
	}
}

// A recovery holds the hotkey only for a take the hotkey could reach: an
// older take still decoding leaves the chord to a recent undelivered one.
func TestHotkeyTakeDuringAnOldRecovery(t *testing.T) {
	r := newRig(t, &fakeSource{}, "recovered words")
	r.asr.block = make(chan struct{})
	now := r.c.Now()
	fixtureTake(t, r.dir, "20200417-103800", now, 4*time.Minute, 3*time.Second, "a recent take", failed)
	leftTake(t, r.dir, now.Add(-72*time.Hour), 2*audio.Rate, "", store.Event{Ev: "start"})
	r.c.Recover()
	id, err := r.c.HotkeyTake(now)
	close(r.asr.block)
	r.wait(t)
	if id != "20200417-103800" || err != nil {
		t.Fatalf("during an older take's recovery: %q %v, want the recent take", id, err)
	}
}

// Right after Esc the hotkey is for the take just cancelled: while it is on
// its way, an older undelivered take is not pasted in its place.
func TestHotkeyTakeWaitsForTheCancelledOne(t *testing.T) {
	now := time.Date(2020, 4, 17, 10, 42, 0, 0, time.Local)
	dir := t.TempDir()
	fixtureTake(t, dir, "20200417-103800", now, 4*time.Minute, 3*time.Second, "an older undelivered take", failed)
	ctl := &Controller{Store: store.Store{Dir: dir}, finished: []*take{{cancelled: true}}}
	if id, err := ctl.HotkeyTake(now); id != "" || err != ErrTranscribing {
		t.Fatalf("got %q %v, want %v", id, err, ErrTranscribing)
	}
}

// A cancelled take the ASR heard nothing in is silent, not failed.
func TestHotkeyTakeSilentCancelled(t *testing.T) {
	now := time.Date(2020, 4, 17, 10, 42, 0, 0, time.Local)
	dir := t.TempDir()
	fixtureTake(t, dir, "20200417-104000", now, 2*time.Minute, 3*time.Second, "", cancelled)
	if err := (store.Store{Dir: dir}).SaveText(filepath.Join(dir, "20200417-104000"), "", ""); err != nil {
		t.Fatal(err)
	}
	ctl := &Controller{Store: store.Store{Dir: dir}}
	if id, err := ctl.HotkeyTake(now); id != "" || err == nil || err.Error() != "没有识别到文字" {
		t.Fatalf("got %q %v, want 没有识别到文字", id, err)
	}
}

// Idle, the hotkey pastes the take into what has focus now, without Enter:
// a deliver line via hotkey, and the flash names 当前光标. While a take
// records or transcribes it pastes nothing and says why; with nothing to
// paste it says so.
func TestHotkey(t *testing.T) {
	t.Run("idle", func(t *testing.T) {
		r, base := keptTake(t, errors.New("pane gone"), "a take written for the test")
		r.del.setFront("B")
		r.c.Hotkey()
		r.wait(t)
		if got := r.del.got(); got != "B|a take written for the test" {
			t.Fatalf("delivered %q", got)
		}
		evs, _ := store.Read(base)
		if last := evs[len(evs)-1]; last.Ev != "deliver" || !last.OK || last.Via != "hotkey" || last.N != 2 || last.Submit != "none" {
			t.Fatalf("last line %+v", last)
		}
		if !strings.Contains(r.ui.String(), "flash:已粘贴 0:01「a take wri…」→ 当前光标") {
			t.Fatalf("ui %s", r.ui)
		}
		if !r.del.allReleased() {
			t.Fatal("the captured target was not released")
		}
	})
	t.Run("a paste that failed leaves the next chord free", func(t *testing.T) {
		r, _ := keptTake(t, errors.New("pane gone"), "a take written for the test")
		r.del.mu.Lock()
		r.del.deliverErr = errors.New("pane gone")
		r.del.mu.Unlock()
		r.c.Hotkey()
		r.waitHotkey(t)
		r.del.mu.Lock()
		r.del.deliverErr = nil
		r.del.mu.Unlock()
		r.del.setFront("B")
		r.c.Hotkey()
		r.wait(t)
		if got := r.del.got(); !strings.HasSuffix(got, "B|a take written for the test") {
			t.Fatalf("the second chord: delivered %q; ui %s", got, r.ui)
		}
	})
	t.Run("recording", func(t *testing.T) {
		r, _ := keptTake(t, errors.New("pane gone"), "a take written for the test")
		r.c.Toggle()
		r.c.Hotkey()
		r.c.Cancel()
		r.wait(t)
		if got := r.del.got(); got != "" {
			t.Fatalf("delivered %q while recording", got)
		}
		if !strings.Contains(r.ui.String(), "flash:还在录音") {
			t.Fatalf("ui %s", r.ui)
		}
	})
	t.Run("transcribing", func(t *testing.T) {
		r, _ := keptTake(t, errors.New("pane gone"), "a take written for the test")
		r.asr.block = make(chan struct{})
		r.c.Toggle()
		r.c.Toggle()
		r.c.Hotkey()
		close(r.asr.block)
		r.wait(t)
		if got := r.del.got(); got != "A|a take written for the test" {
			t.Fatalf("delivered %q: want only the second take's own delivery", got)
		}
		if !strings.Contains(r.ui.String(), "flash:还在识别") {
			t.Fatalf("ui %s", r.ui)
		}
	})
	t.Run("delivering", func(t *testing.T) {
		r, base := keptTake(t, errors.New("pane gone"), "the undelivered take", "the take delivering")
		r.del.block, r.del.entered = make(chan struct{}), make(chan struct{})
		entered := r.del.entered
		r.c.Toggle()
		r.c.Toggle()
		<-entered
		r.c.Hotkey()
		close(r.del.block)
		r.wait(t)
		if got := r.del.got(); got != "A|the take delivering" {
			t.Fatalf("delivered %q: want only the second take's own delivery", got)
		}
		if got := state(base); got != "undelivered/deliver_failed" {
			t.Fatalf("the undelivered take: %s", got)
		}
		if !strings.Contains(r.ui.String(), "flash:还在识别") {
			t.Fatalf("ui %s", r.ui)
		}
	})
	t.Run("a cancelled take whose text is not saved yet", func(t *testing.T) {
		r, base := rig2020(t, &fakeSource{n: 2 * audio.Rate}, "the take just cancelled")
		older := filepath.Join(r.dir, "20200417-103900")
		fixtureTake(t, r.dir, filepath.Base(older), r.c.now(), 3*time.Minute, 3*time.Second, "the older take", cancelled)
		r.asr.block = make(chan struct{})
		r.c.Toggle()
		r.c.Cancel()
		for !strings.Contains(r.ui.String(), "flash:已取消") {
			time.Sleep(time.Millisecond)
		}
		r.c.Hotkey()
		r.waitHotkey(t)
		if got := r.del.got(); got != "" {
			t.Fatalf("delivered %q before the take's text was saved", got)
		}
		if !strings.HasSuffix(r.ui.String(), "flash:还在识别") {
			t.Fatalf("ui %s", r.ui)
		}
		close(r.asr.block)
		r.wait(t)
		r.del.setFront("B")
		r.c.Hotkey()
		r.wait(t)
		if got := r.del.got(); got != "B|the take just cancelled" {
			t.Fatalf("once the text is saved: delivered %q", got)
		}
		if got := state(older); got != "cancelled/" {
			t.Fatalf("the older take: %s", got)
		}
		if got := state(base); got != "pasted/" {
			t.Fatalf("the cancelled take: %s", got)
		}
	})
	t.Run("the chord within the microphone's tail after Esc", func(t *testing.T) {
		gate := make(chan struct{})
		r, base := rig2020(t, &fakeSource{n: 2 * audio.Rate, stopGate: gate}, "the take just cancelled")
		older := filepath.Join(r.dir, "20200417-103900")
		fixtureTake(t, r.dir, filepath.Base(older), r.c.now(), 3*time.Minute, 3*time.Second, "the older take", cancelled)
		r.c.Toggle()
		r.c.Cancel()
		r.c.Hotkey()
		r.waitHotkey(t)
		if got := r.del.got(); got != "" {
			t.Fatalf("delivered %q while the cancelled take's microphone still streamed", got)
		}
		if !strings.HasSuffix(r.ui.String(), "flash:还在识别") {
			t.Fatalf("ui %s", r.ui)
		}
		close(gate)
		r.wait(t)
		r.del.setFront("B")
		r.c.Hotkey()
		r.wait(t)
		if got := r.del.got(); got != "B|the take just cancelled" {
			t.Fatalf("once the take is kept: delivered %q", got)
		}
		if got := state(older); got != "cancelled/" {
			t.Fatalf("the older take: %s", got)
		}
		if got := state(base); got != "pasted/" {
			t.Fatalf("the cancelled take: %s", got)
		}
	})
	t.Run("a cancelled take whose transcription failed", func(t *testing.T) {
		r, _ := rig2020(t, &fakeSource{n: 2 * audio.Rate}, "the take just cancelled")
		older := filepath.Join(r.dir, "20200417-103900")
		fixtureTake(t, r.dir, filepath.Base(older), r.c.now(), 3*time.Minute, 3*time.Second, "the older take", cancelled)
		r.asr.fail = map[int]bool{1: true}
		r.c.Toggle()
		r.c.Cancel()
		r.wait(t)
		r.c.Hotkey()
		r.waitHotkey(t)
		if got := r.del.got(); got != "" {
			t.Fatalf("delivered %q for a cancelled take without text", got)
		}
		if !strings.HasSuffix(r.ui.String(), "flash:"+MsgASRFailed) {
			t.Fatalf("ui %s", r.ui)
		}
		if got := state(older); got != "cancelled/" {
			t.Fatalf("the older take: %s", got)
		}
	})
	t.Run("a slow paste frees the caller: the next tap starts its take at once", func(t *testing.T) {
		r, base := keptTake(t, errors.New("pane gone"), "a take written for the test")
		r.del.block = make(chan struct{})
		returned := make(chan struct{})
		go func() { r.c.Hotkey(); close(returned) }()
		select {
		case <-returned:
		case <-time.After(100 * time.Millisecond):
			t.Error("Hotkey still running 100 ms into a paste that waits on the deliverer")
		}
		r.c.Hotkey()
		if !strings.HasSuffix(r.ui.String(), "flash:"+ErrResending.Error()) {
			t.Errorf("a second chord while the paste runs: ui %s", r.ui)
		}
		if !r.c.Busy() {
			t.Error("not Busy while the hotkey's paste runs")
		}
		start := time.Now()
		r.c.Toggle()
		if st := r.c.State(); st != Recording || time.Since(start) > 100*time.Millisecond {
			t.Errorf("the tap after the chord: state %v after %v", st, time.Since(start))
		}
		close(r.del.block)
		<-returned
		r.c.Cancel()
		r.wait(t)
		n := 0
		evs, _ := store.Read(base)
		for _, e := range evs {
			if e.Ev == "deliver" && e.Via == "hotkey" {
				n++
			}
		}
		if n != 1 || !strings.HasPrefix(r.del.got(), "A|a take written for the test") {
			t.Fatalf("%d hotkey deliver lines, delivered %q", n, r.del.got())
		}
	})
	t.Run("nothing to paste", func(t *testing.T) {
		r := newRig(t, &fakeSource{n: audio.Rate})
		r.c.Hotkey()
		if got := r.del.got(); got != "" {
			t.Fatalf("delivered %q", got)
		}
		if !strings.Contains(r.ui.String(), "flash:"+MsgNothingToResend) {
			t.Fatalf("ui %s", r.ui)
		}
	})
}

// waitHotkey waits for a hotkey paste in flight, if any.
func (r *rig) waitHotkey(t *testing.T) {
	t.Helper()
	for deadline := time.Now().Add(2 * time.Second); r.c.hotkeying.Load(); time.Sleep(time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatal("the hotkey's paste did not end")
		}
	}
}

// The flashes that name the hotkey name the configured tap key, not the
// default one.
func TestHotkeyHintNamesTheKey(t *testing.T) {
	r := newRig(t, &fakeSource{n: audio.Rate}, "a take written for the test")
	r.c.HotkeyKey = "右Shift"
	r.c.Toggle()
	r.c.Cancel()
	r.wait(t)
	r.del.deliverErr = errors.New("pane gone")
	r.c.Toggle()
	r.c.Toggle()
	r.wait(t)
	for _, want := range []string{"flash:已取消 0:01 · 右Shift+Esc 重贴", "alert:未送达 · 文字在剪贴板 · 右Shift+Esc 重贴"} {
		if !strings.Contains(r.ui.String(), want) {
			t.Errorf("ui %s: want %q", r.ui, want)
		}
	}
}

// The sound is asked for once for each take that becomes undelivered — by
// its own delivery, its transcription, or a failed resend of a take never
// delivered — and never for a cancel, a take without speech, one delivered,
// or a failed resend of one delivered, which stays delivered.
func TestSoundOncePerUndeliveredTake(t *testing.T) {
	for _, c := range []struct {
		name  string
		setup func(r *rig)
		act   func(r *rig)
		after func(r *rig) // once the take is kept
		want  int64
	}{
		{name: "deliver error", setup: func(r *rig) { r.del.deliverErr = errors.New("pane gone") }, want: 1},
		{name: "every chunk failed", setup: func(r *rig) { r.asr.fail = map[int]bool{1: true} }, want: 1},
		{name: "one chunk failed", setup: func(r *rig) {
			r.src.n = 3 * audio.Rate
			r.c.NewChunker = func() Chunker { return &everyN{n: audio.Rate} }
			r.asr.fail = map[int]bool{2: true}
		}, want: 1},
		{name: "cancel", act: func(r *rig) { r.c.Toggle(); r.c.Cancel() }, want: 0},
		{name: "no speech", setup: func(r *rig) { r.asr.texts = []string{""} }, want: 0},
		{name: "delivered", want: 0},
		{name: "a failed resend of an undelivered take", setup: func(r *rig) { r.del.deliverErr = errors.New("pane gone") },
			after: func(r *rig) { r.c.Resend(Resend{ID: "20200417-104200", To: &target{name: "B"}, Via: "cli"}) }, want: 1},
		{name: "a failed resend of a delivered take", after: func(r *rig) {
			r.del.mu.Lock()
			r.del.deliverErr = errors.New("pane gone")
			r.del.mu.Unlock()
			r.c.Resend(Resend{ID: "20200417-104200", To: &target{name: "B"}, Via: "cli"})
		}, want: 0}, // it stays delivered
	} {
		t.Run(c.name, func(t *testing.T) {
			r, _ := rig2020(t, &fakeSource{n: audio.Rate}, "a take written for the test")
			var sounds atomic.Int64
			r.c.Sound = func() { sounds.Add(1) }
			if c.setup != nil {
				c.setup(r)
			}
			if c.act == nil {
				c.act = func(r *rig) { r.c.Toggle(); r.c.Toggle() }
			}
			c.act(r)
			r.wait(t)
			if c.after != nil {
				c.after(r)
			}
			if n := sounds.Load(); n != c.want {
				t.Fatalf("sound asked for %d times, want %d; ui %s", n, c.want, r.ui)
			}
		})
	}
}
