package refine

import (
	"fmt"
	"reflect"
	"strings"
	"testing"
)

// A glossary keeps its lists in the order given and cuts each at its cap:
// a term counts once, in any case; an empty or over-long entry, a hint
// that changes nothing and a second hint for a heard form are left out
// without counting against the cap.
func TestGlossaryIsCappedInOrder(t *testing.T) {
	g, past := NewGlossary("  a backend engineer;\n talks about clusters ",
		[]string{"Kubernetes", " kubectl ", "", "kubernetes", "Postgres", strings.Repeat("x", maxEntry+1)},
		[]Hint{{"cube control", "kubectl"}, {"Postgres", "postgres"}, {"", "GitHub"}, {"get hub", ""}, {"Cube Control", "kubectl"}, {"post gress", "Postgres"}})
	want := Glossary{About: "a backend engineer; talks about clusters",
		Terms: []string{"Kubernetes", "kubectl", "Postgres"},
		Hints: []Hint{{"cube control", "kubectl"}, {"post gress", "Postgres"}}}
	if !reflect.DeepEqual(g, want) || past != 0 {
		t.Errorf("glossary %+v, %d past the caps; want %+v", g, past, want)
	}

	var terms []string
	var hints []Hint
	for i := range MaxTerms + 7 {
		terms = append(terms, fmt.Sprintf("term-%d", i))
	}
	for i := range MaxHints + 3 {
		hints = append(hints, Hint{fmt.Sprintf("heard %d", i), "meant"})
	}
	g, past = NewGlossary(strings.Repeat("字", MaxAbout+50), terms, hints)
	if len(g.Terms) != MaxTerms || g.Terms[0] != "term-0" || g.Terms[MaxTerms-1] != fmt.Sprintf("term-%d", MaxTerms-1) ||
		len(g.Hints) != MaxHints || g.Hints[MaxHints-1].Heard != fmt.Sprintf("heard %d", MaxHints-1) || past != 10 ||
		g.About != strings.Repeat("字", MaxAbout) {
		t.Errorf("%d terms (last %q), %d hints, about of %d characters, %d past the caps", len(g.Terms), g.Terms[len(g.Terms)-1], len(g.Hints), len([]rune(g.About)), past)
	}
}

// The glossary's blocks are the about line, the terms and the
// mis-hearings, in that order, and a part it lacks gives none: an empty
// glossary adds nothing to the prompt.
func TestGlossaryBlocks(t *testing.T) {
	g := Glossary{About: "a backend engineer", Terms: []string{"Kubernetes", "kubectl"}, Hints: []Hint{{"cube control", "kubectl"}}}
	want := []string{
		"关于说话的人：a backend engineer",
		termsHead + "\nKubernetes\nkubectl",
		hintsHead + "\ncube control → kubectl",
	}
	if got := g.Blocks(); !reflect.DeepEqual(got, want) {
		t.Errorf("blocks %q, want %q", got, want)
	}
	if got := (Glossary{Terms: g.Terms}).Blocks(); !reflect.DeepEqual(got, want[1:2]) {
		t.Errorf("terms alone: blocks %q", got)
	}
	if got := (Glossary{}).Blocks(); got != nil {
		t.Errorf("an empty glossary gives blocks %q", got)
	}
	// The terms block itself forbids bringing a correct word to a glossary spelling.
	if !strings.Contains(termsHead, "不要换成词表里的写法") {
		t.Errorf("the terms block lost its rule: %q", termsHead)
	}
}
