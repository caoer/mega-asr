//go:build darwin

package mac

import (
	"sync"

	"github.com/ebitengine/purego"
)

// Grants is what TCC reports for this process. TCC answers for the
// responsible process, so only the launchd-started app sees its own grants;
// the same call from a terminal reports the terminal's.
type Grants struct {
	Accessibility bool `json:"accessibility"` // AXIsProcessTrusted
	ListenEvents  bool `json:"listen_events"` // CGPreflightListenEventAccess (Input Monitoring)
	PostEvents    bool `json:"post_events"`   // CGPreflightPostEventAccess
	Microphone    bool `json:"microphone"`    // MicrophoneAuth is MicAuthorized
}

// MicAuth is the Microphone grant's state: AVAuthorizationStatus for audio.
type MicAuth int

const (
	MicNotDetermined MicAuth = iota // never asked: the first open of an input prompts
	MicRestricted                   // blocked by policy; the user cannot grant it
	MicDenied                       // an input opens and delivers silence
	MicAuthorized
)

func (a MicAuth) String() string {
	switch a {
	case MicNotDetermined:
		return "not determined"
	case MicRestricted:
		return "restricted"
	case MicDenied:
		return "denied"
	case MicAuthorized:
		return "authorized"
	}
	return "unknown"
}

var (
	avOnce           sync.Once
	avMediaTypeAudio uintptr
)

// MicrophoneAuth reads the Microphone grant without prompting.
func MicrophoneAuth() MicAuth {
	avOnce.Do(func() {
		avMediaTypeAudio = symbolValue(dlopen(fw+"AVFoundation.framework/AVFoundation"), "AVMediaTypeAudio")
	})
	var st int
	WithPool(func() {
		st = sendInt(class("AVCaptureDevice"), "authorizationStatusForMediaType:", avMediaTypeAudio)
	})
	return MicAuth(st)
}

var (
	axIsProcessTrusted            func() bool
	axIsProcessTrustedWithOptions func(uintptr) bool
	cgPreflightListenEventAccess  func() bool
	cgPreflightPostEventAccess    func() bool
	axTrustedCheckOptionPrompt    uintptr
)

func init() {
	purego.RegisterLibFunc(&axIsProcessTrusted, libAS, "AXIsProcessTrusted")
	purego.RegisterLibFunc(&axIsProcessTrustedWithOptions, libAS, "AXIsProcessTrustedWithOptions")
	purego.RegisterLibFunc(&cgPreflightListenEventAccess, libCG, "CGPreflightListenEventAccess")
	purego.RegisterLibFunc(&cgPreflightPostEventAccess, libCG, "CGPreflightPostEventAccess")
	axTrustedCheckOptionPrompt = symbolValue(libAS, "kAXTrustedCheckOptionPrompt")
}

// CheckGrants reads the grants without prompting.
func CheckGrants() Grants {
	return Grants{
		Accessibility: axIsProcessTrusted(),
		ListenEvents:  cgPreflightListenEventAccess(),
		PostEvents:    cgPreflightPostEventAccess(),
		Microphone:    MicrophoneAuth() == MicAuthorized,
	}
}

// PromptAccessibility lists this app in System Settings › Privacy & Security ›
// Accessibility (unchecked) and shows the system prompt; the toggle stays the
// user's. It reports whether the grant is already on.
func PromptAccessibility() bool {
	var ok bool
	WithPool(func() {
		yes := send(class("NSNumber"), "numberWithBool:", true)
		opts := send(class("NSDictionary"), "dictionaryWithObject:forKey:", yes, axTrustedCheckOptionPrompt)
		ok = axIsProcessTrustedWithOptions(uintptr(opts))
	})
	return ok
}
