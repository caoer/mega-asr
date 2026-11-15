package app

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
	"time"

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
		if run := stage.Take(); run != nil {
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
	run := stage.Take()
	if run == nil {
		t.Fatal("no step while refine.on is true")
	}
	text, ev := run(context.Background(), session.RefineInput{Text: "用海库跑一下"})
	if text != "用Haiku跑一下" || ev == nil || ev.Ev != "refine" || ev.Why != "" || ev.Err != "" || ev.Dropped != 1 ||
		len(ev.Edits) != 1 || ev.Edits[0].To != "Haiku" || ev.Edits[0].Why != "模型名" ||
		ev.Mode != "fix-words" || ev.Provider != "gateway" || ev.Model != "claude-haiku-5-5" || ev.Effort != "low" ||
		ev.Usage == nil || ev.Usage.Input != 900 {
		t.Fatalf("text %q, refine line %+v", text, ev)
	}
	if err := Set(path, "refine.on", "false", nil); err != nil {
		t.Fatal(err)
	}
	if run := stage.Take(); run != nil {
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
	writeConfig(t, dir, "[refine]\nmode = \"rewrite\"\n")
	takes()
	if n := strings.Count(logged.String(), "keeping refine.on = false"); n != 1 {
		t.Fatalf("a file that does not load is reported %d times in 3 takes, want once:\n%s", n, logged.String())
	}
	refineConfig(t, dir, p.URL, "")
	if stage.Take() == nil {
		t.Fatalf("the step is off after %s switched it on", path)
	}
}

// A slow call, a failed one, a refusal, an answer that is not an edit
// list, an edit past the guards and an unreadable key each deliver the
// text as it was, and the refine line says why.
func TestRefineKeepsTheTextAndSaysWhy(t *testing.T) {
	const text = "用海库跑一下"
	status := func(code int, body string) func(http.ResponseWriter, map[string]any) {
		return func(w http.ResponseWriter, _ map[string]any) { w.WriteHeader(code); io.WriteString(w, body) }
	}
	for _, tc := range []struct {
		why, more string
		answer    func(http.ResponseWriter, map[string]any)
		err       string
	}{
		{"timeout", "timeout = \"50ms\"\n", func(http.ResponseWriter, map[string]any) { time.Sleep(300 * time.Millisecond) }, "no answer within 50ms"},
		{"http", "", status(529, `{"type":"error","error":{"type":"overloaded_error"}}`), "status 529"},
		{"refusal", "", status(200, `{"content":[],"stop_reason":"refusal","usage":{"input_tokens":9}}`), "declined"},
		{"parse", "", status(200, `{"content":[{"type":"text","text":"好的，我来跑。"}],"stop_reason":"end_turn"}`), "not an edit list"},
		{"guard", "", edits(`[{"from":"海库","why":"","to":"Haiku 是 Anthropic 的小模型，适合做这种校对工作"}]`), "in place of"},
		{"key", "", edits(`[]`), "model.key"},
	} {
		p := newProvider(t)
		p.answer = tc.answer
		dir := isolate(t)
		refineConfig(t, dir, p.URL, tc.more)
		if tc.why == "key" {
			os.Remove(filepath.Join(dir, "model.key"))
		}
		l, err := Load(LoadOpts{})
		if err != nil {
			t.Fatal(err)
		}
		got, ev := l.RefineStage(LoadOpts{}).Take()(context.Background(), session.RefineInput{Text: text})
		if got != text || ev == nil || ev.Ev != "refine" || ev.Why != tc.why || !strings.Contains(ev.Err, tc.err) || ev.Edits != nil {
			t.Errorf("%s: text %q, refine line %+v", tc.why, got, ev)
		}
		if tc.why == "key" && p.requests.Load() != 0 {
			t.Errorf("a call went out without a key")
		}
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
timeout = "6s"
wait = true
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
	want := RefineConfig{On: true, Mode: "fix-words", Provider: "gateway", Model: "claude-sonnet-5-5", Effort: "off",
		Timeout: Duration(6 * time.Second), Wait: true, AgentCopy: "both",
		Context: RefineContextConfig{App: true, Field: true, Clipboard: true, Screen: "ocr"},
		Providers: map[string]RefineProviderConfig{
			"gateway": {Kind: "anthropic", BaseURL: "https://gateway.example/anthropic", KeyFile: filepath.Join(home, "keys/model.key")},
			"other":   {Kind: "openai", BaseURL: "https://api.example/v1", KeyFile: "/keys/other.key"},
		}}
	if !reflect.DeepEqual(l.Refine, want) {
		t.Fatalf("got  %+v\nwant %+v", l.Refine, want)
	}
	// a set timeout is the limit for every take
	if got := l.Refine.Limit(&store.Target{Kind: "herdr"}); got != 6*time.Second {
		t.Fatalf("limit %s with timeout = 6s", got)
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
		{"[refine]\nmode = \"rewrite\"\n", "refine.mode: \"rewrite\" is not one of fix-words"},
		{"[refine]\neffort = \"max\"\n", "refine.effort"},
		{"[refine]\ntimeout = \"-1s\"\n", "refine.timeout"},
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

// With timeout unset the limit follows who waits: 20 s for a take toward
// an agent pane or a Telegram chat, 10 s for a take held for the answer —
// any other destination, and every take while wait is on.
func TestRefineLimitFollowsWait(t *testing.T) {
	r := Default().Refine
	for _, tc := range []struct {
		target *store.Target
		wait   bool
		want   time.Duration
	}{
		{&store.Target{Kind: "herdr"}, false, 20 * time.Second},
		{&store.Target{Kind: "telegram"}, false, 20 * time.Second},
		{&store.Target{Kind: "app"}, false, 10 * time.Second},
		{&store.Target{Kind: "clipboard"}, false, 10 * time.Second},
		{nil, false, 10 * time.Second},
		{&store.Target{Kind: "herdr"}, true, 10 * time.Second},
	} {
		r.Wait = tc.wait
		if got := r.Limit(tc.target); got != tc.want {
			t.Errorf("target %+v, wait %v: %s, want %s", tc.target, tc.wait, got, tc.want)
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

// Toward an agent's target — a herdr pane with an agent in it, the keypad's
// Telegram chat — agent_copy chooses what is delivered: refined, the
// corrected text; both, the corrected text with the marked changes; raw, the
// text as recognised. Every other target gets the corrected text under each
// value. The refine line keeps the edits whatever was delivered.
func TestAgentCopy(t *testing.T) {
	const heard, fixed = "用海库跑一下", "用Haiku跑一下"
	both := fixed + "\n\n" + refine.MarkEdits + " 海库 → Haiku"
	pane := &store.Target{Kind: "herdr", Pane: "w1:p2", Agent: "claude"}
	chat := &store.Target{Kind: "telegram", App: "报到群"}
	others := []*store.Target{{Kind: "herdr", Pane: "w1:p1"}, {Kind: "app", App: "Notes"}, {Kind: "clipboard"}, nil}
	p := newProvider(t)
	p.answer = edits(`[{"from":"海库","why":"模型名","to":"Haiku"}]`)
	for _, tc := range []struct{ copy, agent, line string }{
		{"refined", fixed, ""},
		{"both", both, "both"},
		{"raw", heard, "raw"},
	} {
		dir := isolate(t)
		refineConfig(t, dir, p.URL, "agent_copy = \""+tc.copy+"\"\n")
		l, err := Load(LoadOpts{})
		if err != nil {
			t.Fatal(err)
		}
		run := l.RefineStage(LoadOpts{}).Take()
		check := func(target *store.Target, want, line string) {
			got, ev := run(context.Background(), session.RefineInput{Text: heard, Target: target})
			if got != want || ev == nil || ev.Why != "" || ev.Copy != line || len(ev.Edits) != 1 || ev.Edits[0].To != "Haiku" {
				t.Errorf("%s toward %+v: delivered %q, want %q; refine line %+v", tc.copy, target, got, want, ev)
			}
		}
		check(pane, tc.agent, tc.line)
		check(chat, tc.agent, tc.line)
		for _, o := range others {
			check(o, fixed, "")
		}
	}
}

// both adds nothing when the step changed nothing: the agent gets the text
// as it came, once.
func TestAgentCopyBothUnchanged(t *testing.T) {
	const heard = "跑一下 Haiku"
	p := newProvider(t)
	p.answer = edits(`[]`)
	dir := isolate(t)
	refineConfig(t, dir, p.URL, "agent_copy = \"both\"\n")
	l, err := Load(LoadOpts{})
	if err != nil {
		t.Fatal(err)
	}
	for _, target := range []*store.Target{{Kind: "herdr", Pane: "w1:p2", Agent: "codex"}, {Kind: "telegram"}} {
		got, ev := l.RefineStage(LoadOpts{}).Take()(context.Background(), session.RefineInput{Text: heard, Target: target})
		if got != heard || ev == nil || ev.Why != "" || ev.Copy != "" || len(ev.Edits) != 0 {
			t.Errorf("toward %+v: delivered %q, refine line %+v", target, got, ev)
		}
	}
}
