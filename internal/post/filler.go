package post

import (
	"log"
	"regexp"
	"slices"
	"strings"
	"unicode"
	"unicode/utf8"
)

// Filler returns the filler stage: it removes 嗯/呃/um/uh, stutter repeats
// and VAD-break full stops, joining across a break after JoinWords and the
// words in join. With no join words it is the Go port of
// testdata/filler/rules.py and scores the same on the gold set beside it
// (filler_test.go).
//
// Invariant: the output is a subsequence of the input, apart from inserted
// "，". On a violation the stage logs it and delivers the input unchanged.
func Filler(join []string) Stage {
	vad := vadRE(join)
	return func(s string) string {
		out := cleanFillers(s, vad)
		if !subsequence(strings.ReplaceAll(out, "，", ""), s) {
			log.Printf("post: filler: output is not a subsequence of the input; delivering raw")
			return s
		}
		return out
	}
}

// cleanFillers applies the four rules and the join pass without the
// invariant guard.
func cleanFillers(s string, vad *regexp.Regexp) string {
	s = enFillers(s)
	s = zhFillers(s)
	s = repeats(s)
	s = vadJoin(s, vad)
	return joinPass(s)
}

// R1 English fillers, word-bounded (Unicode word boundary, as in Python), with
// a trailing comma and whitespace.
var reEN = regexp.MustCompile(`(?i)(?:u+m+|u+h+|erm|hmm+)`)

func enFillers(s string) string {
	var b strings.Builder
	pos, from := 0, 0
	for from <= len(s) {
		loc := reEN.FindStringIndex(s[from:])
		if loc == nil {
			break
		}
		i, j := from+loc[0], from+loc[1]
		if isWordRune(lastRune(s[:i])) || isWordRune(firstRune(s[j:])) {
			from = i + len(string(firstRune(s[i:])))
			continue
		}
		if strings.HasPrefix(s[j:], ",") {
			j++
		}
		for j < len(s) && unicode.IsSpace(firstRune(s[j:])) {
			j += len(string(firstRune(s[j:])))
		}
		b.WriteString(s[pos:i])
		pos, from = j, j
	}
	b.WriteString(s[pos:])
	return b.String()
}

// R2 Chinese filler runs: 嗯/呃 with attached punctuation. One run may span a
// VAD break, where a full stop sits between two filler characters.
const zhRun = `(?:[嗯呃]+[，。、]?[\s\p{Z}]*)+`

var (
	reZH     = regexp.MustCompile(zhRun)
	reZHOnly = regexp.MustCompile(`^` + zhRun + `$`)
)

func zhFillers(s string) string {
	if reZHOnly.MatchString(strings.TrimSpace(s)) {
		return s // the whole utterance is 嗯 (= yes): keep it
	}
	var b strings.Builder
	pos := 0
	for _, m := range reZH.FindAllStringIndex(s, -1) {
		seg := s[pos:m[0]]
		run := strings.TrimRightFunc(s[m[0]:m[1]], unicode.IsSpace)
		if end := lastRune(run); end == '。' || end == '？' || end == '！' {
			// keep the sentence end; drop a "，" right before it
			b.WriteString(strings.TrimSuffix(seg, "，"))
			b.WriteRune(end)
		} else {
			b.WriteString(seg)
		}
		pos = m[1]
	}
	b.WriteString(s[pos:])
	return b.String()
}

// R3 stutter repeats of a closed set of function words, to a fixpoint.
type repeat struct {
	re   *regexp.Regexp
	word string
}

var reRepeats = func() []repeat {
	var rs []repeat
	for _, w := range []string{"这个", "那个", "就是", "然后", "我们", "我", "你", "他", "她", "它", "这", "那"} {
		rs = append(rs, repeat{regexp.MustCompile(`(?:` + w + `){2,}`), w})
	}
	return rs
}()

func repeats(s string) string {
	for prev := ""; prev != s; {
		prev = s
		for _, r := range reRepeats {
			s = r.re.ReplaceAllLiteralString(s, r.word)
		}
	}
	return s
}

// R4 VAD-break join: a clause-final function word followed by 。 and more
// CJK text is not a sentence end; the 。 becomes ，. JoinWords are the
// built-in words; the config's post.join_words add to them.
var JoinWords = []string{"就是", "然后", "因为", "但是", "而且", "或者", "所以"}

func vadRE(join []string) *regexp.Regexp {
	var alt []string
	for _, w := range append(slices.Clone(JoinWords), join...) {
		if w != "" {
			alt = append(alt, regexp.QuoteMeta(w))
		}
	}
	return regexp.MustCompile(`(?:` + strings.Join(alt, "|") + `)。`)
}

func vadJoin(s string, vad *regexp.Regexp) string {
	var b strings.Builder
	pos := 0
	for _, m := range vad.FindAllStringIndex(s, -1) {
		if r := firstRune(s[m[1]:]); r < '一' || r > '鿿' {
			continue
		}
		b.WriteString(s[pos : m[1]-len("。")])
		b.WriteString("，")
		pos = m[1]
	}
	b.WriteString(s[pos:])
	return b.String()
}

var joinRules = []struct {
	re   *regexp.Regexp
	repl string
}{
	{regexp.MustCompile(`，+`), "，"},
	{regexp.MustCompile(`。+`), "。"},
	{regexp.MustCompile(`，([。？！])`), "$1"},
	{regexp.MustCompile(`([。？！])，`), "$1"},
	{regexp.MustCompile(`^[，、\s\p{Z}]+`), ""},
	{regexp.MustCompile(`[ \t]{2,}`), " "},
	{regexp.MustCompile(` ([,.?!])`), "$1"},
}

// joinPass tidies the punctuation the deletions leave behind.
func joinPass(s string) string {
	for _, r := range joinRules {
		s = r.re.ReplaceAllString(s, r.repl)
	}
	return s
}

// subsequence reports whether sub is a subsequence of s, rune by rune.
func subsequence(sub, s string) bool {
	rs := []rune(s)
	i := 0
	for _, r := range sub {
		for i < len(rs) && rs[i] != r {
			i++
		}
		if i == len(rs) {
			return false
		}
		i++
	}
	return true
}

func isWordRune(r rune) bool {
	return r == '_' || unicode.IsLetter(r) || unicode.IsDigit(r) || unicode.IsNumber(r)
}

func firstRune(s string) rune {
	if s == "" {
		return -1
	}
	r, _ := utf8.DecodeRuneInString(s)
	return r
}

func lastRune(s string) rune {
	if s == "" {
		return -1
	}
	r, _ := utf8.DecodeLastRuneInString(s)
	return r
}
