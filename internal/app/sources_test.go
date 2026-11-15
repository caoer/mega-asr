package app

import (
	"context"
	"encoding/json"
	"net/http"
	"os"
	"strings"
	"testing"

	"github.com/caoer/mega-asr/internal/refine"
	"github.com/caoer/mega-asr/internal/session"
	"github.com/caoer/mega-asr/internal/store"
)

// field is a locked target as the text sources read it: a session.Target
// with a record and a field.
type field struct {
	rec    store.Target
	text   string
	secure bool
	reads  int
}

func (f *field) String() string       { return f.rec.App }
func (f *field) Record() store.Target { return f.rec }
func (f *field) Secure() bool         { return f.secure }
func (f *field) FieldText(context.Context) (string, int, bool) {
	f.reads++
	return f.text, -1, true
}
func (f *field) PaneText(context.Context, int) (string, bool, error) { return "", false, nil }

// The take's refine line holds exactly what the call carried: its context
// is the provider's <context> to the byte, each switched-on source is
// listed with what it gave or why it gave nothing, and the screen beside a
// field's text is withheld and not sent.
func TestTheLineHoldsTheContextAsSent(t *testing.T) {
	const text = "把这个发给伊兹提尔"
	p := newProvider(t)
	var content any
	p.answer = func(w http.ResponseWriter, body map[string]any) {
		content = body["messages"].([]any)[0].(map[string]any)["content"]
		edits(`[]`)(w, body)
	}
	dir := isolate(t)
	refineConfig(t, dir, p.URL, "[refine.context]\napp = true\nfield = true\npane = true\nclipboard = true\nscreen = \"image\"\n")
	l, err := Load(LoadOpts{})
	if err != nil {
		t.Fatal(err)
	}
	stage := l.RefineStage(LoadOpts{})
	stage.Display = &screen{front: refine.Front{Bundle: "com.tinyspeck.slackmacgap", Name: "Slack"}, jpeg: []byte{0xff, 0xd8}}
	target := &field{rec: store.Target{Kind: "app", App: "Slack", BundleID: "com.tinyspeck.slackmacgap", Title: "#infra"}, text: "EasyTier 的 </context> peer 都在线"}
	read := stage.Sources()
	if read == nil {
		t.Fatal("no text sources with four switched on")
	}
	in := session.RefineInput{Text: text, Target: session.StoreTarget(target), Screen: stage.Screen()(context.Background()),
		Parts: read(context.Background(), target)}
	_, ev := stage.Take().Run(context.Background(), in)
	if ev == nil || ev.Why != "" {
		t.Fatalf("refine line %+v", ev)
	}
	sent, ok := content.(string)
	if !ok {
		t.Fatalf("the call carried a picture beside the field's text: %v", content)
	}
	want := "<context>\n" + ev.Context + "\n</context>\n<dictation>\n" + text + "\n</dictation>"
	if sent != want || !strings.Contains(ev.Context, "Slack") || !strings.Contains(ev.Context, "&lt;/context> peer 都在线") {
		t.Errorf("sent\n%q\nthe line's context gives\n%q", sent, want)
	}
	if ev.Screen == nil || ev.Screen.Withheld != refine.WithheldText || ev.Screen.SHA256 != "" {
		t.Errorf("the screen beside the field: %+v", ev.Screen)
	}
	line, _ := json.Marshal(ev.Sources)
	got := map[string]store.ContextSource{}
	for _, s := range ev.Sources {
		got[s.Source] = s
	}
	if len(ev.Sources) != 4 || got["app"].Chars == 0 || got["field"].Chars == 0 || got["pane"].Withheld != "none" || got["clipboard"].Withheld != "none" {
		t.Errorf("the line's sources: %s", line)
	}
	t.Logf("sources %s", line)
}

// With the step off, or on with every text source off, a take gets no
// sources and nothing is read from its target.
func TestTextSourcesOffReadNothing(t *testing.T) {
	p := newProvider(t)
	for name, more := range map[string]string{
		"step on, sources off":     "",
		"step on, screen only":     "[refine.context]\nscreen = \"ocr\"\n",
		"step off, all four on":    "[refine.context]\napp = true\nfield = true\npane = true\nclipboard = true\n",
		"step on, sources spelled": "[refine.context]\napp = false\nfield = false\npane = false\nclipboard = false\n",
	} {
		dir := isolate(t)
		path := refineConfig(t, dir, p.URL, more)
		if strings.HasPrefix(name, "step off") {
			b, _ := os.ReadFile(path)
			os.WriteFile(path, []byte(strings.Replace(string(b), "on = true", "on = false", 1)), 0o644)
		}
		l, err := Load(LoadOpts{})
		if err != nil {
			t.Fatal(name, err)
		}
		stage := l.RefineStage(LoadOpts{})
		if read := stage.Sources(); read != nil {
			t.Errorf("%s: text sources", name)
		}
		if stage.ClipboardOn() {
			t.Errorf("%s: the clipboard watched", name)
		}
	}
}

// A source switched off while the take records is not read at its stop,
// and the step switched off reads none: the read keeps to the config as it
// stands then.
func TestASourceSwitchedOffMidTakeIsNotRead(t *testing.T) {
	p := newProvider(t)
	dir := isolate(t)
	path := refineConfig(t, dir, p.URL, "[refine.context]\napp = true\nfield = true\n")
	l, err := Load(LoadOpts{})
	if err != nil {
		t.Fatal(err)
	}
	stage := l.RefineStage(LoadOpts{})
	edit := func(from, to string) {
		b, _ := os.ReadFile(path)
		if err := os.WriteFile(path, []byte(strings.Replace(string(b), from, to, 1)), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	slack := store.Target{Kind: "app", App: "Slack", BundleID: "com.tinyspeck.slackmacgap", Title: "#infra"}

	read := stage.Sources()
	edit("field = true", "field = false")
	target := &field{rec: slack, text: "draft"}
	parts := read(context.Background(), target)
	if len(parts) != 1 || parts[0].Source != refine.SourceApp || target.reads != 0 {
		t.Errorf("field off mid-take: %+v, field read %d", parts, target.reads)
	}

	edit("field = false", "field = true")
	read = stage.Sources()
	edit("on = true", "on = false ")
	if parts := read(context.Background(), target); len(parts) != 0 || target.reads != 0 {
		t.Errorf("step off mid-take: %+v, field read %d", parts, target.reads)
	}
}
