// Package compare is megavoice's compare mode: each take also goes to the
// engines selected beside the primary, their answers are kept as the take's
// record, <take>.compare.json, and a label — the user's corrected text or a
// note — is appended to labels.jsonl as an asrbench manifest row. The
// primary delivers as it always does: nothing here is waited on.
package compare

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"math"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/caoer/mega-asr/internal/asr"
	"github.com/caoer/mega-asr/internal/audio"
	"github.com/caoer/mega-asr/internal/post"
	"github.com/caoer/mega-asr/internal/session"
)

// Engine is one engine a take can go to: one that streams the take as it
// records, or one that transcribes its WAV once it ends.
type Engine struct {
	Name   string
	Stream asr.Streamer
	File   func(ctx context.Context, wav string) (string, error)
}

// Fanout makes each take's tee.
type Fanout struct {
	Primary string // asr.engine: its text is delivered
	Local   string // the chunk engine, which stands in when the primary's stream fails
	// Engines are the engines to run beside the primary on the next take;
	// none when compare mode is off. Called as each take starts.
	Engines func() []Engine
	Post    post.Chain
	// Timeout bounds a secondary's answer from the take's end, plus half
	// the take's length (0: 2 min).
	Timeout time.Duration
	// Saved, when set, gets each record once it is written.
	Saved func(Record)

	wg sync.WaitGroup // records being collected
}

// Wait blocks until every record in flight is written.
func (f *Fanout) Wait() { f.wg.Wait() }

// Record is a compare take: every engine's answer, the primary's included.
type Record struct {
	ID          string                  `json:"id"`
	WAV         string                  `json:"wav"`
	DurS        float64                 `json:"dur_s"`
	Stopped     time.Time               `json:"stopped"`
	Primary     string                  `json:"primary"`
	DeliveredBy string                  `json:"delivered_by"` // the primary, or the local engine when the primary's stream failed
	Engines     map[string]EngineResult `json:"engines"`
	// Silent: the take's main track was never heard (audio.Hearing) —
	// digital silence throughout, or no more than a pop as its input
	// opened — so any engine's text of it is invented; no label takes an
	// engine's text as its reference.
	Silent bool `json:"silent,omitempty"`
}

// EngineResult is one engine's answer; latency is from the take's stop.
type EngineResult struct {
	Text      string `json:"text"`          // after the post chain (corrections, fillers), as it would be delivered
	Raw       string `json:"raw,omitempty"` // the engine's own text
	LatencyMS int64  `json:"latency_ms"`
	Error     string `json:"error,omitempty"`
	// Words are the words of Raw with their times in the take, from an
	// engine that gives them (asr.Timed: doubao); none from the others.
	Words []asr.Word `json:"words,omitempty"`
}

// RecordPath is the compare record beside a take's WAV.
func RecordPath(wav string) string { return strings.TrimSuffix(wav, ".wav") + ".compare.json" }

// ReadRecord reads a take's compare record; nil when it has none.
func ReadRecord(wav string) (*Record, error) {
	b, err := os.ReadFile(RecordPath(wav))
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var r Record
	if err := json.Unmarshal(b, &r); err != nil {
		return nil, fmt.Errorf("%s: %w", RecordPath(wav), err)
	}
	return &r, nil
}

// Tee is session.Controller.Tee: nil when no engine runs beside the
// primary for this take.
func (f *Fanout) Tee(base string) session.TakeTee {
	var runs []*run
	for _, e := range f.Engines() {
		if e.Name == f.Primary {
			continue
		}
		r := &run{e: e}
		if e.Stream != nil {
			r.stream = e.Stream.Open()
		}
		runs = append(runs, r)
	}
	if len(runs) == 0 {
		return nil
	}
	return &tee{f: f, base: base, runs: runs, primary: make(chan session.TakeResult, 1)}
}

// tee is one take's fan-out.
type tee struct {
	f       *Fanout
	base    string
	runs    []*run
	primary chan session.TakeResult
	hearing audio.Hearing // whether the take's main track was heard
}

