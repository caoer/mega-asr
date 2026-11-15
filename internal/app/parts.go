package app

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/caoer/mega-asr/internal/asr"
	"github.com/caoer/mega-asr/internal/audio"
	"github.com/caoer/mega-asr/internal/compare"
	"github.com/caoer/mega-asr/internal/l10n"
	"github.com/caoer/mega-asr/internal/names"
	"github.com/caoer/mega-asr/internal/post"
	"github.com/caoer/mega-asr/internal/refine"
	"github.com/caoer/mega-asr/internal/session"
	"github.com/caoer/mega-asr/internal/store"
	"github.com/caoer/mega-asr/internal/takes"
)

// Chunker cuts a take at pauses as it records.
func (c Config) Chunker() session.Chunker {
	return &audio.Chunker{Pause: time.Duration(c.Take.ChunkPause), Max: time.Duration(c.Take.ChunkMax)}
}

// Transcriber is what a controller transcribes chunks with: the resident
// process with the CLI as fallback, or, in exec mode, the CLI alone. The
// hotword list is read at every chunk, so edits take effect at the next one.
// The rechecks (rechecks.tsv and rechecks.d/*.tsv, asr.Recheck) are read at
// every chunk too.
func (c Config) Transcriber() asr.Transcriber {
	if c.ASR.FunASR.Mode == "exec" {
		return asr.Rechecking{Transcriber: c.Exec(), Rechecks: rechecks}
	}
	return c.Resident()
}

// PlainTranscriber is Transcriber without the hotwords and rechecks.
func (c Config) PlainTranscriber() asr.Transcriber {
	if c.ASR.FunASR.Mode == "exec" {
		return c.exec(nil)
	}
	return c.resident(nil, nil)
}

func (c Config) Resident() *Fallback { return c.resident(hotwords, rechecks) }

func (c Config) resident(hw func() []string, rc func() []asr.Recheck) *Fallback {
	f := c.ASR.FunASR
	return &Fallback{
		new: func() *asr.Resident {
			return &asr.Resident{Root: f.Root, LLM: f.LLM, Hotwords: hw, GPU: f.GPULayers, EncGPU: f.Encoder == "gpu", VADGPU: f.VAD == "gpu", Threads: f.Threads, LLMThreads: f.LLMThreads}
		},
		exec:     c.exec(hw),
		rechecks: rc,
	}
}

func hotwords() []string { return asr.ReadHotwords(HotwordsFiles()...) }

func rechecks() []asr.Recheck { return asr.ReadRechecks(RechecksFiles()...) }

// Exec runs the CLI once per clip, with the resident process's engine flags.
func (c Config) Exec() asr.FunASR { return c.exec(hotwords) }

func (c Config) exec(hw func() []string) asr.FunASR {
	f := c.ASR.FunASR
	return asr.FunASR{Root: f.Root, LLM: f.LLM, Hotwords: hw, GPU: f.GPULayers, EncGPU: f.Encoder == "gpu", VADGPU: f.VAD == "gpu", Threads: f.Threads, LLMThreads: f.LLMThreads}
}

// Post is the chain applied after ASR: the correction tables, then filler
// removal with post.join_words.
func (c Config) Post() post.Chain {
	return post.Chain{post.CorrectionsFiles(CorrectionsFiles), post.Filler(c.Postprocess.JoinWords)}
}

// namesURL is where the names gate asks Jev.
var namesURL = names.URL

// NamesGate is the dictation names gate of [names] (NamesConfig).
func (c Config) NamesGate() names.Gate {
	n := c.Names
	return names.Gate{People: n.People, Key: n.Key, Jev: names.Jev{URL: namesURL, Model: names.Model},
		Threshold: n.Threshold, Timeout: time.Duration(n.Timeout)}
}

// NamesStage is session.Controller.Names for this config: nil while
// names.on is off. A take the gate fires on gets a names line. The
// controller leaves out a take the model step runs on.
func (c Config) NamesStage() func(ctx context.Context, text string) (string, *store.Event) {
	if !c.Names.On {
		return nil
	}
	g := c.NamesGate()
	return func(ctx context.Context, text string) (string, *store.Event) {
		r := g.Correct(ctx, text)
		if !r.Fired {
			return text, nil
		}
		ev := &store.Event{Ev: "names", N: r.Questions, LatencyMS: r.Wall.Milliseconds()}
		for _, e := range r.Edits {
			ev.Names = append(ev.Names, store.NameEdit{From: e.From, To: e.To, Person: e.Person, P: e.P})
		}
		if r.Err != nil {
			ev.Err = r.Err.Error()
			log.Printf("names: %v; delivering the text as decoded", r.Err)
		}
		return r.Text, ev
	}
}

