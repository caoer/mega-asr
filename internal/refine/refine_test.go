package refine

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// fake is a Provider that answers from a function.
type fake func(ctx context.Context, req Request) (Reply, error)

func (f fake) Refine(ctx context.Context, req Request) (Reply, error) { return f(ctx, req) }

// An edit is made wherever its From occurs in the text as it stood, and
// nowhere else: one whose From is absent or equals its To is dropped, and
// an edit never rewrites what an earlier one wrote.
func TestApplyEditsWhereFromOccurs(t *testing.T) {
	text := "用海库跑一下，海库比 Sonnet 快，然后转移一下"
	out, applied, dropped, err := Apply(text, []Edit{
		{From: "海库", To: "Haiku"},
		{From: "转移一下", To: "转译一下"},
		{From: "康胖", To: "compound"},   // not in the text
		{From: "Sonnet", To: "Sonnet"}, // nothing to change
		{From: "Haiku", To: "hive"},    // only in what the first edit wrote
	})
	if err != nil {
		t.Fatal(err)
	}
	if want := "用Haiku跑一下，Haiku比 Sonnet 快，然后转译一下"; out != want {
		t.Fatalf("got  %q\nwant %q", out, want)
	}
	if len(applied) != 2 || applied[0].From != "海库" || applied[1].To != "转译一下" || dropped != 3 {
		t.Fatalf("applied %+v, dropped %d", applied, dropped)
	}
	if out, applied, _, err := Apply(text, nil); out != text || applied != nil || err != nil {
		t.Fatalf("no edits: %q %+v %v", out, applied, err)
	}
}

// The fix-words guards reject a rewrite whole: a From that changes a long
// stretch, a To far longer than what it replaces, or edits that together
// change a large share of the text. The unchanged words a model quotes
// around a mis-hearing do not count.
func TestGuardsRejectARewrite(t *testing.T) {
	long := strings.Repeat("我们今天下午要把这个功能做完然后发布出去。", 3)
	for _, tc := range []struct {
		name, text string
		edits      []Edit
		want       string // "" applies
	}{
		{"a mis-heard term", "不要用 ask your question 因为问了之后你会卡住", []Edit{{From: "ask your question", To: "AskUserQuestion"}}, ""},
		{"quoted context around it", long + "不要用 ask your question 因为问了之后你会卡住",
			[]Edit{{From: "不要用 ask your question 因为问了之后你会卡住", To: "不要用 AskUserQuestion 因为问了之后你会卡住"}}, ""},
		{"two terms that are most of a short line", "Cooper Netties 上的 Reddis", []Edit{{From: "Cooper Netties", To: "Kubernetes"}, {From: "Reddis", To: "Redis"}}, ""},
		{"a sentence rewritten", long, []Edit{{From: "我们今天下午要把这个功能做完然后发布出去。我们今天下午要把这个功能做完然后发布出去。", To: "今天下午发布。"}}, "rewrites 41 characters"},
		{"an answer in place of a word", "这个怎么配置", []Edit{{From: "配置", To: "配置：先打开设置，再选择模型，然后填入你自己的 API key"}}, "in place of"},
		{"most of the text changed", long, []Edit{{From: "今天下午要把这个功能", To: "明天上午得把那个模块"}, {From: "做完然后发布出去", To: "写好之后部署上线"}}, "of 63 characters"},
	} {
		out, applied, _, err := Apply(tc.text, tc.edits)
		switch {
		case tc.want == "" && (err != nil || len(applied) != len(tc.edits) || out == tc.text):
			t.Errorf("%s: %q, applied %d, %v", tc.name, out, len(applied), err)
		case tc.want != "" && (err == nil || !strings.Contains(err.Error(), tc.want) || out != tc.text || applied != nil):
			t.Errorf("%s: %q, applied %d, err %v; want the text as it was and an error with %q", tc.name, out, len(applied), err, tc.want)
		}
	}
}

