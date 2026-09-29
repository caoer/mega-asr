package labels

import (
	"context"
	"encoding/json"
	"errors"
	"maps"
	"reflect"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/caoer/mega-asr/internal/pages"
)

type mem struct {
	mu   sync.Mutex
	objs map[string]pages.Object
	cas  func(key string) // before each CAS, unlocked
}

func newMem() *mem { return &mem{objs: map[string]pages.Object{}} }

func (m *mem) put(key string, data any) {
	b, _ := json.Marshal(data)
	m.mu.Lock()
	defer m.mu.Unlock()
	o := m.objs[key]
	m.objs[key] = pages.Object{Key: key, Value: pages.Value{Schema: recSchema, Data: b}, Version: o.Version + 1}
}

func (m *mem) get(t *testing.T, key string) map[string]any {
	t.Helper()
	m.mu.Lock()
	defer m.mu.Unlock()
	var raw map[string]any
	if err := json.Unmarshal(m.objs[key].Value.Data, &raw); err != nil {
		t.Fatalf("%s: %v", key, err)
	}
	return raw
}

func (m *mem) List(_ context.Context, prefix string) ([]pages.Object, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []pages.Object
	for k, o := range m.objs {
		if strings.HasPrefix(k, prefix) {
			out = append(out, o)
		}
	}
	return out, nil
}

func (m *mem) Record(_ context.Context, key string) (pages.Object, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	o, ok := m.objs[key]
	if !ok {
		return o, &pages.Error{Status: 404, Code: "no_such_object"}
	}
	return o, nil
}

