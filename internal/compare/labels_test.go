package compare

import (
	"bufio"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/caoer/mega-asr/internal/asr"
	"github.com/caoer/mega-asr/internal/audio"
)

// takes writes takes into dir: id → delivered text ("" is still pending).
func takes(t *testing.T, dir string, ts map[string]string) {
	t.Helper()
	for id, text := range ts {
		if err := audio.SaveWAV(filepath.Join(dir, id+".wav"), make([]int16, audio.Rate*3/2)); err != nil {
			t.Fatal(err)
		}
		if text != "" {
			os.WriteFile(filepath.Join(dir, id+".txt"), []byte(text+"\n"), 0o644)
		}
	}
}

func TestLabelRow(t *testing.T) {
	dir := t.TempDir()
	takes(t, dir, map[string]string{"20200314-103000": "周末去爬山记得带狗普罗", "20200314-103500": "plain"})
	rec := Record{ID: "20200314-103000", Primary: "funasr", DeliveredBy: "funasr",
		Engines: map[string]EngineResult{"funasr": {Text: "周末去爬山记得带狗普罗", LatencyMS: 310}, "doubao": {Text: "周末去爬山记得带 GoPro", LatencyMS: 690,
			Words: []asr.Word{{Text: "周末", StartMS: 120, EndMS: 480}, {Text: "GoPro", StartMS: 1400, EndMS: 1900}}}}}
	writeJSON(RecordPath(filepath.Join(dir, "20200314-103000.wav")), rec)
	at := time.Date(2020, 3, 14, 11, 0, 0, 0, time.UTC)
	l := &Labels{Path: filepath.Join(t.TempDir(), "megavoice", "labels.jsonl"), Now: func() time.Time { return at }}

	row, err := l.Save(dir, Label{ID: "20200314-103000", Ref: " 周末去爬山记得带 GoPro \n"})
	if err != nil {
		t.Fatal(err)
	}
	b, _ := os.ReadFile(l.Path)
	want := `{"id":"20200314-103000","wav":"` + filepath.Join(dir, "20200314-103000.wav") + `","ref":"周末去爬山记得带 GoPro","lang":"mixed","dur_s":1.5,"source":"megavoice-label","delivered":"周末去爬山记得带狗普罗","primary":"funasr","delivered_by":"funasr","engines":{"doubao":{"text":"周末去爬山记得带 GoPro","latency_ms":690},"funasr":{"text":"周末去爬山记得带狗普罗","latency_ms":310}},"correct":["doubao"],"label_source":"typed","labeled_at":"2020-03-14T11:00:00Z"}` + "\n"
	if string(b) != want { // the row as before word timing: doubao's words stay in the compare record
		t.Fatalf("row\n%s\nwant\n%s", b, want)
	}
	if again, _ := ReadRecord(filepath.Join(dir, "20200314-103000.wav")); len(again.Engines["doubao"].Words) != 2 {
		t.Errorf("the compare record lost its words: %+v", again.Engines["doubao"])
	}
	if row.Ref != "周末去爬山记得带 GoPro" {
		t.Errorf("returned row %+v", row)
	}

	// a note alone, on a take without a compare record; then a re-save of
	// the first take supersedes its earlier row
	if _, err := l.Save(dir, Label{ID: "20200314-103500", Note: "mic was muted"}); err != nil {
		t.Fatal(err)
	}
	if _, err := l.Save(dir, Label{ID: "20200314-103000", Ref: "周末去爬山记得带 GoPro。", Note: "punctuation"}); err != nil {
		t.Fatal(err)
	}
	latest, err := l.Latest()
	if err != nil || len(latest) != 2 || latest["20200314-103000"].Note != "punctuation" || latest["20200314-103500"].Ref != "" ||
		latest["20200314-103500"].Lang != "" || latest["20200314-103500"].Engines != nil || latest["20200314-103500"].Delivered != "plain" {
		t.Fatalf("latest %+v, %v", latest, err)
	}
	if n := lines(t, l.Path); n != 3 {
		t.Fatalf("%d rows, want 3 (append-only)", n)
	}

	for _, c := range []struct {
		id, ref, note string
		want          error
	}{{"20200314-103000", " ", "", ErrEmpty}, {"../x", "a", "", ErrNoTake}, {"20200314-113000", "a", "", ErrNoTake}} {
		if _, err := l.Save(dir, Label{ID: c.id, Ref: c.ref, Note: c.note}); !errors.Is(err, c.want) {
			t.Errorf("Save(%q, %q, %q): %v, want %v", c.id, c.ref, c.note, err, c.want)
		}
	}
	if _, err := l.Save(dir, Label{ID: "20200314-103000"}); !errors.Is(err, ErrEmpty) {
		t.Errorf("empty label: %v", err)
	}
}

