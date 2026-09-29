package meeting

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/caoer/mega-asr/internal/pages"
)

var t0 = time.Date(2020, 4, 21, 16, 5, 0, 0, time.UTC)

func rec(id, source, state string, start, dur time.Duration, roles ...string) Record {
	r := Record{ID: id, Source: source, State: state, Started: t0.Add(start), DurationS: dur.Seconds()}
	for _, role := range roles {
		r.Files = append(r.Files, File{Role: role, File: "f-" + id + "-" + role, SHA256: "x"})
	}
	return r
}

func ids(jobs []Job) []string {
	var s []string
	for _, j := range jobs {
		s = append(s, j.Local.ID+"~"+j.Feishu.ID)
	}
	return s
}

func TestJobs(t *testing.T) {
	mac := rec("mac1", "mac", Processing, 0, time.Hour, "remote", "mic", "segments")
	fs := rec("fs1", "feishu", Uploaded, 2*time.Minute, 50*time.Minute, "media", "feishu-transcript")
	short := rec("fs2", "feishu", Uploaded, 55*time.Minute, 30*time.Minute, "media", "feishu-transcript")
	noMedia := rec("fs3", "feishu", Uploaded, 0, time.Hour, "feishu-transcript")

	// A local recording aligns against its longest-overlapping minute and
	// flags the other; a minute without media cannot be offset.
	got := Jobs(mac, []Record{mac, fs, short, noMedia}, false)
	if want := []string{"mac1~fs1"}; fmt.Sprint(ids(got)) != fmt.Sprint(want) || fmt.Sprint(got[0].Others) != "[fs2]" {
		t.Errorf("local claimed: %v others %v", ids(got), got[0].Others)
	}
	if got := Jobs(mac, []Record{mac, noMedia}, false); got != nil {
		t.Errorf("minute without media paired: %v", ids(got))
	}

	// A minute claimed while its recording is still processing leaves it to
	// the recording's own run.
	fsClaimed := fs
	fsClaimed.State = Processing
	if got := Jobs(fsClaimed, []Record{mac, fsClaimed}, false); got != nil {
		t.Errorf("minute scored a recording still processing: %v", ids(got))
	}
	// Once the recording is ingested, the minute scores it; the shorter
	// minute is not its best, so it scores nothing.
	mac.State = Ingested
	if got := Jobs(fsClaimed, []Record{mac, fsClaimed, short}, false); fmt.Sprint(ids(got)) != "[mac1~fs1]" {
		t.Errorf("minute after ingest: %v", ids(got))
	}
	if got := Jobs(short, []Record{mac, fs, short}, false); got != nil {
		t.Errorf("second-best minute scored: %v", ids(got))
	}
	// Already scored against this minute: only a redo scores it again.
	mac.Scores = &Scores{Against: "fs1"}
	if got := Jobs(fsClaimed, []Record{mac, fsClaimed}, false); got != nil {
		t.Errorf("scored twice: %v", ids(got))
	}
	if got := Jobs(fsClaimed, []Record{mac, fsClaimed}, true); len(got) != 1 {
		t.Errorf("redo: %v", ids(got))
	}
}

// fakeReg is the page's record store and file store, with versions.
type fakeReg struct {
	objs    map[string]pages.Object
	files   map[string][]byte
	deleted []string
	n       int
}

func newFakeReg(t *testing.T, recs ...Record) *fakeReg {
	f := &fakeReg{objs: map[string]pages.Object{}, files: map[string][]byte{}}
	for _, r := range recs {
		if _, err := f.CAS(context.Background(), "rec."+r.ID, Schema, r, 0); err != nil {
			t.Fatal(err)
		}
	}
	return f
}

func (f *fakeReg) List(_ context.Context, prefix string) ([]pages.Object, error) {
	var out []pages.Object
	for k, o := range f.objs {
		if strings.HasPrefix(k, prefix) {
			out = append(out, o)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Key < out[j].Key })
	return out, nil
}

func (f *fakeReg) Record(_ context.Context, key string) (pages.Object, error) {
	o, ok := f.objs[key]
	if !ok {
		return o, &pages.Error{Status: 404, Code: "no_such_object"}
	}
	return o, nil
}

