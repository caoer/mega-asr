package session

import (
	"context"
	"errors"
	"os"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/caoer/mega-asr/internal/audio"
	"github.com/caoer/mega-asr/internal/refine"
	"github.com/caoer/mega-asr/internal/store"
)

// paneDeliverer captures targets whose record names an agent pane.
type paneDeliverer struct{ *fakeDeliverer }

type paneTarget struct{ Target }

func (paneTarget) Record() store.Target { return store.Target{Kind: "herdr", Pane: "w1:p1"} }

func (d paneDeliverer) Capture() Target { return paneTarget{d.fakeDeliverer.Capture()} }

// refineLines are the take's refine and deliver lines.
func refineLines(t *testing.T, base string) (refine, deliver *store.Event) {
	t.Helper()
	evs, err := store.Read(base)
	if err != nil {
		t.Fatal(err)
	}
	for i := range evs {
		switch evs[i].Ev {
		case "refine":
			refine = &evs[i]
		case "deliver":
			deliver = &evs[i]
		}
	}
	return refine, deliver
}

// lineNames are the ev names of the take's record, in order.
func lineNames(t *testing.T, base string) string {
	t.Helper()
	evs, err := store.Read(base)
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, e := range evs {
		names = append(names, e.Ev)
	}
	return strings.Join(names, ",")
}

// step is a model step whose call is run.
func step(run func(context.Context, RefineInput) (string, *store.Event)) func() *RefineStep {
	return func() *RefineStep { return &RefineStep{Run: run} }
}

// unanswered is a model step whose call ends only when it is cancelled; it
// counts its calls and closes asked as the first one starts.
type unanswered struct {
	calls atomic.Int32
	asked chan struct{}
}

func newUnanswered() *unanswered { return &unanswered{asked: make(chan struct{})} }

func (u *unanswered) run(ctx context.Context, in RefineInput) (string, *store.Event) {
	if u.calls.Add(1) == 1 {
		close(u.asked)
	}
	t0 := time.Now()
	<-ctx.Done()
	return in.Text, &store.Event{Ev: "refine", Model: "m", LatencyMS: time.Since(t0).Milliseconds(), Why: "cancelled", Err: "cancelled"}
}

// countNames counts the names gate's calls on r and rewrites 林小雨.
func countNames(r *rig) *atomic.Int32 {
	var n atomic.Int32
	r.c.Names = func(_ context.Context, text string) (string, *store.Event) {
		n.Add(1)
		return strings.ReplaceAll(text, "林小雨", "林晓雨"), &store.Event{Ev: "names", N: 1,
			Names: []store.NameEdit{{From: "林小雨", To: "林晓雨", Person: "林晓雨", P: 0.9}}}
	}
	return &n
}

// The model step runs after Post on a dictated take, with the take's text
// and where it goes: the delivered text is its, the .txt keeps the text
// before it, and its refine line and the deliver line's text are in the
// record. While it runs the view says so and shows the text as recognised;
// once the text is out the overlay flashes what changed.
func TestRefineCorrectsTheDeliveredText(t *testing.T) {
	r := newRig(t, &fakeSource{n: audio.Rate}, "用海库跑一下 派森")
	r.c.Deliver = paneDeliverer{r.del}
	var got RefineInput
	r.c.Refine = step(func(_ context.Context, in RefineInput) (string, *store.Event) {
		got = in
		return strings.ReplaceAll(in.Text, "海库", "Haiku"), &store.Event{Ev: "refine", Model: "m", LatencyMS: 4200,
			Edits: []store.RefineEdit{{From: "用海库跑", To: "用Haiku跑", Why: "模型名"}}}
	})
	r.c.Toggle()
	r.c.Toggle()
	r.wait(t)
	if d := r.del.got(); !strings.HasSuffix(d, "|用Haiku跑一下 Python") {
		t.Fatalf("delivered %q", d)
	}
	if got.Text != "用海库跑一下 Python" || got.Target == nil || got.Target.Kind != "herdr" {
		t.Fatalf("the step got %+v, target %+v", got, got.Target)
	}
	base := r.dir + "/20200417-104200"
	if txt, _ := os.ReadFile(base + ".txt"); string(txt) != "用海库跑一下 Python\n" {
		t.Fatalf("txt %q", txt)
	}
	refine, deliver := refineLines(t, base)
	if refine == nil || len(refine.Edits) != 1 || refine.Edits[0].To != "用Haiku跑" || deliver == nil || deliver.Text != "用Haiku跑一下 Python" {
		t.Fatalf("refine %+v, deliver %+v", refine, deliver)
	}
	r.ui.mu.Lock()
	defer r.ui.mu.Unlock()
	shown := false
	for _, v := range r.ui.views {
		shown = shown || v.Refining && v.Live != nil && v.Live() == "用海库跑一下 Python"
	}
	if !shown || !strings.Contains(strings.Join(r.ui.events, ","), "flash:海库 → Haiku") {
		t.Fatalf("refining view shown %v; ui %s", shown, strings.Join(r.ui.events, ","))
	}
}

