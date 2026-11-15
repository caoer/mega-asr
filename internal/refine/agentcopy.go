package refine

import (
	"strings"
	"unicode"
)

// The marks that open the second part of a "both" delivery.
const (
	MarkEdits    = "[Mega Voice · heard → corrected]"
	MarkOriginal = "[Mega Voice · as heard]"
)

// Both is the one text a "both" delivery carries: the corrected text, a blank
// line, then the marked second part — the changes, heard → corrected, when
// the step fixed words (edits), the whole of heard when it rewrote the text.
// It is text alone when nothing changed.
func Both(heard, text string, edits []Edit) string {
	if text == heard {
		return text
	}
	if len(edits) == 0 {
		return text + "\n\n" + MarkOriginal + "\n" + heard
	}
	parts := make([]string, 0, len(edits))
	for _, e := range edits {
		from, to := Shown(e.From, e.To)
		parts = append(parts, from+" → "+to)
	}
	return text + "\n\n" + MarkEdits + " " + strings.Join(parts, " · ")
}

// Shown is how an edit reads to a person or an agent: from and to without
// the words they share at both ends. Unlike Changed it cuts only between
// words — at a space or punctuation, or beside a CJK character — so
// "super adviser" → "supervisor" stays whole. When a side would be empty
// the edit is shown whole.
func Shown(from, to string) (string, string) {
	f, t := []rune(from), []rune(to)
	p := 0
	for p < len(f) && p < len(t) && f[p] == t[p] {
		p++
	}
	for p > 0 && spaced(f[p-1]) && (p < len(f) && spaced(f[p]) || p < len(t) && spaced(t[p])) {
		p--
	}
	s := 0
	for s < len(f)-p && s < len(t)-p && f[len(f)-1-s] == t[len(t)-1-s] {
		s++
	}
	for s > 0 && spaced(f[len(f)-s]) && (len(f)-s > p && spaced(f[len(f)-s-1]) || len(t)-s > p && spaced(t[len(t)-s-1])) {
		s--
	}
	sf, st := string(f[p:len(f)-s]), string(t[p:len(t)-s])
	if sf == "" || st == "" {
		return from, to
	}
	return sf, st
}

// spaced reports whether r can run on into a longer word: a letter or digit
// of a script that spaces its words; a CJK character stands alone (wide).
func spaced(r rune) bool {
	return (unicode.IsLetter(r) || unicode.IsDigit(r)) && !wide(r)
}
