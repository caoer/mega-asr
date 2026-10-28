package session

import (
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/caoer/mega-asr/internal/audio"
	"github.com/caoer/mega-asr/internal/store"
)

func TestHotkeyTakeSkipsPhoneFailuresAndCancelledFallback(t *testing.T) {
	for _, outcome := range []string{"source_failed", "sink_failed", "cancelled", "cancelled_silent"} {
		t.Run(outcome, func(t *testing.T) {
			r := newRig(t, &fakeSource{}, "phone text")
			const macID = "20200417-103800"
			fixtureTake(t, r.dir, macID, r.c.now(), 4*time.Minute, time.Second, "Mac text", failed)
			sink := &fakeDeliverer{front: "iPhone 键盘"}
			src := &described{fakeSource: &fakeSource{n: audio.Rate}, in: audio.TakeInput{Source: "phone", Name: "fixture phone"}}
			if outcome == "source_failed" {
				src.cut, src.cutErr = true, errors.New("phone stream gap")
			}
			if outcome == "sink_failed" {
				sink.deliverErr = errors.New("phone gone")
			}
			if outcome == "cancelled_silent" {
				r.asr.texts = []string{""}
			}
			id, err := r.c.StartStream(src, sink, "keyboard")
			if err != nil {
				t.Fatal(err)
			}
			if outcome == "sink_failed" {
				r.c.StopFrom("keyboard", id)
			}
			if outcome == "cancelled" || outcome == "cancelled_silent" {
				r.c.CancelFrom("keyboard", id)
			}
			r.wait(t)
			if (outcome == "cancelled" || outcome == "cancelled_silent") && strings.Contains(r.ui.String(), "右Option+Esc 重贴") {
				t.Fatal("phone cancellation advertised the Mac resend hotkey")
			}
			if got, err := r.c.HotkeyTake(r.c.now()); got != macID || err != nil {
				t.Fatalf("selected %q: %v", got, err)
			}
			// Persisted input must preserve that selection on a fresh controller too.
			fresh := &Controller{Store: r.c.Store}
			if got, err := fresh.HotkeyTake(r.c.now()); got != macID || err != nil {
				t.Fatalf("fresh selected %q: %v", got, err)
			}
			if _, err := r.c.Base(id); err != nil {
				t.Fatalf("phone history removed: %v", err)
			}
			// With no Mac failure, a cancelled phone must not become the fallback.
			if err := store.Append(filepath.Join(r.dir, macID), store.Event{Ev: "dismiss"}); err != nil {
				t.Fatal(err)
			}
			if got, err := fresh.HotkeyTake(r.c.now()); got != "" || err != nil {
				t.Fatalf("phone-only selection %q: %v", got, err)
			}
			if outcome == "source_failed" {
				// An explicit history choice remains available despite the implicit filter.
				ev, err := r.c.Resend(Resend{ID: id, To: r.del.Capture(), Via: "page"})
				if err != nil || !ev.OK || r.del.got() != "telegram|phone text" {
					t.Fatalf("explicit phone history resend: %+v %v, %q", ev, err, r.del.got())
				}
				r.wait(t)
			}
		})
	}
}
