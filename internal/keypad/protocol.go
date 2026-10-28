package keypad

import (
	"strconv"
	"strings"
	"unicode"
	"unicode/utf8"
)

// The line protocol, apart from whatever carries it (Transport). UTF-8 text
// lines of at most MaxLine bytes, each ended by '\n'.
//
// Mac → keypad, the whole Snapshot at every change and every 2 s:
//
//	state idle|recording|transcribing|delivering
//	end paste|send
//	lock 0|1
//	group <label>
//	topic <label>
//	done none|pasted|sent|failed|timeout
//
// Keypad → Mac: "knob <±n>", "knob <±n> held", "key4",
// "key1 down", "key1 up" (see Event).
const (
	// MaxLine is the longest line the firmware buffers, newline excluded.
	MaxLine = 63
	// MaxLabel is how many bytes of a group or topic label reach the keypad.
	MaxLabel = 24
)

// Done is how the last delivery ended, shown on the keypad for DoneHold.
type Done string

const (
	DoneNone   Done = "none"
	DonePasted Done = "pasted"
	DoneSent   Done = "sent"
	DoneFailed Done = "failed"
	// DoneTimeout: the send ran out of time and may have posted all the same.
	DoneTimeout Done = "timeout"
)

// Snapshot is everything the keypad draws. The firmware keeps only the last
// one it received.
type Snapshot struct {
	State string // idle|recording|transcribing|delivering; "" sends nothing
	Send  bool   // the take in flight presses Enter: "end send", else "end paste"
	Lock  bool   // key 1 delivers to Group/Topic
	Group string // picked group's label, trimmed to MaxLabel on the wire
	Topic string // picked topic's label, trimmed to MaxLabel on the wire
	Done  Done   // "" is DoneNone
	// Chat and TopicID travel only inside the Mac, to bind a key edge to
	// the destination of the snapshot it followed. Encode sends no ids.
	Chat    string
	TopicID int64
}

// Encode renders the snapshot as its lines, nil when State is empty.
func (s Snapshot) Encode() []byte {
	if s.State == "" {
		return nil
	}
	end, lock, done := "paste", "0", s.Done
	if s.Send {
		end = "send"
	}
	if s.Lock {
		lock = "1"
	}
	if done == "" {
		done = DoneNone
	}
	var b strings.Builder
	for _, l := range []string{
		"state " + oneLine(s.State),
		"end " + end,
		"lock " + lock,
		"group " + TrimLabel(s.Group),
		"topic " + TrimLabel(s.Topic),
		"done " + oneLine(string(done)),
	} {
		b.WriteString(cut(l, MaxLine))
		b.WriteByte('\n')
	}
	return []byte(b.String())
}

// TrimLabel makes a label safe to draw: control characters become spaces,
// surrounding space goes, and it is cut to MaxLabel bytes on a rune boundary.
func TrimLabel(s string) string {
	return strings.TrimSpace(cut(strings.TrimSpace(oneLine(s)), MaxLabel))
}

func oneLine(s string) string {
	return strings.Map(func(r rune) rune {
		if unicode.IsControl(r) || r == utf8.RuneError {
			return ' '
		}
		return r
	}, s)
}

// cut shortens s to at most n bytes without splitting a rune.
func cut(s string, n int) string {
	if len(s) <= n {
		return s
	}
	for n > 0 && !utf8.RuneStart(s[n]) {
		n--
	}
	return s[:n]
}

// EventKind names what the keypad reported.
type EventKind int

const (
	Knob     EventKind = iota + 1 // the knob turned N detents (negative: anticlockwise)
	Key4                          // key 4 went down
	Key1Down                      // key 1 went down
	Key1Up                        // key 1 went up
)

// Event is one line from the keypad.
type Event struct {
	Kind EventKind
	N    int  // Knob: detents turned, never 0
	Held bool // Knob: turned with the knob pressed in
	// Connection identifies a key-1 edge's port session, assigned by the link.
	Connection uint64
	// Shown is the last full snapshot written before this key-1 edge.
	Shown Snapshot
}

// ParseEvent reads one keypad line, its newline already removed; ok is false
// for anything that is not an event, which the link ignores.
func ParseEvent(line string) (ev Event, ok bool) {
	f := strings.Fields(line)
	switch {
	case len(f) == 2 && f[0] == "key1" && f[1] == "down":
		return Event{Kind: Key1Down}, true
	case len(f) == 2 && f[0] == "key1" && f[1] == "up":
		return Event{Kind: Key1Up}, true
	case len(f) == 1 && f[0] == "key4":
		return Event{Kind: Key4}, true
	case (len(f) == 2 || len(f) == 3 && f[2] == "held") && f[0] == "knob":
		n, err := strconv.Atoi(f[1])
		if err != nil || n == 0 {
			return Event{}, false
		}
		return Event{Kind: Knob, N: n, Held: len(f) == 3}, true
	}
	return Event{}, false
}

// lines splits a byte stream into lines. A line longer than maxIn bytes is
// noise (a reset mid-line, a stray boot log) and is dropped whole.
type lines struct {
	buf  []byte
	skip bool // inside an overlong line, dropping until its newline
}

const maxIn = 256

func (l *lines) feed(b []byte, each func(string)) {
	for _, c := range b {
		switch {
		case c == '\n':
			if !l.skip {
				each(strings.TrimRight(string(l.buf), "\r"))
			}
			l.buf, l.skip = l.buf[:0], false
		case l.skip:
		case len(l.buf) == maxIn:
			l.buf, l.skip = l.buf[:0], true
		default:
			l.buf = append(l.buf, c)
		}
	}
}
