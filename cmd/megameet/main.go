// megameet — record a meeting as two tracks: what the meeting app plays (a
// Core Audio process tap) and the local mic, frame-aligned, each a WAV under
// <meeting.data>/recordings/<id>/tracks/ beside meta.toml. `serve` runs as
// the MegaMeet.app launchd agent and owns the recording, so macOS asks the
// app, not the calling terminal, for the permissions; the other commands
// talk to it over its control socket.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/caoer/mega-asr/internal/app"
	"github.com/caoer/mega-asr/internal/audio"
	"github.com/caoer/mega-asr/internal/ctl"
	"github.com/caoer/mega-asr/internal/pages"
)

const usage = `usage: megameet [--config FILE] [--set KEY=VALUE]... <command> [args]

  --config FILE             settings file (default: $MEGAVOICE_CONFIG, else
                            ~/.config/megavoice/config.toml; the [meeting] tables)
  --set section.key=value   override one setting for this run; repeatable

  serve                     run the recorder (launchd starts it)
  start [--app BUNDLE_ID]... [--title T] [--speakers N] [--test]
                            record the apps' audio and the mic; --app defaults
                            to meeting.capture.apps; --test marks the record a
                            test: processed, never ingested into the wiki
  stop                      end the recording; prints its directory
  status                    the recording in progress, or the last one; with
                            [meeting.page], the registry: the drain's last tick,
                            the queue, failed records, recent RTF
  mics                      input devices, their UIDs, and the one
                            meeting.capture.mic selects (each start reads it)
  apps                      Core Audio client processes: the bundle id to tap
                            is the one with out=Y while the other side speaks
  upload                    push every recording still pending to the page
                            ([meeting.page]); serve does this at stop and start
  upload --rec ID --role ROLE FILE
                            replace one file of a record (a sha256 mismatch)
  upload --source file [--title T] [--speakers N] [--test] FILE...
                            register media files as a new record, on any host;
                            --test as for start
  pull <id>                 fetch a record's files into <meeting.data>/pull/<id>
                            (meta.toml, tracks/), each checked against its
                            sha256, and mark them verified on the record
  process <id>              pull, then transcribe every track (one resident
                            ASR) into segments.json and transcript.md, with
                            the source's stages; both uploaded to the record
  register-feishu [--archive DIR] [--wiki DIR]
                            register every minute the Feishu archive has
                            fetched (meeting.align.feishu_archive) that the page
                            lacks; minutes the wiki (meeting.ingest.wiki) has
                            filed carry wiki, so they are not ingested again
  align <id>                align a registry record with the Feishu minute of
                            the same meeting: a local recording against the
                            minute, a minute onto the recordings it overlaps;
                            scores on the local record, align.json uploaded
  align --dir DIR --feishu DIR
                            the same over a recording folder and a Feishu
                            minute folder, no registry
  score [--since DAY|SPAN]  the score table: every alignment run (the ASR
                            regression gate)
  score --root A --root B [--last N] [ID...]
                            re-process the named records, else the last N
                            (default 5) scored ones, with each engine root (DIR
                            or NAME=DIR, laid out like [asr.funasr] root; the
                            config's asr.funasr.llm does not apply) and print each root's CER (all, zh, en) and its delta
                            against the first; nothing is published
  drain [--every 60s] [--max N]
                            the server: claim queued records (q.<id>) and run
                            them — pull, process, align, and the ingest when
                            meeting.ingest.auto; one tick, or one every --every
  check <id|DIR>            is a pulled directory complete (docs/megameet.md §
                            Directory contract)? exit 0, or name what is missing
  delete <id>...            delete records: each becomes a tombstone (state
                            deleted; its files and q.<id> purged, so no source
                            registers it again), and this host's recording,
                            pull and pending copies go
  titles [--limit N] [--ids ID,... [--redo]]
                            write display_title and labels, with
                            [meeting.summary]'s model, onto every record that
                            lacks either (only those keys; a rerun changes
                            nothing); prints each record and the cost;
                            --redo rewrites the named records' two keys
  labels <command>          the closed and open label lists: list, records,
                            log; rename, merge, split, set; ask, question,
                            apply for changes that wait for an approval
                            (promote, demote, any naming a closed label);
                            megameet labels help
`

