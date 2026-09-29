//go:build darwin

package audio

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

// Input is the device Start opened, as a take records it: its transport
// without the four-char padding, the channel asked for, and the fallback
// note.
func TestMicTakeInput(t *testing.T) {
	m := &Mic{UID: "podium-condenser", Name: "Podium Condenser", Channel: 3}
	m.dev = inputDev{uid: "stage-interface", name: "Stage Interface", channels: 4, transport: "usb "}
	m.note = "Podium Condenser not connected — using Stage Interface"
	want := TakeInput{Source: "local", Name: "Stage Interface", UID: "stage-interface", Transport: "usb", Channel: 3, Note: m.note}
	if got := m.Input(); got != want {
		t.Errorf("Input = %+v, want %+v", got, want)
	}
}

func TestInputsHaveChannels(t *testing.T) {
	in, err := Inputs()
	if err != nil {
		t.Fatal(err)
	}
	if len(in) == 0 {
		t.Skip("this Mac has no input device")
	}
	for _, i := range in {
		t.Logf("%-28s channels %d default %-5v %s", i.Name, i.Channels, i.Default, i.UID)
		if i.Channels <= 0 {
			t.Errorf("%s: channels %d", i.Name, i.Channels)
		}
	}
}

// An absent pinned UID resolves on this Mac's real devices and lid to a
// wired input, with the note — without opening anything.
func TestPickInputAbsentOnThisMac(t *testing.T) {
	if err := loadHAL(); err != nil {
		t.Fatal(err)
	}
	lid := lidClosed()
	d, note, err := pickInput("no-such-device", "Podium Condenser", inputDevs(), lid)
	t.Logf("lid closed %v: %s (%q) %q %v", lid, d.name, d.transport, note, err)
	if err != nil {
		t.Skip("no wired input on this Mac")
	}
	if !d.wired() || (lid && d.builtIn()) {
		t.Fatalf("fell back to %s (%q) with the lid closed %v", d.name, d.transport, lid)
	}
	if want := "Podium Condenser not connected — using " + d.name; note != want {
		t.Fatalf("note %q, want %q", note, want)
	}
}

// liveMic records from the input MEGAVOICE_MIC_UID names, never the
// default, which may be a Bluetooth input that opening switches into call
// mode. It runs only when that is set.
func liveMic(t *testing.T) string {
	uid := os.Getenv("MEGAVOICE_MIC_UID")
	if uid == "" || uid == "default" {
		t.Skip("MEGAVOICE_MIC_UID not set")
	}
	return uid
}

func record(t *testing.T, m *Mic, d time.Duration) []int16 {
	t.Helper()
	ch, err := m.Start(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	var s []int16
	done := make(chan struct{})
	go func() {
		defer close(done)
		for b := range ch {
			s = append(s, b...)
		}
	}()
	time.Sleep(d)
	m.Stop()
	<-done
	if err := m.Err(); err != nil {
		t.Fatal(err)
	}
	return s
}

// TestMicLive records 3 s from MEGAVOICE_MIC_UID, on channel
// MEGAVOICE_MIC_CHANNEL (default 0: the mix), prints the level and writes
// mic.wav to MEGAVOICE_MIC_OUT when that is set. A room is above -80 dBFS;
// exact zeros mean the Microphone grant is missing for the process that
// started this test.
func TestMicLive(t *testing.T) {
	uid := liveMic(t)
	m := &Mic{UID: uid, Tail: 300 * time.Millisecond}
	m.Channel, _ = strconv.Atoi(os.Getenv("MEGAVOICE_MIC_CHANNEL"))
	start := time.Now()
	s := record(t, m, 3*time.Second)
	rms, peak := Stats(s)
	t.Logf("device %q channel %d: %d samples (%.2f s) in %v, RMS %.1f dBFS, peak %.1f dBFS, note %q",
		m.Device(), m.Channel, len(s), float64(len(s))/Rate, time.Since(start).Round(time.Millisecond), rms, peak, m.Note())
	if out := os.Getenv("MEGAVOICE_MIC_OUT"); out != "" {
		if err := SaveWAV(filepath.Join(out, "mic.wav"), s); err != nil {
			t.Fatal(err)
		}
	}
	if len(s) < 3*Rate {
		t.Errorf("%d samples, want at least 3 s", len(s))
	}
	if rms <= DeadLevel {
		t.Errorf("RMS %.1f dBFS: digital silence — is Microphone allowed for this terminal?", rms)
	}
}

// TestMicLiveFallback pins a UID that is not connected and records half a
// second from what it falls back to — only when that is the input
// MEGAVOICE_MIC_UID names, the one cleared for opening.
func TestMicLiveFallback(t *testing.T) {
	uid := liveMic(t)
	if err := loadHAL(); err != nil {
		t.Fatal(err)
	}
	d, _, err := pickInput("no-such-device", "", inputDevs(), lidClosed())
	if err != nil || d.uid != uid {
		t.Skipf("the fallback here is %q, not MEGAVOICE_MIC_UID", d.name)
	}
	m := &Mic{UID: "no-such-device", Name: "Podium Condenser"}
	s := record(t, m, 500*time.Millisecond)
	t.Logf("device %q note %q samples %d", m.Device(), m.Note(), len(s))
	if want := "Podium Condenser not connected — using " + d.name; m.Note() != want || m.Device() != d.name {
		t.Fatalf("device %q note %q, want %q %q", m.Device(), m.Note(), d.name, want)
	}
}

// lidClosed agrees with ioreg's AppleClamshellState; a Mac without a lid
// has none and reports false.
func TestLidClosed(t *testing.T) {
	out, err := exec.Command("ioreg", "-r", "-k", "AppleClamshellState", "-d", "1").Output()
	if err != nil {
		t.Fatal(err)
	}
	want := strings.Contains(string(out), `"AppleClamshellState" = Yes`)
	if got := lidClosed(); got != want {
		t.Fatalf("lidClosed %v, ioreg says %v", got, want)
	}
	t.Logf("lid closed: %v", want)
}
