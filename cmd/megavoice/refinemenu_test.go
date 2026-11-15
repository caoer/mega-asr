package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/caoer/mega-asr/internal/app"
	"github.com/caoer/mega-asr/internal/refine"
	"github.com/caoer/mega-asr/internal/session"
	"github.com/caoer/mega-asr/internal/store"
)

// Each item of the Model Step submenu writes its key, and the next take of
// the process that was already running reads it: its refine line names the
// new mode, model and effort, with no restart.
func TestRefineMenuAppliesAtTheNextTake(t *testing.T) {
	cfg := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", cfg)
	dir := filepath.Join(cfg, "megavoice")
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			return
		}
		var body struct {
			Output struct {
				Format struct {
					Schema struct{ Properties map[string]any }
				}
			} `json:"output_config"`
		}
		json.NewDecoder(r.Body).Decode(&body)
		answer := `{"edits":[]}`
		if _, ok := body.Output.Format.Schema.Properties["text"]; ok {
			answer = `{"text":"我们明天开会。"}`
		}
		json.NewEncoder(w).Encode(map[string]any{"content": []map[string]string{{"type": "text", "text": answer}}, "stop_reason": "end_turn"})
	}))
	t.Cleanup(srv.Close)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	os.WriteFile(filepath.Join(dir, "model.key"), []byte("vk-test\n"), 0o600)
	path := filepath.Join(dir, "config.toml")
	os.WriteFile(path, []byte("[refine]\nprovider = \"gateway\"\n\n[refine.providers.gateway]\nkind = \"anthropic\"\nbase_url = \""+
		srv.URL+"\"\nkey_file = \""+filepath.Join(dir, "model.key")+"\"\n"), 0o644)
	o := app.LoadOpts{Path: path}
	l, err := load(o)
	if err != nil {
		t.Fatal(err)
	}
	stage := l.RefineStage(o) // the running process, made once
	app.Seed()

	menu := func() []row {
		l, err := load(o)
		if err != nil {
			t.Fatal(err)
		}
		return []row{voiceState{Refine: refineOf(l.Refine)}.refineRow()}
	}
	click := func(title string) {
		t.Helper()
		a := rowAct(menu(), title)
		if a == nil || a.kind != actSet {
			t.Fatalf("no setting row %q in\n%s", title, titles(menu()))
		}
		if err := set(path, a.key, a.arg); err != nil {
			t.Fatalf("%s: %v", title, err)
		}
	}
	take := func() (string, *store.Event) {
		step := stage.Take()
		if step == nil {
			return "", nil
		}
		return step.Run(context.Background(), session.RefineInput{Text: "嗯我们明天开会"})
	}

	if _, ev := take(); ev != nil {
		t.Fatalf("off: a take ran the step: %+v", ev)
	}
	click("On")
	if _, ev := take(); ev == nil || ev.Mode != "fix-words" || ev.Model != "claude-haiku-5-5" || ev.Effort != "low" || ev.Why != "" {
		t.Fatalf("on: %+v", ev)
	}
	click("Rewrite")
	if text, ev := take(); ev == nil || ev.Mode != "tidy" || text != "我们明天开会。" {
		t.Errorf("tidy: %q, %+v", text, ev)
	}
	click("claude-sonnet-5-5")
	click("High")
	if _, ev := take(); ev == nil || ev.Model != "claude-sonnet-5-5" || ev.Effort != "high" {
		t.Errorf("model and effort: %+v", ev)
	}
	click("Screen: Its Text (OCR)")
	l, _ = load(o)
	if c := l.Refine.Context; c.Screen != "ocr" {
		t.Errorf("context: %+v", c)
	}
	if got := titles(menu()); !strings.Contains(got, "Model Step: Model Writes · Rewrite") || !strings.Contains(got, "✓claude-sonnet-5-5") {
		t.Errorf("the menu after the clicks:\n%s", got)
	}
	click("On")
	if _, ev := take(); ev != nil {
		t.Errorf("off again: a take ran the step: %+v", ev)
	}
}

// grant is a refine.Display that counts the requests for the grant.
type grant struct {
	on    bool
	asked int
}

func (g *grant) Access() bool                              { return g.on }
func (g *grant) Request() bool                             { g.asked++; return g.on }
func (g *grant) Front() (refine.Front, error)              { return refine.Front{}, nil }
func (g *grant) Capture(refine.Front) (refine.Shot, error) { return nil, nil }

// Switching the screen source on from the menu, to its text or its
// picture, asks for the Screen Recording grant once while it is off;
// switching it off, or on with the grant given, asks nothing.
func TestSwitchingTheScreenOnFromTheMenuAsks(t *testing.T) {
	rows := []row{voiceState{Refine: refineOf(app.RefineConfig{Mode: "fix-words", Context: app.RefineContextConfig{Screen: "off"}})}.refineRow()}
	for _, tc := range []struct {
		title string
		on    bool
		asked int
	}{
		{"Screen: Its Text (OCR)", false, 1},
		{"Screen: Image", false, 1},
		{"Screen: Off", false, 0},
		{"Screen: Image", true, 0},
	} {
		a := rowAct(rows, tc.title)
		if a == nil || a.kind != actSet {
			t.Fatalf("no setting row %q in\n%s", tc.title, titles(rows))
		}
		g := &grant{on: tc.on}
		err := askScreen(g, a.key, a.arg)
		if g.asked != tc.asked || (err != nil) != (tc.asked == 1) {
			t.Errorf("%s with the grant %v: asked %d times, %v", tc.title, tc.on, g.asked, err)
		}
	}
}
