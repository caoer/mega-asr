// Package refine is dictation's model step: after recognition and the post
// chain, one model call returns the changes to make to a take's text, and
// the step applies them under its mode's guards. The call has no time limit:
// it ends with an answer, a failure, or the caller's cancel. When it fails or
// is refused, when the answer does not parse, or when a change fails a guard,
// the result says why and carries the text as it was.
package refine

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"slices"
	"sort"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"
)

// Request is one call, assembled: what every client of the step sends, and
// what a Provider turns into its API's request.
type Request struct {
	Model string // the provider's model id
	// Effort is off, low, medium or high: how much the model reasons. off is
	// no thinking on a model that allows it, else its lowest effort.
	Effort string
	// System is the instruction in cache order, the most stable block first:
	// the mode's prompt, then the speaker's glossary.
	System []string
	// PerTake is how many of the last System blocks change from take to
	// take: the glossary entries a call carries for its dictation
	// (Glossary.PerTake). The cache breakpoint is on the block before them,
	// the last one every take sends the same; 0 puts it on the last block.
	PerTake int
	// Context is what was in front of the speaker, sent inside <context>;
	// "" sends none.
	Context string
	// Names are the spans of the dictation that sound like the name of a
	// person the speaker knows, a line each (NameLine), sent under their
	// head inside <names>; "" sends none.
	Names string
	// Image is a picture of what was in front of the speaker, sent ahead of
	// the text; nil sends none.
	Image *Image
	// Dictation is the take's text, sent inside <dictation>: material for
	// the model, never a message to it.
	Dictation string
	// Output is what the answer is: EditList ("" too), the changes to make,
	// or FullText, the whole text to deliver.
	Output string
}

// Image is a picture as a call sends it.
type Image struct {
	MediaType string // image/jpeg
	Data      []byte
	W, H      int // pixels
}

// UserContent is the user message of r: the context and the names, when
// there are any, and the dictation, each inside its tag. A closing tag
// written inside any, spaced out or in full-width characters too, is sent
// with its < as &lt;, so only the message's own tags close.
func (r Request) UserContent() string {
	var b strings.Builder
	if r.Context != "" {
		b.WriteString("<context>\n" + InTag(r.Context) + "\n</context>\n")
	}
	if r.Names != "" {
		b.WriteString(namesHead + "\n<names>\n" + InTag(r.Names) + "\n</names>\n")
	}
	b.WriteString("<dictation>\n" + InTag(r.Dictation) + "\n</dictation>")
	return b.String()
}

// closingTag is the end of a tag the request puts material in: the
// dictation, the context and the names, and the glossary's about line,
// terms and mis-hearings. InTag keeps material from ending its own tag.
var closingTag = regexp.MustCompile(`(?i)[<＜]([\s\p{Z}]*[/／][\s\p{Z}]*(?:dictation|context|names|about|terms|hints))`)

// InTag is s as a request sends it inside one of its tags: a closing tag
// written in it has its < sent as &lt;. A second pass changes nothing.
func InTag(s string) string { return closingTag.ReplaceAllString(s, "&lt;$1") }

// Edit is one change the model asks for: From, a stretch of the dictation
// as it stands, becomes To; Why is the model's one-line reason.
type Edit struct {
	From string `json:"from"`
	Why  string `json:"why,omitempty"`
	To   string `json:"to"`
}

// Usage is what one call cost, in tokens.
type Usage struct {
	Input      int `json:"input_tokens"`
	CacheWrite int `json:"cache_creation_input_tokens"`
	CacheRead  int `json:"cache_read_input_tokens"`
	Output     int `json:"output_tokens"`
}

// Reply is a provider's answer: the edit list, or for a FullText request
// the text.
type Reply struct {
	Edits []Edit
	Text  string
	Usage Usage
}

// Provider makes the call. Its error is ErrRefusal when the model declined,
// ErrUnparseable when the answer is not an edit list, an *HTTPError for a
// status other than 200, and the context's when the caller cancelled it.
type Provider interface {
	Refine(ctx context.Context, req Request) (Reply, error)
}

var (
	ErrRefusal     = errors.New("the model declined")
	ErrUnparseable = errors.New("the answer is not an edit list or a text")
)

// HTTPError is a provider's status other than 200, with the head of its body,
// the key replaced by [key] wherever the body echoed it.
type HTTPError struct {
	Status int
	Body   string
}

func (e *HTTPError) Error() string { return fmt.Sprintf("status %d: %s", e.Status, e.Body) }

