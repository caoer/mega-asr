//go:build darwin

package mac

import (
	"errors"
	"runtime"
	"sync"
	"unsafe"

	"github.com/ebitengine/purego"
	"github.com/ebitengine/purego/objc"
)

var (
	connectivityOnce     sync.Once
	connectivityErr      error
	connectivityClass    objc.ID
	connectivityCallback uintptr
	connectivityMu       sync.Mutex
	connectivityHandlers = map[uintptr]func(){}
	scStoreCreate        func(uintptr, uintptr, uintptr, uintptr) uintptr
	scStoreSetKeys       func(uintptr, uintptr, uintptr) bool
	scStoreSource        func(uintptr, uintptr, int) uintptr
	cfRemoveSource       func(uintptr, uintptr, uintptr)
)

func connectivityNotify(key uintptr) {
	connectivityMu.Lock()
	f := connectivityHandlers[key]
	connectivityMu.Unlock()
	if f != nil {
		f()
	}
}

func connectivityBindings() error {
	connectivityOnce.Do(func() {
		lib, err := purego.Dlopen(fw+"SystemConfiguration.framework/SystemConfiguration", purego.RTLD_GLOBAL|purego.RTLD_NOW)
		if err != nil {
			connectivityErr = err
			return
		}
		purego.RegisterLibFunc(&scStoreCreate, lib, "SCDynamicStoreCreate")
		purego.RegisterLibFunc(&scStoreSetKeys, lib, "SCDynamicStoreSetNotificationKeys")
		purego.RegisterLibFunc(&scStoreSource, lib, "SCDynamicStoreCreateRunLoopSource")
		purego.RegisterLibFunc(&cfRemoveSource, libCF, "CFRunLoopRemoveSource")
		connectivityCallback = purego.NewCallback(func(_ uintptr, _ uintptr, info uintptr) { connectivityNotify(info) })
		cls, err := objc.RegisterClass("MegaVoiceConnectivityObserver", objc.GetClass("NSObject"), nil, nil, []objc.MethodDef{
			{Cmd: sel("changed:"), Fn: func(self objc.ID, _ objc.SEL, _ objc.ID) { connectivityNotify(uintptr(self)) }},
		})
		if err != nil {
			connectivityErr = err
			return
		}
		connectivityClass = objc.ID(cls)
	})
	return connectivityErr
}

// WatchConnectivity invalidates a sender immediately on workspace wake and
// IPv4/IPv6/default-route, DNS or proxy changes. Callbacks only enqueue work.
func WatchConnectivity(changed func()) (func(), error) {
	if err := connectivityBindings(); err != nil {
		return nil, err
	}
	var observer, center objc.ID
	var store, source uintptr
	var setupErr error
	DoSync(func() {
		observer = send(send(connectivityClass, "alloc"), "init")
		key := uintptr(observer)
		connectivityMu.Lock()
		connectivityHandlers[key] = changed
		connectivityMu.Unlock()
		center = send(send(class("NSWorkspace"), "sharedWorkspace"), "notificationCenter")
		send(center, "addObserver:selector:name:object:", observer, sel("changed:"), nsString("NSWorkspaceDidWakeNotification"), objc.ID(0))
		ctx := struct{ version, info, retain, release, description uintptr }{info: key}
		store = scStoreCreate(0, uintptr(nsString("MegaVoice Telegram connectivity")), connectivityCallback, uintptr(unsafe.Pointer(&ctx)))
		runtime.KeepAlive(&ctx)
		if store == 0 {
			setupErr = errors.New("SCDynamicStoreCreate failed")
			return
		}
		patterns := []objc.ID{
			nsString("State:/Network/Global/IPv4"), nsString("State:/Network/Global/IPv6"),
			nsString("State:/Network/Global/DNS"), nsString("State:/Network/Global/Proxies"),
			nsString("State:/Network/Service/.*/(IPv4|IPv6|DNS|Proxies)"),
		}
		array := send(class("NSArray"), "arrayWithObjects:count:", unsafe.Pointer(&patterns[0]), len(patterns))
		if !scStoreSetKeys(store, 0, uintptr(array)) {
			setupErr = errors.New("SCDynamicStoreSetNotificationKeys failed")
			return
		}
		runtime.KeepAlive(patterns)
		source = scStoreSource(0, store, 0)
		if source == 0 {
			setupErr = errors.New("SCDynamicStoreCreateRunLoopSource failed")
			return
		}
		cfRunLoopAddSource(cfRunLoopGetMain(), source, cfRunLoopCommonModes)
	})
	var once sync.Once
	stop := func() {
		once.Do(func() {
			connectivityMu.Lock()
			delete(connectivityHandlers, uintptr(observer))
			connectivityMu.Unlock()
			// Unregistration stays on AppKit's thread without making process shutdown
			// wait on that thread while it joins its Go workers.
			Do(func() {
				send(center, "removeObserver:", observer)
				if source != 0 {
					cfRemoveSource(cfRunLoopGetMain(), source, cfRunLoopCommonModes)
					cfRelease(source)
				}
				if store != 0 {
					cfRelease(store)
				}
				send(observer, "release")
			})
		})
	}
	if setupErr != nil {
		stop()
		return nil, setupErr
	}
	return stop, nil
}
