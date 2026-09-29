package mac

// choiceIndex is the button an NSAlert's runModal answer resp names among n
// (NSAlertFirstButtonReturn is 1000), or -1 when it names none.
func choiceIndex(resp, n int) int {
	if i := resp - 1000; i >= 0 && i < n {
		return i
	}
	return -1
}
