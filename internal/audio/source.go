// Package audio captures 16 kHz mono speech: a Source streams samples, and
// the helpers here write them as WAV and measure their level.
package audio

import (
	"context"
	"path/filepath"
)

// Rate is the sample rate every Source delivers and the ASR expects.
const Rate = 16000

// MinSamples is the shortest clip worth transcribing (0.3 s); a tap-tap
// shorter than this is a mistake, not speech.
const MinSamples = Rate * 3 / 10

// TooShort reports whether a clip of n samples is below MinSamples.
func TooShort(n int) bool { return n < MinSamples }

// Source streams 16 kHz mono signed 16-bit samples. The channel closes when
// the stream ends — after Stop has drained it, or on its own when the stream
// is cut; Err then says why, nil after a clean Stop.
type Source interface {
	Start(ctx context.Context) (<-chan []int16, error)
	Stop()
	Err() error
}

// TakeInput is the device a take was captured from, as the take's
// <base>.input.json records it. Source is capture.source's kind: "local" (this
// machine's input: CoreAudio on a Mac, ALSA on a mic server), "ssh" (arecord
// on Host), "remote" (a paired mic server) or "file" (a replay of a WAV).
type TakeInput struct {
	Source    string `json:"source"`
	Name      string `json:"name,omitempty"`      // local: the device's display name; remote: the box's name
	UID       string `json:"uid,omitempty"`       // local on a Mac: the CoreAudio device UID
	Transport string `json:"transport,omitempty"` // local on a Mac: CoreAudio's transport type, "usb", "bltn", "blue"…
	Host      string `json:"host,omitempty"`      // ssh: the ssh destination; remote: the box's host:port
	PCM       string `json:"pcm,omitempty"`       // ssh, local ALSA: the PCM arecord opened
	Channel   int    `json:"channel"`             // 0 mono; n the n-th channel alone (1..6 on the XVF3800)
	Note      string `json:"note,omitempty"`      // local: why another device than the pinned one records
	File      string `json:"file,omitempty"`      // file: the WAV replayed
}

// Bluetooth reports an input of this Mac on Bluetooth: it starts with up to
// BluetoothWarmup of digital silence when opened cold.
func (i TakeInput) Bluetooth() bool {
	return i.Source == "local" && (i.Transport == "blue" || i.Transport == "blea")
}

// DeviceName is the input as a person names it: the device's name, the mic
// server's, the ssh host's PCM, or the replayed file's.
func (i TakeInput) DeviceName() string {
	switch {
	case i.Source == "file":
		return filepath.Base(i.File)
	case i.Name != "":
		return i.Name
	case i.Source == "ssh":
		return i.Host + " " + i.PCM
	case i.PCM != "":
		return i.PCM
	}
	return i.UID
}

// Described is a Source that names the device it opened; Input is valid
// once Start has returned without error.
type Described interface{ Input() TakeInput }
