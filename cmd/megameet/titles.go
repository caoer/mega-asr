package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/caoer/mega-asr/internal/app"
	"github.com/caoer/mega-asr/internal/drain"
	"github.com/caoer/mega-asr/internal/labels"
	"github.com/caoer/mega-asr/internal/meeting"
	"github.com/caoer/mega-asr/internal/pages"
)

// titlesCmd writes display_title and labels onto every record that lacks
// either, with [meeting.summary]'s model, reading each record's transcript
// files fetched into <meeting.data>/titles/<id>.
func titlesCmd(o app.LoadOpts, args []string) error {
	fs := flag.NewFlagSet("titles", flag.ContinueOnError)
	limit := fs.Int("limit", 0, "title at most this many records; 0 is every one")
	ids := fs.String("ids", "", "only these records, comma-separated")
	redo := fs.Bool("redo", false, "with --ids: rewrite their display_title; labels stay")
	check := fs.Bool("check", false, "list the live records whose display_title breaks the title rule, and write nothing")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() > 0 {
		return fmt.Errorf("titles: unexpected %q", fs.Args())
	}
	l, err := app.Load(o)
	if err != nil {
		return err
	}
	m := l.Meeting
	if len(m.Summary.Command) == 0 {
		return errors.New("titles: meeting.summary.command is empty: no model to ask")
	}
	c, err := pages.FromConfig(m)
	if err != nil {
		return err
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if *check {
		return checkTitles(ctx, c)
	}
	sc := m.Summary
	t := &drain.Titler{
		Reg:   c,
		Sum:   &drain.Summarizer{Command: sc.Command, Model: sc.Model, MaxChars: sc.MaxChars, Timeout: 5 * time.Minute, Vocab: func(ctx context.Context) (labels.Vocab, error) { return labels.ReadVocab(ctx, c) }},
		Fetch: transcriptFetcher(c, filepath.Join(m.Data, "titles")),
	}
	var only []string
	for _, id := range strings.Split(*ids, ",") {
		if id = strings.TrimPrefix(strings.TrimSpace(id), "rec."); id != "" {
			only = append(only, id)
		}
	}
	if t.Redo = *redo; t.Redo && len(only) == 0 {
		return errors.New("titles: --redo rewrites named records only: give --ids")
	}
	n := map[string]int{}
	usd := 0.0
	err = t.Run(ctx, only, *limit, func(r drain.TitleResult) {
		n[r.Outcome]++
		usd += r.USD
		if r.Outcome != "skipped" || r.Why != "has both" {
			fmt.Println(r)
		}
	})
	fmt.Printf("titles: %d updated, %d skipped, %d failed; $%.4f", n["updated"], n["skipped"], n["failed"], usd)
	if k := n["updated"] + n["failed"]; k > 0 {
		fmt.Printf(" ($%.4f a meeting)", usd/float64(k))
	}
	fmt.Println()
	if err == nil && n["failed"] > 0 {
		err = fmt.Errorf("titles: %d failed", n["failed"])
	}
	return err
}

// checkTitles prints each live record whose display_title breaks the title
// rule (labels.TitleProblem), then the count and their ids for
// `titles --redo --ids`; any break is an error.
func checkTitles(ctx context.Context, c *pages.Client) error {
	l, _, err := labels.Read(ctx, c)
	if err != nil {
		return err
	}
	recs, err := labels.Records(ctx, c)
	if err != nil {
		return err
	}
	var bad []string
	for _, r := range recs {
		if why := labels.TitleProblem(r.DisplayTitle, l.Types()); why != "" {
			fmt.Printf("%s\t%s\t(%s)\n", r.ID, r.DisplayTitle, why)
			bad = append(bad, r.ID)
		}
	}
	fmt.Printf("titles --check: %d of %d live records break the title rule\n", len(bad), len(recs))
	if len(bad) > 0 {
		fmt.Println("ids: " + strings.Join(bad, ","))
		return fmt.Errorf("titles: %d display titles break the title rule", len(bad))
	}
	return nil
}

// transcriptFetcher fetches a record's transcripts (Feishu's and ours) into
// dir/<id>, laid out as a pulled directory: its own cache, apart from the
// drain's pull directories.
func transcriptFetcher(c *pages.Client, dir string) func(ctx context.Context, raw map[string]any) (string, error) {
	fetch := fetcher(c, dir)
	return func(ctx context.Context, raw map[string]any) (string, error) {
		b, _ := json.Marshal(raw)
		var r meeting.Record
		if err := json.Unmarshal(b, &r); err != nil {
			return "", err
		}
		if r.ID == "" || filepath.Base(r.ID) != r.ID || strings.HasPrefix(r.ID, ".") {
			return "", fmt.Errorf("%q is not a record id", r.ID)
		}
		_, err := fetch(ctx, r, "feishu-transcript", "segments")
		return filepath.Join(dir, r.ID), err
	}
}
