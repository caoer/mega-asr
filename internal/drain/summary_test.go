package drain

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/caoer/mega-asr/internal/labels"
)

// fakeClaude is a claude CLI that saves its stdin, prints a launcher line
// and then the JSON result whose model answer is answer.
func fakeClaude(t *testing.T, answer string) (cmd, stdin string) {
	t.Helper()
	dir := t.TempDir()
	stdin = filepath.Join(dir, "stdin")
	res := strings.NewReplacer(`\`, `\\`, `"`, `\"`, "\n", `\n`).Replace(answer)
	script := "#!/bin/sh\ncat > " + stdin + "\necho \"$MAX_THINKING_TOKENS $DISABLE_PROMPT_CACHING\" > " + stdin + ".env\necho 'launcher: session ready'\n" +
		`printf '%s\n' '{"type":"result","is_error":false,"result":"` + res + `","total_cost_usd":0.0123,"modelUsage":{"claude-haiku-4-5":{"inputTokens":9000,"outputTokens":300,"cacheReadInputTokens":0,"cacheCreationInputTokens":0}}}'` + "\n"
	cmd = filepath.Join(dir, "claude")
	if err := os.WriteFile(cmd, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	return cmd, stdin
}

// workshop is the closed list of the tests' tool library: two meeting types
// with a project entry between them, so Types keeps list order.
func workshop() labels.List {
	return labels.List{Closed: []labels.Entry{{Name: "修理夜", Kind: "type"}, {Name: "TL-40", Kind: "project"}, {Name: "入门课", Kind: "type"}}}
}

// diarized is a one-track recording whose segments carry diarized speakers,
// written out of time order.
func diarized(t *testing.T) string {
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "segments.json"), []byte(`[
	 {"speaker": "S1", "track": "media", "start_s": 4, "text": "台钳的钳口磨平了，得换一副。"},
	 {"speaker": "S2", "track": "media", "start_s": 3725, "text": "旧的那副先放进回收箱。"},
	 {"speaker": "S3", "track": "media", "start_s": 130, "text": "下个月进一批木工夹。"}]`), 0o644)
	return dir
}

