// Package score is asrbench's text scorer: one normalisation and one token
// alignment for every place that judges a transcript against a reference
// (asrbench score, megameet align).
//
// Normalisation, applied to reference and hypothesis alike:
//   - width fold: fullwidth ASCII U+FF01–FF5E → ASCII, U+3000 → space,
//     halfwidth 。、 → 。、, curly apostrophes → ' (the NFKC mappings that occur
//     in ASR output; the rest of NFKC is not applied);
//   - lower case;
//   - tokens: every Han/kana/hangul character is one token; a run of letters is
//     one word, a run of digits is one word (a letter↔digit change splits, so
//     "USB3" = "USB 3"); an apostrophe between letters and a dot between
//     digits stay inside the word;
//   - everything else (punctuation, symbols, spaces) separates tokens and is
//     not scored as text. Numbers are not normalised: "5" and "五" differ;
//   - filler tokens are dropped with the mark that follows them: 嗯 呃 um uh
//     uhm erm hmm mm mhm (models differ in whether they write fillers, and
//     megavoice deletes them after ASR);
//   - English number words zero…twenty and the tens become digits ("forty" =
//     "40"); Chinese numerals are left as they are;
//   - characters that differ only in writing, never in sound, fold to one:
//     他/她/它 → 他, 的/地/得 → 的;
//   - a run of two or more single letters is one word ("A B C" = "abc",
//     "x ray" stays two words), unless the run is only a/i ("I a").
//
// The token error rate (substitutions + deletions + insertions over reference
// tokens) is CER for Chinese (tokens are characters), WER for English (words)
// and MER for mixed text. Punctuation is scored on the aligned token pairs:
// the mark after each token is none, pause (, 、 ; :) or end (. 。 ? ! …).
// HanErr and LatErr split the errors by script: substitutions and deletions
// count against the reference token's script, insertions against the
// hypothesis token's.
package score

import (
	"fmt"
	"strings"
	"unicode"
)

// Normalize is text as the scorer compares it: the tokens' texts after the
// fillers, number words, homophones and spelled letters are folded.
func Normalize(s string) []string { return Words(DropFillers(Tokenize(s))) }

// Punctuation classes: the mark that follows a token.
const (
	PunctNone  = 0
	PunctPause = 'p'
	PunctEnd   = 'e'
)

// Token is one scored unit and the punctuation class that follows it.
type Token struct {
	Text  string
	Punct byte
}

// fold maps the width and apostrophe variants ASR output mixes.
func fold(r rune) rune {
	switch {
	case r >= 0xFF01 && r <= 0xFF5E:
		return r - 0xFEE0
	case r == 0x3000:
		return ' '
	case r == 0xFF61:
		return '。'
	case r == 0xFF64:
		return '、'
	case r == '’' || r == '‘':
		return '\''
	}
	return r
}

// IsCJK is a Han, kana or hangul character: one token each.
func IsCJK(r rune) bool {
	return unicode.In(r, unicode.Han, unicode.Hiragana, unicode.Katakana, unicode.Hangul)
}

func punctClass(r rune) byte {
	switch r {
	case '.', '。', '?', '!', '…':
		return PunctEnd
	case ',', '、', ';', ':':
		return PunctPause
	}
	return PunctNone
}

// Tokenize normalises s and splits it into tokens, each carrying the
// punctuation class that follows it (end wins over pause).
func Tokenize(s string) []Token {
	rs := []rune(s)
	for i, r := range rs {
		rs[i] = fold(r)
	}
	var toks []Token
	var word []rune
	digits := false
	flush := func() {
		if len(word) > 0 {
			toks = append(toks, Token{Text: strings.ToLower(string(word))})
			word = word[:0]
		}
	}
	next := func(i int) rune {
		if i+1 < len(rs) {
			return rs[i+1]
		}
		return 0
	}
	for i, r := range rs {
		switch {
		case IsCJK(r):
			flush()
			toks = append(toks, Token{Text: string(r)})
		case unicode.IsLetter(r) || unicode.IsDigit(r):
			d := unicode.IsDigit(r)
			if len(word) > 0 && d != digits {
				flush()
			}
			if len(word) == 0 {
				digits = d
			}
			word = append(word, r)
		case unicode.Is(unicode.Mn, r) && len(word) > 0:
			word = append(word, r)
		case r == '\'' && len(word) > 0 && !digits && unicode.IsLetter(next(i)) && !IsCJK(next(i)):
			word = append(word, r)
		case r == '.' && len(word) > 0 && digits && unicode.IsDigit(next(i)):
			word = append(word, r)
		default:
			flush()
			if c := punctClass(r); c != PunctNone && len(toks) > 0 {
				if last := &toks[len(toks)-1]; last.Punct != PunctEnd {
					last.Punct = c
				}
			}
		}
	}
	flush()
	return toks
}

