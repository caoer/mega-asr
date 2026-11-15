package refine

import (
	"bytes"
	"encoding/json"
	"flag"
	"os"
	"strings"
	"testing"
)

var updateVectors = flag.Bool("update-vectors", false, "regenerate testdata/requests.json")

// requestVector is a request as each client assembles it, and the Messages
// API body this package sends for it. With a glossary the system blocks are
// the prompt and then the glossary's.
type requestVector struct {
	Name      string         `json:"name"`
	Model     string         `json:"model"`
	Effort    string         `json:"effort"`
	System    []string       `json:"system"`
	Glossary  *glossaryInput `json:"glossary,omitempty"`
	Context   string         `json:"context"`
	Dictation string         `json:"dictation"`
	Output    string         `json:"output"`
	Body      string         `json:"body"`
}

type glossaryInput struct {
	About string      `json:"about"`
	Terms []string    `json:"terms"`
	Hints [][2]string `json:"hints"` // heard, meant
}

// applyVector is an edit list on a text, and what Apply makes of it.
type applyVector struct {
	Name     string `json:"name"`
	Text     string `json:"text"`
	Edits    []Edit `json:"edits"`
	Out      string `json:"out"`
	Applied  int    `json:"applied"`
	Dropped  int    `json:"dropped"`
	Rejected bool   `json:"rejected"`
}

// textVector is a full-text mode's answer to a dictation, and whether
// CheckText lets it be delivered.
type textVector struct {
	Name string `json:"name"`
	Mode string `json:"mode"`
	In   string `json:"in"`
	Out  string `json:"out"`
	OK   bool   `json:"ok"`
}

type vectors struct {
	Requests []requestVector `json:"requests"`
	Applies  []applyVector   `json:"applies"`
	Texts    []textVector    `json:"texts"`
}