// Why a step gave no answer to apply (Result.Why).
const (
	WhyCancelled = "cancelled" // the caller cancelled the call
	WhyHTTP      = "http"      // a status other than 200
	WhyRefusal   = "refusal"   // the model declined
	WhyParse     = "parse"     // the answer is not the edit list or the text asked for
	WhyGuard     = "guard"     // a change failed the mode's guard
	WhyError     = "error"     // anything else: no route
)

// Step is the model step as one take runs it.
type Step struct {
	Provider Provider
	Mode     string // what the model is asked to do: FixWords or a text mode; "" is fix-words
	Model    string
	Effort   string
	System   []string // Request.System
	PerTake  int      // Request.PerTake
	Context  string   // Request.Context
	Names    string   // Request.Names
	Image    *Image   // Request.Image
}

// Result is what a step did to a text.
type Result struct {
	Text    string // the text to deliver: the input unless edits were applied
	Edits   []Edit // the edits applied
	Dropped int    // edits left out: From is not in the text on its own, or the edit is no repair
	Why     string // why there is no answer to apply; "" when the answer was applied, edits or none
	Err     error
	Usage   Usage
	Wall    time.Duration
}

// Run makes the call for text and applies its answer. It waits for the
// provider as long as ctx lets it.
func (s Step) Run(ctx context.Context, text string) Result {
	t0 := time.Now()
	output := OutputOf(s.Mode)
	reply, err := s.Provider.Refine(ctx, Request{Model: s.Model, Effort: s.Effort, System: s.System, PerTake: s.PerTake,
		Context: s.Context, Names: s.Names, Image: s.Image, Dictation: text, Output: output})
	r := Result{Text: text, Usage: reply.Usage, Wall: time.Since(t0)}
	if err != nil {
		r.Err = err
		var he *HTTPError
		switch {
		case ctx.Err() != nil:
			r.Why, r.Err = WhyCancelled, fmt.Errorf("cancelled after %s", r.Wall.Round(time.Millisecond))
		case errors.Is(err, ErrRefusal):
			r.Why = WhyRefusal
		case errors.Is(err, ErrUnparseable):
			r.Why = WhyParse
		case errors.As(err, &he):
			r.Why = WhyHTTP
		default:
			r.Why = WhyError
		}
		return r
	}
	if output == FullText {
		if err := CheckText(s.Mode, text, reply.Text); err != nil {
			r.Why, r.Err = WhyGuard, err
			return r
		}
		r.Text = reply.Text
		return r
	}
	out, applied, dropped, err := Apply(text, reply.Edits)
	r.Dropped = dropped
	if err != nil {
		r.Why, r.Err = WhyGuard, err
		return r
	}
	r.Text, r.Edits = out, applied
	return r
}

// The fix-words guards: past any of them an answer is a rewrite or a reply,
// not a repair of mis-heard words, and none of it is applied. They measure
// an edit's changed part (Changed), so the unchanged words a model quotes
// around a mis-hearing to place it do not count, and they measure it by
// size, so a term weighs the same spelled in letters and written by sound
// in Chinese characters.
const (
	maxFrom  = 32  // characters in the changed part of a From
	toSlack  = 8   // a To's changed part may be twice the size of its From's, plus this
	maxShare = 0.3 // the edits' size over the text's, once over shareFloor
	// shareFloor: two mis-heard terms can be most of a one-line dictation.
	shareFloor = 32
	// shortShare is how much of a dictation the edits may take out: a
	// quarter of it stays as it was said, however short it is.
	shortShare = 0.75
)

// size is how much s says, in letters: a Chinese character, a kana or a
// Hangul syllable counts as two, the letters a term takes for each
// character it is written with by sound.
func size(s string) (n int) {
	for _, r := range s {
		if wide(r) {
			n += 2
		} else {
			n++
		}
	}
	return n
}

func wide(r rune) bool {
	return unicode.In(r, unicode.Han, unicode.Hiragana, unicode.Katakana, unicode.Hangul)
}

// maxAdd is what an edit that takes nothing out may add: the rest of a
// word, a space.
const maxAdd = 4

// sentenceMarks are the punctuation of a sentence, which a term does not
// have: a hyphen, a dot, a slash or an underscore can be part of a term.
const sentenceMarks = "，。？！；：、（）“”‘’《》…?!;,()\"\n"

