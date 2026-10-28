//go:build darwin

package mac

import (
	"errors"
	"fmt"
	"sync"
	"unsafe"

	"github.com/ebitengine/purego/objc"
)

// The login item is SMAppService's main app service (macOS 13+): the app
// itself, opened at login as if from Finder, with no launchd plist to write.

var smOnce sync.Once

func mainAppService() objc.ID {
	smOnce.Do(func() { dlopen(fw + "ServiceManagement.framework/ServiceManagement") })
	if class("SMAppService") == 0 {
		return 0
	}
	return send(class("SMAppService"), "mainAppService")
}

// LoginItemOn reports whether this app opens at login.
func LoginItemOn() bool {
	s := mainAppService()
	return s != 0 && sendInt(s, "status") == 1 // SMAppServiceStatusEnabled
}

// SetLoginItem makes this app open at login (on) or no longer (off).
func SetLoginItem(on bool) error {
	s := mainAppService()
	if s == 0 {
		return errors.New("login item: SMAppService is not available")
	}
	name := "registerAndReturnError:"
	if !on {
		name = "unregisterAndReturnError:"
	}
	var nserr objc.ID
	if sendBool(s, name, unsafe.Pointer(&nserr)) {
		return nil
	}
	why := "unknown error"
	if nserr != 0 {
		why = goString(send(nserr, "localizedDescription"))
	}
	return fmt.Errorf("login item: %s", why)
}
