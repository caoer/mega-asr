package app

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"io/fs"
	"log"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/caoer/mega-asr/internal/audio"
	"github.com/caoer/mega-asr/internal/refine"
	"github.com/caoer/mega-asr/internal/session"
	"github.com/caoer/mega-asr/internal/store"
)

// provider is a fake model provider: a Messages API endpoint that answers
// every call as answer says and counts the requests it gets, calls or not.
type provider struct {
	*httptest.Server
	requests atomic.Int32
	answer   func(w http.ResponseWriter, body map[string]any)
}

func newProvider(t *testing.T) *provider {
	p := &provider{}
	p.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		p.requests.Add(1)
		if r.Method != http.MethodPost {
			return
		}
		var body map[string]any
		b, _ := io.ReadAll(r.Body)
		json.Unmarshal(b, &body)
		p.answer(w, body)
	}))
	t.Cleanup(p.Close)
	return p
}

// edits answers with an edit list.
func edits(list string) func(http.ResponseWriter, map[string]any) {
	return func(w http.ResponseWriter, _ map[string]any) {
		b, _ := json.Marshal(map[string]any{
			"content":     []map[string]string{{"type": "text", "text": `{"edits":` + list + `}`}},
			"stop_reason": "end_turn",
			"usage":       map[string]int{"input_tokens": 900, "output_tokens": 40},
		})
		w.Write(b)
	}
}

// refineConfig is a config file with the step on, its provider at url and
// the key in a file beside it; more is appended to the [refine] table.
func refineConfig(t *testing.T, dir, url, more string) string {
	t.Helper()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "model.key"), []byte("vk-test\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	return writeConfig(t, dir, "[refine]\non = true\nprovider = \"gateway\"\n"+more+
		"\n[refine.providers.gateway]\nkind = \"anthropic\"\nbase_url = \""+url+"\"\nkey_file = \""+filepath.Join(dir, "model.key")+"\"\n")
}

// With no [refine] table, or with on = false, a take gets no step and its
// start opens nothing: the provider hears nothing at all.
func TestRefineOffMakesNoCall(t *testing.T) {
	p := newProvider(t)
	for _, body := range []string{
		"",
		"[refine]\non = false\nprovider = \"gateway\"\n[refine.providers.gateway]\nkind = \"anthropic\"\nbase_url = \"" + p.URL + "\"\nkey_file = \"/nonexistent\"\n",
	} {
		dir := isolate(t)
		writeConfig(t, dir, body)
		l, err := Load(LoadOpts{})
		if err != nil {
			t.Fatal(err)
		}
		stage := l.RefineStage(LoadOpts{})
		stage.Warm()
		if step := stage.Take(); step != nil {
			t.Errorf("%q: the step runs", body)
		}
	}
	if n := p.requests.Load(); n != 0 {
		t.Fatalf("the provider got %d requests", n)
	}
}

// The step applies the model's edits and records them; a change to the
// config file is read at the next take, so switching it off applies to it.
func TestRefineStageReadsTheFileAtEachTake(t *testing.T) {
	p := newProvider(t)
	p.answer = edits(`[{"from":"海库","why":"模型名","to":"Haiku"},{"from":"康胖","why":"","to":"compound"}]`)
	dir := isolate(t)
	path := refineConfig(t, dir, p.URL, "")
	l, err := Load(LoadOpts{})
	if err != nil {
		t.Fatal(err)
	}
	stage := l.RefineStage(LoadOpts{})
	stage.Warm()
	step := stage.Take()
	if step == nil {
		t.Fatal("no step while refine.on is true")
	}
	text, ev := step.Run(context.Background(), session.RefineInput{Text: "用海库跑一下"})
	if text != "用Haiku跑一下" || ev == nil || ev.Ev != "refine" || ev.Why != "" || ev.Err != "" || ev.Dropped != 1 ||
		len(ev.Edits) != 1 || ev.Edits[0].To != "Haiku" || ev.Edits[0].Why != "模型名" ||
		ev.Mode != "fix-words" || ev.Provider != "gateway" || ev.Model != "claude-haiku-5-5" || ev.Effort != "low" ||
		ev.Usage == nil || ev.Usage.Input != 900 {
		t.Fatalf("text %q, refine line %+v", text, ev)
	}
	if err := Set(path, "refine.on", "false", nil); err != nil {
		t.Fatal(err)
	}
	if step := stage.Take(); step != nil {
		t.Fatal("the step still runs after refine.on = false")
	}
}