func (f *fakeReg) CAS(_ context.Context, key, schema string, data any, version int) (int, error) {
	if f.objs[key].Version != version {
		return 0, &pages.Error{Status: 409, Code: "version_conflict", Version: f.objs[key].Version}
	}
	b, err := json.Marshal(data)
	if err != nil {
		return 0, err
	}
	f.objs[key] = pages.Object{Key: key, Value: pages.Value{Schema: schema, Data: b}, Version: version + 1}
	return version + 1, nil
}

func (f *fakeReg) Upload(_ context.Context, path string, meta map[string]any) (string, string, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return "", "", err
	}
	f.n++
	id := fmt.Sprintf("up%d-%s-%s", f.n, meta["rec"], meta["role"])
	f.files[id] = b
	return id, "sum", nil
}

func (f *fakeReg) DeleteFile(_ context.Context, id string) error {
	f.deleted = append(f.deleted, id)
	return nil
}

func (f *fakeReg) raw(t *testing.T, key string) map[string]any {
	var m map[string]any
	if err := json.Unmarshal(f.objs[key].Value.Data, &m); err != nil {
		t.Fatal(err)
	}
	return m
}

// fakeTools finds the Feishu media at offset seconds of the local
// timeline, drift-free, and makes meeteval's answer from the texts.
type fakeTools struct {
	offset, feishuS, localS float64
	score                   float64
}

func (f fakeTools) Offset(_ context.Context, _ string, _ []string, _ float64) (Search, error) {
	return Search{FeishuS: f.feishuS, LocalS: f.localS, Probes: []Probe{
		{At: "start", FeishuS: 100, LocalS: 100 + f.offset, Score: f.score},
		{At: "end", FeishuS: f.feishuS - 200, LocalS: f.feishuS - 200 + f.offset, Score: f.score},
	}}, nil
}

func (f fakeTools) CP(_ context.Context, ref, hyp []Utterance, _ float64) (CPResult, error) {
	// A hypothesis speaker whose words equal a reference speaker's
	// is assigned to it; the rates are fixed.
	words := func(us []Utterance) map[string]string {
		m := map[string]string{}
		for _, u := range us {
			m[u.Speaker] += u.Words + " "
		}
		return m
	}
	var as [][2]string
	for rs, rw := range words(ref) {
		for hs, hw := range words(hyp) {
			if rw == hw {
				as = append(as, [2]string{rs, hs})
			}
		}
	}
	return CPResult{CP: ErrorRate{Rate: 0.1, Assignment: as}, TCP: ErrorRate{Rate: 0.2}}, nil
}

