package drain

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"
)

// resolver is a fake `people resolve`: each name becomes the slug of its
// lower-cased first word, except the ambiguous ones.
type resolver struct {
	mu        sync.Mutex
	calls     []string // "<id> role=name,…"
	ambiguous map[string]bool
	fail      error
}

func (r *resolver) resolve(_ context.Context, id string, _ map[string]any, names map[string]string) ([]Person, error) {
	var got []string
	for role, n := range names {
		got = append(got, role+"="+n)
	}
	sort.Strings(got)
	r.mu.Lock()
	r.calls = append(r.calls, id+" "+strings.Join(got, ","))
	r.mu.Unlock()
	if r.fail != nil {
		return nil, r.fail
	}
	var rows []Person
	for role, n := range names {
		if r.ambiguous[n] {
			rows = append(rows, Person{Role: role, Name: n, Candidates: []string{"notes:a", "notes:b"}})
			continue
		}
		rows = append(rows, Person{Role: role, Name: n, Person: strings.ToLower(strings.Fields(n)[0]), Wiki: "notes"})
	}
	return rows, nil
}

func peopleDrain(t *testing.T, m *mem, st Stages, r *resolver) *Drain {
	st.Resolve = r.resolve
	d := newDrain(m, st, "h:1", &logs{})
	d.State = filepath.Join(t.TempDir(), "state.json")
	return d
}

func speakers(sp ...map[string]any) []any {
	out := make([]any, len(sp))
	for i, s := range sp {
		out[i] = s
	}
	return out
}

func personOf(t *testing.T, m *mem, id, role string) any {
	t.Helper()
	for _, x := range m.rec(t, id)["speakers"].([]any) {
		if e := x.(map[string]any); e["role"] == role {
			return e["person"]
		}
	}
	t.Fatalf("rec.%s has no speaker %s", id, role)
	return nil
}

// Placeholders, a name equal to its role, a deleted record: none fires. A
// real name fires once and its person is written.
func TestNamedSpeakerResolvesOnce(t *testing.T) {
	m, f, r := newMem(), &fake{}, &resolver{}
	m.put(t, "rec.a", map[string]any{"id": "a", "state": "aligned", "speakers": speakers(
		map[string]any{"role": "S1", "name": "Speaker 1"},
		map[string]any{"role": "remote", "name": "remote"},
		map[string]any{"role": "S3", "name": "Alice Wong"},
		map[string]any{"role": "S4", "name": "Bob", "person": "bob"},
	)})
	m.put(t, "rec.gone", map[string]any{"id": "gone", "state": "deleted", "speakers": speakers(map[string]any{"role": "S1", "name": "Carol"})})
	d := peopleDrain(t, m, f.stages(true), r)
	d.Tick(context.Background())
	d.Tick(context.Background())
	if got := strings.Join(r.calls, "; "); got != "a S3=Alice Wong" {
		t.Fatalf("resolve calls %q", got)
	}
	if p := personOf(t, m, "a", "S3"); p != "alice" {
		t.Fatalf("S3 person %v", p)
	}
	if p := personOf(t, m, "a", "S1"); p != nil {
		t.Fatalf("placeholder got person %v", p)
	}
	if rec := m.rec(t, "a"); rec["action"] != nil || m.has("q.a") {
		t.Fatalf("an aligned record is not reingested: %v", rec)
	}
	if len(f.calls) != 0 {
		t.Fatalf("stages ran %q", f.calls)
	}
}

