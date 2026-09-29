package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"os"
	"os/signal"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/caoer/mega-asr/internal/app"
	"github.com/caoer/mega-asr/internal/meeting"
	"github.com/caoer/mega-asr/internal/pages"
)

// alignCmd aligns a registry record with the Feishu minute of the same
// meeting (either side may be named: the Feishu one scores the local
// recordings it overlaps), or, with --dir, a recording folder with a Feishu
// minute folder and no registry.
func alignCmd(o app.LoadOpts, args []string) error {
	fs := flag.NewFlagSet("align", flag.ContinueOnError)
	dir := fs.String("dir", "", "a recording folder (meta.toml, tracks, segments.json): align files, no registry")
	feishu := fs.String("feishu", "", "with --dir: a Feishu minute folder (meta.json, transcript.json, media.*)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	l, err := app.Load(o)
	if err != nil {
		return err
	}
	m := l.Meeting
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	tools := meeting.Py{Dir: filepath.Join(m.Data, "py")}
	if *dir != "" {
		if *feishu == "" || fs.NArg() > 0 {
			return errors.New("align --dir DIR --feishu DIR")
		}
		return alignFiles(ctx, m, tools, *dir, *feishu)
	}
	if fs.NArg() != 1 {
		return errors.New("align <id> | align --dir DIR --feishu DIR")
	}
	id := strings.TrimPrefix(fs.Arg(0), "rec.")
	c, err := pages.FromConfig(m)
	if err != nil {
		return err
	}
	al := &meeting.Aligner{Reg: c, Fetch: fetcher(c, filepath.Join(m.Data, "pull")), Tools: tools, Data: m.Data, Logf: log.Printf}
	p, err := al.Claimed(ctx, id, true)
	if p != nil {
		err = errors.Join(err, al.Write(ctx, id, *p))
	}
	return err
}

// fetcher downloads a record's files into dir/<rec>/<role>.<codec>,
// checking each against its sha256; a file already there with the right
// digest is not fetched again.
func fetcher(c *pages.Client, dir string) meeting.Fetch {
	return func(ctx context.Context, r meeting.Record, roles ...string) (map[string]string, error) {
		out := map[string]string{}
		for _, role := range roles {
			f, ok := r.File(role)
			if !ok {
				continue
			}
			path := filepath.Join(dir, r.ID, role)
			if f.Codec != "" {
				path += "." + f.Codec
			}
			if sum, err := fileSHA256(path); err == nil && sum == f.SHA256 {
				out[role] = path
				continue
			}
			if err := download(ctx, c, f, path); err != nil {
				return out, fmt.Errorf("%s %s: %w", r.ID, role, err)
			}
			out[role] = path
		}
		return out, nil
	}
}

func download(ctx context.Context, c *pages.Client, f meeting.File, path string) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	body, err := c.Get(ctx, f.File, 0, 0)
	if err != nil {
		return err
	}
	defer body.Close()
	tmp, err := os.CreateTemp(filepath.Dir(path), ".pull-*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	h := sha256.New()
	_, err = io.Copy(io.MultiWriter(tmp, h), body)
	if cerr := tmp.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		return err
	}
	if sum := hex.EncodeToString(h.Sum(nil)); f.SHA256 != "" && sum != f.SHA256 {
		return fmt.Errorf("sha256 %s, the record says %s", sum, f.SHA256)
	}
	return os.Rename(tmp.Name(), path)
}