func main() {
	var o app.LoadOpts
	gfs := flag.NewFlagSet("megameet", flag.ContinueOnError)
	gfs.Usage = func() { fmt.Fprint(os.Stderr, usage) }
	gfs.StringVar(&o.Path, "config", "", "settings file")
	gfs.Func("set", "section.key=value", func(s string) error { o.Sets = append(o.Sets, s); return nil })
	if err := gfs.Parse(os.Args[1:]); err != nil || gfs.NArg() < 1 {
		if err == nil {
			fmt.Fprint(os.Stderr, usage)
		}
		os.Exit(2)
	}
	cmd, args := gfs.Arg(0), gfs.Args()[1:]
	var err error
	switch cmd {
	case "serve":
		err = serve(o)
	case "start":
		err = start(args)
	case "stop":
		err = stop()
	case "status":
		err = status(o)
	case "apps":
		err = apps()
	case "mics":
		err = mics(o)
	case "upload":
		err = upload(o, args)
	case "pull":
		err = pullCmd(o, args)
	case "process":
		err = processCmd(o, args)
	case "register-feishu":
		err = registerFeishuCmd(o, args)
	case "align":
		err = alignCmd(o, args)
	case "score":
		err = scoreCmd(o, args)
	case "drain":
		err = drainCmd(o, args)
	case "check":
		err = checkCmd(o, args)
	case "delete":
		err = deleteCmd(o, args)
	case "titles":
		err = titlesCmd(o, args)
	case "labels":
		err = labelsCmd(o, args)
	default:
		fmt.Fprint(os.Stderr, usage)
		os.Exit(2)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "megameet:", err)
		os.Exit(1)
	}
}

func stateDir() string {
	if d := os.Getenv("XDG_STATE_HOME"); d != "" {
		return filepath.Join(d, "megameet")
	}
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".local", "state", "megameet")
}

func sockPath() string { return filepath.Join(stateDir(), "ctl.sock") }

// call sends one request to serve and decodes its data into v.
func call(req ctl.Request, timeout time.Duration, v any) error {
	resp, err := ctl.Call(sockPath(), req, timeout)
	if errors.Is(err, ctl.ErrNotRunning) {
		return fmt.Errorf("megameet serve is not running (%s)", sockPath())
	}
	if err != nil {
		return err
	}
	if !resp.OK {
		return errors.New(resp.Error)
	}
	if v == nil || len(resp.Data) == 0 {
		return nil
	}
	return json.Unmarshal(resp.Data, v)
}

type multiFlag []string

func (m *multiFlag) String() string     { return strings.Join(*m, ",") }
func (m *multiFlag) Set(s string) error { *m = append(*m, s); return nil }

func start(args []string) error {
	fs := flag.NewFlagSet("start", flag.ContinueOnError)
	var a StartArgs
	var appsFlag multiFlag
	fs.Var(&appsFlag, "app", "bundle id to tap; repeatable")
	fs.StringVar(&a.Title, "title", "", "meeting title")
	fs.IntVar(&a.Speakers, "speakers", 0, "speakers the diarizer expects; 0 estimates")
	fs.BoolVar(&a.Test, "test", false, "a test recording: processed, never ingested")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() > 0 {
		return fmt.Errorf("start: unexpected %q", fs.Args())
	}
	a.Apps = appsFlag
	b, _ := json.Marshal(a)
	var m Meta
	// The first recording under launchd waits on the permission prompts.
	if err := call(ctl.Request{Cmd: "start", Args: b}, 2*time.Minute, &m); err != nil {
		return err
	}
	fmt.Printf("recording %s (apps %s)\n", m.ID, strings.Join(m.Apps, ", "))
	fmt.Println(checkMic(func() (Status, error) {
		var s Status
		return s, call(ctl.Request{Cmd: "status"}, 5*time.Second, &s)
	}, 250*time.Millisecond, 15*time.Second))
	return nil
}