// While the config file stays as it is a take does not read it: with the
// step off, takes after the first read nothing, a file that no longer
// loads is reported once and not at each take's start and end, and the
// file's next version is read at the next take.
func TestRefineStageReadsTheFileOnlyWhenItChanged(t *testing.T) {
	p := newProvider(t)
	dir := isolate(t)
	t.Setenv("MEGAVOICE_MEGA_ASR", "/legacy") // each read of the config logs that it is ignored
	path := writeConfig(t, dir, "[refine]\non = false\n")
	l, err := Load(LoadOpts{})
	if err != nil {
		t.Fatal(err)
	}
	stage := l.RefineStage(LoadOpts{})
	var logged bytes.Buffer
	log.SetOutput(&logged)
	defer log.SetOutput(os.Stderr)
	takes := func() {
		for range 3 {
			stage.Warm()
			if stage.Take() != nil {
				t.Fatal("the step runs")
			}
		}
	}
	takes()
	if n := strings.Count(logged.String(), "MEGAVOICE_MEGA_ASR is set and ignored"); n != 1 {
		t.Fatalf("3 takes with the step off read the config %d times, want the first take's once:\n%s", n, logged.String())
	}
	logged.Reset()
	writeConfig(t, dir, "[refine]\nmode = \"../rewrite\"\n")
	takes()
	if n := strings.Count(logged.String(), "keeping refine.on = false"); n != 1 {
		t.Fatalf("a file that does not load is reported %d times in 3 takes, want once:\n%s", n, logged.String())
	}
	refineConfig(t, dir, p.URL, "")
	if stage.Take() == nil {
		t.Fatalf("the step is off after %s switched it on", path)
	}
}

// A failed call, a refusal, an answer that is not an edit list, an edit
// past the guards and an unreadable key each give the text back as it was
// with a refine line that says why: the controller holds such a take.
func TestRefineSaysWhyThereIsNoAnswer(t *testing.T) {
	const text = "用海库跑一下"
	status := func(code int, body string) func(http.ResponseWriter, map[string]any) {
		return func(w http.ResponseWriter, _ map[string]any) { w.WriteHeader(code); io.WriteString(w, body) }
	}
	for _, tc := range []struct {
		why    string
		answer func(http.ResponseWriter, map[string]any)
		err    string
	}{
		{"http", status(529, `{"type":"error","error":{"type":"overloaded_error"}}`), "status 529"},
		{"refusal", status(200, `{"content":[],"stop_reason":"refusal","usage":{"input_tokens":9}}`), "declined"},
		{"parse", status(200, `{"content":[{"type":"text","text":"好的，我来跑。"}],"stop_reason":"end_turn"}`), "not an edit list"},
		{"guard", edits(`[{"from":"海库","why":"","to":"Haiku 是 Anthropic 的小模型，适合做这种校对工作"}]`), "in place of"},
		{"key", edits(`[]`), "model.key"},
	} {
		p := newProvider(t)
		p.answer = tc.answer
		dir := isolate(t)
		refineConfig(t, dir, p.URL, "")
		if tc.why == "key" {
			os.Remove(filepath.Join(dir, "model.key"))
		}
		l, err := Load(LoadOpts{})
		if err != nil {
			t.Fatal(err)
		}
		got, ev := l.RefineStage(LoadOpts{}).Take().Run(context.Background(), session.RefineInput{Text: text})
		if got != text || ev == nil || ev.Ev != "refine" || ev.Why != tc.why || !strings.Contains(ev.Err, tc.err) || ev.Edits != nil {
			t.Errorf("%s: text %q, refine line %+v", tc.why, got, ev)
		}
		if tc.why == "key" && p.requests.Load() != 0 {
			t.Errorf("a call went out without a key")
		}
	}
}

