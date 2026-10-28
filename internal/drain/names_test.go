package drain

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

// The people are made up.
const namesIndex = `{"updated_at":"2020-03-11T06:40:00Z","people":[
 {"slug":"lin-xiaoxi","name":"林小溪","spoken":["小溪"]},
 {"slug":"zhou-mingyuan","name":"周明远","spoken":["明远"]}]}`

func TestNamesPending(t *testing.T) {
	sp := speakers(map[string]any{"role": "S1", "name": "林小溪", "person": "lin-xiaoxi"}, map[string]any{"role": "S2", "name": "Speaker 2"})
	for _, c := range []struct {
		names   any
		pending bool
		focus   []string
	}{
		{nil, true, nil}, // never had a pass
		{map[string]any{"people": []any{"lin-xiaoxi"}}, false, nil},
		{map[string]any{"people": []any{}}, true, []string{"lin-xiaoxi"}}, // resolved since
		{map[string]any{"people": []any{"lin-xiaoxi"}, "refused": "429"}, true, nil},
	} {
		rec := map[string]any{"speakers": sp}
		if c.names != nil {
			rec["names"] = c.names
		}
		p, f := NamesPending(rec)
		if p != c.pending || !reflect.DeepEqual(f, c.focus) {
			t.Errorf("names %v: pending %v focus %v", c.names, p, f)
		}
	}
}

// replies answers each call by the first key its text holds.
func replies(m map[string]string) func(context.Context, string, string) (string, error) {
	return func(_ context.Context, _, user string) (string, error) {
		text := user[strings.Index(user, "TEXT\n"):]
		for k, v := range m {
			if strings.Contains(text, k) {
				return v, nil
			}
		}
		return `{"edits":[]}`, nil
	}
}

func writeJSONFile(t *testing.T, path string, v any) {
	t.Helper()
	b, _ := json.Marshal(v)
	if err := os.WriteFile(path, b, 0o600); err != nil {
		t.Fatal(err)
	}
}

// Our text is rewritten and its raw kept; Feishu's file is untouched and
// its corrections are in names.json, which BuildRecord lays over it. A
// focused run starts from the corrected Feishu text and keeps the first
// run's changes.
func TestNamerRewritesOursAndCorrectsFeishuBeside(t *testing.T) {
	dir := t.TempDir()
	writeJSONFile(t, filepath.Join(dir, "segments.json"), []map[string]any{
		{"track": "media", "speaker": "S1", "start_s": 0.0, "end_s": 5.0, "raw": "我跟小西说", "text": "我跟小西说", "engine": "funasr"},
		{"track": "media", "speaker": "S1", "start_s": 5.0, "end_s": 9.0, "raw": "明元呢", "text": "明元呢", "engine": "funasr"},
	})
	feishu := `{"header":"h","segments":[{"speaker":"林小溪","start_s":1,"text":"小西你好"},{"speaker":"周明远","start_s":6,"text":"明元在"}]}`
	if err := os.WriteFile(filepath.Join(dir, "feishu-transcript.json"), []byte(feishu), 0o600); err != nil {
		t.Fatal(err)
	}
	rec := map[string]any{"id": "r1", "source": "feishu", "speakers": speakers(map[string]any{"name": "林小溪"}, map[string]any{"role": "S1", "name": "Speaker 1"})}
	n := &Namer{Command: []string{"model"}, Concurrency: 1, WindowChars: 1,
		Index: func(context.Context) ([]byte, error) { return []byte(namesIndex), nil },
		call:  replies(map[string]string{"小西": `{"edits":[{"from":"小西","to":"小溪","person":"林小溪"}]}`})}
	rep, ours, err := n.Run(context.Background(), dir, rec, nil)
	if err != nil || !ours {
		t.Fatalf("ours %v err %v", ours, err)
	}
	var segs []map[string]any
	if err := readJSON(filepath.Join(dir, "segments.json"), &segs); err != nil {
		t.Fatal(err)
	}
	if segs[0]["text"] != "我跟小溪说" || segs[0]["raw"] != "我跟小西说" || segs[0]["engine"] != "funasr" || segs[1]["text"] != "明元呢" {
		t.Fatalf("segments %v", segs)
	}
	if b, _ := os.ReadFile(filepath.Join(dir, "feishu-transcript.json")); !bytes.Equal(b, []byte(feishu)) {
		t.Fatal("Feishu's file changed")
	}
	if len(rep.Changes) != 2 || rep.Changes[1].Transcript != "feishu" || rep.Changes[1].After != "小溪你好" || !reflect.DeepEqual(rep.People, []string{}) {
		t.Fatalf("report %+v", rep)
	}
	r, err := BuildRecord(dir, rec)
	if err != nil || r.Turns[0].Text != "小溪你好" {
		t.Fatalf("the ingest and Summarize read the corrected Feishu text: %+v %v", r.Turns, err)
	}

	// 周明远 is resolved on a speaker: a focused run for him.
	rec["speakers"] = speakers(map[string]any{"name": "林小溪"}, map[string]any{"role": "S1", "name": "周明远", "person": "zhou-mingyuan"})
	n.call = replies(map[string]string{"明元": `{"edits":[{"from":"明元","to":"明远","person":"周明远"}]}`})
	rep, _, err = n.Run(context.Background(), dir, rec, []string{"zhou-mingyuan"})
	if err != nil {
		t.Fatal(err)
	}
	if rep.Calls != 2 || len(rep.Changes) != 4 || !reflect.DeepEqual(rep.People, []string{"zhou-mingyuan"}) {
		t.Fatalf("focused: calls %d changes %+v people %v", rep.Calls, rep.Changes, rep.People)
	}
	r, _ = BuildRecord(dir, rec)
	if r.Turns[0].Text != "小溪你好" || r.Turns[1].Text != "明远在" {
		t.Fatalf("both runs' corrections: %+v", r.Turns)
	}
}

