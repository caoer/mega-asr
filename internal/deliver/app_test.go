//go:build darwin

package deliver

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"errors"
	"github.com/caoer/mega-asr/internal/mac"
	"github.com/caoer/mega-asr/internal/session"
)

// A resend to the clipboard copies the text and presses nothing: no focus
// change, no paste, no Enter.
func TestClipboardPressesNothing(t *testing.T) {
	var calls []string
	copyText = func(s string) error { calls = append(calls, "copy:"+s); return nil }
	pasteInto = func(mac.App, string, time.Duration, time.Duration) (*mac.PasteReceipt, error) {
		calls = append(calls, "paste")
		return nil, nil
	}
	t.Cleanup(func() { copyText, pasteInto = mac.CopyText, mac.PasteInto })
	d := Deliverer{}
	to, err := d.Resolve("clipboard")
	if err != nil {
		t.Fatal(err)
	}
	if err := d.Deliver(to, "a line written for the test"); err != nil {
		t.Fatal(err)
	}
	if err := d.Submit(to); err == nil {
		t.Error("Submit to the clipboard did not refuse")
	}
	if got := strings.Join(calls, ","); got != "copy:a line written for the test" {
		t.Fatalf("calls %s", got)
	}
	if r := to.(*Target).Record(); r.Kind != "clipboard" {
		t.Fatalf("record target %+v", r)
	}
}

// An unconfirmed original-composer read propagates as an uncertain delivery.
func TestAppReceived(t *testing.T) {
	pasteInto = func(mac.App, string, time.Duration, time.Duration) (*mac.PasteReceipt, error) {
		return nil, mac.ErrPasteUnconfirmed
	}
	t.Cleanup(func() { pasteInto = mac.PasteInto })
	d := Deliverer{}
	to := &Target{App: mac.App{PID: 4242, Name: "Fake composer"}}
	if err := d.Deliver(to, "invented text"); !errors.Is(err, session.ErrUnconfirmedPaste) {
		t.Fatal(err)
	}
	if err := d.Submit(to); !errors.Is(err, session.ErrUnconfirmedPaste) {
		t.Fatal(err)
	}
}

// fakeHerdrList is a herdr whose pane list holds w1:p1 in workspace saltmarsh.
func fakeHerdrList(t *testing.T) string {
	t.Helper()
	bin := filepath.Join(t.TempDir(), "herdr")
	script := `#!/bin/sh
case "$1 $2" in
"pane list") echo '{"result":{"panes":[{"pane_id":"w1:p1","workspace_id":"w1"}]}}' ;;
"workspace list") echo '{"result":{"workspaces":[{"workspace_id":"w1","label":"saltmarsh"}]}}' ;;
*) echo "unexpected: $*" >&2; exit 1 ;;
esac
`
	if err := os.WriteFile(bin, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	return bin
}

// A pane herdr does not list is refused before anything reaches herdr's
// argv, so a pane id shaped as a flag is never one; so is a pid no app runs
// as, an app:<pid>:<bundle id> whose pid runs another app now, and a bare
// app:<pid>, which names no app.
func TestResolveRefusesGoneTargets(t *testing.T) {
	appOf = func(pid int32) (mac.App, error) {
		if pid != 4242 {
			return mac.App{}, fmt.Errorf("no running application with pid %d", pid)
		}
		return mac.App{PID: 4242, BundleID: "org.example.notes", Name: "Notes"}, nil
	}
	t.Cleanup(func() { appOf = mac.AppOf })
	d := Deliverer{Herdr: Herdr{Bin: fakeHerdrList(t)}}
	for to, want := range map[string]string{
		"pane:w1:p1":                 "herdr w1:p1 saltmarsh",
		"app:4242":                   "",
		"app:4242:org.example.notes": "Notes 4242 org.example.notes",
		"pane:w1:p9":                 "",
		"pane:--x":                   "",
		"app:4243":                   "",
		"app:4242:org.example.chat":  "",
		"app:0":                      "",
		"app:notes":                  "",
		"window:1":                   "",
	} {
		st, err := d.Resolve(to)
		switch {
		case want == "" && err == nil:
			t.Errorf("%s resolved to %v, want a refusal", to, st)
		case want == "":
		case err != nil:
			t.Errorf("%s: %v", to, err)
		default:
			r := st.(*Target).Record()
			got := strings.TrimSpace(fmt.Sprintf("%s %s %s", r.Kind, r.Pane, r.Workspace))
			if r.Kind == "app" {
				got = fmt.Sprintf("%s %d %s", r.App, r.PID, r.BundleID)
			}
			if got != want {
				t.Errorf("%s resolved to %q, want %q", to, got, want)
			}
		}
	}
	if _, err := d.Resolve("app:4242"); err == nil || !strings.Contains(err.Error(), "app:<pid>:<bundle id>") {
		t.Errorf("a bare pid: %v, want a refusal naming the bundle id", err)
	}
	d.Herdr.Bin = "/nonexistent/herdr"
	if _, err := d.Resolve("pane:w1:p1"); err == nil {
		t.Error("a pane resolved while herdr did not answer")
	}
}
