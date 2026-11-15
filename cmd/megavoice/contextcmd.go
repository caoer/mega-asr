//go:build darwin

package main

import (
	"context"
	"encoding/json"
	"flag"
	"io"
	"log"
	"os"
	"slices"
	"strings"
	"time"

	"github.com/caoer/mega-asr/internal/app"
	"github.com/caoer/mega-asr/internal/deliver"
	"github.com/caoer/mega-asr/internal/mac"
	"github.com/caoer/mega-asr/internal/refine"
	"github.com/caoer/mega-asr/internal/session"
	"github.com/caoer/mega-asr/internal/store"
)

// contextLine is `megavoice context`'s line: the text sources as a refine
// line records them, the target they read, and the <context> they give.
type contextLine struct {
	Target  *store.Target         `json:"target,omitempty"`
	Sources []store.ContextSource `json:"sources"`
	Context string                `json:"context,omitempty"`
}

// contextCmd runs the model step's text sources once, as a take does: the
// target in front is locked after D, the take lasts F, and the sources are
// read from that target at its end. The clipboard is watched from the
// start, so a copy during D counts as one just before the take. It runs
// under the main event loop, as serve does: the clipboard's watcher reads
// the pasteboard on the main thread.
func contextCmd(o app.LoadOpts, args []string, out io.Writer) error {
	fs := flag.NewFlagSet("context", flag.ContinueOnError)
	after := fs.Duration("after", 0, "lock the target after this long, to bring a window in front")
	dur := fs.Duration("for", 0, "how long the take lasts before its sources are read")
	which := fs.String("sources", "app,field,pane,clipboard", "the sources to read, by name")
	pane := fs.String("pane", "", "lock this herdr pane as the target, as serve --pane does")
	if err := fs.Parse(args); err != nil || fs.NArg() != 0 {
		return exitCode(2)
	}
	c, err := load(o)
	if err != nil {
		return err
	}
	mac.Run(func() {
		if err := readContext(c, *after, *dur, *which, *pane, out); err != nil {
			log.Print(err)
			os.Exit(1)
		}
		os.Exit(0)
	})
	return nil
}

func readContext(c app.Loaded, after, dur time.Duration, which, pane string, out io.Writer) error {
	names := strings.Split(which, ",")
	src := c.Refine.Context.TextSources(nil)
	src.App, src.Field, src.Pane, src.Clipboard = slices.Contains(names, refine.SourceApp), slices.Contains(names, refine.SourceField),
		slices.Contains(names, refine.SourcePane), slices.Contains(names, refine.SourceClipboard)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if src.Clipboard {
		src.Clip = &refine.ClipWatch{Board: mac.Clipboard{}}
		go src.Clip.Run(ctx)
	}
	time.Sleep(after)
	dlv := deliver.Deliverer{Pane: pane, Agents: func() bool { return true }, Herdr: deliver.Herdr{Bin: "herdr"}, HerdrApps: c.Deliver.HerdrApps}
	target := dlv.Capture()
	defer target.(*deliver.Target).Release()
	ts := src.Start()
	time.Sleep(dur)
	rec := session.StoreTarget(target)
	parts := ts.Read(ctx, rec, target.(refine.Spot))
	enc := json.NewEncoder(out)
	enc.SetEscapeHTML(false)
	return enc.Encode(contextLine{Target: rec, Sources: app.SourcesRecord(parts), Context: refine.Compose(parts, nil)})
}
