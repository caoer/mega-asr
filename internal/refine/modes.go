package refine

import (
	"errors"
	"fmt"
	"unicode"
	"unicode/utf8"
)

// What a mode's answer is (Request.Output).
const (
	EditList = "edits" // the changes to make to the dictation
	FullText = "text"  // the whole text to deliver in its place
)

// Modes are the modes the step knows: fix-words swaps mis-heard words and
// answers with an edit list; tidy (fillers, repeats, punctuation) and
// translate (by meaning) answer with the whole text, under CheckText.
var Modes = []string{"fix-words", "tidy", "translate"}

// OutputOf is what mode asks the model for: FullText for tidy and
// translate, else an EditList.
func OutputOf(mode string) string {
	switch mode {
	case "tidy", "translate":
		return FullText
	}
	return EditList
}

// The full-text guards: past them an answer is not the dictation tidied or
// translated but something in its place, such as a reply to it, and the
// dictation is delivered as it stood.
const (
	// tidyKeep: tidy keeps this share of the dictation's characters, in
	// order; removing fillers and repeats takes out less.
	tidyKeep = 0.6
	// tidyAdd: tidy adds at most this share of the dictation's characters,
	// plus tidySlack, for punctuation and a fixed casing.
	tidyAdd   = 0.25
	tidySlack = 8
	// A translation is at least an eighth of the dictation's length and at
	// most six times it plus translateSlack, in characters: English to
	// Chinese runs to about a sixth, Chinese to English to about four.
	translateMin   = 8
	translateMax   = 6
	translateSlack = 16
	// maxCells bounds the comparison tidy makes (characters in × out).
	maxCells = 64 << 20
)

// CheckText is the guard of a full-text mode over its answer out for the
// dictation in: nil when out may be delivered.
func CheckText(mode, in, out string) error {
	n, m := utf8.RuneCountInString(in), utf8.RuneCountInString(out)
	if m == 0 {
		return errors.New("the answer is empty")
	}
	switch mode {
	case "tidy":
		if n*m > maxCells {
			return fmt.Errorf("%d and %d characters are too long to compare", n, m)
		}
		kept := common(in, out)
		if float64(kept) < tidyKeep*float64(n) {
			return fmt.Errorf("the answer keeps %d of %d characters, under %.0f%%", kept, n, tidyKeep*100)
		}
		if added := m - kept; float64(added) > tidyAdd*float64(n)+tidySlack {
			return fmt.Errorf("the answer adds %d characters to %d", added, n)
		}
	case "translate":
		if m*translateMin < n || m > translateMax*n+translateSlack {
			return fmt.Errorf("the answer is %d characters for %d, outside an eighth to six times", m, n)
		}
	default:
		return fmt.Errorf("mode %q answers with an edit list, not a text", mode)
	}
	return nil
}

// common is the length of the longest common subsequence of a and b in
// characters, letter case aside: what of a is still in b, in order.
func common(a, b string) int {
	x, y := []rune(a), []rune(b)
	prev, cur := make([]int, len(y)+1), make([]int, len(y)+1)
	for i := range x {
		for j := range y {
			switch {
			case unicode.ToLower(x[i]) == unicode.ToLower(y[j]):
				cur[j+1] = prev[j] + 1
			case prev[j+1] >= cur[j]:
				cur[j+1] = prev[j+1]
			default:
				cur[j+1] = cur[j]
			}
		}
		prev, cur = cur, prev
	}
	return prev[len(y)]
}
