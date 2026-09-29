package drain

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/caoer/mega-asr/internal/pages"
)

// mem is the page's /d/ store: compare-and-swap by version, 0 = absent.
type mem struct {
	mu   sync.Mutex
	at   int64 // updated_at of the last write
	objs map[string]pages.Object
	cas  func(key string) // called before each CAS, unlocked; may be nil
}

func newMem() *mem { return &mem{objs: map[string]pages.Object{}} }

func (m *mem) put(t *testing.T, key string, data any) {
	t.Helper()
	b, _ := json.Marshal(data)
	m.mu.Lock()
	defer m.mu.Unlock()
	o := m.objs[key]
	m.at++
	m.objs[key] = pages.Object{Key: key, Value: pages.Value{Schema: recSchema, Data: b}, Version: o.Version + 1, UpdatedAt: m.at}
}

func (m *mem) rec(t *testing.T, id string) map[string]any {
	t.Helper()
	m.mu.Lock()
	defer m.mu.Unlock()
	o, ok := m.objs["rec."+id]
	if !ok {
		t.Fatalf("no rec.%s", id)
	}
	var raw map[string]any
	json.Unmarshal(o.Value.Data, &raw)
	return raw
}

func (m *mem) has(key string) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	_, ok := m.objs[key]
	return ok
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
	sort.Slice(out, func(i, j int) bool { return out[i].Key < out[j].Key })
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

func (m *mem) CAS(ctx context.Context, key, schema string, data any, version int) (int, error) {
	if m.cas != nil {
		m.cas(key)
	}
	return m.swap(key, schema, data, version)
}

func (m *mem) swap(key, schema string, data any, version int) (int, error) {
	b, err := json.Marshal(data)
	if err != nil {
		return 0, err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.objs[key].Version != version {
		return 0, &pages.Error{Status: 409, Code: "version_conflict", Version: m.objs[key].Version}
	}
	m.at++
	m.objs[key] = pages.Object{Key: key, Value: pages.Value{Schema: schema, Data: b}, Version: version + 1, UpdatedAt: m.at}
	return version + 1, nil
}

func (m *mem) Delete(_ context.Context, key string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, ok := m.objs[key]; !ok {
		return &pages.Error{Status: 404, Code: "no_such_object"}
	}
	delete(m.objs, key)
	return nil
}

// fake records which stages ran.
type fake struct {
	mu     sync.Mutex
	calls  []string
	fail   map[string]error // stage → error
	ingest []bool           // replace flags
	wait   chan struct{}    // Process blocks on it when set
}

func (f *fake) note(s string) {
	f.mu.Lock()
	f.calls = append(f.calls, s)
	f.mu.Unlock()
}

func (f *fake) stages(withIngest bool) Stages {
	st := Stages{
		Pull: func(_ context.Context, id string) (string, error) {
			f.note("pull " + id)
			return "/pull/" + id, f.fail["pull"]
		},
		Process: func(_ context.Context, id, dir string) error {
			f.note("process " + id)
			if f.wait != nil {
				<-f.wait
			}
			return f.fail["process"]
		},
		Align: func(_ context.Context, id string, redo bool) (Patch, error) {
			f.note(fmt.Sprintf("align %s redo=%v", id, redo))
			return func(raw map[string]any) error { raw["align"] = "unpaired"; return nil }, f.fail["align"]
		},
	}
	if withIngest {
		st.Ingest = func(_ context.Context, id, dir string, rec map[string]any, replace bool) (Wiki, error) {
			f.note("ingest " + id)
			f.mu.Lock()
			f.ingest = append(f.ingest, replace)
			err := f.fail["ingest"]
			if errors.Is(err, errOnce) {
				delete(f.fail, "ingest")
			}
			f.mu.Unlock()
			return Wiki{Page: "minutes/x.md", Commit: "abc123"}, err
		}
	}
	return st
}

var errOnce = errors.New("fails once")

type logs struct {
	mu    sync.Mutex
	lines []string
}

func (l *logs) f(format string, args ...any) {
	l.mu.Lock()
	l.lines = append(l.lines, fmt.Sprintf(format, args...))
	l.mu.Unlock()
}

func (l *logs) has(s string) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	for _, x := range l.lines {
		if strings.Contains(x, s) {
			return true
		}
	}
	return false
}

