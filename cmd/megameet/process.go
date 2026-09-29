package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log"
	"path/filepath"

	"github.com/caoer/mega-asr/internal/app"
	"github.com/caoer/mega-asr/internal/meeting"
	"github.com/caoer/mega-asr/internal/pages"
)

// pullCmd materializes a record in <meeting.data>/pull/<id>.
func pullCmd(o app.LoadOpts, args []string) error {
	fs := flag.NewFlagSet("pull", flag.ContinueOnError)
	if err := fs.Parse(args); err != nil || fs.NArg() != 1 {
		return errors.New("pull <id>")
	}
	id := fs.Arg(0)
	l, err := app.Load(o)
	if err != nil {
		return err
	}
	cl, err := pages.FromConfig(l.Meeting)
	if err != nil {
		return err
	}
	dir := meeting.PullDir(l.Meeting.Data, id)
	rec, err := meeting.Pull(context.Background(), cl, id, dir)
	if err != nil {
		return err
	}
	fmt.Printf("%s: %s, %d files verified\n", id, dir, len(rec.Files))
	return nil
}

// processCmd pulls a record, transcribes it, and puts segments.json and
// transcript.md on the page.
func processCmd(o app.LoadOpts, args []string) error {
	fs := flag.NewFlagSet("process", flag.ContinueOnError)
	if err := fs.Parse(args); err != nil || fs.NArg() != 1 {
		return errors.New("process <id>")
	}
	id := fs.Arg(0)
	l, err := app.Load(o)
	if err != nil {
		return err
	}
	cl, err := pages.FromConfig(l.Meeting)
	if err != nil {
		return err
	}
	ctx := context.Background()
	dir := meeting.PullDir(l.Meeting.Data, id)
	if _, err := meeting.Pull(ctx, cl, id, dir); err != nil {
		return err
	}
	root := l.ASR.FunASR.Root
	if r, err := filepath.EvalSymlinks(root); err == nil {
		root = r // a nix out-link names the store path it points at
	}
	p := &meeting.Processor{
		ASR: l.Transcriber(), Post: l.Post(), Chunker: l.Chunker,
		Engine: l.ASR.Engine, Root: root, Logf: log.Printf,
	}
	out, err := p.Process(ctx, dir)
	if err != nil {
		return err
	}
	if err := meeting.Publish(ctx, cl, id, dir, out, meeting.Engine{Root: root, LLM: l.ASR.FunASR.LLM}); err != nil {
		return err
	}
	fmt.Printf("%s: %d segments, %.0f s of audio in %.0f s (RTF %.3f), %d loops, %d re-decoded, %d echoes dropped, %d of %d pieces failed\n%s\n",
		id, len(out.Segments), out.AudioS, out.WallS, out.RTF(), out.Loops, out.Redecoded, out.Echoes, out.Failed, out.Chunks, dir)
	return nil
}
