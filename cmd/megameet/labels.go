package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"slices"
	"strings"
	"syscall"

	"github.com/caoer/mega-asr/internal/app"
	"github.com/caoer/mega-asr/internal/labels"
	"github.com/caoer/mega-asr/internal/pages"
)

const labelsUsage = `megameet labels <command> — the closed and open label lists (docs/megameet.md § Labels)

  list [--json]                  the closed list with counts, the open list, the
                                 questions, and records breaking the type rule
  records [--label L] [--json]   live records: id, labels, display title, title, gist
  log [--limit N] [--json]       the change log, newest last
  init                           create the labels record from the seed types
  ask --text T [-- COMMAND...]   record a question that waits for an answer; COMMAND is
                                 the change it asks approval for; prints its id
  question ID --ask ASK_ID       join the Telegram ask's id to the question
  question ID --approve|--decline|--expire --answer TEXT
                                 record the answer
  apply ID                       run an approved question's COMMAND
  apply --approved               run every approved question's COMMAND (the
                                 meetings page's hook); a failure is recorded
                                 on its question and the pass goes on

Label commands (flags --reason R, --dry-run, before the arguments):
` + labels.Usage + `

rename, merge and split naming a closed label, promote and demote need an
approval: they run only through ask → question --approve → apply.
Global: --by NAME (the log's author; default megameet labels (<user>@<host>)).`

