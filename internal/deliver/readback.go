package deliver

import (
	"context"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"
)

// What a readback records on the received line after a deliver line: the
// text is in the target whole, short of what was sent, or it cannot be told.
// It is recorded only; nothing acts on it.
const (
	Whole   = "whole"
	Short   = "short"
	Unknown = "unknown"
)

const (
	// anchor is how many letters and digits of the text's head or tail
	// identify it in a read.
	anchor = 24
	// slack is how many letters and digits may follow the text on a screen:
	// the program's own lines under its input or under a sent message.
	slack = 400
	// screenLines is how many lines of a pane are read beyond the text's own.
	screenLines = 400
)

// When a readback reads. A pane is read twice after the last key it got,
// the paste or the Enter that sent it, and the deadline leaves room for a
// read that is slow on a busy host. An app's focused value is read once,
// while the settle before its Enter runs, since a chat field empties on
// Enter.
const (
	paneFirst     = 300 * time.Millisecond
	paneSecond    = 1500 * time.Millisecond
	paneDeadline  = 4 * time.Second
	fieldAfter    = 100 * time.Millisecond
	fieldDeadline = 1500 * time.Millisecond
)

// Claude Code folds a long paste into a chip; "+N lines" counts the newlines
// of what it received and is absent for a single line.
var chip = regexp.MustCompile(`\[Pasted text #\d+(?: \+(\d+) lines)?\]`)

// letters keeps the letters and digits of s: a screen wraps, indents and
// frames the text it shows, and a field may reflow it, but neither changes
// its letters.
func letters(s string) []rune {
	var out []rune
	for _, r := range s {
		if unicode.IsLetter(r) || unicode.IsDigit(r) {
			out = append(out, r)
		}
	}
	return out
}

// hasSuffix reports whether s ends with suffix.
func hasSuffix(s, suffix []rune) bool {
	return len(suffix) <= len(s) && slices.Equal(s[len(s)-len(suffix):], suffix)
}

// lastIndex is the rune offset of the last occurrence of sub in s, or -1.
func lastIndex(s, sub []rune) int {
	str := string(s)
	off := strings.LastIndex(str, string(sub))
	if off < 0 {
		return -1
	}
	return utf8.RuneCountInString(str[:off])
}

// field classifies a text field's or editor's value after a paste. The
// value is the whole content and a paste ends where the cursor was, so, as
// a pane's screen is read at its end, the value is read at its tail: the
// text is whole when the value ends with it, short when the value ends with
// its head and not the rest (the field took only so many letters), and
// unknown otherwise: the field was cleared, the paste has not landed, the
// app changed a letter of it, or it went inside the content. A copy of the
// text elsewhere in the value is never read as this paste.
func field(text, value string) (verdict, why string) {
	t, v := letters(text), letters(value)
	if len(t) == 0 || len(v) == 0 {
		return Unknown, "empty"
	}
	if hasSuffix(v, t) {
		return Whole, "found"
	}
	for m := len(t) - 1; m >= min(anchor, len(t)); m-- {
		if t[m-1] == v[len(v)-1] && hasSuffix(v, t[:m]) {
			return Short, "first " + strconv.Itoa(m) + " of " + strconv.Itoa(len(t))
		}
	}
	return Unknown, "not at the end"
}

