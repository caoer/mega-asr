package session

import (
	"context"
	"errors"
	"os"
	"strings"
	"testing"

	"github.com/caoer/mega-asr/internal/audio"
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

// The model step runs after Post on a dictated take, with the take's text
// and where it goes: the delivered text is its, the .txt keeps the text
// before it, and its refine line and the deliver line's text are in the
// record. While it runs the view says so and shows the text as recognised;
// once the text is out the overlay flashes what changed.
func TestRefineCorrectsTheDeliveredText(t *testing.T) {
	r := newRig(t, &fakeSource{n: audio.Rate}, "用海库跑一下 派森")
	r.c.Deliver = paneDeliverer{r.del}
	var got RefineInput
	r.c.Refine = func() func(context.Context, RefineInput) (string, *store.Event) {
		return func(_ context.Context, in RefineInput) (string, *store.Event) {
			got = in
			return strings.ReplaceAll(in.Text, "海库", "Haiku"), &store.Event{Ev: "refine", Model: "m", LatencyMS: 4200,
				Edits: []store.RefineEdit{{From: "用海库跑", To: "用Haiku跑", Why: "模型名"}}}
		}
	}
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

// A step that is off leaves the take's path as it is: the same views, the
// same text, no refine line. A step that fails delivers the text as it
// stood, its refine line says why, and the overlay says so.
func TestRefineOffAndFailed(t *testing.T) {
	r := newRig(t, &fakeSource{n: audio.Rate}, "用海库跑一下 派森")
	asked := 0
	r.c.Refine = func() func(context.Context, RefineInput) (string, *store.Event) { asked++; return nil }
	r.c.Toggle()
	r.c.Toggle()
	r.wait(t)
	base := r.dir + "/20200417-104200"
	refine, deliver := refineLines(t, base)
	if got := r.del.got(); got != "telegram|用海库跑一下 Python" || asked != 1 || refine != nil || deliver == nil || deliver.Text != "" ||
		r.ui.String() != "recording,transcribing,hide" {
		t.Fatalf("off: delivered %q, asked %d, refine %+v, deliver %+v, ui %s", got, asked, refine, deliver, r.ui)
	}

	r = newRig(t, &fakeSource{n: audio.Rate}, "用海库跑一下 派森")
	failed := &store.Event{Ev: "refine", Model: "m", LatencyMS: 10000, Why: "timeout", Err: "no answer within 10s"}
	r.c.Refine = func() func(context.Context, RefineInput) (string, *store.Event) {
		return func(_ context.Context, in RefineInput) (string, *store.Event) { return in.Text, failed }
	}
	r.c.Toggle()
	r.c.Toggle()
	r.wait(t)
	refine, deliver = refineLines(t, r.dir+"/20200417-104200")
	if got := r.del.got(); got != "telegram|用海库跑一下 Python" || refine == nil || refine.Why != "timeout" || refine.Err == "" ||
		deliver == nil || deliver.Text != "" || !strings.Contains(r.ui.String(), "flash:"+refineNote(failed)) || refineNote(failed) == "" {
		t.Fatalf("failed: delivered %q, refine %+v, deliver %+v, ui %s", got, refine, deliver, r.ui)
	}
}

// A phone take whose stream broke is held as cut off without the names
// stage or the model being asked: no call to wait on, no line of theirs.
func TestRefineSkipsABrokenPhoneStream(t *testing.T) {
	r, base := rig2020(t, &fakeSource{}, "partial text")
	asked, named := 0, 0
	r.c.Names = func(_ context.Context, text string) (string, *store.Event) { named++; return text, nil }
	r.c.Refine = func() func(context.Context, RefineInput) (string, *store.Event) {
		asked++
		return func(_ context.Context, in RefineInput) (string, *store.Event) {
			return in.Text, &store.Event{Ev: "refine", Model: "m"}
		}
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
// controller agent_copy as the stage does.
func stepFixing(copy string) func() func(context.Context, RefineInput) (string, *store.Event) {
	return func() func(context.Context, RefineInput) (string, *store.Event) {
		return func(_ context.Context, in RefineInput) (string, *store.Event) {
			return strings.ReplaceAll(in.Text, "海库", "Haiku"), &store.Event{Ev: "refine", Model: "m", LatencyMS: 900,
				Edits: []store.RefineEdit{{From: "用海库跑", To: "用Haiku跑", Why: "模型名"}}, Copy: copy}
		}
	}
}

// marked is what both adds to the corrected text of 用海库跑一下 ….
func marked(heard string) string { return "\n\n[Mega Voice · as heard]\n" + heard }

// Toward an agent's target agent_copy chooses what is delivered — refined
// the corrected text, both the corrected text and the marked changes, raw
// the text as heard — and every other target gets the corrected text. The
// deliver line keeps the take's text, never the second part, with what was
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
		{"both", "shell pane", &store.Target{Kind: "herdr", Pane: "w1:p1"}, fixed, fixed, "", ""},
		{"raw", "shell pane", &store.Target{Kind: "herdr", Pane: "w1:p1"}, fixed, fixed, "", ""},
		{"both", "app", &store.Target{Kind: "app", App: "Notes"}, fixed, fixed, "", ""},
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

// On a keypad take the copy follows where the take ends: its Enter sends the
// both copy to the chat; a plain stop pastes the corrected text alone at the
// cursor.
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
		want := "telegram|用Haiku跑一下 Python"
		if send {
			want = "chat|用Haiku跑一下 Python" + marked("用海库跑一下 Python")
		}
		if d := r.del.got(); !strings.HasPrefix(d, want) {
			t.Errorf("send %v: delivered %q, want %q", send, d, want)
		}
	}
}
