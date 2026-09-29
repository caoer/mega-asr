//go:build darwin

package mac

import "github.com/ebitengine/purego"

var nsBeep func()

func init() {
	purego.RegisterLibFunc(&nsBeep, libAppKit, "NSBeep")
}

// Beep plays the system alert sound once, at the alert volume the user set,
// on the main thread; it returns at once.
func Beep() { Do(nsBeep) }