// Whatever keeps a step from an answer it can apply, the text comes back
// as it was and the result says why.
func TestStepKeepsTheTextAndSaysWhy(t *testing.T) {
	const text = "用海库跑一下"
	for _, tc := range []struct {
		why string
		p   fake
	}{
		{WhyTimeout, func(ctx context.Context, _ Request) (Reply, error) { <-ctx.Done(); return Reply{}, ctx.Err() }},
		{WhyHTTP, func(context.Context, Request) (Reply, error) {
			return Reply{}, &HTTPError{Status: 529, Body: "overloaded"}
		}},
		{WhyRefusal, func(context.Context, Request) (Reply, error) { return Reply{}, ErrRefusal }},
		{WhyParse, func(context.Context, Request) (Reply, error) { return Reply{}, ErrUnparseable }},
		{WhyGuard, func(context.Context, Request) (Reply, error) {
			return Reply{Edits: []Edit{{From: "海库", To: "Haiku 是 Anthropic 的小模型，适合做这种校对工作"}}}, nil
		}},
		{WhyError, func(context.Context, Request) (Reply, error) { return Reply{}, errors.New("no route to host") }},
	} {
		r := Step{Provider: tc.p, Model: "m", Effort: "low", System: []string{"p"}, Timeout: 30 * time.Millisecond}.Run(context.Background(), text)
		if r.Text != text || r.Why != tc.why || r.Err == nil || r.Edits != nil {
			t.Errorf("%s: text %q, why %q, err %v, edits %+v", tc.why, r.Text, r.Why, r.Err, r.Edits)
		}
	}
	r := Step{Provider: fake(func(_ context.Context, req Request) (Reply, error) {
		if req.Dictation != text || req.Model != "m" || req.Effort != "low" || len(req.System) != 1 {
			t.Errorf("request %+v", req)
		}
		return Reply{Edits: []Edit{{From: "海库", To: "Haiku", Why: "模型名"}}, Usage: Usage{Input: 900, Output: 40}}, nil
	}), Model: "m", Effort: "low", System: []string{"p"}, Timeout: time.Second}.Run(context.Background(), text)
	if r.Text != "用Haiku跑一下" || r.Why != "" || r.Err != nil || len(r.Edits) != 1 || r.Usage.Input != 900 {
		t.Fatalf("applied: %+v", r)
	}
}

// messages is a Messages API endpoint that answers every POST with status
// and body, and keeps the last request it got.
type messages struct {
	*httptest.Server
	status int
	body   string
	got    map[string]any
	header http.Header
	conns  atomic.Int32
}

func newMessages(t *testing.T) *messages {
	m := &messages{status: 200}
	m.Server = httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			return
		}
		if r.URL.Path != "/anthropic/v1/messages" {
			t.Errorf("path %s", r.URL.Path)
		}
		b, _ := io.ReadAll(r.Body)
		m.got, m.header = nil, r.Header
		if err := json.Unmarshal(b, &m.got); err != nil {
			t.Errorf("request body: %v", err)
		}
		w.WriteHeader(m.status)
		io.WriteString(w, m.body)
	}))
	m.Config.ConnState = func(_ net.Conn, s http.ConnState) {
		if s == http.StateNew {
			m.conns.Add(1)
		}
	}
	m.Start()
	t.Cleanup(m.Close)
	return m
}

func (m *messages) provider() Anthropic {
	return Anthropic{BaseURL: m.URL + "/anthropic", Key: "vk-test", Client: m.Client()}
}

func answer(stop, text string) string {
	b, _ := json.Marshal(map[string]any{
		"content":     []map[string]string{{"type": "thinking", "thinking": ""}, {"type": "text", "text": text}},
		"stop_reason": stop,
		"usage":       map[string]int{"input_tokens": 12, "cache_read_input_tokens": 4400, "output_tokens": 350},
	})
	return string(b)
}

// The request is the one the trial measured: the system blocks in order
// with the cache breakpoint on the last, the dictation inside its tag
// after the context, adaptive thinking at the step's effort, and the edit
// list as the output's JSON schema.
func TestAnthropicRequest(t *testing.T) {
	m := newMessages(t)
	m.body = answer("end_turn", `{"edits":[{"from":"海库","why":"模型名","to":"Haiku"}]}`)
	reply, err := m.provider().Refine(context.Background(), Request{Model: "claude-haiku-5-5", Effort: "low",
		System: []string{"prompt", "glossary"}, Context: "screen", Dictation: "用海库"})
	if err != nil {
		t.Fatal(err)
	}
	if len(reply.Edits) != 1 || reply.Edits[0] != (Edit{From: "海库", Why: "模型名", To: "Haiku"}) || reply.Usage != (Usage{Input: 12, CacheRead: 4400, Output: 350}) {
		t.Fatalf("reply %+v", reply)
	}
	if m.header.Get("x-api-key") != "vk-test" || m.header.Get("anthropic-version") != "2023-06-01" {
		t.Errorf("headers %v", m.header)
	}
	body, _ := json.Marshal(m.got)
	for _, want := range []string{
		`"model":"claude-haiku-5-5"`,
		`"system":[{"text":"prompt","type":"text"},{"cache_control":{"type":"ephemeral"},"text":"glossary","type":"text"}]`,
		`"messages":[{"content":"\u003ccontext\u003e\nscreen\n\u003c/context\u003e\n\u003cdictation\u003e\n用海库\n\u003c/dictation\u003e","role":"user"}]`,
		`"thinking":{"type":"adaptive"}`,
		`"effort":"low"`,
		`"format":{"schema":{"additionalProperties":false,"properties":{"edits":`,
		`"type":"json_schema"`,
	} {
		if !strings.Contains(string(body), want) {
			t.Errorf("request lacks %s:\n%s", want, body)
		}
	}
	for _, key := range []string{"temperature", "top_p", "top_k", "stream"} {
		if _, ok := m.got[key]; ok {
			t.Errorf("request sends %s", key)
		}
	}
}

