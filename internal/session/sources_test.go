package session

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/caoer/mega-asr/internal/audio"
	"github.com/caoer/mega-asr/internal/refine"
	"github.com/caoer/mega-asr/internal/store"
)

// The text sources are asked for as the take starts and read at its stop
// from the target it is delivered to, before the step runs; what they read
// goes to the step. A take delivered at once (agent_copy raw) reads them
// before its delivery.
func TestTakeReadsItsTextSourcesFromItsTarget(t *testing.T) {
	for _, aside := range []bool{false, true} {
		r := newRig(t, &fakeSource{n: audio.Rate}, "用海库跑一下")
		r.c.Deliver = recordDeliverer{r.del, store.Target{Kind: "herdr", Pane: "w1:p2", Agent: "claude"}}
		parts := []refine.Part{{Source: refine.SourcePane, Text: "Last lines of the terminal pane being dictated into:\n$ go test"}}
		started := 0
		var readFrom Target
		var deliveredAtRead string
		r.c.Sources = func() func(context.Context, Target) []refine.Part {
			started++
			return func(_ context.Context, t Target) []refine.Part {
				readFrom, deliveredAtRead = t, r.del.got()
				return parts
			}
		}
		var got []refine.Part
		done := make(chan struct{})
		r.c.Refine = func() *RefineStep {
			return &RefineStep{
				Aside: func(*store.Target) bool { return aside },
				Run: func(_ context.Context, in RefineInput) (string, *store.Event) {
					defer close(done)
					got = in.Parts
					return in.Text, &store.Event{Ev: "refine", Model: "m"}
				},
			}
		}
		r.c.Toggle()
		if started != 1 {
			t.Fatalf("aside %v: the sources were asked for %d times as the take started", aside, started)
		}
		r.c.Toggle()
		select {
		case <-done:
		case <-time.After(2 * time.Second):
			t.Fatalf("aside %v: the step never ran", aside)
		}
		r.wait(t)
		rt, ok := readFrom.(recordTarget)
		if !ok || rt.rec.Pane != "w1:p2" || len(got) != 1 || got[0].Text != parts[0].Text {
			t.Errorf("aside %v: read from %v, the step got %+v", aside, readFrom, got)
		}
		if deliveredAtRead != "" || !strings.HasSuffix(r.del.got(), "用海库跑一下") {
			t.Errorf("aside %v: delivered %q before the sources were read, %q after", aside, deliveredAtRead, r.del.got())
		}
	}
}

// The sources are read as the take stops, while it is still transcribed,
// not once its text is ready: a copy made or a field typed into after the
// stop is not the take's. A keypad take stopped by Enter reads them from
// the Telegram chat it is sent to, a plain stop from the cursor.
func TestTheSourcesAreReadAsTheTakeStops(t *testing.T) {
	for _, enter := range []bool{false, true} {
		r := newRig(t, &fakeSource{n: audio.Rate}, "用海库跑一下")
		r.c.Deliver = keypadDeliverer{r.del}
		r.asr.block = make(chan struct{})
		read := make(chan string, 1) // the kind of target read from
		r.c.Sources = func() func(context.Context, Target) []refine.Part {
			return func(_ context.Context, t Target) []refine.Part {
				kind := "cursor"
				if rec := StoreTarget(t); rec != nil {
					kind = rec.Kind
				}
				read <- kind
				return []refine.Part{{Source: refine.SourceClipboard, Text: "copied before the stop"}}
			}
		}
		var got []refine.Part
		r.c.Refine = func() *RefineStep {
			return &RefineStep{Run: func(_ context.Context, in RefineInput) (string, *store.Event) {
				got = in.Parts
				return in.Text, &store.Event{Ev: "refine", Model: "m"}
			}}
		}
		r.c.Toggle()
		if enter {
			r.c.Enter()
		} else {
			r.c.Toggle()
		}
		select {
		case kind := <-read:
			if (kind == "telegram") != enter {
				t.Errorf("enter %v: read from a %s target", enter, kind)
			}
		case <-time.After(2 * time.Second):
			t.Fatalf("enter %v: the sources were not read while the take transcribed", enter)
		}
		close(r.asr.block)
		r.wait(t)
		if len(got) != 1 || got[0].Text != "copied before the stop" {
			t.Errorf("enter %v: the step got %+v", enter, got)
		}
	}
}
