//go:build darwin

package deliver

import (
	"errors"
	"fmt"
	"log"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/caoer/mega-asr/internal/l10n"
	"github.com/caoer/mega-asr/internal/mac"
	"github.com/caoer/mega-asr/internal/session"
	"github.com/caoer/mega-asr/internal/store"
)

// The mac calls a delivery makes; a test replaces them.
var (
	pasteInto = mac.PasteInto
	copyText  = mac.CopyText
	appOf     = mac.AppOf
)

// Target is a locked delivery target: a herdr pane, an app and the window
// that was focused in it, or the clipboard.
type Target struct {
	Pane      string // herdr pane id; empty for an app target
	Workspace string // the pane's herdr workspace label
	App       mac.App
	Clipboard bool // the text is copied and no key is pressed

	sent    time.Time         // when the pane got its last key: the paste, or the Enter
	entered bool              // Submit pressed Enter in the pane
	receipt *mac.PasteReceipt // confirmation from the captured composer
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
func (t *Target) Release() {
	if t.receipt != nil {
		t.receipt.Release()
	}
	t.App.Release()
}

func (t *Target) String() string {
	if t.Clipboard {
		return l10n.T("剪贴板", "Clipboard")
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
	if t.Clipboard || t.Pane != "" || t.App.PID == 0 {
		if err = copyText(text); err != nil {
			return clipboardError(err)
		}
	}
	switch {
	case t.Clipboard:
	case t.Pane != "":
		if err = d.Herdr.SendText(t.Pane, text); err == nil {
			t.sent = time.Now()
		}
	case t.App.PID == 0:
		err = errors.New("no app was frontmost")
	default:
		t.receipt, err = pasteInto(t.App, text, d.FocusWait, fieldDeadline)
		return clipboardError(err)
	}
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
	if t.receipt == nil {
		return session.ErrUnconfirmedPaste
	}
	return clipboardError(t.receipt.Submit())
}

// The controller reads a delivery's target back through Received.
var _ session.ReadBacker = Deliverer{}

// Received reads back what the target holds after Deliver, and after Submit
// when it ran: whole, short or unknown, and why; "" for the clipboard, which
// has nothing to read. A pane is read through herdr; an app's captured
// composer was verified before the Enter. It changes
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
	case t.receipt != nil:
		value, ok := t.receipt.Value()
		if !ok {
			return Unknown, "original composer did not confirm the paste"
		}
		return field(text, value)
	}
	return Unknown, "not read"
}

func clipboardError(err error) error {
	switch {
	case errors.Is(err, mac.ErrClipboardPending):
		return fmt.Errorf("%w: %v", session.ErrClipboardBusy, err)
	case errors.Is(err, mac.ErrPasteUnconfirmed):
		return fmt.Errorf("%w: %v", session.ErrUnconfirmedPaste, err)
	}
	return err
}

// CopyText refuses to replace a paste whose consumer has not confirmed it.
func CopyText(text string) error { return clipboardError(mac.CopyText(text)) }