// The unit gate: a Feishu minute claimed after its mac recording reached
// ingested writes the scores onto the mac record, keeps its state, and
// never puts it back in the queue.
func TestFeishuAfterIngestedScoresLocal(t *testing.T) {
	dir := t.TempDir()
	write := func(name string, v any) string {
		p := filepath.Join(dir, name)
		b, _ := json.Marshal(v)
		if err := os.WriteFile(p, b, 0o600); err != nil {
			t.Fatal(err)
		}
		return p
	}
	segsPath := write("segments.json", []Segment{
		{Track: "remote", Speaker: "remote", StartS: 230, EndS: 240, Text: "仓库门口的灯坏了"},
		{Track: "mic", Speaker: "mic", StartS: 242, EndS: 250, Text: "明早找电工来修"},
		{Track: "remote", Speaker: "remote", StartS: 260, EndS: 266, Text: "顺便看看插座"},
	})
	turnsPath := write("transcript.json", map[string]any{"segments": []Turn{
		{Speaker: "S2", StartS: 200, Text: "仓库门口的灯坏了。"},
		{Speaker: "S1", StartS: 212, Text: "明早找电工来修。"},
		{Speaker: "S2", StartS: 230, Text: "顺便看看插座。"},
	}})

	mac := rec("20200421-160500-mac-host-a", "mac", Ingested, 0, time.Hour, "remote", "mic", "segments")
	mac.Wiki = &Wiki{Page: "minutes/x.md", Commit: "abc"}
	mac.Scores = &Scores{Loops: 2, RTF: 0.4}
	mac.Files = append(mac.Files, File{Role: "align", File: "old-align"})
	fs := rec("20200421-160530-feishu-host-a", "feishu", Processing, 30*time.Second, 50*time.Minute, "media", "feishu-transcript")
	reg := newFakeReg(t, mac, fs)
	before := reg.objs["rec."+mac.ID].Version

	al := &Aligner{
		Reg:   reg,
		Tools: fakeTools{offset: 30, feishuS: 3000, localS: 3600, score: 42},
		Data:  filepath.Join(dir, "data"),
		Fetch: func(_ context.Context, r Record, roles ...string) (map[string]string, error) {
			m := map[string]string{}
			for _, role := range roles {
				if _, ok := r.File(role); !ok {
					continue
				}
				switch role {
				case "mic":
					continue // no training rows: they need real audio
				case "segments":
					m[role] = segsPath
				case "feishu-transcript":
					m[role] = turnsPath
				default:
					m[role] = filepath.Join(dir, role+".flac") // the fake tools read no audio
				}
			}
			return m, nil
		},
	}
	self, err := al.Claimed(context.Background(), fs.ID, false)
	if err != nil {
		t.Fatal(err)
	}
	if self != nil {
		t.Errorf("a Feishu minute got a patch of its own: %+v", self)
	}
	got := reg.raw(t, "rec."+mac.ID)
	if got["state"] != Ingested || got["action"] != nil || got["claim"] != nil {
		t.Errorf("state %v action %v claim %v: want ingested, no action, no claim", got["state"], got["action"], got["claim"])
	}
	if w, _ := got["wiki"].(map[string]any); w["commit"] != "abc" {
		t.Errorf("wiki %v changed", got["wiki"])
	}
	for k := range reg.objs {
		if strings.HasPrefix(k, "q.") {
			t.Errorf("queued again: %s", k)
		}
	}
	s, _ := got["scores"].(map[string]any)
	if s["against"] != fs.ID || s["cer"] != 0.0 || s["offset_s"] != 30.0 || s["cpcer"] != 0.1 || s["loops"] != 2.0 || s["rtf"] != 0.4 {
		t.Errorf("scores %v", s)
	}
	if v := reg.objs["rec."+mac.ID].Version; v != before+1 {
		t.Errorf("mac record version %d, want %d", v, before+1)
	}
	var align string
	for _, f := range got["files"].([]any) {
		if m := f.(map[string]any); m["role"] == "align" {
			if align != "" {
				t.Errorf("two align files")
			}
			align = m["file"].(string)
		}
	}
	if !strings.HasSuffix(align, mac.ID+"-align") || fmt.Sprint(reg.deleted) != "[old-align]" {
		t.Errorf("align file %q, deleted %v", align, reg.deleted)
	}
	lines, err := ReadLines(filepath.Join(al.Data, "scores.jsonl"), time.Time{})
	if err != nil || len(lines) != 1 || lines[0].Rec != mac.ID {
		t.Errorf("score table %+v %v", lines, err)
	}

	// The minute claimed again (a retry) finds the recording scored.
	if _, err := al.Claimed(context.Background(), fs.ID, false); err != nil {
		t.Fatal(err)
	}
	if v := reg.objs["rec."+mac.ID].Version; v != before+1 {
		t.Errorf("rescored without a realign: version %d", v)
	}
}

func TestFit(t *testing.T) {
	for _, tc := range []struct {
		name   string
		probes []Probe
		paired bool
		off    float64
		drift  float64
	}{
		{"line", []Probe{{"start", 100, 117.3, 40}, {"end", 3900, 3917.3 - 0.38, 35}}, true, 17.31, -100},
		{"one probe", []Probe{{"start", 100, 117.3, 40}}, true, 17.3, 0},
		{"weak end", []Probe{{"start", 100, 117.3, 40}, {"end", 3900, 3000, 4}}, true, 17.3, 0},
		{"bad drift", []Probe{{"start", 100, 117.3, 40}, {"end", 3900, 3930, 50}}, true, 30, 0},
		{"no match", []Probe{{"start", 100, 500, 6}, {"end", 3900, 20, 7}}, false, 0, 0},
	} {
		r := Result{Search: Search{Probes: tc.probes}}
		ok := fit(&r)
		if ok != tc.paired || math.Abs(r.OffsetS-tc.off) > 0.01 || math.Abs(r.DriftPPM-tc.drift) > 0.5 {
			t.Errorf("%s: paired %v offset %.3f drift %.1f, want %v %.3f %.1f (%v)", tc.name, ok, r.OffsetS, r.DriftPPM, tc.paired, tc.off, tc.drift, r.Flags)
		}
	}
}

