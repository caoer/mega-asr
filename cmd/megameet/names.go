package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"log"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/caoer/mega-asr/internal/app"
	"github.com/caoer/mega-asr/internal/drain"
	"github.com/caoer/mega-asr/internal/meeting"
	"github.com/caoer/mega-asr/internal/pages"
)

// namer is the drain's names pass over the page c: the people index read
// from its people.index record, the results published onto the record.
func namer(nc app.MeetingNamesConfig, c *pages.Client) *drain.Namer {
	return &drain.Namer{
		Command: nc.Command, Effort: nc.Effort, Concurrency: nc.Concurrency, WindowChars: nc.WindowChars,
		Timeout: 5 * time.Minute, Logf: log.Printf,
		Index: func(ctx context.Context) ([]byte, error) {
			o, err := c.Record(ctx, "people.index")
			if err != nil {
				return nil, err
			}
			return o.Value.Data, nil
		},
		Publish: func(ctx context.Context, id, dir string, ours bool) error {
			return meeting.PublishNames(ctx, c, id, dir, ours)
		},
	}
}

// namesCmd runs the names pass over a pulled directory and publishes
// nothing: DIR's segments.json and names.json are rewritten, so run it on a
// copy.
func namesCmd(o app.LoadOpts, args []string) error {
	fs := flag.NewFlagSet("names", flag.ContinueOnError)
	dir := fs.String("dir", "", "the pulled directory (a copy: its segments.json and names.json are rewritten)")
	index := fs.String("index", "", "the people index as a file; default: the page's people.index record")
	focus := fs.String("focus", "", "only the windows that may name these people (person slugs, comma-separated)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *dir == "" || fs.NArg() > 0 {
		return errors.New("names: usage: names --dir DIR [--index FILE] [--focus SLUG,...]")
	}
	l, err := app.Load(o)
	if err != nil {
		return err
	}
	nc := l.Meeting.Names
	if len(nc.Command) == 0 {
		return errors.New("names: meeting.names.command is empty: no model to ask")
	}
	var rec map[string]any
	b, err := os.ReadFile(filepath.Join(*dir, "rec.json"))
	if err != nil {
		return err
	}
	if err := json.Unmarshal(b, &rec); err != nil {
		return fmt.Errorf("rec.json: %w", err)
	}
	var n *drain.Namer
	if *index != "" {
		n = &drain.Namer{Command: nc.Command, Effort: nc.Effort, Concurrency: nc.Concurrency, WindowChars: nc.WindowChars, Timeout: 5 * time.Minute,
			Index: func(context.Context) ([]byte, error) { return os.ReadFile(*index) }}
	} else {
		c, err := pages.FromConfig(l.Meeting)
		if err != nil {
			return err
		}
		n = namer(nc, c)
	}
	var fo []string
	for _, s := range strings.Split(*focus, ",") {
		if s = strings.TrimSpace(s); s != "" {
			fo = append(fo, s)
		}
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	rep, ours, err := n.Run(ctx, *dir, rec, fo)
	if err != nil {
		return err
	}
	for _, c := range rep.Changes {
		fmt.Printf("%s #%d: %s\n  → %s\n", c.Transcript, c.Index, c.Before, c.After)
	}
	fmt.Printf("names: %d windows, %d calls, %d failed, %d texts changed (ours rewritten: %v), %d edits refused by the guards, %.0f s",
		rep.Windows, rep.Calls, rep.Failed, len(rep.Changes), ours, len(rep.Dropped), rep.WallS)
	if rep.Refused != "" {
		fmt.Printf("; refused: %s", rep.Refused)
	}
	fmt.Println()
	return nil
}
