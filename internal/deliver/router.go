package deliver

import (
	"errors"
	"fmt"
	"sync"

	"github.com/caoer/mega-asr/internal/session"
	"github.com/caoer/mega-asr/internal/store"
)

// Outcome is how a delivery ended, as the keypad shows it.
type Outcome string

const (
	Pasted Outcome = "pasted" // the text landed on the Mac and no Enter followed
	Sent   Outcome = "sent"   // Enter followed the paste, or Telegram took the message
	Failed Outcome = "failed" // the text is on the clipboard only
	// Timeout: the send ran out of time and may have posted all the same
	// (session.ErrMaybeSent); the text is on the clipboard too.
	Timeout Outcome = "timeout"
)

// Router captures the cursor for every take. A keypad take also keeps its
// locked chat, chosen at start; its end mode chooses between the two.
type Router struct {
	Mac      session.Deliverer // the frontmost app, a herdr pane or the clipboard
	Telegram Telegram
	// Outcome hears how each delivery ended; nil hears nothing.
	Outcome func(Outcome)

	mu      sync.Mutex
	pending map[session.Target]*enter // Mac deliveries that landed and are not settled yet
}

var (
	_ session.Deliverer      = (*Router)(nil)
	_ session.ReadBacker     = (*Router)(nil)
	_ session.CursorCapturer = (*Router)(nil)
)

// Capture captures the current window, including under the keypad lock.
func (r *Router) Capture() session.Target { return r.Mac.Capture() }

// CaptureKeypad uses the lock the keypad showed at this tap's down edge.
// Nil means the tap was cursor-bound, even if the saved lock has changed.
func (r *Router) CaptureKeypad(t *TelegramTarget) session.Target {
	if t != nil && t.Chat != "" {
		return &keypadTarget{telegram: *t, cursor: r.Mac.Capture()}
	}
	return r.Mac.Capture()
}

// keypadTarget owns both destinations captured at the start. The start
// record names the lock; the delivery record names the destination used.
type keypadTarget struct {
	telegram TelegramTarget
	cursor   session.Target
}

func (t *keypadTarget) String() string       { return t.telegram.String() }
func (t *keypadTarget) Record() store.Target { return t.telegram.Record() }
func (t *keypadTarget) TargetLabel() string {
	s := t.telegram.Group
	if t.telegram.TopicLabel != "" {
		s += " / " + t.telegram.TopicLabel
	}
	return s
}

func (t *keypadTarget) ForEnd(send bool) session.Target {
	if send {
		return &t.telegram
	}
	return t.cursor
}

func (t *keypadTarget) Release() {
	if rl, ok := t.cursor.(interface{ Release() }); ok {
		rl.Release()
	}
}

// CaptureCursor is what the Mac captures, locked or not: where the user is
// typing, for the resend hotkey.
func (r *Router) CaptureCursor() session.Target { return r.Mac.Capture() }

// Deliver sends to Telegram or delivers on the Mac, by the target's kind. A
// Telegram send ends there: sent, timeout or failed. A Mac delivery that fails is
// failed; one that lands waits for its Enter, if any, until Received.
func (r *Router) Deliver(t session.Target, text string) error {
	if tt, ok := t.(*TelegramTarget); ok {
		err := r.Telegram.Deliver(tt, text)
		switch {
		case err == nil:
			r.tell(Sent)
		case errors.Is(err, session.ErrMaybeSent):
			r.tell(Timeout)
		default:
			r.tell(Failed)
		}
		return err
	}
	if err := r.Mac.Deliver(t, text); err != nil {
		r.tell(Failed)
		return err
	}
	if c, ok := t.(interface{ CopyOnly() bool }); ok && c.CopyOnly() {
		return nil // a copy to the clipboard is no paste: the keypad shows nothing
	}
	r.mu.Lock()
	if r.pending == nil {
		r.pending = map[session.Target]*enter{}
	}
	r.pending[t] = &enter{}
	r.mu.Unlock()
	return nil
}

// Submit is nothing for Telegram, whose send is the submit; on the Mac it
// presses Enter, and the delivery settles as sent, or failed when the
// Enter fails.
func (r *Router) Submit(t session.Target) error {
	if _, ok := t.(*TelegramTarget); ok {
		return nil
	}
	err := r.Mac.Submit(t)
	r.mu.Lock()
	if e := r.pending[t]; e != nil {
		e.pressed, e.err = true, err
	}
	r.mu.Unlock()
	return err
}

// enter is whether a Mac delivery's Enter was pressed, and its error.
type enter struct {
	pressed bool
	err     error
}

// Received settles a Mac delivery and reads its target back. The controller
// calls it once after every delivery that landed, past the point where an
// Enter could follow it, so the outcome is told here: pasted, sent, or
// failed when the Enter failed. For Telegram it reports the message id the
// send printed, nothing when there was none.
func (r *Router) Received(t session.Target, text string) (verdict, why string) {
	if tt, ok := t.(*TelegramTarget); ok {
		if tt.MessageID == 0 {
			return "", ""
		}
		return Whole, fmt.Sprintf("telegram message %d", tt.MessageID)
	}
	r.mu.Lock()
	e := r.pending[t]
	delete(r.pending, t)
	r.mu.Unlock()
	switch {
	case e == nil: // not delivered through this router
	case !e.pressed:
		r.tell(Pasted)
	case e.err != nil:
		r.tell(Failed)
	default:
		r.tell(Sent)
	}
	if rb, ok := r.Mac.(session.ReadBacker); ok {
		return rb.Received(t, text)
	}
	return "", ""
}

func (r *Router) tell(o Outcome) {
	if r.Outcome != nil {
		r.Outcome(o)
	}
}