// Windowed scores see the same text as the same; a transcript shifted by
// a window's length is scored against the wrong stretch of Feishu.
func TestAlignWindowsShift(t *testing.T) {
	var turns []Turn
	var segs []Segment
	for i := 0; i < 20; i++ {
		var b strings.Builder
		for k := 0; k < 12; k++ {
			b.WriteRune(rune(0x4E00 + 37*i + k))
		}
		text := b.String() + fmt.Sprintf(" meeting %d", i)
		turns = append(turns, Turn{Speaker: "A", StartS: float64(60 * i), Text: text})
		segs = append(segs, Segment{Track: "remote", Speaker: "remote", StartS: float64(60*i) + 20, EndS: float64(60*i) + 50, Text: text})
	}
	tools := fakeTools{offset: 20, feishuS: 1250, localS: 1300, score: 30}
	in := Input{Rec: "r", Against: "f", Segments: segs, Turns: turns}
	res, err := Align(context.Background(), tools, in)
	if err != nil || !res.Paired {
		t.Fatal(err, res.Flags)
	}
	if res.CER != 0 || res.RefTokens == 0 || len(res.Windows) != 5 {
		t.Errorf("aligned: CER %.3f over %d tokens, %d windows", res.CER, res.RefTokens, len(res.Windows))
	}
	shifted := append([]Segment(nil), segs...)
	for i := range shifted {
		shifted[i].StartS += WindowS
		shifted[i].EndS += WindowS
	}
	in.Segments = shifted
	res2, err := Align(context.Background(), tools, in)
	if err != nil {
		t.Fatal(err)
	}
	if res2.CER < 0.5 {
		t.Errorf("shifted by %v s: CER %.3f, want well above 0", WindowS, res2.CER)
	}
}

// trainRows labels a mic segment with the stretch of one speaker's turns in
// reach of it, joined, that its tokens align to; the row keeps the edit
// count. Other tracks, filler-only segments and a segment with none of that
// speaker's turns in reach make no row, but every mic segment takes a number.
// Times move by the offset and rate.
func TestTrainRows(t *testing.T) {
	turns := []Turn{
		{Speaker: "S1", StartS: 0, Text: "东墙那排灯管先拆下来。"},
		{Speaker: "S2", StartS: 6, Text: "拆完放到推车上，"},
		{Speaker: "S2", StartS: 9, Text: "推车在后门旁边。"},
		{Speaker: "S1", StartS: 15, Text: "推车轮子有点歪。"},
		{Speaker: "S2", StartS: 400, Text: "Next week we paint the north wall, it's dry by then."},
	}
	segs := []Segment{
		{Track: "mic", StartS: 6, EndS: 12, Text: "拆完放到推车桑推车在后门旁边"},
		{Track: "remote", StartS: 15, EndS: 18, Text: "推车轮子有点歪"},
		{Track: "mic", StartS: 30, EndS: 31, Text: "嗯"},
		{Track: "mic", StartS: 395, EndS: 402, Text: "next week we paint the north wall"},
		{Track: "mic", StartS: 600, EndS: 603, Text: "推车"},
		{Track: "mic", StartS: 800, EndS: 801, Text: "uh"},
	}
	rows := trainRows(Input{Rec: "k2", Date: "2020-06-09"}, segs, turns, "S2", 2, 0.5)
	var got []string
	for _, r := range rows {
		got = append(got, fmt.Sprintf("%s %g+%g %s %d/%d", r.ID, r.StartS, r.DurS, r.Text, r.Errors, r.RefTokens))
	}
	if want := "k2-mic-0001 5+3 拆完放到推车上，推车在后门旁边。 1/14|k2-mic-0003 199.5+3.5 Next week we paint the north wall, 0/7"; strings.Join(got, "|") != want {
		t.Fatalf("rows %q, want %q", strings.Join(got, "|"), want)
	}
	// Starting at 先 (锁 dropped), at 锁 (先 read as 锁) or at 后 (先 left
	// over) costs one edit each; the walk back keeps the start at the
	// matching token.
	ref := "先锁后门。"
	r := units(ref)
	if i, j, _ := locate(units("先后门"), r); slice(ref, r[i].lo, r[j-1].hi) != ref {
		t.Errorf("tie at the start: %q", slice(ref, r[i].lo, r[j-1].hi))
	}
}

