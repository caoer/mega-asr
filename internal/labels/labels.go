// Package labels is the meetings page's label vocabulary: the closed list,
// settled by consent and stored as the registry record `labels` (labels@1), and
// the open list, every other label on a live record, derived and never
// stored. A record's meeting type is its label that is a closed `type`
// entry, at most one. The package reads both lists, applies the type rule,
// and changes labels on records by a CAS touching `labels` alone, logging
// each change as an append-only `labellog.<stamp>` record.
package labels

import (
	"context"
	"encoding/json"
	"fmt"
	"regexp"
	"slices"
	"sort"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/caoer/mega-asr/internal/pages"
)

// Registry is the page's record store.
type Registry interface {
	List(ctx context.Context, prefix string) ([]pages.Object, error)
	Record(ctx context.Context, key string) (pages.Object, error)
	CAS(ctx context.Context, key, schema string, data any, version int) (int, error)
}

const (
	Key       = "labels"
	Schema    = "labels@1"
	LogPrefix = "labellog."
	LogSchema = "labellog@1"
	recSchema = "meeting@1"
)

// Kinds a closed entry may have.
var Kinds = []string{"type", "project", "person", "topic"}

// SeedTypes is the closed list's meeting types while `labels` is absent:
// none. A fresh store names no meeting type until one is promoted.
var SeedTypes = []string{}

// Entry is one closed label.
type Entry struct {
	Name string `json:"name"`
	Kind string `json:"kind"`
}

// Question is one question awaiting an answer. Proposal is the `megameet
// labels` command it asks consent for (none for a free question); `apply`
// runs it once the question is approved. Status: open → approved | declined
// | expired; approved → applied. The meetings page answers too: it writes
// status, answer and answered on the record itself, as `labels question`
// does. Error is the last failed apply of an approved question, cleared
// when one succeeds.
type Question struct {
	ID       string   `json:"id"`
	Asked    string   `json:"asked"`
	Text     string   `json:"text"`
	Proposal []string `json:"proposal,omitempty"`
	Ask      string   `json:"ask,omitempty"` // where it is asked: "page", or a Telegram ask's id
	Status   string   `json:"status"`
	Answer   string   `json:"answer,omitempty"`
	Answered string   `json:"answered,omitempty"`
	Applied  string   `json:"applied,omitempty"`
	Error    string   `json:"error,omitempty"`
}

// List is the `labels` record.
type List struct {
	Closed    []Entry    `json:"closed"`
	Questions []Question `json:"questions"`
	UpdatedAt string     `json:"updated_at,omitempty"`
	UpdatedBy string     `json:"updated_by,omitempty"`
}

// Seed is the closed list before `labels` exists.
func Seed() List {
	l := List{Closed: []Entry{}, Questions: []Question{}}
	for _, t := range SeedTypes {
		l.Closed = append(l.Closed, Entry{Name: t, Kind: "type"})
	}
	return l
}

// Find is the closed entry named name.
func (l List) Find(name string) (Entry, bool) {
	for _, e := range l.Closed {
		if e.Name == name {
			return e, true
		}
	}
	return Entry{}, false
}

// IsType says whether name is a closed type.
func (l List) IsType(name string) bool {
	e, ok := l.Find(name)
	return ok && e.Kind == "type"
}

// Types is the closed types, in list order.
func (l List) Types() []string {
	var out []string
	for _, e := range l.Closed {
		if e.Kind == "type" {
			out = append(out, e.Name)
		}
	}
	return out
}

// Question is the question with id.
func (l *List) Question(id string) (*Question, error) {
	for i := range l.Questions {
		if l.Questions[i].ID == id {
			return &l.Questions[i], nil
		}
	}
	return nil, fmt.Errorf("no question %s", id)
}

// Read reads the `labels` record; absent, it is the seed at version 0.
func Read(ctx context.Context, reg Registry) (List, int, error) {
	o, err := reg.Record(ctx, Key)
	if pages.Code(err) == "no_such_object" {
		return Seed(), 0, nil
	}
	if err != nil {
		return List{}, 0, err
	}
	var l List
	if err := json.Unmarshal(o.Value.Data, &l); err != nil {
		return List{}, 0, fmt.Errorf("%s: %w", Key, err)
	}
	if l.Closed == nil {
		l.Closed = []Entry{}
	}
	if l.Questions == nil {
		l.Questions = []Question{}
	}
	return l, o.Version, nil
}

// Clean trims and de-duplicates labels, order kept, and applies the type
// rule: of the closed types among them only the first stays.
func Clean(labels []string, l List) []string {
	out := []string{}
	seen := map[string]bool{}
	typed := false
	for _, s := range labels {
		s = strings.Join(strings.Fields(s), " ")
		if s == "" || seen[s] {
			continue
		}
		if l.IsType(s) {
			if typed {
				continue
			}
			typed = true
		}
		seen[s] = true
		out = append(out, s)
	}
	return out
}