// RefineStage is the model step of [refine] (RefineConfig) for a
// controller: Take is session.Controller.Refine and Warm its Warm. Each
// looks at the config file (o) as it is called and reads it when it has
// changed, so the menu's switch and `config set refine.on` apply at the
// next take; while the file stays as it is, a take reads nothing.
type RefineStage struct {
	// Display is this machine's screen, for the step's screen source; nil
	// where there is none to capture, and the source stays off.
	Display refine.Display
	// Clip watches this machine's clipboard, for the clipboard source; nil
	// where there is none, and the source sends nothing.
	Clip *refine.ClipWatch

	o    LoadOpts
	mu   sync.Mutex
	last RefineConfig
	seen fileStamp // the file as last read; the zero value before the first look
}

// fileStamp is what tells a file from its next version without reading it.
type fileStamp struct {
	looked bool // false only before the first look
	mod    time.Time
	size   int64 // -1 when there is no file
}

func (c Config) RefineStage(o LoadOpts) *RefineStage {
	o.Check = nil
	return &RefineStage{o: o, last: c.RefineTake()}
}

// current is [refine] as the file reads now; as it last loaded when the
// file no longer does, which is logged once for each version of the file.
func (s *RefineStage) current() RefineConfig {
	s.mu.Lock()
	defer s.mu.Unlock()
	path, _ := Path(s.o.Path)
	now := fileStamp{looked: true, size: -1}
	if fi, err := os.Stat(path); err == nil {
		now.mod, now.size = fi.ModTime(), fi.Size()
	}
	if now == s.seen {
		return s.last
	}
	s.seen = now
	if l, err := Load(s.o); err != nil {
		log.Printf("refine: %v; keeping refine.on = %v", err, s.last.On)
	} else {
		s.last = l.RefineTake()
	}
	return s.last
}

// Take gives a take's model step; nil while refine.on is off: no call is
// made and no line written.
func (s *RefineStage) Take() *session.RefineStep {
	r := s.current()
	if !r.On {
		return nil
	}
	return &session.RefineStep{
		Run: func(ctx context.Context, in session.RefineInput) (string, *store.Event) {
			text, ev := r.RunTake(ctx, in)
			if ev.Why == "" {
				ev.Copy = r.AgentCopy // the controller delivers by it (session agentCopy)
			}
			return text, ev
		},
		Aside: r.Aside,
	}
}

// Config is [refine] as the file reads now.
func (s *RefineStage) Config() RefineConfig { return s.current() }

// StepIn is the model step of [refine] as the file reads now, in mode: a
// person's run again on a kept take from the takes page, one call, made
// whether refine.on is on or off. A mode refine.mode cannot name now
// (RefineModeNames) is refused. Its line carries no agent copy: the send settles that.
func (s *RefineStage) StepIn(mode string) (*session.RefineStep, error) {
	if names := RefineModeNames(); !slices.Contains(names, mode) {
		return nil, fmt.Errorf("refine.mode %q: want one of %s", mode, strings.Join(names, ", "))
	}
	r := s.current()
	r.Mode = mode
	return &session.RefineStep{Run: r.RunTake}, nil
}

// Screen is session.Controller.Screen: as a take starts it sets the screen
// source to work on the window in front and gives the call that waits for
// what it got. nil while refine.on or refine.context.screen is off: nothing
// is captured.
func (s *RefineStage) Screen() func(context.Context) *refine.Screen {
	r := s.current()
	mode := r.Context.Screen
	if !r.On || s.Display == nil || mode != refine.ScreenOCR && mode != refine.ScreenImage {
		return nil
	}
	src := refine.ScreenSource{Mode: mode, Exclude: r.Context.Exclude, Display: s.Display}
	got := make(chan *refine.Screen, 1)
	go func() { got <- src.Take() }()
	return func(ctx context.Context) *refine.Screen {
		late := time.NewTimer(screenWait)
		defer late.Stop()
		select {
		case sc := <-got:
			return sc
		case <-late.C:
			log.Printf("refine: the screen was not ready %s after the take; it goes without it", screenWait)
		case <-ctx.Done():
		}
		return &refine.Screen{Mode: mode, Withheld: refine.WithheldLate}
	}
}

// screenWait is the most a take waits, once it has stopped, for the
// capture its start set going; a capture later than that is left behind
// (refine.WithheldLate) and the take runs without the screen. Reading the
// text of a full Retina window took about 1.4 s on an M1 Max.
var screenWait = 2 * time.Second