// The step waits for a provider that does not answer: past the time any
// limit would have set, the call is still under way, and it ends when the
// take's context is cancelled, with a refine line that says so.
func TestRefineWaitsForTheProvider(t *testing.T) {
	p := newProvider(t)
	release := make(chan struct{})
	p.answer = func(http.ResponseWriter, map[string]any) { <-release }
	defer close(release)
	dir := isolate(t)
	refineConfig(t, dir, p.URL, "")
	l, err := Load(LoadOpts{})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan *store.Event, 1)
	go func() {
		_, ev := l.RefineStage(LoadOpts{}).Take().Run(ctx, session.RefineInput{Text: "用海库跑一下"})
		done <- ev
	}()
	select {
	case ev := <-done:
		t.Fatalf("the call ended on its own: %+v", ev)
	case <-time.After(500 * time.Millisecond):
	}
	cancel()
	if ev := <-done; ev == nil || ev.Why != refine.WhyCancelled || ev.LatencyMS < 500 || p.requests.Load() != 1 {
		t.Fatalf("cancelled: refine line %+v, %d requests", ev, p.requests.Load())
	}
}

// The whole [refine] table loads, the keys a later build acts on
// included; a key the table does not have is refused by name, as in any
// other table.
func TestRefineConfigKeys(t *testing.T) {
	dir := isolate(t)
	writeConfig(t, dir, `
[refine]
on = true
mode = "fix-words"
provider = "gateway"
model = "claude-sonnet-5-5"
effort = "off"
agent_copy = "both"

[refine.context]
app = true
field = true
clipboard = true
screen = "ocr"

[refine.providers.gateway]
kind = "anthropic"
base_url = "https://gateway.example/anthropic"
key_file = "~/keys/model.key"

[refine.providers.other]
kind = "openai"
base_url = "https://api.example/v1"
key_file = "/keys/other.key"
`)
	l, err := Load(LoadOpts{})
	if err != nil {
		t.Fatal(err)
	}
	home, _ := os.UserHomeDir()
	want := RefineConfig{On: true, Mode: "fix-words", Provider: "gateway", Model: "claude-sonnet-5-5", Effort: "off", AgentCopy: "both",
		Context: RefineContextConfig{App: true, Field: true, Clipboard: true, Screen: "ocr", Exclude: ScreenExclude},
		Providers: map[string]RefineProviderConfig{
			"gateway": {Kind: "anthropic", BaseURL: "https://gateway.example/anthropic", KeyFile: filepath.Join(home, "keys/model.key")},
			"other":   {Kind: "openai", BaseURL: "https://api.example/v1", KeyFile: "/keys/other.key"},
		}}
	if !reflect.DeepEqual(l.Refine, want) {
		t.Fatalf("got  %+v\nwant %+v", l.Refine, want)
	}
	// the effective config prints each provider as its own table and loads back
	var out strings.Builder
	l.WriteEffective(&out, "megavoice", VoiceTables...)
	if !strings.Contains(out.String(), "\n[refine.providers.gateway]\n") || !strings.Contains(out.String(), "\n[refine.context]\n") {
		t.Fatalf("effective config:\n%s", out.String())
	}
	back, err := Load(LoadOpts{Path: writeConfig(t, t.TempDir(), out.String())})
	if err != nil || !reflect.DeepEqual(back.Refine, l.Refine) {
		t.Fatalf("reloaded %+v, %v", back.Refine, err)
	}

	for _, tc := range []struct{ body, want string }{
		{"[refine]\ntemperature = 0.2\n", "unknown key refine.temperature"},
		{"[refine.context]\nbrowser = true\n", "unknown key refine.context.browser"},
		{"[refine.providers.gateway]\nkind = \"anthropic\"\nbase_url = \"https://g.example\"\nkey_file = \"/k\"\nkey = \"sk\"\n", "unknown key refine.providers.gateway.key"},
		{"[refine]\non = true\n", "refine.provider: \"\" is not a [refine.providers.<name>] table"},
		{"[refine]\non = true\nprovider = \"o\"\n[refine.providers.o]\nkind = \"openai\"\nbase_url = \"https://o.example/v1\"\nkey_file = \"/k\"\n", "refine.provider: o is kind openai"},
		{"[refine]\nmode = \"../rewrite\"\n", "refine.mode: \"../rewrite\" is not a mode"},
		{"[refine]\nmode = \"\"\n", "refine.mode: \"\" is not a mode"},
		{"[refine]\neffort = \"max\"\n", "refine.effort"},
		// the step has no time limit to set: a file that names one is refused
		{"[refine]\ntimeout = \"6s\"\n", "unknown key refine.timeout"},
		{"[refine]\nwait = true\n", "unknown key refine.wait"},
		{"[refine]\nagent_copy = \"none\"\n", "refine.agent_copy"},
		{"[refine.context]\nscreen = \"on\"\n", "refine.context.screen"},
		{"[refine.providers.g]\nkind = \"gemini\"\nbase_url = \"https://g.example\"\nkey_file = \"/k\"\n", "refine.providers.g.kind"},
		{"[refine.providers.g]\nkind = \"anthropic\"\nbase_url = \"g.example\"\nkey_file = \"/k\"\n", "refine.providers.g.base_url"},
		{"[refine.providers.g]\nkind = \"anthropic\"\nbase_url = \"https://g.example\"\n", "refine.providers.g.key_file"},
	} {
		dir := isolate(t)
		writeConfig(t, dir, tc.body)
		if _, err := Load(LoadOpts{}); err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%q: got %v, want an error with %q", tc.body, err, tc.want)
		}
	}
}

