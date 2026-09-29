//go:build darwin

package mac

import "testing"

// MicrophoneAuth reads AVFoundation's status without prompting; from a test
// binary TCC answers for the terminal that started it.
func TestMicrophoneAuth(t *testing.T) {
	a := MicrophoneAuth()
	t.Logf("microphone: %s", a)
	if a < MicNotDetermined || a > MicAuthorized {
		t.Fatalf("status %d outside AVAuthorizationStatus", a)
	}
	if g := CheckGrants(); g.Microphone != (a == MicAuthorized) {
		t.Fatalf("Grants.Microphone %v with status %s", g.Microphone, a)
	}
}
