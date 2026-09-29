package drain

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"

	"github.com/caoer/mega-asr/internal/pages"
)

// Titler writes display_title and labels onto the records that lack
// either — the backfill of the ones the drain's Summarize stage never saw.
// It writes only the missing keys, by CAS, so a rerun changes nothing and
// labels set on the page and every other field stay as they are.
type Titler struct {
	Reg Registry
	Sum *Summarizer
	// Fetch puts the record's transcript files in a directory laid out as
	// a pulled one (feishu-transcript.json, segments.json) and returns it.
	Fetch func(ctx context.Context, rec map[string]any) (dir string, err error)
	// Redo rewrites display_title of the records it runs over, which keep
	// their labels: a title fix on named records only.
	Redo bool
}

// TitleResult is what the backfill did with one record.
type TitleResult struct {
	ID, Title    string
	DisplayTitle string
	Labels       []string
	Content      string
	USD          float64
	Outcome      string // updated | skipped | failed
	Why          string // a skip's reason or a failure
}

// Run titles the non-deleted records missing display_title or labels, in
// key order: only ids when given, at most limit of them when limit > 0.
// each sees every record it considered.
func (t *Titler) Run(ctx context.Context, ids []string, limit int, each func(TitleResult)) error {
	objs, err := t.Reg.List(ctx, "rec.")
	if err != nil {
		return err
	}
	tried := 0
	for _, o := range objs {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		raw, v, err := decode(o)
		if err != nil || v.State == "deleted" || len(ids) > 0 && !slices.Contains(ids, v.ID) {
			continue
		}
		r := TitleResult{ID: v.ID, Title: v.Title, Outcome: "skipped", Why: "has both"}
		if HasDisplayTitle(raw) && HasLabels(raw) && !t.Redo {
			each(r)
			continue
		}
		if limit > 0 && tried >= limit {
			break
		}
		tried++
		t.one(ctx, o.Key, raw, &r)
		each(r)
	}
	return nil
}

func (t *Titler) one(ctx context.Context, key string, raw map[string]any, r *TitleResult) {
	dir, err := t.Fetch(ctx, raw)
	var sum Summary
	if err == nil {
		sum, err = t.Sum.Summarize(ctx, dir, raw)
	}
	r.USD, r.Content = sum.USD, sum.Content
	if errors.Is(err, ErrNoContent) {
		r.Why = err.Error()
		return
	}
	if err != nil {
		r.Outcome, r.Why = "failed", oneLine(err.Error())
		return
	}
	wrote, err := setMissing(ctx, t.Reg, key, sum.DisplayTitle, sum.Labels, t.Redo)
	switch {
	case err != nil:
		r.Outcome, r.Why = "failed", "write: "+err.Error()
	case !wrote:
		r.Why = "written meanwhile, or deleted"
	default:
		r.Outcome, r.Why = "updated", ""
	}
	if sum.TitleProblem != "" {
		r.Why = "the title breaks the title rule after a retry: " + sum.TitleProblem
	}
	r.DisplayTitle, r.Labels = sum.DisplayTitle, sum.Labels
}

// setMissing writes display_title and labels onto the record where it
// lacks them (display_title also when redo), touching nothing else; a
// version_conflict re-reads and tries again.
func setMissing(ctx context.Context, reg Registry, key, title string, labels []string, redo bool) (bool, error) {
	for try := 0; ; try++ {
		o, err := reg.Record(ctx, key)
		if err != nil {
			return false, err
		}
		raw, v, err := decode(o)
		if err != nil {
			return false, err
		}
		if v.State == "deleted" {
			return false, nil
		}
		changed := false
		if !HasDisplayTitle(raw) || redo {
			raw["display_title"], changed = title, true
		}
		if !HasLabels(raw) {
			raw["labels"], changed = labels, true
		}
		if !changed {
			return false, nil
		}
		_, err = reg.CAS(ctx, key, recSchema, raw, o.Version)
		if pages.Code(err) == "version_conflict" && try < 5 {
			continue
		}
		return err == nil, err
	}
}

// String is the result as one table row.
func (r TitleResult) String() string {
	s := fmt.Sprintf("%s\t%s\t%s", r.ID, r.Outcome, r.Title)
	if r.Outcome == "updated" {
		s += fmt.Sprintf(" → %s | %s | %s | $%.4f", r.DisplayTitle, strings.Join(r.Labels, ", "), r.Content, r.USD)
	}
	if r.Why != "" {
		s += " (" + r.Why + ")"
	}
	return s
}
