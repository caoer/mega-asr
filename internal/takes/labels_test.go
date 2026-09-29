package takes

import (
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/caoer/mega-asr/internal/asr"
	"github.com/caoer/mega-asr/internal/compare"
	"github.com/caoer/mega-asr/internal/store"
)

// The page reads a take's label, its primary engine and an engine's word
// timing from the take's own answers, and writes the label through
// /takes/api/label as a row of the label file, the same row the file has
// always had; a refused label says why by its code.
func TestLabelsOnTheTake(t *testing.T) {
	dir := t.TempDir()
	base := take(t, dir, "20200314-092653", "dust the pistol gently", "Dust the pistol gently.")
	take(t, dir, "20200314-093500", "water the ferns", "Water the ferns.")
	words := []asr.Word{{Text: "Dust", StartMS: 300, EndMS: 620}, {Text: "the", StartMS: 640, EndMS: 760}, {Text: "pistil", StartMS: 800, EndMS: 1250}}
	rec := compare.Record{ID: "20200314-092653", Primary: "funasr", DeliveredBy: "funasr", Engines: map[string]compare.EngineResult{
		"funasr": {Text: "Dust the pistol gently.", LatencyMS: 400},
		"doubao": {Text: "Dust the pistil gently.", LatencyMS: 900, Words: words},
	}}
	b, _ := json.Marshal(rec)
	if err := os.WriteFile(compare.RecordPath(base+".wav"), b, 0o644); err != nil {
		t.Fatal(err)
	}
	labels := filepath.Join(t.TempDir(), "labels.jsonl")
	h := New(dir, &compare.Labels{Path: labels}, func() []Engine { return nil })
	g := &Guard{Next: h, Key: func() (string, error) { return key, nil }, Port: "7865", Policy: Policy()}
	get := func(path string, v any) {
		t.Helper()
		w := request(g, http.MethodGet, path, nil)
		if w.Code != http.StatusOK || json.Unmarshal(w.Body.Bytes(), v) != nil {
			t.Fatalf("GET %s: %d %s", path, w.Code, w.Body)
		}
	}
	labeled := func() map[string]bool {
		t.Helper()
		var pg Page
		get("/takes/api/takes", &pg)
		out := map[string]bool{}
		for _, r := range pg.Takes {
			out[r.ID] = r.Labeled
		}
		return out
	}

	var tk Take
	get("/takes/api/take/20200314-092653", &tk)
	if tk.Label != nil || tk.Labeled || labeled()["20200314-092653"] {
		t.Fatalf("an unlabelled take: label %+v, labeled %v", tk.Label, tk.Labeled)
	}
	got := map[string]Answer{}
	for _, a := range tk.Answers {
		got[a.Engine] = a
	}
	if !got["funasr"].Primary || got["doubao"].Primary || len(got["doubao"].Words) != 3 || got["doubao"].Words[2] != words[2] || got["funasr"].Words != nil {
		t.Fatalf("answers %+v", tk.Answers)
	}

	// a composed ground truth, then a click, then a clear
	for _, c := range []struct {
		body, source, ref string
		correct           []string
	}{
		{`{"id":"20200314-092653","ref":"Dust the pistil gently.","note":"meant pistil"}`, "typed", "Dust the pistil gently.", []string{"doubao"}},
		{`{"id":"20200314-092653","correct":["funasr"]}`, "click", "Dust the pistol gently.", []string{"funasr"}},
	} {
		w := post(g, "/takes/api/label", c.body, nil)
		var row compare.Row
		if w.Code != http.StatusOK || json.Unmarshal(w.Body.Bytes(), &row) != nil || row.LabelSource != c.source || row.Ref != c.ref || strings.Join(row.Correct, ",") != strings.Join(c.correct, ",") {
			t.Fatalf("POST %s: %d %s", c.body, w.Code, w.Body)
		}
		get("/takes/api/take/20200314-092653", &tk)
		if tk.Label == nil || tk.Label.Ref != c.ref || tk.Label.LabelSource != c.source || !labeled()["20200314-092653"] || labeled()["20200314-093500"] {
			t.Fatalf("after %s: label %+v", c.source, tk.Label)
		}
	}
	if w := post(g, "/takes/api/label", `{"id":"20200314-092653","clear":true}`, nil); w.Code != http.StatusOK {
		t.Fatalf("clear: %d %s", w.Code, w.Body)
	}
	get("/takes/api/take/20200314-092653", &tk)
	if tk.Label != nil || labeled()["20200314-092653"] {
		t.Fatalf("after a clear: label %+v", tk.Label)
	}
	// the rows carry the engines' texts, never their word timing
	file, _ := os.ReadFile(labels)
	if n := strings.Count(string(file), "\n"); n != 3 || strings.Contains(string(file), "start_ms") || !strings.Contains(string(file), `"engines":{"doubao":{"text":"Dust the pistil gently.","latency_ms":900}`) {
		t.Fatalf("label file (%d rows):\n%s", n, file)
	}

	// a take whose text came from the backup input takes a note, never a text
	if err := store.Append(filepath.Join(dir, "20200314-093500"), store.Event{Ev: "text", Engine: "funasr", Audio: "backup"}); err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct {
		body   string
		status int
		code   string
	}{
		{`{"id":"20200314-093500","ref":"Water the ferns."}`, http.StatusBadRequest, "backup_text"},
		{`{"id":"20200314-093500","correct":["delivered"]}`, http.StatusBadRequest, "backup_text"},
		{`{"id":"20200314-092653"}`, http.StatusBadRequest, "label_empty"},
		{`{"id":"20200314-092653","correct":["sensevoice"]}`, http.StatusBadRequest, "no_answer"},
		{`{"id":"20200314-110000","ref":"x"}`, http.StatusNotFound, "no_take"},
		{`{"id":`, http.StatusBadRequest, "bad_request"},
	} {
		if status, code := refusal(t, post(g, "/takes/api/label", c.body, nil)); status != c.status || code != c.code {
			t.Errorf("POST %s: %d %s, want %d %s", c.body, status, code, c.status, c.code)
		}
	}
	if w := post(g, "/takes/api/label", `{"id":"20200314-093500","note":"hold for now"}`, nil); w.Code != http.StatusOK {
		t.Fatalf("a note on the backup take: %d %s", w.Code, w.Body)
	}
	if file, _ := os.ReadFile(labels); strings.Count(string(file), "\n") != 4 {
		t.Fatalf("label file after the refusals:\n%s", file)
	}
}

// The compare panel's old address sends the browser to the Takes page; the
// Location carries no fragment, so the browser keeps the one it opened,
// the key.
func TestRootRedirects(t *testing.T) {
	h := listener(t, t.TempDir())
	w := request(h, http.MethodGet, "/", func(r *http.Request) { r.Header.Del(Header); r.Header.Set("Sec-Fetch-Site", "none") })
	if w.Code != http.StatusFound || w.Header().Get("Location") != "/takes/" {
		t.Fatalf("GET /: %d, Location %q; want 302 to /takes/", w.Code, w.Header().Get("Location"))
	}
	for _, p := range []string{"/api/takes", "/api/label", "/audio/20200314-092653.wav"} {
		if w := request(h, http.MethodGet, p, nil); w.Code != http.StatusNotFound {
			t.Errorf("GET %s, the compare panel's API: %d, want 404", p, w.Code)
		}
	}
}

// A browser window showing the page carries its title: the resend picker
// knows such a window by PageTitle.
func TestPageTitle(t *testing.T) {
	b, err := page.ReadFile("page/index.html")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(b), "<title>"+PageTitle+"</title>") {
		t.Errorf("index.html's title is not %q", PageTitle)
	}
}