// screen classifies one read of a terminal pane after a delivery. The text
// is the last thing the pane's program printed, followed by at most slack
// letters of its own. It is whole when it is found there; short when a
// Claude Code chip counts other lines than it has, when its tail is there
// and the letters before that tail's run are not the text's, or when its
// head is there and the run from it stops short. A run that reaches the top
// of the read, a chip without a count, and a read without the text are
// unknown. why says which.
func screen(text, read string) (verdict, why string) {
	t, r := letters(text), letters(read)
	if len(t) == 0 || len(r) == 0 {
		return Unknown, "empty"
	}
	if i := lastIndex(r, t); i >= 0 && len(r)-(i+len(t)) <= slack {
		return Whole, "found"
	}
	if cs := chip.FindAllStringSubmatchIndex(read, -1); cs != nil {
		c := cs[len(cs)-1]
		if len(letters(read[c[1]:])) <= slack {
			if c[2] < 0 {
				return Unknown, "chip"
			}
			n, _ := strconv.Atoi(read[c[2]:c[3]])
			if n != strings.Count(text, "\n") {
				return Short, "chip +" + strconv.Itoa(n) + " lines"
			}
			return Unknown, "chip"
		}
	}
	k := min(anchor, len(t))
	if i := lastIndex(r, t[len(t)-k:]); i >= 0 && len(r)-(i+k) <= slack {
		end, m := i+k, k
		for m < len(t) && end-m-1 >= 0 && r[end-m-1] == t[len(t)-m-1] {
			m++
		}
		if end-m == 0 {
			return Unknown, "the read starts inside the text"
		}
		return Short, "last " + strconv.Itoa(m) + " of " + strconv.Itoa(len(t))
	}
	if i := lastIndex(r, t[:k]); i >= 0 {
		m := k
		for m < len(t) && i+m < len(r) && r[i+m] == t[m] {
			m++
		}
		if len(r)-(i+m) <= slack {
			return Short, "first " + strconv.Itoa(m) + " of " + strconv.Itoa(len(t))
		}
	}
	return Unknown, "not found"
}

// readScreen reads a pane at first and at second after start and
// classifies what it shows: whole as soon as a read finds the text, short
// only when both reads find the same shortfall (a screen still drawing a
// paste looks short once), unknown otherwise, and for a read that errors or
// that ctx ends before. why is the second read's reason.
func readScreen(ctx context.Context, read func(context.Context) (string, error), text string, start time.Time, first, second time.Duration) (verdict, why string) {
	var got [2]string
	for i, at := range [2]time.Duration{first, second} {
		select {
		case <-time.After(time.Until(start.Add(at))):
		case <-ctx.Done():
			return Unknown, "no answer by the deadline"
		}
		s, err := read(ctx)
		if err != nil {
			return Unknown, "the read failed"
		}
		v, w := screen(text, s)
		if v == Whole {
			return Whole, w
		}
		got[i], why = v+" "+w, w
	}
	switch {
	case strings.HasPrefix(got[0], Short) && got[0] == got[1]:
		return Short, why
	case strings.HasPrefix(got[0], Short) && strings.HasPrefix(got[1], Short):
		return Unknown, "the two reads differ"
	}
	return Unknown, why
}

// before returns f's verdict, or unknown when f has not answered by
// deadline; f's context ends then, and so does f.
func before(deadline time.Time, f func(context.Context) (string, string)) (verdict, why string) {
	ctx, cancel := context.WithDeadline(context.Background(), deadline)
	defer cancel()
	type answer struct{ verdict, why string }
	ch := make(chan answer, 1)
	go func() {
		v, w := f(ctx)
		ch <- answer{v, w}
	}()
	select {
	case a := <-ch:
		return a.verdict, a.why
	case <-time.After(time.Until(deadline)):
		return Unknown, "no answer by the deadline"
	}
}

// received reads a pane back after a delivery that ended at sent. A pane
// can be told in two cases: an agent's pane after the Enter, which prints
// the sent message whole (before the Enter a long paste is a chip that
// gives no length), and another program's pane before any Enter, whose
// input line holds the paste (after an Enter a shell runs it). Any other
// case, a herdr error, and a readback past its deadline are unknown.
func (h Herdr) received(pane, text string, sent time.Time, entered bool) (verdict, why string) {
	return before(sent.Add(paneDeadline), func(ctx context.Context) (string, string) {
		sock, err := h.socket(ctx)
		if err != nil {
			return Unknown, "herdr did not answer"
		}
		agent, err := paneAgent(ctx, sock, pane)
		switch {
		case err != nil:
			return Unknown, "herdr did not answer"
		case agent != "" && !entered:
			return Unknown, "an agent's pane before its Enter"
		case agent == "" && entered:
			return Unknown, "a program's pane after its Enter"
		}
		lines := strings.Count(text, "\n") + screenLines
		read := func(ctx context.Context) (string, error) { return readPane(ctx, sock, pane, lines) }
		return readScreen(ctx, read, text, sent, paneFirst, paneSecond)
	})
}
