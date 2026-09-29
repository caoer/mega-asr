package labels

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"sort"
	"strings"
	"sync/atomic"
	"time"

	"github.com/caoer/mega-asr/internal/pages"
)

// Store changes labels. On records it writes by a CAS touching `labels`
// alone, re-reading on version_conflict; on the closed list by a CAS on
// `labels`. Every change that lands is logged as one `labellog.<stamp>`
// record, created once and never rewritten.
type Store struct {
	Reg    Registry
	By     string // who changes: the log's by and the closed list's updated_by
	Now    func() time.Time
	DryRun bool // plan and report; write nothing
}

// RecChange is one record's labels before and after.
type RecChange struct {
	ID     string   `json:"id"`
	Before []string `json:"before"`
	After  []string `json:"after"`
}

// Change is one change-log entry.
type Change struct {
	At           string      `json:"at"`
	By           string      `json:"by"`
	Op           []string    `json:"op"` // the command as run: ["rename", "巢础", "巢础片"]
	Reason       string      `json:"reason,omitempty"`
	Question     string      `json:"question,omitempty"` // the question whose consent it ran on
	ClosedBefore []Entry     `json:"closed_before,omitempty"`
	ClosedAfter  []Entry     `json:"closed_after,omitempty"`
	Records      []RecChange `json:"records"`
	Key          string      `json:"-"`
}

// ConsentError is a change that runs only on an approved question, asked
// for with `labels ask … -- <op>` and run by `labels apply`.
type ConsentError struct{ Why string }

func (e *ConsentError) Error() string {
	return "needs consent (" + e.Why + "): ask with `megameet labels ask --text … -- <this command>`, then `megameet labels apply <id>` once the question is approved"
}

// op is a parsed label command.
type op struct {
	name   string
	from   []string            // rename, merge, split, demote, promote: the labels acted on
	into   string              // rename, merge
	kind   string              // promote
	id     string              // set
	labels []string            // set
	assign map[string][]string // split: record id → the labels that replace from
}

// Usage names the label commands Run takes.
const Usage = `rename OLD NEW              every record's OLD becomes NEW
merge FROM... --into TO     every record's FROM labels become TO
split FROM ID=A,B ID2= ...  each record carrying FROM gets its own labels in FROM's place (every such record named; ID= drops it)
set ID [LABEL...]           one record's labels, whole (the type rule applied)
promote NAME KIND           NAME joins the closed list (KIND: type | project | person | topic)
demote NAME                 NAME leaves the closed list; records keep it as an open label`

func parse(argv []string) (op, error) {
	if len(argv) == 0 {
		return op{}, errors.New("no label command")
	}
	o := op{name: argv[0]}
	a := argv[1:]
	nonEmpty := func(ss ...string) error {
		for _, s := range ss {
			if strings.TrimSpace(s) == "" {
				return fmt.Errorf("%s: an empty label", o.name)
			}
		}
		return nil
	}
	switch o.name {
	case "rename":
		if len(a) != 2 {
			return o, errors.New("rename OLD NEW")
		}
		o.from, o.into = a[:1], a[1]
		return o, nonEmpty(a...)
	case "merge":
		i := slices.Index(a, "--into")
		if i < 1 || i != len(a)-2 {
			return o, errors.New("merge FROM... --into TO")
		}
		o.from, o.into = a[:i], a[i+1]
		return o, nonEmpty(append(slices.Clone(o.from), o.into)...)
	case "split":
		if len(a) < 2 {
			return o, errors.New("split FROM ID=A,B ...")
		}
		o.from, o.assign = a[:1], map[string][]string{}
		for _, s := range a[1:] {
			id, ls, ok := strings.Cut(s, "=")
			id = strings.TrimPrefix(strings.TrimSpace(id), "rec.")
			if !ok || id == "" {
				return o, fmt.Errorf("split: %q is not ID=A,B", s)
			}
			var out []string
			for _, l := range strings.Split(ls, ",") {
				if l = strings.TrimSpace(l); l != "" {
					out = append(out, l)
				}
			}
			o.assign[id] = out
		}
		return o, nonEmpty(a[0])
	case "set":
		if len(a) < 1 {
			return o, errors.New("set ID [LABEL...]")
		}
		o.id, o.labels = strings.TrimPrefix(a[0], "rec."), a[1:]
		return o, nonEmpty(a...)
	case "promote":
		if len(a) != 2 || !slices.Contains(Kinds, a[1]) {
			return o, errors.New("promote NAME KIND (type | project | person | topic)")
		}
		o.from, o.kind = a[:1], a[1]
		return o, nonEmpty(a[0])
	case "demote":
		if len(a) != 1 {
			return o, errors.New("demote NAME")
		}
		o.from = a
		return o, nonEmpty(a...)
	}
	return o, fmt.Errorf("%q is not a label command", o.name)
}

