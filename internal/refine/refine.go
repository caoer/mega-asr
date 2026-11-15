// Package refine is dictation's model step: after recognition and the post
// chain, one model call returns the changes to make to a take's text, and
// the step applies them under its mode's guards. When the call is slow,
// fails or is refused, when the answer does not parse, or when a change
// fails a guard, the text stands as it was.
package refine

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"sort"
	"strings"
	"time"
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
	// the mode's prompt, then the speaker's glossary. The last block is the
	// cache breakpoint.
	System []string
	// Context is what was in front of the speaker, sent inside <context>;
	// "" sends none.
	Context string
	// Dictation is the take's text, sent inside <dictation>: material for
	// the model, never a message to it.
	Dictation string
}

// UserContent is the user message of r: the context, when there is one, and
// the dictation, each inside its tag.
func (r Request) UserContent() string {
	var b strings.Builder
	if r.Context != "" {
		b.WriteString("<context>\n" + r.Context + "\n</context>\n")
	}
	b.WriteString("<dictation>\n" + r.Dictation + "\n</dictation>")
	return b.String()
}

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

// Reply is a provider's answer: the edit list.
type Reply struct {
	Edits []Edit
	Usage Usage
}

// Provider makes the call. Its error is ErrRefusal when the model declined,
// ErrUnparseable when the answer is not an edit list, an *HTTPError for a
// status other than 200, and the context's when the call outlasted it.
type Provider interface {
	Refine(ctx context.Context, req Request) (Reply, error)
}

var (
	ErrRefusal     = errors.New("the model declined")
	ErrUnparseable = errors.New("the answer is not an edit list")
)

// HTTPError is a provider's status other than 200, with the head of its body.
type HTTPError struct {
	Status int
	Body   string
}

func (e *HTTPError) Error() string { return fmt.Sprintf("status %d: %s", e.Status, e.Body) }

// Why a step left the text as it stood (Result.Why).
const (
	WhyTimeout = "timeout" // the call outlasted Step.Timeout
	WhyHTTP    = "http"    // a status other than 200
	WhyRefusal = "refusal" // the model declined
	WhyParse   = "parse"   // the answer is not an edit list
	WhyGuard   = "guard"   // a change failed the mode's guard
	WhyError   = "error"   // anything else: no route, a cancelled take
)

// Step is the model step as one take runs it.
type Step struct {
	Provider Provider
	Model    string
	Effort   string
	System   []string      // Request.System
	Timeout  time.Duration // past it the text stands as it was
}

// Result is what a step did to a text.
type Result struct {
	Text    string // the text to deliver: the input unless edits were applied
	Edits   []Edit // the edits applied
	Dropped int    // edits left out: From is not in the text, or equals To
	Why     string // why the text stands as it was; "" when the answer was applied, edits or none
	Err     error
	Usage   Usage
	Wall    time.Duration
}

// Run makes the call for text and applies its answer.
func (s Step) Run(ctx context.Context, text string) Result {
	ctx, cancel := context.WithTimeout(ctx, s.Timeout)
	defer cancel()
	t0 := time.Now()
	reply, err := s.Provider.Refine(ctx, Request{Model: s.Model, Effort: s.Effort, System: s.System, Dictation: text})
	r := Result{Text: text, Usage: reply.Usage, Wall: time.Since(t0)}
	if err != nil {
		r.Err = err
		var he *HTTPError
		switch {
		case errors.Is(err, context.DeadlineExceeded) || errors.Is(ctx.Err(), context.DeadlineExceeded):
			r.Why, r.Err = WhyTimeout, fmt.Errorf("no answer within %s", s.Timeout)
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
	out, applied, dropped, err := Apply(text, reply.Edits)
	r.Dropped = dropped
	if err != nil {
		r.Why, r.Err = WhyGuard, err
		return r
	}
	r.Text, r.Edits = out, applied
	return r
}

// The fix-words guards: past any of them an answer is a rewrite, not a
// repair of mis-heard words, and none of it is applied. Lengths are in
// characters of an edit's changed part (Changed), so the unchanged words a
// model quotes around a mis-hearing to place it do not count.
const (
	maxFrom  = 32  // the changed part of a From
	toSlack  = 8   // a To's changed part may be three times its From's, plus this
	maxShare = 0.3 // changed characters over the text's, once over shareFloor
	// shareFloor: two mis-heard terms can be most of a one-line dictation.
	shareFloor = 24
)

// Apply makes the edits to text, each wherever its From occurs in text as
// it stood: an edit whose From is not in text, or equals its To, is dropped
// and counted, and where two edits claim the same stretch the earlier one
// has it. It returns the new text and the edits it made; an edit past the
// fix-words guards rejects them all.
func Apply(text string, edits []Edit) (out string, applied []Edit, dropped int, err error) {
	type span struct {
		at, end int
		to      string
	}
	var spans []span
	changed := 0
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
		if nt > 3*nf+toSlack {
			return text, nil, dropped, fmt.Errorf("edit %q → %q puts %d characters in place of %d", e.From, e.To, nt, nf)
		}
		made := false
		for i := 0; ; {
			j := strings.Index(text[i:], e.From)
			if j < 0 {
				break
			}
			s := span{i + j, i + j + len(e.From), e.To}
			i = s.end
			if slices.ContainsFunc(spans, func(x span) bool { return s.at < x.end && x.at < s.end }) {
				continue
			}
			spans = append(spans, s)
			changed += nf
			made = true
		}
		if made {
			applied = append(applied, e)
		} else {
			dropped++
		}
	}
	if total := utf8.RuneCountInString(text); changed > shareFloor && float64(changed) > maxShare*float64(total) {
		return text, nil, dropped, fmt.Errorf("the edits change %d of %d characters, over %.0f%%", changed, total, maxShare*100)
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