// fillers are dropped from both sides before scoring.
var fillers = map[string]bool{"嗯": true, "呃": true, "um": true, "uh": true, "uhm": true, "erm": true, "hmm": true, "mm": true, "mhm": true}

var numberWords = func() map[string]string {
	m := map[string]string{}
	for i, w := range strings.Fields("zero one two three four five six seven eight nine ten eleven twelve thirteen fourteen fifteen sixteen seventeen eighteen nineteen twenty") {
		m[w] = fmt.Sprint(i)
	}
	for i, w := range strings.Fields("thirty forty fifty sixty seventy eighty ninety") {
		m[w] = fmt.Sprint(30 + 10*i)
	}
	return m
}()

// homophones fold characters that are written differently but sound the same.
var homophones = map[string]string{"她": "他", "它": "他", "地": "的", "得": "的"}

// DropFillers removes filler tokens, turns English number Words into digits,
// folds homophone characters and joins runs of spelled single letters.
func DropFillers(toks []Token) []Token {
	out := toks[:0:0]
	for _, t := range toks {
		if fillers[t.Text] {
			continue
		}
		if d, ok := numberWords[t.Text]; ok {
			t.Text = d
		}
		if f, ok := homophones[t.Text]; ok {
			t.Text = f
		}
		out = append(out, t)
	}
	return joinLetters(out)
}

func singleLetter(t Token) bool {
	r := []rune(t.Text)
	return len(r) == 1 && r[0] < unicode.MaxASCII && unicode.IsLetter(r[0])
}

// joinLetters merges runs of two or more single-letter tokens into one word;
// the run keeps the punctuation of its last letter. A run of only a/i stays.
func joinLetters(toks []Token) []Token {
	var out []Token
	for i := 0; i < len(toks); {
		j := i
		for j < len(toks) && singleLetter(toks[j]) && (j == i || toks[j-1].Punct == PunctNone) {
			j++
		}
		onlyAI := true
		for k := i; k < j; k++ {
			if toks[k].Text != "a" && toks[k].Text != "i" {
				onlyAI = false
			}
		}
		if j-i < 2 || onlyAI {
			out = append(out, toks[i])
			i++
			continue
		}
		var b strings.Builder
		for k := i; k < j; k++ {
			b.WriteString(toks[k].Text)
		}
		out = append(out, Token{Text: b.String(), Punct: toks[j-1].Punct})
		i = j
	}
	return out
}

// Words are the tokens' texts.
func Words(toks []Token) []string {
	w := make([]string, len(toks))
	for i, t := range toks {
		w[i] = t.Text
	}
	return w
}

// Clip is one reference/hypothesis pair.
type Clip struct {
	RefTok, Sub, Del, Ins int
	PTP, PFP, PFN         int
	ETP, EFP, EFN         int
	TermRef, TermHit      int
	HanRef, HanErr        int // CJK reference tokens; errors on CJK tokens
	LatRef, LatErr        int // Latin/digit reference tokens; errors on them
	Confusions            [][2]string
}

// Errors is substitutions + deletions + insertions.
func (c Clip) Errors() int { return c.Sub + c.Del + c.Ins }

// Rate is Errors over the reference tokens.
func (c Clip) Rate() float64 {
	if c.RefTok == 0 {
		return 0
	}
	return float64(c.Errors()) / float64(c.RefTok)
}

// ScoreClip aligns hyp to ref token by token and counts the errors.
func ScoreClip(ref, hyp string, terms [][]string) Clip {
	r, h := DropFillers(Tokenize(ref)), DropFillers(Tokenize(hyp))
	c := Clip{RefTok: len(r)}
	for _, t := range r {
		if IsCJK([]rune(t.Text)[0]) {
			c.HanRef++
		} else {
			c.LatRef++
		}
	}
	ops := alignTokens(r, h)
	var rs, hs []int // the current run of non-matching ops
	flush := func() {
		if len(rs)+len(hs) > 0 {
			c.Confusions = append(c.Confusions, [2]string{span(r, rs), span(h, hs)})
			rs, hs = rs[:0], hs[:0]
		}
	}
	for _, op := range ops {
		switch op.kind {
		case 'M':
			flush()
		case 'S':
			c.Sub++
			c.countErr(r[op.r])
			rs, hs = append(rs, op.r), append(hs, op.h)
		case 'D':
			c.Del++
			c.countErr(r[op.r])
			rs = append(rs, op.r)
		case 'I':
			c.Ins++
			c.countErr(h[op.h])
			hs = append(hs, op.h)
		}
		if op.kind != 'M' && op.kind != 'S' {
			continue
		}
		rc, hc := r[op.r].Punct, h[op.h].Punct
		if rc != PunctNone && hc == rc {
			c.PTP++
		} else {
			if hc != PunctNone {
				c.PFP++
			}
			if rc != PunctNone {
				c.PFN++
			}
		}
		switch {
		case rc == PunctEnd && hc == PunctEnd:
			c.ETP++
		case hc == PunctEnd:
			c.EFP++
		case rc == PunctEnd:
			c.EFN++
		}
	}
	flush()
	rw, hw := Words(r), Words(h)
	for _, t := range terms {
		nr, nh := count(rw, t), count(hw, t)
		c.TermRef += nr
		c.TermHit += min(nr, nh)
	}
	return c
}

