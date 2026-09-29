//go:build darwin

package deliver

import (
	"context"
	"errors"
	"fmt"
	"log"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/caoer/mega-asr/internal/mac"
	"github.com/caoer/mega-asr/internal/session"
	"github.com/caoer/mega-asr/internal/store"
)

// The mac calls a delivery makes; a test replaces them.
var (
	activate    = mac.Activate
	paste       = mac.Paste
	copyText    = mac.CopyText
	pressReturn = mac.PressReturn
	focusedText = mac.FocusedText
	appOf       = mac.AppOf
)

// Target is a locked delivery target: a herdr pane, an app and the window
// that was focused in it, or the clipboard.
type Target struct {
	Pane      string // herdr pane id; empty for an app target
	Workspace string // the pane's herdr workspace label
	App       mac.App
	Clipboard bool // the text is copied and no key is pressed

	sent    time.Time     // when the pane got its last key: the paste, or the Enter
	entered bool          // Submit pressed Enter in the pane
	value   chan appValue // an app's focused value, read once after the paste
}

// appValue is an app's focused element's text; ok is false when it has none.
type appValue struct {
	text string
	ok   bool
}

// CopyOnly reports whether the target is the clipboard: a delivery copies
// the text and presses nothing.
func (t *Target) CopyOnly() bool { return t.Clipboard }

// Record is the target as a take's record gives it: kind herdr, app or
// clipboard, the pane and its workspace, the app by name, pid and bundle id
// with its focused window's title (for a pane, the terminal that hosts herdr).
func (t *Target) Record() store.Target {
	if t.Clipboard {
		return store.Target{Kind: "clipboard"}
	}
	kind := "app"
	if t.Pane != "" {
		kind = "herdr"
	}
	return store.Target{Kind: kind, Pane: t.Pane, Workspace: t.Workspace, App: t.App.Name,
		PID: int(t.App.PID), BundleID: t.App.BundleID, Title: t.App.Title}
}

// Release frees the captured window reference; safe to call twice.
func (t *Target) Release() { t.App.Release() }

func (t *Target) String() string {
	if t.Clipboard {
		return "剪贴板"
	}
	if t.Pane != "" {
		return "herdr pane " + t.Pane
	}
	return fmt.Sprintf("%s (%s) %q", t.App.Name, t.App.BundleID, t.App.Title)
}

// Deliverer delivers to the target locked when the recording started.
type Deliverer struct {
	// Pane, when set, is every take's target in place of what is frontmost:
	// a scratch instance for checks delivers to a pane of its own.
	Pane      string
	Herdr     Herdr
	HerdrApps []string      // bundle ids of terminals that host herdr
	FocusWait time.Duration // how long the captured window has to take focus
	Settle    time.Duration // between the text and the Enter that sends it
}

// Capture locks the target: what is frontmost now.
func (d Deliverer) Capture() session.Target {
	if d.Pane != "" {
		return &Target{Pane: d.Pane}
	}
	t := &Target{App: mac.Frontmost()}
	if slices.Contains(d.HerdrApps, t.App.BundleID) {
		pane, ws, err := d.Herdr.Focused()
		switch {
		case err != nil:
			log.Printf("deliver: %v", err)
		case herdrFront(t.App.BundleID, t.App.Title, d.HerdrApps, ws):
			t.Pane, t.Workspace = pane, ws
		}
	}
	return t
}

