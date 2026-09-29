package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"sort"
	"strings"
	"syscall"
	"text/tabwriter"
	"time"

	"github.com/caoer/mega-asr/internal/app"
	"github.com/caoer/mega-asr/internal/drain"
	"github.com/caoer/mega-asr/internal/labels"
	"github.com/caoer/mega-asr/internal/meeting"
	"github.com/caoer/mega-asr/internal/pages"
)

func drainState(m app.MeetingConfig) string { return filepath.Join(m.Data, "drain", "state.json") }

// drainCmd runs the server's loop over the registry: one tick (the
// systemd timer's), or a tick every --every.
func drainCmd(o app.LoadOpts, args []string) error {
	fs := flag.NewFlagSet("drain", flag.ContinueOnError)
	every := fs.Duration("every", 0, "tick this often until stopped; 0 runs one tick")
	most := fs.Int("max", 0, "claim at most this many records a tick; 0 is no limit")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() > 0 {
		return fmt.Errorf("drain: unexpected %q", fs.Args())
	}
	l, err := app.Load(o)
	if err != nil {
		return err
	}
	m := l.Meeting
	c, err := pages.FromConfig(m)
	if err != nil {
		return err
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	st, closeStages, err := serverStages(ctx, l.Config, c)
	if err != nil {
		return err
	}
	defer closeStages()
	notify := notifier(m.Drain.Notify)
	if m.Ingest.Auto {
		in := &drain.Ingester{Command: m.Ingest.Command, Repos: m.Ingest.Repos, Runs: filepath.Join(m.Data, "ingest"), Notify: notify, Logf: log.Printf,
			Assets: m.Ingest.Assets, AssetsRemote: m.Ingest.AssetsRemote, AssetsAnnexRemote: m.Ingest.AssetsAnnexRemote}
		if m.Ingest.Wiki != "" {
			in.Wiki = filepath.Base(m.Ingest.Wiki)
		}
		in.Filed = filedCheck(m)
		st.Ingest, st.IngestStatus = in.Ingest, in.Status
		if m.Ingest.PeopleResolve {
			in.People = filepath.Join(m.Data, "people")
			st.Resolve = in.Resolve
		}
	}
	if sc := m.Summary; len(sc.Command) > 0 {
		sm := &drain.Summarizer{Command: sc.Command, Model: sc.Model, MaxChars: sc.MaxChars, Timeout: 5 * time.Minute, Logf: log.Printf,
			Vocab: func(ctx context.Context) (labels.Vocab, error) { return labels.ReadVocab(ctx, c) }}
		st.Summarize = sm.Stage
	}
	d := drain.New(c, st, fmt.Sprintf("%s:%d", hostName(), os.Getpid()))
	d.State, d.Notify, d.Logf, d.IngestSources, d.Max = drainState(m), notify, log.Printf, m.Ingest.Sources, *most
	d.FeishuSince, _ = m.Ingest.Since() // valid: app.Load checked it
	d.MinDuration = time.Duration(m.Ingest.MinDuration)
	d.Forget = forget(m.Data)
	for {
		run, err := d.Tick(ctx)
		if err != nil {
			log.Printf("drain: %v", err)
		}
		log.Printf("drain: tick %s: %d queued, %d handled in %.1f s", run.Started.Format(time.TimeOnly), run.Queued, handled(run), run.Ended.Sub(run.Started).Seconds())
		if *every <= 0 {
			return err
		}
		select {
		case <-ctx.Done():
			return nil
		case <-time.After(*every):
		}
	}
}

// filedCheck reads which Feishu minutes the wiki holds (register-feishu's
// test) from meeting.ingest.wiki, fresh from its upstream: another ingest
// may have filed one since the last run.
func filedCheck(m app.MeetingConfig) func(token string) (drain.Wiki, bool, error) {
	return func(token string) (drain.Wiki, bool, error) {
		wiki := m.Ingest.Wiki
		if wiki == "" {
			return drain.Wiki{}, false, errors.New("no wiki checkout to read: set meeting.ingest.wiki")
		}
		if out, err := exec.Command("git", "-C", wiki, "pull", "-q", "--ff-only").CombinedOutput(); err != nil {
			log.Printf("filed check: git pull in %s: %v: %s (reading it as it is)", wiki, err, strings.TrimSpace(string(out)))
		}
		filed, err := filedMinutes(wiki)
		if err != nil {
			return drain.Wiki{}, false, err
		}
		w, ok := filed[token]
		return drain.Wiki{Page: w.Page, Commit: w.Commit}, ok, nil
	}
}

// forget removes a deleted record's pulled files and alignment on this
// host.
func forget(data string) func(id string) {
	return func(id string) {
		if id == "" || filepath.Base(id) != id || strings.HasPrefix(id, ".") {
			return
		}
		for _, dir := range []string{meeting.PullDir(data, id), filepath.Join(data, "align", id)} {
			if _, err := os.Lstat(dir); err != nil {
				continue
			}
			if err := os.RemoveAll(dir); err != nil {
				log.Printf("forget %s: %v", dir, err)
				continue
			}
			log.Printf("forget: rec.%s is deleted: %s removed", id, dir)
		}
	}
}

func handled(r drain.Run) int {
	n := 0
	for _, x := range r.Results {
		if x.Outcome != "skipped" {
			n++
		}
	}
	return n
}

// alignStage runs the alignment for a claimed record and writes what it
// owes onto that record itself (scores, align, the align file), as the
// other stages write their own fields.
func alignStage(c *pages.Client, m app.MeetingConfig) func(ctx context.Context, id string, redo bool) (drain.Patch, error) {
	al := &meeting.Aligner{Reg: c, Fetch: fetcher(c, filepath.Join(m.Data, "pull")), Tools: meeting.Py{Dir: filepath.Join(m.Data, "py")}, Data: m.Data, Logf: log.Printf}
	return func(ctx context.Context, id string, redo bool) (drain.Patch, error) {
		p, err := al.Claimed(ctx, id, redo)
		if p != nil {
			err = errors.Join(err, al.Write(ctx, id, *p))
		}
		return nil, err
	}
}

// notifier sends a line through the configured program; none only logs.
func notifier(argv []string) func(string) {
	if len(argv) == 0 {
		return nil
	}
	return func(line string) {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		out, err := exec.CommandContext(ctx, argv[0], append(argv[1:], line)...).CombinedOutput()
		if err != nil {
			log.Printf("notify %s: %v: %s", argv[0], err, strings.TrimSpace(string(out)))
		}
	}
}

// drainStatus prints the server's side: the last tick, the queue, the
// failed records and the recent real-time factors.
func drainStatus(w io.Writer, ctx context.Context, m app.MeetingConfig, reg drain.Registry) error {
	st := drain.Load(drainState(m))
	if r := st.Last; r != nil {
		fmt.Fprintf(w, "drain: last tick %s by %s, %.1f s, %d queued\n", r.Started.Local().Format("2006-01-02 15:04:05"), r.By, r.Ended.Sub(r.Started).Seconds(), r.Queued)
		for _, x := range r.Results {
			if x.Outcome != "skipped" {
				fmt.Fprintf(w, "  %s: %s %s%s\n", x.ID, x.Job, x.Outcome, colon(x.Error))
			}
		}
		if r.Error != "" {
			fmt.Fprintf(w, "  error: %s\n", r.Error)
		}
	} else {
		fmt.Fprintln(w, "drain: no tick on this host yet")
	}
	recs, err := reg.List(ctx, "rec.")
	if err != nil {
		return err
	}
	qs, err := reg.List(ctx, "q.")
	if err != nil {
		return err
	}
	byID := map[string]meeting.Record{}
	count := map[string]int{}
	deleted := 0
	for _, o := range recs {
		var r meeting.Record
		if err := json.Unmarshal(o.Value.Data, &r); err != nil {
			continue
		}
		if r.State == meeting.Deleted {
			deleted++
			continue
		}
		byID[r.ID] = r
		count[r.State]++
	}
	var states []string
	for s, n := range count {
		states = append(states, fmt.Sprintf("%s %d", s, n))
	}
	sort.Strings(states)
	fmt.Fprintf(w, "records: %d (%s)", len(byID), strings.Join(states, ", "))
	if deleted > 0 {
		fmt.Fprintf(w, "; %d deleted", deleted)
	}
	fmt.Fprintln(w)

	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	fmt.Fprintf(tw, "queue: %d\n", len(qs))
	for _, q := range qs {
		id := strings.TrimPrefix(q.Key, "q.")
		r := byID[id]
		claim := "-"
		if r.Claim != nil {
			claim = r.Claim.By + " until " + r.Claim.Expires.Local().Format("15:04")
		}
		fmt.Fprintf(tw, "  %s\t%s\t%s\t%s\tattempts %d\n", id+testMark(r), r.State+dash(r.Action), orDash(r.Stage), claim, r.Attempts)
	}
	var failed, done []meeting.Record
	for _, r := range byID {
		switch {
		case r.State == meeting.Failed:
			failed = append(failed, r)
		case r.Scores != nil && r.Scores.RTF > 0:
			done = append(done, r)
		}
	}
	sort.Slice(failed, func(i, j int) bool { return failed[i].Updated.After(failed[j].Updated) })
	sort.Slice(done, func(i, j int) bool { return done[i].Updated.After(done[j].Updated) })
	fmt.Fprintf(tw, "failed: %d\n", len(failed))
	for _, r := range failed {
		fmt.Fprintf(tw, "  %s\t%s\t%s\n", r.ID+testMark(r), orDash(r.Stage), r.Error)
	}
	if len(done) > 5 {
		done = done[:5]
	}
	fmt.Fprintf(tw, "recent RTF:\n")
	for _, r := range done {
		fmt.Fprintf(tw, "  %s\t%s\tRTF %.2f\tloops %d\t%.0f s\n", r.ID+testMark(r), r.State, r.Scores.RTF, r.Scores.Loops, r.DurationS)
	}
	return tw.Flush()
}

func colon(s string) string {
	if s == "" {
		return ""
	}
	return ": " + s
}

func dash(s string) string {
	if s == "" {
		return ""
	}
	return " (" + s + " queued)"
}

func orDash(s string) string {
	if s == "" {
		return "-"
	}
	return s
}

// testMark flags a test record where status lists it.
func testMark(r meeting.Record) string {
	if r.Test {
		return " (test)"
	}
	return ""
}
