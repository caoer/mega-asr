package app

import (
	"bytes"
	"context"
	"encoding/json"
	"log"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const testKey = "sk-ant-test-0123456789"

// A key is kept only once the test call with it answers: a call that fails
// leaves the config and the key file as they were and says why, and the
// key is in no error, log line or `config show`.
func TestSetRefineKeyKeepsItOnlyAfterTheTest(t *testing.T) {
	var logs bytes.Buffer
	log.SetOutput(&logs)
	t.Cleanup(func() { log.SetOutput(os.Stderr) })
	var sentKey string
	var answer func(http.ResponseWriter, map[string]any)
	p := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		sentKey = r.Header.Get("x-api-key")
		var body map[string]any
		json.NewDecoder(r.Body).Decode(&body)
		answer(w, body)
	}))
	t.Cleanup(p.Close)
	ok := edits(`[]`)
	refused := func(w http.ResponseWriter, _ map[string]any) {
		w.WriteHeader(http.StatusUnauthorized)
		w.Write([]byte(`{"type":"error","error":{"type":"authentication_error","message":"invalid x-api-key ` + testKey + `"}}`))
	}
	saw := func(answer func(http.ResponseWriter, map[string]any)) func(http.ResponseWriter, map[string]any) {
		return func(w http.ResponseWriter, body map[string]any) {
			b, _ := json.Marshal(body["messages"])
			if !strings.Contains(string(b), "dictation") {
				t.Errorf("test call without a dictation: %s", b)
			}
			answer(w, body)
		}
	}
	old := RefineBaseURL
	RefineBaseURL = p.URL
	t.Cleanup(func() { RefineBaseURL = old })

	// no file, no provider: a refused call writes nothing
	dir := isolate(t)
	path := filepath.Join(dir, "config.toml")
	answer = saw(refused)
	_, err := SetRefineKey(context.Background(), path, " "+testKey+"\n", nil)
	if err == nil || !strings.Contains(err.Error(), "401") || strings.Contains(err.Error(), testKey) {
		t.Fatalf("refused: err %v", err)
	}
	if sentKey != testKey {
		t.Errorf("the test call sent key %q", sentKey)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Errorf("a refused key wrote the config: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, "anthropic.key")); !os.IsNotExist(err) {
		t.Errorf("a refused key was written: %v", err)
	}

	// an answer keeps it: the key file, the provider, the step still off
	answer = saw(ok)
	if _, err := SetRefineKey(context.Background(), path, testKey, nil); err != nil {
		t.Fatal(err)
	}
	kf := filepath.Join(dir, "anthropic.key")
	if b, err := os.ReadFile(kf); err != nil || strings.TrimSpace(string(b)) != testKey {
		t.Fatalf("key file %q, %v", b, err)
	}
	if st, _ := os.Stat(kf); st.Mode().Perm() != 0o600 {
		t.Errorf("key file mode %v", st.Mode().Perm())
	}
	l, err := Load(LoadOpts{Path: path})
	if err != nil {
		t.Fatal(err)
	}
	r := l.Refine
	if r.On || r.Provider != "anthropic" || r.Providers["anthropic"] != (RefineProviderConfig{Kind: "anthropic", BaseURL: p.URL, KeyFile: kf}) {
		t.Errorf("refine after the key: %+v", r)
	}
	if k, err := r.Providers[r.Provider].Key(); err != nil || k != testKey {
		t.Errorf("the provider reads key %q, %v", k, err)
	}
	var show bytes.Buffer
	l.WriteEffective(&show, "megavoice")
	cfg, _ := os.ReadFile(path)
	for what, s := range map[string]string{"config show": show.String(), "config file": string(cfg), "log": logs.String()} {
		if strings.Contains(s, testKey) {
			t.Errorf("the key is in the %s:\n%s", what, s)
		}
	}

	// a provider already selected: a failed test leaves its key file alone,
	// a passing one replaces the key and nothing else
	dir = isolate(t)
	path = refineConfig(t, dir, p.URL, "model = \"claude-sonnet-5-5\"\n")
	before, _ := os.ReadFile(path)
	answer = saw(refused)
	if _, err := SetRefineKey(context.Background(), path, testKey, nil); err == nil || !strings.Contains(err.Error(), "claude-sonnet-5-5") {
		t.Errorf("refused: err %v", err)
	}
	if b, _ := os.ReadFile(filepath.Join(dir, "model.key")); string(b) != "vk-test\n" {
		t.Errorf("a refused key replaced the old one: %q", b)
	}
	answer = saw(ok)
	if _, err := SetRefineKey(context.Background(), path, testKey, nil); err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(filepath.Join(dir, "model.key")); string(b) != testKey+"\n" {
		t.Errorf("key file %q", b)
	}
	if after, _ := os.ReadFile(path); !bytes.Equal(after, before) {
		t.Errorf("the config changed:\n%s", after)
	}
	if strings.Contains(logs.String(), testKey) {
		t.Errorf("the key is in the log:\n%s", logs.String())
	}
}

