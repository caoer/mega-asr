//go:build darwin

package mac

import (
	"bytes"
	"errors"
	"fmt"
	"image/jpeg"
	"math"
	"strings"
	"sync"
	"unsafe"

	"github.com/ebitengine/purego"
	"github.com/ebitengine/purego/objc"

	"github.com/caoer/mega-asr/internal/refine"
)

// Screen is this Mac's screen for the model step's screen source
// (refine.Display): the Screen Recording grant, the window in front, its
// picture, its text by the Vision framework and its JPEG by ImageIO. The
// frameworks are loaded at the first use, so a process that never captures
// loads none of them.
type Screen struct{}

var (
	cgPreflightScreenCaptureAccess func() bool
	cgRequestScreenCaptureAccess   func() bool
	isSecureEventInputEnabled      func() bool

	cgWindowListCopyWindowInfo             func(option, relativeTo uint32) uintptr
	cgWindowListCreateImage                func(bounds CGRect, option, window, image uint32) uintptr
	cgRectMakeWithDictionaryRepresentation func(dict uintptr, r *CGRect) bool
	cgImageGetWidth, cgImageGetHeight      func(uintptr) uintptr
	cgImageRelease                         func(uintptr)
	cfArrayGetCount                        func(uintptr) int
	cfArrayGetValueAtIndex                 func(uintptr, int) uintptr
	cfDictionaryGetValue                   func(dict, key uintptr) uintptr
	cfNumberGetValue                       func(n uintptr, kind int, out unsafe.Pointer) bool

	cgImageDestinationCreateWithData func(data, kind uintptr, count int, options uintptr) uintptr
	cgImageDestinationAddImage       func(dest, image, properties uintptr)
	cgImageDestinationFinalize       func(dest uintptr) bool

	kWindowOwnerPID, kWindowLayer, kWindowNumber, kWindowName, kWindowAlpha, kWindowBounds uintptr
	kDestinationQuality, kDestinationMaxPixelSize                                          uintptr

	screenOnce sync.Once
	screenErr  error // a symbol this macOS does not have
)

func init() {
	purego.RegisterLibFunc(&cgPreflightScreenCaptureAccess, libCG, "CGPreflightScreenCaptureAccess")
	purego.RegisterLibFunc(&cgRequestScreenCaptureAccess, libCG, "CGRequestScreenCaptureAccess")
	purego.RegisterLibFunc(&isSecureEventInputEnabled, libCarbon, "IsSecureEventInputEnabled")
}

// loadScreen binds what a capture calls. A symbol this macOS lacks is an
// error every capture then returns: the take goes without the screen.
func loadScreen() error {
	screenOnce.Do(func() {
		defer func() {
			if r := recover(); r != nil {
				screenErr = fmt.Errorf("screen capture is not available on this macOS: %v", r)
			}
		}()
		imageIO := dlopen(fw + "ImageIO.framework/ImageIO")
		dlopen(fw + "Vision.framework/Vision")
		purego.RegisterLibFunc(&cgWindowListCopyWindowInfo, libCG, "CGWindowListCopyWindowInfo")
		purego.RegisterLibFunc(&cgWindowListCreateImage, libCG, "CGWindowListCreateImage")
		purego.RegisterLibFunc(&cgRectMakeWithDictionaryRepresentation, libCG, "CGRectMakeWithDictionaryRepresentation")
		purego.RegisterLibFunc(&cgImageGetWidth, libCG, "CGImageGetWidth")
		purego.RegisterLibFunc(&cgImageGetHeight, libCG, "CGImageGetHeight")
		purego.RegisterLibFunc(&cgImageRelease, libCG, "CGImageRelease")
		purego.RegisterLibFunc(&cfArrayGetCount, libCF, "CFArrayGetCount")
		purego.RegisterLibFunc(&cfArrayGetValueAtIndex, libCF, "CFArrayGetValueAtIndex")
		purego.RegisterLibFunc(&cfDictionaryGetValue, libCF, "CFDictionaryGetValue")
		purego.RegisterLibFunc(&cfNumberGetValue, libCF, "CFNumberGetValue")
		purego.RegisterLibFunc(&cgImageDestinationCreateWithData, imageIO, "CGImageDestinationCreateWithData")
		purego.RegisterLibFunc(&cgImageDestinationAddImage, imageIO, "CGImageDestinationAddImage")
		purego.RegisterLibFunc(&cgImageDestinationFinalize, imageIO, "CGImageDestinationFinalize")
		kWindowOwnerPID = symbolValue(libCG, "kCGWindowOwnerPID")
		kWindowLayer = symbolValue(libCG, "kCGWindowLayer")
		kWindowNumber = symbolValue(libCG, "kCGWindowNumber")
		kWindowName = symbolValue(libCG, "kCGWindowName")
		kWindowAlpha = symbolValue(libCG, "kCGWindowAlpha")
		kWindowBounds = symbolValue(libCG, "kCGWindowBounds")
		kDestinationQuality = symbolValue(imageIO, "kCGImageDestinationLossyCompressionQuality")
		kDestinationMaxPixelSize = symbolValue(imageIO, "kCGImageDestinationImageMaxPixelSize")
	})
	return screenErr
}

