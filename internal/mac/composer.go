package mac

// restoreComposer succeeds only while the original window and composer both
// hold focus after the restoration attempt.
func restoreComposer(window, focused func() bool, restore func()) bool {
	if !window() {
		return false
	}
	if !focused() {
		restore()
	}
	return window() && focused()
}

// composerHeld reports whether the composer captured at the tap still holds
// focus. An app that exposed no focused element at the tap (WeChat, Feishu,
// a Chromium window with accessibility off) has no composer to verify: the
// window check alone decides, and the paste takes the unreadable path.
func composerHeld(captured bool, same func() bool) bool {
	return !captured || same()
}