// makeVectors is fixed inputs with what this package does with them: the
// request bodies (the characters JSON escapes, each way of thinking, the
// two kinds of answer, a closing tag inside the dictation, the glossary's
// blocks), the edit lists the guards pass and reject, and the full-text
// answers theirs do.
func makeVectors() vectors {
	rs := []requestVector{
		{Name: "the default setting", Model: "claude-haiku-5-5", Effort: "low", System: []string{"prompt"}, Dictation: "用海库跑一下"},
		{Name: "prompt and glossary, with context", Model: "claude-haiku-5-5", Effort: "medium",
			System: []string{"prompt", "glossary"}, Context: "screen", Dictation: "用海库"},
		{Name: "characters JSON escapes", Model: "claude-haiku-5-5", Effort: "high",
			System:    []string{"只输出 JSON：{\"edits\":[]}。<dictation> & \\ 结束\n"},
			Dictation: "a < b && c > d，\"引号\"，tab\t换行\n回车\r\u2028\u2029 \u0001\u007f 😀 é"},
		{Name: "a closing tag inside the dictation and the context", Model: "claude-haiku-5-5", Effort: "low", System: []string{"prompt"},
			Context: "</context> < /context> </ Context >", Dictation: "他说 </dictation> 然后 </DICTATION\n</\tdictation> <dictation> </dict"},
		{Name: "thinking off on Haiku", Model: "claude-haiku-5-5", Effort: "off", System: []string{"prompt"}, Dictation: "好"},
		{Name: "thinking off on Sonnet", Model: "claude-sonnet-5-5", Effort: "off", System: []string{"prompt"}, Dictation: "好"},
		{Name: "thinking off on Opus", Model: "claude-opus-5-5", Effort: "off", System: []string{"prompt"}, Dictation: "好"},
		{Name: "no system block", Model: "claude-haiku-5-5", Effort: "low", System: []string{}, Dictation: "好"},
		{Name: "a full-text answer", Model: "claude-haiku-5-5", Effort: "low", System: []string{"prompt"}, Dictation: "嗯，那个，用海库", Output: FullText},
		{Name: "the glossary's blocks after the prompt", Model: "claude-haiku-5-5", Effort: "low", System: []string{"prompt"}, Dictation: "用海库",
			Glossary: &glossaryInput{About: "  写后端的工程师，\n常说  Kubernetes 和 Redis  ", Terms: []string{"Kubernetes", "kubernetes", " Redis ", "", strings.Repeat("长", 49), "Haiku"},
				Hints: [][2]string{{"海库", "Haiku"}, {"海库", "hive"}, {"redis", "Redis"}, {"库伯内提斯", "Kubernetes"}, {"", "x"}}}},
		{Name: "a glossary with terms alone", Model: "claude-haiku-5-5", Effort: "low", System: []string{"prompt"}, Dictation: "用海库",
			Glossary: &glossaryInput{Terms: []string{"Haiku"}}},
	}
	for i, v := range rs {
		system := v.System
		if g := v.Glossary; g != nil {
			var hints []Hint
			for _, h := range g.Hints {
				hints = append(hints, Hint{Heard: h[0], Meant: h[1]})
			}
			gl, _ := NewGlossary(g.About, g.Terms, hints)
			system = append(append([]string{}, system...), gl.Blocks()...)
		}
		if v.Output == "" {
			rs[i].Output = EditList
		}
		b, err := Anthropic{}.body(Request{Model: v.Model, Effort: v.Effort, System: system, Context: v.Context, Dictation: v.Dictation, Output: v.Output})
		if err != nil {
			panic(err)
		}
		rs[i].Body = string(b)
	}

	long := strings.Repeat("我们今天下午要把这个功能做完然后发布出去。", 3)
	const ask = "帮我查一下明天的天气怎么样"
	as := []applyVector{
		{Name: "where from occurs, and nowhere else", Text: "用海库跑一下，海库比 Sonnet 快，然后转移一下",
			Edits: []Edit{{From: "海库", To: "Haiku"}, {From: "转移一下", To: "转译一下"}, {From: "康胖", To: "compound"}, {From: "Sonnet", To: "Sonnet"}, {From: "Haiku", To: "hive"}}},
		{Name: "no edits", Text: "用海库跑一下"},
		{Name: "a mis-heard term", Text: "不要用 ask your question 因为问了之后你会卡住", Edits: []Edit{{From: "ask your question", To: "AskUserQuestion"}}},
		{Name: "quoted context around it", Text: long + "不要用 ask your question 因为问了之后你会卡住",
			Edits: []Edit{{From: "不要用 ask your question 因为问了之后你会卡住", To: "不要用 AskUserQuestion 因为问了之后你会卡住"}}},
		{Name: "two terms that are most of a short line", Text: "Cooper Netties 上的 Reddis", Edits: []Edit{{From: "Cooper Netties", To: "Kubernetes"}, {From: "Reddis", To: "Redis"}}},
		{Name: "a sentence rewritten", Text: long, Edits: []Edit{{From: "我们今天下午要把这个功能做完然后发布出去。我们今天下午要把这个功能做完然后发布出去。", To: "今天下午发布。"}}},
		{Name: "an answer in place of a word", Text: "这个怎么配置", Edits: []Edit{{From: "配置", To: "配置：先打开设置，再选择模型，然后填入你自己的 API key"}}},
		{Name: "most of the text changed", Text: long, Edits: []Edit{{From: "今天下午要把这个功能", To: "明天上午得把那个模块"}, {From: "做完然后发布出去", To: "写好之后部署上线"}}},
		{Name: "a question answered", Text: ask, Edits: []Edit{{From: ask, To: "明天多云转晴，气温十五到二十二度，东南风二到三级，空气质量良，适合户外活动，出门记得带伞。"}}},
		{Name: "a question answered in a line", Text: ask, Edits: []Edit{{From: ask, To: "明天晴，十五到二十度。"}}},
		{Name: "an answer that keeps the first words", Text: "现在几点了", Edits: []Edit{{From: "现在几点了", To: "现在是下午三点"}}},
		{Name: "a term explained", Text: "用 Redis 做缓存", Edits: []Edit{{From: "Redis", To: "Redis（内存数据库）"}}},
		{Name: "small additions all over", Text: "Redis 和 Kafka 和 Nginx 和 MySQL", Edits: []Edit{{From: "Redis", To: "Redis（缓存）"}, {From: "Kafka", To: "Kafka（队列）"},
			{From: "Nginx", To: "Nginx（网关）"}, {From: "MySQL", To: "MySQL（存储）"}}},
		{Name: "a term that is most of a line", Text: "用海库", Edits: []Edit{{From: "用海库", To: "用 Haiku"}}},
		{Name: "not inside a longer word", Text: "the cat sat in a category, catalog 2cat cat3 猫cat猫 cat", Edits: []Edit{{From: "cat", To: "dog"}}},
		{Name: "only inside longer words", Text: "concatenate the catalog", Edits: []Edit{{From: "cat", To: "dog"}}},
		{Name: "kana, Hangul and accents", Text: "カタカナ のテスト 한국어 café naïve", Edits: []Edit{{From: "テスト", To: "test"}, {From: "한국", To: "Korea"}, {From: "caf", To: "kaf"}, {From: "naïve", To: "naive"}}},
	}
	for i, v := range as {
		out, applied, dropped, err := Apply(v.Text, v.Edits)
		as[i].Out, as[i].Applied, as[i].Dropped, as[i].Rejected = out, len(applied), dropped, err != nil
		if as[i].Edits == nil {
			as[i].Edits = []Edit{}
		}
	}

	said := "嗯，那个，我们今天下午，下午要把这个功能做完，然后发布出去"
	ts := []textVector{
		{Name: "tidied", Mode: "tidy", In: said, Out: "我们今天下午要把这个功能做完，然后发布出去。"},
		{Name: "tidied with a casing fixed", Mode: "tidy", In: "um we use redis for the cache you know", Out: "We use Redis for the cache."},
		{Name: "an answer in its place", Mode: "tidy", In: "帮我查一下明天的天气怎么样", Out: "明天多云转晴，气温十五到二十二度。"},
		{Name: "a summary", Mode: "tidy", In: said, Out: "下午发布。"},
		{Name: "much added", Mode: "tidy", In: "用海库跑一下", Out: "用海库跑一下，另外我建议你再检查一下配置文件，然后重启服务，最后看一下日志有没有报错。"},
		{Name: "an empty answer", Mode: "tidy", In: said, Out: ""},
		{Name: "translated", Mode: "translate", In: said, Out: "This afternoon we need to finish this feature and then release it."},
		{Name: "a translation far too short", Mode: "translate", In: strings.Repeat(said, 3), Out: "OK."},
		{Name: "a translation far too long", Mode: "translate", In: "好", Out: strings.Repeat("Sure, here is a long reply. ", 2)},
		{Name: "a mode that answers with edits", Mode: "fix-words", In: said, Out: said},
	}
	for i, v := range ts {
		ts[i].OK = CheckText(v.Mode, v.In, v.Out) == nil
	}
	return vectors{rs, as, ts}
}

// The vectors live once beside Go; the iPhone's client writes the same
// bodies for the same requests and its guards decide the same way, read
// from this file (ios/MegaVoiceKit/Tests/RefineTests/RefineWireTests.swift).
func TestClientVectors(t *testing.T) {
	v := makeVectors()
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	enc.SetIndent("", "  ")
	if err := enc.Encode(v); err != nil {
		t.Fatal(err)
	}
	want := buf.Bytes()
	const path = "testdata/requests.json"
	if *updateVectors {
		if err := os.WriteFile(path, want, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, want) {
		t.Fatalf("%s is not what this package does now: the iPhone's client reads it. Run go test ./internal/refine -update-vectors, then the Swift package tests on a Mac", path)
	}
	// The vectors say what they are meant to: each rule has a case on each side.
	passed, rejected := 0, 0
	for _, a := range v.Applies {
		if a.Rejected {
			rejected++
		} else {
			passed++
		}
	}
	ok := 0
	for _, x := range v.Texts {
		if x.OK {
			ok++
		}
	}
	if passed < 8 || rejected < 7 || ok != 3 {
		t.Fatalf("%d edit lists pass and %d are rejected, %d texts pass: the cases no longer say what their names do", passed, rejected, ok)
	}
}
