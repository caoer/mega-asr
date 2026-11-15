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

// A phone take whose stream broke is held as cut off without the model
// being asked: no call to wait on, no refine line.
func TestRefineSkipsABrokenPhoneStream(t *testing.T) {
	r, base := rig2020(t, &fakeSource{}, "partial text")
	asked := 0
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
	if got := state(base); asked != 0 || refine != nil || got != "undelivered/phone stream sequence gap" || sink.got() != "" {
		t.Fatalf("asked %d, refine %+v, final %q, phone %q", asked, refine, got, sink.got())
	}
}

// What the step gives an agent's target is what the take delivers, whole and
// once: under both the corrected text with its marked second part, which the
// deliver line keeps; under raw the text as recognised, with no text on the
// deliver line, while the refine line keeps the edits and the overlay says
// they were only recorded.
func TestRefineAgentCopies(t *testing.T) {
	edits := []store.RefineEdit{{From: "用海库跑", To: "用Haiku跑", Why: "模型名"}}
	for _, tc := range []struct {
		copy, text, deliverText string
	}{
		{"both", "用Haiku跑一下 Python\n\n[Mega Voice · heard → corrected] 海库 → Haiku", "用Haiku跑一下 Python\n\n[Mega Voice · heard → corrected] 海库 → Haiku"},
		{"raw", "用海库跑一下 Python", ""},
	} {
		r := newRig(t, &fakeSource{n: audio.Rate}, "用海库跑一下 派森")
		r.c.Deliver = paneDeliverer{r.del}
		ev := &store.Event{Ev: "refine", Model: "m", LatencyMS: 900, Edits: edits, Copy: tc.copy}
		r.c.Refine = func() func(context.Context, RefineInput) (string, *store.Event) {
			return func(context.Context, RefineInput) (string, *store.Event) { return tc.text, ev }
		}
		r.c.Toggle()
		r.c.Toggle()
		r.wait(t)
		if d := r.del.got(); !strings.HasSuffix(d, "|"+tc.text) || strings.Count(d, "Python") != 1 {
			t.Fatalf("%s: delivered %q", tc.copy, d)
		}
		refine, deliver := refineLines(t, r.dir+"/20200417-104200")
		if refine == nil || refine.Copy != tc.copy || len(refine.Edits) != 1 || deliver == nil || deliver.Text != tc.deliverText {
			t.Fatalf("%s: refine %+v, deliver %+v", tc.copy, refine, deliver)
		}
		note := refineNote(ev)
		if !strings.Contains(r.ui.String(), "flash:"+note) || strings.HasPrefix(note, "海库") == (tc.copy == "raw") {
			t.Fatalf("%s: note %q, ui %s", tc.copy, note, r.ui)
		}
	}
}