// Stage sends the transcript in time order with the named speakers, the
// closed types and the label list; it writes display_title always (with the
// type taken out), labels onto a record without them, summary as a clamped
// gist and the details, and never title.
func TestSummarizerStage(t *testing.T) {
	answer := "Here it is:\n```json\n" + `{"subjects": ["台钳钳口", "木工夹"], "display_title": "【入门课】台钳钳口换新", "type": "入门课",
 "labels": ["tl-40", "  #工具墙 ", "修理夜", "砂带机", "Tl-40", "这个标签写得实在是太长了超过十六个字了吧", "焊台", ""],
 "gist": "` + strings.Repeat("钳", GistMax+1) + `", "details": "  旧钳口留作备件。  "}` + "\n```"
	cmd, stdin := fakeClaude(t, answer)
	var logged []string
	s := &Summarizer{Command: []string{cmd}, Model: "claude-haiku-4-5", MaxChars: 40000,
		Logf: func(f string, a ...any) { logged = append(logged, fmt.Sprintf(f, a...)) },
		Vocab: func(context.Context) (labels.Vocab, error) {
			return labels.Vocab{List: workshop(), Counts: map[string]int{"链锯": 4, "修理夜": 9, "工具墙": 7, "TL-40": 1, "电钻": 4}}, nil
		}}
	rec := map[string]any{"id": "t1", "source": "file", "title": "晚间录音 2", "started": "2020-02-11T19:30:00Z",
		"speakers": []any{map[string]any{"role": "S2", "name": "Alice Example"}, map[string]any{"role": "S1", "name": ""}}}
	p, err := s.Stage(context.Background(), "t1", diarized(t), rec)
	if err != nil {
		t.Fatal(err)
	}
	if env, _ := os.ReadFile(stdin + ".env"); string(env) != "0 1\n" {
		t.Fatalf("claude ran with thinking or the prompt cache: %q", env)
	}
	b, _ := os.ReadFile(stdin)
	in := string(b)
	head := "Meeting types: 修理夜, 入门课\nLabel list (most used first): TL-40, 工具墙, 电钻, 链锯\nRecording name: 晚间录音 2\nStarted: 2020-02-11T19:30:00Z\n"
	first, second, third := strings.Index(in, "[0:00:04] S1: 台钳的钳口磨平了，得换一副。"), strings.Index(in, "[0:02:10] S3: 下个月进一批木工夹。"), strings.Index(in, "[1:02:05] Alice Example: 旧的那副先放进回收箱。")
	if !strings.HasPrefix(in, head) || first < 0 || second < first || third < second {
		t.Fatalf("text sent:\n%s", in)
	}
	if len(logged) != 1 || logged[0] != "summary rec.t1: claude-haiku-4-5 from asr, 9000 in / 300 out tokens, $0.0123" {
		t.Fatalf("logged %q", logged)
	}

	p(rec)
	gist, details, _ := strings.Cut(rec["summary"].(string), "\n\n")
	if utf8.RuneCountInString(gist) != GistMax || !strings.HasSuffix(gist, "…") || details != "旧钳口留作备件。" {
		t.Fatalf("summary %q", rec["summary"])
	}
	// The answer's type first; a second type, a repeat in other case, a
	// label over labelMax, a blank and a second new label all go.
	if rec["title"] != "晚间录音 2" || rec["display_title"] != "台钳钳口换新" || !slices.Equal(rec["labels"].([]string), []string{"入门课", "TL-40", "工具墙", "砂带机"}) {
		t.Fatalf("title %v, display_title %v, labels %v", rec["title"], rec["display_title"], rec["labels"])
	}

	chosen := map[string]any{"source": "file", "labels": []any{}}
	p(chosen)
	if !reflect.DeepEqual(chosen["labels"], []any{}) || chosen["display_title"] != "台钳钳口换新" {
		t.Fatalf("[] (none chosen) overwritten: %v", chosen)
	}
}

// meetingText reads Feishu's transcript for a minute that has a non-empty
// one, ours otherwise, and Feishu's summary for a minute alone; with no words
// anywhere it tells a silent recording from one not transcribed yet.
func TestMeetingTextContent(t *testing.T) {
	ours := `[{"track": "media", "start_s": 2, "text": "扫码枪连不上了。"}]`
	theirs := `{"segments": [{"speaker": "S1", "start_s": 9, "text": "借出登记表第二页缺了日期。"}]}`
	for _, c := range []struct {
		name     string
		files    map[string]string
		rec      map[string]any
		content  string
		has, not []string
		err      error
	}{
		{name: "a file recording ignores a summary field",
			files: map[string]string{"segments.json": ours}, rec: map[string]any{"source": "file", "summary": "不该读到"},
			content: "asr", has: []string{"[0:00:02] media: 扫码枪连不上了。"}, not: []string{"不该读到", "Feishu's summary"}},
		{name: "silent, never summarized",
			files: map[string]string{"feishu-transcript.json": `{"segments": []}`}, rec: map[string]any{"source": "feishu"},
			err: errNoSpeech},
		{name: "a minute with both",
			files: map[string]string{"segments.json": ours, "feishu-transcript.json": theirs}, rec: map[string]any{"source": "feishu", "summary": " 登记表补齐了日期。 "},
			content: "feishu-transcript+feishu-summary", has: []string{"Feishu's summary of the meeting:\n登记表补齐了日期。\n", "[0:00:09] S1: 借出登记表第二页缺了日期。"}, not: []string{"扫码枪"}},
		{name: "nothing yet",
			rec: map[string]any{"source": "file"}, err: ErrNoContent},
		{name: "Feishu's transcript empty: ours",
			files: map[string]string{"segments.json": ours, "feishu-transcript.json": `{"segments": []}`}, rec: map[string]any{"source": "feishu", "summary": "登记表补齐了日期。"},
			content: "asr+feishu-summary", has: []string{"扫码枪连不上了。"}},
		{name: "the summary alone",
			rec: map[string]any{"source": "feishu", "summary": "登记表补齐了日期。"}, content: "feishu-summary", not: []string{"Transcript:"}},
	} {
		dir := t.TempDir()
		for name, body := range c.files {
			os.WriteFile(filepath.Join(dir, name), []byte(body), 0o644)
		}
		text, content, _, err := meetingText(dir, c.rec, 40000)
		if !errors.Is(err, c.err) || content != c.content {
			t.Errorf("%s: content %q, err %v", c.name, content, err)
			continue
		}
		for _, s := range c.has {
			if !strings.Contains(text, s) {
				t.Errorf("%s: no %q in\n%s", c.name, s, text)
			}
		}
		for _, s := range c.not {
			if strings.Contains(text, s) {
				t.Errorf("%s: %q in\n%s", c.name, s, text)
			}
		}
	}
}

