package names

import (
	"context"
	"errors"
	"sort"
	"strings"
	"sync"
	"time"
	"unicode/utf8"
)

// Caller asks the model once: system prompt, user message, reply text. An
// error wrapping ErrRefused is the subscription refusing: the pass stops
// there.
type Caller func(ctx context.Context, system, user string) (string, error)

// ErrRefused is a call the model's account refused (a rate or usage limit).
var ErrRefused = errors.New("refused by the model's account")

// Transcript is one transcript's texts in order: our segments, or Feishu's
// turns.
type Transcript struct {
	Name  string // ours | feishu
	Texts []string
}

// Pass is one run over a record's transcripts.
type Pass struct {
	Index       *Index
	Speakers    []string // the recording's speakers, by name
	Call        Caller
	Concurrency int // calls at once; below 1 is 1
	WindowChars int // runes a window gathers from consecutive texts, at least one text; below 1 is one text a window
	// Focus, when set, limits the pass to the windows with a candidate for
	// one of these people (Index.People indices): a speaker just named.
	Focus []int
}

// Change is one text a pass rewrote.
type Change struct {
	Transcript string `json:"src"`
	Index      int    `json:"i"`
	Before     string `json:"before"`
	After      string `json:"text"`
	Names      []Name `json:"names"`
}

// Name is one name the pass wrote into a text: what stands in the text now,
// the person's slug, and the form it replaced.
type Name struct {
	Text   string `json:"text"`
	Person string `json:"person"`
	Heard  string `json:"heard"`
}

// Dropped is an edit a guard refused.
type Dropped struct {
	Transcript string `json:"src"`
	Window     int    `json:"w"`
	Edit
	Why string `json:"why"`
}

// PassResult is what a pass did.
type PassResult struct {
	Texts   map[string][]string // source → its texts after the pass
	Changes []Change
	Dropped []Dropped
	Windows int // windows the transcripts make
	Calls   int // calls answered
	Failed  int // calls that failed (no answer, an unreadable reply)
	Refused string
	WallS   float64
}

type window struct {
	src   int
	first int // index of its first text
	n     int
}

// windows cuts each source into runs of consecutive non-empty texts of at
// most max runes (one text at least).
func windows(srcs []Transcript, max int) []window {
	var out []window
	for si, s := range srcs {
		cur, chars := window{src: si, first: -1}, 0
		flush := func() {
			if cur.n > 0 {
				out = append(out, cur)
			}
			cur, chars = window{src: si, first: -1}, 0
		}
		for i, t := range s.Texts {
			n := utf8.RuneCountInString(strings.TrimSpace(t))
			if n == 0 {
				if cur.n > 0 {
					cur.n++ // an empty text inside a window rides along
				}
				continue
			}
			if cur.n > 0 && (max < 1 || chars+n > max) {
				flush()
			}
			if cur.n == 0 {
				cur.first = i
			}
			cur.n = i - cur.first + 1
			chars += n
		}
		flush()
	}
	return out
}

// Run calls the model once per window, guards its edits, and applies the
// kept ones to every text of the window. A refusal stops the calls not yet
// made; what was answered is applied.
func (p Pass) Run(ctx context.Context, srcs []Transcript) PassResult {
	t0 := time.Now()
	res := PassResult{Texts: map[string][]string{}}
	texts := make([][]string, len(srcs))
	for i, s := range srcs {
		texts[i] = append([]string(nil), s.Texts...)
	}
	ws := windows(srcs, p.WindowChars)
	res.Windows = len(ws)
	if len(p.Focus) > 0 {
		focus := map[int]bool{}
		for _, f := range p.Focus {
			focus[f] = true
		}
		kept := ws[:0]
		for _, w := range ws {
			for _, c := range p.Index.Candidates(Join(texts[w.src][w.first : w.first+w.n])) {
				if focus[p.Index.byName[c.Person]] {
					kept = append(kept, w)
					break
				}
			}
		}
		ws = kept
	}
	type answer struct {
		edits []Edit
		ok    bool
	}
	answers := make([]answer, len(ws))
	var mu sync.Mutex
	cctx, stop := context.WithCancel(ctx)
	defer stop()
	next := make(chan int)
	go func() {
		defer close(next)
		for k := range ws {
			select {
			case next <- k:
			case <-cctx.Done():
				return
			}
		}
	}()
	var wg sync.WaitGroup
	for range max(p.Concurrency, 1) {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for k := range next {
				if cctx.Err() != nil {
					return
				}
				w := ws[k]
				text := Join(texts[w.src][w.first : w.first+w.n])
				reply, err := p.Call(cctx, Prompt, p.Index.User(text, p.Speakers))
				var edits []Edit
				if err == nil {
					edits, err = Parse(reply)
				}
				mu.Lock()
				switch {
				case errors.Is(err, ErrRefused):
					if res.Refused == "" {
						res.Refused = err.Error()
					}
					stop()
				case err != nil:
					if cctx.Err() == nil {
						res.Failed++
					}
				default:
					res.Calls++
					answers[k] = answer{edits, true}
				}
				mu.Unlock()
			}
		}()
	}
	wg.Wait()
	for k, w := range ws {
		if !answers[k].ok {
			continue
		}
		window := Join(texts[w.src][w.first : w.first+w.n])
		var kept []Edit
		var who []int
		for _, e := range answers[k].edits {
			person, why := p.Index.Guard(window, e)
			if why != "" {
				res.Dropped = append(res.Dropped, Dropped{Transcript: srcs[w.src].Name, Window: w.first, Edit: e, Why: why})
				continue
			}
			kept, who = append(kept, e), append(who, person)
		}
		if len(kept) == 0 {
			continue
		}
		for i := w.first; i < w.first+w.n; i++ {
			before := texts[w.src][i]
			after, fires := applyFires(before, kept, who)
			if len(fires) == 0 {
				continue
			}
			texts[w.src][i] = after
			c := Change{Transcript: srcs[w.src].Name, Index: i, Before: before, After: after}
			for _, f := range fires {
				c.Names = append(c.Names, Name{Text: f.to, Person: p.Index.People[f.person].Slug, Heard: f.from})
			}
			res.Changes = append(res.Changes, c)
		}
	}
	sort.SliceStable(res.Changes, func(i, j int) bool {
		if res.Changes[i].Transcript != res.Changes[j].Transcript {
			return res.Changes[i].Transcript > res.Changes[j].Transcript // ours before feishu
		}
		return res.Changes[i].Index < res.Changes[j].Index
	})
	for i, s := range srcs {
		res.Texts[s.Name] = texts[i]
	}
	res.WallS = time.Since(t0).Seconds()
	return res
}