// The mode's prompt is the built-in, or modes.d/<mode>.md beside the
// config once the user keeps one; it is what the call's system block says.
func TestRefinePromptFile(t *testing.T) {
	p := newProvider(t)
	var system string
	p.answer = func(w http.ResponseWriter, body map[string]any) {
		system = body["system"].([]any)[0].(map[string]any)["text"].(string)
		edits(`[]`)(w, body)
	}
	dir := isolate(t)
	refineConfig(t, dir, p.URL, "")
	l, err := Load(LoadOpts{})
	if err != nil {
		t.Fatal(err)
	}
	if _, ev := l.Refine.Run(context.Background(), "好", nil); ev.Why != "" || !strings.Contains(system, "<dictation>") || !strings.Contains(system, `{"edits":[`) {
		t.Fatalf("built-in prompt: refine line %+v, system %q", ev, system)
	}
	os.MkdirAll(filepath.Join(dir, "modes.d"), 0o755)
	if err := os.WriteFile(filepath.Join(dir, "modes.d", "fix-words.md"), []byte("my prompt\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if l.Refine.Run(context.Background(), "好", nil); system != "my prompt" {
		t.Fatalf("system %q, want the user's file", system)
	}
}

// The stage gives the take's text — the corrected text, never a marked
// second copy — for every target under every agent_copy, and hands the
// controller agent_copy on the refine line, which delivers by it. A take's
// text is what a resend and the takes list read later, wherever they paste.
func TestStageGivesTheTakeText(t *testing.T) {
	const heard, fixed = "用海库跑一下", "用Haiku跑一下"
	p := newProvider(t)
	p.answer = edits(`[{"from":"海库","why":"模型名","to":"Haiku"}]`)
	targets := []*store.Target{{Kind: "herdr", Pane: "w1:p2", Agent: "claude"}, {Kind: "telegram"},
		{Kind: "herdr", Pane: "w1:p1"}, {Kind: "app", App: "Notes"}, {Kind: "clipboard"}, nil}
	for _, copy := range []string{"refined", "both", "raw"} {
		dir := isolate(t)
		refineConfig(t, dir, p.URL, "agent_copy = \""+copy+"\"\n")
		l, err := Load(LoadOpts{})
		if err != nil {
			t.Fatal(err)
		}
		step := l.RefineStage(LoadOpts{}).Take()
		for _, target := range targets {
			got, ev := step.Run(context.Background(), session.RefineInput{Text: heard, Target: target})
			if got != fixed || ev == nil || ev.Why != "" || ev.Copy != copy || len(ev.Edits) != 1 || ev.Edits[0].To != "Haiku" {
				t.Errorf("%s toward %+v: text %q, want %q; refine line %+v", copy, target, got, fixed, ev)
			}
			// only a take that gets the text as recognised goes out before the answer
			if aside := step.Aside(target); aside != (copy == "raw" && target.IsAgent()) {
				t.Errorf("%s toward %+v: aside %v", copy, target, aside)
			}
		}
	}
}

// A capture notes the pane's agent only while the step is on with
// agent_copy both or raw; off, or refined, it asks herdr nothing more.
func TestRefineNotesAgent(t *testing.T) {
	for _, tc := range []struct {
		on   bool
		copy string
		want bool
	}{
		{false, "", false},
		{false, "both", false},
		{true, "refined", false},
		{true, "both", true},
		{true, "raw", true},
	} {
		dir := isolate(t)
		switch {
		case tc.on:
			refineConfig(t, dir, "http://127.0.0.1:9", "agent_copy = \""+tc.copy+"\"\n")
		case tc.copy != "":
			writeConfig(t, dir, "[refine]\nagent_copy = \""+tc.copy+"\"\n")
		default:
			writeConfig(t, dir, "")
		}
		l, err := Load(LoadOpts{})
		if err != nil {
			t.Fatal(err)
		}
		if got := l.RefineStage(LoadOpts{}).NotesAgent(); got != tc.want {
			t.Errorf("on %v, agent_copy %q: notes the agent %v, want %v", tc.on, tc.copy, got, tc.want)
		}
	}
}

// A take through a controller built from the config, with the names gate
// and the model step both configured, on a text the gate fires on.

// heardSource is a second of sound, then the stream's end at Stop.
type heardSource struct {
	ch   chan []int16
	once sync.Once
}

func (s *heardSource) Start(context.Context) (<-chan []int16, error) {
	b := make([]int16, 16000)
	for i := range b {
		b[i] = int16((i%64 - 32) * 100)
	}
	s.ch = make(chan []int16, 1)
	s.ch <- b
	return s.ch, nil
}
func (s *heardSource) Stop()      { s.once.Do(func() { close(s.ch) }) }
func (s *heardSource) Err() error { return nil }

// said is an ASR that hears the same sentence in every take.
type said string

func (s said) Transcribe(context.Context, string, bool) (string, error) { return string(s), nil }

// pasted keeps what a controller delivers.
type pasted struct {
	mu   sync.Mutex
	text []string
}

type cursor struct{}

func (cursor) String() string { return "the cursor" }

func (p *pasted) Capture() session.Target { return cursor{} }
func (p *pasted) Deliver(_ session.Target, text string) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.text = append(p.text, text)
	return nil
}
func (p *pasted) Submit(session.Target) error { return nil }

