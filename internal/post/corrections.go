package post

import (
	"bufio"
	"fmt"
	"io"
	"log"
	"os"
	"regexp"
	"sort"
	"strings"
	"unicode"
	"unicode/utf8"
)

// zhPrefix before a mishearing limits the rule to Chinese context.
const zhPrefix = "zh:"

// Rule replaces a known mishearing with the intended text.
//
// A mishearing with no upper-case letter matches in any case; one with an
// upper-case letter matches only as written. A Latin letter or digit at
// either end of the mishearing matches only at a word boundary, and Chinese
// text beside it counts as one. A Zh rule fires only where the nearest
// non-space character on one side is Chinese, which suits a mishearing that
// is also an ordinary English word.
type Rule struct {
	From, To string
	Zh       bool
	re       *regexp.Regexp
}

// Corrections is a correction table. Longer patterns apply first, so a short
// rule cannot break a longer one; of two the same length, the exact-case one
// applies first.
type Corrections []Rule

// ParseCorrections reads "mishearing<TAB>intended" lines; blank lines and
// lines starting with # are skipped. "zh:" before the mishearing makes a Zh
// rule.
func ParseCorrections(r io.Reader) (Corrections, error) {
	var c Corrections
	sc := bufio.NewScanner(r)
	for n := 1; sc.Scan(); n++ {
		line := strings.TrimRight(sc.Text(), "\r")
		if strings.TrimSpace(line) == "" || strings.HasPrefix(strings.TrimSpace(line), "#") {
			continue
		}
		from, to, ok := strings.Cut(line, "\t")
		zh := strings.HasPrefix(from, zhPrefix)
		from = strings.TrimPrefix(from, zhPrefix)
		if !ok || from == "" {
			return nil, fmt.Errorf("line %d: want mishearing<TAB>intended, got %q", n, line)
		}
		c = append(c, Rule{From: from, To: to, Zh: zh, re: compile(from)})
	}
	if err := sc.Err(); err != nil {
		return nil, err
	}
	sort.SliceStable(c, func(i, j int) bool {
		if len(c[i].From) != len(c[j].From) {
			return len(c[i].From) > len(c[j].From)
		}
		return !folds(c[i].From) && folds(c[j].From)
	})
	return c, nil
}

func folds(s string) bool { return !strings.ContainsFunc(s, unicode.IsUpper) }

func isWord(r rune) bool {
	return r < utf8.RuneSelf && (r == '_' || unicode.IsLetter(r) || unicode.IsDigit(r))
}

func compile(from string) *regexp.Regexp {
	p := regexp.QuoteMeta(from)
	if first, _ := utf8.DecodeRuneInString(from); isWord(first) {
		p = `\b` + p
	}
	if last, _ := utf8.DecodeLastRuneInString(from); isWord(last) {
		p += `\b`
	}
	if folds(from) {
		p = "(?i)" + p
	}
	return regexp.MustCompile(p)
}

// Apply rewrites every occurrence of every rule.
func (c Corrections) Apply(s string) string {
	for _, r := range c {
		s = r.apply(s)
	}
	return s
}

func (r Rule) apply(s string) string {
	if !r.Zh {
		return r.re.ReplaceAllLiteralString(s, r.To)
	}
	var b strings.Builder
	last := 0
	for _, m := range r.re.FindAllStringIndex(s, -1) {
		if !hanBeside(s, m[0], m[1]) {
			continue
		}
		b.WriteString(s[last:m[0]])
		b.WriteString(r.To)
		last = m[1]
	}
	if last == 0 {
		return s
	}
	b.WriteString(s[last:])
	return b.String()
}

// hanBeside reports whether the nearest non-space character before i or
// after j is a Chinese character.
func hanBeside(s string, i, j int) bool {
	before := strings.TrimRightFunc(s[:i], unicode.IsSpace)
	after := strings.TrimLeftFunc(s[j:], unicode.IsSpace)
	r1, _ := utf8.DecodeLastRuneInString(before)
	r2, _ := utf8.DecodeRuneInString(after)
	return unicode.Is(unicode.Han, r1) || unicode.Is(unicode.Han, r2)
}

// CorrectionsFile is a Stage that reads the table at path on every call, so
// an edit takes effect at the next utterance. A missing file corrects
// nothing; a malformed one is logged and skipped.
func CorrectionsFile(path string) Stage {
	return func(s string) string {
		f, err := os.Open(path)
		if err != nil {
			if !os.IsNotExist(err) {
				log.Printf("post: corrections: %v", err)
			}
			return s
		}
		defer f.Close()
		c, err := ParseCorrections(f)
		if err != nil {
			log.Printf("post: corrections %s: %v", path, err)
			return s
		}
		return c.Apply(s)
	}
}
