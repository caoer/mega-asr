package asr

import (
	"bufio"
	"log"
	"os"
	"strings"
	"unicode/utf8"
)

// MaxHotwords caps FunASR's list; the CLI also drops trailing terms past 512
// prompt tokens.
const MaxHotwords = 50

// Doubao's corpus.context hotword limits (doc 6561/1354869), as measured on
// the plan endpoint: the streaming pass reads the terms joined with one
// character between them up to DoubaoStreamChars characters (CJK and Latin
// letters and spaces alike) and silently ignores the rest; the two-pass
// re-decode reads past that, up to DoubaoMaxHotwords terms.
const (
	DoubaoStreamChars = 100
	DoubaoMaxHotwords = 5000
)

// ReadHotwords reads FunASR's list: one term per line from each path in
// turn, at most MaxHotwords over all the paths.
func ReadHotwords(paths ...string) []string { return ReadHotwordsMax(MaxHotwords, paths...) }

// ReadHotwordsMax reads one term per line from each path in turn, as one
// list; blank lines and # comments are skipped, a term with a comma is
// dropped (the CLI splits on commas), a term already listed is skipped, and
// terms past max over all the paths are dropped. A missing file adds nothing.
func ReadHotwordsMax(max int, paths ...string) []string {
	var terms []string
	seen := map[string]bool{}
	past := 0
	for _, path := range paths {
		f, err := os.Open(path)
		if err != nil {
			if !os.IsNotExist(err) {
				log.Printf("asr: hotwords: %v", err)
			}
			continue
		}
		sc := bufio.NewScanner(f)
		for sc.Scan() {
			t := strings.TrimSpace(sc.Text())
			switch {
			case t == "" || strings.HasPrefix(t, "#") || seen[t]:
			case strings.Contains(t, ","):
				log.Printf("asr: hotwords: %q dropped: a term cannot contain a comma", t)
			case len(terms) == max:
				past++
			default:
				seen[t] = true
				terms = append(terms, t)
			}
		}
		f.Close()
	}
	if past > 0 {
		log.Printf("asr: hotwords: %d terms past the first %d dropped", past, max)
	}
	return terms
}

// DoubaoHotwords is the list a Doubao request sends, in order. Without
// two-pass the text is the streaming pass's alone, so terms past its
// DoubaoStreamChars are dropped; with two-pass all go, up to
// DoubaoMaxHotwords, and those past the streaming budget steer the final text
// but not the partials. Counts are logged, never terms. Swift's
// DoubaoHotwords applies the same rule.
func DoubaoHotwords(terms []string, twoPass bool) []string {
	n := doubaoStreamTerms(terms)
	switch {
	case !twoPass && n < len(terms):
		log.Printf("asr: doubao: %d hotwords past the streaming pass's %d characters dropped", len(terms)-n, DoubaoStreamChars)
		return terms[:n]
	case !twoPass:
		return terms
	}
	if len(terms) > DoubaoMaxHotwords {
		log.Printf("asr: doubao: %d hotwords past %d dropped", len(terms)-DoubaoMaxHotwords, DoubaoMaxHotwords)
		terms = terms[:DoubaoMaxHotwords]
	}
	if n < len(terms) {
		log.Printf("asr: doubao: %d hotwords past the streaming pass's %d characters steer the two-pass text only", len(terms)-n, DoubaoStreamChars)
	}
	return terms
}

// doubaoStreamTerms is how many leading terms fit the streaming pass: their
// characters plus one between each pair, at most DoubaoStreamChars.
func doubaoStreamTerms(terms []string) int {
	used := -1
	for i, t := range terms {
		used += utf8.RuneCountInString(t) + 1
		if used > DoubaoStreamChars {
			return i
		}
	}
	return len(terms)
}
