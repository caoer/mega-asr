package app

import (
	"bufio"
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
// at each take: the hot words, then what the corrections table corrects to,
// then the glossary files, as the terms, each file in its own order; the
// table's rules as the known mis-hearings; about.txt as the about line.
// With none of the files it is empty.
func Glossary() refine.Glossary {
	terms := lines(HotwordsFiles()...)
	var hints []refine.Hint
	for _, r := range post.ReadRules(CorrectionsFiles()...) {
		terms = append(terms, r.To)
		hints = append(hints, refine.Hint{Heard: r.From, Meant: r.To})
	}
	terms = append(terms, lines(GlossaryFiles()...)...)
	g, past := refine.NewGlossary(strings.Join(lines(AboutPath()), " "), terms, hints)
	if past > 0 {
		log.Printf("refine: glossary: %d entries past its caps (%d terms, %d mis-hearings) left out", past, refine.MaxTerms, refine.MaxHints)
	}
	return g
}

// lines reads each path in turn as one list: a line is an entry, blank
// lines and # comments are skipped, and a missing file adds nothing.
func lines(paths ...string) []string {
	var out []string
	for _, path := range paths {
		f, err := os.Open(path)
		if err != nil {
			if !os.IsNotExist(err) {
				log.Printf("refine: glossary: %v", err)
			}
			continue
		}
		sc := bufio.NewScanner(f)
		for sc.Scan() {
			if l := strings.TrimSpace(sc.Text()); l != "" && !strings.HasPrefix(l, "#") {
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