// checkMic watches a new recording's status until its mic has delivered
// audio.DeadAfter and a little more, and says whether the mic is live or
// digital silence (a muted or disconnected input).
func checkMic(status func() (Status, error), every, limit time.Duration) string {
	enough := (audio.DeadAfter + 500*time.Millisecond).Seconds()
	for deadline := time.Now().Add(limit); ; time.Sleep(every) {
		s, err := status()
		switch {
		case err != nil:
			return "mic check: " + err.Error()
		case !s.Recording:
			return "mic check: the recording ended"
		case s.Mic.Silent:
			return "WARNING: " + s.Mic.Warning
		case s.Seconds >= enough:
			return fmt.Sprintf("mic %q: live, %.0f dBFS", s.Mic.Device, s.Mic.LevelDBFS)
		case time.Now().After(deadline):
			return fmt.Sprintf("mic check: only %.1f s recorded after %s; see `megameet status`", s.Seconds, limit)
		}
	}
}

func stop() error {
	var s struct {
		Meta
		Dir string `json:"dir"`
	}
	if err := call(ctl.Request{Cmd: "stop"}, 30*time.Second, &s); err != nil {
		return err
	}
	fmt.Printf("stopped %s: %.1f s\n%s\n", s.ID, s.DurationS, s.Dir)
	if s.Error != "" {
		fmt.Printf("ended with: %s\n", s.Error)
	}
	return nil
}

// status prints the recorder's state when it runs here, and the
// registry's when a page is configured; either one suffices.
func status(o app.LoadOpts) error {
	l, lerr := app.Load(o)
	page := lerr == nil && l.Meeting.Page.Slug != ""
	err := recorderStatus()
	if !page {
		return errors.Join(err, lerr)
	}
	if err != nil && !strings.Contains(err.Error(), "is not running") {
		fmt.Println("recorder:", err)
	}
	c, err := pages.FromConfig(l.Meeting)
	if err != nil {
		return err
	}
	return drainStatus(os.Stdout, context.Background(), l.Meeting, c)
}

func recorderStatus() error {
	var s Status
	if err := call(ctl.Request{Cmd: "status"}, 5*time.Second, &s); err != nil {
		return err
	}
	if s.Recording {
		fmt.Printf("recording %s: %.0f s (apps %s)\n", s.Current.ID, s.Seconds, strings.Join(s.Current.Apps, ", "))
		fmt.Printf("mic %q: %.0f dBFS\n", s.Mic.Device, s.Mic.LevelDBFS)
		if s.Mic.Silent {
			fmt.Printf("WARNING: %s (since %s)\n", s.Mic.Warning, s.Mic.Since.Format("15:04:05"))
		}
	} else {
		fmt.Println("idle")
	}
	if s.Last != nil {
		fmt.Printf("last %s: %.1f s", s.Last.ID, s.Last.DurationS)
		if s.Last.Error != "" {
			fmt.Printf(", ended with: %s", s.Last.Error)
		}
		fmt.Println()
	}
	for id, p := range s.Uploads {
		fmt.Printf("upload %s: %s\n", id, p)
	}
	fmt.Println("recordings:", s.Dir)
	return nil
}

func apps() error {
	list, err := audio.Apps()
	if err != nil {
		return err
	}
	w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintln(w, "PID\tOUT\tIN\tBUNDLE\tNAME")
	yn := map[bool]string{true: "Y", false: "-"}
	for _, a := range list {
		fmt.Fprintf(w, "%d\t%s\t%s\t%s\t%s\n", a.PID, yn[a.Out], yn[a.In], a.Bundle, a.Name)
	}
	return w.Flush()
}

