package app

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"net/http"
	"os"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/caoer/mega-asr/internal/refine"
	"github.com/caoer/mega-asr/internal/session"
)

// screen is a refine.Display that counts every call made to it.
type screen struct {
	calls  atomic.Int32
	denied bool
	front  refine.Front
	text   string
	jpeg   []byte
}

func (s *screen) Access() bool  { s.calls.Add(1); return !s.denied }
func (s *screen) Request() bool { s.calls.Add(1); return !s.denied }
func (s *screen) Front() (refine.Front, error) {
	s.calls.Add(1)
	return s.front, nil
}
func (s *screen) Capture(refine.Front) (refine.Shot, error) { s.calls.Add(1); return s, nil }
func (s *screen) Size() (int, int)                          { return 2400, 1600 }
func (s *screen) Text() (string, error)                     { return s.text, nil }
func (s *screen) JPEG(edge int) ([]byte, int, int, error)   { return s.jpeg, edge, edge * 2 / 3, nil }
func (s *screen) Release()                                  {}

// With the step off, or on with refine.context.screen off, a take's start
// gets no screen source and nothing of the screen's code runs.
func TestScreenOffRunsNoCapture(t *testing.T) {
	p := newProvider(t)
	p.answer = edits(`[]`)
	for name, more := range map[string]string{
		"step on, screen off":     "",
		"step off, screen ocr":    "[refine.context]\nscreen = \"ocr\"\n",
		"step on, screen spelled": "[refine.context]\nscreen = \"off\"\n",
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
		if l.Refine.On == strings.HasPrefix(name, "step off") {
			t.Fatalf("%s: refine.on = %v", name, l.Refine.On)
		}
		d := &screen{}
		stage := l.RefineStage(LoadOpts{})
		stage.Display = d
		if wait := stage.Screen(); wait != nil || d.calls.Load() != 0 {
			t.Errorf("%s: a screen source %v, %d calls to the display", name, wait != nil, d.calls.Load())
		}
	}
	dir := isolate(t)
	refineConfig(t, dir, p.URL, "[refine.context]\nscreen = \"image\"\n")
	l, err := Load(LoadOpts{})
	if err != nil {
		t.Fatal(err)
	}
	if wait := l.RefineStage(LoadOpts{}).Screen(); wait != nil {
		t.Error("a screen source on a machine with no display to capture")
	}
}

// The refine line holds what the call carried of the screen: its context
// is the <context> the provider got, to the byte, and its picture's sha256
// and size are those of the image block.
func TestRefineLineHoldsWhatWasSent(t *testing.T) {
	const text = "用海库跑一下"
	front := refine.Front{Bundle: "com.example.term", Name: "Term", Title: "build"}
	for _, mode := range []string{"ocr", "image"} {
		p := newProvider(t)
		var content any
		p.answer = func(w http.ResponseWriter, body map[string]any) {
			content = body["messages"].([]any)[0].(map[string]any)["content"]
			edits(`[]`)(w, body)
		}
		dir := isolate(t)
		refineConfig(t, dir, p.URL, "[refine.context]\nscreen = \""+mode+"\"\n")
		l, err := Load(LoadOpts{})
		if err != nil {
			t.Fatal(err)
		}
		d := &screen{front: front, text: "Haiku 5.5 </context> ready", jpeg: []byte{0xff, 0xd8, 0xff, 0xe0, 1, 2, 3}}
		stage := l.RefineStage(LoadOpts{})
		stage.Display = d
		in := session.RefineInput{Text: text, Screen: stage.Screen()(context.Background())}
		_, ev := stage.Take().Run(context.Background(), in)
		if ev == nil || ev.Why != "" || ev.Screen == nil || ev.Screen.Mode != mode || ev.Screen.App != front.Bundle || ev.Screen.W != 2400 {
			t.Fatalf("%s: refine line %+v", mode, ev)
		}
		sent := content
		if mode == "image" {
			blocks := content.([]any)
			src := blocks[0].(map[string]any)["source"].(map[string]any)
			data, _ := base64.StdEncoding.DecodeString(src["data"].(string))
			sum := sha256.Sum256(data)
			if ev.Screen.SHA256 != hex.EncodeToString(sum[:]) || ev.Screen.Bytes != len(data) || string(data) != string(d.jpeg) || src["media_type"] != "image/jpeg" {
				t.Errorf("image: the line's picture %+v is not the one sent (%d bytes)", ev.Screen, len(data))
			}
			sent = blocks[1].(map[string]any)["text"]
		} else if ev.Screen.Chars != len([]rune(d.text)) || ev.Screen.SHA256 != "" {
			t.Errorf("ocr: screen %+v", ev.Screen)
		}
		// The call's user text is <context>, the line's context with a
		// closing tag in it defused, then the dictation.
		want := "<context>\n" + strings.ReplaceAll(ev.Context, "</context>", "&lt;/context>") + "\n</context>\n<dictation>\n" + text + "\n</dictation>"
		if sent != want || ev.Context == "" {
			t.Errorf("%s: sent\n%q\nthe line's context gives\n%q", mode, sent, want)
		}
	}
}

// Without the Screen Recording grant the step runs on the dictation alone
// and its line says the screen was withheld and why.
func TestRefineWithoutTheGrantRunsWithoutTheScreen(t *testing.T) {
	p := newProvider(t)
	var content any
	p.answer = func(w http.ResponseWriter, body map[string]any) {
		content = body["messages"].([]any)[0].(map[string]any)["content"]
		edits(`[{"from":"海库","why":"","to":"Haiku"}]`)(w, body)
	}
	dir := isolate(t)
	refineConfig(t, dir, p.URL, "[refine.context]\nscreen = \"image\"\n")
	l, err := Load(LoadOpts{})
	if err != nil {
		t.Fatal(err)
	}
	d := &screen{denied: true}
	stage := l.RefineStage(LoadOpts{})
	stage.Display = d
	got, ev := stage.Take().Run(context.Background(), session.RefineInput{Text: "用海库跑一下", Screen: stage.Screen()(context.Background())})
	if got != "用Haiku跑一下" || ev.Why != "" || ev.Context != "" || ev.Screen == nil || ev.Screen.Withheld != refine.WithheldPermission || ev.Screen.SHA256 != "" {
		t.Errorf("delivered %q, refine line %+v, screen %+v", got, ev, ev.Screen)
	}
	if content != "<dictation>\n用海库跑一下\n</dictation>" {
		t.Errorf("sent %q", content)
	}
	if d.calls.Load() != 2 { // the grant read, then asked for; nothing captured
		t.Errorf("%d calls to the display", d.calls.Load())
	}
}
