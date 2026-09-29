package drain

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/caoer/mega-asr/internal/labels"
)

// GistMax is the most runes the summary's first paragraph holds: the page's
// row shows it on one clamped line.
const GistMax = 140

// titleMax bounds a generated display title.
const titleMax = 60

// Label rules: a meeting gets at most maxLabels, each at most labelMax runes.
const (
	maxLabels = 5
	labelMax  = 16
)

// Summarizer titles, labels and summarizes a processed meeting with a cheap
// model through the claude CLI (on the server, the launcher the ingest
// uses). The transcript goes to the model provider and nowhere else: nothing
// of it is logged.
type Summarizer struct {
	Command  []string // the claude CLI; the call's flags are appended
	Model    string   // e.g. claude-haiku-4-5
	MaxChars int      // transcript runes sent at most; a longer meeting sends its head and tail
	Timeout  time.Duration
	// Vocab reads the closed list and the labels in use; nil offers the
	// seed's closed list alone.
	Vocab func(ctx context.Context) (labels.Vocab, error)
	Logf  func(format string, args ...any)
}

// Summary is one answer.
type Summary struct {
	DisplayTitle string
	Subjects     []string // what the meeting spent most time on, most first; the title names one
	Type         string   // the model's meeting type; Labels holds it when it is a closed type
	Labels       []string
	Summary      string // gist, a blank line, details
	Content      string // what the model read: feishu-transcript | asr, "+feishu-summary"; or feishu-summary
	Model        string
	USD          float64
	InTokens     int
	OutTokens    int
	Excerpted    bool // the transcript was longer than MaxChars
	// TitleProblem is why DisplayTitle breaks the title rule after the one
	// retry (labels.TitleProblem); "" when it keeps it.
	TitleProblem string
}

// ErrNoContent is a record with no transcript and no Feishu summary yet.
var ErrNoContent = errors.New("no transcript and no Feishu summary to read")

// errNoSpeech is a transcribed record with no words in its transcripts and
// no Feishu summary: it gets EmptyTitle and no labels, with no model call.
var errNoSpeech = errors.New("the transcripts are empty")

// EmptyTitle is the display title of a recording with no speech.
const EmptyTitle = "空录音（无语音）"

const summaryPrompt = `You title, label and summarize one meeting from its content.
Reply with exactly one JSON object and nothing else:
{"subjects": ["..."], "display_title": "...", "type": "...", "labels": ["..."], "gist": "...", "details": "..."}
- Write them in the language most of the meeting is spoken in (a Chinese meeting gets Chinese, an English one English; keep English terms as spoken).
- subjects: the one to three things this meeting spent most time on, most first: a decision, product, place, project or person, each a few words (旧锅炉拆除, 春季招新名额). Never a word for meeting or reporting (协调, 核心): a meeting that went round many topics still has one or two it spent most on.
- display_title: a short, informative title that tells this meeting apart from the others in a list: its topic, or the project and what was decided, or who met and why. At most 30 characters. A title, not a sentence or summary: no date, no quotes, no trailing punctuation. The recording name is often generic (新录音, 新录音 2, xx的视频会议, Recording 20xx…): never repeat such a name. Never put a meeting type or a word for a kind of meeting in the title (the page shows the type beside the title): no word from the meeting types list, no 日会, 双周会, no title ending in 会议, not even as the subject (name what was discussed instead). A recurring meeting is titled by what this one covered, never by its series name (松果俱乐部, Open Bench). It names one or two of the subjects, written as in subjects: subjects [旧锅炉拆除, 春季招新名额] give a title such as 「春季招新名额定为二十人」, whatever the series is called; a title made only of words for meeting and reporting names no subject.
- type: the meeting's type, copied exactly from the meeting types list, or "" when none fits. Only when the meeting is of that type: a one-to-one type only for two people talking, a talk or training type only for one speaker sharing knowledge with an audience, a named recurring meeting only for that meeting. Never a type that is not in the list.
- labels: 1 to 4 short labels to find the meeting by: the project(s) it is substantially about, the person met for a 1:1 or an interview. No meeting type here. Two labels that fit beat four loose ones. Reuse a label from the label list whenever one fits, written exactly as listed. Add at most one label that is not in the list, and only for a project or person the list lacks, never a meeting type. No "#".
- gist: one sentence, at most 100 characters: what the meeting was about and its main outcome.
- details: 2-3 short sentences, at most 200 characters in all: the decisions, the open questions, who does what next. Plain text, no headings or lists.
- Say only what the content supports. A transcript is machine speech recognition: read through its misrecognized words.`

