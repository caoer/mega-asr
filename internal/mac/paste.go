//go:build darwin

package mac

import (
	"context"
	"errors"
	"github.com/ebitengine/purego"
	"time"
)

const (
	cgHIDEventTap          = 0
	cgCombinedSessionState = 0
	kVKANSIV               = 9
	ownEvent               = 0x6d766f63 // "mvoc" in kCGEventSourceUserData: the tap lets it pass
	ucKeyActionDisplay     = 3
	ucNoDeadKeys           = 1 // 1 << kUCKeyTranslateNoDeadKeysBit
)

var (
	cgEventSourceCreate        func(state int32) uintptr
	cgEventCreateKeyboardEvent func(src uintptr, key uint16, down bool) uintptr
	cgEventSetFlags            func(ev uintptr, flags uint64)
	cgEventSetIntegerField     func(ev uintptr, field uint32, value int64)
	cgEventPost                func(tap uint32, ev uintptr)

	tisCopyCurrentKeyboardLayoutInputSource func() uintptr
	tisGetInputSourceProperty               func(src, key uintptr) uintptr
	tisUnicodeKeyLayoutData                 uintptr
	cfDataGetBytePtr                        func(uintptr) uintptr
	lmGetKbdType                            func() uint8
	ucKeyTranslate                          func(layout uintptr, code, action uint16, mods, kbdType, opts uint32, dead *uint32, max uint, n *uint, buf *uint16) int32
)

func init() {
	purego.RegisterLibFunc(&cgEventSourceCreate, libCG, "CGEventSourceCreate")
	purego.RegisterLibFunc(&cgEventCreateKeyboardEvent, libCG, "CGEventCreateKeyboardEvent")
	purego.RegisterLibFunc(&cgEventSetFlags, libCG, "CGEventSetFlags")
	purego.RegisterLibFunc(&cgEventSetIntegerField, libCG, "CGEventSetIntegerValueField")
	purego.RegisterLibFunc(&cgEventPost, libCG, "CGEventPost")
	purego.RegisterLibFunc(&tisCopyCurrentKeyboardLayoutInputSource, libCarbon, "TISCopyCurrentKeyboardLayoutInputSource")
	purego.RegisterLibFunc(&tisGetInputSourceProperty, libCarbon, "TISGetInputSourceProperty")
	tisUnicodeKeyLayoutData = symbolValue(libCarbon, "kTISPropertyUnicodeKeyLayoutData")
	purego.RegisterLibFunc(&cfDataGetBytePtr, libCF, "CFDataGetBytePtr")
	purego.RegisterLibFunc(&lmGetKbdType, libCarbon, "LMGetKbdType")
	purego.RegisterLibFunc(&ucKeyTranslate, libCarbon, "UCKeyTranslate")
}

// keycodeFor finds the virtual keycode that types ch in the current keyboard
// layout (not the input method: a Pinyin IME still reports its ABC layout).
// Main thread only.
func keycodeFor(ch rune) uint16 {
	src := tisCopyCurrentKeyboardLayoutInputSource()
	if src == 0 {
		return kVKANSIV
	}
	defer cfRelease(src)
	data := tisGetInputSourceProperty(src, tisUnicodeKeyLayoutData)
	if data == 0 {
		return kVKANSIV
	}
	layout := cfDataGetBytePtr(data)
	kbd := uint32(lmGetKbdType())
	for code := uint16(0); code < 128; code++ {
		var dead uint32
		var n uint
		var buf [4]uint16
		if ucKeyTranslate(layout, code, ucKeyActionDisplay, 0, kbd, ucNoDeadKeys, &dead, 4, &n, &buf[0]) == 0 &&
			n == 1 && rune(buf[0]) == ch {
			return code
		}
	}
	return kVKANSIV
}

// Paste leaves text on the general pasteboard, where clipboard managers keep
// it in their history, and posts ⌘V to whatever has focus.
func Paste(text string) error { return PasteContext(context.Background(), text) }

// PasteContext refuses expired requests and missing event grants before posting.
func PasteContext(ctx context.Context, text string) error {
	var key uint16
	DoSync(func() { key = keycodeFor('v') })
	if err := CopyText(text); err != nil {
		return err
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	return postKey(key, flagCommand)
}

// CopyText leaves text on the general pasteboard.
func CopyText(text string) error {
	return appPasteboard.copy(nil, func() error { return writeClipboard(text) })
}

func writeClipboard(text string) error {
	DoSync(func() {
		pb := send(class("NSPasteboard"), "generalPasteboard")
		send(pb, "clearContents")
		send(pb, "setString:forType:", nsString(text), nsString("public.utf8-plain-text"))
	})
	return nil
}

// PressReturn posts a bare Return to whatever has focus.
func PressReturn() error { return postKey(keyReturn, 0) }

func postKey(code uint16, flags uint64) error {
	return postKeyFocused(code, flags, nil)
}

// postKeyFocused prepares both events before checking the captured composer.
func postKeyFocused(code uint16, flags uint64, focused func() bool) error {
	if !axIsProcessTrusted() || !cgPreflightPostEventAccess() {
		return errors.New("Accessibility or post-event access is off; text is on the clipboard")
	}
	src := cgEventSourceCreate(cgCombinedSessionState)
	if src != 0 {
		defer cfRelease(src)
	}
	var events [2]uintptr
	defer func() {
		for _, ev := range events {
			if ev != 0 {
				cfRelease(ev)
			}
		}
	}()
	for i, down := range []bool{true, false} {
		ev := cgEventCreateKeyboardEvent(src, code, down)
		if ev == 0 {
			return errors.New("could not create keyboard event")
		}
		events[i] = ev
		cgEventSetFlags(ev, flags)
		cgEventSetIntegerField(ev, cgSourceUserData, ownEvent)
	}
	if focused != nil && !focused() {
		return ErrPasteUnconfirmed
	}
	for _, ev := range events {
		cgEventPost(cgHIDEventTap, ev)
	}
	return nil
}

var appPasteboard = clipboardBoard{onMain: onMain}

// PasteInto prepares the clipboard first, then restores the captured composer
// and posts immediately after the final focus check. Readback confirms the
// original composer before the delivery turn or Enter can proceed.
func PasteInto(a App, text string, focusWait, wait time.Duration) (*PasteReceipt, error) {
	owned := a.retain()
	var key uint16
	ops := composerOps{
		prepare: func(text string) error {
			DoSync(func() {
				key = keycodeFor('v')
				pb := send(class("NSPasteboard"), "generalPasteboard")
				send(pb, "clearContents")
				send(pb, "setString:forType:", nsString(text), nsString("public.utf8-plain-text"))
			})
			return nil
		},
		activate:  func() error { return Activate(owned, focusWait) },
		focused:   owned.Focused,
		read:      owned.Text,
		selection: owned.Selection,
		post:      func() error { return postKeyFocused(key, flagCommand, owned.Focused) },
		enter:     func() error { return postKeyFocused(keyReturn, 0, owned.Focused) },

		release: func() { owned.Release() },
	}
	return pasteComposer(&appPasteboard, ops, text, wait)
}
