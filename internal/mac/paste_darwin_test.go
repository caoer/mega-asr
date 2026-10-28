//go:build darwin

package mac

import (
	"errors"
	"fmt"
	"testing"
	"time"
)

func TestPostKeyRefusesMissingGrant(t *testing.T) {
	ax, post, event := axIsProcessTrusted, cgPreflightPostEventAccess, cgEventPost
	defer func() { axIsProcessTrusted, cgPreflightPostEventAccess, cgEventPost = ax, post, event }()
	for _, grants := range [][2]bool{{false, true}, {true, false}, {false, false}} {
		axIsProcessTrusted = func() bool { return grants[0] }
		cgPreflightPostEventAccess = func() bool { return grants[1] }
		posted := false
		cgEventPost = func(uint32, uintptr) { posted = true }
		if err := postKey(keyReturn, 0); err == nil || posted {
			t.Fatalf("grants %v: err %v posted %v", grants, err, posted)
		}
	}
}

func TestUnreadableComposerRejectsFocusMoveDuringEventPreparation(t *testing.T) {
	ax, grant := axIsProcessTrusted, cgPreflightPostEventAccess
	source, create := cgEventSourceCreate, cgEventCreateKeyboardEvent
	flags, tag, post, release := cgEventSetFlags, cgEventSetIntegerField, cgEventPost, cfRelease
	t.Cleanup(func() {
		axIsProcessTrusted, cgPreflightPostEventAccess = ax, grant
		cgEventSourceCreate, cgEventCreateKeyboardEvent = source, create
		cgEventSetFlags, cgEventSetIntegerField, cgEventPost, cfRelease = flags, tag, post, release
	})
	f := newComposerFake()
	f.unreadable = true
	axIsProcessTrusted = func() bool { return true }
	cgPreflightPostEventAccess = func() bool { f.move(2); return true }
	cgEventSourceCreate = func(int32) uintptr { return 10 }
	cgEventCreateKeyboardEvent = func(_ uintptr, _ uint16, down bool) uintptr {
		if down {
			return 1
		}
		return 2
	}
	cgEventSetFlags = func(uintptr, uint64) {}
	cgEventSetIntegerField = func(uintptr, uint32, int64) {}
	cgEventPost = func(_ uint32, ev uintptr) {
		f.posts++
		if ev == 1 {
			f.fields[f.focus] += f.clipboard
		}
	}
	cfRelease = func(uintptr) {}
	ops := f.ops()
	ops.post = func() error { return postKeyFocused(kVKANSIV, flagCommand, ops.focused) }
	p, err := pasteComposer(&clipboardBoard{}, ops, "text", time.Second)
	defer p.Release()
	if !errors.Is(err, ErrPasteUnconfirmed) {
		t.Errorf("paste err=%v", err)
	}
	// Returning to the original composer must not enable Enter on its old draft.
	f.move(1)
	if err := p.Submit(); !errors.Is(err, ErrPasteUnconfirmed) {
		t.Errorf("submit err=%v", err)
	}
	if f.posts != 0 || f.enters != 0 || f.fields[1] != "draft " || f.fields[2] != "" {
		t.Fatalf("posts=%d enters=%d fields=%v", f.posts, f.enters, f.fields)
	}
}

func TestPostKeyChecksFocusAfterPreparation(t *testing.T) {
	ax, grant := axIsProcessTrusted, cgPreflightPostEventAccess
	source, create := cgEventSourceCreate, cgEventCreateKeyboardEvent
	flags, tag, post, release := cgEventSetFlags, cgEventSetIntegerField, cgEventPost, cfRelease
	t.Cleanup(func() {
		axIsProcessTrusted, cgPreflightPostEventAccess = ax, grant
		cgEventSourceCreate, cgEventCreateKeyboardEvent = source, create
		cgEventSetFlags, cgEventSetIntegerField, cgEventPost, cfRelease = flags, tag, post, release
	})
	for _, key := range []uint16{kVKANSIV, keyReturn} {
		for _, moveAt := range []string{"grant", "source", "down", "up", "flags", "tag", "none"} {
			t.Run(fmt.Sprintf("key%d/%s", key, moveAt), func(t *testing.T) {
				focused := true
				created, posted, configured, checked := 0, 0, 0, 0
				released := map[uintptr]bool{}
				move := func(phase string) {
					if phase == moveAt {
						focused = false
					}
				}
				axIsProcessTrusted = func() bool { return true }
				cgPreflightPostEventAccess = func() bool { move("grant"); return true }
				cgEventSourceCreate = func(int32) uintptr { move("source"); return 10 }
				cgEventCreateKeyboardEvent = func(_ uintptr, code uint16, down bool) uintptr {
					if code != key {
						t.Fatalf("key %d, want %d", code, key)
					}
					created++
					if down {
						move("down")
					} else {
						move("up")
					}
					return uintptr(created)
				}
				cgEventSetFlags = func(uintptr, uint64) { configured++; move("flags") }
				cgEventSetIntegerField = func(uintptr, uint32, int64) { configured++; move("tag") }
				cgEventPost = func(uint32, uintptr) { posted++ }
				cfRelease = func(ref uintptr) {
					if released[ref] {
						t.Fatalf("double release %d", ref)
					}
					released[ref] = true
				}
				err := postKeyFocused(key, 0, func() bool {
					checked++
					if created != 2 || configured != 4 {
						t.Errorf("focus checked before preparation: created=%d configured=%d", created, configured)
					}
					return focused
				})
				if moveAt == "none" {
					if err != nil || posted != 2 {
						t.Fatalf("stable focus: err=%v posted=%d", err, posted)
					}
				} else if !errors.Is(err, ErrPasteUnconfirmed) || posted != 0 {
					t.Fatalf("focus moved during %s: err=%v posted=%d", moveAt, err, posted)
				}
				if checked != 1 || len(released) != 3 {
					t.Fatalf("checks=%d released=%v", checked, released)
				}
			})
		}
	}
}