type run struct {
	e      Engine
	stream asr.Stream
	res    EngineResult
}

func (t *tee) Write(s []int16) {
	t.hearing.Add(s)
	for _, r := range t.runs {
		if r.stream != nil {
			r.stream.Write(s)
		}
	}
}

func (t *tee) End(stopped time.Time, wav string, samples int) {
	if wav == "" {
		for _, r := range t.runs {
			if r.stream != nil {
				r.stream.Abort()
			}
		}
		return
	}
	t.f.wg.Add(1)
	go t.collect(stopped, wav, samples)
}

func (t *tee) Done(r session.TakeResult) {
	select {
	case t.primary <- r:
	default:
	}
}

// collect runs every secondary at once, waits for them and the primary,
// and writes the record.
func (t *tee) collect(stopped time.Time, wav string, samples int) {
	defer t.f.wg.Done()
	dur := time.Duration(samples) * time.Second / audio.Rate
	timeout := t.f.Timeout
	if timeout == 0 {
		timeout = 2 * time.Minute
	}
	ctx, cancel := context.WithTimeout(context.Background(), timeout+dur/2)
	defer cancel()
	var wg sync.WaitGroup
	for _, r := range t.runs {
		wg.Add(1)
		go func() {
			defer wg.Done()
			var raw string
			var err error
			var words []asr.Word
			switch {
			case r.stream != nil:
				raw, err = r.stream.Finish(ctx)
				if tw, ok := r.stream.(asr.Timed); ok && err == nil {
					words = tw.Words()
				}
			case r.e.File != nil:
				raw, err = r.e.File(ctx, wav)
			default:
				err = fmt.Errorf("engine %s cannot take a file", r.e.Name)
			}
			r.res = result(t.f.Post, raw, err, time.Since(stopped))
			r.res.Words = words
		}()
	}
	wg.Wait()
	p := <-t.primary

	rec := Record{
		ID: filepath.Base(t.base), WAV: wav, DurS: math.Round(dur.Seconds()*100) / 100, Stopped: stopped,
		Primary: t.f.Primary, DeliveredBy: t.f.Primary, Engines: map[string]EngineResult{}, Silent: !t.hearing.Heard(),
	}
	for _, r := range t.runs {
		rec.Engines[r.e.Name] = r.res
	}
	delivered := EngineResult{Text: p.Text, Raw: p.Raw, LatencyMS: p.Latency.Milliseconds()}
	if p.Failed > 0 {
		delivered.Error = fmt.Sprintf("%d chunks failed", p.Failed)
	}
	if p.StreamErr != nil {
		rec.DeliveredBy = t.f.Local
		rec.Engines[t.f.Primary] = EngineResult{Error: p.StreamErr.Error(), LatencyMS: p.Latency.Milliseconds()}
		rec.Engines[t.f.Local] = delivered
	} else {
		rec.Engines[t.f.Primary] = delivered
	}
	if err := writeJSON(RecordPath(wav), rec); err != nil {
		log.Printf("compare: %v", err)
		return
	}
	var line []string
	for name, e := range rec.Engines {
		s := fmt.Sprintf("%s %d ms", name, e.LatencyMS)
		if e.Error != "" {
			s += " (" + e.Error + ")"
		}
		line = append(line, s)
	}
	log.Printf("compare: %s: %s", rec.ID, strings.Join(line, ", "))
	if t.f.Saved != nil {
		t.f.Saved(rec)
	}
}

func result(p post.Chain, raw string, err error, latency time.Duration) EngineResult {
	r := EngineResult{Raw: raw, LatencyMS: latency.Milliseconds()}
	if err != nil {
		r.Raw, r.Error = "", err.Error()
		return r
	}
	r.Text = p.Apply(raw)
	return r
}

// writeJSON writes v to path through a temporary file, so a reader never
// sees half a record.
func writeJSON(path string, v any) error {
	b, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), ".compare-*.json")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if _, err := tmp.Write(append(b, '\n')); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Chmod(tmp.Name(), 0o644); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), path)
}