// A mode's prompt is the build's until the user opens it to edit: a start
// writes no prompt file, so a take sends the prompt of the build that runs.
// Opening it writes the built-in once as modes.d/<mode>.md, and from then
// the file is the user's: an edit survives the next start and a new build.
func TestPromptIsTheBuildsUntilEdited(t *testing.T) {
	dir := isolate(t)
	prompts := func() int {
		files, _ := os.ReadDir(filepath.Join(dir, "modes.d"))
		return len(files)
	}
	Seed() // a start
	for _, m := range RefineModes {
		want, _ := defaults.ReadFile("defaults/modes/" + m + ".md")
		if s, err := (RefineConfig{Mode: m}).Prompt(); err != nil || s != strings.TrimSpace(string(want)) {
			t.Errorf("%s: the prompt is not the built-in: %v", m, err)
		}
	}
	if n := prompts(); n != 0 {
		t.Errorf("a start wrote %d prompt files", n)
	}
	tidy := (RefineConfig{Mode: "tidy"}).PromptPath()
	SeedPrompt(tidy) // Edit Prompt…
	want, _ := defaults.ReadFile("defaults/modes/tidy.md")
	if b, err := os.ReadFile(tidy); err != nil || len(b) == 0 || !bytes.Equal(b, want) {
		t.Errorf("opened to edit: %d bytes, %v, want the built-in", len(b), err)
	}
	if err := os.WriteFile(tidy, []byte("my own tidy prompt\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	Seed()           // the next start, or a new build's
	SeedPrompt(tidy) // opened again
	if s, err := (RefineConfig{Mode: "tidy"}).Prompt(); err != nil || s != "my own tidy prompt" {
		t.Errorf("tidy's prompt %q, %v", s, err)
	}
	// A file that is no built-in mode's prompt is not written.
	SeedPrompt(GlossaryPath())
	SeedPrompt(filepath.Join(dir, "modes.d", "meeting-notes.md"))
	if _, err := os.Stat(GlossaryPath()); err == nil || prompts() != 1 {
		t.Errorf("opening other files wrote a prompt: %d prompt files, glossary %v", prompts(), err)
	}
}

// A text mode whose prompt file is gone loads, and its takes are held until
// the file is back; setting a key meanwhile is tested in fix-words and kept,
// the mode left as it is.
func TestSetRefineKeyWhileTheModesPromptIsGone(t *testing.T) {
	var asked string
	p := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		json.NewDecoder(r.Body).Decode(&body)
		b, _ := json.Marshal(body)
		asked = string(b)
		edits(`[]`)(w, body)
	}))
	t.Cleanup(p.Close)
	dir := isolate(t)
	path := refineConfig(t, dir, p.URL, "mode = \"meeting-notes\"\n")
	if _, err := SetRefineKey(context.Background(), path, testKey, nil); err != nil {
		t.Fatalf("the key was refused while modes.d/meeting-notes.md is gone: %v", err)
	}
	if fw, _ := (RefineConfig{Mode: "fix-words"}).Prompt(); !strings.Contains(asked, jsonText(strings.SplitN(fw, "\n", 2)[0])) {
		t.Errorf("the test call did not send fix-words' prompt: %s", asked)
	}
	if b, _ := os.ReadFile(filepath.Join(dir, "model.key")); string(b) != testKey+"\n" {
		t.Errorf("key file %q", b)
	}
	if l, err := Load(LoadOpts{Path: path}); err != nil || l.Refine.Mode != "meeting-notes" {
		t.Errorf("refine.mode after the key: %q, %v", l.Refine.Mode, err)
	}
}

// jsonText is s as it reads inside a JSON string.
func jsonText(s string) string {
	b, _ := json.Marshal(s)
	return strings.Trim(string(b), `"`)
}