// An ambiguous name is resolved once and waits; renamed, it fires again.
func TestAmbiguousNameDoesNotRefire(t *testing.T) {
	m, f, r := newMem(), &fake{}, &resolver{ambiguous: map[string]bool{"Li": true}}
	m.put(t, "rec.b", map[string]any{"id": "b", "state": "aligned", "speakers": speakers(map[string]any{"role": "S1", "name": "Li"})})
	d := peopleDrain(t, m, f.stages(true), r)
	for range 3 {
		d.Tick(context.Background())
	}
	if len(r.calls) != 1 || personOf(t, m, "b", "S1") != nil {
		t.Fatalf("calls %q, person %v", r.calls, personOf(t, m, "b", "S1"))
	}
	if st := Load(d.State); st.People["b/S1"].Outcome != "ambiguous" {
		t.Fatalf("state %+v", st.People)
	}
	m.put(t, "rec.b", map[string]any{"id": "b", "state": "aligned", "speakers": speakers(map[string]any{"role": "S1", "name": "Li Wei"})})
	d.Tick(context.Background())
	if len(r.calls) != 2 || personOf(t, m, "b", "S1") != "li" {
		t.Fatalf("after rename: calls %q, person %v", r.calls, personOf(t, m, "b", "S1"))
	}
	if st := Load(d.State); len(st.People) != 0 {
		t.Fatalf("a settled name's try stays: %+v", st.People)
	}
}

// A failed resolve waits PeopleRetry, then runs again.
func TestFailedResolveRetriesLater(t *testing.T) {
	m, f, r := newMem(), &fake{}, &resolver{fail: errors.New("lookup timed out")}
	m.put(t, "rec.c", map[string]any{"id": "c", "state": "aligned", "speakers": speakers(map[string]any{"role": "S1", "name": "Dana"})})
	d := peopleDrain(t, m, f.stages(true), r)
	var lines []string
	d.Notify = func(s string) { lines = append(lines, s) }
	d.Tick(context.Background())
	d.Tick(context.Background())
	if len(r.calls) != 1 || len(lines) != 1 {
		t.Fatalf("calls %q, notify %q", r.calls, lines)
	}
	r.fail = nil
	d.Now = func() time.Time { return t0.Add(d.PeopleRetry) }
	d.Tick(context.Background())
	if len(r.calls) != 2 || personOf(t, m, "c", "S1") != "dana" {
		t.Fatalf("after the retry: calls %q", r.calls)
	}
}

// The page writes the record between the drain's read and its swap: the
// swap loses, the drain reads again, and both writes stand.
func TestPageWriteSurvivesThePersonWrite(t *testing.T) {
	m, f, r := newMem(), &fake{}, &resolver{}
	m.put(t, "rec.d", map[string]any{"id": "d", "state": "aligned", "speakers": speakers(
		map[string]any{"role": "S1", "name": "Erin"}, map[string]any{"role": "S2", "name": "Speaker 2"})})
	var once sync.Once
	m.cas = func(key string) {
		if key != "rec.d" {
			return
		}
		once.Do(func() {
			o, _ := m.Record(context.Background(), key)
			rec := m.rec(t, "d")
			rec["labels"] = []any{"planning"}
			rec["speakers"].([]any)[1].(map[string]any)["name"] = "Frank"
			if _, err := m.swap(key, recSchema, rec, o.Version); err != nil {
				t.Errorf("page write: %v", err)
			}
		})
	}
	d := peopleDrain(t, m, f.stages(true), r)
	d.Tick(context.Background())
	rec := m.rec(t, "d")
	if rec["labels"] == nil || personOf(t, m, "d", "S1") != "erin" {
		t.Fatalf("record %v", rec)
	}
	if e := rec["speakers"].([]any)[1].(map[string]any); e["name"] != "Frank" || e["person"] != nil {
		t.Fatalf("the page's rename %v", e)
	}
}

// An ingested record gets its reingest queued, and the same tick runs it
// with the person in the record the ingest reads.
func TestIngestedRecordIsReingested(t *testing.T) {
	m, f, r := newMem(), &fake{}, &resolver{}
	m.put(t, "rec.e", map[string]any{"id": "e", "state": "ingested", "wiki": map[string]any{"page": "old.md", "commit": "0"},
		"speakers": speakers(map[string]any{"role": "S1", "name": "Gil"})})
	st := f.stages(true)
	var seen []any
	ingest := st.Ingest
	st.Ingest = func(ctx context.Context, id, dir string, rec map[string]any, replace bool) (Wiki, error) {
		seen = rec["speakers"].([]any)
		return ingest(ctx, id, dir, rec, replace)
	}
	d := peopleDrain(t, m, st, r)
	d.Tick(context.Background())
	if got := strings.Join(f.calls, ", "); got != "pull e, ingest e" || len(f.ingest) != 1 || !f.ingest[0] {
		t.Fatalf("stages %q, replace %v", got, f.ingest)
	}
	if e := seen[0].(map[string]any); e["person"] != "gil" {
		t.Fatalf("the ingest read %v", seen)
	}
	if rec := m.rec(t, "e"); rec["state"] != "ingested" || rec["action"] != nil || m.has("q.e") {
		t.Fatalf("record %v", rec)
	}
}

