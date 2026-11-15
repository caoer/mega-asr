package refine

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
)

// Tidy and translate ask for the whole text and deliver it when it passes
// their guard; an answer past it, or an empty one, delivers the dictation
// as it stood and says why.
func TestFullTextModesUnderTheirGuards(t *testing.T) {
	const spoken = "嗯那个我们明天就是说开会讨论一下那个 Redis 的问题"
	for _, tc := range []struct {
		mode, in, answer string
		delivered        bool
	}{
		{"tidy", spoken, "我们明天开会，讨论一下 Redis 的问题。", true},
		{"tidy", "um so I think we we should ship it tomorrow", "So I think we should ship it tomorrow.", true},
		// a reply in place of the dictation keeps too little of it
		{"tidy", "Redis 的持久化怎么配置", "Redis 的持久化可以用 RDB 快照或者 AOF 日志两种方式来配置，在 redis.conf 里设置 save 和 appendonly。", false},
		{"tidy", "我们明天开会讨论一下", "明天", false},
		{"tidy", spoken, "", false},
		{"translate", "我们明天开会讨论一下 Redis 的问题", "We meet tomorrow to discuss the Redis issue.", true},
		{"translate", "Let's ship the release tomorrow morning after the review", "评审之后明天上午发版", true},
		{"translate", "好的", "Okay.", true},
		{"translate", "我们明天开会讨论一下 Redis 的问题", strings.Repeat("We meet tomorrow to discuss the Redis issue. ", 6), false},
		{"translate", strings.Repeat("我们明天开会讨论一下 Redis 的问题。", 4), "Yes.", false},
	} {
		var got Request
		p := fake(func(_ context.Context, req Request) (Reply, error) { got = req; return Reply{Text: tc.answer}, nil })
		r := Step{Provider: p, Mode: tc.mode, Model: "m", Effort: "low", System: []string{"p"}}.Run(context.Background(), tc.in)
		if got.Output != FullText {
			t.Errorf("%s %q: asked for %q, want the full text", tc.mode, tc.answer, got.Output)
		}
		switch {
		case tc.delivered && (r.Text != tc.answer || r.Why != "" || r.Err != nil):
			t.Errorf("%s %q: delivered %q, why %q, err %v; want the answer", tc.mode, tc.answer, r.Text, r.Why, r.Err)
		case !tc.delivered && (r.Text != tc.in || r.Why != WhyGuard || r.Err == nil):
			t.Errorf("%s %q: delivered %q, why %q, err %v; want the dictation and the guard", tc.mode, tc.answer, r.Text, r.Why, r.Err)
		}
	}
	// fix-words, and a step with no mode, ask for an edit list as before
	for _, mode := range []string{"", "fix-words"} {
		var got Request
		p := fake(func(_ context.Context, req Request) (Reply, error) { got = req; return Reply{Text: "ignored"}, nil })
		r := Step{Provider: p, Mode: mode}.Run(context.Background(), "用海库")
		if got.Output != EditList || r.Text != "用海库" || r.Why != "" {
			t.Errorf("mode %q: asked for %q, delivered %q, why %q", mode, got.Output, r.Text, r.Why)
		}
	}
}

// A full-text request carries the text schema, and its answer is the text;
// an edit list where a text was asked for does not parse.
func TestAnthropicFullText(t *testing.T) {
	m := newMessages(t)
	m.body = answer("end_turn", `{"text":"我们明天开会。"}`)
	req := Request{Model: "claude-haiku-5-5", Effort: "low", System: []string{"p"}, Dictation: "嗯我们明天开会", Output: FullText}
	reply, err := m.provider().Refine(context.Background(), req)
	if err != nil || reply.Text != "我们明天开会。" || reply.Edits != nil {
		t.Fatalf("reply %+v, err %v", reply, err)
	}
	body, _ := json.Marshal(m.got)
	if want := `"format":{"schema":{"additionalProperties":false,"properties":{"text":{"type":"string"}},"required":["text"]`; !strings.Contains(string(body), want) {
		t.Errorf("request lacks %s:\n%s", want, body)
	}
	m.body = answer("end_turn", `{"edits":[]}`)
	if _, err := m.provider().Refine(context.Background(), req); !errors.Is(err, ErrUnparseable) {
		t.Errorf("an edit list for a text: err %v", err)
	}
	m.body = answer("end_turn", `{"text":"x"}`)
	req.Output = EditList
	if _, err := m.provider().Refine(context.Background(), req); !errors.Is(err, ErrUnparseable) {
		t.Errorf("a text for an edit list: err %v", err)
	}
}