// A Feishu minute keeps Feishu's summary: its patch adds display_title and
// labels and changes nothing else. With no Vocab the seed has no meeting
// type, so the model's type is not kept.
func TestSummarizerStageFeishuPatch(t *testing.T) {
	cmd, _ := fakeClaude(t, `{"display_title": "登记表补日期", "type": "修理夜", "labels": ["工具墙"], "gist": "登记表补齐了日期。", "details": ""}`)
	s := &Summarizer{Command: []string{cmd}, Model: "m", MaxChars: 40000}
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "feishu-transcript.json"), []byte(`{"segments": [{"speaker": "S1", "start_s": 9, "text": "借出登记表第二页缺了日期。"}]}`), 0o644)
	rec := map[string]any{"id": "m", "source": "feishu", "title": "录音 7", "summary": "飞书写的摘要", "state": "aligned"}
	before := maps.Clone(rec)
	p, err := s.Stage(context.Background(), "m", dir, rec)
	if err != nil {
		t.Fatal(err)
	}
	p(rec)
	for k, v := range before {
		if rec[k] != v {
			t.Fatalf("%s changed: %v → %v", k, v, rec[k])
		}
	}
	if len(rec) != len(before)+2 || rec["display_title"] != "登记表补日期" || !slices.Equal(rec["labels"].([]string), []string{"工具墙"}) {
		t.Fatalf("rec %v", rec)
	}
}

// A transcribed recording with no words gets the empty title and no labels
// without a model call.
func TestSummarizeNoSpeechSkipsTheModel(t *testing.T) {
	cmd, stdin := fakeClaude(t, `{}`)
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "segments.json"), []byte(`[{"track": "media", "start_s": 1, "text": "  "}]`), 0o644)
	sum, err := (&Summarizer{Command: []string{cmd}, Model: "m"}).Summarize(context.Background(), dir, map[string]any{"source": "file"})
	if err != nil || sum.DisplayTitle != EmptyTitle || sum.Labels == nil || len(sum.Labels) != 0 {
		t.Fatalf("%+v, %v", sum, err)
	}
	if _, err := os.Stat(stdin); err == nil {
		t.Fatal("the model was asked about a recording with no speech")
	}
}