// Summarize asks the model for the meeting's display title, labels and
// summary.
func (s *Summarizer) Summarize(ctx context.Context, dir string, rec map[string]any) (Summary, error) {
	text, content, excerpted, err := meetingText(dir, rec, s.MaxChars)
	if errors.Is(err, errNoSpeech) {
		return Summary{DisplayTitle: EmptyTitle, Labels: []string{}, Content: "none: no speech"}, nil
	}
	if err != nil {
		return Summary{}, err
	}
	vocab := labels.Vocab{List: labels.Seed()}
	if s.Vocab != nil {
		if vocab, err = s.Vocab(ctx); err != nil {
			return Summary{}, fmt.Errorf("the labels in use: %w", err)
		}
	}
	types, others := vocab.Offer()
	text = "Meeting types: " + strings.Join(types, ", ") + "\nLabel list (most used first): " + strings.Join(others, ", ") + "\n" + text
	if s.Timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, s.Timeout)
		defer cancel()
	}
	out, err := s.ask(ctx, dir, summaryPrompt, text)
	if err != nil {
		return Summary{}, err
	}
	sum, err := parseAnswer(out)
	sum.Excerpted, sum.Content = excerpted, content
	if err != nil {
		return sum, err
	}
	sum.Labels = pickLabels(sum.Type, sum.Labels, vocab)
	sum.DisplayTitle = labels.StripType(sum.DisplayTitle, types)
	if why := titleProblem(sum.DisplayTitle, sum.Subjects, types); why != "" {
		s.retitle(ctx, dir, &sum, types, why)
	}
	return sum, nil
}

const retitlePrompt = `You write the display title of one meeting from its summary.
Reply with exactly one JSON object and nothing else: {"display_title": "..."}
- A short, informative title naming what this meeting covered: its topic, or the project and what was decided. At most 30 characters, in the summary's language. No date, no quotes, no trailing punctuation.
- It names one or two of the meeting's subjects, written as listed. A title made only of words for meeting and reporting names no subject.
- The rejected title broke the rule given with it. Never put a meeting type or a word for a kind of meeting in the title: none of the meeting types listed, no 日会, 双周会, no title ending in 会议, not even as the subject (name what was discussed instead), and never a series name (松果俱乐部, Open Bench).`

// retitle asks once more, for the title alone, when the first one breaks
// the title rule; a second break takes the title from the subjects, or with
// none to take is kept, with TitleProblem saying why, for the writer to log. The retry reads the model's own summary, not the transcript: it
// costs a fraction of the first call.
func (s *Summarizer) retitle(ctx context.Context, dir string, sum *Summary, types []string, why string) {
	first := sum.DisplayTitle
	text := "Meeting types: " + strings.Join(types, ", ") + "\nRejected title: " + first + "\nRule it broke: " + why + "\nSubjects: " + strings.Join(sum.Subjects, ", ") + "\n\nSummary:\n" + sum.Summary + "\n"
	out, err := s.ask(ctx, dir, retitlePrompt, text)
	var title string
	if err == nil {
		var u Summary
		var res string
		if res, u, err = parseResult(out); err == nil {
			var ans struct {
				DisplayTitle string `json:"display_title"`
			}
			if err = json.Unmarshal([]byte(jsonBody(res)), &ans); err != nil {
				err = fmt.Errorf("the model's answer is not the JSON asked for (%d bytes)", len(res))
			}
			title = cleanTitle(ans.DisplayTitle)
		}
		sum.USD += u.USD
		sum.InTokens += u.InTokens
		sum.OutTokens += u.OutTokens
	}
	if err == nil && title != "" {
		title = labels.StripType(title, types)
		if why = titleProblem(title, sum.Subjects, types); why == "" {
			sum.DisplayTitle = title
			return
		}
		sum.DisplayTitle = title
	} else if err != nil {
		why += "; the retry failed: " + oneLine(err.Error())
	}
	if t := subjectTitle(sum.Subjects, types); t != "" {
		sum.DisplayTitle = t
		return
	}
	sum.TitleProblem = why
}