// Sources is session.Controller.Sources: as a take starts it notes the
// clipboard and gives the call that reads the text sources from the take's
// target at its stop. nil while refine.on is off or every text source is:
// nothing is read.
func (s *RefineStage) Sources() func(context.Context, session.Target) []refine.Part {
	r := s.current()
	src := r.Context.TextSources(s.Clip)
	if !r.On || !src.On() {
		return nil
	}
	ts := src.Start()
	return func(ctx context.Context, t session.Target) []refine.Part {
		spot, _ := t.(refine.Spot)
		return ts.Read(ctx, session.StoreTarget(t), spot)
	}
}

// ClipboardOn reports whether the clipboard source is switched on, for its
// watcher (refine.ClipWatch.On).
func (s *RefineStage) ClipboardOn() bool {
	r := s.current()
	return r.On && r.Context.Clipboard
}

// NotesAgent reports whether a take's capture should note the agent in its
// herdr pane: only while the step is on with agent_copy both or raw, the
// values that deliver differently to an agent, or with the app or pane
// source, which name it.
func (s *RefineStage) NotesAgent() bool {
	r := s.current()
	return r.On && (r.AgentCopy != "refined" || r.Context.App || r.Context.Pane)
}

// warmLimit bounds the handshake Warm opens the connection with. A take's
// wait for the model's answer has no bound.
const warmLimit = 10 * time.Second

// Warm opens the provider's connection as a take starts, so the call at
// its end does not wait for the handshake; nothing while refine.on is off.
func (s *RefineStage) Warm() {
	r := s.current()
	if !r.On {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), warmLimit)
	defer cancel()
	if err := (refine.Anthropic{BaseURL: r.Providers[r.Provider].BaseURL}).Warm(ctx); err != nil {
		log.Printf("refine: opening the connection: %v", err)
	}
}

// Aside reports whether a take bound for target is delivered as recognised
// whatever the model answers: agent_copy raw toward an agent's target. Such
// a take does not wait for the call.
func (r RefineConfig) Aside(target *store.Target) bool {
	return r.AgentCopy == "raw" && target.IsAgent()
}

// Run is the model step on one text bound for target: the text to deliver
// and the take's refine line. It waits for the model as long as ctx lets
// it. Whatever keeps the step from an answer it can apply — no key, a
// failed call, a refusal, an answer past the guards, a cancel — the text
// comes back as it was, and the line says why.
func (r RefineConfig) Run(ctx context.Context, text string, target *store.Target) (string, *store.Event) {
	return r.RunTake(ctx, session.RefineInput{Text: text, Target: target})
}

// RunTake is Run on a take as the session hands it over: its text, its
// target and what its screen source got, which the call carries and the
// refine line records as sent.
func (r RefineConfig) RunTake(ctx context.Context, in session.RefineInput) (string, *store.Event) {
	text := in.Text
	ev := &store.Event{V: 1, Ev: "refine", Mode: r.Mode, Provider: r.Provider, Model: r.Model, Effort: r.Effort}
	fail := func(why string, err error) (string, *store.Event) {
		ev.At, ev.Why, ev.Err = time.Now(), why, err.Error()
		log.Printf("refine: %s: %v", why, err)
		return text, ev
	}
	p := r.Providers[r.Provider]
	key, err := p.Key()
	if err != nil {
		return fail("key", err)
	}
	prompt, err := r.Prompt()
	if err != nil {
		return fail(refine.WhyError, err)
	}
	step := refine.Step{Provider: refine.Anthropic{BaseURL: p.BaseURL, Key: key}, Mode: r.Mode, Model: r.Model, Effort: r.Effort,
		System: append([]string{prompt}, Glossary().Reach(r.GlossaryReach(), text).Blocks()...), Names: r.namesIn(text)}
	sc := in.Screen
	if sc != nil {
		sc = r.screenNow(sc)
	}
	step.Context = refine.Compose(in.Parts, sc) // before the screen's record: it may withhold the screen
	ev.Context, ev.Sources = refine.InTag(step.Context), SourcesRecord(in.Parts)
	if sc != nil {
		step.Image, ev.Screen = sc.Image, ScreenRecord(sc)
	}
	res := step.Run(ctx, text)
	ev.LatencyMS, ev.Dropped = res.Wall.Milliseconds(), res.Dropped
	if u := res.Usage; u != (refine.Usage{}) {
		ev.Usage = &store.RefineUsage{Input: u.Input, CacheWrite: u.CacheWrite, CacheRead: u.CacheRead, Output: u.Output}
	}
	for _, e := range res.Edits {
		ev.Edits = append(ev.Edits, store.RefineEdit{From: e.From, To: e.To, Why: e.Why})
	}
	if res.Err != nil {
		return fail(res.Why, res.Err)
	}
	if refine.OutputOf(r.Mode) == refine.FullText {
		ev.Text = res.Text // what the model wrote; the take's .txt stays as recognised
	}
	ev.At = time.Now()
	return res.Text, ev
}