// A root comparison scores a new transcript at the offset and drift the
// record's alignment fitted: the same transcript scores as Align did, and a
// worse one scores worse, by the delta the table prints.
func TestRescore(t *testing.T) {
	var turns []Turn
	var segs, worse []Segment
	for i := 0; i < 20; i++ {
		var b strings.Builder
		for k := 0; k < 10; k++ {
			b.WriteRune(rune(0x4E00 + 37*i + k))
		}
		text := b.String() + fmt.Sprintf(" meeting %d", i)
		turns = append(turns, Turn{Speaker: "A", StartS: float64(60 * i), Text: text})
		s := Segment{Track: "remote", StartS: float64(60*i) + 20, EndS: float64(60*i) + 50, Text: text, Root: "/base"}
		segs = append(segs, s)
		if i%2 == 0 {
			s.Text = "错" + text[3:] // one Han character of ten substituted
		}
		s.Root = "/lora"
		worse = append(worse, s)
	}
	tools := fakeTools{offset: 20, feishuS: 1250, localS: 1300, score: 30}
	prev, err := Align(context.Background(), tools, Input{Rec: "r", Against: "f", Segments: segs, Turns: turns})
	if err != nil || !prev.Paired {
		t.Fatal(err, prev.Flags)
	}
	base := Rescore(prev, segs, turns)
	if base.CER != prev.CER || base.RefTokens != prev.RefTokens || len(base.Windows) != len(prev.Windows) {
		t.Errorf("same transcript: CER %.3f over %d tokens, Align said %.3f over %d", base.CER, base.RefTokens, prev.CER, prev.RefTokens)
	}
	lora := Rescore(prev, worse, turns)
	if lora.Root != "/lora" || lora.CERZh != 0.05 || lora.WEREn != 0 {
		t.Errorf("worse transcript: root %q, zh %.4f (want 0.05), en %.4f", lora.Root, lora.CERZh, lora.WEREn)
	}
	var b strings.Builder
	if err := PrintRoots(&b, [][]RootRun{{{Root: "base", Res: base}, {Root: "lora", Res: lora}}}); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(b.String(), "lora − base") || !strings.Contains(b.String(), "+5.00 pp") {
		t.Errorf("table:\n%s", b.String())
	}

	// A Feishu minute against its own transcript: offset 0, no drift.
	own := make([]Segment, len(segs))
	for i, s := range segs {
		s.StartS -= 20
		own[i] = s
	}
	if r := Rescore(Itself("f", 1250), own, turns); r.CER != 0 || r.RefTokens != base.RefTokens {
		t.Errorf("itself: CER %.3f over %d tokens", r.CER, r.RefTokens)
	}
}

func TestRescorable(t *testing.T) {
	paired := rec("a-mac", "mac", Aligned, 0, time.Hour, "remote", "mic", "segments", "align")
	paired.Scores = &Scores{Against: "m-feishu"}
	unpaired := rec("b-mac", "mac", Aligned, time.Hour, time.Hour, "remote", "mic", "segments")
	minute := rec("m-feishu", "feishu", Aligned, 2*time.Hour, time.Hour, "media", "feishu-transcript", "segments")
	raw := rec("n-feishu", "feishu", Uploaded, 3*time.Hour, time.Hour, "media", "feishu-transcript")
	got := Rescorable([]Record{paired, unpaired, minute, raw})
	if len(got) != 2 || got[0].ID != "m-feishu" || got[1].ID != "a-mac" {
		t.Errorf("rescorable: %v", got)
	}
}

func (f *fakeReg) Delete(_ context.Context, key string) error {
	if _, ok := f.objs[key]; !ok {
		return &pages.Error{Status: 404, Code: "no_such_object"}
	}
	delete(f.objs, key)
	return nil
}

func (f *fakeReg) Get(_ context.Context, id string, _, _ int64) (io.ReadCloser, error) {
	return io.NopCloser(bytes.NewReader(f.files[id])), nil
}
