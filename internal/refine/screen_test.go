package refine

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"strings"
	"testing"
)

// display is a Display that counts what the source asks of it.
type display struct {
	access, grant bool // the grant now, and what a request makes it
	front         Front
	text          string
	asked, shots  int
	edge          int
	released      bool
}

func (d *display) Access() bool { return d.access }
func (d *display) Request() bool {
	d.asked++
	d.access = d.grant
	return d.access
}
func (d *display) Front() (Front, error) { return d.front, nil }
func (d *display) Capture(Front) (Shot, error) {
	d.shots++
	return d, nil
}
func (d *display) Size() (int, int)      { return 3024, 1964 }
func (d *display) Text() (string, error) { return d.text, nil }
func (d *display) JPEG(edge int) ([]byte, int, int, error) {
	d.edge = edge
	return []byte("jpeg"), edge, edge * 1964 / 3024, nil
}
func (d *display) Release() { d.released = true }

func TestScreenSendsNothingFromAPasswordFieldOrAnExcludedApp(t *testing.T) {
	for _, tc := range []struct {
		name  string
		front Front
		why   string
	}{
		{"password field", Front{Bundle: "com.apple.Safari", Name: "Safari", Title: "Sign in", Secure: true}, WithheldSecure},
		{"excluded app", Front{Bundle: "com.example.vault", Name: "Vault", Title: "All items"}, WithheldExcluded},
	} {
		for _, mode := range []string{ScreenOCR, ScreenImage} {
			d := &display{access: true, front: tc.front, text: "hunter2"}
			sc := ScreenSource{Mode: mode, Exclude: []string{"com.example.vault"}, Display: d}.Take()
			if d.shots != 0 {
				t.Errorf("%s, %s: the window was captured", tc.name, mode)
			}
			if sc.Withheld != tc.why || sc.Text != "" || sc.Image != nil || sc.Title != "" || sc.Context() != "" {
				t.Errorf("%s, %s: %+v, context %q", tc.name, mode, sc, sc.Context())
			}
		}
	}
}

func TestScreenAsksForTheGrantAndGoesWithoutIt(t *testing.T) {
	front := Front{Bundle: "com.example.term", Name: "Term", Title: "build"}
	d := &display{front: front, text: "make test"}
	sc := ScreenSource{Mode: ScreenOCR, Display: d}.Take()
	if d.asked != 1 || d.shots != 0 || sc.Withheld != WithheldPermission || sc.Context() != "" {
		t.Errorf("refused: asked %d, captured %d, %+v", d.asked, d.shots, sc)
	}
	d = &display{grant: true, front: front, text: "make test"}
	sc = ScreenSource{Mode: ScreenOCR, Display: d}.Take()
	if d.asked != 1 || d.shots != 1 || sc.Withheld != "" || sc.Text != "make test" {
		t.Errorf("granted at the prompt: asked %d, captured %d, %+v", d.asked, d.shots, sc)
	}
	d = &display{access: true, front: front, text: "make test"}
	if (ScreenSource{Mode: ScreenOCR, Display: d}).Take(); d.asked != 0 {
		t.Errorf("asked %d times with the grant on", d.asked)
	}
}

func TestScreenForms(t *testing.T) {
	front := Front{Bundle: "com.example.term", Name: "Term", Title: "build"}
	d := &display{access: true, front: front, text: "  kubectl get pods\nRedis ready  \n"}
	sc := ScreenSource{Mode: ScreenOCR, Display: d}.Take()
	want := "Text of the window in front of the speaker (Term, \"build\"), as read off the screen:\nkubectl get pods\nRedis ready"
	if sc.Context() != want || sc.Image != nil || sc.W != 3024 || !d.released {
		t.Errorf("ocr: %+v\ncontext %q", sc, sc.Context())
	}

	d = &display{access: true, front: front}
	sc = ScreenSource{Mode: ScreenImage, Display: d}.Take()
	if sc.Image == nil || string(sc.Image.Data) != "jpeg" || sc.Image.MediaType != "image/jpeg" || sc.Image.W != ScreenEdge || d.edge != ScreenEdge || sc.Text != "" {
		t.Errorf("image: %+v", sc)
	}
	if got := sc.Context(); got != `The image is the window in front of the speaker: Term, "build".` {
		t.Errorf("image context %q", got)
	}

	d = &display{access: true, front: front, text: " \n"}
	if sc = (ScreenSource{Mode: ScreenOCR, Display: d}).Take(); sc.Withheld != WithheldEmpty || sc.Context() != "" {
		t.Errorf("a window with no text: %+v", sc)
	}

	d = &display{access: true, front: front, text: strings.Repeat("旧", 500) + strings.Repeat("新", maxScreenText)}
	if sc = (ScreenSource{Mode: ScreenOCR, Display: d}).Take(); sc.Text != strings.Repeat("新", maxScreenText) {
		t.Errorf("a long window keeps %d characters, starting %q", len([]rune(sc.Text)), string([]rune(sc.Text)[:3]))
	}
}

type failing struct{ display }

func (f *failing) Capture(Front) (Shot, error) { return nil, errors.New("no window") }

func TestScreenCaptureFailureIsWithheld(t *testing.T) {
	d := &failing{display{access: true}}
	if sc := (ScreenSource{Mode: ScreenOCR, Display: d}).Take(); sc.Withheld != WithheldError || sc.Err == nil {
		t.Errorf("%+v", sc)
	}
}

func TestAnthropicRequestWithAnImage(t *testing.T) {
	m := newMessages(t)
	m.body = answer("end_turn", `{"edits":[]}`)
	req := Request{Model: "claude-haiku-5-5", Effort: "low", System: []string{"prompt"},
		Context: "The image is the window.", Image: &Image{MediaType: "image/jpeg", Data: []byte{0xff, 0xd8, 0xff}}, Dictation: "用海库"}
	if _, err := m.provider().Refine(context.Background(), req); err != nil {
		t.Fatal(err)
	}
	var buf strings.Builder
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	enc.Encode(m.got["messages"])
	body := strings.TrimSpace(buf.String())
	want := `[{"content":[{"source":{"data":"` + base64.StdEncoding.EncodeToString([]byte{0xff, 0xd8, 0xff}) +
		`","media_type":"image/jpeg","type":"base64"},"type":"image"},` +
		`{"text":"<context>\nThe image is the window.\n</context>\n<dictation>\n用海库\n</dictation>","type":"text"}],"role":"user"}]`
	if body != want {
		t.Errorf("messages\n got %s\nwant %s", body, want)
	}
}