// A take meets a model once. With the names gate and the model step both
// set, a take the step runs on is not put to the names gate: the step's is
// its one call. A take the step is off for goes through the gate as it does
// without the step.
func TestRefineIsTheTakesOneModelCall(t *testing.T) {
	r := newRig(t, &fakeSource{n: audio.Rate}, "问一下林小雨")
	names := countNames(r)
	var calls atomic.Int32
	r.c.Refine = step(func(_ context.Context, in RefineInput) (string, *store.Event) {
		calls.Add(1)
		return in.Text, &store.Event{Ev: "refine", Model: "m"}
	})
	r.c.Toggle()
	r.c.Toggle()
	r.wait(t)
	base := r.dir + "/20200417-104200"
	if got := lineNames(t, base); names.Load() != 0 || calls.Load() != 1 || strings.Contains(got, "names") || !strings.Contains(got, "refine") {
		t.Fatalf("step on: %d names calls, %d step calls, record %s", names.Load(), calls.Load(), got)
	}
}

// A step that is off leaves the take's path as it is: the names gate is
// asked, the same text goes out with the same lines and the same views, and
// no key is taken for a wait.
func TestRefineOffIsThePathWithoutIt(t *testing.T) {
	run := func(refine func() *RefineStep) (delivered, lines, ui string, names int32, waited bool) {
		r := newRig(t, &fakeSource{n: audio.Rate}, "问一下林小雨 派森")
		n := countNames(r)
		r.c.Refine = refine
		r.c.OnState = func(State) { waited = waited || r.c.Waiting() }
		r.c.Toggle()
		r.c.Toggle()
		r.wait(t)
		return r.del.got(), lineNames(t, r.dir+"/20200417-104200"), r.ui.String(), n.Load(), waited
	}
	asked := 0
	d0, l0, u0, n0, w0 := run(nil)
	d1, l1, u1, n1, w1 := run(func() *RefineStep { asked++; return nil })
	if d0 != "telegram|问一下林晓雨 Python" || n0 != 1 || !strings.Contains(l0, "names") || strings.Contains(l0, "refine") || u0 != "recording,transcribing,hide" || w0 {
		t.Fatalf("no step: delivered %q, %d names calls, record %s, ui %s, waited %v", d0, n0, l0, u0, w0)
	}
	if asked != 1 || d1 != d0 || l1 != l0 || u1 != u0 || n1 != n0 || w1 {
		t.Fatalf("step off: asked %d times; delivered %q, %d names calls, record %s, ui %s, waited %v; want as without the step", asked, d1, n1, l1, u1, w1)
	}
}

// A call that gives no answer to use holds the take: nothing is delivered,
// the record keeps the refine line with why and the hold, the take's audio
// and its text as recognised stay in the store, and the overlay alerts with
// what went wrong. The take after it is delivered as usual.
func TestRefineHoldsATakeWithNoUsableAnswer(t *testing.T) {
	for _, why := range []string{"http", "refusal", "parse", "guard", "key", "error"} {
		r := newRig(t, &fakeSource{n: audio.Rate}, "用海库跑一下 派森")
		r.asr.byTake = map[string]string{"20200417-104200": "用海库跑一下 派森", "20200417-104200-2": "第二句"}
		failed := &store.Event{Ev: "refine", Model: "m", LatencyMS: 900, Why: why, Err: "what went wrong"}
		r.c.Refine = step(func(_ context.Context, in RefineInput) (string, *store.Event) {
			if in.Text == "第二句" {
				return in.Text, &store.Event{Ev: "refine", Model: "m"}
			}
			return in.Text, failed
		})
		r.c.Toggle()
		r.c.Toggle()
		r.c.Toggle()
		r.c.Toggle()
		r.wait(t)
		base := r.dir + "/20200417-104200"
		refine, deliver := refineLines(t, base)
		txt, _ := os.ReadFile(base + ".txt")
		_, wavErr := os.Stat(base + ".wav")
		if got := r.del.got(); got != "telegram|第二句" || deliver != nil || refine == nil || refine.Why != why || refine.Err == "" ||
			state(base) != "undelivered/refine_failed" || string(txt) != "用海库跑一下 Python\n" || wavErr != nil ||
			!strings.HasSuffix(lineNames(t, base), "refine,hold") {
			t.Fatalf("%s: delivered %q, refine %+v, deliver %+v, state %s, txt %q, wav %v, record %s",
				why, got, refine, deliver, state(base), txt, wavErr, lineNames(t, base))
		}
		if alert := "alert:" + r.c.refineFailedMsg(failed); !strings.Contains(r.ui.String(), alert) {
			t.Fatalf("%s: ui %s, want %s", why, r.ui, alert)
		}
	}
	// each reason has its own words, and the key's name the key
	c := &Controller{}
	seen := map[string]bool{}
	for _, why := range []string{"http", "refusal", "parse", "guard", "key", "error"} {
		seen[c.refineFailedMsg(&store.Event{Why: why})] = true
	}
	if len(seen) != 6 || !strings.Contains(c.refineFailedMsg(&store.Event{Why: "key"}), "密钥") {
		t.Fatalf("alerts %v", seen)
	}
}