// Access reports the Screen Recording grant without prompting. The system
// answers for this run of the app: a grant given in System Settings shows
// once the app has been restarted.
func (Screen) Access() bool { return cgPreflightScreenCaptureAccess() }

// Request asks for the Screen Recording grant: the first time the system
// prompts and lists the app in System Settings › Privacy & Security ›
// Screen & System Audio Recording, where the toggle stays the user's. It
// reports the grant as it is now.
func (Screen) Request() bool { return cgRequestScreenCaptureAccess() }

const (
	cfNumberSInt64  = 4
	cfNumberFloat64 = 13
	// CGWindowListOption and CGWindowImageOption bits.
	windowsOnScreen        = 1 << 0
	windowsIncludingWindow = 1 << 3
	windowsNoDesktop       = 1 << 4
	imageBoundsIgnoreFrame = 1 << 0
	// minWindow is the smallest side, in points, of a window that can be
	// what someone is looking at.
	minWindow = 64
)

func dictInt(dict, key uintptr) (int64, bool) {
	var v int64
	n := cfDictionaryGetValue(dict, key)
	return v, n != 0 && cfNumberGetValue(n, cfNumberSInt64, unsafe.Pointer(&v))
}

// Front is the window in front: the first ordinary window in the system's
// front-to-back order, with its application. Secure is set when that
// application's focused element is a password field or the system's secure
// keyboard input is on.
func (Screen) Front() (f refine.Front, err error) {
	if err := loadScreen(); err != nil {
		return f, err
	}
	WithPool(func() {
		list := cgWindowListCopyWindowInfo(windowsOnScreen|windowsNoDesktop, 0)
		if list == 0 {
			err = errors.New("the window list is not readable")
			return
		}
		defer cfRelease(list)
		for i := range cfArrayGetCount(list) {
			w := cfArrayGetValueAtIndex(list, i)
			layer, _ := dictInt(w, kWindowLayer)
			pid, ok1 := dictInt(w, kWindowOwnerPID)
			id, ok2 := dictInt(w, kWindowNumber)
			var alpha float64
			if a := cfDictionaryGetValue(w, kWindowAlpha); a != 0 {
				cfNumberGetValue(a, cfNumberFloat64, unsafe.Pointer(&alpha))
			}
			var r CGRect
			if b := cfDictionaryGetValue(w, kWindowBounds); b != 0 {
				cgRectMakeWithDictionaryRepresentation(b, &r)
			}
			if layer != 0 || !ok1 || !ok2 || alpha == 0 || r.W < minWindow || r.H < minWindow {
				continue
			}
			f.PID, f.Window = int32(pid), uint32(id)
			f.Title = goString(objc.ID(cfDictionaryGetValue(w, kWindowName)))
			break
		}
		if f.Window == 0 {
			err = errors.New("no window is in front")
			return
		}
		if app := send(class("NSRunningApplication"), "runningApplicationWithProcessIdentifier:", f.PID); app != 0 {
			f.Bundle, f.Name = goString(send(app, "bundleIdentifier")), goString(send(app, "localizedName"))
		}
		f.Secure = isSecureEventInputEnabled() || passwordFocused(f.PID)
	})
	return f, err
}

// passwordFocused reports whether the app's focused element is a password
// field, as the Accessibility API names one. Without the Accessibility
// grant it asks nothing, since a question would bring up the system's
// prompt for that grant; the system's secure input still tells.
func passwordFocused(pid int32) bool {
	if !axIsProcessTrusted() {
		return false
	}
	el := focusedElement(pid)
	if el == 0 {
		return false
	}
	defer cfRelease(el)
	sub := axCopy(el, "AXSubrole")
	if sub == 0 {
		return false
	}
	defer cfRelease(sub)
	return goString(objc.ID(sub)) == "AXSecureTextField"
}