// TypeOf is the record's meeting type: its first label that is a closed
// type, or "".
func TypeOf(labels []string, l List) string {
	for _, s := range labels {
		if l.IsType(s) {
			return s
		}
	}
	return ""
}

// StripType takes a meeting type out of a display title where it heads or
// ends it, either bracketed or set off by a separator: with the type 巡箱,
// "【巡箱】蜂螨防治" and "蜂螨防治 / 巡箱" both become "蜂螨防治". A title
// that is only a type stays as it is.
func StripType(title string, types []string) string {
	ts := slices.Clone(types)
	sort.Slice(ts, func(i, j int) bool { return utf8.RuneCountInString(ts[i]) > utf8.RuneCountInString(ts[j]) })
	const sep = " ：:|｜-—–·/、,，"
	out := strings.TrimSpace(title)
	for changed := true; changed; {
		changed = false
		for _, t := range ts {
			for _, b := range [][2]string{{"[", "]"}, {"【", "】"}, {"（", "）"}, {"(", ")"}, {"「", "」"}} {
				w := b[0] + t + b[1]
				if s, ok := strings.CutPrefix(out, w); ok {
					out, changed = s, true
				}
				if s, ok := strings.CutSuffix(out, w); ok {
					out, changed = s, true
				}
			}
			if s, ok := strings.CutPrefix(out, t); ok && (s == "" || strings.ContainsAny(firstRune(s), sep)) {
				out, changed = s, true
			}
			if s, ok := strings.CutSuffix(out, t); ok && (s == "" || strings.ContainsAny(lastRune(s), sep)) {
				out, changed = s, true
			}
			out = strings.Trim(out, sep)
		}
	}
	if out == "" {
		return strings.TrimSpace(title)
	}
	return out
}

// KindWords name a kind of meeting without being a closed type. A title
// holding one names the meeting by its kind.
var KindWords = []string{"早会", "晨会", "夕会", "站会", "例会", "周会", "双周会", "月会", "日会",
	"总结会", "同步会", "碰头会", "启动会", "复盘会", "评审会", "讨论会", "分享会", "对齐会", "standup", "stand-up"}

// seriesFiller is what a series name holds besides its kind: the week, the
// weekday, the month, and words that say nothing of the content.
var seriesFiller = regexp.MustCompile(`第\s*\d+\s*周|周[一二三四五六日天]|\d+\s*月份?|每周|双周|工作|日常|团队|技术|项目|会议|[\s：:|｜\-—–·/、,，()（）\[\]【】「」]`)

// activityWords say that people met and reported, not what about: a title
// made of them and the series filler alone ("交流安排", "Updates")
// names no subject.
var activityWords = regexp.MustCompile(`(?i)进展|进度|同步|汇报|运营|协调|业务|多|规划|计划|安排|总结|回顾|讨论|沟通|对齐|交流|更新|推进|情况|事项|任务|待办|重点|核心|研发|条线|各项|各|本周|上周|下周|今日|昨日|当日|近期|例行|常规|整体|相关|及|与|和|的|\b(?:sync|updates?|progress|status|weekly|daily|and)\b|&`)

// NamesSubject says whether a title names one of subjects: it shares a run
// of at least three runes with one, the words for meeting and reporting and
// the series filler cut out of both, without case: "冬季喂糖浆配比" names
// the subject "糖浆配比", "交流安排" names none. No subjects is true.
func NamesSubject(title string, subjects []string) bool {
	if len(subjects) == 0 {
		return true
	}
	mask := func(s string) []rune {
		s = strings.ToLower(s)
		return []rune(seriesFiller.ReplaceAllString(activityWords.ReplaceAllString(s, "\x00"), "\x00"))
	}
	t := mask(title)
	for _, sub := range subjects {
		u := mask(sub)
		// the longest common run, by dynamic programming over runes
		prev := make([]int, len(u)+1)
		for i := range t {
			cur := make([]int, len(u)+1)
			for j := range u {
				if t[i] == u[j] && t[i] != 0 {
					if cur[j+1] = prev[j] + 1; cur[j+1] >= 3 {
						return true
					}
				}
			}
			prev = cur
		}
	}
	return false
}