// subjectTitle is the title a second break falls back to: the model's first
// one or two subjects that keep the title rule, joined ("旧锅炉拆除、春季招新名额");
// "" when none does.
func subjectTitle(subjects, types []string) string {
	var ok []string
	for _, sub := range subjects {
		if labels.TitleProblem(sub, types) == "" && len(ok) < 2 {
			ok = append(ok, sub)
		}
	}
	if len(ok) == 2 && utf8.RuneCountInString(ok[0]+"、"+ok[1]) > 30 {
		ok = ok[:1]
	}
	return cleanTitle(strings.Join(ok, "、"))
}

// titleProblem is labels.TitleProblem, and then a title that names none of
// the meeting's subjects: the model's own list, so a title of meeting and
// reporting words alone fails however it is worded.
func titleProblem(title string, subjects, types []string) string {
	if why := labels.TitleProblem(title, types); why != "" {
		return why
	}
	if labels.NamesSubject(title, subjects) {
		return ""
	}
	return "names none of the meeting's subjects (" + strings.Join(subjects, ", ") + ")"
}

// ask runs the claude CLI once with system as its system prompt and text on
// its stdin.
func (s *Summarizer) ask(ctx context.Context, dir, system, text string) ([]byte, error) {
	args := append(append([]string(nil), s.Command[1:]...), "-p", "--model", s.Model, "--output-format", "json",
		"--setting-sources", "", "--strict-mcp-config", "--tools", "", "--system-prompt", system)
	cmd := exec.CommandContext(ctx, s.Command[0], args...)
	cmd.Dir = dir
	// No thinking and no prompt cache: thinking multiplies the output of a
	// long transcript, and the CLI's one-hour cache write bills the
	// transcript at twice the input price for a prompt read once.
	cmd.Env = append(os.Environ(), "MAX_THINKING_TOKENS=0", "DISABLE_PROMPT_CACHING=1")
	cmd.Stdin = strings.NewReader(text)
	var out, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &stderr
	if err := cmd.Run(); err != nil {
		return nil, fmt.Errorf("%s: %v: %s", s.Command[0], err, lastLine(stderr.String()))
	}
	return out.Bytes(), nil
}

// Stage is the drain's Summarize stage: the record gets display_title, and
// labels when it has none (labels set on the page stay); a non-feishu record gets
// summary (a Feishu minute keeps Feishu's). title is never written.
func (s *Summarizer) Stage(ctx context.Context, id, dir string, rec map[string]any) (Patch, error) {
	sum, err := s.Summarize(ctx, dir, rec)
	if err != nil {
		return nil, err
	}
	s.logf("summary rec.%s: %s from %s, %d in / %d out tokens, $%.4f%s", id, sum.Model, sum.Content, sum.InTokens, sum.OutTokens, sum.USD,
		map[bool]string{true: " (transcript excerpted)"}[sum.Excerpted])
	if sum.TitleProblem != "" {
		s.logf("summary rec.%s: display title %q breaks the title rule after a retry (%s): kept", id, sum.DisplayTitle, sum.TitleProblem)
	}
	return func(raw map[string]any) error {
		raw["display_title"] = sum.DisplayTitle
		if !HasLabels(raw) {
			raw["labels"] = sum.Labels
		}
		if raw["source"] != "feishu" && sum.Summary != "" {
			raw["summary"] = sum.Summary
		}
		return nil
	}, nil
}

// HasLabels says whether a record carries labels: [] is none chosen, absent
// (or null) is not labelled yet.
func HasLabels(raw map[string]any) bool { return raw["labels"] != nil }

// HasDisplayTitle says whether a record carries a display title.
func HasDisplayTitle(raw map[string]any) bool {
	t, _ := raw["display_title"].(string)
	return strings.TrimSpace(t) != ""
}

