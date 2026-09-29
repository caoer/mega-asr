package drain

import (
	"context"
	"os"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/caoer/mega-asr/internal/labels"
)

// The backfill writes the missing keys alone, by CAS through a conflict,
// grows the vocabulary as it goes, and a rerun changes nothing.
func TestTitlerBackfill(t *testing.T) {
	cmd, stdin := fakeClaude(t, `{"display_title": "Tide table printout", "labels": ["tides", "buoys"], "gist": "g", "details": "d"}`)
	m := newMem()
	m.put(t, "rec.a", map[string]any{"id": "a", "source": "feishu", "title": "untitled take", "state": "aligned", "summary": "飞书", "attempts": 2})
	m.put(t, "rec.b", map[string]any{"id": "b", "source": "feishu", "title": "Harbour call", "state": "ingested", "labels": []string{"hand-set"}})
	m.put(t, "rec.c", map[string]any{"id": "c", "source": "feishu", "title": "x", "state": "deleted"})
	m.put(t, "rec.d", map[string]any{"id": "d", "source": "feishu", "title": "y", "state": "aligned", "display_title": "有", "labels": []string{"tides"}})
	m.put(t, "rec.e", map[string]any{"id": "e", "source": "mac", "state": "uploaded"})
	before := m.rec(t, "a")
	conflicted := false
	m.cas = func(key string) {
		if key == "rec.a" && !conflicted { // the drain writes in between
			conflicted = true
			r := m.rec(t, "a")
			r["attempts"] = 3.0
			m.put(t, key, r)
		}
	}
	tt := &Titler{Reg: m, Sum: &Summarizer{Command: []string{cmd}, Model: "m", MaxChars: 40000,
		Vocab: func(ctx context.Context) (labels.Vocab, error) { return labels.ReadVocab(ctx, m) }},
		Fetch: func(_ context.Context, rec map[string]any) (string, error) {
			if rec["id"] == "e" {
				return t.TempDir(), nil // not processed yet: no transcript
			}
			return diarized(t), nil
		}}
	var got []string
	each := func(r TitleResult) { got = append(got, r.ID+" "+r.Outcome) }
	if err := tt.Run(context.Background(), nil, 0, each); err != nil {
		t.Fatal(err)
	}
	if want := []string{"a updated", "b updated", "d skipped", "e skipped"}; !slices.Equal(got, want) {
		t.Fatalf("results %v, want %v", got, want)
	}
	a := m.rec(t, "a")
	for k, v := range before {
		if k != "attempts" && !reflect.DeepEqual(a[k], v) {
			t.Fatalf("a.%s changed: %v → %v", k, v, a[k])
		}
	}
	if !conflicted || a["attempts"] != 3.0 || len(a) != len(before)+2 || a["display_title"] != "Tide table printout" || !reflect.DeepEqual(a["labels"], []any{"tides", "buoys"}) {
		t.Fatalf("a: %v", a)
	}
	if b := m.rec(t, "b"); b["display_title"] != "Tide table printout" || !reflect.DeepEqual(b["labels"], []any{"hand-set"}) {
		t.Fatalf("b's labels overwritten: %v", b)
	}
	// b was asked after a's labels landed: the vocabulary grew.
	if in, _ := readFile(stdin); !strings.Contains(in, "buoys") {
		t.Fatalf("b's label list lacks a's new label:\n%s", in)
	}
	got = nil
	if err := tt.Run(context.Background(), nil, 0, each); err != nil {
		t.Fatal(err)
	}
	if want := []string{"a skipped", "b skipped", "d skipped", "e skipped"}; !slices.Equal(got, want) {
		t.Fatalf("rerun %v", got)
	}
	// --redo rewrites the named records' display_title; their labels stay.
	tt.Redo, got = true, nil
	if err := tt.Run(context.Background(), []string{"d"}, 0, each); err != nil || !slices.Equal(got, []string{"d updated"}) {
		t.Fatalf("redo %v, %v", got, err)
	}
	if d := m.rec(t, "d"); d["display_title"] != "Tide table printout" || !reflect.DeepEqual(d["labels"], []any{"tides"}) {
		t.Fatalf("redo d: %v", d)
	}
}

func readFile(p string) (string, error) {
	b, err := os.ReadFile(p)
	return string(b), err
}