// TitleProblem says why a display title breaks the title rule, "" when it
// keeps it. The rule: the title names what the meeting covered, so it holds
// no closed type and no kind word anywhere, even beside a real subject, does
// not end in 会议, is more than a series name ("双周巡箱") or a date, and
// names a subject: some word is left once the week, day and month and the
// words for meeting and reporting are taken out. Latin letters match
// without case.
func TitleProblem(title string, types []string) string {
	t := strings.ToLower(strings.TrimSpace(title))
	var hits []string
	words := append(slices.Clone(types), KindWords...)
	sort.SliceStable(words, func(i, j int) bool { return utf8.RuneCountInString(words[i]) > utf8.RuneCountInString(words[j]) })
	for _, w := range words {
		if w != "" && strings.Contains(t, strings.ToLower(w)) {
			t = strings.ReplaceAll(t, strings.ToLower(w), " ")
			hits = append(hits, w)
		}
	}
	if strings.HasSuffix(t, "会议") {
		hits = append(hits, "会议")
	}
	rest := seriesFiller.ReplaceAllString(t, "")
	switch {
	case t != "" && !strings.ContainsFunc(rest, unicode.IsLetter):
		if len(hits) == 0 {
			return "says nothing of what was covered"
		}
		return "a bare series name (" + strings.Join(hits, ", ") + ")"
	case len(hits) > 0:
		return "names the meeting's kind: " + strings.Join(hits, ", ")
	case t != "" && !strings.ContainsFunc(seriesFiller.ReplaceAllString(activityWords.ReplaceAllString(t, " "), ""), unicode.IsLetter):
		return "names no subject, only that the team met and reported"
	}
	return ""
}

func firstRune(s string) string {
	_, n := utf8.DecodeRuneInString(s)
	return s[:n]
}

func lastRune(s string) string {
	_, n := utf8.DecodeLastRuneInString(s)
	return s[len(s)-n:]
}

// Rec is a live record as the label tools see it.
type Rec struct {
	Key          string
	Version      int
	ID           string
	Title        string
	DisplayTitle string
	Gist         string // the summary's first line
	Labels       []string
	Labelled     bool // it carries labels ([] counts); absent is pending
	raw          map[string]any
}

func decodeRec(o pages.Object) (Rec, bool, error) {
	var raw map[string]any
	if err := json.Unmarshal(o.Value.Data, &raw); err != nil {
		return Rec{}, false, fmt.Errorf("%s: %w", o.Key, err)
	}
	if raw["state"] == "deleted" {
		return Rec{}, false, nil
	}
	r := Rec{Key: o.Key, Version: o.Version, raw: raw, Labelled: raw["labels"] != nil}
	r.ID, _ = raw["id"].(string)
	r.Title, _ = raw["title"].(string)
	r.DisplayTitle, _ = raw["display_title"].(string)
	sum, _ := raw["summary"].(string)
	r.Gist, _, _ = strings.Cut(strings.TrimSpace(sum), "\n")
	if ls, ok := raw["labels"].([]any); ok {
		for _, x := range ls {
			if s, ok := x.(string); ok {
				r.Labels = append(r.Labels, s)
			}
		}
	}
	return r, true, nil
}

// Records is every live (non-deleted) record, in key order.
func Records(ctx context.Context, reg Registry) ([]Rec, error) {
	objs, err := reg.List(ctx, "rec.")
	if err != nil {
		return nil, err
	}
	var out []Rec
	for _, o := range objs {
		r, live, err := decodeRec(o)
		if err != nil || !live {
			continue
		}
		out = append(out, r)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Key < out[j].Key })
	return out, nil
}

// Counts is how many records carry each label.
func Counts(recs []Rec) map[string]int {
	n := map[string]int{}
	for _, r := range recs {
		for _, s := range r.Labels {
			if s = strings.TrimSpace(s); s != "" {
				n[s]++
			}
		}
	}
	return n
}

// Vocab is both lists: the closed one and the count of every label in use.
type Vocab struct {
	List   List
	Counts map[string]int
}

// ReadVocab reads the closed list and the labels in use.
func ReadVocab(ctx context.Context, reg Registry) (Vocab, error) {
	l, _, err := Read(ctx, reg)
	if err != nil {
		return Vocab{}, err
	}
	recs, err := Records(ctx, reg)
	if err != nil {
		return Vocab{}, err
	}
	return Vocab{List: l, Counts: Counts(recs)}, nil
}

// Open is the open list: the labels in use that are not closed, most used
// first.
func (v Vocab) Open() []string {
	var out []string
	for s, n := range v.Counts {
		if _, closed := v.List.Find(s); !closed && n > 0 {
			out = append(out, s)
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if v.Counts[out[i]] != v.Counts[out[j]] {
			return v.Counts[out[i]] > v.Counts[out[j]]
		}
		return out[i] < out[j]
	})
	return out
}

// Offer is what the model may label with: the closed types, and the other
// labels (the closed non-type entries, then the open list, most used first).
func (v Vocab) Offer() (types, others []string) {
	types = v.List.Types()
	for _, e := range v.List.Closed {
		if e.Kind != "type" {
			others = append(others, e.Name)
		}
	}
	return types, append(others, v.Open()...)
}

// Violation is a record that breaks the type rule: two closed types or more.
type Violation struct {
	ID     string
	Labels []string
	Types  []string
}

// Violations is every record carrying more than one closed type.
func Violations(recs []Rec, l List) []Violation {
	var out []Violation
	for _, r := range recs {
		var ts []string
		for _, s := range r.Labels {
			if l.IsType(s) {
				ts = append(ts, s)
			}
		}
		if len(ts) > 1 {
			out = append(out, Violation{ID: r.ID, Labels: r.Labels, Types: ts})
		}
	}
	return out
}