// meetingText is what the model reads about the meeting: its recording
// name, the label list's place, Feishu's summary for a minute that has one,
// and the transcript — Feishu's for a minute that carries a non-empty one,
// else ours. content names what it holds.
func meetingText(dir string, rec map[string]any, max int) (text, content string, excerpted bool, err error) {
	r, err := BuildRecord(dir, rec)
	content = "asr"
	if r.Engine == "Feishu Minutes" {
		content = "feishu-transcript"
		if err != nil { // Feishu's transcript is empty or unreadable: ours
			alt := maps.Clone(rec)
			alt["source"] = "asr"
			r, err = BuildRecord(dir, alt)
			content = "asr"
		}
	}
	title, _ := rec["title"].(string)
	started, _ := rec["started"].(string)
	head := fmt.Sprintf("Recording name: %s\nStarted: %s\n", title, started)
	var fsum string
	if rec["source"] == "feishu" {
		fsum, _ = rec["summary"].(string)
		fsum = strings.TrimSpace(fsum)
	}
	if fsum != "" {
		head += "\nFeishu's summary of the meeting:\n" + fsum + "\n"
	}
	if err != nil {
		switch {
		case fsum != "":
			return head, "feishu-summary", false, nil
		case exists(filepath.Join(dir, "segments.json")) || exists(filepath.Join(dir, "feishu-transcript.json")):
			return "", "", false, errNoSpeech
		}
		return "", "", false, ErrNoContent
	}
	if fsum != "" {
		content += "+feishu-summary"
	}
	lines, excerpted := transcriptText(r, max)
	return head + "\nTranscript:\n" + lines, content, excerpted, nil
}

// pickLabels is the record's labels from the model's answer: its type
// when it is a closed type (else the first closed type among its labels),
// then its other labels trimmed, de-duplicated and bounded, order kept. A
// label equal to an offered one but for case takes the offered spelling;
// only the first label offered nowhere survives; no meeting type is coined.
func pickLabels(typ string, ls []string, v labels.Vocab) []string {
	types, others := v.Offer()
	fold := func(list []string) map[string]string {
		m := map[string]string{}
		for _, l := range list {
			m[strings.ToLower(l)] = l
		}
		return m
	}
	isType, known := fold(types), fold(others)
	norm := func(l string) string {
		return strings.TrimSpace(strings.TrimLeft(strings.Join(strings.Fields(l), " "), "#"))
	}
	out := []string{}
	if t, ok := isType[strings.ToLower(norm(typ))]; ok {
		out = append(out, t)
	}
	coined := false
	for _, l := range ls {
		l = norm(l)
		if l == "" || utf8.RuneCountInString(l) > labelMax {
			continue
		}
		k := strings.ToLower(l)
		if t, ok := isType[k]; ok {
			out = append(out, t) // labels.Clean keeps the first type alone
			continue
		}
		if listed, ok := known[k]; ok {
			l = listed
		} else if coined {
			continue
		} else {
			coined = true
		}
		out = append(out, l)
	}
	out = labels.Clean(out, v.List)
	if len(out) > maxLabels {
		out = out[:maxLabels]
	}
	return out
}

// transcriptText is the transcript as "[h:mm:ss] speaker: text" lines; one
// longer than max runes keeps its first two thirds and its last third of
// max, with a line marking the cut.
func transcriptText(r Record, max int) (string, bool) {
	lines := make([]string, len(r.Turns))
	total := 0
	for i, t := range r.Turns {
		sp := t.Speaker
		if name := r.Speakers[sp]; name != "" {
			sp = name
		}
		lines[i] = fmt.Sprintf("[%s] %s: %s", clock(t.StartS), sp, strings.Join(strings.Fields(t.Text), " "))
		total += utf8.RuneCountInString(lines[i]) + 1
	}
	if max <= 0 || total <= max {
		return strings.Join(lines, "\n"), false
	}
	var a, b int // lines[:a] and lines[b:] are kept
	for n := 0; a < len(lines); a++ {
		if n += utf8.RuneCountInString(lines[a]) + 1; n > max*2/3 {
			break
		}
	}
	b = len(lines)
	for n := 0; b > a; b-- {
		if n += utf8.RuneCountInString(lines[b-1]) + 1; n > max/3 {
			break
		}
	}
	if b <= a {
		b = a + 1
	}
	cut := fmt.Sprintf("[… %s to %s left out for length …]", clock(r.Turns[a].StartS), clock(r.Turns[b-1].StartS))
	return strings.Join(append(append(append([]string(nil), lines[:a]...), cut), lines[b:]...), "\n"), true
}

