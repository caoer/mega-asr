//go:build darwin

package main

import (
	"path/filepath"
	"testing"
	"time"

	"github.com/caoer/mega-asr/internal/audio"
	"github.com/caoer/mega-asr/internal/store"
)

// A resend without an id takes the newest undelivered take the user did not
// dismiss.
func TestNewestUndeliveredSkipsDismissed(t *testing.T) {
	dir := t.TempDir()
	now := time.Date(2020, 4, 17, 10, 42, 0, 0, time.Local)
	for _, tk := range []struct {
		name      string
		dismissed bool
	}{{"20200417-103800", false}, {"20200417-104000", true}} {
		at, _ := store.NameTime(tk.name)
		evs := []store.Event{{Ev: "start"}, {Ev: "stop", Kind: "tap"}, {Ev: "hold", Why: "deliver_failed"}}
		if tk.dismissed {
			evs = append(evs, store.Event{Ev: "dismiss"})
		}
		for _, e := range evs {
			e.At = at
			if err := store.Append(filepath.Join(dir, tk.name), e); err != nil {
				t.Fatal(err)
			}
		}
	}
	if id, err := newestUndelivered(dir, now); id != "20200417-103800" || err != nil {
		t.Fatalf("%q, %v; want the older take", id, err)
	}
}

func TestImplicitResendSkipsPhoneAndUncertainTakes(t *testing.T) {
	s := store.Store{Dir: t.TempDir()}
	for _, tk := range []struct{ name, source, why string }{
		{"20200417-103800", "local", "deliver_failed"},
		{"20200417-104000", "phone", "deliver_failed"},
		{"20200417-104100", "local", "delivery_cut"},
	} {
		base := filepath.Join(s.Dir, tk.name)
		at, _ := store.NameTime(tk.name)
		for _, e := range []store.Event{{Ev: "start", At: at}, {Ev: "hold", At: at, Why: tk.why}} {
			if err := store.Append(base, e); err != nil {
				t.Fatal(err)
			}
		}
		if err := s.SaveInput(base, audio.TakeInput{Source: tk.source}); err != nil {
			t.Fatal(err)
		}
	}
	if id, err := newestUndelivered(s.Dir, time.Date(2020, 4, 17, 10, 42, 0, 0, time.Local)); err != nil || id != "20200417-103800" {
		t.Fatalf("implicit resend %q %v", id, err)
	}
}