// A call the model never answers keeps the take waiting, with no limit:
// nothing is delivered, the view shows the wait with the text as recognised
// and the time since the take stopped, and Esc is the take's. Esc then
// cancels the call: the take ends cancelled and is kept whole — audio, text
// as recognised, the refine line that says it was cancelled.
func TestRefineWaitsUntilEsc(t *testing.T) {
	r, base := rig2020(t, &fakeSource{n: audio.Rate}, "用海库跑一下 派森")
	u := newUnanswered()
	r.c.Refine = step(u.run)
	if r.c.CancelFrom("", "") {
		t.Fatal("a cancel with no take cancelled something")
	}
	r.c.Toggle()
	r.c.Toggle()
	<-u.asked
	time.Sleep(400 * time.Millisecond)
	r.waitView(t, "the wait", func(v View) bool {
		return v.Refining && v.Live != nil && v.Live() == "用海库跑一下 Python" && !v.Started.IsZero() && v.Quality == nil
	})
	if got := r.del.got(); got != "" || !r.c.Waiting() || !r.c.Busy() || state(base) != "transcribing/" {
		t.Fatalf("while waiting: delivered %q, waiting %v, busy %v, state %s", got, r.c.Waiting(), r.c.Busy(), state(base))
	}
	if !r.c.CancelFrom("", "") {
		t.Fatal("Esc cancelled nothing")
	}
	r.wait(t)
	refine, deliver := refineLines(t, base)
	txt, _ := os.ReadFile(base + ".txt")
	_, wavErr := os.Stat(base + ".wav")
	if r.del.got() != "" || deliver != nil || refine == nil || refine.Why != "cancelled" || refine.LatencyMS < 400 ||
		state(base) != "cancelled/" || string(txt) != "用海库跑一下 Python\n" || wavErr != nil || u.calls.Load() != 1 || r.c.Waiting() {
		t.Fatalf("after Esc: delivered %q, refine %+v, deliver %+v, state %s, txt %q, wav %v, calls %d",
			r.del.got(), refine, deliver, state(base), txt, wavErr, u.calls.Load())
	}
	if !strings.Contains(r.ui.String(), "flash:"+MsgCancelled()) || r.c.Enter() {
		t.Fatalf("ui %s; a Return after the cancel must pass on", r.ui)
	}
}

// Takes behind a waiting one wait their turn, however soon their own answer
// comes. Cancelling the waiting take frees them; its answer frees them too,
// and then both go out in take order. No take is lost or delivered twice.
func TestTakesBehindAWaitingOneWaitTheirTurn(t *testing.T) {
	for _, end := range []string{"cancel", "answer"} {
		r := newRig(t, &fakeSource{n: audio.Rate}, "first", "second")
		r.asr.byTake = map[string]string{"20200417-104200": "first", "20200417-104200-2": "second"}
		asked, answer := make(chan struct{}), make(chan struct{})
		var second atomic.Bool
		r.c.Refine = step(func(ctx context.Context, in RefineInput) (string, *store.Event) {
			if in.Text == "second" {
				second.Store(true)
				return "SECOND", &store.Event{Ev: "refine", Model: "m"}
			}
			close(asked)
			select {
			case <-answer:
				return "FIRST", &store.Event{Ev: "refine", Model: "m"}
			case <-ctx.Done():
				return in.Text, &store.Event{Ev: "refine", Model: "m", Why: "cancelled", Err: "cancelled"}
			}
		})
		r.del.setFront("A")
		r.c.Toggle()
		r.c.Toggle()
		<-asked
		r.del.setFront("B")
		r.c.Toggle()
		r.c.Toggle()
		for !second.Load() {
			time.Sleep(time.Millisecond)
		}
		time.Sleep(50 * time.Millisecond)
		if got := r.del.got(); got != "" {
			t.Fatalf("%s: delivered %q while the first take waits", end, got)
		}
		// the take shown is the one still waiting on the model, which Esc cancels
		r.ui.mu.Lock()
		v := r.ui.views[len(r.ui.views)-1]
		r.ui.mu.Unlock()
		if !v.Refining || v.Live == nil || v.Live() != "first" {
			t.Fatalf("%s: the view while the first take waits: refining %v", end, v.Refining)
		}
		first, next := r.dir+"/20200417-104200", r.dir+"/20200417-104200-2"
		want := "B|SECOND"
		if end == "cancel" {
			if !r.c.CancelFrom("", "") {
				t.Fatal("Esc cancelled nothing")
			}
		} else {
			close(answer)
			want = "A|FIRST;B|SECOND"
		}
		r.wait(t)
		if got := r.del.got(); got != want || state(next) != "pasted/" || (end == "cancel") != (state(first) == "cancelled/") {
			t.Fatalf("%s: delivered %q, want %q; states %s and %s", end, got, want, state(first), state(next))
		}
		for _, base := range []string{first, next} {
			if txt, err := os.ReadFile(base + ".txt"); err != nil || len(txt) == 0 {
				t.Fatalf("%s: %s.txt %q, %v", end, base, txt, err)
			}
		}
	}
}