// pickLabels never coins a type, keeps one type (the answer's own first),
// takes the offered spelling of a known label, keeps one new label, and
// holds maxLabels.
func TestPickLabels(t *testing.T) {
	v := labels.Vocab{List: workshop(), Counts: map[string]int{"工具墙": 3, "电钻": 2, "链锯": 2, "砂纸": 1}}
	for _, c := range []struct {
		typ  string
		ls   []string
		want []string
	}{
		{"焊台", []string{"", "  ", "焊台", "砂带机", strings.Repeat("钳", labelMax+1)}, []string{"焊台"}},
		{"入门课", []string{"tl-40", "Tl-40", "工具墙"}, []string{"入门课", "TL-40", "工具墙"}},
		{"", []string{"修理夜", "入门课", "工具墙"}, []string{"修理夜", "工具墙"}},
		{"", []string{"入门课", "TL-40", "工具墙", "电钻", "链锯", "砂纸", "焊台"}, []string{"入门课", "TL-40", "工具墙", "电钻", "链锯"}},
		{" 修理夜 ", []string{"#入门课"}, []string{"修理夜"}},
	} {
		got := pickLabels(c.typ, c.ls, v)
		if !slices.Equal(got, c.want) {
			t.Errorf("pickLabels(%q, %q) = %q, want %q", c.typ, c.ls, got, c.want)
		}
		if n := len(slices.DeleteFunc(slices.Clone(got), func(l string) bool { return !v.List.IsType(l) })); n > 1 {
			t.Errorf("%d types: %q", n, got)
		}
	}
}

