package deliver

import (
	"errors"
	"slices"
	"testing"
	"time"

	"github.com/caoer/mega-asr/internal/session"
)

// macTarget is a fake Mac target.
type macTarget struct{ name string }

func (t *macTarget) String() string { return t.name }

// fakeMac is a Mac deliverer that records its calls.
type fakeMac struct {
	calls      []string
	deliverErr error
	submitErr  error
}

func (m *fakeMac) Capture() session.Target { return &macTarget{"front"} }
func (m *fakeMac) Deliver(t session.Target, text string) error {
	m.calls = append(m.calls, "deliver "+t.String()+" "+text)
	return m.deliverErr
}
func (m *fakeMac) Submit(t session.Target) error {
	m.calls = append(m.calls, "submit "+t.String())
	return m.submitErr
}
func (m *fakeMac) Received(t session.Target, text string) (string, string) {
	return Short, "read back"
}

func newRouter(mac *fakeMac, send Telegram) (*Router, *[]Outcome, *TelegramTarget) {
	var got []Outcome
	lock := &TelegramTarget{}
	r := &Router{Mac: mac, Telegram: send, Outcome: func(o Outcome) { got = append(got, o) }}
	return r, &got, lock
}

// take runs a delivery as the controller does: Deliver, Submit when the
// take sends, then Received once it landed.
func take(r *Router, t session.Target, text string, send bool) (verdict, why string) {
	if et, ok := t.(session.EndTarget); ok {
		t = et.ForEnd(send)
	}
	if err := r.Deliver(t, text); err != nil {
		return
	}
	if send {
		r.Submit(t)
	}
	return r.Received(t, text)
}

func TestRouterChoosesAtCapture(t *testing.T) {
	mac := &fakeMac{}
	r, _, lock := newRouter(mac, fakeSend(`echo "Sent → message 9"`))
	if _, ok := r.Capture().(*macTarget); !ok {
		t.Fatal("unlocked: want the Mac's target")
	}
	*lock = TelegramTarget{Chat: "-100", Topic: 3, Group: "g", TopicLabel: "t"}
	if _, ok := r.Capture().(*macTarget); !ok {
		t.Fatal("Capture under lock: want the Mac's target")
	}
	got := r.CaptureKeypad(lock).(session.EndTarget)
	tt, ok := got.ForEnd(true).(*TelegramTarget)
	if !ok || *tt != *lock {
		t.Fatalf("locked: got %#v, want the locked target", got)
	}
	*lock = TelegramTarget{Chat: "-200"} // the keypad moves on; the take keeps its target
	if tt.Chat != "-100" {
		t.Errorf("the captured target followed the keypad: %q", tt.Chat)
	}
	if _, ok := r.CaptureKeypad(nil).(*macTarget); !ok {
		t.Error("no shown lock must capture the Mac's target")
	}
}

func TestRouterDoneWords(t *testing.T) {
	for _, c := range []struct {
		name               string
		deliverErr, subErr error
		send               bool
		want               Outcome
	}{
		{"paste only → pasted", nil, nil, false, Pasted},
		{"Enter pressed → sent", nil, nil, true, Sent},
		{"paste fails → failed", errors.New("no app"), nil, false, Failed},
		{"Enter fails → failed", nil, errors.New("lost focus"), true, Failed},
	} {
		t.Run(c.name, func(t *testing.T) {
			mac := &fakeMac{deliverErr: c.deliverErr, submitErr: c.subErr}
			r, got, _ := newRouter(mac, Telegram{})
			mt := r.Capture()
			v, why := take(r, mt, "hello", c.send)
			if !slices.Equal(*got, []Outcome{c.want}) {
				t.Errorf("outcomes %q, want [%s]", *got, c.want)
			}
			if c.deliverErr == nil && (v != Short || why != "read back") {
				t.Errorf("Received %q %q: want the Mac's read-back", v, why)
			}
			if len(r.pending) != 0 {
				t.Errorf("%d deliveries left pending", len(r.pending))
			}
		})
	}
}

func TestRouterTelegramSent(t *testing.T) {
	mac := &fakeMac{}
	r, got, lock := newRouter(mac, fakeSend(`cat >/dev/null; echo "Sent → message 31"`))
	*lock = TelegramTarget{Chat: "-100", Group: "g"}
	tt := r.CaptureKeypad(lock)
	v, why := take(r, tt, "hello", true) // Enter under lock: one send, Submit is nothing
	if !slices.Equal(*got, []Outcome{Sent}) {
		t.Errorf("outcomes %q, want [sent]", *got)
	}
	if v != Whole || why != "telegram message 31" {
		t.Errorf("Received %q %q, want the message id", v, why)
	}
	if len(mac.calls) != 0 {
		t.Errorf("the Mac was called: %q", mac.calls)
	}
}

func TestRouterTelegramFailed(t *testing.T) {
	mac := &fakeMac{}
	var copied string
	send := fakeSend(`echo "session expired" >&2; exit 1`)
	send.Copy = func(s string) error { copied = s; return nil }
	r, got, lock := newRouter(mac, send)
	*lock = TelegramTarget{Chat: "-100", Group: "g"}
	tt := r.CaptureKeypad(lock).(session.EndTarget).ForEnd(true)
	if err := r.Deliver(tt, "hello"); err == nil || err.Error() != "telegram: session expired" {
		t.Errorf("err %v", err)
	}
	if !slices.Equal(*got, []Outcome{Failed}) {
		t.Errorf("outcomes %q, want [failed]", *got)
	}
	if copied != "hello" {
		t.Errorf("clipboard %q: a failed send keeps the text there", copied)
	}
	if len(mac.calls) != 0 {
		t.Errorf("the Mac was called: %q (no fallback)", mac.calls)
	}
}

func TestRouterResendTarget(t *testing.T) {
	// A resend names a Mac target while the keypad is locked: it goes to the Mac.
	mac := &fakeMac{}
	r, got, lock := newRouter(mac, fakeSend(`exit 1`))
	*lock = TelegramTarget{Chat: "-100"}
	take(r, &macTarget{"pane:p1"}, "hello", true)
	if !slices.Equal(mac.calls, []string{"deliver pane:p1 hello", "submit pane:p1"}) {
		t.Errorf("calls %q", mac.calls)
	}
	if !slices.Equal(*got, []Outcome{Sent}) {
		t.Errorf("outcomes %q, want [sent]", *got)
	}
}

// copyTarget is a fake clipboard target.
type copyTarget struct{ macTarget }

func (copyTarget) CopyOnly() bool { return true }

// A resend to the clipboard copies and pastes nothing: no keypad outcome.
func TestRouterClipboardNoOutcome(t *testing.T) {
	r, got, _ := newRouter(&fakeMac{}, Telegram{})
	take(r, &copyTarget{macTarget{"clipboard"}}, "hello", false)
	if len(*got) != 0 {
		t.Errorf("outcomes %q, want none", *got)
	}
}

// A send past its deadline may have posted: timeout, not failed.
func TestRouterTelegramTimeout(t *testing.T) {
	send := fakeSend(`exec sleep 30`)
	send.Wait = 200 * time.Millisecond
	r, got, lock := newRouter(&fakeMac{}, send)
	*lock = TelegramTarget{Chat: "-100", Group: "g"}
	if err := r.Deliver(r.CaptureKeypad(lock).(session.EndTarget).ForEnd(true), "hello"); !errors.Is(err, session.ErrMaybeSent) {
		t.Errorf("err %v, want ErrMaybeSent", err)
	}
	if !slices.Equal(*got, []Outcome{Timeout}) {
		t.Errorf("outcomes %q, want [timeout]", *got)
	}
}
