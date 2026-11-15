package refine

import (
	"strings"
	"unicode/utf8"
)

// Glossary is what a call tells the model of the speaker, after the mode's
// prompt: who they are, the terms they say, and how the recogniser has
// mis-heard them.
type Glossary struct {
	About string   // who the speaker is and what they talk about, one line
	Terms []string // the terms the speaker says, the most used first
	Hints []Hint   // known mis-hearings
}

// Hint is one known mis-hearing: the recogniser wrote Heard where the
// speaker said Meant.
type Hint struct{ Heard, Meant string }

// The glossary's caps. It is the terms a speaker says, not an inventory: a
// list of every name on a machine pulls the model to names nobody said.
const (
	MaxTerms = 350 // terms
	MaxHints = 200 // mis-hearings
	MaxAbout = 400 // characters of the about line
	maxEntry = 48  // characters of a term, or of one side of a mis-hearing
)

// NewGlossary is the glossary of about, terms and hints, each list in the
// order given and cut at its cap: the first mention of a term counts, in
// any case, as does the first hint for a heard form; an entry that is empty
// or longer than a term is left out, and so is a hint that changes
// nothing. past is how many entries the caps cut.
func NewGlossary(about string, terms []string, hints []Hint) (g Glossary, past int) {
	if a := []rune(strings.Join(strings.Fields(about), " ")); len(a) > MaxAbout {
		g.About = string(a[:MaxAbout])
	} else {
		g.About = string(a)
	}
	entry := func(s string) (string, bool) {
		s = strings.Join(strings.Fields(s), " ")
		return s, s != "" && utf8.RuneCountInString(s) <= maxEntry
	}
	seen := map[string]bool{}
	for _, t := range terms {
		t, ok := entry(t)
		k := strings.ToLower(t)
		if !ok || seen[k] {
			continue
		}
		seen[k] = true
		if len(g.Terms) == MaxTerms {
			past++
			continue
		}
		g.Terms = append(g.Terms, t)
	}
	seen = map[string]bool{}
	for _, h := range hints {
		heard, ok1 := entry(h.Heard)
		meant, ok2 := entry(h.Meant)
		k := strings.ToLower(heard)
		if !ok1 || !ok2 || k == strings.ToLower(meant) || seen[k] {
			continue
		}
		seen[k] = true
		if len(g.Hints) == MaxHints {
			past++
			continue
		}
		g.Hints = append(g.Hints, Hint{heard, meant})
	}
	return g, past
}

// The heads of the glossary's blocks. The terms carry their own rule, so a
// prompt the user wrote is covered too: a term is for recognising a
// mis-heard word, never a spelling to bring a correct word to.
const (
	aboutHead = "关于说话的人："
	termsHead = "词表：他常说的词，只用来认出被听错的词。原文里已经说得通的词或名字，不要换成词表里别的词，也不要换成词表里的写法，大小写、空格、连字符、版本号一律照原文。"
	hintsHead = "识别器把他的词听错过的例子（听成 → 他说的是）。原文里有读音相近的写法、放在句子里说不通时，参照这些例子还原；左边的写法在句子里本来说得通的，不动。"
)

// Blocks are g's system blocks, to follow the mode's prompt in
// Request.System: the about line, the terms, the mis-hearings, the one
// that changes least first. The same glossary gives the same bytes; a part
// g lacks gives no block.
func (g Glossary) Blocks() []string {
	var blocks []string
	if g.About != "" {
		blocks = append(blocks, aboutHead+g.About)
	}
	if len(g.Terms) > 0 {
		blocks = append(blocks, termsHead+"\n"+strings.Join(g.Terms, "\n"))
	}
	if len(g.Hints) > 0 {
		var b strings.Builder
		b.WriteString(hintsHead)
		for _, h := range g.Hints {
			b.WriteString("\n" + h.Heard + " → " + h.Meant)
		}
		blocks = append(blocks, b.String())
	}
	return blocks
}