var t0 = time.Date(2020, 3, 11, 6, 40, 0, 0, time.UTC)

func newDrain(reg Registry, st Stages, by string, l *logs) *Drain {
	d := New(reg, st, by)
	d.Now = func() time.Time { return t0 }
	d.Logf = l.f
	return d
}

func uploaded(m *mem, t *testing.T, id string, extra map[string]any) {
	rec := map[string]any{"id": id, "source": "mac", "title": "t", "state": "uploaded", "attempts": 0, "files": []any{}, "started": t0.Add(-time.Hour).Format(time.RFC3339)}
	for k, v := range extra {
		rec[k] = v
	}
	m.put(t, "rec."+id, rec)
	m.put(t, "q."+id, map[string]string{"state": "uploaded"})
}

func TestUploadedReachesAlignedWithIngestOff(t *testing.T) {
	m, f, l := newMem(), &fake{}, &logs{}
	uploaded(m, t, "a", nil)
	d := newDrain(m, f.stages(false), "host-a:1", l)
	run, err := d.Tick(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(f.calls, ", "); got != "pull a, process a, align a redo=false" {
		t.Fatalf("stages %q", got)
	}
	r := m.rec(t, "a")
	if r["state"] != "aligned" || r["claim"] != nil || r["stage"] != nil || r["align"] != "unpaired" || r["attempts"] != 1.0 {
		t.Fatalf("record %v", r)
	}
	if m.has("q.a") {
		t.Fatal("q.a stays after the record is parked at aligned")
	}
	if run.Results[0].Outcome != "aligned" {
		t.Fatalf("run %+v", run)
	}
}

func TestStageFailureIsFailedWithOneLine(t *testing.T) {
	m, f, l := newMem(), &fake{fail: map[string]error{"process": errors.New("decode: invalid data found")}}, &logs{}
	uploaded(m, t, "b", nil)
	d := newDrain(m, f.stages(true), "host-a:1", l)
	var lines []string
	d.Notify = func(s string) { lines = append(lines, s) }
	d.Tick(context.Background())
	r := m.rec(t, "b")
	if r["state"] != "failed" || r["stage"] != "asr" || !strings.Contains(r["error"].(string), "invalid data") || r["claim"] != nil {
		t.Fatalf("record %v", r)
	}
	if len(lines) != 1 || !strings.Contains(lines[0], "rec.b «t» failed at asr") {
		t.Fatalf("notify %q", lines)
	}
	if m.has("q.b") {
		t.Fatal("q.b stays after failed")
	}
	// A second tick does nothing: failed without an action is not claimable.
	d.Tick(context.Background())
	if len(lines) != 1 || len(f.calls) != 2 {
		t.Fatalf("second tick: %q %q", lines, f.calls)
	}
	// The page's retry re-queues it and clears nothing else; the drain runs it again.
	r["action"] = "retry"
	delete(f.fail, "process")
	m.put(t, "rec.b", r)
	m.put(t, "q.b", map[string]string{"state": "failed"})
	d.Tick(context.Background())
	if r := m.rec(t, "b"); r["state"] != "ingested" || r["error"] != nil || r["action"] != nil || r["attempts"] != 2.0 {
		t.Fatalf("after retry %v", r)
	}
}

func TestPullFailureNamesThePull(t *testing.T) {
	m, f, l := newMem(), &fake{fail: map[string]error{"pull": errors.New("sha_mismatch: mic")}}, &logs{}
	uploaded(m, t, "p", nil)
	newDrain(m, f.stages(false), "h:1", l).Tick(context.Background())
	if r := m.rec(t, "p"); r["state"] != "failed" || r["stage"] != "pull" || r["error"] != "sha_mismatch: mic" {
		t.Fatalf("record %v", r)
	}
}

// Two drains read the same version; the second compare-and-swap loses,
// logs the 409, and runs nothing.
func TestTwoDrainsNeverRunOneRecordTwice(t *testing.T) {
	m, f, lb := newMem(), &fake{}, &logs{}
	uploaded(m, t, "c", nil)
	b := newDrain(m, f.stages(false), "host-b:2", lb)
	// b reads rec.c, then a drain on host-a claims it before b's swap lands.
	o, _ := m.Record(context.Background(), "rec.c")
	var once sync.Once
	m.cas = func(key string) {
		once.Do(func() {
			if _, err := m.swap("rec.c", recSchema, map[string]any{"id": "c", "state": "processing",
				"claim": map[string]any{"by": "host-a:1", "at": "x", "expires": t0.Add(time.Hour).Format(time.RFC3339)}}, o.Version); err != nil {
				t.Errorf("a's claim: %v", err)
			}
		})
	}
	res := b.one(context.Background(), "c")
	if res.Outcome != "lost" || !lb.has("409 version_conflict") {
		t.Fatalf("b: %+v %q", res, lb.lines)
	}
	if len(f.calls) != 0 {
		t.Fatalf("b ran %q", f.calls)
	}
	// And a live claim is never taken: b's next tick skips it.
	m.cas = nil
	run, _ := b.Tick(context.Background())
	if run.Results[0].Outcome != "skipped" || len(f.calls) != 0 {
		t.Fatalf("b's tick %+v %q", run, f.calls)
	}
}

func TestReingestActionIsClaimedInOneTick(t *testing.T) {
	m, f, l := newMem(), &fake{}, &logs{}
	m.put(t, "rec.d", map[string]any{"id": "d", "state": "ingested", "action": "reingest", "wiki": map[string]any{"page": "old.md", "commit": "0"}})
	m.put(t, "q.d", map[string]string{"state": "ingested"})
	newDrain(m, f.stages(true), "h:1", l).Tick(context.Background())
	if got := strings.Join(f.calls, ", "); got != "pull d, ingest d" {
		t.Fatalf("stages %q", got)
	}
	if len(f.ingest) != 1 || !f.ingest[0] {
		t.Fatalf("replace flags %v", f.ingest)
	}
	r := m.rec(t, "d")
	if r["state"] != "ingested" || r["action"] != nil || r["wiki"].(map[string]any)["commit"] != "abc123" || m.has("q.d") {
		t.Fatalf("record %v", r)
	}
}

func TestRealignKeepsTheState(t *testing.T) {
	m, f, l := newMem(), &fake{}, &logs{}
	m.put(t, "rec.e", map[string]any{"id": "e", "state": "ingested", "action": "realign"})
	m.put(t, "q.e", map[string]string{"state": "ingested"})
	newDrain(m, f.stages(true), "h:1", l).Tick(context.Background())
	if got := strings.Join(f.calls, ", "); got != "align e redo=true" {
		t.Fatalf("stages %q", got)
	}
	if r := m.rec(t, "e"); r["state"] != "ingested" || r["align"] != "unpaired" || r["claim"] != nil {
		t.Fatalf("record %v", r)
	}
}

func TestIngestRunsAndRetriesOnce(t *testing.T) {
	m, f, l := newMem(), &fake{fail: map[string]error{"ingest": errOnce}}, &logs{}
	uploaded(m, t, "g", nil)
	newDrain(m, f.stages(true), "h:1", l).Tick(context.Background())
	if got := strings.Join(f.calls, ", "); got != "pull g, process g, align g redo=false, ingest g, ingest g" {
		t.Fatalf("stages %q", got)
	}
	r := m.rec(t, "g")
	if r["state"] != "ingested" || r["wiki"].(map[string]any)["page"] != "minutes/x.md" || f.ingest[0] {
		t.Fatalf("record %v, replace %v", r, f.ingest)
	}
}

// A minute the backfill already filed arrives with wiki set: processed and
// aligned (it scores the local recordings), never ingested again.
func TestWikiSetIsNeverIngestedAgain(t *testing.T) {
	m, f, l := newMem(), &fake{}, &logs{}
	uploaded(m, t, "h", map[string]any{"source": "feishu", "wiki": map[string]any{"page": "p.md", "commit": "c"}})
	newDrain(m, f.stages(true), "h:1", l).Tick(context.Background())
	if strings.Contains(strings.Join(f.calls, ","), "ingest") {
		t.Fatalf("ingested: %q", f.calls)
	}
	if r := m.rec(t, "h"); r["state"] != "ingested" || r["wiki"].(map[string]any)["commit"] != "c" || m.has("q.h") {
		t.Fatalf("record %v", r)
	}
}

func TestExpiredClaimIsReclaimed(t *testing.T) {
	m, f, l := newMem(), &fake{}, &logs{}
	m.put(t, "rec.i", map[string]any{"id": "i", "state": "processing", "attempts": 1,
		"claim": map[string]any{"by": "dead:9", "at": "x", "expires": t0.Add(-time.Minute).Format(time.RFC3339)}})
	m.put(t, "q.i", map[string]string{"state": "uploaded"})
	newDrain(m, f.stages(false), "h:1", l).Tick(context.Background())
	if r := m.rec(t, "i"); r["state"] != "aligned" || r["attempts"] != 2.0 {
		t.Fatalf("record %v", r)
	}
}

func TestSweepAbandonsOldUploads(t *testing.T) {
	m, f, l := newMem(), &fake{}, &logs{}
	m.put(t, "rec.old", map[string]any{"id": "old", "state": "uploading", "updated": t0.Add(-25 * time.Hour).Format(time.RFC3339)})
	m.put(t, "rec.new", map[string]any{"id": "new", "state": "uploading", "updated": t0.Add(-time.Hour).Format(time.RFC3339)})
	newDrain(m, f.stages(false), "h:1", l).Tick(context.Background())
	if r := m.rec(t, "old"); r["state"] != "failed" || r["error"] != "abandoned" {
		t.Fatalf("old %v", r)
	}
	if r := m.rec(t, "new"); r["state"] != "uploading" {
		t.Fatalf("new %v", r)
	}
}

// The heartbeat extends the claim while a stage runs; when another host
// takes the claim the work stops and the record is not marked failed.
func TestHeartbeatAndLostClaim(t *testing.T) {
	m, f, l := newMem(), &fake{wait: make(chan struct{})}, &logs{}
	uploaded(m, t, "j", nil)
	d := newDrain(m, f.stages(false), "h:1", l)
	var mu sync.Mutex
	now := t0
	d.Now = func() time.Time { mu.Lock(); defer mu.Unlock(); return now }
	d.Beat = 5 * time.Millisecond
	done := make(chan Result)
	go func() { done <- d.one(context.Background(), "j") }()
	waitFor(t, func() bool { return len(calls(f)) > 1 }) // in process
	mu.Lock()
	now = t0.Add(10 * time.Minute)
	mu.Unlock()
	waitFor(t, func() bool {
		cl, _ := m.rec(t, "j")["claim"].(map[string]any)
		return cl != nil && cl["expires"] == t0.Add(40*time.Minute).UTC().Format(time.RFC3339Nano)
	})
	// Another drain takes it over.
	r := m.rec(t, "j")
	r["claim"] = map[string]any{"by": "other:3", "at": "y", "expires": t0.Add(time.Hour).Format(time.RFC3339)}
	m.put(t, "rec.j", r)
	waitFor(t, func() bool { return l.has("no longer ours") })
	close(f.wait)
	res := <-done
	if res.Outcome != "lost" {
		t.Fatalf("result %+v", res)
	}
	if r := m.rec(t, "j"); r["state"] != "processing" || r["claim"].(map[string]any)["by"] != "other:3" {
		t.Fatalf("record %v", r)
	}
}

func calls(f *fake) []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.calls...)
}