// A take whose delivered text never depends on the answer does not wait:
// it is delivered as recognised at once, and the call finishes beside it —
// its refine line follows the deliver line, and the overlay then says what
// was only recorded.
func TestRefineAsideDeliversAtOnce(t *testing.T) {
	r, base := rig2020(t, &fakeSource{n: audio.Rate}, "用海库跑一下 派森")
	r.c.Deliver = recordDeliverer{r.del, store.Target{Kind: "herdr", Pane: "w1:p2", Agent: "claude"}}
	answer := make(chan struct{})
	ev := &store.Event{Ev: "refine", Model: "m", LatencyMS: 900, Copy: "raw",
		Edits: []store.RefineEdit{{From: "用海库跑", To: "用Haiku跑", Why: "模型名"}}}
	r.c.Refine = func() *RefineStep {
		return &RefineStep{
			Aside: func(target *store.Target) bool { return target.IsAgent() },
			Run: func(_ context.Context, in RefineInput) (string, *store.Event) {
				<-answer
				return strings.ReplaceAll(in.Text, "海库", "Haiku"), ev
			},
		}
	}
	r.c.Toggle()
	r.c.Toggle()
	deadline := time.Now().Add(2 * time.Second)
	for r.del.got() == "" && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if d := r.del.got(); !strings.HasSuffix(d, "|用海库跑一下 Python") || r.c.Waiting() {
		t.Fatalf("before the answer: delivered %q, waiting %v", d, r.c.Waiting())
	}
	if refine, _ := refineLines(t, base); refine != nil {
		t.Fatalf("a refine line before the answer: %+v", refine)
	}
	close(answer)
	r.wait(t)
	refine, deliver := refineLines(t, base)
	if d := r.del.got(); strings.Count(d, "Python") != 1 || refine == nil || refine.Copy != "raw" || len(refine.Edits) != 1 ||
		deliver == nil || deliver.Text != "" || !strings.HasSuffix(lineNames(t, base), "deliver,refine") || state(base) != "pasted/" {
		t.Fatalf("after the answer: delivered %q, refine %+v, deliver %+v, record %s", d, refine, deliver, lineNames(t, base))
	}
	if note := refineNote(ev); !strings.HasPrefix(note, "仅记录") || !strings.Contains(r.ui.String(), "flash:"+note) {
		t.Fatalf("note %q, ui %s", note, r.ui)
	}
}

// A phone take whose stream broke is held as cut off without the names
// stage or the model being asked: no call to wait on, no line of theirs.
func TestRefineSkipsABrokenPhoneStream(t *testing.T) {
	r, base := rig2020(t, &fakeSource{}, "partial text")
	asked, named := 0, 0
	r.c.Names = func(_ context.Context, text string) (string, *store.Event) { named++; return text, nil }
	r.c.Refine = func() *RefineStep {
		asked++
		return &RefineStep{Run: func(_ context.Context, in RefineInput) (string, *store.Event) {
			return in.Text, &store.Event{Ev: "refine", Model: "m"}
		}}
	}
	sink := &fakeDeliverer{front: "iPhone 键盘"}
	src := &fakeSource{n: audio.Rate, cut: true, cutErr: errors.New("phone stream sequence gap")}
	if _, err := r.c.StartStream(src, sink, "keyboard"); err != nil {
		t.Fatal(err)
	}
	r.wait(t)
	refine, _ := refineLines(t, base)
	if got := state(base); asked != 0 || named != 0 || refine != nil || got != "undelivered/phone stream sequence gap" || sink.got() != "" {
		t.Fatalf("asked %d, named %d, refine %+v, final %q, phone %q", asked, named, refine, got, sink.got())
	}
}

// recordDeliverer captures targets whose record is rec.
type recordDeliverer struct {
	*fakeDeliverer
	rec store.Target
}

type recordTarget struct {
	Target
	rec store.Target
}

func (t recordTarget) Record() store.Target { return t.rec }

func (d recordDeliverer) Capture() Target { return recordTarget{d.fakeDeliverer.Capture(), d.rec} }

// keypadDeliverer captures a keypad take: its Enter sends to a Telegram
// chat, its plain stop pastes at the cursor.
type keypadDeliverer struct{ *fakeDeliverer }

type keypadEnd struct{ Target }

func (t keypadEnd) ForEnd(send bool) Target {
	if send {
		return recordTarget{&target{name: "chat"}, store.Target{Kind: "telegram"}}
	}
	return t.Target
}

func (d keypadDeliverer) Capture() Target { return keypadEnd{d.fakeDeliverer.Capture()} }

// stepFixing is a model step that corrects 海库 to Haiku and hands the
// controller agent_copy as the stage does: on the refine line, and, under
// raw toward an agent's target, as a take that does not wait for the call.
func stepFixing(copy string) func() *RefineStep {
	return func() *RefineStep {
		return &RefineStep{
			Aside: func(target *store.Target) bool { return copy == "raw" && target.IsAgent() },
			Run: func(_ context.Context, in RefineInput) (string, *store.Event) {
				return strings.ReplaceAll(in.Text, "海库", "Haiku"), &store.Event{Ev: "refine", Model: "m", LatencyMS: 900,
					Edits: []store.RefineEdit{{From: "用海库跑", To: "用Haiku跑", Why: "模型名"}}, Copy: copy}
			},
		}
	}
}

// marked is what both adds to the corrected text of 用海库跑一下 ….
func marked(heard string) string { return "\n\n[Mega Voice · as heard]\n" + heard }