// countErr attributes an error to the script of the token it involves.
func (c *Clip) countErr(t Token) {
	if IsCJK([]rune(t.Text)[0]) {
		c.HanErr++
	} else {
		c.LatErr++
	}
}

// alignOp is one step of the alignment: 'M' match, 'S' substitution, 'D'
// deletion (a reference token missing), 'I' insertion; r and h index the
// tokens it involves (-1 where none).
type alignOp struct {
	kind byte
	r, h int
}

// alignTokens is the Levenshtein alignment over token texts, in order.
func alignTokens(r, h []Token) []alignOp {
	n, m := len(r), len(h)
	d := make([][]int32, n+1)
	for i := range d {
		d[i] = make([]int32, m+1)
		d[i][0] = int32(i)
	}
	for j := 0; j <= m; j++ {
		d[0][j] = int32(j)
	}
	for i := 1; i <= n; i++ {
		for j := 1; j <= m; j++ {
			cost := int32(1)
			if r[i-1].Text == h[j-1].Text {
				cost = 0
			}
			d[i][j] = min(d[i-1][j-1]+cost, d[i-1][j]+1, d[i][j-1]+1)
		}
	}
	var ops []alignOp
	i, j := n, m
	for i > 0 || j > 0 {
		switch {
		case i > 0 && j > 0 && r[i-1].Text == h[j-1].Text && d[i][j] == d[i-1][j-1]:
			ops = append(ops, alignOp{'M', i - 1, j - 1})
			i, j = i-1, j-1
		case i > 0 && j > 0 && d[i][j] == d[i-1][j-1]+1:
			ops = append(ops, alignOp{'S', i - 1, j - 1})
			i, j = i-1, j-1
		case i > 0 && d[i][j] == d[i-1][j]+1:
			ops = append(ops, alignOp{'D', i - 1, -1})
			i--
		default:
			ops = append(ops, alignOp{'I', -1, j - 1})
			j--
		}
	}
	for a, b := 0, len(ops)-1; a < b; a, b = a+1, b-1 {
		ops[a], ops[b] = ops[b], ops[a]
	}
	return ops
}

// span joins token texts as megavoice joins chunk texts: no space next to a
// CJK token, one space between two Latin/digit tokens.
func span(toks []Token, idx []int) string {
	var b strings.Builder
	last := ""
	for _, i := range idx {
		t := toks[i].Text
		if last != "" && !IsCJK([]rune(last)[0]) && !IsCJK([]rune(t)[0]) {
			b.WriteByte(' ')
		}
		b.WriteString(t)
		last = t
	}
	return b.String()
}

// count is the number of non-overlapping occurrences of t in w.
func count(w, t []string) int {
	n := 0
	for i := 0; i+len(t) <= len(w); {
		match := true
		for k := range t {
			if w[i+k] != t[k] {
				match = false
				break
			}
		}
		if match {
			n++
			i += len(t)
		} else {
			i++
		}
	}
	return n
}

// Tally sums clip scores over a bucket.
type Tally struct {
	Clip
	Clips int
	Lat   []float64
}

// Add sums one clip in; a positive latency joins the percentiles.
func (t *Tally) Add(c Clip, latencyMS float64) {
	t.Clips++
	t.RefTok += c.RefTok
	t.Sub += c.Sub
	t.Del += c.Del
	t.Ins += c.Ins
	t.PTP += c.PTP
	t.PFP += c.PFP
	t.PFN += c.PFN
	t.ETP += c.ETP
	t.EFP += c.EFP
	t.EFN += c.EFN
	t.TermRef += c.TermRef
	t.TermHit += c.TermHit
	t.HanRef += c.HanRef
	t.HanErr += c.HanErr
	t.LatRef += c.LatRef
	t.LatErr += c.LatErr
	if latencyMS > 0 {
		t.Lat = append(t.Lat, latencyMS)
	}
}