func waitFor(t *testing.T, ok func() bool) {
	t.Helper()
	for i := 0; i < 400; i++ {
		if ok() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("timed out")
}

func TestStaleQueueKeysGo(t *testing.T) {
	m, f, l := newMem(), &fake{}, &logs{}
	m.put(t, "q.gone", map[string]string{"state": "uploaded"})
	m.put(t, "rec.done", map[string]any{"id": "done", "state": "ingested"})
	m.put(t, "q.done", map[string]string{"state": "uploaded"})
	newDrain(m, f.stages(false), "h:1", l).Tick(context.Background())
	if m.has("q.gone") || m.has("q.done") || len(f.calls) != 0 {
		t.Fatalf("queue %v calls %q", m.objs, f.calls)
	}
}

// The newest queued record runs first, and one queued while a tick runs is
// taken in the same tick.
func TestNewestFirstAndRelisted(t *testing.T) {
	m, f, l := newMem(), &fake{}, &logs{}
	uploaded(m, t, "1-old", nil)
	uploaded(m, t, "2-old", nil)
	st := f.stages(false)
	pull := st.Pull
	st.Pull = func(ctx context.Context, id string) (string, error) {
		if id == "2-old" {
			uploaded(m, t, "3-new", nil) // arrives mid-tick
		}
		return pull(ctx, id)
	}
	newDrain(m, st, "h:1", l).Tick(context.Background())
	var pulls []string
	for _, c := range f.calls {
		if strings.HasPrefix(c, "pull ") {
			pulls = append(pulls, strings.TrimPrefix(c, "pull "))
		}
	}
	if strings.Join(pulls, ",") != "2-old,3-new,1-old" {
		t.Fatalf("order %q", pulls)
	}
}

// A drain stopped mid-stage leaves the record for the next one: not failed,
// its claim lapsed.
func TestInterruptedReleasesTheClaim(t *testing.T) {
	m, f, l := newMem(), &fake{}, &logs{}
	uploaded(m, t, "k", nil)
	ctx, cancel := context.WithCancel(context.Background())
	st := f.stages(false)
	st.Process = func(ctx context.Context, id, dir string) error { cancel(); return ctx.Err() }
	var lines []string
	d := newDrain(m, st, "h:1", l)
	d.Notify = func(s string) { lines = append(lines, s) }
	if r := d.one(ctx, "k"); r.Outcome != "interrupted" {
		t.Fatalf("%+v", r)
	}
	r := m.rec(t, "k")
	if r["state"] != "processing" || r["claim"].(map[string]any)["expires"] != t0.UTC().Format(time.RFC3339Nano) || len(lines) != 0 || !m.has("q.k") {
		t.Fatalf("record %v, lines %q", r, lines)
	}
	if job, _ := d.job(view{State: "processing"}); job != "full" {
		t.Fatal("a lapsed claim is not claimable")
	}
}

// Feishu minutes are the backfill's to ingest: with the ingest on, one
// rests at aligned; the page's reingest still ingests it.
func TestFeishuRestsAtAlignedUnlessAsked(t *testing.T) {
	m, f, l := newMem(), &fake{}, &logs{}
	uploaded(m, t, "fs", map[string]any{"source": "feishu"})
	d := newDrain(m, f.stages(true), "h:1", l)
	d.Tick(context.Background())
	if r := m.rec(t, "fs"); r["state"] != "aligned" || strings.Contains(strings.Join(f.calls, ","), "ingest") {
		t.Fatalf("record %v calls %q", r, f.calls)
	}
	r := m.rec(t, "fs")
	r["action"] = "reingest"
	m.put(t, "rec.fs", r)
	m.put(t, "q.fs", map[string]string{"state": "aligned"})
	d.Tick(context.Background())
	if r := m.rec(t, "fs"); r["state"] != "ingested" || f.calls[len(f.calls)-1] != "ingest fs" {
		t.Fatalf("record %v calls %q", r, f.calls)
	}
}

func TestMaxStopsTheTick(t *testing.T) {
	m, f, l := newMem(), &fake{}, &logs{}
	uploaded(m, t, "1-old", nil)
	uploaded(m, t, "2-new", nil)
	d := newDrain(m, f.stages(false), "h:1", l)
	d.Max = 1
	d.Tick(context.Background())
	if got := strings.Join(f.calls, ","); got != "pull 2-new,process 2-new,align 2-new redo=false" || !m.has("q.1-old") {
		t.Fatalf("%q", got)
	}
}

// A record re-queued by a page action while the tick runs on is claimed in
// that same tick, though the tick tried it before.
func TestActionMidTickIsTakenInTheSameTick(t *testing.T) {
	m, f, l := newMem(), &fake{}, &logs{}
	uploaded(m, t, "1-backfill", nil)
	uploaded(m, t, "2-new", nil)
	st := f.stages(false)
	pull := st.Pull
	st.Pull = func(ctx context.Context, id string) (string, error) {
		if id == "1-backfill" {
			r := m.rec(t, "2-new")
			r["action"] = "reingest"
			m.put(t, "rec.2-new", r)
			m.put(t, "q.2-new", map[string]string{"state": "aligned"})
		}
		return pull(ctx, id)
	}
	st.Ingest = func(context.Context, string, string, map[string]any, bool) (Wiki, error) {
		f.note("ingest")
		return Wiki{Page: "p", Commit: "c"}, nil
	}
	newDrain(m, st, "h:1", l).Tick(context.Background())
	if got := strings.Join(f.calls, ","); !strings.HasSuffix(got, "align 1-backfill redo=false,ingest,pull 2-new,ingest") {
		t.Fatalf("%q", got)
	}
}

// feishuPulls makes Pull lay out each id's Feishu transcript with one
// segment per speaker label given; nil speakers is a pull without one.
func feishuPulls(t *testing.T, st *Stages, speakers map[string][]string) {
	t.Helper()
	dirs := map[string]string{}
	for id, sp := range speakers {
		dir := t.TempDir()
		dirs[id] = dir
		if sp == nil {
			continue
		}
		var segs []map[string]any
		for i, s := range sp {
			segs = append(segs, map[string]any{"speaker": s, "start_s": float64(i), "text": "嗯"})
		}
		writeJSON(t, filepath.Join(dir, "feishu-transcript.json"), map[string]any{"segments": segs})
	}
	pull := st.Pull
	st.Pull = func(ctx context.Context, id string) (string, error) {
		_, err := pull(ctx, id)
		return dirs[id], err
	}
}

// With feishu in the sources, only a minute Feishu created at or after
// feishu_since is ingested, and only when the backfill's rule holds; the
// others rest at aligned with the reason.
func TestNewFeishuMinutesAutoIngest(t *testing.T) {
	m, f, l := newMem(), &fake{}, &logs{}
	at := func(d time.Duration) string { return t0.Add(d).Format(time.RFC3339) }
	uploaded(m, t, "hist", map[string]any{"source": "feishu", "started": at(-48 * time.Hour), "duration_s": 600.0})
	uploaded(m, t, "new", map[string]any{"source": "feishu", "started": at(-time.Hour), "duration_s": 600.0})
	uploaded(m, t, "solo", map[string]any{"source": "feishu", "started": at(-time.Hour), "duration_s": 600.0})
	uploaded(m, t, "short", map[string]any{"source": "feishu", "started": at(-time.Hour), "duration_s": 120.0})
	uploaded(m, t, "notx", map[string]any{"source": "feishu", "started": at(-time.Hour), "duration_s": 600.0})
	st := f.stages(true)
	feishuPulls(t, &st, map[string][]string{"hist": {"A", "B"}, "new": {"A", "B"}, "solo": {"A", "A", "A"}, "short": {"A", "B"}, "notx": nil})
	d := newDrain(m, st, "h:1", l)
	d.IngestSources = append(d.IngestSources, "feishu")
	d.FeishuSince = t0.Add(-24 * time.Hour)
	d.Tick(context.Background())
	var ingested []string
	for _, c := range f.calls {
		if strings.HasPrefix(c, "ingest ") {
			ingested = append(ingested, c)
		}
	}
	if strings.Join(ingested, ",") != "ingest new" || m.rec(t, "new")["state"] != "ingested" {
		t.Fatalf("ingests %q, new %v", ingested, m.rec(t, "new"))
	}
	for id, why := range map[string]string{
		"hist":  "created before meeting.ingest.feishu_since 2020-03-10T06:40:00Z: the backfill's",
		"solo":  "1 speaker(s) in Feishu's transcript: a solo recording",
		"short": "120 s: shorter than 5 min",
		"notx":  "no Feishu transcript",
	} {
		if r := m.rec(t, id); r["state"] != "aligned" || r["ingest_skipped"] != why || m.has("q."+id) {
			t.Errorf("%s: %v", id, r)
		}
	}
	if _, ok := m.rec(t, "new")["ingest_skipped"]; ok {
		t.Error("new: ingest_skipped written")
	}
}

// A new minute the backfill filed between its claim and its ingest is not
// ingested again: the record takes the wiki's page and commit.
func TestMinuteFiledBeforeItsIngestIsSkipped(t *testing.T) {
	m, f, l := newMem(), &fake{}, &logs{}
	uploaded(m, t, "fl", map[string]any{"source": "feishu", "duration_s": 600.0, "feishu": map[string]any{"token": "obcn"}})
	st := f.stages(false)
	feishuPulls(t, &st, map[string][]string{"fl": {"A", "B"}})
	in := &Ingester{Command: []string{"false"}, Runs: t.TempDir(), Filed: func(token string) (Wiki, bool, error) {
		return Wiki{Page: "minutes/fl.md", Commit: "c0ffee1"}, token == "obcn", nil
	}}
	st.Ingest = in.Ingest
	d := newDrain(m, st, "h:1", l)
	d.IngestSources, d.FeishuSince = []string{"feishu"}, t0.Add(-24*time.Hour)
	d.Tick(context.Background())
	if r := m.rec(t, "fl"); r["state"] != "ingested" || r["wiki"].(map[string]any)["commit"] != "c0ffee1" {
		t.Fatalf("record %v", r)
	}
}

// A tombstone is never claimed, not even with a page action on it: its
// queue key goes and this host forgets its copies.
func TestDeletedIsNeverClaimed(t *testing.T) {
	m, f, l := newMem(), &fake{}, &logs{}
	uploaded(m, t, "d", map[string]any{"state": "deleted", "action": "retry", "deleted_at": "2020-03-11T05:10:00Z"})
	uploaded(m, t, "e", map[string]any{"state": "deleted", "action": "reingest"})
	d := newDrain(m, f.stages(true), "host-a:1", l)
	var forgot []string
	d.Forget = func(id string) { forgot = append(forgot, id) }
	run, err := d.Tick(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(f.calls) != 0 {
		t.Fatalf("stages ran on a tombstone: %v", f.calls)
	}
	for _, id := range []string{"d", "e"} {
		if r := m.rec(t, id); r["state"] != "deleted" || r["claim"] != nil || r["attempts"] != 0.0 {
			t.Fatalf("rec.%s %v", id, r)
		}
		if m.has("q." + id) {
			t.Fatalf("q.%s stays", id)
		}
	}
	if len(run.Results) != 2 || run.Results[0].Outcome != "removed" || run.Results[0].Error != "deleted" {
		t.Fatalf("run %+v", run.Results)
	}
	sort.Strings(forgot)
	if strings.Join(forgot, ",") != "d,d,e,e" { // the sweep, then the queue pass
		t.Fatalf("forgot %v", forgot)
	}
}

// While the ingest runs, its progress lands on the record's ingest key by
// claim writes that change nothing else; the last state lands when it ends.
func TestIngestProgressIsMirroredOntoTheRecord(t *testing.T) {
	m, f, l := newMem(), &fake{}, &logs{}
	uploaded(m, t, "g", nil)
	st := f.stages(true)
	var mu sync.Mutex
	status := map[string]any(nil)
	set := func(s map[string]any) { mu.Lock(); status = s; mu.Unlock() }
	st.IngestStatus = func(id string) (map[string]any, bool) {
		mu.Lock()
		defer mu.Unlock()
		return status, status != nil && id == "g"
	}
	ingest := st.Ingest
	var before, during map[string]any
	st.Ingest = func(ctx context.Context, id, dir string, rec map[string]any, replace bool) (Wiki, error) {
		before = m.rec(t, "g")
		set(map[string]any{"run": "r", "state": "running", "step": "write", "usd": 0.5, "page_slug": "ingest-x", "updated_at": "a"})
		deadline := time.Now().Add(2 * time.Second)
		for time.Now().Before(deadline) {
			if r := m.rec(t, "g"); r["ingest"] != nil {
				during = r
				break
			}
			time.Sleep(5 * time.Millisecond)
		}
		set(map[string]any{"run": "r", "state": "done", "usd": 2.1, "page_slug": "ingest-x", "wiki_page": "minutes/x.md", "updated_at": "b"})
		return ingest(ctx, id, dir, rec, replace)
	}
	d := newDrain(m, st, "h:1", l)
	d.Mirror = 5 * time.Millisecond
	d.Tick(context.Background())
	if during == nil || during["ingest"].(map[string]any)["step"] != "write" {
		t.Fatalf("no progress while running: %v", during)
	}
	for k, v := range before {
		if k != "updated" && mustJSON(during[k]) != mustJSON(v) {
			t.Errorf("the mirror changed %s: %v → %v", k, v, during[k])
		}
	}
	for k := range during {
		if _, ok := before[k]; !ok && k != "ingest" && k != "updated" {
			t.Errorf("the mirror added %s", k)
		}
	}
	r := m.rec(t, "g")
	if r["state"] != "ingested" || r["ingest"].(map[string]any)["state"] != "done" || r["ingest"].(map[string]any)["wiki_page"] != "minutes/x.md" {
		t.Fatalf("record %v", r)
	}
}

// A file record shorter than meeting.ingest.min_duration rests at aligned
// with its reason; a long one is ingested; a reingest action ingests the
// short one too.
func TestShortRecordRestsUnlessAsked(t *testing.T) {
	m, f, l := newMem(), &fake{}, &logs{}
	uploaded(m, t, "brief", map[string]any{"source": "file", "duration_s": 51.4})
	uploaded(m, t, "long", map[string]any{"source": "file", "duration_s": 180.0})
	d := newDrain(m, f.stages(true), "h:1", l)
	d.MinDuration = 2 * time.Minute
	d.Tick(context.Background())
	if r := m.rec(t, "brief"); r["state"] != "aligned" || r["ingest_skipped"] != "51 s: shorter than 2 min" || m.has("q.brief") {
		t.Fatalf("brief: %v", r)
	}
	if r := m.rec(t, "long"); r["state"] != "ingested" || r["ingest_skipped"] != nil {
		t.Fatalf("long: %v", r)
	}
	r := m.rec(t, "brief")
	r["action"] = "reingest"
	m.put(t, "rec.brief", r)
	m.put(t, "q.brief", map[string]string{"state": "aligned"})
	d.Tick(context.Background())
	if r := m.rec(t, "brief"); r["state"] != "ingested" || r["ingest_skipped"] != nil || f.calls[len(f.calls)-1] != "ingest brief" {
		t.Fatalf("brief after reingest: %v calls %q", r, f.calls)
	}
}

// A test record is processed and aligned, never ingested, not even on a
// reingest action.
func TestTestRecordIsNeverIngested(t *testing.T) {
	m, f, l := newMem(), &fake{}, &logs{}
	uploaded(m, t, "tst", map[string]any{"source": "file", "duration_s": 3600.0, "test": true})
	d := newDrain(m, f.stages(true), "h:1", l)
	d.Tick(context.Background())
	if got := strings.Join(f.calls, ", "); got != "pull tst, process tst, align tst redo=false" {
		t.Fatalf("stages %q", got)
	}
	if r := m.rec(t, "tst"); r["state"] != "aligned" || r["ingest_skipped"] != testWhy || m.has("q.tst") {
		t.Fatalf("record %v", r)
	}
	r := m.rec(t, "tst")
	r["action"] = "reingest"
	m.put(t, "rec.tst", r)
	m.put(t, "q.tst", map[string]string{"state": "aligned"})
	d.Tick(context.Background())
	if r := m.rec(t, "tst"); r["state"] != "aligned" || r["action"] != nil || r["ingest_skipped"] != testWhy || r["wiki"] != nil || m.has("q.tst") {
		t.Fatalf("after reingest %v", r)
	}
	if strings.Contains(strings.Join(f.calls, ","), "ingest") {
		t.Fatalf("ingested: %q", f.calls)
	}
}
