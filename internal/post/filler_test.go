package post

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// goldEntry is one utterance of testdata/filler/gold.md: the raw text, the
// text with must-remove marks applied (min) and with must+may applied (max),
// and the content-char count of the must spans.
type goldEntry struct {
	name, raw, min, max string
	must                int
}

var goldMark = regexp.MustCompile(`(?s)([\[{])(.*?)(?:\|(.*?))?([\]}])(?:\^([a-z]))?`)

// parseGold mirrors parse_gold in testdata/filler/score.py.
func parseGold(t *testing.T) []goldEntry {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("testdata", "filler", "gold.md"))
	if err != nil {
		t.Fatal(err)
	}
	blocks := regexp.MustCompile(`(?m)^## `).Split(string(b), -1)[1:]
	var gold []goldEntry
	for _, block := range blocks {
		name, body, _ := strings.Cut(block, "\n")
		body = strings.TrimSpace(body)
		var raw, gmin, gmax strings.Builder
		must, pos := 0, 0
		for _, m := range goldMark.FindAllStringSubmatchIndex(body, -1) {
			plain := body[pos:m[0]]
			raw.WriteString(plain)
			gmin.WriteString(plain)
			gmax.WriteString(plain)
			span, repl := body[m[4]:m[5]], ""
			if m[6] >= 0 {
				repl = body[m[6]:m[7]]
			}
			raw.WriteString(span)
			gmax.WriteString(repl)
			if body[m[2]:m[3]] == "[" {
				gmin.WriteString(repl)
				must += len([]rune(content(span)))
			} else {
				gmin.WriteString(span)
			}
			pos = m[1]
		}
		tail := body[pos:]
		raw.WriteString(tail)
		gmin.WriteString(tail)
		gmax.WriteString(tail)
		gold = append(gold, goldEntry{strings.TrimSpace(name), raw.String(), gmin.String(), gmax.String(), must})
	}
	return gold
}

// content drops punctuation and whitespace, as score.py does.
func content(s string) string {
	return strings.Map(func(r rune) rune {
		if strings.ContainsRune("，。？！、,.?!'\" \t\n", r) {
			return -1
		}
		return r
	}, s)
}

// missing counts the content chars of a that b does not keep: len(a) minus
// the longest common subsequence, over content chars.
func missing(a, b string) int {
	x, y := []rune(content(a)), []rune(content(b))
	prev, cur := make([]int, len(y)+1), make([]int, len(y)+1)
	for i := range x {
		for j := range y {
			switch {
			case x[i] == y[j]:
				cur[j+1] = prev[j] + 1
			case prev[j+1] >= cur[j]:
				cur[j+1] = prev[j+1]
			default:
				cur[j+1] = cur[j]
			}
		}
		prev, cur = cur, prev
	}
	return len(x) - prev[len(y)]
}

// The port reproduces rules.py byte for byte on every gold utterance
// (want/ holds rules.py's output).
func TestFillerMatchesRulesPy(t *testing.T) {
	for _, g := range parseGold(t) {
		want, err := os.ReadFile(filepath.Join("testdata", "filler", "want", g.name+".txt"))
		if err != nil {
			t.Fatal(err)
		}
		if got := Filler(nil)(g.raw); strings.TrimSpace(got) != strings.TrimSpace(string(want)) {
			t.Errorf("%s:\n got %q\nwant %q", g.name, got, want)
		}
	}
}

// Score against the gold set, as score.py does. rules.py removes 42 of the 60
// must-remove content chars. It keeps the 16 in false starts, which are beyond
// it, and 061611's "uh" glued to Chinese text, where no word boundary sits. It
// drops three chars it should keep — 061447's quoted "Hmm" is the text of a
// note, not a filler — and inserts none.
func TestFillerGoldScore(t *testing.T) {
	var hit, miss, wrong, ins int
	for _, g := range parseGold(t) {
		out := Filler(nil)(g.raw)
		w, n := missing(g.max, out), missing(out, g.raw)
		m := max(0, missing(out, g.min)-n)
		hit, miss, wrong, ins = hit+g.must-m, miss+m, wrong+w, ins+n
	}
	t.Logf("recall(must) = %.2f (%d/%d)  content-loss chars = %d  inserted chars = %d",
		float64(hit)/float64(hit+miss), hit, hit+miss, wrong, ins)
	if hit != 42 || hit+miss != 60 || wrong != 3 || ins != 0 {
		t.Errorf("score drifted from rules.py: hit %d/%d, wrong %d, inserted %d; want 42/60, 3, 0", hit, hit+miss, wrong, ins)
	}
}

// The guard in Filler: deletions pass, inserted "，" pass, a reorder fails.
func TestSubsequence(t *testing.T) {
	if !subsequence("ac", "abc") || !subsequence(strings.ReplaceAll("ab，cd", "，", ""), "ab。cd") || subsequence("ca", "abc") {
		t.Error("subsequence is wrong")
	}
}

// A configured join word joins across a VAD break as a built-in one does;
// without it the full stop stays.
func TestFillerJoinWords(t *testing.T) {
	raw := "顶绳挂好了，这样一来。下一位就能直接上。"
	if got := Filler(nil)(raw); got != raw {
		t.Errorf("no join words: %q, want %q", got, raw)
	}
	want := "顶绳挂好了，这样一来，下一位就能直接上。"
	if got := Filler([]string{"这样一来"})(raw); got != want {
		t.Errorf("join word 这样一来: %q, want %q", got, want)
	}
}
