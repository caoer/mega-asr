package refine

import (
	"errors"
	"fmt"
	"strings"

	"github.com/caoer/mega-asr/internal/l10n"
)

// What a mode's answer is (Request.Output).
const (
	EditList = "edits" // the changes to make to the dictation
	FullText = "text"  // the whole text to deliver in its place
)

// FixWords is the mode that changes mis-heard words alone: the model
// answers with an edit list and the rest stays as said. Every other mode is
// a text mode: the model writes the text that is delivered.
const FixWords = "fix-words"

// Modes are the modes the app comes with: fix-words, then the text modes
// tidy (the dictation written as meant: fillers, repeats, punctuation) and
// translate (by meaning). A prompt file of any other name is a text mode
// too.
var Modes = []string{FixWords, "tidy", "translate"}

// ModeName is how the app names a mode to the person: the names of the
// modes it comes with, else the prompt file's name.
func ModeName(mode string) string {
	switch mode {
	case "", FixWords:
		return l10n.T("Fix Words Only", "只改词")
	case "tidy":
		return l10n.T("Rewrite", "转译")
	case "translate":
		return l10n.T("Translate", "按意思翻译")
	}
	return mode
}

// OutputOf is what mode asks the model for: an EditList for fix-words (and
// ""), the FullText for any other mode.
func OutputOf(mode string) string {
	if mode == "" || mode == FixWords {
		return EditList
	}
	return FullText
}

// CheckText is the check on a text mode's answer out for the dictation in:
// nil when out may be delivered, which is when it is not blank. What the
// model writes goes on screen as it wrote it, however much it reorganised.
func CheckText(mode, in, out string) error {
	if OutputOf(mode) != FullText {
		return fmt.Errorf("mode %q answers with an edit list, not a text", mode)
	}
	if strings.TrimSpace(out) == "" {
		return errors.New("the answer is blank")
	}
	return nil
}
