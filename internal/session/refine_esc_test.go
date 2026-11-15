package session

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/caoer/mega-asr/internal/audio"
	"github.com/caoer/mega-asr/internal/store"
)

// waiters is a model step whose call for each text ends only when it is
// cancelled; asked(text) is closed as that text's call starts.
type waiters struct {
	mu sync.Mutex
	ch map[string]chan struct{}
}

func (w *waiters) asked(text string) chan struct{} {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.ch == nil {
		w.ch = map[string]chan struct{}{}
	}
	if w.ch[text] == nil {
		w.ch[text] = make(chan struct{})
	}
	return w.ch[text]
}

func (w *waiters) run(ctx context.Context, in RefineInput) (string, *store.Event) {
	close(w.asked(in.Text))
	<-ctx.Done()
	return in.Text, &store.Event{Ev: "refine", Model: "m", Why: "cancelled", Err: "cancelled"}
}

// waitLastView waits until the newest view the overlay was shown is ok.
func (r *rig) waitLastView(t *testing.T, what string, ok func(View) bool) {
	t.Helper()
	for deadline := time.Now().Add(2 * time.Second); time.Now().Before(deadline); time.Sleep(2 * time.Millisecond) {
		r.ui.mu.Lock()
		found := len(r.ui.views) > 0 && ok(r.ui.views[len(r.ui.views)-1])
		r.ui.mu.Unlock()
		if found {
			return
		}
	}
	t.Fatalf("the last view is not %s: %s", what, r.ui)
}

func liveIs(v View, text string) bool { return v.Live != nil && v.Live() == text }

// A take delivered as recognised whose call finishes beside it holds up
// nothing: while the call is unanswered the controller is not Busy, a
// drain finds nothing in flight and Close lets an exit go ahead.
func TestRefineAsideHoldsNoExit(t *testing.T) {
	r, base := rig2020(t, &fakeSource{n: audio.Rate}, "用海库跑一下 派森")
	r.c.Deliver = recordDeliverer{r.del, store.Target{Kind: "herdr", Pane: "w1:p2", Agent: "claude"}}
	answer := make(chan struct{})
	r.c.Refine = func() *RefineStep {
		return &RefineStep{
			Aside: func(target *store.Target) bool { return target.IsAgent() },
			Run: func(_ context.Context, in RefineInput) (string, *store.Event) {
				<-answer
				return in.Text, &store.Event{Ev: "refine", Model: "m", Copy: "raw"}
			},
		}
	}
	r.c.Toggle()
	r.c.Toggle()
	for deadline := time.Now().Add(2 * time.Second); state(base) != "pasted/" && time.Now().Before(deadline); {
		time.Sleep(time.Millisecond)
	}
	time.Sleep(20 * time.Millisecond) // the take's own goroutine ends after its last line
	if busy, left, closed := r.c.Busy(), r.c.Drain(0), r.c.Close("exiting"); busy || left != nil || !closed {
		t.Fatalf("with the call beside a delivered take unanswered: busy %v, in flight %v, close %v, state %s", busy, left, closed, state(base))
	}
	r.c.Reopen()
	close(answer)
	r.wait(t)
	if !strings.HasSuffix(lineNames(t, base), "deliver,refine") {
		t.Fatalf("record %s", lineNames(t, base))
	}
}

// A take with failed chunks keeps no .txt. Held by the model step — no
// usable answer, or Esc while it waits — it keeps its text as recognised on
// its hold line, and a resend of it sends that text.
func TestHeldTakeWithFailedChunksKeepsItsText(t *testing.T) {
	for _, end := range []string{"no answer", "cancel"} {
		r, base := rig2020(t, &fakeSource{n: 3 * audio.Rate}, "one", "two", "three")
		r.asr.fail = map[int]bool{2: true}
		r.c.NewChunker = func() Chunker { return &everyN{n: audio.Rate} }
		u := newUnanswered()
		r.c.Refine = step(u.run)
		want := "undelivered/" + HoldRefineFailed
		if end == "no answer" {
			r.c.Refine = step(func(_ context.Context, in RefineInput) (string, *store.Event) {
				return in.Text, &store.Event{Ev: "refine", Model: "m", Why: "http", Err: "status 529"}
			})
		}
		r.c.Toggle()
		r.c.Toggle()
		if end == "cancel" {
			<-u.asked
			if !r.c.CancelFrom("", "") {
				t.Fatal("Esc cancelled nothing")
			}
			want = "cancelled/"
		}
		r.wait(t)
		evs, err := store.Read(base)
		if err != nil {
			t.Fatal(err)
		}
		text, source, err := textOf(base, evs, "")
		if state(base) != want || err != nil || source != "delivered" || !strings.Contains(text, "one") || !strings.Contains(text, "three") || r.del.got() != "" {
			t.Fatalf("%s: state %s, text %q (%s, %v), delivered %q, record %s", end, state(base), text, source, err, r.del.got(), lineNames(t, base))
		}
	}
}

