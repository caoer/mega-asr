package keypad

import (
	"slices"
	"strings"
	"testing"
)

func TestEncodeSnapshot(t *testing.T) {
	got := string(Snapshot{State: "recording", Send: true, Lock: true, Group: "报到群", Topic: "ccc-status", Done: DoneSent}.Encode())
	want := "state recording\nend send\nlock 1\ngroup 报到群\ntopic ccc-status\ndone sent\n"
	if got != want {
		t.Fatalf("Encode =\n%q\nwant\n%q", got, want)
	}
	// Zero fields read as paste, unlocked, no labels, done none.
	got = string(Snapshot{State: "idle"}.Encode())
	want = "state idle\nend paste\nlock 0\ngroup \ntopic \ndone none\n"
	if got != want {
		t.Fatalf("Encode(zero) = %q, want %q", got, want)
	}
	if b := (Snapshot{Group: "x"}).Encode(); b != nil {
		t.Fatalf("Encode with no state = %q, want nothing", b)
	}
}

// A long or hostile label reaches the wire as one line of at most MaxLabel
// bytes, never splitting a rune, and every line fits the firmware's buffer.
func TestEncodeTrimsLabels(t *testing.T) {
	long := strings.Repeat("蓝", 20) // 60 bytes
	b := Snapshot{State: "idle", Group: long, Topic: "a\nlock 1\r"}.Encode()
	ls := strings.Split(strings.TrimSuffix(string(b), "\n"), "\n")
	if len(ls) != 6 {
		t.Fatalf("got %d lines, want 6: %q", len(ls), b)
	}
	if want := "group " + strings.Repeat("蓝", 8); ls[3] != want { // 24 bytes
		t.Fatalf("group line = %q, want %q", ls[3], want)
	}
	if ls[4] != "topic a lock 1" {
		t.Fatalf("topic line = %q", ls[4])
	}
	for _, l := range ls {
		if len(l) > MaxLine {
			t.Fatalf("line over %d bytes: %q", MaxLine, l)
		}
	}
}

func TestTrimLabel(t *testing.T) {
	for in, want := range map[string]string{
		"  ccc-status  ":               "ccc-status",
		"exactly-twenty-four-byte":     "exactly-twenty-four-byte",
		"twenty-five-bytes-long-xy":    "twenty-five-bytes-long-x",
		"安昌的群 · 日常":                    "安昌的群 · 日常",
		strings.Repeat("ab", 11) + "群": strings.Repeat("ab", 11), // 22 + 3 bytes: the rune does not fit
		"tab\there":                    "tab here",
		"":                             "",
	} {
		if got := TrimLabel(in); got != want {
			t.Errorf("TrimLabel(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestParseEvent(t *testing.T) {
	good := map[string]Event{
		"knob 1":       {Kind: Knob, N: 1},
		"knob +2":      {Kind: Knob, N: 2},
		"knob -3":      {Kind: Knob, N: -3},
		"knob -1 held": {Kind: Knob, N: -1, Held: true},
		"knob 4 held":  {Kind: Knob, N: 4, Held: true},
		"key4":         {Kind: Key4},
		" key4 ":       {Kind: Key4},
		"key1 down":    {Kind: Key1Down},
		"key1 up":      {Kind: Key1Up},
	}
	for line, want := range good {
		if got, ok := ParseEvent(line); !ok || got != want {
			t.Errorf("ParseEvent(%q) = %+v, %v; want %+v", line, got, ok, want)
		}
	}
	for _, line := range []string{"", "knob", "knob 0", "knob x", "knob 1 pressed", "knob 1 held x", "key4 down", "key5", "state idle", "key1", "key1 held", "key1 down x", "key1 up x"} {
		if ev, ok := ParseEvent(line); ok {
			t.Errorf("ParseEvent(%q) = %+v, want ignored", line, ev)
		}
	}
}

// Lines split across reads join; CRLF is accepted; an overlong line is
// dropped whole and the stream picks up at the next newline.
func TestLineSplitter(t *testing.T) {
	var ls lines
	var got []string
	each := func(s string) { got = append(got, s) }
	ls.feed([]byte("kno"), each)
	ls.feed([]byte("b 1\r\nkey4\nknob"), each)
	ls.feed([]byte(" -1 held\n"), each)
	ls.feed([]byte(strings.Repeat("x", maxIn+10)), each)
	ls.feed([]byte("still noise\nkey4\n"), each)
	want := []string{"knob 1", "key4", "knob -1 held", "key4"}
	if !slices.Equal(got, want) {
		t.Fatalf("lines = %q, want %q", got, want)
	}
}

// The deadline's done word reaches the keypad as its own verb.
func TestEncodeDoneTimeout(t *testing.T) {
	if b := string((Snapshot{State: "idle", Done: DoneTimeout}).Encode()); !strings.Contains(b, "\ndone timeout\n") {
		t.Errorf("encoded %q", b)
	}
}
