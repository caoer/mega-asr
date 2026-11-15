package refine

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
)

// A text mode — tidy, translate, or any prompt file of another name — asks
// for the whole text and delivers what the model wrote, however much it
// reorganised the sentence; a blank answer delivers nothing of it and says
// why.
func TestTextModesDeliverWhatTheModelWrote(t *testing.T) {
	const spoken = "嗯那个我们明天就是说开会讨论一下那个 Redis 的问题"
	for _, tc := range []struct {
		mode, in, answer string
		delivered        bool
	}{
		{"tidy", spoken, "我们明天开会，讨论一下 Redis 的问题。", true},
		// the whole sentence reorganised: little of it left in order
		{"tidy", "就是那个上线的事情我觉得明天早上评审完了以后吧可以发", "评审结束后，明天上午发版。", true},
		{"tidy", "um so I think we we should ship it tomorrow", "Ship it tomorrow, I think.", true},
		{"translate", "我们明天开会讨论一下 Redis 的问题", "We meet tomorrow to discuss the Redis issue.", true},
		{"translate", strings.Repeat("我们明天开会讨论一下 Redis 的问题。", 4), "Meeting tomorrow on Redis.", true},
		{"meeting-notes", spoken, "- 明天开会\n- 议题：Redis", true},
		{"tidy", spoken, "", false},
		{"translate", "好的", " ", false},
		{"meeting-notes", spoken, "\n\t\u3000 \r\n", false},
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
		case !tc.delivered && (r.Text != tc.in || r.Why != WhyGuard || r.Err == nil || !strings.Contains(r.Err.Error(), "blank")):
			t.Errorf("%s %q: text %q, why %q, err %v; want the dictation kept and the answer called blank", tc.mode, tc.answer, r.Text, r.Why, r.Err)
		}
	}
}

// fix-words, and a step with no mode, ask for an edit list and apply it
// under its guards as before: the same lists apply, the same are refused.
func TestFixWordsIsUnchanged(t *testing.T) {
	const long = "我们今天下午要把这个功能做完然后发布出去。我们今天下午要把这个功能做完然后发布出去。"
	for _, tc := range []struct {
		in    string
		edits []Edit
		want  string
		why   string
	}{
		{"用海库跑一下", []Edit{{From: "海库", To: "Haiku"}}, "用Haiku跑一下", ""},
		{"用海库跑一下", nil, "用海库跑一下", ""},
		{long, []Edit{{From: long, To: "今天下午发布。"}}, long, WhyGuard},
		{"现在几点了", []Edit{{From: "现在几点了", To: "下午三点整"}}, "现在几点了", WhyGuard},
	} {
		for _, mode := range []string{"", FixWords} {
			var got Request
			p := fake(func(_ context.Context, req Request) (Reply, error) { got = req; return Reply{Edits: tc.edits}, nil })
			r := Step{Provider: p, Mode: mode}.Run(context.Background(), tc.in)
			if got.Output != EditList || r.Text != tc.want || r.Why != tc.why {
				t.Errorf("mode %q, %v on %q: asked for %q, delivered %q, why %q; want %q, why %q", mode, tc.edits, tc.in, got.Output, r.Text, r.Why, tc.want, tc.why)
			}
		}
	}
	if err := CheckText(FixWords, "好", "好"); err == nil {
		t.Error("CheckText passed a text for fix-words, which answers with an edit list")
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