// agent_copy chooses what is delivered: refined the corrected text; both,
// to every target — an agent's pane, a Telegram chat, a plain pane, an app,
// a target with no record — the corrected text and the marked changes; raw,
// toward an agent's target, the text as heard, and every other target the
// corrected text. The deliver line keeps the take's text, never the second part, with what was
// sent in its place beside it; the refine line says what went out and keeps
// the edits; under raw the overlay says they were only recorded.
func TestRefineAgentCopies(t *testing.T) {
	const heard, fixed = "用海库跑一下 Python", "用Haiku跑一下 Python"
	for _, tc := range []struct {
		copy, to        string
		rec             *store.Target
		got, text, sent string
		line            string
	}{
		{"refined", "agent pane", &store.Target{Kind: "herdr", Pane: "w1:p2", Agent: "claude"}, fixed, fixed, "", ""},
		{"both", "agent pane", &store.Target{Kind: "herdr", Pane: "w1:p2", Agent: "claude"}, fixed + marked(heard), fixed, fixed + marked(heard), "both"},
		{"raw", "agent pane", &store.Target{Kind: "herdr", Pane: "w1:p2", Agent: "claude"}, heard, "", "", "raw"},
		{"both", "chat", &store.Target{Kind: "telegram"}, fixed + marked(heard), fixed, fixed + marked(heard), "both"},
		{"raw", "chat", &store.Target{Kind: "telegram"}, heard, "", "", "raw"},
		{"both", "shell pane", &store.Target{Kind: "herdr", Pane: "w1:p1"}, fixed + marked(heard), fixed, fixed + marked(heard), "both"},
		{"raw", "shell pane", &store.Target{Kind: "herdr", Pane: "w1:p1"}, fixed, fixed, "", ""},
		{"both", "app", &store.Target{Kind: "app", App: "Notes"}, fixed + marked(heard), fixed, fixed + marked(heard), "both"},
		{"both", "unrecorded", nil, fixed + marked(heard), fixed, fixed + marked(heard), "both"},
		{"refined", "app", &store.Target{Kind: "app", App: "Notes"}, fixed, fixed, "", ""},
		{"raw", "unrecorded", nil, fixed, fixed, "", ""},
	} {
		r := newRig(t, &fakeSource{n: audio.Rate}, "用海库跑一下 派森")
		if tc.rec != nil {
			r.c.Deliver = recordDeliverer{r.del, *tc.rec}
		}
		r.c.Refine = stepFixing(tc.copy)
		r.c.Toggle()
		r.c.Toggle()
		r.wait(t)
		name := tc.copy + " to " + tc.to
		if d := r.del.got(); d != "telegram|"+tc.got { // one delivery, nothing else
			t.Errorf("%s: delivered %q, want %q", name, d, tc.got)
			continue
		}
		refine, deliver := refineLines(t, r.dir+"/20200417-104200")
		if refine == nil || refine.Copy != tc.line || len(refine.Edits) != 1 || deliver == nil || deliver.Text != tc.text ||
			deliver.Sent != tc.sent || deliver.Chars != len([]rune(tc.got)) {
			t.Errorf("%s: refine %+v, deliver %+v", name, refine, deliver)
		}
		if note := refineNote(refine); strings.HasPrefix(note, "海库") == (tc.line == "raw") {
			t.Errorf("%s: overlay note %q", name, note)
		}
	}
}

// A take with a failed chunk keeps no .txt, so a resend and the takes list
// read its text off its deliver line. Delivered with both, that text is the
// corrected text: the marked second part went to the agent and nowhere else.
func TestBothKeepsTheMarkerOffTheTake(t *testing.T) {
	r := newRig(t, &fakeSource{n: 2 * audio.Rate}, "用海库跑一下", "二")
	r.asr.fail = map[int]bool{2: true}
	r.c.NewChunker = func() Chunker { return &everyN{n: audio.Rate} }
	r.c.Deliver = recordDeliverer{r.del, store.Target{Kind: "herdr", Pane: "w1:p2", Agent: "claude"}}
	r.c.Refine = stepFixing("both")
	r.c.Toggle()
	r.c.Toggle()
	r.wait(t)
	base := r.dir + "/20200417-104200"
	if d := r.del.got(); !strings.HasSuffix(d, "|用Haiku跑一下"+marked("用海库跑一下")) {
		t.Fatalf("the agent got %q", d)
	}
	evs, err := store.Read(base)
	if err != nil {
		t.Fatal(err)
	}
	text, _, err := textOf(base, evs, "")
	if err != nil || text != "用Haiku跑一下" {
		t.Fatalf("a resend would paste %q (%v), want the corrected text alone", text, err)
	}
}

// On a keypad take both goes wherever the take ends: its Enter sends the
// both copy to the chat; a plain stop pastes it at the cursor.
func TestRefineBothOnAKeypadTake(t *testing.T) {
	for _, send := range []bool{true, false} {
		r := newRig(t, &fakeSource{n: audio.Rate}, "用海库跑一下 派森")
		r.c.Deliver = keypadDeliverer{r.del}
		r.c.Refine = stepFixing("both")
		r.c.Toggle()
		if send {
			r.c.Enter()
		} else {
			r.c.Toggle()
		}
		r.wait(t)
		want := "telegram|用Haiku跑一下 Python" + marked("用海库跑一下 Python")
		if send {
			want = "chat|用Haiku跑一下 Python" + marked("用海库跑一下 Python")
		}
		if d := r.del.got(); !strings.HasPrefix(d, want) {
			t.Errorf("send %v: delivered %q, want %q", send, d, want)
		}
	}
}

