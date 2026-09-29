//go:build darwin

package mac

import (
	"fmt"
	"time"

	"github.com/ebitengine/purego"
	"github.com/ebitengine/purego/objc"
)

var (
	axUIElementCreateApplication   func(pid int32) uintptr
	axUIElementCopyAttributeValue  func(el, attr uintptr, out *uintptr) int32
	axUIElementSetAttributeValue   func(el, attr, value uintptr) int32
	axUIElementPerformAction       func(el, action uintptr) int32
	axUIElementSetMessagingTimeout func(el uintptr, seconds float32) int32
	cfBooleanTrue                  uintptr
)

func init() {
	purego.RegisterLibFunc(&axUIElementCreateApplication, libAS, "AXUIElementCreateApplication")
	purego.RegisterLibFunc(&axUIElementCopyAttributeValue, libAS, "AXUIElementCopyAttributeValue")
	purego.RegisterLibFunc(&axUIElementSetAttributeValue, libAS, "AXUIElementSetAttributeValue")
	purego.RegisterLibFunc(&axUIElementPerformAction, libAS, "AXUIElementPerformAction")
	purego.RegisterLibFunc(&axUIElementSetMessagingTimeout, libAS, "AXUIElementSetMessagingTimeout")
	cfBooleanTrue = symbolValue(libCF, "kCFBooleanTrue")
}

// App is an application and, when it has them, its focused window and focused
// element at the time it was captured. Window and Element are retained
// AXUIElementRefs; Release frees them.
type App struct {
	PID      int32   `json:"pid"`
	BundleID string  `json:"bundle_id"`
	Name     string  `json:"name"`
	Title    string  `json:"title"` // the focused window's title
	Active   bool    `json:"-"`     // RunningApps: the frontmost app
	Window   uintptr `json:"-"`
	Element  uintptr `json:"-"`
}

// Release frees the captured references.
func (a *App) Release() {
	if a.Window != 0 {
		cfRelease(a.Window)
		a.Window = 0
	}
	if a.Element != 0 {
		cfRelease(a.Element)
		a.Element = 0
	}
}

// Frontmost captures the frontmost application and its focused window.
func Frontmost() App {
	var a App
	DoSync(func() {
		app := send(send(class("NSWorkspace"), "sharedWorkspace"), "frontmostApplication")
		a = appInfo(app)
	})
	WithPool(func() {
		a.Window, a.Title = focusedWindow(a.PID)
		a.Element = focusedElement(a.PID)
	})
	return a
}

// AppOf captures a running application by pid and its focused window.
func AppOf(pid int32) (App, error) {
	var a App
	DoSync(func() {
		app := send(class("NSRunningApplication"), "runningApplicationWithProcessIdentifier:", pid)
		a = appInfo(app)
	})
	if a.PID == 0 {
		return a, fmt.Errorf("no running application with pid %d", pid)
	}
	WithPool(func() { a.Window, a.Title = focusedWindow(a.PID) })
	return a, nil
}

// RunningApps lists the running applications that have a Dock icon (the
// regular activation policy), each with its focused window's title and
// whether it is the frontmost; no window reference is kept.
func RunningApps() []App {
	var apps []App
	DoSync(func() {
		ws := send(class("NSWorkspace"), "sharedWorkspace")
		front := appInfo(send(ws, "frontmostApplication")).PID
		list := send(ws, "runningApplications")
		for i := range sendInt(list, "count") {
			app := send(list, "objectAtIndex:", uint(i))
			if sendInt(app, "activationPolicy") != 0 { // NSApplicationActivationPolicyRegular
				continue
			}
			if a := appInfo(app); a.PID != 0 {
				a.Active = a.PID == front
				apps = append(apps, a)
			}
		}
	})
	WithPool(func() {
		for i := range apps {
			var win uintptr
			win, apps[i].Title = focusedWindow(apps[i].PID)
			if win != 0 {
				cfRelease(win)
			}
		}
	})
	return apps
}

func appInfo(app objc.ID) App {
	if app == 0 {
		return App{}
	}
	return App{
		PID:      objc.Send[int32](app, sel("processIdentifier")),
		BundleID: goString(send(app, "bundleIdentifier")),
		Name:     goString(send(app, "localizedName")),
	}
}

