package meeting

import (
	"context"
	"fmt"
	"io"
	"sort"
	"text/tabwriter"
)

// Rescorable are the records a root comparison can score, newest first: a
// local recording paired with a Feishu minute (scores.against and an align
// file), and a Feishu minute megameet has transcribed (a segments file),
// which scores against its own transcript.
func Rescorable(all []Record) []Record {
	var out []Record
	for _, r := range all {
		_, align := r.File("align")
		_, segs := r.File("segments")
		_, text := r.File("feishu-transcript")
		_, media := r.File("media")
		switch {
		case IsLocal(r) && r.Scores != nil && r.Scores.Against != "" && align:
		case r.Source == "feishu" && segs && text && media:
		default:
			continue
		}
		out = append(out, r)
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].Started.After(out[j].Started) })
	return out
}

// Basis is what a root comparison scores a record's new transcripts
// against: Feishu's turns, and the alignment the record already has.
type Basis struct {
	Prev  Result
	Turns []Turn
}

// BasisOf reads a rescorable record's basis: a local recording's align file
// and its minute's transcript, or a minute's own transcript at offset 0.
func BasisOf(ctx context.Context, fetch Fetch, rec Record, all []Record) (Basis, error) {
	if rec.Source == "feishu" {
		got, err := fetch(ctx, rec, "feishu-transcript")
		if err != nil {
			return Basis{}, err
		}
		turns, err := ReadTurns(got["feishu-transcript"])
		if err != nil {
			return Basis{}, err
		}
		length := rec.DurationS
		for _, t := range turns {
			if t.StartS >= length {
				length = t.StartS + 1
			}
		}
		return Basis{Prev: Itself(rec.ID, length), Turns: turns}, nil
	}
	if rec.Scores == nil {
		return Basis{}, fmt.Errorf("%s: no scores", rec.ID)
	}
	var minute Record
	for _, r := range all {
		if r.ID == rec.Scores.Against {
			minute = r
		}
	}
	if minute.ID == "" {
		return Basis{}, fmt.Errorf("%s: its minute %s is not on the page", rec.ID, rec.Scores.Against)
	}
	got, err := fetch(ctx, rec, "align")
	if err != nil {
		return Basis{}, err
	}
	var prev Result
	if err := readJSON(got["align"], &prev); err != nil {
		return Basis{}, err
	}
	if !prev.Paired {
		return Basis{}, fmt.Errorf("%s: its align file is unpaired", rec.ID)
	}
	got, err = fetch(ctx, minute, "feishu-transcript")
	if err != nil {
		return Basis{}, err
	}
	turns, err := ReadTurns(got["feishu-transcript"])
	if err != nil {
		return Basis{}, err
	}
	return Basis{Prev: prev, Turns: turns}, nil
}

// RootRun is one engine root's transcript of one record, scored.
type RootRun struct {
	Root string // the name the comparison gives it
	Res  Result
	Out  Outcome
}

// PrintRoots prints, per record, one row per root (CER over all tokens,
// Han characters and Latin words, in 5-min windows) and each root's delta
// against the first in percentage points; over more than one record, the
// same rows pooled over every window.
func PrintRoots(w io.Writer, recs [][]RootRun) error {
	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	type rates struct{ all, zh, en float64 }
	block := func(title string, names []string, rs []rates, extra func(i int) string) {
		fmt.Fprintln(tw, title)
		fmt.Fprintln(tw, "root\tCER\tzh\ten\t"+extra(-1))
		for i, r := range rs {
			fmt.Fprintf(tw, "%s\t%.2f%%\t%.2f%%\t%.2f%%\t%s\n", names[i], 100*r.all, 100*r.zh, 100*r.en, extra(i))
		}
		for i := 1; i < len(rs); i++ {
			fmt.Fprintf(tw, "%s − %s\t%+.2f pp\t%+.2f pp\t%+.2f pp\t\n", names[i], names[0],
				100*(rs[i].all-rs[0].all), 100*(rs[i].zh-rs[0].zh), 100*(rs[i].en-rs[0].en))
		}
		fmt.Fprintln(tw, "\t\t\t\t")
	}
	type sum struct{ err, ref, hanErr, hanRef, latErr, latRef int }
	var names []string
	var pooled []sum
	for _, runs := range recs {
		if len(runs) == 0 {
			continue
		}
		r0 := runs[0].Res
		against := "against " + r0.Against
		if r0.Against == r0.Rec {
			against = "against its own Feishu transcript"
		}
		var ns []string
		var rs []rates
		for i, run := range runs {
			ns = append(ns, run.Root)
			rs = append(rs, rates{run.Res.CER, run.Res.CERZh, run.Res.WEREn})
			if len(pooled) <= i {
				pooled = append(pooled, sum{})
				names = append(names, run.Root)
			}
			for _, win := range run.Res.Windows {
				p := &pooled[i]
				p.err += win.Errors
				p.ref += win.RefTokens
				p.hanErr += win.HanErr
				p.hanRef += win.HanRef
				p.latErr += win.LatErr
				p.latRef += win.LatRef
			}
		}
		block(fmt.Sprintf("%s %s (%d reference tokens)", r0.Rec, against, r0.RefTokens), ns, rs, func(i int) string {
			if i < 0 {
				return "loops\tRTF"
			}
			return fmt.Sprintf("%d\t%.3f", runs[i].Out.Loops, runs[i].Out.RTF())
		})
	}
	if len(recs) > 1 {
		var rs []rates
		for _, p := range pooled {
			rs = append(rs, rates{ratio(p.err, p.ref), ratio(p.hanErr, p.hanRef), ratio(p.latErr, p.latRef)})
		}
		block(fmt.Sprintf("all %d records (%d reference tokens)", len(recs), pooled[0].ref), names, rs, func(int) string { return "\t" })
	}
	return tw.Flush()
}