func clock(s float64) string {
	d := time.Duration(s) * time.Second
	return fmt.Sprintf("%d:%02d:%02d", int(d.Hours()), int(d.Minutes())%60, int(d.Seconds())%60)
}

// parseAnswer reads claude's JSON result (the last line that is a JSON
// object: a launcher may print its own lines first) and the model's answer
// inside it.
func parseAnswer(out []byte) (Summary, error) {
	result, sum, err := parseResult(out)
	if err != nil {
		return sum, err
	}
	var ans struct {
		Subjects     []string `json:"subjects"`
		DisplayTitle string   `json:"display_title"`
		Type         string   `json:"type"`
		Labels       []string `json:"labels"`
		Gist         string   `json:"gist"`
		Details      string   `json:"details"`
	}
	if err := json.Unmarshal([]byte(jsonBody(result)), &ans); err != nil {
		return sum, fmt.Errorf("the model's answer is not the JSON asked for (%d bytes): %v", len(result), err)
	}
	gist := clamp(strings.Join(strings.Fields(ans.Gist), " "), GistMax)
	if gist == "" {
		return sum, errors.New("the model's answer has no gist")
	}
	sum.DisplayTitle = cleanTitle(ans.DisplayTitle)
	if sum.DisplayTitle == "" {
		return sum, errors.New("the model's answer has no display_title")
	}
	sum.Type, sum.Labels = ans.Type, ans.Labels
	for _, sub := range ans.Subjects {
		if sub = strings.Join(strings.Fields(sub), " "); sub != "" {
			sum.Subjects = append(sum.Subjects, sub)
		}
	}
	sum.Summary = gist
	if d := strings.TrimSpace(ans.Details); d != "" {
		sum.Summary += "\n\n" + d
	}
	return sum, nil
}

// jsonBody is the JSON object inside a model answer (a fenced one too).
func jsonBody(s string) string {
	if i, j := strings.Index(s, "{"), strings.LastIndex(s, "}"); i >= 0 && j > i {
		return s[i : j+1]
	}
	return s
}

// cleanTitle is a model's title on one line, unquoted and bounded.
func cleanTitle(t string) string {
	return clamp(strings.Trim(strings.Join(strings.Fields(t), " "), "\"'“”「」《》"), titleMax)
}

// parseResult reads claude's JSON result: the model's answer text, and the
// model, cost and tokens in a Summary.
func parseResult(out []byte) (string, Summary, error) {
	var res struct {
		IsError    bool    `json:"is_error"`
		Result     string  `json:"result"`
		USD        float64 `json:"total_cost_usd"`
		ModelUsage map[string]struct {
			InputTokens              int `json:"inputTokens"`
			OutputTokens             int `json:"outputTokens"`
			CacheReadInputTokens     int `json:"cacheReadInputTokens"`
			CacheCreationInputTokens int `json:"cacheCreationInputTokens"`
		} `json:"modelUsage"`
	}
	lines := strings.Split(strings.TrimSpace(string(out)), "\n")
	found := false
	for i := len(lines) - 1; i >= 0 && !found; i-- {
		if l := strings.TrimSpace(lines[i]); strings.HasPrefix(l, "{") {
			found = json.Unmarshal([]byte(l), &res) == nil
		}
	}
	if !found {
		return "", Summary{}, fmt.Errorf("claude printed no JSON result (%d bytes)", len(out))
	}
	var sum Summary
	sum.USD = res.USD
	for m, u := range res.ModelUsage {
		sum.Model = m
		sum.InTokens += u.InputTokens + u.CacheReadInputTokens + u.CacheCreationInputTokens
		sum.OutTokens += u.OutputTokens
	}
	if res.IsError {
		return "", sum, fmt.Errorf("claude: %s", oneLine(res.Result))
	}
	return res.Result, sum, nil
}

// clamp cuts s to at most n runes, ending a cut one with "…".
func clamp(s string, n int) string {
	if utf8.RuneCountInString(s) <= n {
		return s
	}
	return string([]rune(s)[:n-1]) + "…"
}

func lastLine(s string) string {
	s = strings.TrimSpace(s)
	if i := strings.LastIndex(s, "\n"); i >= 0 {
		s = s[i+1:]
	}
	return oneLine(s)
}

func (s *Summarizer) logf(format string, args ...any) {
	if s.Logf != nil {
		s.Logf(format, args...)
	}
}