func upload(o app.LoadOpts, args []string) error {
	fs := flag.NewFlagSet("upload", flag.ContinueOnError)
	rec := fs.String("rec", "", "record id whose file --role replaces")
	role := fs.String("role", "", "the file's role in the record")
	source := fs.String("source", "", "file: register FILEs as a new record")
	title := fs.String("title", "", "the new record's title")
	speakers := fs.Int("speakers", 0, "speakers the diarizer expects; 0 estimates")
	test := fs.Bool("test", false, "with --source file: a test record, processed, never ingested")
	files, err := parseInterspersed(fs, args)
	if err != nil {
		return err
	}
	if *test && *source == "" {
		return errors.New("--test marks a new record: upload --source file --test FILE...")
	}
	l, err := app.Load(o)
	if err != nil {
		return err
	}
	c := l.Meeting
	pend := ledger(filepath.Join(c.Data, "pending"))
	cl, err := pages.FromConfig(c)
	if err != nil {
		return err
	}
	u := &uploader{reg: cl, ledger: pend, now: time.Now} // progress goes to the log, on stderr
	ctx := context.Background()
	switch {
	case *rec != "":
		if *role == "" || len(files) != 1 {
			return errors.New("upload --rec ID --role ROLE FILE")
		}
		f, err := u.replace(ctx, *rec, *role, files[0])
		if err != nil {
			return err
		}
		fmt.Printf("rec.%s: %s is file %s (%d bytes, sha256 %s)\n", *rec, f.Role, f.File, f.Bytes, f.SHA256)
		return nil
	case *source != "":
		if *source != "file" || len(files) == 0 {
			return errors.New("upload --source file [--title T] [--test] FILE...")
		}
		j, err := fileJob(files, *title, *speakers, hostName())
		if err != nil {
			return err
		}
		j.Test = *test
		if _, err := os.Stat(pend.path(j.ID)); err == nil {
			return fmt.Errorf("%s is already pending; `megameet upload` resumes it", j.ID)
		}
		if err := pend.put(j); err != nil {
			return err
		}
		fmt.Printf("rec.%s: %d file(s)\n", j.ID, len(j.Files))
		return u.run(ctx, j.ID)
	case len(files) > 0:
		return errors.New("FILE needs --rec/--role or --source file")
	}
	return u.resume(ctx)
}

// fileJob registers media files as one record: role media each, started
// where the longest one would have begun if it ended at its mtime.
func fileJob(paths []string, title string, speakers int, host string) (job, error) {
	j := job{Source: "file", Host: host, Title: title, Speakers: speakers}
	for _, p := range paths {
		abs, err := filepath.Abs(p)
		if err != nil {
			return j, err
		}
		st, err := os.Stat(abs)
		if err != nil {
			return j, err
		}
		if d := probeDuration(abs); d > j.DurationS {
			j.DurationS = d
		}
		if st.ModTime().After(j.Stopped) {
			j.Stopped = st.ModTime()
		}
		j.Files = append(j.Files, jobFile{Role: "media", Path: abs, Codec: codecOf(abs)})
	}
	j.Started = j.Stopped.Add(-time.Duration(j.DurationS * float64(time.Second)))
	j.ID = j.Started.Format("20060102-150405") + "-file-" + host
	return j, nil
}

// parseInterspersed parses flags wherever they stand among the positional
// arguments (`upload x.m4a --source file`) and returns the positionals.
func parseInterspersed(fs *flag.FlagSet, args []string) ([]string, error) {
	var pos []string
	for {
		if err := fs.Parse(args); err != nil {
			return nil, err
		}
		if fs.NArg() == 0 {
			return pos, nil
		}
		pos = append(pos, fs.Arg(0))
		args = fs.Args()[1:]
	}
}
