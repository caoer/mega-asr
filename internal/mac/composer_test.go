package mac

import (
	"context"
	"errors"
	"testing"
	"time"
)

// noElementApp stands for an app as Activate and Focused see it: frontmost
// window, and a composer element captured at the tap or not.
type noElementApp struct {
	front, captured, sameElement bool
	posts, enters                int
}

func (a *noElementApp) ops() composerOps {
	focused := func() bool {
		return a.front && composerHeld(a.captured, func() bool { return a.sameElement })
	}
	return composerOps{
		prepare: func(string) error { return nil },
		activate: func() error {
			if !restoreComposer(func() bool { return a.front }, func() bool { return composerHeld(a.captured, func() bool { return a.sameElement }) }, func() {}) {
				return errors.New("App did not take focus within 1s")
			}
			return nil
		},
		focused: focused,
		read:    func(context.Context) (string, bool) { return "", false }, // no captured element: unreadable
		post:    func() error { a.posts++; return nil },
		enter:   func() error { a.enters++; return nil },
		release: func() {},
	}
}

// WeChat, Feishu and Chromium windows with accessibility off expose no
// focused element: the take pastes and sends while their window is in front.
func TestNoCapturedElementPastesIntoFrontWindow(t *testing.T) {
	a := &noElementApp{front: true}
	p, err := pasteComposer(&clipboardBoard{}, a.ops(), "text", time.Second)
	if err != nil {
		t.Fatalf("paste: %v", err)
	}
	if err := p.Submit(); err != nil {
		t.Fatalf("submit: %v", err)
	}
	p.Release()
	if a.posts != 1 || a.enters != 1 {
		t.Fatalf("posts %d enters %d, want 1 and 1", a.posts, a.enters)
	}
}

func TestComposerMovesStillRefuse(t *testing.T) {
	for name, a := range map[string]*noElementApp{
		"window moved, no element":        {front: false},
		"captured element lost its focus": {front: true, captured: true, sameElement: false},
	} {
		p, err := pasteComposer(&clipboardBoard{}, a.ops(), "text", time.Second)
		p.Release()
		if err == nil || a.posts != 0 {
			t.Fatalf("%s: err %v posts %d, want a refusal and no paste", name, err, a.posts)
		}
	}
}

func TestOriginalComposerRestoredBeforePasteAndEnter(t *testing.T) {
	for _, reachable := range []bool{true, false} {
		current := 2
		fields := map[int]string{}
		clipboard := ""
		ok := restoreComposer(func() bool { return true }, func() bool { return current == 1 }, func() {
			if reachable {
				current = 1
			}
		})
		if ok {
			fields[current] += "text"
			fields[current] += "[ENTER]"
		} else {
			clipboard = "text"
		}
		if fields[2] != "" {
			t.Fatalf("second composer received %q", fields[2])
		}
		if reachable && fields[1] != "text[ENTER]" {
			t.Fatalf("original composer %q", fields[1])
		}
		if !reachable && clipboard != "text" {
			t.Fatal("unreachable composer must retain clipboard text")
		}
	}
}

func TestComposerLosesWindowDuringRestore(t *testing.T) {
	window := true
	if restoreComposer(func() bool { return window }, func() bool { return false }, func() { window = false }) {
		t.Fatal("restored after window changed")
	}
}
