package audio

import (
	"bytes"
	"encoding/binary"
	"os/exec"
	"strings"
	"testing"
)

func frames(n int) []byte {
	b := make([]byte, n*2*Channels)
	for i := range n {
		for c := range Channels {
			// sample value encodes frame and channel: 100*frame + channel(1-based)
			binary.LittleEndian.PutUint16(b[(i*Channels+c)*2:], uint16(int16(100*i+c+1)))
		}
	}
	return b
}

func TestDeinterleaveAcrossChunks(t *testing.T) {
	b := frames(5)
	d := Deinterleaver{Channels: Channels, Channel: 2}
	var got []int16
	// split at odd offsets: mid-sample and mid-frame
	for _, cut := range [][2]int{{0, 7}, {7, 13}, {13, 40}, {40, len(b)}} {
		got = append(got, d.Push(b[cut[0]:cut[1]])...)
	}
	want := []int16{2, 102, 202, 302, 402}
	if len(got) != len(want) {
		t.Fatalf("got %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("got %v, want %v", got, want)
		}
	}
}

func TestShortClip(t *testing.T) {
	for _, c := range []struct {
		n    int
		want bool
	}{{0, true}, {4799, true}, {4800, false}, {16000, false}} {
		if got := TooShort(c.n); got != c.want {
			t.Errorf("TooShort(%d) = %v, want %v", c.n, got, c.want)
		}
	}
}

// arecord hands over 20 ms periods (-F) from a 200 ms buffer (-B): its
// default period is 125 ms, which a stop waits out. The shell ends it when
// its stdin closes.
func TestRemoteEndsOnStdinClose(t *testing.T) {
	x := XVF{Device: "hw:Array,0", Channel: 2}
	want := "arecord -q -D hw:Array,0 -f S16_LE -r 16000 -c 6 -t raw -F 20000 -B 200000 - & cat >/dev/null; kill -INT $! 2>/dev/null; wait"
	if got := x.remote(); got != want {
		t.Errorf("remote = %q", got)
	}
}

// A host runs the shell over ssh; no host runs it on this machine.
func TestCommandByHost(t *testing.T) {
	x := XVF{Host: "micbox", Device: "hw:Array,0", CtlPath: "/s/cm"}
	name, args := x.command()
	if name != "ssh" || args[len(args)-2] != "micbox" || args[len(args)-1] != x.remote() {
		t.Errorf("ssh: %s %q", name, args)
	}
	x.Host = ""
	name, args = x.command()
	if name != "sh" || len(args) != 2 || args[0] != "-c" || args[1] != x.remote() {
		t.Errorf("local: %s %q", name, args)
	}
}

// Channel 0 records the PCM in mono: arecord asks for one channel and ALSA
// mixes, so any device works; n opens the XVF3800's six.
func TestMonoOpensOneChannel(t *testing.T) {
	x := XVF{Device: "default"}
	want := "arecord -q -D default -f S16_LE -r 16000 -c 1 -t raw -F 20000 -B 200000 - & cat >/dev/null; kill -INT $! 2>/dev/null; wait"
	if got := x.remote(); got != want {
		t.Errorf("remote = %q", got)
	}
}

// A device that is not six channels, read for channel n, fails naming the
// device and the rule, not arecord's words alone.
func TestNotSixChannels(t *testing.T) {
	x := XVF{Device: "hw:USB,0", Channel: 2}
	err := x.unavailable("arecord: set_params:1398: Channels count non available")
	for _, w := range []string{"hw:USB,0", "not a 6-channel", "channel 0"} {
		if err == nil || !strings.Contains(err.Error(), w) {
			t.Errorf("err %v lacks %q", err, w)
		}
	}
	x.Host = "micbox"
	if err := x.unavailable("arecord: main:850: audio open error: Device or resource busy"); err == nil || !strings.Contains(err.Error(), "array unavailable: arecord: main:850") {
		t.Errorf("other failure: %v", err)
	}
}

// A take on channel 0 delivers the PCM's samples as they are.
func TestMonoPassesSamplesThrough(t *testing.T) {
	x := XVF{Device: "default", Channel: 0}
	x.cmd = exec.Command("true")
	if err := x.cmd.Start(); err != nil {
		t.Skip(err)
	}
	x.done = make(chan struct{})
	ch := make(chan []int16, 4)
	b := []byte{1, 0, 2, 0, 3}
	go x.pump(bytes.NewReader(b), ch)
	var got []int16
	for s := range ch {
		got = append(got, s...)
	}
	if len(got) != 2 || got[0] != 1 || got[1] != 2 {
		t.Fatalf("got %v", got)
	}
}