// A click labels a take with an engine's text: the clicked engines and
// every engine that said the same (ignoring spacing, punctuation and case)
// are correct. A take without a compare record has its delivered text as
// the one engine, "delivered". Clearing writes a row that unlabels it.
func TestClickLabel(t *testing.T) {
	dir := t.TempDir()
	takes(t, dir, map[string]string{"20200314-103000": "周末去爬山记得带狗普罗", "20200314-103500": "plain text"})
	writeJSON(RecordPath(filepath.Join(dir, "20200314-103000.wav")), Record{ID: "20200314-103000", Primary: "funasr", DeliveredBy: "funasr",
		Engines: map[string]EngineResult{"funasr": {Text: "周末去爬山记得带狗普罗"}, "doubao": {Text: "周末去爬山记得带 GoPro"},
			"other": {Text: "周末去爬山记得带gopro。"}, "broken": {Error: "timeout"}}})
	l := &Labels{Path: filepath.Join(t.TempDir(), "labels.jsonl")}
	save := func(in Label) Row {
		t.Helper()
		row, err := l.Save(dir, in)
		if err != nil {
			t.Fatal(err)
		}
		return row
	}

	row := save(Label{ID: "20200314-103000", Correct: []string{"doubao"}, Note: "n"})
	if row.Ref != "周末去爬山记得带 GoPro" || row.Lang != "mixed" || strings.Join(row.Correct, ",") != "doubao,other" || row.LabelSource != "click" || row.Note != "n" {
		t.Fatalf("click row %+v", row)
	}
	// two engines marked though they differ: the first one's text is the ref
	row = save(Label{ID: "20200314-103000", Correct: []string{"funasr", "doubao"}})
	if row.Ref != "周末去爬山记得带狗普罗" || strings.Join(row.Correct, ",") != "funasr,doubao" {
		t.Fatalf("multi row %+v", row)
	}
	row = save(Label{ID: "20200314-103500", Correct: []string{"delivered"}})
	if row.Ref != "plain text" || strings.Join(row.Correct, ",") != "delivered" || row.LabelSource != "click" {
		t.Fatalf("no-record row %+v", row)
	}
	// typed text that says what an engine said marks it too
	row = save(Label{ID: "20200314-103500", Ref: "Plain text!"})
	if row.LabelSource != "typed" || strings.Join(row.Correct, ",") != "delivered" {
		t.Fatalf("typed row %+v", row)
	}
	for _, in := range []Label{{ID: "20200314-103000", Correct: []string{"nope"}}, {ID: "20200314-103000", Correct: []string{"broken"}},
		{ID: "20200314-103000", Correct: []string{"delivered"}}, {ID: "20200314-103500", Correct: []string{"funasr"}}} {
		if _, err := l.Save(dir, in); err == nil {
			t.Errorf("Save(%+v): no error", in)
		}
	}

	row = save(Label{ID: "20200314-103000", Clear: true})
	if row.LabelSource != "cleared" || row.Ref != "" || row.Correct != nil {
		t.Fatalf("cleared row %+v", row)
	}
	latest, err := l.Latest()
	if _, ok := latest["20200314-103000"]; ok || err != nil || latest["20200314-103500"].Ref != "Plain text!" {
		t.Fatalf("latest after clear %+v, %v", latest, err)
	}
}

func lines(t *testing.T, path string) int {
	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	n := 0
	for sc := bufio.NewScanner(f); sc.Scan(); n++ {
	}
	return n
}

func TestLang(t *testing.T) {
	for s, want := range map[string]string{"你好。": "zh", "hello, world": "en", "买两斤 mango": "mixed", "123 。": ""} {
		if got := Lang(s); got != want {
			t.Errorf("Lang(%q) = %q, want %q", s, got, want)
		}
	}
}
