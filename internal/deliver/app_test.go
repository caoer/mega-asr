//go:build darwin

package deliver

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/caoer/mega-asr/internal/mac"
)

// A resend to the clipboard copies the text and presses nothing: no focus
// change, no paste, no Enter.
func TestClipboardPressesNothing(t *testing.T) {
	var calls []string
	activate = func(mac.App, time.Duration) error { calls = append(calls, "activate"); return nil }
	paste = func(string) { calls = append(calls, "paste") }
	copyText = func(s string) { calls = append(calls, "copy:"+s) }
	pressReturn = func() { calls = append(calls, "return") }
	t.Cleanup(func() {
		activate, paste, copyText, pressReturn = mac.Activate, mac.Paste, mac.CopyText, mac.PressReturn
	})
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

// An app's focused value is read once after the paste: whole when it holds
// the text, unknown when the element has no text value or the read hangs.
// The delivery itself is done before the readback is asked for.
func TestAppReceived(t *testing.T) {
	activate = func(mac.App, time.Duration) error { return nil }
	paste = func(string) {}
	t.Cleanup(func() {
		activate, paste, focusedText = mac.Activate, mac.Paste, mac.FocusedText
	})
	const text = "Bob Example measured the queue depth every minute. 队列深度每分钟测一次。"
	hang := make(chan struct{})
	t.Cleanup(func() { close(hang) })
	for _, c := range []struct {
		name  string
		value func(int32) (string, bool)
		want  string
	}{
		{"holds the text", func(int32) (string, bool) { return "draft " + text, true }, Whole},
		{"holds its head", func(int32) (string, bool) { return text[:40], true }, Short},
		{"no text value", func(int32) (string, bool) { return "", false }, Unknown},
		{"the read hangs", func(int32) (string, bool) { <-hang; return text, true }, Unknown},
	} {
		focusedText = c.value
		d := Deliverer{}
		to := &Target{App: mac.App{PID: 4242, Name: "Notes"}}
		if err := d.Deliver(to, text); err != nil {
			t.Fatalf("%s: deliver %v", c.name, err)
		}
		start := time.Now()
		if got, why := d.Received(to, text); got != c.want || time.Since(start) > fieldDeadline+time.Second {
			t.Errorf("%s: %s (%s) after %v, want %s", c.name, got, why, time.Since(start), c.want)
		}
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
