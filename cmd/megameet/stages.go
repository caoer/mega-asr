package main

import (
	"context"
	"fmt"
	"log"
	"os"
	"path/filepath"

	"github.com/caoer/mega-asr/internal/app"
	"github.com/caoer/mega-asr/internal/drain"
	"github.com/caoer/mega-asr/internal/meeting"
	"github.com/caoer/mega-asr/internal/pages"
)

// serverStages are the drain's stages over the page: pull and process
// (one resident ASR shared by every record of the run) and align. Each writes its own fields onto the record.
func serverStages(ctx context.Context, cfg app.Config, c *pages.Client) (drain.Stages, func(), error) {
	m := cfg.Meeting
	root := cfg.ASR.FunASR.Root
	if r, err := filepath.EvalSymlinks(root); err == nil {
		root = r // a nix out-link names the store path it points at
	}
	p := &meeting.Processor{
		ASR: cfg.Transcriber(), Post: cfg.Post(), Chunker: cfg.Chunker,
		Engine: cfg.ASR.Engine, Root: root, Logf: log.Printf,
	}
	return drain.Stages{
		Pull: func(ctx context.Context, id string) (string, error) {
			dir := meeting.PullDir(m.Data, id)
			rec, err := meeting.Pull(ctx, c, id, dir)
			if err != nil {
				return dir, err
			}
			return dir, pullResults(ctx, c, rec, dir)
		},
		Process: func(ctx context.Context, id, dir string) error {
			out, err := p.Process(ctx, dir)
			if err != nil {
				return err
			}
			log.Printf("process %s: %d segments, %.0f s of audio in %.0f s (RTF %.3f), %d loops, %d of %d pieces failed",
				id, len(out.Segments), out.AudioS, out.WallS, out.RTF(), out.Loops, out.Failed, out.Chunks)
			return meeting.Publish(ctx, c, id, dir, out, meeting.Engine{Root: root, LLM: cfg.ASR.FunASR.LLM})
		},
		Align: alignStage(c, m),
	}, func() {}, nil
}

// pullResults puts a processed record's segments.json and transcript.md
// in its pulled directory when they are not there — a reingest on a host
// that did not process it.
func pullResults(ctx context.Context, c *pages.Client, rec meeting.Record, dir string) error {
	for role, name := range map[string]string{"segments": "segments.json", "transcript": "transcript.md"} {
		f, ok := rec.File(role)
		path := filepath.Join(dir, name)
		if _, err := os.Stat(path); !ok || err == nil {
			continue
		}
		if err := download(ctx, c, f, path); err != nil {
			return fmt.Errorf("%s: %w", name, err)
		}
	}
	return nil
}