func (m *mem) CAS(_ context.Context, key, schema string, data any, version int) (int, error) {
	if m.cas != nil {
		m.cas(key)
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if o := m.objs[key]; o.Version != version {
		return 0, &pages.Error{Status: 409, Code: "version_conflict"}
	}
	b, _ := json.Marshal(data)
	m.objs[key] = pages.Object{Key: key, Value: pages.Value{Schema: schema, Data: b}, Version: version + 1}
	return version + 1, nil
}

func labelsOf(t *testing.T, m *mem, id string) []string {
	t.Helper()
	var out []string
	for _, x := range m.get(t, "rec."+id)["labels"].([]any) {
		out = append(out, x.(string))
	}
	return out
}

// apiary is the closed list the store tests start from: two meeting types,
// with one entry of each other kind between and after them.
func apiary() List {
	return List{Closed: []Entry{
		{Name: "巡箱", Kind: "type"},
		{Name: "蜡烛坊", Kind: "project"},
		{Name: "摇蜜日", Kind: "type"},
		{Name: "越冬", Kind: "topic"},
		{Name: "Alice Example", Kind: "person"},
	}, Questions: []Question{}}
}

var at = time.Date(2020, 6, 9, 7, 45, 12, 0, time.UTC)

const stamp = "2020-06-09T07:45:12Z"

func newStore(reg Registry) *Store {
	return &Store{Reg: reg, By: "labels-test", Now: func() time.Time { return at }}
}

// apiaryStore is a registry holding apiary() and five records: three
// labelled, one pending, one deleted.
func apiaryStore(t *testing.T) (*mem, *Store) {
	t.Helper()
	m := newMem()
	m.put(Key, apiary())
	m.put("rec.h-11", map[string]any{"id": "h-11", "labels": []string{"巡箱", "蜂螨", "蜡烛坊"}})
	m.put("rec.h-12", map[string]any{"id": "h-12", "labels": []string{"蜂螨", "药剂"}})
	m.put("rec.h-13", map[string]any{"id": "h-13", "labels": []string{"越冬", "摇蜜日"}})
	m.put("rec.h-14", map[string]any{"id": "h-14"})
	m.put("rec.h-15", map[string]any{"id": "h-15", "state": "deleted", "labels": []string{"蜂螨"}})
	return m, newStore(m)
}

func closedOf(t *testing.T, reg Registry) List {
	t.Helper()
	l, _, err := Read(context.Background(), reg)
	if err != nil {
		t.Fatal(err)
	}
	return l
}

func logged(t *testing.T, m *mem) []Change {
	t.Helper()
	cs, err := Log(context.Background(), m)
	if err != nil {
		t.Fatal(err)
	}
	return cs
}

func versions(m *mem) map[string]int {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := map[string]int{}
	for k, o := range m.objs {
		out[k] = o.Version
	}
	return out
}

func wantErr(t *testing.T, err error, want string) {
	t.Helper()
	if err == nil || err.Error() != want {
		t.Fatalf("error %v, want %q", err, want)
	}
}

// flaky fails every CAS on key while failing is set.
type flaky struct {
	*mem
	key     string
	failing bool
}

var errFlaky = errors.New("registry offline")

func (f *flaky) CAS(ctx context.Context, key, schema string, data any, version int) (int, error) {
	if f.failing && key == f.key {
		return 0, errFlaky
	}
	return f.mem.CAS(ctx, key, schema, data, version)
}

func TestClean(t *testing.T) {
	l := apiary()
	for _, c := range []struct{ in, want []string }{
		{[]string{"摇蜜日", "花期", "巡箱", "蜡烛坊"}, []string{"摇蜜日", "花期", "蜡烛坊"}},
		{[]string{" 蜂螨 ", "蜂螨", "蜂  胶"}, []string{"蜂螨", "蜂 胶"}},
		{[]string{"越冬", "Alice Example", "巡箱", "巡箱"}, []string{"越冬", "Alice Example", "巡箱"}},
		{[]string{"", "   "}, []string{}},
		{nil, []string{}},
	} {
		if got := Clean(c.in, l); !reflect.DeepEqual(got, c.want) {
			t.Errorf("Clean(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

func TestTypeOf(t *testing.T) {
	l := apiary()
	if got := TypeOf([]string{"花期", "摇蜜日", "巡箱"}, l); got != "摇蜜日" {
		t.Errorf("the first closed type: got %q", got)
	}
	if got := TypeOf([]string{"越冬", "蜡烛坊"}, l); got != "" {
		t.Errorf("closed entries of other kinds are no type: got %q", got)
	}
	if got := TypeOf(nil, l); got != "" {
		t.Errorf("no labels: got %q", got)
	}
	if got := l.Types(); !slices.Equal(got, []string{"巡箱", "摇蜜日"}) {
		t.Errorf("Types = %q", got)
	}
}

func TestViolations(t *testing.T) {
	recs := []Rec{
		{ID: "h-21", Labels: []string{"巡箱", "分蜂", "摇蜜日"}},
		{ID: "h-22", Labels: []string{"巡箱"}},
		{ID: "h-23"},
	}
	want := []Violation{{ID: "h-21", Labels: recs[0].Labels, Types: []string{"巡箱", "摇蜜日"}}}
	if got := Violations(recs, apiary()); !reflect.DeepEqual(got, want) {
		t.Errorf("Violations = %+v", got)
	}
	if got := Violations(recs[1:], apiary()); got != nil {
		t.Errorf("one type or none breaks nothing: %+v", got)
	}
}

func TestStripType(t *testing.T) {
	types := []string{"验蜂", "取蜜"}
	for _, c := range []struct{ title, want string }{
		{"\t验蜂/蜂群合并  ", "蜂群合并"},
		{"蜂王台【验蜂】", "蜂王台"},
		{"取蜜", "取蜜"},
		{"(验蜂)(取蜜) 蜂王台", "蜂王台"},
		{"取蜜机保养 ｜ 取蜜", "取蜜机保养"},
		{"蜂房消毒 - 验蜂", "蜂房消毒"},
	} {
		if got := StripType(c.title, types); got != c.want {
			t.Errorf("StripType(%q) = %q, want %q", c.title, got, c.want)
		}
	}
}

func TestTitleProblem(t *testing.T) {
	// 验蜂复盘会 holds the kind word 复盘会: the longer type is matched first.
	types := []string{"验蜂", "取蜜", "验蜂复盘会"}
	const (
		bare    = "a bare series name "
		kind    = "names the meeting's kind: "
		nothing = "says nothing of what was covered"
		met     = "names no subject, only that the team met and reported"
	)
	for _, c := range []struct{ title, want string }{
		{"Frame spacing in hot weather", ""},
		{"运营", met},
		{"验蜂复盘会", bare + "(验蜂复盘会)"},
		{"蜂房消毒会议", kind + "会议"},
		{"6 月份", nothing},
		{"条线", met},
		{"蜂箱称重STAND-UP", kind + "stand-up"},
		{"第 7 周取蜜", bare + "(取蜜)"},
		{"蜂王台出现后的处理", ""},
		{"多", met},
		{"分享会上的蜂房消毒办法", kind + "分享会"},
		{"Update", met},
		{"蜂箱验蜂记录", kind + "验蜂"},
		{"研发", met},
		{"", ""},
	} {
		if got := TitleProblem(c.title, types); got != c.want {
			t.Errorf("TitleProblem(%q) = %q, want %q", c.title, got, c.want)
		}
	}
}

func TestNamesSubject(t *testing.T) {
	for _, c := range []struct {
		want     bool
		title    string
		subjects []string
	}{
		{true, "送检FOULBROOD样本", []string{"如何防控Foulbrood蔓延"}},
		{false, "蜡封蜜脾", []string{"蜜脾摇取"}},
		{true, "蜂房消毒办法", nil},
		{false, "待办安排", []string{"待办安排"}},
		{true, "蜂王台出现后的处理", []string{"蜂房消毒", "蜂王台出现"}},
		{false, "第 3 周", []string{"第 3 周"}},
	} {
		if got := NamesSubject(c.title, c.subjects); got != c.want {
			t.Errorf("NamesSubject(%q, %q) = %v", c.title, c.subjects, got)
		}
	}
}

func TestRead(t *testing.T) {
	ctx := context.Background()
	m := newMem()
	l, v, err := Read(ctx, m)
	if err != nil || v != 0 || l.Closed == nil || len(l.Closed) != 0 || l.Questions == nil {
		t.Fatalf("absent: %+v v%d %v", l, v, err)
	}
	m.put(Key, map[string]any{"closed": []Entry{{Name: "巡箱", Kind: "type"}}})
	l, v, err = Read(ctx, m)
	if err != nil || v != 1 || l.Questions == nil || !l.IsType("巡箱") || l.IsType("越冬") {
		t.Fatalf("stored: %+v v%d %v", l, v, err)
	}
	if _, err := l.Question("q1"); err == nil {
		t.Fatal("a question on an empty list")
	}
	m.put(Key, "蜂箱编号重排")
	if _, _, err := Read(ctx, m); err == nil || !strings.HasPrefix(err.Error(), Key+": ") {
		t.Fatalf("a record that is no list: %v", err)
	}
}

func TestInit(t *testing.T) {
	ctx := context.Background()
	m := newMem()
	dry := newStore(m)
	dry.DryRun = true
	if _, made, err := dry.Init(ctx); err != nil || !made {
		t.Fatalf("dry run: %v %v", made, err)
	}
	if len(versions(m)) != 0 {
		t.Fatal("a dry run wrote")
	}
	s := newStore(m)
	l, made, err := s.Init(ctx)
	if err != nil || !made || l.UpdatedBy != "labels-test" || l.UpdatedAt != stamp || len(l.Closed) != 0 {
		t.Fatalf("Init: %+v %v %v", l, made, err)
	}
	if _, made, err := s.Init(ctx); err != nil || made {
		t.Fatalf("second Init: %v %v", made, err)
	}
	log := logged(t, m)
	if len(log) != 1 || !slices.Equal(log[0].Op, []string{"init"}) || log[0].By != "labels-test" || log[0].At != stamp ||
		!strings.HasPrefix(log[0].Key, "labellog.20200609T074512.000000Z.") {
		t.Fatalf("log: %+v", log)
	}
	if loc := (&Store{}).now().Location(); loc != time.UTC {
		t.Errorf("the clock's zone: %v", loc)
	}
}

func TestInitLosesRace(t *testing.T) {
	m := newMem()
	m.cas = func(key string) {
		if key == Key {
			m.cas = nil
			m.put(Key, List{Closed: []Entry{{Name: "摇蜜日", Kind: "type"}}, Questions: []Question{}})
		}
	}
	l, made, err := newStore(m).Init(context.Background())
	if err != nil || made || !l.IsType("摇蜜日") {
		t.Fatalf("Init after another writer: %+v %v %v", l, made, err)
	}
	if len(logged(t, m)) != 0 {
		t.Fatal("the losing Init logged")
	}
}

func TestRecords(t *testing.T) {
	m := newMem()
	m.put("rec.h-32", map[string]any{"id": "h-32", "title": "蜂具清点", "display_title": "清点蜂具",
		"summary": "  蜂箱编号重排\n其余两项", "labels": []string{"分蜂", " 花期 ", ""}})
	m.put("rec.h-31", map[string]any{"id": "h-31"})
	m.put("rec.h-33", map[string]any{"id": "h-33", "state": "deleted", "labels": []string{"分蜂"}})
	m.objs["rec.h-34"] = pages.Object{Key: "rec.h-34", Value: pages.Value{Data: []byte("{")}}
	m.put("labellog.x", map[string]any{})
	recs, err := Records(context.Background(), m)
	if err != nil || len(recs) != 2 {
		t.Fatalf("Records: %+v %v", recs, err)
	}
	a, b := recs[0], recs[1]
	if a.ID != "h-31" || a.Labelled || a.Labels != nil || a.Version != 1 {
		t.Errorf("pending record: %+v", a)
	}
	if b.Key != "rec.h-32" || !b.Labelled || b.Title != "蜂具清点" || b.DisplayTitle != "清点蜂具" || b.Gist != "蜂箱编号重排" {
		t.Errorf("labelled record: %+v", b)
	}
	if got := Counts(recs); !maps.Equal(got, map[string]int{"分蜂": 1, "花期": 1}) {
		t.Errorf("Counts = %v", got)
	}
}

func TestVocab(t *testing.T) {
	m := newMem()
	m.put(Key, apiary())
	m.put("rec.h-41", map[string]any{"id": "h-41", "labels": []string{"巡箱", "蜂胶", "分蜂", "蜡烛坊"}})
	m.put("rec.h-42", map[string]any{"id": "h-42", "labels": []string{"分蜂", "蜜源"}})
	m.put("rec.h-43", map[string]any{"id": "h-43", "labels": []string{"巢础", "蜂胶", "分蜂"}})
	v, err := ReadVocab(context.Background(), m)
	if err != nil {
		t.Fatal(err)
	}
	open := []string{"分蜂", "蜂胶", "巢础", "蜜源"}
	if got := v.Open(); !slices.Equal(got, open) {
		t.Errorf("Open = %q", got)
	}
	types, others := v.Offer()
	if !slices.Equal(types, []string{"巡箱", "摇蜜日"}) || !slices.Equal(others, append([]string{"蜡烛坊", "越冬", "Alice Example"}, open...)) {
		t.Errorf("Offer = %q, %q", types, others)
	}
}

func TestParseRefuses(t *testing.T) {
	for _, c := range []struct {
		argv []string
		want string
	}{
		{[]string{"demote"}, "demote NAME"},
		{[]string{"merge", "蜂胶", "--into"}, "merge FROM... --into TO"},
		{[]string{"split", "蜂螨", "h-11"}, `split: "h-11" is not ID=A,B`},
		{nil, "no label command"},
		{[]string{"promote", "分蜂", "season"}, "promote NAME KIND (type | project | person | topic)"},
		{[]string{"rename", "蜂胶", "\t"}, "rename: an empty label"},
		{[]string{"set"}, "set ID [LABEL...]"},
		{[]string{"merge", "--into", "蜂胶"}, "merge FROM... --into TO"},
		{[]string{"split", "蜂螨"}, "split FROM ID=A,B ..."},
		{[]string{"rename", "蜂胶"}, "rename OLD NEW"},
		{[]string{"split", "蜂螨", "rec.=花期"}, `split: "rec.=花期" is not ID=A,B`},
		{[]string{"merge", " ", "--into", "蜂胶"}, "merge: an empty label"},
		{[]string{"reorder", "蜂胶"}, `"reorder" is not a label command`},
		{[]string{"set", "h-11", " "}, "set: an empty label"},
		{[]string{"promote", "", "topic"}, "promote: an empty label"},
	} {
		_, err := parse(c.argv)
		if err == nil || err.Error() != c.want {
			t.Errorf("parse(%q): %v, want %q", c.argv, err, c.want)
		}
	}
}

func TestParseSplit(t *testing.T) {
	o, err := parse([]string{"split", "蜂螨", " rec.h-11 = 瓦螨, ,小蜂螨", "h-12="})
	if err != nil {
		t.Fatal(err)
	}
	want := map[string][]string{"h-11": {"瓦螨", "小蜂螨"}, "h-12": nil}
	if !slices.Equal(o.from, []string{"蜂螨"}) || !reflect.DeepEqual(o.assign, want) {
		t.Errorf("split: from %q assign %q", o.from, o.assign)
	}
}

func TestConsent(t *testing.T) {
	l := apiary()
	for _, c := range []struct {
		argv []string
		want string
	}{
		{[]string{"demote", "蜡烛坊"}, "demote changes the closed list"},
		{[]string{"split", "药剂", "h-12=蜂胶,越冬"}, `split names the closed label "越冬"`},
		{[]string{"set", "h-13"}, ""},
		{[]string{"merge", "瓦螨", "小蜂螨", "--into", "螨害"}, ""},
		{[]string{"promote", "药剂", "project"}, "promote changes the closed list"},
		{[]string{"rename", "摇蜜日", "取蜜日"}, `rename names the closed label "摇蜜日"`},
		{[]string{"merge", "花期", "--into", "巡箱"}, `merge names the closed label "巡箱"`},
	} {
		o, err := parse(c.argv)
		if err != nil {
			t.Fatal(err)
		}
		if got := o.consent(l); got != c.want {
			t.Errorf("consent(%q) = %q, want %q", c.argv, got, c.want)
		}
	}
}

func TestRunNeedsConsent(t *testing.T) {
	m, s := apiaryStore(t)
	before := versions(m)
	_, err := s.Run(context.Background(), []string{"merge", "蜂螨", "--into", "越冬"}, "", "")
	var ce *ConsentError
	if !errors.As(err, &ce) || ce.Why != `merge names the closed label "越冬"` {
		t.Fatalf("Run: %v", err)
	}
	if !strings.HasPrefix(err.Error(), `needs consent (merge names the closed label "越冬"): `) {
		t.Errorf("message: %s", err)
	}
	if !maps.Equal(versions(m), before) {
		t.Error("the refusal wrote")
	}
}

func TestRenameOpenLabel(t *testing.T) {
	m, s := apiaryStore(t)
	c, err := s.Run(context.Background(), []string{"rename", "蜂螨", "瓦螨"}, "按图鉴的写法", "")
	if err != nil {
		t.Fatal(err)
	}
	want := []RecChange{
		{ID: "h-11", Before: []string{"巡箱", "蜂螨", "蜡烛坊"}, After: []string{"巡箱", "瓦螨", "蜡烛坊"}},
		{ID: "h-12", Before: []string{"蜂螨", "药剂"}, After: []string{"瓦螨", "药剂"}},
	}
	if !reflect.DeepEqual(c.Records, want) || c.ClosedAfter != nil {
		t.Fatalf("change: %+v", c)
	}
	if got := labelsOf(t, m, "h-15"); !slices.Equal(got, []string{"蜂螨"}) {
		t.Errorf("a deleted record was relabelled: %q", got)
	}
	log := logged(t, m)
	if len(log) != 1 || log[0].Reason != "按图鉴的写法" || log[0].At != stamp || !slices.Equal(log[0].Op, []string{"rename", "蜂螨", "瓦螨"}) ||
		!reflect.DeepEqual(log[0].Records, want) {
		t.Errorf("log: %+v", log)
	}
}

func TestMergeOpenLabels(t *testing.T) {
	m, s := apiaryStore(t)
	if _, err := s.Run(context.Background(), []string{"merge", "蜂螨", "药剂", "--into", "螨害"}, "", ""); err != nil {
		t.Fatal(err)
	}
	if got := labelsOf(t, m, "h-11"); !slices.Equal(got, []string{"巡箱", "螨害", "蜡烛坊"}) {
		t.Errorf("h-11: %q", got)
	}
	if got := labelsOf(t, m, "h-12"); !slices.Equal(got, []string{"螨害"}) {
		t.Errorf("h-12 keeps one of the merged pair: %q", got)
	}
}

func TestMergeClosed(t *testing.T) {
	ctx := context.Background()
	t.Run("closed into open renames the entry", func(t *testing.T) {
		m, s := apiaryStore(t)
		c, err := s.Run(ctx, []string{"rename", "蜡烛坊", "椴树蜜"}, "", "q4")
		if err != nil {
			t.Fatal(err)
		}
		if l := closedOf(t, m); l.Closed[1] != (Entry{Name: "椴树蜜", Kind: "project"}) || len(l.Closed) != 5 {
			t.Errorf("closed: %+v", l.Closed)
		}
		if !reflect.DeepEqual(c.ClosedBefore, apiary().Closed) || len(c.ClosedAfter) != 5 {
			t.Errorf("change: %+v", c)
		}
		if got := labelsOf(t, m, "h-11"); !slices.Equal(got, []string{"巡箱", "蜂螨", "椴树蜜"}) {
			t.Errorf("h-11: %q", got)
		}
		if log := logged(t, m); len(log) != 1 || log[0].Question != "q4" {
			t.Errorf("log: %+v", log)
		}
	})
	t.Run("closed into closed drops the entry", func(t *testing.T) {
		m, s := apiaryStore(t)
		m.put("rec.h-16", map[string]any{"id": "h-16", "labels": []string{"摇蜜日", "分蜂", "巡箱"}})
		if _, err := s.Run(ctx, []string{"merge", "摇蜜日", "--into", "巡箱"}, "", "q5"); err != nil {
			t.Fatal(err)
		}
		if got := closedOf(t, m).Types(); !slices.Equal(got, []string{"巡箱"}) {
			t.Errorf("types: %q", got)
		}
		if got := labelsOf(t, m, "h-13"); !slices.Equal(got, []string{"越冬", "巡箱"}) {
			t.Errorf("h-13: %q", got)
		}
		if got := labelsOf(t, m, "h-16"); !slices.Equal(got, []string{"巡箱", "分蜂"}) {
			t.Errorf("h-16 keeps one type: %q", got)
		}
	})
	t.Run("two closed into open keep the first", func(t *testing.T) {
		m, s := apiaryStore(t)
		if _, err := s.Run(ctx, []string{"merge", "蜡烛坊", "越冬", "--into", "蜜源"}, "", "q6"); err != nil {
			t.Fatal(err)
		}
		want := []Entry{{"巡箱", "type"}, {"蜜源", "project"}, {"摇蜜日", "type"}, {"Alice Example", "person"}}
		if got := closedOf(t, m).Closed; !slices.Equal(got, want) {
			t.Errorf("closed: %+v", got)
		}
		if got := labelsOf(t, m, "h-13"); !slices.Equal(got, []string{"蜜源", "摇蜜日"}) {
			t.Errorf("h-13: %q", got)
		}
	})
}

func TestSplit(t *testing.T) {
	ctx := context.Background()
	m, s := apiaryStore(t)
	before := versions(m)
	_, err := s.Run(ctx, []string{"split", "蜂螨", "h-11=瓦螨"}, "", "")
	wantErr(t, err, "split 蜂螨: every record carrying it needs its labels; missing h-12")
	if !maps.Equal(versions(m), before) {
		t.Fatal("a refused split wrote")
	}
	c, err := s.Run(ctx, []string{"split", "蜂螨", "h-12=", "rec.h-11=瓦螨,小蜂螨", "h-14=花期"}, "", "")
	if err != nil {
		t.Fatal(err)
	}
	if len(c.Records) != 2 || c.ClosedAfter != nil {
		t.Errorf("change: %+v", c)
	}
	if got := labelsOf(t, m, "h-11"); !slices.Equal(got, []string{"巡箱", "瓦螨", "小蜂螨", "蜡烛坊"}) {
		t.Errorf("h-11: %q", got)
	}
	if got := labelsOf(t, m, "h-12"); !slices.Equal(got, []string{"药剂"}) {
		t.Errorf("h-12: %q", got)
	}
	if _, ok := m.get(t, "rec.h-14")["labels"]; ok {
		t.Error("h-14 does not carry the split label, and was labelled")
	}
	c, err = s.Run(ctx, []string{"split", "越冬", "h-13=蜜源"}, "", "q2")
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := closedOf(t, m).Find("越冬"); ok || len(c.ClosedAfter) != 4 {
		t.Errorf("a split closed label stays closed: %+v", c)
	}
	if got := labelsOf(t, m, "h-13"); !slices.Equal(got, []string{"蜜源", "摇蜜日"}) {
		t.Errorf("h-13: %q", got)
	}
}

func TestSet(t *testing.T) {
	ctx := context.Background()
	m, s := apiaryStore(t)
	c, err := s.Run(ctx, []string{"set", "rec.h-14", "摇蜜日", "分蜂", "巡箱"}, "", "")
	if err != nil {
		t.Fatal(err)
	}
	want := []RecChange{{ID: "h-14", Before: []string{}, After: []string{"摇蜜日", "分蜂"}}}
	if !reflect.DeepEqual(c.Records, want) {
		t.Errorf("set a pending record: %+v", c.Records)
	}
	if c, err := s.Run(ctx, []string{"set", "h-11", "巡箱", "蜂螨", "蜡烛坊"}, "", ""); err != nil || len(c.Records) != 0 {
		t.Errorf("the same labels: %+v %v", c, err)
	}
	_, err = s.Run(ctx, []string{"set", "h-15", "分蜂"}, "", "")
	wantErr(t, err, "set: no live record h-15")
	c, err = s.Run(ctx, []string{"set", "h-12"}, "", "")
	if err != nil || len(c.Records) != 1 || len(c.Records[0].After) != 0 || len(labelsOf(t, m, "h-12")) != 0 {
		t.Errorf("no labels: %+v %v", c, err)
	}
	if n := len(logged(t, m)); n != 2 {
		t.Errorf("%d log entries, want 2", n)
	}
}

func TestPromoteDemote(t *testing.T) {
	ctx := context.Background()
	m, s := apiaryStore(t)
	c, err := s.Run(ctx, []string{"promote", "糖浆", "project"}, "", "q2")
	if err != nil {
		t.Fatal(err)
	}
	want := append(apiary().Closed, Entry{Name: "糖浆", Kind: "project"})
	if !slices.Equal(closedOf(t, m).Closed, want) || !slices.Equal(c.ClosedAfter, want) {
		t.Fatalf("promote: %+v", c)
	}
	if c, err := s.Run(ctx, []string{"promote", "糖浆", "project"}, "", "q3"); err != nil || c.ClosedAfter != nil {
		t.Errorf("promote again: %+v %v", c, err)
	}
	_, err = s.Run(ctx, []string{"promote", "糖浆", "topic"}, "", "q3")
	wantErr(t, err, `promote: "糖浆" is already closed, as project`)
	if _, err := s.Run(ctx, []string{"demote", "Alice Example"}, "", "q4"); err != nil {
		t.Fatal(err)
	}
	if _, ok := closedOf(t, m).Find("Alice Example"); ok {
		t.Error("Alice Example still closed")
	}
	_, err = s.Run(ctx, []string{"demote", "蜜源"}, "", "q5")
	wantErr(t, err, `demote: "蜜源" is not on the closed list`)
	m.objs["labellog.0"] = pages.Object{Key: "labellog.0", Value: pages.Value{Data: []byte("[")}}
	log := logged(t, m)
	if len(log) != 2 || log[0].Op[0] != "promote" || log[1].Op[0] != "demote" || log[1].Question != "q4" {
		t.Errorf("log: %+v", log)
	}
}

func TestDryRun(t *testing.T) {
	ctx := context.Background()
	m, s := apiaryStore(t)
	s.DryRun = true
	before := versions(m)
	c, err := s.Run(ctx, []string{"rename", "蜂螨", "瓦螨"}, "", "")
	if err != nil || len(c.Records) != 2 || !slices.Equal(c.Records[1].After, []string{"瓦螨", "药剂"}) || c.At != stamp {
		t.Errorf("rename: %+v %v", c, err)
	}
	c, err = s.Run(ctx, []string{"promote", "蜂胶", "project"}, "", "q1")
	if err != nil || len(c.ClosedAfter) != 6 {
		t.Errorf("promote: %+v %v", c, err)
	}
	if !maps.Equal(versions(m), before) {
		t.Error("a dry run wrote")
	}
}

func TestRelabelRetries(t *testing.T) {
	ctx := context.Background()
	t.Run("a writer got there first", func(t *testing.T) {
		m, s := apiaryStore(t)
		m.cas = func(key string) {
			if key == "rec.h-12" {
				m.cas = nil
				m.put(key, map[string]any{"id": "h-12", "labels": []string{"蜂胶", "蜂螨"}})
			}
		}
		c, err := s.Run(ctx, []string{"rename", "蜂螨", "瓦螨"}, "", "")
		if err != nil {
			t.Fatal(err)
		}
		want := RecChange{ID: "h-12", Before: []string{"蜂胶", "蜂螨"}, After: []string{"蜂胶", "瓦螨"}}
		if len(c.Records) != 2 || !reflect.DeepEqual(c.Records[1], want) {
			t.Errorf("change: %+v", c.Records)
		}
	})
	t.Run("the record was deleted", func(t *testing.T) {
		m, s := apiaryStore(t)
		m.cas = func(key string) {
			if key == "rec.h-12" {
				m.cas = nil
				m.put(key, map[string]any{"id": "h-12", "state": "deleted"})
			}
		}
		c, err := s.Run(ctx, []string{"rename", "蜂螨", "瓦螨"}, "", "")
		if err != nil || len(c.Records) != 1 || c.Records[0].ID != "h-11" {
			t.Errorf("change: %+v %v", c, err)
		}
		if _, ok := m.get(t, "rec.h-12")["labels"]; ok {
			t.Error("a deleted record was relabelled")
		}
	})
}

func TestListRetries(t *testing.T) {
	m, s := apiaryStore(t)
	m.cas = func(key string) {
		if key == Key {
			m.cas = nil
			l := apiary()
			l.Closed = append(l.Closed, Entry{Name: "蜜源", Kind: "project"})
			m.put(Key, l)
		}
	}
	if _, err := s.Run(context.Background(), []string{"promote", "药剂", "project"}, "", "q1"); err != nil {
		t.Fatal(err)
	}
	l := closedOf(t, m)
	if _, ok := l.Find("蜜源"); !ok || !slices.Contains(l.Closed, Entry{Name: "药剂", Kind: "project"}) || l.UpdatedBy != "labels-test" {
		t.Errorf("closed: %+v", l)
	}
}

func TestQuestions(t *testing.T) {
	ctx := context.Background()
	m, s := apiaryStore(t)
	_, err := s.Ask(ctx, " ", nil)
	wantErr(t, err, "ask: the question's text is empty")
	_, err = s.Ask(ctx, "今年还订巢础吗？", []string{"reorder"})
	wantErr(t, err, `ask: the proposal: "reorder" is not a label command`)
	free, err := s.Ask(ctx, "今年还订巢础吗？", nil)
	if err != nil || free.ID != "q1" || free.Status != "open" || free.Asked != stamp {
		t.Fatalf("free question: %+v %v", free, err)
	}
	drop := []string{"demote", "Alice Example"}
	q, err := s.Ask(ctx, "Alice Example 已离开协会，名单上还留着吗？", drop)
	if err != nil || q.ID != "q2" {
		t.Fatalf("question: %+v %v", q, err)
	}
	_, err = s.Ask(ctx, "再确认一次？", drop)
	wantErr(t, err, "ask: q2 already asks this, still open")
	if err := s.SetAsk(ctx, "q2", "tg-5501"); err != nil {
		t.Fatal(err)
	}
	wantErr(t, s.SetAsk(ctx, "q9", "tg-5501"), "no question q9")
	wantErr(t, s.Answer(ctx, "q2", "perhaps", ""), `answer: "perhaps" is not approved, declined or expired`)
	wantErr(t, s.Answer(ctx, "q9", "declined", ""), "no question q9")
	if err := s.Answer(ctx, "q2", "approved", "照办"); err != nil {
		t.Fatal(err)
	}
	wantErr(t, s.Answer(ctx, "q2", "declined", ""), "answer: q2 is approved, not open")
	if again, err := s.Ask(ctx, "再确认一次？", drop); err != nil || again.ID != "q3" {
		t.Errorf("asking again once q2 is answered: %+v %v", again, err)
	}
	got := closedOf(t, m).Questions[1]
	want := Question{ID: "q2", Asked: stamp, Text: "Alice Example 已离开协会，名单上还留着吗？", Proposal: drop, Ask: "tg-5501",
		Status: "approved", Answer: "照办", Answered: stamp}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("q2 = %+v", got)
	}
	var ops []string
	for _, c := range logged(t, m) {
		ops = append(ops, c.Question+" "+strings.Join(c.Op, " "))
	}
	if !slices.Equal(ops, []string{"q1 ask", "q2 ask demote Alice Example", "q2 question q2 approved", "q3 ask demote Alice Example"}) {
		t.Errorf("log: %q", ops)
	}
}

func TestApply(t *testing.T) {
	ctx := context.Background()
	m, s := apiaryStore(t)
	if _, err := s.Ask(ctx, "今年还订巢础吗？", nil); err != nil {
		t.Fatal(err)
	}
	_, err := s.Apply(ctx, "q1", "")
	wantErr(t, err, "apply: q1 is open, not approved")
	if err := s.Answer(ctx, "q1", "approved", "订"); err != nil {
		t.Fatal(err)
	}
	_, err = s.Apply(ctx, "q1", "")
	wantErr(t, err, "apply: q1 proposes no label command")
	_, err = s.Apply(ctx, "q9", "")
	wantErr(t, err, "no question q9")

	if _, err := s.Ask(ctx, "Alice Example 已离开协会，名单上还留着吗？", []string{"demote", "Alice Example"}); err != nil {
		t.Fatal(err)
	}
	if err := s.Answer(ctx, "q2", "approved", "照办"); err != nil {
		t.Fatal(err)
	}
	s.DryRun = true
	c, err := s.Apply(ctx, "q2", "")
	if err != nil || len(c.ClosedAfter) != 4 {
		t.Fatalf("dry run: %+v %v", c, err)
	}
	if l := closedOf(t, m); len(l.Closed) != 5 || l.Questions[1].Status != "approved" {
		t.Fatalf("a dry apply wrote: %+v", l)
	}
	s.DryRun = false
	c, err = s.Apply(ctx, "q2", "")
	if err != nil || c.Reason != "approved: 照办" || c.Question != "q2" {
		t.Fatalf("apply: %+v %v", c, err)
	}
	l := closedOf(t, m)
	if _, ok := l.Find("Alice Example"); ok || l.Questions[1].Status != "applied" || l.Questions[1].Applied != stamp {
		t.Errorf("after apply: %+v", l)
	}
	_, err = s.Apply(ctx, "q2", "")
	wantErr(t, err, "apply: q2 is applied, not approved")
}

func TestApplyFailureStaysApproved(t *testing.T) {
	ctx := context.Background()
	m, s := apiaryStore(t)
	long := strings.Repeat("x", 600)
	if _, err := s.Ask(ctx, "标签留不留？", []string{"demote", long}); err != nil {
		t.Fatal(err)
	}
	if err := s.Answer(ctx, "q1", "approved", "就这么办"); err != nil {
		t.Fatal(err)
	}
	_, err := s.Apply(ctx, "q1", "")
	wantErr(t, err, `demote: "`+long+`" is not on the closed list`)
	q := closedOf(t, m).Questions[0]
	if q.Status != "approved" || q.Error != err.Error()[:500] {
		t.Fatalf("failed apply: %s, error of %d bytes", q.Status, len(q.Error))
	}
	if _, err := s.Run(ctx, []string{"promote", long, "project"}, "", "q0"); err != nil {
		t.Fatal(err)
	}
	c, err := s.Apply(ctx, "q1", "补上")
	if err != nil || c.Reason != "补上" {
		t.Fatalf("retry: %+v %v", c, err)
	}
	if q := closedOf(t, m).Questions[0]; q.Status != "applied" || q.Error != "" {
		t.Errorf("after the retry: %+v", q)
	}
}

func TestApplyApproved(t *testing.T) {
	ctx := context.Background()
	m, s := apiaryStore(t)
	for _, q := range []struct {
		text, status string
		proposal     []string
	}{
		{"h-99 也标上蜂胶？", "approved", []string{"set", "h-99", "蜂胶"}},
		{"越冬还需要固定吗？", "expired", []string{"demote", "越冬"}},
		{"药剂具体点，写草酸？", "approved", []string{"rename", "药剂", "草酸"}},
		{"这周有空碰一下吗？", "approved", nil},
	} {
		asked, err := s.Ask(ctx, q.text, q.proposal)
		if err != nil {
			t.Fatal(err)
		}
		if err := s.Answer(ctx, asked.ID, q.status, ""); err != nil {
			t.Fatal(err)
		}
	}
	got, err := s.ApplyApproved(ctx)
	if err != nil || len(got) != 2 || got[0].ID != "q1" || got[1].ID != "q3" {
		t.Fatalf("ApplyApproved: %+v %v", got, err)
	}
	if got[0].Error != "set: no live record h-99" || got[1].Error != "" || len(got[1].Change.Records) != 1 {
		t.Errorf("a failure does not stop the pass: %+v", got)
	}
	var states []string
	for _, q := range closedOf(t, m).Questions {
		states = append(states, q.Status)
	}
	if !slices.Equal(states, []string{"approved", "expired", "applied", "approved"}) {
		t.Errorf("states: %q", states)
	}
	if got := labelsOf(t, m, "h-12"); !slices.Equal(got, []string{"蜂螨", "草酸"}) {
		t.Errorf("h-12: %q", got)
	}
}

func TestApplyUnrecorded(t *testing.T) {
	ctx := context.Background()
	m, s := apiaryStore(t)
	for _, p := range [][]string{{"demote", "蜜源"}, {"promote", "花期", "project"}} {
		q, err := s.Ask(ctx, "照此修改？", p)
		if err != nil {
			t.Fatal(err)
		}
		if err := s.Answer(ctx, q.ID, "approved", ""); err != nil {
			t.Fatal(err)
		}
	}
	s.Reg = &flaky{mem: m, key: Key, failing: true}
	got, err := s.ApplyApproved(ctx)
	var ue *UnrecordedError
	if !errors.As(err, &ue) || ue.ID != "q1" || !errors.Is(ue.Record, errFlaky) {
		t.Fatalf("ApplyApproved: %v", err)
	}
	wantErr(t, err, `demote: "蜜源" is not on the closed list (and recording it on q1 failed: registry offline)`)
	if errors.Unwrap(err) != ue.Err || len(got) != 1 || got[0].ID != "q1" {
		t.Errorf("results: %+v", got)
	}
	if q := closedOf(t, m).Questions[1]; q.Status != "approved" {
		t.Errorf("the pass went on to q2: %s", q.Status)
	}
}