type noUI struct{}

func (noUI) Show(session.View) {}
func (noUI) Flash(string)      {}
func (noUI) Alert(string)      {}

// One model call per dictation: with both stages configured the take makes
// one request, the provider's, and none to Jev; with the step off the gate
// asks Jev as before.
func TestOneModelCallPerTake(t *testing.T) {
	var jev atomic.Int32
	typesafe := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		jev.Add(1)
		io.WriteString(w, `{"answers":{"q0":{"type":"choice","choice":"林晓雨: 林晓雨","probabilities":{"林晓雨: 林晓雨":0.93}}}}`)
	}))
	defer typesafe.Close()
	defer func(u string) { namesURL = u }(namesURL)
	namesURL = typesafe.URL
	p := newProvider(t)
	p.answer = edits(`[{"from":"海库","why":"模型名","to":"Haiku"}]`)
	for _, tc := range []struct {
		on        bool
		jev, step int32
		delivered string
	}{
		{true, 0, 1, "让林小雨用Haiku跑一下"},
		{false, 1, 0, "让林晓雨用海库跑一下"},
	} {
		jev.Store(0)
		p.requests.Store(0)
		dir := isolate(t)
		for name, body := range map[string]string{
			"people.json":  `{"people":[{"slug":"linxiaoyu","name":"林晓雨"}]}`,
			"typesafe.key": "ts-test\n",
			"model.key":    "vk-test\n",
		} {
			os.MkdirAll(dir, 0o755)
			if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o600); err != nil {
				t.Fatal(err)
			}
		}
		on := "false"
		if tc.on {
			on = "true"
		}
		writeConfig(t, dir, "[names]\non = true\npeople = \""+filepath.Join(dir, "people.json")+"\"\nkey_file = \""+filepath.Join(dir, "typesafe.key")+"\"\n"+
			"[refine]\non = "+on+"\nprovider = \"gateway\"\n[refine.providers.gateway]\nkind = \"anthropic\"\nbase_url = \""+p.URL+
			"\"\nkey_file = \""+filepath.Join(dir, "model.key")+"\"\n")
		l, err := Load(LoadOpts{})
		if err != nil {
			t.Fatal(err)
		}
		out := &pasted{}
		c := &session.Controller{
			NewSource: func() audio.Source { return &heardSource{} },
			ASR:       said("让林小雨用海库跑一下"),
			Names:     l.NamesStage(),
			Refine:    l.RefineStage(LoadOpts{}).Take,
			Deliver:   out,
			UI:        noUI{},
			Store:     store.Store{Dir: t.TempDir()},
			ChunkDir:  t.TempDir(),
		}
		c.Toggle()
		c.Toggle()
		c.Wait()
		if got := strings.Join(out.text, ";"); jev.Load() != tc.jev || p.requests.Load() != tc.step || got != tc.delivered {
			t.Errorf("refine.on = %v: %d Jev requests, %d provider requests, delivered %q; want %d, %d, %q",
				tc.on, jev.Load(), p.requests.Load(), got, tc.jev, tc.step, tc.delivered)
		}
	}
}

