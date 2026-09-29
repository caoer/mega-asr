//go:build darwin

// Package mac drives AppKit, CoreGraphics and the Accessibility API through
// the Objective-C runtime with purego — no cgo. AppKit objects are touched on
// the main thread only (Do, DoSync); CoreGraphics event calls and AX calls run
// on any thread inside WithPool.
package mac

import (
	"runtime"
	"sync"
	"unsafe"

	"github.com/ebitengine/purego"
	"github.com/ebitengine/purego/objc"
)

func dlopen(path string) uintptr {
	h, err := purego.Dlopen(path, purego.RTLD_GLOBAL|purego.RTLD_NOW)
	if err != nil {
		panic("mac: " + err.Error())
	}
	return h
}

const fw = "/System/Library/Frameworks/"

var (
	libObjC   = dlopen("/usr/lib/libobjc.A.dylib")
	libSystem = dlopen("/usr/lib/libSystem.B.dylib")
	libCF     = dlopen(fw + "CoreFoundation.framework/CoreFoundation")
	libCG     = dlopen(fw + "CoreGraphics.framework/CoreGraphics")
	libAS     = dlopen(fw + "ApplicationServices.framework/ApplicationServices")
	libCarbon = dlopen(fw + "Carbon.framework/Carbon")
	libAppKit = dlopen(fw + "AppKit.framework/AppKit")
	_         = dlopen(fw + "QuartzCore.framework/QuartzCore")
)

// symbolValue reads a C global that holds a pointer (a CFStringRef constant,
// a CFBooleanRef) from the image that exports it.
func symbolValue(lib uintptr, name string) uintptr {
	addr, err := purego.Dlsym(lib, name)
	if err != nil {
		panic("mac: " + err.Error())
	}
	return **(**uintptr)(unsafe.Pointer(&addr))
}

var (
	poolPush  func() uintptr
	poolPop   func(uintptr)
	cfRetain  func(uintptr) uintptr
	cfRelease func(uintptr)
	cfEqual   func(uintptr, uintptr) bool
)

func init() {
	purego.RegisterLibFunc(&poolPush, libObjC, "objc_autoreleasePoolPush")
	purego.RegisterLibFunc(&poolPop, libObjC, "objc_autoreleasePoolPop")
	purego.RegisterLibFunc(&cfRetain, libCF, "CFRetain")
	purego.RegisterLibFunc(&cfRelease, libCF, "CFRelease")
	purego.RegisterLibFunc(&cfEqual, libCF, "CFEqual")
}

// WithPool runs fn inside an autorelease pool, pinned to one OS thread so the
// pool is pushed and popped on the same thread.
func WithPool(fn func()) {
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()
	p := poolPush()
	defer poolPop(p)
	fn()
}

var sels sync.Map

func sel(name string) objc.SEL {
	if s, ok := sels.Load(name); ok {
		return s.(objc.SEL)
	}
	s := objc.RegisterName(name)
	sels.Store(name, s)
	return s
}

func class(name string) objc.ID { return objc.ID(objc.GetClass(name)) }

func send(id objc.ID, name string, args ...any) objc.ID { return id.Send(sel(name), args...) }

func sendBool(id objc.ID, name string, args ...any) bool {
	return objc.Send[bool](id, sel(name), args...)
}

func sendInt(id objc.ID, name string, args ...any) int {
	return objc.Send[int](id, sel(name), args...)
}

// nsString returns an autoreleased NSString (toll-free a CFStringRef).
func nsString(s string) objc.ID {
	return send(class("NSString"), "stringWithUTF8String:", s)
}

func goString(id objc.ID) string {
	if id == 0 {
		return ""
	}
	return objc.Send[string](id, sel("UTF8String"))
}
