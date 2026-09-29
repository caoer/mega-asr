//go:build darwin

package mac

import (
	"github.com/ebitengine/purego"
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
func Paste(text string) {
	var key uint16
	DoSync(func() { key = keycodeFor('v') })
	CopyText(text)
	postKey(key, flagCommand)
}

// CopyText leaves text on the general pasteboard.
func CopyText(text string) {
	DoSync(func() {
		pb := send(class("NSPasteboard"), "generalPasteboard")
		send(pb, "clearContents")
		send(pb, "setString:forType:", nsString(text), nsString("public.utf8-plain-text"))
	})
}

// PressReturn posts a bare Return to whatever has focus.
func PressReturn() { postKey(keyReturn, 0) }

func postKey(code uint16, flags uint64) {
	src := cgEventSourceCreate(cgCombinedSessionState)
	for _, down := range []bool{true, false} {
		ev := cgEventCreateKeyboardEvent(src, code, down)
		cgEventSetFlags(ev, flags)
		cgEventSetIntegerField(ev, cgSourceUserData, ownEvent)
		cgEventPost(cgHIDEventTap, ev)
		cfRelease(ev)
	}
	if src != 0 {
		cfRelease(src)
	}
}
