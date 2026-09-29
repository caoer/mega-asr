package score

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"unicode"
)

// The script check scores scripted clips against their script in units of its
// own (every Han character, every run of letters or digits, lower case),
// separate from Normalize's.

// ScriptFile is the check's script, beside the clips: one line per clip, its
// take's stamp (<stamp>.wav in the same directory), a tab, the sentence read.
const ScriptFile = "script.tsv"

var reScriptLine = regexp.MustCompile(`^(\S+)\t(.+)$`)

// ScriptClip is one scripted clip: its take's stamp (<stamp>.wav in the take
// store) and the sentence read.
type ScriptClip struct{ Stamp, Script string }

// ScriptClips reads dir's script, the clips in order.
func ScriptClips(dir string) ([]ScriptClip, error) {
	path := filepath.Join(dir, ScriptFile)
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var out []ScriptClip
	for _, l := range strings.Split(string(b), "\n") {
		if m := reScriptLine.FindStringSubmatch(strings.TrimRight(l, "\r")); m != nil {
			out = append(out, ScriptClip{m[1], m[2]})
		}
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("%s: no clip lines (stamp, tab, sentence)", path)
	}
	return out, nil
}

// ScriptUnits splits text into the script check's units.
func ScriptUnits(s string) []string {
	var u []string
	var word strings.Builder
	flush := func() {
		if word.Len() > 0 {
			u = append(u, strings.ToLower(word.String()))
			word.Reset()
		}
	}
	for _, r := range s {
		switch {
		case unicode.Is(unicode.Han, r):
			flush()
			u = append(u, string(r))
		case unicode.IsLetter(r) || unicode.IsDigit(r):
			word.WriteRune(r)
		default:
			flush()
		}
	}
	flush()
	return u
}

// EditDistance is the Levenshtein distance over units.
func EditDistance(a, b []string) int {
	prev := make([]int, len(b)+1)
	for j := range prev {
		prev[j] = j
	}
	for i := 1; i <= len(a); i++ {
		cur := make([]int, len(b)+1)
		cur[0] = i
		for j := 1; j <= len(b); j++ {
			c := prev[j-1]
			if a[i-1] != b[j-1] {
				c++
			}
			cur[j] = min(c, prev[j]+1, cur[j-1]+1)
		}
		prev = cur
	}
	return prev[len(b)]
}