// Capture takes the picture of f's front window alone, at the display's
// own resolution, whatever covers it.
func (Screen) Capture(f refine.Front) (refine.Shot, error) {
	if err := loadScreen(); err != nil {
		return nil, err
	}
	null := CGRect{math.Inf(1), math.Inf(1), 0, 0} // CGRectNull: the window's own bounds
	img := cgWindowListCreateImage(null, windowsIncludingWindow, f.Window, imageBoundsIgnoreFrame)
	if img == 0 {
		return nil, fmt.Errorf("the window of %s gave no picture", f.Name)
	}
	return &shot{img: img}, nil
}

// shot is a window's picture, a retained CGImageRef.
type shot struct{ img uintptr }

func (s *shot) Size() (w, h int) { return int(cgImageGetWidth(s.img)), int(cgImageGetHeight(s.img)) }

func (s *shot) Release() {
	if s.img != 0 {
		cgImageRelease(s.img)
		s.img = 0
	}
}

// Text reads the picture's text with Vision's accurate recogniser, for
// Simplified Chinese and English, one recognised line per line, in the
// order Vision gives them.
func (s *shot) Text() (text string, err error) {
	WithPool(func() {
		req := send(send(class("VNRecognizeTextRequest"), "alloc"), "init")
		handler := send(send(class("VNImageRequestHandler"), "alloc"), "initWithCGImage:options:", s.img, send(class("NSDictionary"), "dictionary"))
		if req == 0 || handler == 0 {
			err = errors.New("text recognition is not available on this macOS")
			return
		}
		defer send(req, "release")
		defer send(handler, "release")
		langs := send(class("NSMutableArray"), "array")
		send(langs, "addObject:", nsString("zh-Hans"))
		send(langs, "addObject:", nsString("en-US"))
		send(req, "setRecognitionLevel:", 0) // VNRequestTextRecognitionLevelAccurate
		send(req, "setRecognitionLanguages:", langs)
		send(req, "setUsesLanguageCorrection:", true)
		var nserr objc.ID
		if !sendBool(handler, "performRequests:error:", send(class("NSArray"), "arrayWithObject:", req), unsafe.Pointer(&nserr)) {
			err = fmt.Errorf("text recognition: %s", goString(send(nserr, "localizedDescription")))
			return
		}
		results := send(req, "results")
		var lines []string
		for i := range sendInt(results, "count") {
			best := send(send(results, "objectAtIndex:", i), "topCandidates:", 1)
			if sendInt(best, "count") > 0 {
				lines = append(lines, goString(send(send(best, "objectAtIndex:", 0), "string")))
			}
		}
		text = strings.Join(lines, "\n")
	})
	return text, err
}

// jpegQuality is the compression of the picture sent: text stays sharp.
const jpegQuality = 0.8

// JPEG is the picture, scaled down when its long edge is over edge pixels.
func (s *shot) JPEG(edge int) (data []byte, w, h int, err error) {
	sw, sh := s.Size()
	WithPool(func() {
		out := send(class("NSMutableData"), "data")
		dest := cgImageDestinationCreateWithData(uintptr(out), uintptr(nsString("public.jpeg")), 1, 0)
		if dest == 0 {
			err = errors.New("no JPEG writer")
			return
		}
		defer cfRelease(dest)
		props := send(class("NSMutableDictionary"), "dictionary")
		send(props, "setObject:forKey:", send(class("NSNumber"), "numberWithDouble:", jpegQuality), kDestinationQuality)
		if max(sw, sh) > edge { // the writer would also scale a smaller picture up
			send(props, "setObject:forKey:", send(class("NSNumber"), "numberWithInt:", edge), kDestinationMaxPixelSize)
		}
		cgImageDestinationAddImage(dest, s.img, uintptr(props))
		if !cgImageDestinationFinalize(dest) {
			err = errors.New("the picture could not be written as a JPEG")
			return
		}
		data = make([]byte, sendInt(out, "length"))
		if len(data) > 0 {
			send(out, "getBytes:length:", unsafe.Pointer(&data[0]), len(data))
		}
	})
	if err != nil {
		return nil, 0, 0, err
	}
	c, err := jpeg.DecodeConfig(bytes.NewReader(data))
	if err != nil {
		return nil, 0, 0, fmt.Errorf("the picture's JPEG: %w", err)
	}
	return data, c.Width, c.Height, nil
}