// A speaker resolved on an aligned record queues the names job, which runs
// the pass for that person alone and leaves the state; the next tick owes
// nothing.
func TestResolvedSpeakerQueuesANamesPass(t *testing.T) {
	m, f, r := newMem(), &fake{}, &resolver{}
	m.put(t, "rec.a", map[string]any{"id": "a", "state": "aligned", "names": map[string]any{"people": []any{}},
		"speakers": speakers(map[string]any{"role": "S1", "name": "Alice Wong"})})
	st := f.stages(false)
	var focus [][]string
	st.Names = func(_ context.Context, id, dir string, rec map[string]any, fo []string) (Patch, error) {
		focus = append(focus, fo)
		return func(raw map[string]any) error {
			raw["names"] = map[string]any{"people": []any{"alice"}}
			return nil
		}, nil
	}
	d := peopleDrain(t, m, st, r)
	d.Tick(context.Background())
	if !reflect.DeepEqual(focus, [][]string{{"alice"}}) {
		t.Fatalf("names focus %v; calls %q", focus, f.calls)
	}
	rec := m.rec(t, "a")
	if rec["state"] != "aligned" || rec["action"] != nil || rec["stage"] != nil || m.has("q.a") {
		t.Fatalf("record %v", rec)
	}
	d.Tick(context.Background())
	if len(focus) != 1 {
		t.Fatalf("ran again: %v", focus)
	}
}

// A speaker named on the page by an exact index match gets its person with
// no resolve; the tick queues the names pass for it on a record that had one.
func TestPageNamedPersonQueuesANamesPass(t *testing.T) {
	m, f := newMem(), &fake{}
	m.put(t, "rec.b", map[string]any{"id": "b", "state": "ingested", "names": map[string]any{"people": []any{}},
		"speakers": speakers(map[string]any{"role": "S1", "name": "Bob", "person": "bob"})})
	m.put(t, "rec.never", map[string]any{"id": "never", "state": "aligned",
		"speakers": speakers(map[string]any{"role": "S1", "name": "Bob", "person": "bob"})})
	st := f.stages(false)
	var ran []string
	st.Names = func(_ context.Context, id, dir string, rec map[string]any, fo []string) (Patch, error) {
		ran = append(ran, id+":"+strings.Join(fo, ","))
		return func(raw map[string]any) error { raw["names"] = map[string]any{"people": []any{"bob"}}; return nil }, nil
	}
	d := peopleDrain(t, m, st, &resolver{})
	d.Tick(context.Background())
	if !reflect.DeepEqual(ran, []string{"b:bob"}) {
		t.Fatalf("names ran %q", ran)
	}
	if rec := m.rec(t, "b"); rec["state"] != "ingested" || rec["action"] != nil || m.has("q.b") || m.has("q.never") {
		t.Fatalf("record %v", rec)
	}
}