// screenNow is what the call may carry of sc, captured as the take
// started, under the config as it is at the take's end (r): nothing once
// the screen source is switched off or the application is excluded.
func (r RefineConfig) screenNow(sc *refine.Screen) *refine.Screen {
	why := ""
	switch {
	case sc.Withheld != "":
		return sc
	case r.Context.Screen != refine.ScreenOCR && r.Context.Screen != refine.ScreenImage:
		why = refine.WithheldOff
	case slices.Contains(r.Context.Exclude, sc.App):
		why = refine.WithheldExcluded
	default:
		return sc
	}
	return &refine.Screen{Mode: sc.Mode, App: sc.App, Withheld: why}
}

// namesIn is what the call lists of the names scan on text: the spans the
// names gate would ask Jev about, all of them once one sounds the same as a
// listed form, and none otherwise or while names.on is off. The call is the
// take's one model call, so the model judges them in the gate's place.
func (r RefineConfig) namesIn(text string) string {
	if r.people == "" {
		return ""
	}
	ro, err := names.ReadRoster(r.people)
	if err != nil || ro.Empty() {
		return ""
	}
	cs := ro.Candidates(text)
	if !names.Fires(cs) {
		return ""
	}
	lines := make([]string, len(cs))
	for i, c := range cs {
		lines[i] = refine.NameLine(c.Span, c.Form, c.Person)
	}
	return strings.Join(lines, "\n")
}

// SourcesRecord is the text sources' parts as a refine line records them.
func SourcesRecord(parts []refine.Part) []store.ContextSource {
	var out []store.ContextSource
	for _, p := range parts {
		rec := store.ContextSource{Source: p.Source, Chars: utf8.RuneCountInString(p.Text), ReadUS: p.Took.Microseconds(), Withheld: p.Withheld}
		if p.Err != nil {
			rec.Err = p.Err.Error()
		}
		out = append(out, rec)
	}
	return out
}

// ScreenRecord is sc as a refine line records it.
func ScreenRecord(sc *refine.Screen) *store.Screen {
	rec := &store.Screen{Mode: sc.Mode, App: sc.App, W: sc.W, H: sc.H, Chars: utf8.RuneCountInString(sc.Text),
		CaptureMS: sc.Capture.Milliseconds(), RecognizeMS: sc.Recognize.Milliseconds(), EncodeMS: sc.Encode.Milliseconds(),
		Withheld: sc.Withheld}
	if sc.Err != nil {
		rec.Err = sc.Err.Error()
	}
	if im := sc.Image; im != nil {
		sum := sha256.Sum256(im.Data)
		rec.Bytes, rec.SHA256, rec.ImageW, rec.ImageH = len(im.Data), hex.EncodeToString(sum[:]), im.W, im.H
	}
	return rec
}

// Doubao is the Doubao ASR 2.0 client; its key is read from key_file at
// each take.
func (c Config) Doubao() *asr.Doubao {
	d := c.ASR.Doubao
	a := &asr.Doubao{URL: d.URL, ResourceID: d.ResourceID, Key: d.Key, TwoPass: d.TwoPass, DDC: d.DDC}
	if d.Hotwords {
		a.Hotwords = func() []string { return asr.ReadHotwordsMax(asr.DoubaoMaxHotwords, HotwordsFiles()...) }
	}
	return a
}

// Streamer is the streaming primary for a controller: Doubao when it is
// asr.engine, else nil (the chunk ASR is the primary).
func (c Config) Streamer() asr.Streamer {
	if c.ASR.Engine == "doubao" {
		return c.Doubao()
	}
	return nil
}

// TranscribeFile transcribes a kept take with tr as the controller would
// have: trimmed and as one window within take.whole_max, else whole file
// with the VAD.
func (c Config) TranscribeFile(ctx context.Context, tr asr.Transcriber, wav string) (string, error) {
	whole := false
	if s, err := audio.ReadWAV(wav); err == nil && session.Whole(len(s), time.Duration(c.Take.WholeMax)) {
		dir, err := os.MkdirTemp("", "megavoice-transcribe")
		if err != nil {
			return "", err
		}
		defer os.RemoveAll(dir)
		wav, whole = filepath.Join(dir, "trimmed.wav"), true
		if err := audio.SaveWAV(wav, audio.Trim(s)); err != nil {
			return "", err
		}
	}
	return tr.Transcribe(ctx, wav, whole)
}

