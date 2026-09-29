//go:build darwin

package mac

import (
	"os"
	"testing"

	"github.com/ebitengine/purego"
)

// TestMain runs the tests on their own goroutine and the main run loop on
// the main thread, so DoSync reaches AppKit as it does under Run.
func TestMain(m *testing.M) {
	var run func()
	purego.RegisterLibFunc(&run, libCF, "CFRunLoopRun")
	go func() { os.Exit(m.Run()) }()
	run()
}

// RunningApps marks exactly one app active: the frontmost one, as Frontmost
// reports it. The resend picker leaves that app out, since it is the
// browser showing the Takes page.
func TestRunningAppsMarksFrontmost(t *testing.T) {
	front := Frontmost()
	front.Release()
	var active []int32
	listed := false
	for _, a := range RunningApps() {
		listed = listed || a.PID == front.PID
		if a.Active {
			active = append(active, a.PID)
		}
	}
	if !listed {
		t.Skipf("the frontmost app (pid %d, %s) has no Dock icon, so RunningApps does not list it", front.PID, front.BundleID)
	}
	if len(active) != 1 || active[0] != front.PID {
		t.Fatalf("active pids %v, want exactly the frontmost %d (%s)", active, front.PID, front.BundleID)
	}
}