// flashes keeps what the overlay flashes.
type flashes struct {
	noUI
	mu  sync.Mutex
	got []string
}

func (f *flashes) Flash(msg string) { f.mu.Lock(); f.got = append(f.got, msg); f.mu.Unlock() }

// A prompt file dropped in modes.d is a text mode: a take in it sends the
// file, asks for the whole text, and delivers what the model wrote however
// much it reorganised; the record keeps the recognised text, the model's
// text and the mode, and the overlay says the model wrote it.
func TestATextModeFromAPromptFile(t *testing.T) {
	const heard, wrote = "就是那个上线的事情我觉得明天早上评审完了以后吧可以发", "评审结束后，明天上午发版。"
	p := newProvider(t)
	var system, schema string
	p.answer = func(w http.ResponseWriter, body map[string]any) {
		system = body["system"].([]any)[0].(map[string]any)["text"].(string)
		b, _ := json.Marshal(body["output_config"])
		schema = string(b)
		out, _ := json.Marshal(map[string]any{
			"content":     []map[string]string{{"type": "text", "text": `{"text":"` + wrote + `"}`}},
			"stop_reason": "end_turn",
			"usage":       map[string]int{"input_tokens": 900, "output_tokens": 20},
		})
		w.Write(out)
	}
	dir := isolate(t)
	refineConfig(t, dir, p.URL, "mode = \"meeting-notes\"\n")
	os.MkdirAll(filepath.Join(dir, "modes.d"), 0o755)
	if err := os.WriteFile(filepath.Join(dir, "modes.d", "meeting-notes.md"), []byte("写成会议纪要里的一句\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	l, err := Load(LoadOpts{})
	if err != nil {
		t.Fatal(err)
	}
	out, ui, takes := &pasted{}, &flashes{}, t.TempDir()
	c := &session.Controller{
		NewSource: func() audio.Source { return &heardSource{} },
		ASR:       said(heard),
		Refine:    l.RefineStage(LoadOpts{}).Take,
		Deliver:   out,
		UI:        ui,
		Store:     store.Store{Dir: takes},
		ChunkDir:  t.TempDir(),
	}
	c.Toggle()
	c.Toggle()
	c.Wait()
	if got := strings.Join(out.text, ";"); got != wrote || system != "写成会议纪要里的一句" || !strings.Contains(schema, `"required":["text"]`) {
		t.Fatalf("delivered %q, system %q, format %s; want the model's text, the file and the text schema", got, system, schema)
	}
	if !slices.Contains(ui.got, "模型写 · meeting-notes") {
		t.Errorf("flashes %q, want the model wrote it", ui.got)
	}
	var record []string
	filepath.WalkDir(takes, func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() || strings.HasSuffix(path, ".wav") {
			return err
		}
		b, _ := os.ReadFile(path)
		record = append(record, filepath.Base(path)+": "+strings.TrimSpace(string(b)))
		return nil
	})
	all := strings.Join(record, "\n")
	t.Logf("the take's record:\n%s", all)
	var refineLine store.Event
	for _, line := range strings.Split(all, "\n") {
		if strings.Contains(line, `"ev":"refine"`) {
			json.Unmarshal([]byte(line[strings.Index(line, "{"):]), &refineLine)
		}
	}
	if !strings.Contains(all, ".txt: "+heard) || refineLine.Mode != "meeting-notes" || refineLine.Text != wrote || refineLine.Why != "" {
		t.Errorf("the record lacks the recognised text in .txt, or the refine line the mode and the model's text:\n%s", all)
	}
	// a mode whose file is gone loads, and its take is held naming the file
	os.Remove(filepath.Join(dir, "modes.d", "meeting-notes.md"))
	if l, err = Load(LoadOpts{}); err != nil {
		t.Fatal(err)
	}
	if text, ev := l.Refine.Run(context.Background(), heard, nil); text != heard || ev.Why != refine.WhyError || !strings.Contains(ev.Err, filepath.Join("modes.d", "meeting-notes.md")) {
		t.Errorf("with the file gone: text %q, refine line %+v; want the take held naming the file", text, ev)
	}
}

// A run again from the takes page is one call in the mode it names, made
// with the step off as well: the provider gets one request, its system
// prompt is that mode's, and the line names the mode. A mode refine.mode
// cannot name is refused before any request.
func TestStepInMakesOneRequestInThatMode(t *testing.T) {
	p := newProvider(t)
	var system string
	p.answer = func(w http.ResponseWriter, body map[string]any) {
		b, _ := json.Marshal(body["system"])
		system = string(b)
		b, _ = json.Marshal(map[string]any{
			"content":     []map[string]string{{"type": "text", "text": `{"text":"用海库跑一下 Python。"}`}},
			"stop_reason": "end_turn",
		})
		w.Write(b)
	}
	dir := isolate(t)
	path := refineConfig(t, dir, p.URL, "mode = \"fix-words\"\n")
	b, _ := os.ReadFile(path)
	if err := os.WriteFile(path, []byte(strings.Replace(string(b), "on = true", "on = false", 1)), 0o644); err != nil {
		t.Fatal(err)
	}
	l, err := Load(LoadOpts{})
	if err != nil {
		t.Fatal(err)
	}
	stage := l.RefineStage(LoadOpts{})
	if _, err := stage.StepIn("poem"); err == nil || p.requests.Load() != 0 {
		t.Fatalf("an unknown mode: %v, %d requests", err, p.requests.Load())
	}
	step, err := stage.StepIn("tidy")
	if err != nil {
		t.Fatal(err)
	}
	got, ev := step.Run(context.Background(), session.RefineInput{Text: "用海库跑一下 Python"})
	prompt, err := RefineConfig{Mode: "tidy"}.Prompt()
	if err != nil {
		t.Fatal(err)
	}
	want, _ := json.Marshal(prompt)
	if n := p.requests.Load(); n != 1 || ev == nil || ev.Mode != "tidy" || ev.Why != "" || got != "用海库跑一下 Python。" ||
		!strings.Contains(system, strings.Trim(string(want), `"`)[:40]) || ev.Copy != "" {
		t.Fatalf("%d requests, text %q, line %+v, system %.120s", n, got, ev, system)
	}
}