func fileSHA256(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// alignFiles aligns a recording folder with a Feishu minute folder as the
// Feishu archive keeps it; the scores go to align.json and the score table,
// not to a registry.
func alignFiles(ctx context.Context, m app.MeetingConfig, tools meeting.Tools, dir, fdir string) error {
	meta, err := readMeta(dir)
	if err != nil {
		return err
	}
	segs, err := meeting.ReadSegments(filepath.Join(dir, "segments.json"))
	if err != nil {
		return err
	}
	var fm struct {
		Token      string `json:"token"`
		CreateTime string `json:"create_time"` // ms since the epoch
	}
	b, err := os.ReadFile(filepath.Join(fdir, "meta.json"))
	if err != nil {
		return err
	}
	if err := json.Unmarshal(b, &fm); err != nil {
		return fmt.Errorf("%s/meta.json: %w", fdir, err)
	}
	ms, err := strconv.ParseInt(fm.CreateTime, 10, 64)
	if err != nil {
		return fmt.Errorf("%s/meta.json: create_time %q", fdir, fm.CreateTime)
	}
	turns, err := meeting.ReadTurns(filepath.Join(fdir, "transcript.json"))
	if err != nil {
		return err
	}
	media, _ := filepath.Glob(filepath.Join(fdir, "media.*"))
	if len(media) == 0 {
		return fmt.Errorf("%s: no media.*", fdir)
	}
	in := meeting.Input{
		Rec: meta.ID, Against: fm.Token, Date: meta.Started.Local().Format("2006-01-02"),
		Segments: segs, Turns: turns, Media: media[0],
		ExpectS: time.UnixMilli(ms).Sub(meta.Started).Seconds(),
	}
	mic := ""
	for _, role := range []string{"remote", "mic", "beam", "media"} {
		for _, t := range meta.Tracks {
			if t.Role == role {
				in.Audio = append(in.Audio, filepath.Join(dir, t.File))
				if role == "mic" {
					mic = filepath.Join(dir, t.File)
				}
			}
		}
	}
	res, err := meeting.Align(ctx, tools, in)
	if err != nil {
		return err
	}
	if !res.Paired {
		return fmt.Errorf("%s against %s: %s", res.Rec, res.Against, strings.Join(res.Flags, "; "))
	}
	path, err := (&meeting.Aligner{Data: m.Data}).Keep(ctx, res, mic)
	if err != nil {
		return err
	}
	s := res.Scores()
	fmt.Printf("%s against %s\n", res.Rec, res.Against)
	for _, p := range res.Search.Probes {
		fmt.Printf("probe %-5s feishu %8.2f s → local %8.2f s  score %.1f\n", p.At, p.FeishuS, p.LocalS, p.Score)
	}
	fmt.Printf("offset %.3f s (wall clocks said %.3f), drift %.1f ppm, score %.1f\n", s.OffsetS, res.ExpectS, s.DriftPPM, s.OffsetScore)
	fmt.Printf("overlap %.0f–%.0f s of Feishu, %d windows, %d reference tokens\n", res.OverlapS[0], res.OverlapS[1], len(res.Windows), res.RefTokens)
	fmt.Printf("CER %.2f%% (zh %.2f%%, en %.2f%%), cpCER %.2f%%, tcpCER %.2f%%\n", 100*s.CER, 100*s.CERZh, 100*s.WEREn, 100*s.CPCER, 100*s.TCPCER)
	if res.Me != "" {
		fmt.Printf("mic track = %s: %d training rows\n", res.Me, len(res.Train))
	}
	for _, f := range res.Flags {
		fmt.Println("flag:", f)
	}
	fmt.Println(path)
	return nil
}

// scoreCmd prints the score table: every alignment run since a day or a
// span ago. With --root, it compares engine roots instead (rootsCmd).
func scoreCmd(o app.LoadOpts, args []string) error {
	fs := flag.NewFlagSet("score", flag.ContinueOnError)
	since := fs.String("since", "", "from this day (2020-01-31) or this long ago (72h, 30d)")
	var roots []root
	fs.Func("root", "an engine root to compare, DIR or NAME=DIR; repeat it", func(s string) error {
		r, err := parseRoot(s)
		roots = append(roots, r)
		return err
	})
	last := fs.Int("last", 5, "with --root: this many of the newest scored records, when no id is named")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if len(roots) > 0 {
		return rootsCmd(o, roots, *last, fs.Args())
	}
	if fs.NArg() > 0 {
		return errors.New("score [--since DAY|SPAN] | score --root A --root B [--last N] [ID...]")
	}
	t, err := parseSince(*since, time.Now())
	if err != nil {
		return err
	}
	l, err := app.Load(o)
	if err != nil {
		return err
	}
	lines, err := meeting.ReadLines(filepath.Join(l.Meeting.Data, "scores.jsonl"), t)
	if err != nil {
		return err
	}
	return meeting.PrintTable(os.Stdout, lines)
}

// root is an engine root under comparison: a directory laid out like
// [asr.funasr] root (bin/llama-funasr-cli, models/), and its name.
type root struct{ Name, Dir string }

// parseRoot reads NAME=DIR, or DIR named by its last element.
func parseRoot(s string) (root, error) {
	name, dir, ok := strings.Cut(s, "=")
	if !ok {
		dir, name = s, filepath.Base(filepath.Clean(s))
	}
	if name == "" || dir == "" {
		return root{}, fmt.Errorf("--root %q: DIR or NAME=DIR", s)
	}
	abs, err := filepath.Abs(dir)
	if err != nil {
		return root{}, err
	}
	if r, err := filepath.EvalSymlinks(abs); err == nil {
		abs = r // a nix out-link names the store path it points at
	}
	for _, f := range []string{"bin/llama-funasr-cli", "models/funasr-encoder-f16.gguf", "models/qwen3-0.6b-q8_0.gguf", "models/fsmn-vad.gguf"} {
		if _, err := os.Stat(filepath.Join(abs, f)); err != nil {
			return root{}, fmt.Errorf("--root %s: %w", s, err)
		}
	}
	return root{Name: name, Dir: abs}, nil
}

// rootsCmd re-processes each record (the ids named, else the last n
// rescorable ones) with every root, one resident engine at a time, and
// scores each transcript against the record's Feishu turns at the offset
// its alignment fitted. Nothing is published: the transcripts and scores
// stay under <meeting.data>/score/<id>/<root>/.
func rootsCmd(o app.LoadOpts, roots []root, n int, ids []string) error {
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
	objs, err := c.List(ctx, "rec.")
	if err != nil {
		return err
	}
	var all []meeting.Record
	for _, obj := range objs {
		var r meeting.Record
		if err := json.Unmarshal(obj.Value.Data, &r); err == nil && r.State != meeting.Deleted {
			all = append(all, r)
		}
	}
	cands := meeting.Rescorable(all)
	var recs []meeting.Record
	if len(ids) == 0 {
		recs = cands[:min(n, len(cands))]
	}
	for _, id := range ids {
		id = strings.TrimPrefix(id, "rec.")
		i := slices.IndexFunc(cands, func(r meeting.Record) bool { return r.ID == id })
		if i < 0 {
			return fmt.Errorf("%s: not a scored record (a local recording with an align file, or a transcribed Feishu minute)", id)
		}
		recs = append(recs, cands[i])
	}
	if len(recs) == 0 {
		return errors.New("no scored record on the page")
	}
	fetch := fetcher(c, filepath.Join(l.Meeting.Data, "pull"))
	var out [][]meeting.RootRun
	for _, rec := range recs {
		basis, err := meeting.BasisOf(ctx, fetch, rec, all)
		if err != nil {
			return err
		}
		dir := meeting.PullDir(l.Meeting.Data, rec.ID)
		if _, err := meeting.Pull(ctx, c, rec.ID, dir); err != nil {
			return err
		}
		var runs []meeting.RootRun
		for _, r := range roots {
			cfg := l.Config
			cfg.ASR.FunASR.Root = r.Dir
			cfg.ASR.FunASR.LLM = "" // a --root is a whole engine: its own models/, not the config's llm
			tr := cfg.Transcriber()
			p := &meeting.Processor{
				ASR: tr, Post: l.Post(), Chunker: l.Chunker, Engine: l.ASR.Engine, Root: r.Dir, Logf: log.Printf,
				Out: filepath.Join(l.Meeting.Data, "score", rec.ID, r.Name),
			}
			got, err := p.Process(ctx, dir)
			if s, ok := tr.(interface{ Stop() }); ok {
				s.Stop()
			}
			if err != nil {
				return fmt.Errorf("%s with %s: %w", rec.ID, r.Name, err)
			}
			res := meeting.Rescore(basis.Prev, got.Segments, basis.Turns)
			if err := writeResult(filepath.Join(p.Out, "score.json"), res); err != nil {
				return err
			}
			log.Printf("score %s with %s: CER %.2f%% (zh %.2f%%, en %.2f%%)", rec.ID, r.Name, 100*res.CER, 100*res.CERZh, 100*res.WEREn)
			runs = append(runs, meeting.RootRun{Root: r.Name, Res: res, Out: got})
		}
		out = append(out, runs)
	}
	for _, r := range roots {
		fmt.Printf("%s = %s\n", r.Name, r.Dir)
	}
	fmt.Println()
	return meeting.PrintRoots(os.Stdout, out)
}

func writeResult(path string, res meeting.Result) error {
	b, err := json.MarshalIndent(res, "", " ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, append(b, '\n'), 0o600)
}

func parseSince(s string, now time.Time) (time.Time, error) {
	switch {
	case s == "":
		return time.Time{}, nil
	case strings.HasSuffix(s, "d"):
		n, err := strconv.Atoi(strings.TrimSuffix(s, "d"))
		if err != nil {
			return time.Time{}, fmt.Errorf("--since %q", s)
		}
		return now.AddDate(0, 0, -n), nil
	}
	if d, err := time.ParseDuration(s); err == nil {
		return now.Add(-d), nil
	}
	t, err := time.ParseInLocation("2006-01-02", s, time.Local)
	if err != nil {
		return time.Time{}, fmt.Errorf("--since %q: a day (2020-01-31) or a span (72h, 30d)", s)
	}
	return t, nil
}