func axApp(pid int32) uintptr {
	el := axUIElementCreateApplication(pid)
	if el != 0 {
		axUIElementSetMessagingTimeout(el, 1.0) // a hung app must not hang delivery
	}
	return el
}

func axCopy(el uintptr, attr string) uintptr {
	var v uintptr
	if axUIElementCopyAttributeValue(el, uintptr(nsString(attr)), &v) != 0 {
		return 0
	}
	return v
}

// focusedWindow returns the app's focused window (retained) and its title.
func focusedWindow(pid int32) (uintptr, string) {
	el := axApp(pid)
	if el == 0 {
		return 0, ""
	}
	defer cfRelease(el)
	win := axCopy(el, "AXFocusedWindow")
	if win == 0 {
		return 0, ""
	}
	var title string
	if t := axCopy(win, "AXTitle"); t != 0 {
		title = goString(objc.ID(t))
		cfRelease(t)
	}
	return win, title
}

// focusedElement returns the app's focused UI element (retained), or 0.
func focusedElement(pid int32) uintptr {
	el := axApp(pid)
	if el == 0 {
		return 0
	}
	defer cfRelease(el)
	return axCopy(el, "AXFocusedUIElement")
}

// FocusedText returns the text the app's focused element holds, as its
// AXValue gives it: a text field's or a text view's content. ok is false
// when the app has no focused element or the element's value is not text.
func FocusedText(pid int32) (text string, ok bool) {
	WithPool(func() {
		el := focusedElement(pid)
		if el == 0 {
			return
		}
		defer cfRelease(el)
		v := axCopy(el, "AXValue")
		if v == 0 {
			return
		}
		defer cfRelease(v)
		if !sendBool(objc.ID(v), "isKindOfClass:", class("NSString")) {
			return
		}
		text, ok = goString(objc.ID(v)), true
	})
	return text, ok
}

// Activate brings a captured app forward and its captured window to focus,
// waits up to timeout for both to hold, then gives the captured element focus
// again if it lost it inside the window. macOS 14+ may refuse a background
// app's activation request, so the AX raise and frontmost attributes back it
// up.
func Activate(a App, timeout time.Duration) error {
	var gone bool
	DoSync(func() {
		app := send(class("NSRunningApplication"), "runningApplicationWithProcessIdentifier:", a.PID)
		if app == 0 {
			gone = true
			return
		}
		send(app, "activateWithOptions:", uint(1<<1)) // NSApplicationActivateIgnoringOtherApps
	})
	if gone {
		return fmt.Errorf("%s (pid %d) has quit", a.Name, a.PID)
	}
	WithPool(func() {
		el := axApp(a.PID)
		if el == 0 {
			return
		}
		defer cfRelease(el)
		if a.Window != 0 {
			axUIElementSetAttributeValue(a.Window, uintptr(nsString("AXMain")), cfBooleanTrue)
			axUIElementPerformAction(a.Window, uintptr(nsString("AXRaise")))
		}
		axUIElementSetAttributeValue(el, uintptr(nsString("AXFrontmost")), cfBooleanTrue)
	})
	deadline := time.Now().Add(timeout)
	for {
		if a.Focused() {
			a.refocusElement()
			return nil
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("%s did not take focus within %v", a.Name, timeout)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// refocusElement puts keyboard focus back on the captured element when focus
// moved to another element of the same window.
func (a App) refocusElement() {
	if a.Element == 0 {
		return
	}
	WithPool(func() {
		cur := focusedElement(a.PID)
		if cur != 0 {
			defer cfRelease(cur)
			if cfEqual(cur, a.Element) {
				return
			}
		}
		if axUIElementSetAttributeValue(a.Element, uintptr(nsString("AXFocused")), cfBooleanTrue) == 0 {
			time.Sleep(50 * time.Millisecond)
		}
	})
}

// Focused reports whether the app is frontmost with the captured window focused.
func (a App) Focused() bool {
	var front int32
	DoSync(func() {
		app := send(send(class("NSWorkspace"), "sharedWorkspace"), "frontmostApplication")
		if app != 0 {
			front = objc.Send[int32](app, sel("processIdentifier"))
		}
	})
	if front != a.PID {
		return false
	}
	if a.Window == 0 {
		return true
	}
	var same bool
	WithPool(func() {
		win, _ := focusedWindow(a.PID)
		if win != 0 {
			same = cfEqual(win, a.Window)
			cfRelease(win)
		}
	})
	return same
}
