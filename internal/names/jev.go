package names

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strings"
	"time"
)

const (
	// URL is TypeSafe's System One endpoint.
	URL = "https://api.typesafe.ai/v1/systemone"
	// Model is the Jev model the design was measured on.
	Model = "jev-1.13.0"
	// Source tells Jev what the text is.
	Source = "a dictated note transcribed by speech recognition, Chinese mixed with English"

	none = "none"
	ctx  = 24 // characters of context each side of a span
)

// Edit is an accepted span. At selects its byte offset; without At, every
// occurrence of From becomes To, a listed form of Person.
type Edit struct {
	At     *int    `json:"at,omitempty"`
	From   string  `json:"from"`
	To     string  `json:"to"`
	Person string  `json:"person"`
	P      float64 `json:"p"`
}

// question is one Choice: which of the people the span sounds like, if
// any, the speaker meant there.
type question struct {
	id, span string
	at       int
	options  map[string]Candidate // criterion key → the form it offers
	body     obj
}

// questions groups candidates by span and asks one Choice per occurrence.
// The wording states the sound match the scan found, with pinyin, and asks
// only whether a person is meant at that spot: asked plainly whether a span
// is a misspelling, Jev cannot hear and says no.
func (r *Roster) questions(text string, cs []Candidate) []question {
	var order []string
	by := map[string][]Candidate{}
	for _, c := range cs {
		if _, ok := by[c.Span]; !ok {
			order = append(order, c.Span)
		}
		by[c.Span] = append(by[c.Span], c)
	}
	const lead = "Speech recognizers often write a spoken person's name as a different word or name that sounds " +
		"the same. A pinyin scan found that `span` sounds the same as, or very close to, the listed " +
		"name(s) below of people who work with the speakers."
	out := make([]question, 0, len(order))
	for gi, span := range order {
		for oi, at := range indexAll(text, span) {
			where := "Judge only the occurrence marked 【】 in `context`."
			qid := fmt.Sprintf("q%d", gi)
			if oi > 0 {
				qid += fmt.Sprintf("-%d", oi)
			}
			q := question{id: qid, span: span, at: at, options: map[string]Candidate{}}
			var crit obj
			for _, c := range by[span] {
				k := c.Person + ": " + c.Form
				what := "a name they are called by"
				if c.Form == c.Person {
					what = "the full name"
				}
				d := fmt.Sprintf("%s (%s) — %s of %s", c.Form, Pinyin(c.Form), what, c.Person)
				if o := without(r.others[c.Person], c.Form); len(o) > 0 {
					d += ", also called " + strings.Join(o, ", ")
				}
				d += "; the speaker is talking about or to this person here, and the recognizer wrote " + span
				crit = append(crit, kv{k, d})
				q.options[k] = c
			}
			crit = append(crit, kv{none, span + " is not a person being talked about or addressed here: it is an ordinary " +
				"word or phrase used with its own meaning, or a place, company, product or thing"})
			q.body = obj{
				{"type", "choice"},
				{"instructions", obj{
					{"about", lead}, {"span", span}, {"span_pinyin", Pinyin(span)}, {"context", markAt(text, span, at)},
					{"note", where},
					{"question", "At the spot where the recognizer wrote `span`, is the speaker referring to one of the " +
						"listed people (so `span` is the recognizer's misspelling of that name), or is `span` not a person here?"},
				}},
				{"criteria", crit},
			}
			out = append(out, q)
		}
	}
	return out
}

func without(xs []string, x string) []string {
	var out []string
	for _, y := range xs {
		if y != x {
			out = append(out, y)
		}
	}
	return out
}

// mark is the span's first occurrence with ctx characters each side, the
// span in 【】.
func mark(text, span string) string {
	return markAt(text, span, strings.Index(text, span))
}

func markAt(text, span string, i int) string {
	before, after := []rune(text[:i]), []rune(text[i+len(span):])
	if len(before) > ctx {
		before = before[len(before)-ctx:]
	}
	if len(after) > ctx {
		after = after[:ctx]
	}
	return string(before) + "【" + span + "】" + string(after)
}

// Jev is a client of the System One endpoint.
type Jev struct {
	URL, Model string
	Key        string
	Client     *http.Client // nil: http.DefaultClient
}

// Usage is what one request cost.
type Usage struct {
	InputTokens int `json:"input_tokens"`
}

type answer struct {
	Type          string             `json:"type"`
	Choice        string             `json:"choice"`
	Probabilities map[string]float64 `json:"probabilities"`
}

