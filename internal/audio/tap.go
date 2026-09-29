package audio

import (
	"context"
	"errors"
	"time"
)

// ErrTapUnsupported is what Tap and Apps return where Core Audio process
// taps do not exist: off macOS, or on macOS before 14.2.
var ErrTapUnsupported = errors.New("process tap needs macOS 14.2 or later")

// Tap records what other apps play and the local microphone through one
// private aggregate device — the mic is its clock, the process tap a
// drift-compensated sub-tap — and delivers them as two Sources whose
// sample counts stay equal. Remote and Mic share the device: the first
// Start opens it, the first Stop closes it and ends both, and both report
// the same Err. Read both: a track nobody reads stalls the other, and after
// about ten seconds the device's blocks are dropped from both tracks alike
// (counted in Info().Overruns) — the tracks stay aligned, the take gets
// shorter. A device that stops calling back for 5 s ends the take with an
// error.
type Tap struct {
	// Apps are the bundle ids whose output is recorded (see Apps for the
	// running ones). Empty records every process's output.
	Apps []string
	// MicUID is the input device's UID; empty is the default input.
	MicUID string

	tapState // per-OS
}

// TapInfo describes an opened Tap, for the take's metadata.
type TapInfo struct {
	Mic       string    // input device name
	MicUID    string    // input device UID
	InputRate float64   // the aggregate's rate, before resampling to Rate
	First     time.Time // when the device delivered its first block: the wall clock of sample 0, to a block
	Padded    int       // remote frames the device did not deliver and the tap filled with silence, at Rate
	Overruns  int       // device callbacks dropped because the reader fell behind
}

// App is a Core Audio client process: what a Tap can record.
type App struct {
	PID    int
	Bundle string // bundle id; empty for a bare executable
	Name   string // process name
	Out    bool   // playing audio now
	In     bool   // capturing audio now
}

// Input is an audio input device: what Tap.MicUID and Mic.UID name.
type Input struct {
	UID      string `json:"uid"`
	Name     string `json:"name"`
	Channels int    `json:"channels"`          // input channels across its streams: Mic.Channel's range
	Default  bool   `json:"default,omitempty"` // the system's default input
}

// Remote is the tapped apps' output, mixed to mono.
func (t *Tap) Remote() Source { return tapTrack{t, 1} }

// Mic is the microphone, mixed to mono.
func (t *Tap) Mic() Source { return tapTrack{t, 0} }

type tapTrack struct {
	t *Tap
	i int
}

func (k tapTrack) Start(ctx context.Context) (<-chan []int16, error) { return k.t.start(ctx, k.i) }
func (k tapTrack) Stop()                                             { k.t.stop() }
func (k tapTrack) Err() error                                        { return k.t.err() }