// consent says why the op needs consent, "" when it needs none: any
// change to the closed list, and any rename, merge or split naming a closed
// label. set, one record's labels, needs none.
func (o op) consent(l List) string {
	switch o.name {
	case "promote", "demote":
		return o.name + " changes the closed list"
	case "set":
		return ""
	}
	named := append(slices.Clone(o.from), o.into)
	for _, ls := range o.assign {
		named = append(named, ls...)
	}
	for _, n := range named {
		if _, ok := l.Find(n); ok {
			return fmt.Sprintf("%s names the closed label %q", o.name, n)
		}
	}
	return ""
}

// Run runs one label command (Usage). question is the approved question
// it runs on ("" outside `apply`); an op that needs consent refuses
// without one.
func (s *Store) Run(ctx context.Context, argv []string, reason, question string) (Change, error) {
	o, err := parse(argv)
	if err != nil {
		return Change{}, err
	}
	l, _, err := Read(ctx, s.Reg)
	if err != nil {
		return Change{}, err
	}
	if why := o.consent(l); why != "" && question == "" {
		return Change{}, &ConsentError{Why: why}
	}
	c := Change{Op: argv, Reason: reason, Question: question}
	switch o.name {
	case "rename", "merge":
		// The closed list first: a closed label merged into an open one is
		// renamed there, and closed ones merged into a closed one leave it.
		if err := s.closedMerge(ctx, &c, o.from, o.into); err != nil {
			return c, err
		}
		c.Records, err = s.relabel(ctx, func(r Rec) ([]string, bool) {
			if !slices.ContainsFunc(r.Labels, func(x string) bool { return slices.Contains(o.from, x) }) {
				return nil, false
			}
			var out []string
			for _, x := range r.Labels {
				if slices.Contains(o.from, x) {
					x = o.into
				}
				out = append(out, x)
			}
			return out, true
		})
	case "split":
		recs, rerr := Records(ctx, s.Reg)
		if rerr != nil {
			return c, rerr
		}
		var missing []string
		for _, r := range recs {
			if _, ok := o.assign[r.ID]; !ok && slices.Contains(r.Labels, o.from[0]) {
				missing = append(missing, r.ID)
			}
		}
		if len(missing) > 0 {
			return c, fmt.Errorf("split %s: every record carrying it needs its labels; missing %s", o.from[0], strings.Join(missing, " "))
		}
		c.Records, err = s.relabel(ctx, func(r Rec) ([]string, bool) {
			repl, ok := o.assign[r.ID]
			if !ok || !slices.Contains(r.Labels, o.from[0]) {
				return nil, false
			}
			var out []string
			for _, x := range r.Labels {
				if x == o.from[0] {
					out = append(out, repl...)
				} else {
					out = append(out, x)
				}
			}
			return out, true
		})
		if err == nil {
			err = s.closedDemote(ctx, &c, o.from[0], false)
		}
	case "set":
		found := false
		c.Records, err = s.relabel(ctx, func(r Rec) ([]string, bool) {
			if r.ID != o.id {
				return nil, false
			}
			found = true
			return o.labels, true
		})
		if err == nil && !found {
			err = fmt.Errorf("set: no live record %s", o.id)
		}
	case "promote":
		err = s.updateList(ctx, &c, func(l *List) error {
			if e, ok := l.Find(o.from[0]); ok {
				if e.Kind == o.kind {
					return nil
				}
				return fmt.Errorf("promote: %q is already closed, as %s", e.Name, e.Kind)
			}
			l.Closed = append(l.Closed, Entry{Name: o.from[0], Kind: o.kind})
			return nil
		})
	case "demote":
		err = s.closedDemote(ctx, &c, o.from[0], true)
	}
	if err != nil {
		return c, err
	}
	return c, s.log(ctx, &c)
}