// A take's screen is asked for as it starts, handed to its model step, and
// the picture the step sent is kept beside the record under the name the
// refine line gives; a take without the grant says so on the overlay.
func TestTakeCarriesItsScreen(t *testing.T) {
	r := newRig(t, &fakeSource{n: audio.Rate}, "用海库跑一下")
	shot := &refine.Screen{Mode: refine.ScreenImage, Image: &refine.Image{MediaType: "image/jpeg", Data: []byte("jpeg bytes")}}
	started := 0
	r.c.Screen = func() func(context.Context) *refine.Screen {
		started++
		return func(context.Context) *refine.Screen { return shot }
	}
	var got *refine.Screen
	r.c.Refine = step(func(_ context.Context, in RefineInput) (string, *store.Event) {
		got = in.Screen
		return in.Text, &store.Event{Ev: "refine", Model: "m", Screen: &store.Screen{Mode: "image", SHA256: "abc", Bytes: 10}}
	})
	r.c.Toggle()
	if started != 1 {
		t.Fatalf("the screen was asked for %d times as the take started", started)
	}
	r.c.Toggle()
	r.wait(t)
	base := r.dir + "/20200417-104200"
	line, _ := refineLines(t, base)
	kept, err := os.ReadFile(base + ".screen.jpg")
	if got != shot || line == nil || line.Screen == nil || line.Screen.Image != "20200417-104200.screen.jpg" || err != nil || string(kept) != "jpeg bytes" {
		t.Fatalf("step got %+v, refine line %+v, kept %q (%v)", got, line, kept, err)
	}

	r = newRig(t, &fakeSource{n: audio.Rate}, "用海库跑一下")
	denied := &store.Event{Ev: "refine", Model: "m", Screen: &store.Screen{Mode: "ocr", Withheld: refine.WithheldPermission}}
	r.c.Screen = func() func(context.Context) *refine.Screen {
		return func(context.Context) *refine.Screen {
			return &refine.Screen{Mode: refine.ScreenOCR, Withheld: refine.WithheldPermission}
		}
	}
	r.c.Refine = step(func(_ context.Context, in RefineInput) (string, *store.Event) { return in.Text, denied })
	r.c.Toggle()
	r.c.Toggle()
	r.wait(t)
	if note := refineNote(denied); !strings.Contains(note, "屏幕") && !strings.Contains(note, "Screen") || !strings.Contains(r.ui.String(), "flash:"+note) {
		t.Fatalf("no grant: note %q, ui %s", note, r.ui)
	}
	if _, err := os.Stat(r.dir + "/20200417-104200.screen.jpg"); err == nil {
		t.Error("a picture was kept for a take that sent none")
	}
}

// A take spoken on the phone is not made at the Mac's screen: with the
// screen source on it captures nothing, and its record says nothing of a
// screen.
func TestAPhoneTakeCapturesNoScreen(t *testing.T) {
	r, base := rig2020(t, &fakeSource{n: audio.Rate}, "用海库跑一下")
	started := 0
	r.c.Screen = func() func(context.Context) *refine.Screen {
		started++
		return func(context.Context) *refine.Screen {
			return &refine.Screen{Mode: refine.ScreenOCR, App: "com.example.term", Text: "the Mac's front window"}
		}
	}
	var got *refine.Screen
	r.c.Refine = step(func(_ context.Context, in RefineInput) (string, *store.Event) {
		got = in.Screen
		return in.Text, &store.Event{Ev: "refine", Model: "m"}
	})
	sink := &fakeDeliverer{front: "iPhone 键盘"}
	id, err := r.c.StartStream(&fakeSource{n: audio.Rate}, sink, "keyboard")
	if err != nil {
		t.Fatal(err)
	}
	if !r.c.StopFrom("keyboard", id) {
		t.Fatal("stop failed")
	}
	r.wait(t)
	line, _ := refineLines(t, base)
	if started != 0 || got != nil || line == nil || line.Screen != nil || line.Context != "" {
		t.Fatalf("screen asked for %d times, the step got %+v, refine line %+v", started, got, line)
	}
	if _, err := os.Stat(base + ".screen.jpg"); err == nil {
		t.Error("a picture was kept for a phone take")
	}
}

// fixedStep is a model step that answers with text in mode and counts its
// calls.
func fixedStep(mode, text string, calls *atomic.Int32) *RefineStep {
	return &RefineStep{Run: func(_ context.Context, in RefineInput) (string, *store.Event) {
		calls.Add(1)
		return text, &store.Event{Ev: "refine", Mode: mode, Model: "m", LatencyMS: 300, Copy: "both"}
	}}
}