// With a take of the Mac's and one a phone streams both waiting on the
// model, the overlay shows the Mac's, whose wait Esc cancels, though the
// phone's is newer. With only the phone's waiting it shows that one and
// says Esc does not cancel it: Esc is not taken, and cancels nothing.
func TestEscCancelsTheTakeShown(t *testing.T) {
	r := newRig(t, &fakeSource{n: audio.Rate})
	r.asr.byTake = map[string]string{"20200417-104200": "mac", "20200417-104200-2": "phone"}
	w := &waiters{}
	r.c.Refine = step(w.run)
	r.c.Toggle()
	r.c.Toggle()
	<-w.asked("mac")
	sink := &fakeDeliverer{front: "iPhone 键盘"}
	if _, err := r.c.StartStream(&fakeSource{n: audio.Rate}, sink, "keyboard"); err != nil {
		t.Fatal(err)
	}
	if !r.c.StopFrom("keyboard", "") {
		t.Fatal("the keyboard stopped nothing")
	}
	<-w.asked("phone")
	r.waitLastView(t, "the Mac's take, Esc's", func(v View) bool { return v.Refining && v.EscCancels && liveIs(v, "mac") })
	if !r.c.Waiting() || !r.c.CancelFrom("", "") {
		t.Fatalf("waiting %v; Esc cancelled nothing", r.c.Waiting())
	}
	r.waitLastView(t, "the phone's take, not Esc's", func(v View) bool { return v.Refining && !v.EscCancels && liveIs(v, "phone") })
	if r.c.Waiting() || r.c.CancelFrom("", "") {
		t.Fatal("Esc is taken for, or cancelled, a take a phone streams")
	}
	if !r.c.CancelFrom("keyboard", "") {
		t.Fatal("the keyboard cancelled nothing")
	}
	r.wait(t)
	if mac, phone := state(r.dir+"/20200417-104200"), state(r.dir+"/20200417-104200-2"); mac != "cancelled/" || phone != "cancelled/" {
		t.Fatalf("states %s, %s", mac, phone)
	}
}

// While a phone keyboard records, Esc still cancels a take of the Mac's
// waiting on the model; the phone's recording carries on.
func TestEscReachesAWaitWhileAPhoneRecords(t *testing.T) {
	r := newRig(t, &fakeSource{n: audio.Rate})
	r.asr.byTake = map[string]string{"20200417-104200": "mac", "20200417-104200-2": "phone"}
	w := &waiters{}
	r.c.Refine = func() *RefineStep {
		return &RefineStep{Run: func(ctx context.Context, in RefineInput) (string, *store.Event) {
			if in.Text == "phone" {
				return in.Text, &store.Event{Ev: "refine", Model: "m"}
			}
			return w.run(ctx, in)
		}}
	}
	r.c.Toggle()
	r.c.Toggle()
	<-w.asked("mac")
	sink := &fakeDeliverer{front: "iPhone 键盘"}
	if _, err := r.c.StartStream(&fakeSource{n: audio.Rate}, sink, "keyboard"); err != nil {
		t.Fatal(err)
	}
	if !r.c.Waiting() || !r.c.CancelFrom("", "") {
		t.Fatalf("while the phone records: waiting %v; Esc cancelled nothing", r.c.Waiting())
	}
	if _, _, ok := r.c.Recording(); !ok {
		t.Fatal("Esc ended the phone's recording")
	}
	if !r.c.StopFrom("keyboard", "") {
		t.Fatal("the keyboard stopped nothing")
	}
	r.wait(t)
	if mac, phone := state(r.dir+"/20200417-104200"), state(r.dir+"/20200417-104200-2"); mac != "cancelled/" || phone != "pasted/" || !strings.HasSuffix(sink.got(), "|phone") {
		t.Fatalf("states %s, %s; phone got %q", mac, phone, sink.got())
	}
}