// closedMerge is a merge's effect on the closed list.
func (s *Store) closedMerge(ctx context.Context, c *Change, from []string, into string) error {
	return s.updateList(ctx, c, func(l *List) error {
		_, intoClosed := l.Find(into)
		var out []Entry
		for _, e := range l.Closed {
			if slices.Contains(from, e.Name) {
				if intoClosed {
					continue
				}
				e.Name, intoClosed = into, true // the first closed one is renamed
			}
			out = append(out, e)
		}
		l.Closed = out
		return nil
	})
}

func (s *Store) closedDemote(ctx context.Context, c *Change, name string, must bool) error {
	return s.updateList(ctx, c, func(l *List) error {
		i := slices.IndexFunc(l.Closed, func(e Entry) bool { return e.Name == name })
		if i < 0 {
			if must {
				return fmt.Errorf("demote: %q is not on the closed list", name)
			}
			return nil
		}
		l.Closed = slices.Delete(l.Closed, i, i+1)
		return nil
	})
}

// updateList changes the `labels` record by fn, by CAS, retrying on
// version_conflict; an unchanged closed list writes nothing. A change to
// the closed list lands in c.
func (s *Store) updateList(ctx context.Context, c *Change, fn func(*List) error) error {
	for try := 0; ; try++ {
		l, v, err := Read(ctx, s.Reg)
		if err != nil {
			return err
		}
		before, _ := json.Marshal(l)
		next := l
		next.Closed = slices.Clone(l.Closed)
		next.Questions = slices.Clone(l.Questions)
		if err := fn(&next); err != nil {
			return err
		}
		if after, _ := json.Marshal(next); string(after) == string(before) {
			return nil
		}
		if c != nil && !slices.Equal(l.Closed, next.Closed) {
			c.ClosedBefore, c.ClosedAfter = l.Closed, next.Closed
		}
		if s.DryRun {
			return nil
		}
		next.UpdatedAt, next.UpdatedBy = s.now().Format(time.RFC3339), s.By
		_, err = s.Reg.CAS(ctx, Key, Schema, next, v)
		if pages.Code(err) == "version_conflict" && try < 5 {
			continue
		}
		return err
	}
}

// relabel gives each live record fn's labels (the type rule applied), by a
// CAS touching `labels` alone; on version_conflict it re-reads the record
// and asks fn again.
func (s *Store) relabel(ctx context.Context, fn func(Rec) ([]string, bool)) ([]RecChange, error) {
	l, _, err := Read(ctx, s.Reg)
	if err != nil {
		return nil, err
	}
	recs, err := Records(ctx, s.Reg)
	if err != nil {
		return nil, err
	}
	changes := []RecChange{}
	for _, r := range recs {
		for try := 0; ; try++ {
			want, ok := fn(r)
			if !ok {
				break
			}
			want = Clean(want, l)
			if r.Labelled && slices.Equal(want, r.Labels) {
				break
			}
			before := r.Labels
			if before == nil {
				before = []string{}
			}
			if s.DryRun {
				changes = append(changes, RecChange{ID: r.ID, Before: before, After: want})
				break
			}
			r.raw["labels"] = want
			_, err := s.Reg.CAS(ctx, r.Key, recSchema, r.raw, r.Version)
			if err == nil {
				changes = append(changes, RecChange{ID: r.ID, Before: before, After: want})
				break
			}
			if pages.Code(err) != "version_conflict" || try >= 5 {
				return changes, fmt.Errorf("%s: %w", r.Key, err)
			}
			o, err := s.Reg.Record(ctx, r.Key)
			if err != nil {
				return changes, fmt.Errorf("%s: %w", r.Key, err)
			}
			var live bool
			if r, live, err = decodeRec(o); err != nil || !live {
				break
			}
		}
	}
	return changes, nil
}

