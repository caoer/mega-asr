package audio

import (
	"slices"
	"testing"
	"time"
)

// The inputs the tests choose among. None is the system default until a
// case makes it so with sysDefault.
var (
	podium   = inputDev{uid: "podium-condenser", name: "Podium Condenser", channels: 1, transport: "usb "}
	internal = inputDev{uid: "builtin-mic", name: "Built-in Microphone", channels: 1, transport: "bltn"}
	stage    = inputDev{uid: "stage-interface", name: "Stage Interface", channels: 4, transport: "usb "}
	handheld = inputDev{uid: "handheld-bt", name: "Handheld BT", channels: 1, transport: "blue"}
	boom     = inputDev{uid: "boom-mic", name: "Boom Mic", channels: 1, transport: "usb "}
)

func sysDefault(d inputDev) inputDev {
	d.def = true
	return d
}

// Each row is one outcome of pickInput: the default input, the pinned one
// when present, a fallback in its order of preference with the note that
// names the absent input (by pin, else by UID), or an error.
func TestPickInput(t *testing.T) {
	const none = ""
	for i, c := range []struct {
		uid, pin  string
		lidClosed bool
		devs      []inputDev
		want      string // UID, or the error
		note      string
	}{
		{podium.uid, none, false, []inputDev{handheld}, "podium-condenser not connected — no wired mic", none},
		{podium.uid, podium.name, true, []inputDev{internal, sysDefault(boom), stage}, boom.uid, "Podium Condenser not connected — using Boom Mic"},
		{none, none, false, []inputDev{boom, sysDefault(stage), handheld}, stage.uid, none},
		{podium.uid, podium.name, false, []inputDev{stage, podium}, podium.uid, none},
		{podium.uid, podium.name, false, []inputDev{stage, boom}, stage.uid, "Podium Condenser not connected — using Stage Interface"},
		{"default", none, true, []inputDev{boom, internal}, "no input device", none},
		{podium.uid, none, false, []inputDev{handheld, internal, sysDefault(stage)}, internal.uid, "podium-condenser not connected — using Built-in Microphone"},
	} {
		d, note, err := pickInput(c.uid, c.pin, c.devs, c.lidClosed)
		got := d.uid
		if err != nil {
			got = err.Error()
		}
		if got != c.want || note != c.note {
			t.Errorf("row %d: %q note %q, want %q note %q", i, got, note, c.want, c.note)
		}
	}
}

// pickBackup over one fixed set of wired inputs, varying what is asked for,
// which input is the main one and the lid; then the built-in as the last
// resort.
func TestPickBackup(t *testing.T) {
	devs := []inputDev{boom, internal, stage}
	for i, c := range []struct {
		uid, main string
		lidClosed bool
		want      string // UID, or the error
	}{
		{BackupAuto, stage.uid, false, boom.uid},
		{internal.uid, stage.uid, true, "backup Built-in Microphone is not a wired input"},
		{BackupAuto, boom.uid, true, stage.uid},
		{podium.uid, stage.uid, false, "backup podium-condenser not connected"},
		{boom.uid, boom.uid, false, "backup Boom Mic is the main input"},
		{internal.uid, boom.uid, false, internal.uid},
	} {
		d, err := pickBackup(c.uid, c.main, devs, c.lidClosed)
		got := d.uid
		if err != nil {
			got = err.Error()
		}
		if got != c.want {
			t.Errorf("row %d: %q, want %q", i, got, c.want)
		}
	}
	rest := []inputDev{stage, internal}
	if d, err := pickBackup(BackupAuto, stage.uid, rest, false); err != nil || d.uid != internal.uid {
		t.Errorf("only the built-in is left: %q, %v", d.uid, err)
	}
	if _, err := pickBackup(BackupAuto, stage.uid, rest, true); err == nil || err.Error() != "no wired input beside the main one for a backup" {
		t.Errorf("only a closed lid's built-in is left: %v", err)
	}
}

// Only a main Bluetooth input with a warm window is held; a backup never
// touches the hold, whatever it is.
func TestWarmFor(t *testing.T) {
	if got := warmFor(true, time.Minute, handheld); got != warmLeave {
		t.Errorf("Bluetooth backup: %v, want warmLeave", got)
	}
	if got := warmFor(false, 90*time.Second, podium); got != warmDrop {
		t.Errorf("wired main: %v, want warmDrop", got)
	}
	if got := warmFor(false, 45*time.Second, handheld); got != warmHold {
		t.Errorf("Bluetooth main: %v, want warmHold", got)
	}
	if got := warmFor(false, 0, handheld); got != warmDrop {
		t.Errorf("Bluetooth main, no warm window: %v, want warmDrop", got)
	}
}

func TestWired(t *testing.T) {
	for tr, want := range map[string]bool{
		"bltn": true, "usb ": true, "thun": true, "hdmi": true, "pci ": true, "ccwd": true,
		"blue": false, "blea": false, "virt": false, "grup": false, "fgrp": false, "airp": false, "ccwl": false,
	} {
		if got := (inputDev{transport: tr}).wired(); got != want {
			t.Errorf("%q wired %v, want %v", tr, got, want)
		}
	}
}