// A take cancelled while it waited on the model is run again from the
// takes page: one call, its refine line numbered after the take's own and
// carrying the model's text, nothing delivered and the take still
// cancelled. Sent, the model's text goes out once; a second send, and a
// run again on the take now delivered, are refused and append nothing.
func TestCancelledTakeRunAgainThenSent(t *testing.T) {
	r, base := rig2020(t, &fakeSource{n: audio.Rate}, "用海库跑一下 派森")
	u := newUnanswered()
	r.c.Refine = step(u.run)
	r.c.Toggle()
	r.c.Toggle()
	<-u.asked
	r.c.CancelFrom("", "")
	r.wait(t)
	if state(base) != "cancelled/" {
		t.Fatalf("state %s", state(base))
	}
	var calls atomic.Int32
	line, err := r.c.Rerefine(context.Background(), "20200417-104200", fixedStep("tidy", "用 Haiku 跑一下 Python。", &calls), "page")
	if err != nil || calls.Load() != 1 || line.N != 2 || line.Via != "page" || line.Mode != "tidy" || line.Text != "用 Haiku 跑一下 Python。" ||
		line.Copy != "" || r.del.got() != "" || state(base) != "cancelled/" {
		t.Fatalf("run again: %v, %d calls, line %+v, delivered %q, state %s", err, calls.Load(), line, r.del.got(), state(base))
	}
	de, err := r.c.Resend(Resend{ID: "20200417-104200", To: r.del.Capture(), Text: "model", Once: true, Via: "page"})
	if err != nil || !de.OK || de.Source != "model" || de.Text != "用 Haiku 跑一下 Python。" || r.del.got() != "telegram|用 Haiku 跑一下 Python。" ||
		state(base) != "pasted/" {
		t.Fatalf("send: %v, line %+v, delivered %q, state %s", err, de, r.del.got(), state(base))
	}
	lines := lineNames(t, base)
	if _, err := r.c.Resend(Resend{ID: "20200417-104200", To: r.del.Capture(), Text: "model", Once: true, Via: "page"}); !errors.Is(err, ErrDelivered) {
		t.Fatalf("a second send: %v", err)
	}
	if _, err := r.c.Rerefine(context.Background(), "20200417-104200", fixedStep("tidy", "x", &calls), "page"); !errors.Is(err, ErrDelivered) || calls.Load() != 1 {
		t.Fatalf("a run again on a delivered take: %v, %d calls", err, calls.Load())
	}
	if r.del.got() != "telegram|用 Haiku 跑一下 Python。" || lineNames(t, base) != lines {
		t.Fatalf("delivered %q, record %s, was %s", r.del.got(), lineNames(t, base), lines)
	}
}

// A take held because the call gave no answer to use, sent as recognised,
// delivers its text as recognised, once: a second send is refused.
func TestHeldTakeSentAsRecognised(t *testing.T) {
	r, base := rig2020(t, &fakeSource{n: audio.Rate}, "用海库跑一下 派森")
	r.c.Refine = step(func(_ context.Context, in RefineInput) (string, *store.Event) {
		return in.Text, &store.Event{Ev: "refine", Model: "m", Why: "http", Err: "status 529"}
	})
	r.c.Toggle()
	r.c.Toggle()
	r.wait(t)
	if state(base) != "undelivered/refine_failed" {
		t.Fatalf("state %s", state(base))
	}
	if _, err := r.c.Resend(Resend{ID: "20200417-104200", To: r.del.Capture(), Text: "model", Once: true}); !errors.Is(err, ErrNoText) {
		t.Fatalf("a model text with none: %v", err)
	}
	de, err := r.c.Resend(Resend{ID: "20200417-104200", To: r.del.Capture(), Text: "delivered", Once: true, Via: "page"})
	if err != nil || !de.OK || de.Text != "" || r.del.got() != "telegram|用海库跑一下 Python" || state(base) != "pasted/" {
		t.Fatalf("send: %v, line %+v, delivered %q, state %s", err, de, r.del.got(), state(base))
	}
	if _, err := r.c.Resend(Resend{ID: "20200417-104200", To: r.del.Capture(), Text: "delivered", Once: true}); !errors.Is(err, ErrDelivered) || r.del.got() != "telegram|用海库跑一下 Python" {
		t.Fatalf("a second send: %v, delivered %q", err, r.del.got())
	}
}

// A model text sent toward an agent's target follows refine.agent_copy as a
// live take does: both sends the model's text and the recognised one under
// its mark, leaving the model's text alone on the clipboard, and the deliver
// line keeps the model's text as the take's.
func TestSentModelTextFollowsAgentCopy(t *testing.T) {
	const heard, fixed = "用海库跑一下 Python", "用Haiku跑一下 Python"
	agent := &store.Target{Kind: "herdr", Pane: "w1:p2", Agent: "claude"}
	for _, tc := range []struct{ copy, got, sent string }{
		{"both", fixed + marked(heard), fixed + marked(heard)},
		{"refined", fixed, ""},
		{"raw", heard, ""},
	} {
		r, _ := rig2020(t, &fakeSource{n: audio.Rate}, "用海库跑一下 派森")
		r.c.Toggle()
		r.c.Cancel()
		r.wait(t)
		var calls atomic.Int32
		if _, err := r.c.Rerefine(context.Background(), "20200417-104200", fixedStep("fix-words", fixed, &calls), "page"); err != nil {
			t.Fatal(err)
		}
		var clips []string
		r.c.Deliver = clipDeliverer{recordDeliverer{r.del, *agent}, &clips}
		de, err := r.c.Resend(Resend{ID: "20200417-104200", To: r.del.Capture(), Text: "model", Once: true, Copy: tc.copy, For: agent})
		if err != nil || r.del.got() != "telegram|"+tc.got || de.Sent != tc.sent || de.Chars != len([]rune(tc.got)) {
			t.Errorf("%s: %v, delivered %q, line %+v", tc.copy, err, r.del.got(), de)
		}
		if wantClips := map[string][]string{"both": {fixed}}[tc.copy]; !slices.Equal(clips, wantClips) {
			t.Errorf("%s: clipboard copies %q, want %q", tc.copy, clips, wantClips)
		}
	}
}