var logSeq atomic.Int64

// log writes the change as a new labellog record, when anything changed.
func (s *Store) log(ctx context.Context, c *Change) error {
	if len(c.Records) == 0 && c.ClosedAfter == nil && c.Op[0] != "ask" && c.Op[0] != "question" {
		return nil
	}
	now := s.now()
	c.At, c.By = now.Format(time.RFC3339), s.By
	if s.DryRun {
		return nil
	}
	// The stamp to the microsecond, then this process's count, sorts a
	// process's entries in order; the random tail keeps two hosts apart.
	var b [3]byte
	rand.Read(b[:])
	c.Key = fmt.Sprintf("%s%s.%04d.%s", LogPrefix, now.Format("20060102T150405.000000Z"), logSeq.Add(1)%10000, hex.EncodeToString(b[:]))
	_, err := s.Reg.CAS(ctx, c.Key, LogSchema, c, 0)
	if err != nil {
		return fmt.Errorf("the change landed but its log entry did not: %w", err)
	}
	return nil
}

func (s *Store) now() time.Time {
	if s.Now != nil {
		return s.Now().UTC()
	}
	return time.Now().UTC()
}

// Ask records a question with the label command it asks consent for (none
// for a free question), and returns it; the Telegram ask's id joins it
// later (SetAsk).
func (s *Store) Ask(ctx context.Context, text string, proposal []string) (Question, error) {
	if strings.TrimSpace(text) == "" {
		return Question{}, errors.New("ask: the question's text is empty")
	}
	if len(proposal) > 0 {
		if _, err := parse(proposal); err != nil {
			return Question{}, fmt.Errorf("ask: the proposal: %w", err)
		}
	}
	var q Question
	c := Change{Op: append([]string{"ask"}, proposal...), Reason: text}
	err := s.updateList(ctx, nil, func(l *List) error {
		for _, x := range l.Questions {
			if x.Status == "open" && len(proposal) > 0 && slices.Equal(x.Proposal, proposal) {
				return fmt.Errorf("ask: %s already asks this, still open", x.ID)
			}
		}
		q = Question{ID: fmt.Sprintf("q%d", len(l.Questions)+1), Asked: s.now().Format(time.RFC3339), Text: text, Proposal: proposal, Status: "open"}
		l.Questions = append(l.Questions, q)
		return nil
	})
	if err != nil {
		return q, err
	}
	c.Question = q.ID
	return q, s.log(ctx, &c)
}

// SetAsk joins the Telegram ask's id to question id.
func (s *Store) SetAsk(ctx context.Context, id, ask string) error {
	return s.updateList(ctx, nil, func(l *List) error {
		q, err := l.Question(id)
		if err != nil {
			return err
		}
		q.Ask = ask
		return nil
	})
}

// Answer records the answer to an open question: status approved, declined
// or expired, with the answer's words.
func (s *Store) Answer(ctx context.Context, id, status, answer string) error {
	if !slices.Contains([]string{"approved", "declined", "expired"}, status) {
		return fmt.Errorf("answer: %q is not approved, declined or expired", status)
	}
	err := s.updateList(ctx, nil, func(l *List) error {
		p, err := l.Question(id)
		if err != nil {
			return err
		}
		if p.Status != "open" {
			return fmt.Errorf("answer: %s is %s, not open", id, p.Status)
		}
		p.Status, p.Answer, p.Answered = status, answer, s.now().Format(time.RFC3339)
		return nil
	})
	if err != nil {
		return err
	}
	c := Change{Op: []string{"question", id, status}, Reason: answer, Question: id}
	return s.log(ctx, &c)
}

