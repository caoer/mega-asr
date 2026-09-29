package asr

import (
	"bufio"
	"log"
	"os"
	"strings"
)

// MaxHotwords caps the list; the CLI also drops trailing terms past 512
// prompt tokens.
const MaxHotwords = 50

// ReadHotwords reads one term per line; blank lines and # comments are
// skipped, a term with a comma is dropped (the CLI splits on commas), and
// terms past MaxHotwords are dropped. A missing file is an empty list.
func ReadHotwords(path string) []string {
	f, err := os.Open(path)
	if err != nil {
		if !os.IsNotExist(err) {
			log.Printf("asr: hotwords: %v", err)
		}
		return nil
	}
	defer f.Close()
	var terms []string
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		t := strings.TrimSpace(sc.Text())
		switch {
		case t == "" || strings.HasPrefix(t, "#"):
		case strings.Contains(t, ","):
			log.Printf("asr: hotwords: %q dropped: a term cannot contain a comma", t)
		case len(terms) == MaxHotwords:
			log.Printf("asr: hotwords: %q dropped: more than %d terms", t, MaxHotwords)
		default:
			terms = append(terms, t)
		}
	}
	return terms
}