func labelsCmd(o app.LoadOpts, args []string) error {
	if len(args) == 0 || args[0] == "-h" || args[0] == "--help" || args[0] == "help" {
		fmt.Println(labelsUsage)
		return nil
	}
	sub, args := args[0], args[1:]
	fs := flag.NewFlagSet("labels "+sub, flag.ContinueOnError)
	by := fs.String("by", fmt.Sprintf("megameet labels (%s@%s)", os.Getenv("USER"), hostName()), "the change log's author")
	asJSON := fs.Bool("json", false, "print JSON")
	reason := fs.String("reason", "", "why: kept in the change log")
	dry := fs.Bool("dry-run", false, "print the change; write nothing")
	label := fs.String("label", "", "records: only those carrying this label")
	limit := fs.Int("limit", 0, "log: the last N entries")
	text := fs.String("text", "", "ask: the question, as it is shown")
	askID := fs.String("ask", "", "question: the Telegram ask's id")
	approve := fs.Bool("approve", false, "question: approved")
	decline := fs.Bool("decline", false, "question: declined")
	expire := fs.Bool("expire", false, "question: the ask timed out unanswered")
	answer := fs.String("answer", "", "question: the answer's text")
	approved := fs.Bool("approved", false, "apply: every approved question")
	// A label command's arguments may hold --into, so its flags come first;
	// the others take flags anywhere.
	var rest []string
	for {
		if err := fs.Parse(args); err != nil {
			return err
		}
		if args = fs.Args(); len(args) == 0 || slices.Contains([]string{"ask", "rename", "merge", "split", "set", "promote", "demote"}, sub) {
			rest = append(rest, args...)
			break
		}
		rest, args = append(rest, args[0]), args[1:]
	}
	l, err := app.Load(o)
	if err != nil {
		return err
	}
	c, err := pages.FromConfig(l.Meeting)
	if err != nil {
		return err
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	s := &labels.Store{Reg: c, By: *by, DryRun: *dry}
	out := func(v any, text func()) error {
		if *asJSON {
			b, _ := json.MarshalIndent(v, "", "  ")
			fmt.Println(string(b))
		} else {
			text()
		}
		return nil
	}
	switch sub {
	case "list":
		return labelsList(ctx, c, *asJSON)
	case "records":
		recs, err := labels.Records(ctx, c)
		if err != nil {
			return err
		}
		type row struct {
			ID           string   `json:"id"`
			Labels       []string `json:"labels"`
			DisplayTitle string   `json:"display_title"`
			Title        string   `json:"title"`
			Gist         string   `json:"gist"`
		}
		var rows []row
		for _, r := range recs {
			if *label == "" || slices.Contains(r.Labels, *label) {
				rows = append(rows, row{r.ID, r.Labels, r.DisplayTitle, r.Title, r.Gist})
			}
		}
		return out(rows, func() {
			for _, r := range rows {
				fmt.Printf("%s\t[%s]\t%s | %s | %s\n", r.ID, strings.Join(r.Labels, ", "), r.DisplayTitle, r.Title, r.Gist)
			}
		})
	case "log":
		log, err := labels.Log(ctx, c)
		if err != nil {
			return err
		}
		if *limit > 0 && len(log) > *limit {
			log = log[len(log)-*limit:]
		}
		return out(log, func() {
			for _, ch := range log {
				printChange(ch)
			}
		})
	case "init":
		lst, created, err := s.Init(ctx)
		if err != nil {
			return err
		}
		fmt.Printf("labels: %s; closed: %s\n", map[bool]string{true: "created", false: "exists"}[created], closedString(lst.Closed))
		return nil
	case "ask":
		q, err := s.Ask(ctx, *text, rest)
		if err != nil {
			return err
		}
		fmt.Println(q.ID)
		return nil
	case "question":
		if len(rest) != 1 {
			return errors.New("labels question ID --ask ASK_ID | --approve|--decline|--expire --answer TEXT")
		}
		if *askID != "" {
			if err := s.SetAsk(ctx, rest[0], *askID); err != nil {
				return err
			}
		}
		var status []string
		for st, on := range map[string]bool{"approved": *approve, "declined": *decline, "expired": *expire} {
			if on {
				status = append(status, st)
			}
		}
		switch {
		case len(status) > 1:
			return errors.New("labels question: one of --approve, --decline, --expire")
		case len(status) == 1:
			if err := s.Answer(ctx, rest[0], status[0], *answer); err != nil {
				return err
			}
		case *askID == "":
			return errors.New("labels question: --ask, or an answer")
		}
		fmt.Printf("%s: recorded\n", rest[0])
		return nil
	case "apply":
		if *approved && len(rest) == 0 {
			res, err := s.ApplyApproved(ctx)
			if perr := out(res, func() {
				for _, a := range res {
					if a.Error != "" {
						fmt.Printf("%s: failed: %s\n", a.ID, a.Error)
						continue
					}
					fmt.Printf("%s: ", a.ID)
					printChange(a.Change)
				}
				if len(res) == 0 {
					fmt.Println("no approved question to apply")
				}
			}); perr != nil {
				return perr
			}
			return err
		}
		if *approved || len(rest) != 1 {
			return errors.New("labels apply ID | labels apply --approved")
		}
		ch, err := s.Apply(ctx, rest[0], *reason)
		if err != nil {
			return err
		}
		return out(ch, func() { printChange(ch) })
	case "rename", "merge", "split", "set", "promote", "demote":
		ch, err := s.Run(ctx, append([]string{sub}, rest...), *reason, "")
		if err != nil {
			return err
		}
		return out(ch, func() {
			if *dry {
				fmt.Print("dry run: ")
			}
			printChange(ch)
		})
	}
	return fmt.Errorf("labels: %q is not a command (megameet labels help)", sub)
}

func labelsList(ctx context.Context, c *pages.Client, asJSON bool) error {
	lst, v, err := labels.Read(ctx, c)
	if err != nil {
		return err
	}
	recs, err := labels.Records(ctx, c)
	if err != nil {
		return err
	}
	vocab := labels.Vocab{List: lst, Counts: labels.Counts(recs)}
	type count struct {
		Name  string `json:"name"`
		Kind  string `json:"kind,omitempty"`
		Count int    `json:"count"`
	}
	var res struct {
		Stored     bool               `json:"stored"` // false: the seed stands in
		Closed     []count            `json:"closed"`
		Open       []count            `json:"open"`
		Questions  []labels.Question  `json:"questions"`
		Violations []labels.Violation `json:"type_rule_breaks"`
		Unlabelled []string           `json:"unlabelled"`
	}
	res.Stored = v > 0
	for _, e := range lst.Closed {
		res.Closed = append(res.Closed, count{e.Name, e.Kind, vocab.Counts[e.Name]})
	}
	for _, n := range vocab.Open() {
		res.Open = append(res.Open, count{Name: n, Count: vocab.Counts[n]})
	}
	res.Questions = lst.Questions
	res.Violations = labels.Violations(recs, lst)
	for _, r := range recs {
		if !r.Labelled {
			res.Unlabelled = append(res.Unlabelled, r.ID)
		}
	}
	if asJSON {
		b, _ := json.MarshalIndent(res, "", "  ")
		fmt.Println(string(b))
		return nil
	}
	fmt.Printf("closed (%d%s):\n", len(res.Closed), map[bool]string{false: ", the seed: no labels record yet"}[res.Stored])
	for _, e := range res.Closed {
		fmt.Printf("  %-8s %s  %d\n", e.Kind, e.Name, e.Count)
	}
	fmt.Printf("open (%d):\n", len(res.Open))
	for _, e := range res.Open {
		fmt.Printf("  %s  %d\n", e.Name, e.Count)
	}
	fmt.Printf("questions (%d):\n", len(res.Questions))
	for _, q := range res.Questions {
		fmt.Printf("  %s %s asked %s%s: %s", q.ID, q.Status, q.Asked, map[bool]string{true: " ask " + q.Ask}[q.Ask != ""], q.Text)
		if len(q.Proposal) > 0 {
			fmt.Printf(" [%s]", strings.Join(q.Proposal, " "))
		}
		if q.Answer != "" {
			fmt.Printf(" — answer: %s", q.Answer)
		}
		fmt.Println()
	}
	fmt.Printf("type rule breaks (%d):\n", len(res.Violations))
	for _, x := range res.Violations {
		fmt.Printf("  %s [%s]: types %s\n", x.ID, strings.Join(x.Labels, ", "), strings.Join(x.Types, ", "))
	}
	if len(res.Unlabelled) > 0 {
		fmt.Printf("not labelled yet (%d): %s\n", len(res.Unlabelled), strings.Join(res.Unlabelled, " "))
	}
	return nil
}

func printChange(c labels.Change) {
	fmt.Printf("%s %s %s: %d records", c.At, c.By, strings.Join(c.Op, " "), len(c.Records))
	if c.Question != "" {
		fmt.Printf(" (%s)", c.Question)
	}
	if c.Reason != "" {
		fmt.Printf(" — %s", c.Reason)
	}
	fmt.Println()
	if c.ClosedAfter != nil {
		fmt.Printf("  closed: %s → %s\n", closedString(c.ClosedBefore), closedString(c.ClosedAfter))
	}
	for _, r := range c.Records {
		fmt.Printf("  rec.%s: [%s] → [%s]\n", r.ID, strings.Join(r.Before, ", "), strings.Join(r.After, ", "))
	}
	if c.Key != "" {
		fmt.Printf("  %s\n", c.Key)
	}
}

func closedString(es []labels.Entry) string {
	var s []string
	for _, e := range es {
		s = append(s, e.Name+"/"+e.Kind)
	}
	return strings.Join(s, ", ")
}