// Apply runs an approved question's proposal on its consent and marks it
// applied. A proposal that fails leaves the question approved with the
// failure in its error, for the next apply to retry.
func (s *Store) Apply(ctx context.Context, id, reason string) (Change, error) {
	l, _, err := Read(ctx, s.Reg)
	if err != nil {
		return Change{}, err
	}
	q, err := l.Question(id)
	if err != nil {
		return Change{}, err
	}
	if q.Status != "approved" {
		return Change{}, fmt.Errorf("apply: %s is %s, not approved", id, q.Status)
	}
	if len(q.Proposal) == 0 {
		return Change{}, fmt.Errorf("apply: %s proposes no label command", id)
	}
	if reason == "" {
		reason = "approved: " + q.Answer
	}
	c, err := s.Run(ctx, q.Proposal, reason, id)
	if s.DryRun {
		return c, err
	}
	if err != nil {
		msg := err.Error()
		if len(msg) > 500 {
			msg = msg[:500]
		}
		if rerr := s.updateList(ctx, nil, func(l *List) error {
			p, qerr := l.Question(id)
			if qerr != nil {
				return qerr
			}
			p.Error = msg
			return nil
		}); rerr != nil {
			return c, &UnrecordedError{ID: id, Err: err, Record: rerr}
		}
		return c, err
	}
	return c, s.updateList(ctx, nil, func(l *List) error {
		p, err := l.Question(id)
		if err != nil {
			return err
		}
		p.Status, p.Applied, p.Error = "applied", s.now().Format(time.RFC3339), ""
		return nil
	})
}

// UnrecordedError is a failed apply whose failure could not be written on
// its question either.
type UnrecordedError struct {
	ID          string
	Err, Record error
}

func (e *UnrecordedError) Error() string {
	return fmt.Sprintf("%v (and recording it on %s failed: %v)", e.Err, e.ID, e.Record)
}

func (e *UnrecordedError) Unwrap() error { return e.Err }

// Applied is one question an ApplyApproved pass ran: its change, or the
// error it failed with (recorded on the question too).
type Applied struct {
	ID     string `json:"id"`
	Change Change `json:"change"`
	Error  string `json:"error,omitempty"`
}

// ApplyApproved applies every approved question that proposes a label
// command, oldest first; one that fails is recorded on its question and the
// pass goes on. The error is the pass's own: the list unreadable, or a
// failure it could not record on its question.
func (s *Store) ApplyApproved(ctx context.Context) ([]Applied, error) {
	l, _, err := Read(ctx, s.Reg)
	if err != nil {
		return nil, err
	}
	out := []Applied{}
	for _, q := range l.Questions {
		if q.Status != "approved" || len(q.Proposal) == 0 {
			continue
		}
		c, err := s.Apply(ctx, q.ID, "")
		a := Applied{ID: q.ID, Change: c}
		if err != nil {
			a.Error = err.Error()
			if ue := (*UnrecordedError)(nil); errors.As(err, &ue) {
				return append(out, a), err
			}
		}
		out = append(out, a)
	}
	return out, nil
}

// Log is the change log, oldest first.
func Log(ctx context.Context, reg Registry) ([]Change, error) {
	objs, err := reg.List(ctx, LogPrefix)
	if err != nil {
		return nil, err
	}
	sort.Slice(objs, func(i, j int) bool { return objs[i].Key < objs[j].Key })
	out := make([]Change, 0, len(objs))
	for _, o := range objs {
		var c Change
		if json.Unmarshal(o.Value.Data, &c) == nil {
			c.Key = o.Key
			out = append(out, c)
		}
	}
	return out, nil
}

// Init creates the `labels` record from the seed when it is absent, and
// returns what is stored.
func (s *Store) Init(ctx context.Context) (List, bool, error) {
	l, v, err := Read(ctx, s.Reg)
	if err != nil || v > 0 {
		return l, false, err
	}
	if s.DryRun {
		return l, true, nil
	}
	l.UpdatedAt, l.UpdatedBy = s.now().Format(time.RFC3339), s.By
	if _, err := s.Reg.CAS(ctx, Key, Schema, l, 0); err != nil {
		if pages.Code(err) == "version_conflict" {
			l, _, err = Read(ctx, s.Reg)
			return l, false, err
		}
		return l, false, err
	}
	c := Change{Op: []string{"init"}, ClosedBefore: []Entry{}, ClosedAfter: l.Closed, Records: []RecChange{}}
	return l, true, s.log(ctx, &c)
}