// clipDeliverer is a recordDeliverer that takes a clipboard copy apart from
// what it delivers, and notes each.
type clipDeliverer struct {
	recordDeliverer
	clips *[]string
}

func (d clipDeliverer) DeliverClip(t Target, text, clip string) error {
	*d.clips = append(*d.clips, clip)
	return d.fakeDeliverer.Deliver(t, text)
}

// Under both the agent's pane gets both texts while the clipboard is left
// the take's text, what a Cmd-V or a resend pastes; refined asks for no copy
// apart.
func TestBothLeavesTheTakeTextOnTheClipboard(t *testing.T) {
	for _, copy := range []string{"both", "refined"} {
		r := newRig(t, &fakeSource{n: audio.Rate}, "用海库跑一下 派森")
		var clips []string
		r.c.Deliver = clipDeliverer{recordDeliverer{r.del, store.Target{Kind: "herdr", Pane: "w1:p2", Agent: "claude"}}, &clips}
		r.c.Refine = stepFixing(copy)
		r.c.Toggle()
		r.c.Toggle()
		r.wait(t)
		const fixed = "用Haiku跑一下 Python"
		want, wantClips := "telegram|"+fixed, []string(nil)
		if copy == "both" {
			want, wantClips = "telegram|"+fixed+marked("用海库跑一下 Python"), []string{fixed}
		}
		if d := r.del.got(); d != want {
			t.Errorf("%s: delivered %q, want %q", copy, d, want)
		}
		if !slices.Equal(clips, wantClips) {
			t.Errorf("%s: clipboard copies %q, want %q", copy, clips, wantClips)
		}
	}
}

// A send whose paste was cut and most likely landed (ErrMaybeSent: hold
// delivery_cut) counts as delivered for a send from the page: a second
// send, and a run again, are refused and nothing goes out again.
func TestMaybeSentTakeIsNotSentAgain(t *testing.T) {
	r, base := rig2020(t, &fakeSource{n: audio.Rate}, "用海库跑一下 派森")
	r.c.Toggle()
	r.c.Cancel()
	r.wait(t)
	r.del.mu.Lock()
	r.del.deliverErr = ErrMaybeSent
	r.del.mu.Unlock()
	de, err := r.c.Resend(Resend{ID: "20200417-104200", To: r.del.Capture(), Text: "delivered", Once: true, Via: "page"})
	if err != nil || de.OK || state(base) != "undelivered/delivery_cut" {
		t.Fatalf("a cut send: %v, line %+v, state %s", err, de, state(base))
	}
	r.del.mu.Lock()
	r.del.deliverErr = nil
	r.del.mu.Unlock()
	lines := lineNames(t, base)
	if _, err := r.c.Resend(Resend{ID: "20200417-104200", To: r.del.Capture(), Text: "delivered", Once: true, Via: "page"}); !errors.Is(err, ErrDelivered) {
		t.Fatalf("a second send after a cut one: %v", err)
	}
	var calls atomic.Int32
	if _, err := r.c.Rerefine(context.Background(), "20200417-104200", fixedStep("tidy", "x", &calls), "page"); !errors.Is(err, ErrDelivered) || calls.Load() != 0 {
		t.Fatalf("a run again after a cut send: %v, %d calls", err, calls.Load())
	}
	if r.del.got() != "" || lineNames(t, base) != lines {
		t.Fatalf("delivered %q, record %s, was %s", r.del.got(), lineNames(t, base), lines)
	}
}

// A run again whose call never answers holds up no exit: the controller is
// not Busy, Close answers true and cancels the call, and its line says
// cancelled.
func TestStalledRunAgainDoesNotBlockClose(t *testing.T) {
	r, base := rig2020(t, &fakeSource{n: audio.Rate}, "用海库跑一下 派森")
	r.c.Toggle()
	r.c.Cancel()
	r.wait(t)
	u := newUnanswered()
	done := make(chan store.Event, 1)
	go func() {
		line, _ := r.c.Rerefine(context.Background(), "20200417-104200", &RefineStep{Run: u.run}, "page")
		done <- line
	}()
	<-u.asked
	if r.c.Busy() {
		t.Fatal("Busy while a run again waits on its call")
	}
	if !r.c.Close("quit") {
		t.Fatal("Close refused while a run again waits on its call")
	}
	select {
	case line := <-done:
		if line.Why != "cancelled" || state(base) != "cancelled/" || r.del.got() != "" {
			t.Fatalf("line %+v, state %s, delivered %q", line, state(base), r.del.got())
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the run again outlived Close")
	}
}
