// Package deliver puts a transcript where the user was typing when the
// recording started: herdr's focused pane when a herdr client window was
// frontmost, otherwise the frontmost app's focused window and element, by
// paste; and presses Enter there when the utterance is sent.
package deliver

import (
	"slices"
	"strings"
)

// herdrFront reports whether the frontmost window is a herdr client: a herdr
// host terminal whose title is herdr's "<machine>: <workspace>" for the
// workspace herdr reports focused. A plain window of the same terminal has
// another title and gets a paste instead.
func herdrFront(bundleID, title string, apps []string, workspace string) bool {
	return slices.Contains(apps, bundleID) && workspace != "" && strings.HasSuffix(title, ": "+workspace)
}
