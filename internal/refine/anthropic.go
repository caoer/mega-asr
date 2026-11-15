package refine

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// Anthropic is a Provider over the Messages API, at Anthropic or behind a
// gateway that speaks it: POST BaseURL/v1/messages.
type Anthropic struct {
	BaseURL string       // https://api.anthropic.com, or a gateway's Anthropic route
	Key     string       // sent as x-api-key
	Client  *http.Client // nil: one that keeps its connection between takes
}

// kept keeps a connection to the provider open through a take and between
// takes: Warm opens it as a take starts, and the call at its end uses it.
var kept = func() *http.Client {
	t := http.DefaultTransport.(*http.Transport).Clone()
	t.IdleConnTimeout = 10 * time.Minute
	return &http.Client{Transport: t}
}()

// maxTokens bounds the answer: an edit list and the thinking before it.
const maxTokens = 8000

type block struct {
	Type  string `json:"type"`
	Text  string `json:"text"`
	Cache *cache `json:"cache_control,omitempty"`
}

type cache struct {
	Type string `json:"type"`
}

type message struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

type messagesRequest struct {
	Model     string    `json:"model"`
	MaxTokens int       `json:"max_tokens"`
	System    []block   `json:"system,omitempty"`
	Messages  []message `json:"messages"`
	Thinking  struct {
		Type string `json:"type"`
	} `json:"thinking"`
	Output struct {
		Effort string `json:"effort"`
		Format struct {
			Type   string          `json:"type"`
			Schema json.RawMessage `json:"schema"`
		} `json:"format"`
	} `json:"output_config"`
}

// editsSchema constrains the answer to an edit list: a model cannot answer
// the dictation in its place.
const editsSchema = `{"type":"object","properties":{"edits":{"type":"array","items":{"type":"object",` +
	`"properties":{"from":{"type":"string"},"why":{"type":"string"},"to":{"type":"string"}},` +
	`"required":["from","why","to"],"additionalProperties":false}}},"required":["edits"],"additionalProperties":false}`

// textSchema constrains a FullText answer to the text alone.
const textSchema = `{"type":"object","properties":{"text":{"type":"string"}},"required":["text"],"additionalProperties":false}`

// thinking is the thinking type and the effort the API takes for a model at
// the step's effort. off: Haiku 5.5 takes disabled, Sonnet 5.5 refuses that
// and takes between_tools, and Opus 5.5, Fable and Mythos cannot stop
// thinking, so they run at their lowest effort.
func thinking(model, effort string) (kind, apiEffort string) {
	if effort != "off" {
		return "adaptive", effort
	}
	switch {
	case strings.HasPrefix(model, "claude-sonnet-5-5"):
		return "between_tools", "low"
	case strings.HasPrefix(model, "claude-opus-5-5"), strings.HasPrefix(model, "claude-fable"), strings.HasPrefix(model, "claude-mythos"):
		return "adaptive", "low"
	}
	return "disabled", "low"
}

// body is req as the Messages API takes it. The same Request gives the same
// bytes, so the system blocks are read from the cache on the next take.
func (a Anthropic) body(req Request) ([]byte, error) {
	m := messagesRequest{Model: req.Model, MaxTokens: maxTokens,
		Messages: []message{{Role: "user", Content: req.UserContent()}}}
	for _, s := range req.System {
		m.System = append(m.System, block{Type: "text", Text: s})
	}
	if n := len(m.System); n > 0 {
		m.System[n-1].Cache = &cache{Type: "ephemeral"}
	}
	m.Thinking.Type, m.Output.Effort = thinking(req.Model, req.Effort)
	m.Output.Format.Type, m.Output.Format.Schema = "json_schema", json.RawMessage(editsSchema)
	if req.Output == FullText {
		m.Output.Format.Schema = json.RawMessage(textSchema)
	}
	return json.Marshal(m)
}

func (a Anthropic) client() *http.Client {
	if a.Client != nil {
		return a.Client
	}
	return kept
}

// Warm opens the connection the next call will use, without a call to a
// model, so that call does not wait for the handshake.
func (a Anthropic) Warm(ctx context.Context) error {
	hr, err := http.NewRequestWithContext(ctx, http.MethodHead, a.BaseURL, nil)
	if err != nil {
		return err
	}
	resp, err := a.client().Do(hr)
	if err != nil {
		return err
	}
	return resp.Body.Close()
}

func (a Anthropic) Refine(ctx context.Context, req Request) (Reply, error) {
	body, err := a.body(req)
	if err != nil {
		return Reply{}, err
	}
	hr, err := http.NewRequestWithContext(ctx, http.MethodPost, strings.TrimRight(a.BaseURL, "/")+"/v1/messages", bytes.NewReader(body))
	if err != nil {
		return Reply{}, err
	}
	hr.Header.Set("x-api-key", a.Key)
	hr.Header.Set("anthropic-version", "2023-06-01")
	hr.Header.Set("content-type", "application/json")
	resp, err := a.client().Do(hr)
	if err != nil {
		return Reply{}, err
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if err != nil {
		return Reply{}, err
	}
	if resp.StatusCode != http.StatusOK {
		return Reply{}, &HTTPError{Status: resp.StatusCode, Body: fmt.Sprintf("%.300s", b)}
	}
	var out struct {
		Content []struct {
			Type string `json:"type"`
			Text string `json:"text"`
		} `json:"content"`
		StopReason  string `json:"stop_reason"`
		StopDetails *struct {
			Category string `json:"category"`
		} `json:"stop_details"`
		Usage Usage `json:"usage"`
	}
	if err := json.Unmarshal(b, &out); err != nil {
		return Reply{}, fmt.Errorf("%w: %v", ErrUnparseable, err)
	}
	reply := Reply{Usage: out.Usage}
	switch out.StopReason {
	case "end_turn":
	case "refusal":
		if d := out.StopDetails; d != nil && d.Category != "" {
			return reply, fmt.Errorf("%w (%s)", ErrRefusal, d.Category)
		}
		return reply, ErrRefusal
	default:
		return reply, fmt.Errorf("%w: stop_reason %q", ErrUnparseable, out.StopReason)
	}
	var text strings.Builder
	for _, c := range out.Content {
		if c.Type == "text" {
			text.WriteString(c.Text)
		}
	}
	var answer struct {
		Edits *[]Edit `json:"edits"`
		Text  *string `json:"text"`
	}
	err = json.Unmarshal([]byte(text.String()), &answer)
	switch {
	case err == nil && req.Output == FullText && answer.Text != nil:
		reply.Text = *answer.Text
	case err == nil && req.Output != FullText && answer.Edits != nil:
		reply.Edits = *answer.Edits
	default:
		return reply, fmt.Errorf("%w: %.200q", ErrUnparseable, text.String())
	}
	return reply, nil
}