func TestMonoOf(t *testing.T) {
	// Two buffers: a stereo stream (channels 1, 2) and a mono one (channel 3).
	bufs := []ioBuf{
		{ch: 2, s: []float32{0.1, 0.3, 0.2, 0.4}},
		{ch: 1, s: []float32{0.6, 0.9}},
	}
	for _, c := range []struct {
		channel int
		want    []float32
	}{
		{0, []float32{(0.1 + 0.3 + 0.6) / 3, (0.2 + 0.4 + 0.9) / 3}},
		{1, []float32{0.1, 0.2}},
		{2, []float32{0.3, 0.4}},
		{3, []float32{0.6, 0.9}},
	} {
		got := monoOf(bufs, c.channel)
		if len(got) != len(c.want) {
			t.Fatalf("channel %d: %v, want %v", c.channel, got, c.want)
		}
		for i := range got {
			if d := got[i] - c.want[i]; d > 1e-6 || d < -1e-6 {
				t.Fatalf("channel %d: %v, want %v", c.channel, got, c.want)
			}
		}
	}
	// A short buffer bounds the block.
	if got := monoOf([]ioBuf{{ch: 1, s: []float32{1, 1, 1}}, {ch: 1, s: []float32{1}}}, 0); len(got) != 1 {
		t.Fatalf("frames %d, want 1", len(got))
	}
}

// A channel is 0 (the mix) or one of the device's own, counted from 1.
func TestCheckChannel(t *testing.T) {
	if err := checkChannel(stage, 4); err != nil {
		t.Errorf("the last of four: %v", err)
	}
	if err := checkChannel(podium, 0); err != nil {
		t.Errorf("the mix: %v", err)
	}
	for d, want := range map[int]string{
		5:  "Stage Interface has channels 1..4, not 5",
		-3: "Stage Interface has channels 1..4, not -3",
	} {
		if err := checkChannel(stage, d); err == nil || err.Error() != want {
			t.Errorf("channel %d: %v, want %q", d, err, want)
		}
	}
	if err := checkChannel(podium, 2); err == nil || err.Error() != "Podium Condenser has channels 1..1, not 2" {
		t.Errorf("a mono input's second channel: %v", err)
	}
}

// pumpRun drives a micPump over raw and returns what it delivered and the
// error it halted with.
func pumpRun(t *testing.T, feed func(raw chan<- []float32), first, stall time.Duration) ([]int16, error) {
	t.Helper()
	raw := make(chan []float32, 16)
	out := make(chan []int16, 64)
	var halted error
	halts := 0
	p := micPump{name: boom.name, raw: raw, out: out, rs: newResampler(Rate), first: first, stall: stall,
		halt: func(err error) {
			halts++
			halted = err
			close(raw)
		}}
	go p.run()
	go feed(raw)
	var got []int16
	deadline := time.After(5 * time.Second)
	for {
		select {
		case s, ok := <-out:
			if !ok {
				if halts > 1 {
					t.Fatalf("halted %d times", halts)
				}
				return got, halted
			}
			got = append(got, s...)
		case <-deadline:
			t.Fatal("the pump never closed its output")
		}
	}
}

func TestMicPumpStall(t *testing.T) {
	want := "Boom Mic stopped delivering audio"
	// No first block within its allowance.
	if _, err := pumpRun(t, func(chan<- []float32) {}, 30*time.Millisecond, time.Hour); err == nil || err.Error() != want {
		t.Fatalf("no first block: err %v, want %q", err, want)
	}
	// Blocks, then nothing for the stall window.
	got, err := pumpRun(t, func(raw chan<- []float32) {
		for range 5 {
			raw <- make([]float32, 320)
		}
	}, time.Hour, 50*time.Millisecond)
	if err == nil || err.Error() != want {
		t.Fatalf("stall: err %v, want %q", err, want)
	}
	if len(got) == 0 {
		t.Fatal("the blocks before the stall were not delivered")
	}
}

func TestMicPumpDrainsOnClose(t *testing.T) {
	block := make([]float32, 320)
	for i := range block {
		block[i] = 0.5
	}
	raw := make(chan []float32, 16)
	out := make(chan []int16, 64)
	p := micPump{name: "x", raw: raw, out: out, rs: newResampler(Rate), first: time.Hour, stall: time.Hour,
		halt: func(error) { t.Error("halted on a clean close") }}
	for range 10 {
		raw <- block
	}
	close(raw)
	p.run()
	var got []int16
	for s := range out {
		got = append(got, s...)
	}
	// All but the resampler's half-kernel of lookahead arrives, at the level fed.
	if len(got) < 3000 || !slices.Contains(got, toInt16(0.5)) {
		t.Fatalf("delivered %d samples", len(got))
	}
}