// repair reports whether putting to in place of from (an edit's changed
// part) has the shape of a mis-heard word put right. A reply, a gloss and a
// tidy-up have another: they add Chinese, change Chinese for Chinese of
// another length, add sentence punctuation, or add more than maxAdd where
// they take nothing out.
func repair(from, to string) bool {
	count := func(s string) (wides, letters, marks int) {
		for _, r := range s {
			switch {
			case wide(r):
				wides++
			case spaced(r):
				letters++
			case strings.ContainsRune(sentenceMarks, r):
				marks++
			}
		}
		return
	}
	wf, lf, mf := count(from)
	wt, lt, mt := count(to)
	switch {
	case from == "" && size(to) > maxAdd,
		wt > wf,
		lf == 0 && lt == 0 && wt != wf,
		mt > mf:
		return false
	}
	return true
}

// inWord reports whether text[at:end] is part of a longer word: it starts
// or ends with a letter or digit of a script that spaces its words, and
// the character beside that one in text is another.
func inWord(text string, at, end int) bool {
	first, _ := utf8.DecodeRuneInString(text[at:])
	before, _ := utf8.DecodeLastRuneInString(text[:at])
	last, _ := utf8.DecodeLastRuneInString(text[:end])
	after, _ := utf8.DecodeRuneInString(text[end:])
	return spaced(first) && spaced(before) || spaced(last) && spaced(after)
}

// Apply makes the edits to text, each wherever its From occurs in text as
// it stood, except inside a longer word: an edit whose From is not in text
// on its own, or that is no repair of a mis-heard word (repair), is dropped
// and counted, and where two edits claim the same stretch the earlier one
// has it. It returns the new text and the edits it made; an edit past the
// fix-words guards rejects them all.
func Apply(text string, edits []Edit) (out string, applied []Edit, dropped int, err error) {
	type span struct {
		at, end int
		to      string
	}
	var spans []span
	changed, gone := 0, 0 // the size the edits change, and what they take out of text
	for _, e := range edits {
		if e.From == "" || e.From == e.To || !strings.Contains(text, e.From) {
			dropped++
			continue
		}
		from, to := Changed(e.From, e.To)
		nf, nt := utf8.RuneCountInString(from), utf8.RuneCountInString(to)
		if nf > maxFrom {
			return text, nil, dropped, fmt.Errorf("edit %q → %q rewrites %d characters, over %d", e.From, e.To, nf, maxFrom)
		}
		sf, st := size(from), size(to)
		if st > 2*sf+toSlack {
			return text, nil, dropped, fmt.Errorf("edit %q → %q puts %d characters in place of %d", e.From, e.To, nt, nf)
		}
		if !repair(from, to) {
			dropped++
			continue
		}
		made := false
		for i := 0; ; {
			j := strings.Index(text[i:], e.From)
			if j < 0 {
				break
			}
			s := span{i + j, i + j + len(e.From), e.To}
			i = s.end
			if inWord(text, s.at, s.end) || slices.ContainsFunc(spans, func(x span) bool { return s.at < x.end && x.at < s.end }) {
				continue
			}
			spans = append(spans, s)
			changed += max(sf, st)
			gone += sf
			made = true
		}
		if made {
			applied = append(applied, e)
		} else {
			dropped++
		}
	}
	total := float64(size(text))
	if c := float64(changed); c > shareFloor && c > maxShare*total {
		return text, nil, dropped, fmt.Errorf("the edits change %.0f%% of the text, over %.0f%%", 100*c/total, maxShare*100)
	}
	if g := float64(gone); g > shortShare*total {
		return text, nil, dropped, fmt.Errorf("the edits replace %.0f%% of the text, over %.0f%%", 100*g/total, shortShare*100)
	}
	sort.Slice(spans, func(a, b int) bool { return spans[a].at < spans[b].at })
	var b strings.Builder
	at := 0
	for _, s := range spans {
		b.WriteString(text[at:s.at])
		b.WriteString(s.to)
		at = s.end
	}
	b.WriteString(text[at:])
	return b.String(), applied, dropped, nil
}

// Changed is the part of from and of to that differ: each without the
// characters they share at the start and at the end.
func Changed(from, to string) (string, string) {
	f, t := []rune(from), []rune(to)
	for len(f) > 0 && len(t) > 0 && f[0] == t[0] {
		f, t = f[1:], t[1:]
	}
	for len(f) > 0 && len(t) > 0 && f[len(f)-1] == t[len(t)-1] {
		f, t = f[:len(f)-1], t[:len(t)-1]
	}
	return string(f), string(t)
}