// fakeClaudeSeq is a claude CLI that answers the nth call with answers[n]
// (the last one from then on) and saves each call's stdin as stdin.<n>.
func fakeClaudeSeq(t *testing.T, answers ...string) (cmd, dir string) {
	t.Helper()
	dir = t.TempDir()
	script := "#!/bin/sh\nn=$(cat " + dir + "/n 2>/dev/null || echo 0); n=$((n+1)); echo $n > " + dir + "/n\ncat > " + dir + "/stdin.$n\ncase $n in\n"
	for i, a := range answers {
		c := strconv.Itoa(i + 1)
		if i == len(answers)-1 {
			c = "*"
		}
		res := strings.NewReplacer(`\`, `\\`, `"`, `\"`, "\n", `\n`).Replace(a)
		script += c + ") printf '%s\\n' '{\"type\":\"result\",\"is_error\":false,\"result\":\"" + res + "\",\"total_cost_usd\":0.01,\"modelUsage\":{\"m\":{\"inputTokens\":100,\"outputTokens\":10}}}';;\n"
	}
	cmd = filepath.Join(dir, "claude")
	if err := os.WriteFile(cmd, []byte(script+"esac\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	return cmd, dir
}

// workshopVocab offers the workshop's closed list and no label in use.
func workshopVocab(context.Context) (labels.Vocab, error) { return labels.Vocab{List: workshop()}, nil }

// retitleAnswer is a first answer with subjects (JSON) and title.
func retitleAnswer(subjects, title string) string {
	return `{"subjects": ` + subjects + `, "display_title": "` + title + `", "type": "", "labels": [], "gist": "焊台的地线松了，改用带锁扣的端子。", "details": "端子由管理员统一采购。"}`
}

// A title that breaks the title rule is asked for once more, from the
// model's own summary. A retry that breaks it too, or fails, falls back to
// the subjects that keep the rule (one when two run past 30 runes); with none,
// the title stays and TitleProblem says why.
func TestSummarizerRetitles(t *testing.T) {
	for _, c := range []struct {
		name, subjects, title, retry string
		want, problem                string
		calls                        string
	}{
		{name: "an empty retry title: both subjects", subjects: `["东墙灯管", "推车轮子"]`, title: "双周会", retry: `{"display_title": ""}`,
			want: "东墙灯管、推车轮子", calls: "2\n"},
		{name: "reporting words twice: the first subject alone, two being too long", subjects: `["东墙那排灯管全部换成暖白色并重新布线到二楼配电箱", "推车轮子送去校正"]`, title: "协调", retry: `{"display_title": "核心"}`,
			want: "东墙那排灯管全部换成暖白色并重新布线到二楼配电箱", calls: "2\n"},
		{name: "a kind word mid-title, then a bare type, no subjects", subjects: `[]`, title: "推车在日会上定", retry: `{"display_title": "修理夜"}`,
			want: "修理夜", problem: "a bare series name (修理夜)", calls: "2\n"},
		{name: "no subject named, then a retry whose type is stripped", subjects: `["推车轮子"]`, title: "后门的锁", retry: `{"display_title": "修理夜 | 推车轮子校正"}`,
			want: "推车轮子校正", calls: "2\n"},
		{name: "a bare type, then not JSON, no subject keeping the rule", subjects: `["核心协调"]`, title: "入门课", retry: "cannot say",
			want: "入门课", problem: "a bare series name (入门课); the retry failed: the model's answer is not the JSON asked for (10 bytes)", calls: "2\n"},
		{name: "the subject named in another letter case", subjects: `["lathe 夹头"]`, title: "Lathe 夹头要换",
			want: "Lathe 夹头要换", calls: "1\n"},
		{name: "the subject named without its space", subjects: `["砂轮机 防护罩"]`, title: "砂轮机防护罩到了",
			want: "砂轮机防护罩到了", calls: "1\n"},
	} {
		answers := []string{retitleAnswer(c.subjects, c.title)}
		if c.retry != "" {
			answers = append(answers, c.retry)
		}
		cmd, dir := fakeClaudeSeq(t, answers...)
		s := &Summarizer{Command: []string{cmd}, Model: "m", MaxChars: 40000, Vocab: workshopVocab}
		sum, err := s.Summarize(context.Background(), diarized(t), map[string]any{"source": "file"})
		n, _ := readFile(filepath.Join(dir, "n"))
		if err != nil || sum.DisplayTitle != c.want || sum.TitleProblem != c.problem || n != c.calls {
			t.Errorf("%s: title %q, problem %q, calls %q, %v", c.name, sum.DisplayTitle, sum.TitleProblem, n, err)
		}
	}
}

// The retry reads the rejected title, the rule it broke, the subjects and
// the summary, not the transcript, and its cost adds to the first call's.
func TestSummarizerRetryReadsTheSummary(t *testing.T) {
	cmd, dir := fakeClaudeSeq(t, retitleAnswer(`["焊台接地"]`, "入门课"), `{"display_title": "「焊台接地线」"}`)
	s := &Summarizer{Command: []string{cmd}, Model: "m", MaxChars: 40000, Vocab: workshopVocab}
	sum, err := s.Summarize(context.Background(), diarized(t), map[string]any{"source": "file"})
	if err != nil || sum.DisplayTitle != "焊台接地线" || sum.TitleProblem != "" || sum.USD != 0.02 || sum.InTokens != 200 || sum.OutTokens != 20 {
		t.Fatalf("%+v, %v", sum, err)
	}
	retry, _ := readFile(filepath.Join(dir, "stdin.2"))
	want := "Meeting types: 修理夜, 入门课\nRejected title: 入门课\nRule it broke: a bare series name (入门课)\nSubjects: 焊台接地\n\nSummary:\n焊台的地线松了，改用带锁扣的端子。\n\n端子由管理员统一采购。\n"
	if retry != want {
		t.Fatalf("retry sent:\n%s", retry)
	}
}

// Stage logs a title that still breaks the rule after the retry.
func TestSummarizerStageLogsAKeptTitle(t *testing.T) {
	cmd, _ := fakeClaudeSeq(t, retitleAnswer(`["核心协调"]`, "砂带机换带的日会"), `{"display_title": "入门课"}`)
	var logged []string
	s := &Summarizer{Command: []string{cmd}, Model: "m", MaxChars: 40000, Vocab: workshopVocab, Logf: func(f string, a ...any) { logged = append(logged, fmt.Sprintf(f, a...)) }}
	rec := map[string]any{"source": "file"}
	p, err := s.Stage(context.Background(), "t5", diarized(t), rec)
	if err != nil {
		t.Fatal(err)
	}
	p(rec)
	if rec["display_title"] != "入门课" || len(logged) != 2 ||
		logged[1] != `summary rec.t5: display title "入门课" breaks the title rule after a retry (a bare series name (入门课)): kept` {
		t.Fatalf("rec %v, logged %q", rec, logged)
	}
}

func TestSummarizerRefusesANonJSONAnswer(t *testing.T) {
	cmd, _ := fakeClaude(t, "Sorry, I cannot.")
	s := &Summarizer{Command: []string{cmd}, Model: "m", MaxChars: 40000}
	if _, err := s.Summarize(context.Background(), diarized(t), map[string]any{"source": "file"}); err == nil || strings.Contains(err.Error(), "Sorry") {
		t.Fatalf("err %v (the answer's text stays out of the error)", err)
	}
}

// A transcript over the limit keeps its head and tail and marks the cut.
func TestTranscriptExcerpt(t *testing.T) {
	var r Record
	for i := 0; i < 100; i++ {
		r.Turns = append(r.Turns, Turn{Speaker: "a", StartS: float64(i * 60), Text: strings.Repeat("字", 90)})
	}
	text, cut := transcriptText(r, 3000)
	if !cut || utf8.RuneCountInString(text) > 3300 || !strings.Contains(text, "[0:00:00] a:") || !strings.Contains(text, "[1:39:00] a:") || !strings.Contains(text, "left out for length") {
		t.Fatalf("cut %v, %d runes:\n%s", cut, utf8.RuneCountInString(text), text)
	}
	if _, cut := transcriptText(r, 100000); cut {
		t.Fatal("a short transcript is cut")
	}
}

// The drain summarizes a processed meeting before the ingest reads it, a
// feishu one too; a failed summary leaves the record's run alone.
func TestDrainSummarizes(t *testing.T) {
	m, f, l := newMem(), &fake{}, &logs{}
	uploaded(m, t, "a", map[string]any{"title": ""})
	uploaded(m, t, "b", map[string]any{"source": "feishu", "summary": "会后纪要"})
	uploaded(m, t, "c", map[string]any{"title": "given"})
	st := f.stages(true)
	var seen string
	ingest := st.Ingest
	st.Ingest = func(ctx context.Context, id, dir string, rec map[string]any, replace bool) (Wiki, error) {
		if id == "a" {
			seen, _ = rec["display_title"].(string)
		}
		return ingest(ctx, id, dir, rec, replace)
	}
	st.Summarize = func(_ context.Context, id, dir string, rec map[string]any) (Patch, error) {
		f.note("summarize " + id)
		if id == "c" {
			return nil, errors.New("claude: Not logged in")
		}
		return func(raw map[string]any) error {
			raw["display_title"] = "generated"
			if raw["source"] != "feishu" {
				raw["summary"] = "gist\n\ndetails"
			}
			return nil
		}, nil
	}
	d := newDrain(m, st, "host-a:1", l)
	d.IngestSources = []string{"mac"}
	var notes []string
	d.Notify = func(s string) { notes = append(notes, s) }
	if _, err := d.Tick(context.Background()); err != nil {
		t.Fatal(err)
	}
	if a := m.rec(t, "a"); a["title"] != "" || a["display_title"] != "generated" || a["summary"] != "gist\n\ndetails" || a["state"] != "ingested" || seen != "generated" {
		t.Fatalf("a: %v, the ingest saw display_title %q", a, seen)
	}
	if b := m.rec(t, "b"); b["summary"] != "会后纪要" || b["display_title"] != "generated" || !strings.Contains(strings.Join(f.calls, ","), "summarize b") {
		t.Fatalf("feishu b: %v; calls %v", b, f.calls)
	}
	if c := m.rec(t, "c"); c["title"] != "given" || c["summary"] != nil || c["state"] != "ingested" {
		t.Fatalf("c: %v", c)
	}
	if len(notes) != 1 || !strings.Contains(notes[0], "rec.c «given» not titled or summarized: claude: Not logged in") {
		t.Fatalf("notes %q", notes)
	}
}