// A record under a live claim is left to its job; the resolve runs once
// the claim is gone.
func TestClaimedRecordWaits(t *testing.T) {
	m, f, r := newMem(), &fake{}, &resolver{}
	m.put(t, "rec.g", map[string]any{"id": "g", "state": "processing", "speakers": speakers(map[string]any{"role": "S1", "name": "Hana"}),
		"claim": map[string]any{"by": "other:1", "at": "x", "expires": t0.Add(time.Hour).Format(time.RFC3339)}})
	d := peopleDrain(t, m, f.stages(true), r)
	d.Tick(context.Background())
	if len(r.calls) != 0 {
		t.Fatalf("resolved under a claim: %q", r.calls)
	}
	d.Now = func() time.Time { return t0.Add(2 * time.Hour) }
	d.Tick(context.Background())
	if len(r.calls) != 1 || personOf(t, m, "g", "S1") != "hana" {
		t.Fatalf("after the claim: %q", r.calls)
	}
}

// record.json carries {name, person} for a speaker with a person page and
// a plain name for every other.
func TestRecordJSONCarriesThePerson(t *testing.T) {
	dir := t.TempDir()
	writeJSON(t, filepath.Join(dir, "segments.json"), []map[string]any{{"speaker": "S1", "start_s": 1.0, "text": "hi"}})
	r, err := BuildRecord(dir, map[string]any{"id": "x", "source": "file", "speakers": speakers(
		map[string]any{"role": "S1", "name": "Alice Wong", "person": "alice"},
		map[string]any{"role": "S2", "name": "Speaker 2"},
		map[string]any{"role": "S3", "person": "bob"},
	)})
	if err != nil {
		t.Fatal(err)
	}
	b, _ := json.Marshal(r)
	var got struct {
		Speakers map[string]any `json:"speakers"`
		ID       string         `json:"id"`
	}
	json.Unmarshal(b, &got)
	want := map[string]any{"S1": map[string]any{"name": "Alice Wong", "person": "alice"}, "S2": "Speaker 2", "S3": map[string]any{"person": "bob"}}
	if !reflect.DeepEqual(got.Speakers, want) || got.ID != "x" {
		t.Fatalf("record.json %s", b)
	}
}

// Resolve runs `<command> people resolve <record.json> --out <dir>
// --commit` over the named speakers only and reads its people.json.
func TestResolveRunsTheCommand(t *testing.T) {
	bin := filepath.Join(t.TempDir(), "ingest")
	script := `#!/bin/sh
[ "$1 $2 $4 $6" = "people resolve --out --commit" ] || { echo "args: $*" >&2; exit 2; }
cp "$3" "$5/seen.json"
echo '[{"role":"S1","name":"Alice","person":"alice","wiki":"notes","page":"p.md","created":true}]' > "$5/people.json"
`
	if err := os.WriteFile(bin, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	in := &Ingester{Command: []string{bin}, People: t.TempDir(), Now: func() time.Time { return t0 }}
	rows, err := in.Resolve(context.Background(), "r1", map[string]any{"source": "mac", "title": "t"}, map[string]string{"S1": "Alice"})
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 || rows[0].Person != "alice" || !rows[0].Created {
		t.Fatalf("rows %+v", rows)
	}
	var seen map[string]any
	readJSON(filepath.Join(in.People, "r1", "20200311-064000", "seen.json"), &seen)
	if sp := seen["speakers"].(map[string]any); len(sp) != 1 || sp["S1"] != "Alice" || seen["id"] != "r1" {
		t.Fatalf("record.json %v", seen)
	}
}