// CompareEngines are the engines compare mode can run, built from c: funasr
// transcribes the take's WAV with local — the controller's own chunk ASR,
// so its resident process is shared — and doubao streams the take.
func (c Config) CompareEngines(local asr.Transcriber) map[string]compare.Engine {
	return map[string]compare.Engine{
		"funasr": {Name: "funasr", File: func(ctx context.Context, wav string) (string, error) { return c.TranscribeFile(ctx, local, wav) }},
		"doubao": {Name: "doubao", Stream: c.Doubao()},
	}
}

// Retranscribers are the engines a kept take can be decoded again by, for
// session.Controller.Retranscribe: funasr is the controller's own chunk ASR,
// taken through its queue, and doubao, a cloud engine, is Named only when
// this config runs it (Uses).
func (c Config) Retranscribers() []session.Engine {
	f := c.ASR.FunASR
	return []session.Engine{
		{Name: "funasr", Model: filepath.Base(asr.LLMPath(f.Root, f.LLM)), Named: true},
		{Name: "doubao", Model: c.ASR.Doubao.ResourceID, Cloud: true, Named: c.Uses("doubao"),
			File: func(ctx context.Context, wav string) (string, error) { return c.Doubao().Transcribe(ctx, wav, false) }},
	}
}

// Fanout is compare mode for a controller whose chunk ASR is local. Which
// engines run is read from the config file (o) as each take starts, so the
// menu's toggle and `config set compare.on` apply at the next take; how
// each engine runs is c's.
func (c Config) Fanout(o LoadOpts, local asr.Transcriber) *compare.Fanout {
	engines := c.CompareEngines(local)
	var mu sync.Mutex
	last := c.Compare
	return &compare.Fanout{
		Primary: c.ASR.Engine, Local: "funasr", Post: c.Post(),
		Engines: func() []compare.Engine {
			mu.Lock()
			defer mu.Unlock()
			o.Check = nil
			if l, err := Load(o); err != nil {
				log.Printf("compare: %v; keeping compare.on = %v", err, last.On)
			} else {
				last = l.Compare
			}
			if !last.On {
				return nil
			}
			var out []compare.Engine
			for _, name := range last.Engines {
				out = append(out, engines[name])
			}
			return out
		},
	}
}

// cloud names the service a cloud engine's audio goes to.
var cloud = map[string]l10n.Pair{"doubao": l10n.Pair{"火山引擎", "Volcengine"}}

// current is the config file (o) as it reads now, c when it no longer
// loads. Retranscriber and TakesEngines read it alike, so the page offers an
// engine exactly when the function accepts it.
func (c Config) current(o LoadOpts) Config {
	o.Check = nil
	if l, err := Load(o); err == nil {
		return l.Config
	}
	return c
}

// Retranscriber finds an engine by name among Retranscribers of the config
// file (o) as it reads at each call: a cloud engine is Named only while the
// config runs it.
func (c Config) Retranscriber(o LoadOpts) func(name string) (session.Engine, error) {
	return func(name string) (session.Engine, error) {
		var names []string
		for _, e := range c.current(o).Retranscribers() {
			if e.Name == name {
				return e, nil
			}
			names = append(names, e.Name)
		}
		return session.Engine{}, fmt.Errorf("engine %q: want one of %s", name, strings.Join(names, ", "))
	}
}

// TakesEngines lists the engines for the Takes page, from Retranscribers of
// the config file (o) read at each call, as Retranscriber reads it.
func (c Config) TakesEngines(o LoadOpts) func() []takes.Engine {
	return func() []takes.Engine {
		cur := c.current(o)
		var out []takes.Engine
		for _, e := range cur.Retranscribers() {
			out = append(out, takes.Engine{Name: e.Name, Local: !e.Cloud, Service: cloudName(e.Name),
				Primary: cur.ASR.Engine == e.Name, Named: !e.Cloud || e.Named})
		}
		return out
	}
}

// Labels is the label file, store.labels, which the Takes page reads and
// writes.
func (c Config) Labels() *compare.Labels { return &compare.Labels{Path: c.Store.Labels} }

// cloudName is the service a cloud engine's audio goes to, in the user's
// language; "" for a local engine.
func cloudName(engine string) string {
	c, ok := cloud[engine]
	if !ok {
		return ""
	}
	return c.String()
}