// ask sends every question in one request.
func (j Jev) ask(ctx context.Context, text string, qs []question) (map[string]answer, Usage, error) {
	var questions obj
	for _, q := range qs {
		questions = append(questions, kv{q.id, q.body})
	}
	body, err := json.Marshal(obj{
		{"model", j.Model},
		{"state", obj{{"source", Source}, {"text", text}}},
		{"questions", questions},
	})
	if err != nil {
		return nil, Usage{}, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, j.URL, bytes.NewReader(body))
	if err != nil {
		return nil, Usage{}, err
	}
	req.Header.Set("Authorization", "Bearer "+j.Key)
	req.Header.Set("Content-Type", "application/json")
	cl := j.Client
	if cl == nil {
		cl = http.DefaultClient
	}
	resp, err := cl.Do(req)
	if err != nil {
		return nil, Usage{}, err
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return nil, Usage{}, err
	}
	if resp.StatusCode != http.StatusOK {
		return nil, Usage{}, fmt.Errorf("jev: %s: %.200s", resp.Status, b)
	}
	var out struct {
		Answers map[string]answer `json:"answers"`
		Usage   Usage             `json:"usage"`
	}
	if err := json.Unmarshal(b, &out); err != nil {
		return nil, Usage{}, fmt.Errorf("jev: %w", err)
	}
	return out.Answers, out.Usage, nil
}

// decide turns the answers into edits: a span becomes the top option when
// that is a person with probability at least threshold.
func decide(qs []question, ans map[string]answer, threshold float64) []Edit {
	var edits []Edit
	for _, q := range qs {
		a, ok := ans[q.id]
		if !ok || a.Choice == none {
			continue
		}
		c, ok := q.options[a.Choice]
		if !ok || a.Probabilities[a.Choice] < threshold {
			continue
		}
		edits = append(edits, Edit{At: &q.at, From: q.span, To: c.Form, Person: c.Person, P: a.Probabilities[a.Choice]})
	}
	return edits
}

// Apply rewrites each edit's selected occurrence; an edit without At applies
// to all matches. Where two overlap,
// the earlier one, and of two at one place the longer, wins.
func Apply(text string, edits []Edit) string {
	type fire struct {
		at, end int
		to      string
	}
	var fs []fire
	for _, e := range edits {
		positions := indexAll(text, e.From)
		if e.At != nil {
			i := *e.At
			if i < 0 || i+len(e.From) > len(text) || text[i:i+len(e.From)] != e.From {
				continue
			}
			positions = []int{i}
		}
		for _, i := range positions {
			fs = append(fs, fire{i, i + len(e.From), e.To})
		}
	}
	sort.SliceStable(fs, func(a, b int) bool {
		if fs[a].at != fs[b].at {
			return fs[a].at < fs[b].at
		}
		return fs[a].end > fs[b].end
	})
	var b strings.Builder
	p := 0
	for _, f := range fs {
		if f.at < p {
			continue
		}
		b.WriteString(text[p:f.at])
		b.WriteString(f.to)
		p = f.end
	}
	b.WriteString(text[p:])
	return b.String()
}

// Result is what the gate did to one text.
type Result struct {
	Text       string        // the text to deliver
	Fired      bool          // a candidate sounded the same, and Jev was asked
	Candidates int           // candidate spans the scan found
	Questions  int           // spans Jev judged
	Edits      []Edit        // what was applied
	Wall       time.Duration // the Jev request, when it fired
	Usage      Usage
	Err        error // the request failed: Text is the input
}

// Gate is the dictation names gate.
type Gate struct {
	People    string                 // people.json, read at each take
	Key       func() (string, error) // the TypeSafe key, read at each fired take
	Jev       Jev                    // URL, Model and Client; Key is set from Key
	Threshold float64                // an option this probable or more is applied
	Timeout   time.Duration          // the Jev request's bound; 0: none
	// Ungated asks Jev whenever there is a candidate, same-sound or not: the
	// arm the gate was measured against, for `megavoice names --ungated`.
	Ungated bool
}

// Correct runs the gate over text. A text the gate does not fire on costs
// the scan alone; a failed request delivers the text as it was.
func (g Gate) Correct(ctx context.Context, text string) Result {
	res := Result{Text: text}
	r, err := ReadRoster(g.People)
	if err != nil || r.Empty() {
		return res
	}
	cs := r.Candidates(text)
	res.Candidates = len(cs)
	if !Fires(cs) && !(g.Ungated && len(cs) > 0) {
		return res
	}
	res.Fired = true
	j := g.Jev
	if j.Key, err = g.Key(); err != nil {
		res.Err = err
		return res
	}
	qs := r.questions(text, cs)
	res.Questions = len(qs)
	if g.Timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, g.Timeout)
		defer cancel()
	}
	t0 := time.Now()
	ans, u, err := j.ask(ctx, text, qs)
	res.Wall, res.Usage = time.Since(t0), u
	if err != nil {
		res.Err = err
		return res
	}
	res.Edits = decide(qs, ans, g.Threshold)
	res.Text = Apply(text, res.Edits)
	return res
}

// obj is a JSON object that keeps its keys in order: the options of a
// Choice reach the model as written.
type obj []kv

type kv struct {
	k string
	v any
}

func (o obj) MarshalJSON() ([]byte, error) {
	var b bytes.Buffer
	b.WriteByte('{')
	for i, x := range o {
		if i > 0 {
			b.WriteByte(',')
		}
		k, err := json.Marshal(x.k)
		if err != nil {
			return nil, err
		}
		v, err := json.Marshal(x.v)
		if err != nil {
			return nil, err
		}
		b.Write(k)
		b.WriteByte(':')
		b.Write(v)
	}
	b.WriteByte('}')
	return b.Bytes(), nil
}