// off is no thinking where the model allows it: disabled on Haiku 5.5,
// between_tools on Sonnet 5.5, and the lowest effort on Opus 5.5, which
// cannot stop thinking.
func TestThinkingPerModel(t *testing.T) {
	for _, tc := range []struct{ model, effort, kind, api string }{
		{"claude-haiku-5-5", "low", "adaptive", "low"},
		{"claude-haiku-5-5", "high", "adaptive", "high"},
		{"claude-haiku-5-5", "off", "disabled", "low"},
		{"claude-sonnet-5-5", "off", "between_tools", "low"},
		{"claude-opus-5-5", "off", "adaptive", "low"},
		{"claude-sonnet-5-5", "medium", "adaptive", "medium"},
	} {
		if kind, api := thinking(tc.model, tc.effort); kind != tc.kind || api != tc.api {
			t.Errorf("%s at %s: thinking %s, effort %s; want %s, %s", tc.model, tc.effort, kind, api, tc.kind, tc.api)
		}
	}
}

// A status other than 200, a refusal, a cut-off answer and an answer that
// is not an edit list are each their own error; none gives edits.
func TestAnthropicFailures(t *testing.T) {
	m := newMessages(t)
	var he *HTTPError
	for _, tc := range []struct {
		name   string
		status int
		body   string
		is     func(error) bool
	}{
		{"overloaded", 529, `{"type":"error","error":{"type":"overloaded_error"}}`, func(err error) bool { return errors.As(err, &he) && he.Status == 529 }},
		{"refusal", 200, `{"content":[],"stop_reason":"refusal","stop_details":{"type":"refusal","category":"cyber"},"usage":{"input_tokens":9}}`,
			func(err error) bool { return errors.Is(err, ErrRefusal) && strings.Contains(err.Error(), "cyber") }},
		{"cut off", 200, answer("max_tokens", `{"edits":[{"from":"海`), func(err error) bool { return errors.Is(err, ErrUnparseable) }},
		{"an answer to the dictation", 200, answer("end_turn", "好的，我来帮你配置。"), func(err error) bool { return errors.Is(err, ErrUnparseable) }},
		{"no edits key", 200, answer("end_turn", `{"text":"用 Haiku"}`), func(err error) bool { return errors.Is(err, ErrUnparseable) }},
		{"not JSON", 200, "<html>bad gateway</html>", func(err error) bool { return errors.Is(err, ErrUnparseable) }},
	} {
		m.status, m.body = tc.status, tc.body
		reply, err := m.provider().Refine(context.Background(), Request{Model: "claude-haiku-5-5", Effort: "low", System: []string{"p"}, Dictation: "用海库"})
		if err == nil || !tc.is(err) || reply.Edits != nil {
			t.Errorf("%s: reply %+v, err %v", tc.name, reply, err)
		}
	}
	m.status, m.body = 200, answer("end_turn", `{"edits":[]}`)
	if reply, err := m.provider().Refine(context.Background(), Request{Model: "claude-haiku-5-5", Effort: "low", Dictation: "好"}); err != nil || len(reply.Edits) != 0 {
		t.Errorf("nothing to change: %+v, %v", reply, err)
	}
}

// Warm opens the connection without a call to a model, and the call that
// follows uses it.
func TestWarmOpensTheConnectionTheCallUses(t *testing.T) {
	m := newMessages(t)
	m.body = answer("end_turn", `{"edits":[]}`)
	p := m.provider()
	if err := p.Warm(context.Background()); err != nil {
		t.Fatal(err)
	}
	if m.got != nil || m.conns.Load() != 1 {
		t.Fatalf("after Warm: a request %v, %d connections", m.got, m.conns.Load())
	}
	if _, err := p.Refine(context.Background(), Request{Model: "claude-haiku-5-5", Effort: "low", System: []string{"p"}, Dictation: "好"}); err != nil {
		t.Fatal(err)
	}
	if n := m.conns.Load(); n != 1 {
		t.Fatalf("%d connections opened, want the one Warm opened", n)
	}
}