// Resolve is the target a resend names: front (what has focus now, as a
// take's start captures it), pane:<id> (a herdr pane), app:<pid>:<bundle id>
// (the focused window of the app running as pid, when that app is still the
// one the caller saw) or clipboard. A pane herdr does not list, a pid no app
// runs as or another app runs as now, and a pid without its bundle id are
// refused; a target that goes away after this fails its delivery, which
// says why.
func (d Deliverer) Resolve(to string) (session.Target, error) {
	kind, arg, _ := strings.Cut(to, ":")
	switch {
	case to == "" || to == "front":
		return d.Capture(), nil
	case to == "clipboard":
		return &Target{Clipboard: true}, nil
	case kind == "pane" && arg != "":
		panes, err := d.Herdr.Panes()
		if err != nil {
			return nil, fmt.Errorf("target %q: %w", to, err)
		}
		for _, p := range panes {
			if p.ID == arg {
				return &Target{Pane: arg, Workspace: p.Workspace}, nil
			}
		}
		return nil, fmt.Errorf("target %q: herdr lists no such pane", to)
	case kind == "app":
		spid, bundle, _ := strings.Cut(arg, ":")
		pid, err := strconv.ParseInt(spid, 10, 32)
		if err != nil || pid <= 0 {
			return nil, fmt.Errorf("target %q: app:<pid>:<bundle id> wants a pid", to)
		}
		if bundle == "" { // a pid alone cannot tell the app the caller saw from one that took its pid since
			return nil, fmt.Errorf("target %q: name the app too, app:<pid>:<bundle id>", to)
		}
		a, err := appOf(int32(pid))
		if err != nil {
			return nil, fmt.Errorf("target %q: %w", to, err)
		}
		if a.BundleID != bundle {
			a.Release()
			return nil, fmt.Errorf("target %q: pid %d runs %s (%s) now", to, pid, a.Name, a.BundleID)
		}
		return &Target{App: a}, nil
	}
	return nil, fmt.Errorf("target %q: want front, pane:<id>, app:<pid>:<bundle id> or clipboard", to)
}

// Deliver pastes text into a herdr pane through herdr, or focuses the captured
// app window again and pastes, or copies it for the clipboard. Either way the
// text stays on the clipboard, so clipboard history keeps every transcript;
// on failure the error says why.
func (d Deliverer) Deliver(st session.Target, text string) error {
	t := st.(*Target)
	var err error
	switch {
	case t.Clipboard:
	case t.Pane != "":
		if err = d.Herdr.SendText(t.Pane, text); err == nil {
			t.sent = time.Now()
		}
	case t.App.PID == 0:
		err = errors.New("no app was frontmost")
	default:
		if err = activate(t.App, d.FocusWait); err == nil {
			paste(text)
			ch, read, pid := make(chan appValue, 1), focusedText, t.App.PID
			t.value = ch
			go func() {
				time.Sleep(fieldAfter)
				v, ok := read(pid)
				ch <- appValue{v, ok}
			}()
			return nil
		}
	}
	copyText(text)
	return err
}

// Submit presses Enter in the target after Deliver: herdr sends the key to
// the pane; an app gets a Return posted while its captured window still has
// focus. Settle lets the text land first, so the Enter is not read as part
// of a paste.
func (d Deliverer) Submit(st session.Target) error {
	t := st.(*Target)
	if t.Clipboard {
		return errors.New("the clipboard has no Enter")
	}
	time.Sleep(d.Settle)
	if t.Pane != "" {
		if err := d.Herdr.SendKeys(t.Pane, "enter"); err != nil {
			return err
		}
		t.sent, t.entered = time.Now(), true
		return nil
	}
	if !t.App.Focused() {
		return fmt.Errorf("%s lost focus before Enter", t.App.Name)
	}
	pressReturn()
	return nil
}

// The controller reads a delivery's target back through Received.
var _ session.ReadBacker = Deliverer{}

// Received reads back what the target holds after Deliver, and after Submit
// when it ran: whole, short or unknown, and why; "" for the clipboard, which
// has nothing to read. A pane is read through herdr; an app's focused
// element was read while the settle before the Enter ran. It changes
// nothing in the target and returns within a few seconds.
func (d Deliverer) Received(st session.Target, text string) (verdict, why string) {
	t := st.(*Target)
	switch {
	case t.Clipboard:
		return "", ""
	case t.Pane != "":
		if t.sent.IsZero() {
			return Unknown, "no key reached the pane"
		}
		return d.Herdr.received(t.Pane, text, t.sent, t.entered)
	case t.value != nil:
		return before(time.Now().Add(fieldDeadline), func(ctx context.Context) (string, string) {
			var v appValue
			select {
			case v = <-t.value:
			case <-ctx.Done():
				return Unknown, "no answer by the deadline"
			}
			if !v.ok {
				return Unknown, "the focused element has no text"
			}
			return field(text, v.text)
		})
	}
	return Unknown, "not read"
}
