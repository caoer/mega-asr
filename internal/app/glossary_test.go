package app

import (
	"context"
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/caoer/mega-asr/internal/refine"
)

// vocabulary writes files, name → content, beside the config in dir.
func vocabulary(t *testing.T, dir string, files map[string]string) {
	t.Helper()
	for name, body := range files {
		path := filepath.Join(dir, name)
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
}

// The glossary is the hot words, then what the corrections table corrects
// to, then glossary.txt and glossary.d, each file in its own order and a
// term once; the table's rules are its mis-hearings and about.txt its
// about line.
func TestGlossaryFromTheFourSources(t *testing.T) {
	dir := isolate(t)
	vocabulary(t, dir, map[string]string{
		"hotwords.txt":          "# mine\nKubernetes\nkubectl\n",
		"hotwords.d/team.txt":   "Postgres\nkubectl\n",
		"corrections.tsv":       "# sample\ncube control\tkubectl\nzh:mark down\tMarkdown\n",
		"corrections.d/b.tsv":   "post gress\tPostgres\nget hub\tGitHub\n",
		"glossary.txt":          "TypeScript\n\n# a comment\ngithub\n",
		"glossary.d/10-a.txt":   "Redis\n",
		"glossary.d/20-b.txt":   "Grafana\nRedis\n",
		"glossary.d/notes.md":   "not a list\n",
		"about.txt":             "# who I am\na backend engineer;\ntalks about clusters\n",
		"corrections.d/bad.tsv": "no tab here\n",
	})
	want := refine.Glossary{
		About: "a backend engineer; talks about clusters",
		Terms: []string{"Kubernetes", "kubectl", "Postgres", "Markdown", "GitHub", "TypeScript", "Redis", "Grafana"},
		Hints: []refine.Hint{{Heard: "cube control", Meant: "kubectl"}, {Heard: "mark down", Meant: "Markdown"},
			{Heard: "post gress", Meant: "Postgres"}, {Heard: "get hub", Meant: "GitHub"}},
	}
	if got := Glossary(); !reflect.DeepEqual(got, want) {
		t.Errorf("glossary\n %+v\nwant\n %+v", got, want)
	}
}

// systemOf runs the step on text and gives the system blocks of the
// request it made, as sent.
func systemOf(t *testing.T, p *provider, text string) string {
	t.Helper()
	var system []byte
	p.answer = func(w http.ResponseWriter, body map[string]any) {
		system, _ = json.Marshal(body["system"])
		edits(`[]`)(w, body)
	}
	l, err := Load(LoadOpts{})
	if err != nil {
		t.Fatal(err)
	}
	if _, ev := l.Refine.Run(context.Background(), text, nil); ev.Why != "" {
		t.Fatalf("refine line %+v", ev)
	}
	return string(system)
}

// The call's system blocks are the prompt, the about line, the terms and
// the mis-hearings, with the cache breakpoint on the last: these bytes,
// the same for every take while no file changes, so a take reads them from
// the cache. A changed file changes them at the next take.
func TestRefinePrefixIsTheSameBytesAcrossTakes(t *testing.T) {
	p := newProvider(t)
	dir := isolate(t)
	refineConfig(t, dir, p.URL, "")
	vocabulary(t, dir, map[string]string{
		"modes.d/fix-words.md": "my prompt\n",
		"hotwords.txt":         "Kubernetes\n",
		"corrections.tsv":      "cube control\tkubectl\n",
		"glossary.txt":         "Grafana\n",
		"about.txt":            "a backend engineer\n",
	})
	const want = `[{"text":"my prompt","type":"text"},` +
		`{"text":"关于说话的人：a backend engineer","type":"text"},` +
		`{"text":"词表：他常说的词，只用来认出被听错的词。原文里已经说得通的词或名字，不要换成词表里别的词，也不要换成词表里的写法，大小写、空格、连字符、版本号一律照原文。\nKubernetes\nkubectl\nGrafana","type":"text"},` +
		`{"cache_control":{"type":"ephemeral"},"text":"识别器把他的词听错过的例子（听成 → 他说的是）。原文里有读音相近的写法、放在句子里说不通时，参照这些例子还原；左边的写法在句子里本来说得通的，不动。\ncube control → kubectl","type":"text"}]`
	first := systemOf(t, p, "第一段话")
	if first != want {
		t.Errorf("system blocks\n%s\nwant\n%s", first, want)
	}
	if second := systemOf(t, p, "another take, with other words"); second != first {
		t.Errorf("the system blocks changed between takes:\n%s\n%s", first, second)
	}

	vocabulary(t, dir, map[string]string{"glossary.txt": "Grafana\nRedis\n"})
	if third := systemOf(t, p, "第一段话"); third == first || !strings.Contains(third, `Grafana\nRedis`) {
		t.Errorf("a term added to glossary.txt is not in the next take's request:\n%s", third)
	}
}

// With no hot words, corrections, glossary or about file the call carries
// the prompt alone, as the step did before it had a glossary.
func TestRefineWithoutGlossaryFilesSendsThePromptAlone(t *testing.T) {
	p := newProvider(t)
	dir := isolate(t)
	refineConfig(t, dir, p.URL, "")
	vocabulary(t, dir, map[string]string{"modes.d/fix-words.md": "my prompt\n"})
	if got, want := systemOf(t, p, "好"), `[{"cache_control":{"type":"ephemeral"},"text":"my prompt","type":"text"}]`; got != want {
		t.Errorf("system %s, want %s", got, want)
	}
}
