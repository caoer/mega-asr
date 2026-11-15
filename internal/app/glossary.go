package app

import (
	"bufio"
	"bytes"
	"log"
	"os"
	"path/filepath"
	"strings"

	"github.com/caoer/mega-asr/internal/post"
	"github.com/caoer/mega-asr/internal/refine"
)

// GlossaryPath is the user's glossary beside the config: terms the model
// step should know that are not worth a hot-word slot, one per line.
func GlossaryPath() string { return filepath.Join(ConfigDir(), "glossary.txt") }

// GlossaryFiles is glossary.txt, then glossary.d/*.txt in name order.
func GlossaryFiles() []string { return withDir(GlossaryPath(), "glossary.d", "*.txt") }

// AboutPath is the about-me line beside the config: who the user is and
// what they talk about.
func AboutPath() string { return filepath.Join(ConfigDir(), "about.txt") }

// Glossary is what the model step is told of the user, read from the files
// at each take. Its terms are the hot words, then what the corrections
// table corrects to, then the glossary, each file in its own order: first
// from the user's own three files, then from the .d files other tools
// write, so those are what the cap cuts. The table's rules are the known
// mis-hearings, but for a zh: rule: its heard form is an ordinary word,
// wrong only beside Chinese, where the table has put it right already.
// about.txt is the about line. A hot-word or corrections file that still is
// the sample Seed wrote counts for nothing: its terms are examples, not the
// user's. With none of the files the glossary is empty.
func Glossary() refine.Glossary {
	var terms []string
	var hints []refine.Hint
	add := func(hot, corr, glos []string) {
		terms = append(terms, lines(hot...)...)
		for _, r := range post.ReadRules(corr...) {
			terms = append(terms, r.To)
			if !r.Zh {
				hints = append(hints, refine.Hint{Heard: r.From, Meant: r.To})
			}
		}
		terms = append(terms, lines(glos...)...)
	}
	hot, corr, glos := HotwordsFiles(), CorrectionsFiles(), GlossaryFiles()
	add(own(hot[0], "hotwords.txt"), own(corr[0], "corrections.tsv"), glos[:1])
	add(hot[1:], corr[1:], glos[1:])
	g, past := refine.NewGlossary(strings.Join(lines(AboutPath()), " "), terms, hints)
	if past > 0 {
		log.Printf("refine: glossary: %d entries past its caps (%d terms, %d mis-hearings) left out", past, refine.MaxTerms, refine.MaxHints)
	}
	return g
}

// own is path as a list of files to read: none while path still holds the
// sample Seed wrote from defaults/<sample>.
func own(path, sample string) []string {
	if !post.Regular(path) {
		return nil
	}
	b, err := os.ReadFile(path)
	if d, _ := defaults.ReadFile("defaults/" + sample); err == nil && bytes.Equal(b, d) {
		return nil
	}
	return []string{path}
}

// lines reads each path in turn as one list: a line is an entry, blank
// lines and # comments are skipped, and a byte-order mark at a file's start
// is not part of its first line. A missing file adds nothing, nor does a
// path that is not a regular file: a pipe would never finish reading.
func lines(paths ...string) []string {
	var out []string
	for _, path := range paths {
		if !post.Regular(path) {
			continue
		}
		f, err := os.Open(path)
		if err != nil {
			log.Printf("refine: glossary: %v", err)
			continue
		}
		sc := bufio.NewScanner(f)
		for first := true; sc.Scan(); first = false {
			l := sc.Text()
			if first {
				l = strings.TrimPrefix(l, "\ufeff")
			}
			if l = strings.TrimSpace(l); l != "" && !strings.HasPrefix(l, "#") {
				out = append(out, l)
			}
		}
		if err := sc.Err(); err != nil {
			log.Printf("refine: glossary: %s: %v", path, err)
		}
		f.Close()
	}
	return out
}
