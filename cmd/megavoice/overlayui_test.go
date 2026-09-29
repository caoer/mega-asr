//go:build darwin

package main

import (
	"testing"
	"time"

	"github.com/caoer/mega-asr/internal/audio"
	"github.com/caoer/mega-asr/internal/mac"
	"github.com/caoer/mega-asr/internal/session"
)

// fakeOverlay keeps what the overlay shows: the label, "" once hidden, and
// whether a waveform is drawn under it.
type fakeOverlay struct {
	label string
	wave  bool
}

func (o *fakeOverlay) Show(label string, w mac.Wave)             { o.label, o.wave = label, w.Quality != nil }
func (o *fakeOverlay) Stopwatch(time.Time, func() time.Duration) {}
func (o *fakeOverlay) Hide()                                     { o.label, o.wave = "", false }

// fakeClock hands overlayUI timers that fire only when the test says.
type fakeClock struct {
	d []time.Duration
	f []func()
}

func (c *fakeClock) after(d time.Duration, f func()) *time.Timer {
	c.d, c.f = append(c.d, d), append(c.f, f)
	t := time.NewTimer(time.Hour)
	t.Stop()
	return t
}

// expire fires the newest timer, as the time of the message it ends runs out,
// and reports how long that message was meant to last.
func (c *fakeClock) expire(t *testing.T) time.Duration {
	t.Helper()
	if len(c.f) == 0 {
		t.Fatal("no timer running")
	}
	d, f := c.d[len(c.d)-1], c.f[len(c.f)-1]
	f()
	return d
}

func newTestUI() (*overlayUI, *fakeOverlay, *fakeClock) {
	ov, clk := &fakeOverlay{}, &fakeClock{}
	return &overlayUI{ov: ov, after: clk.after}, ov, clk
}

// A record line that cannot be written alerts, and the take's outcome flashes
// in the same call: the alert keeps the label for its 6 s, then the outcome
// has its 1.5 s.
func TestOverlayAlertOutlastsTheOutcome(t *testing.T) {
	u, ov, clk := newTestUI()
	u.Alert(session.MsgRecordFailed)
	u.Flash(session.MsgEmpty)
	if ov.label != session.MsgRecordFailed {
		t.Fatalf("label %q, want the alert %q", ov.label, session.MsgRecordFailed)
	}
	if d := clk.expire(t); d != 6*time.Second || ov.label != session.MsgEmpty {
		t.Fatalf("alert lasted %v, then label %q; want 6s, then %q", d, ov.label, session.MsgEmpty)
	}
	if d := clk.expire(t); d != 1500*time.Millisecond || ov.label != "" {
		t.Fatalf("flash lasted %v, then label %q; want 1.5s, then hidden", d, ov.label)
	}
}

// A take's failure stays up while the next take records under it: its
// waveform moves, its device note and a cancel's flash wait; the newest of
// them is shown once the alert is over.
func TestOverlayFlashWaitsForAlert(t *testing.T) {
	u, ov, clk := newTestUI()
	u.Alert(session.MsgUndelivered)
	u.Show(session.View{Started: time.Unix(1577836800, 0), Quality: func() []audio.Quality { return nil }})
	u.Flash("Podium Condenser not connected — using Built-in Microphone")
	u.Flash("已取消 0:04")
	if ov.label != session.MsgUndelivered || !ov.wave {
		t.Fatalf("label %q, waveform %v; want the alert over the moving waveform", ov.label, ov.wave)
	}
	u.Show(session.View{})
	if clk.expire(t); ov.label != "已取消 0:04" {
		t.Fatalf("after the alert: label %q, want the cancel's flash", ov.label)
	}
}

// A second failure waits its turn and is shown once, however often it comes.
func TestOverlayAlertsTakeTurns(t *testing.T) {
	u, ov, clk := newTestUI()
	u.Alert(session.MsgRecordFailed)
	u.Alert(session.MsgASRFailed)
	u.Alert(session.MsgRecordFailed)
	u.Alert(session.MsgASRFailed)
	for _, want := range []string{session.MsgRecordFailed, session.MsgASRFailed, ""} {
		if ov.label != want {
			t.Fatalf("label %q, want %q", ov.label, want)
		}
		if want != "" {
			clk.expire(t)
		}
	}
}

// The no-signal notice is the recording view's label: an alert lies over it
// for its 6 s and a flash for its 1.5 s, and it is back once they end, for as
// long as the input is silent; a live block brings back the recording label.
func TestOverlayNoSignalLastsWhileSilent(t *testing.T) {
	u, ov, clk := newTestUI()
	wave := func() []audio.Quality { return nil }
	u.Show(session.View{Quality: wave})
	u.Alert(session.MsgUndelivered)
	u.Show(session.View{Quality: wave, NoSignal: true})
	if ov.label != session.MsgUndelivered {
		t.Fatalf("label %q, want the alert over the notice", ov.label)
	}
	if clk.expire(t); ov.label != session.NoSignal || !ov.wave {
		t.Fatalf("after the alert: label %q, waveform %v; want the notice over the waveform", ov.label, ov.wave)
	}
	u.Flash("已取消 0:04")
	if clk.expire(t); ov.label != session.NoSignal {
		t.Fatalf("after a flash: label %q, want the notice", ov.label)
	}
	u.Show(session.View{Quality: wave})
	if ov.label == session.NoSignal || ov.label == "" {
		t.Fatalf("signal back: label %q, want the recording label", ov.label)
	}
}
