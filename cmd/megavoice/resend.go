//go:build darwin

package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/caoer/mega-asr/internal/ctl"
	"github.com/caoer/mega-asr/internal/session"
	"github.com/caoer/mega-asr/internal/store"
	"golang.org/x/sys/unix"
)

// resendArgs are the resend request's arguments.
type resendArgs struct {
	ID     string        `json:"id"`   // "": the newest undelivered take of the last 24 h
	To     string        `json:"to"`   // front, pane:<id>, app:<pid>:<bundle id> or clipboard; "" is front
	Send   bool          `json:"send"` // press Enter after the paste
	Text   string        `json:"text"` // delivered, raw, an engine, or retranscription N; "" is delivered
	Caller *store.Caller `json:"caller,omitempty"`
}

// retranscribeArgs are the retranscribe request's arguments.
type retranscribeArgs struct {
	ID     string `json:"id"`
	Engine string `json:"engine"`
}

const resendUsage = `usage: megavoice resend [ID] [--to TARGET] [--send] [--text TEXT]

Delivers a kept take's text again and appends a deliver line (via cli, with
the calling process) to its record. It waits up to 10 s for a delivery under
way. ID is a take's name (megavoice takes); without one, the newest
undelivered take of the last 24 h.

  --to TARGET   where the text goes (default front):
                  front        what has focus now, as a take's start sees it
                  pane:<id>    a herdr pane
                  app:<pid>:<bundle id>
                               a running app's focused window, refused
                               when another app runs as that pid now
                  clipboard    copied; no key is pressed
  --send        press Enter after the paste (not with clipboard); without
                it the text is pasted and the Enter is yours
  --text TEXT   which text (default delivered):
                  delivered            the take's delivered text (.txt)
                  raw                  the ASR's text before corrections
                  <engine>             that engine's answer in compare mode
                  "retranscription N"  the Nth re-transcription's answer

Prints the deliver line. Exit: 0 delivered; 1 refused, nothing appended (no
such take, still recording or transcribing, no text of that kind, a target
herdr does not list or no app runs as, another delivery held on for 10 s,
the agent is not running); 2 usage; 3 the delivery or its Enter failed: the
line says why, the text is on the clipboard; 4 the record could not be
written: the line printed is not in it, and the message says whether the
text went out.
`

const retranscribeUsage = `usage: megavoice retranscribe ID --engine NAME

Decodes a kept take's WAV again with NAME and appends the answer to its
record as a retranscribe line (engine, model, raw, text, latency_ms, err).
No stored text changes: .txt, .raw.txt and .compare.json stay as they are.

  --engine NAME   funasr (on this Mac, behind any live take) or doubao (a
                  cloud engine: the take's audio goes to Volcengine; refused
                  unless asr.engine or, with compare.on, compare.engines
                  names it)

Prints the retranscribe line. Exit: 0 the answer is appended; 1 refused,
nothing appended (no such take or no audio, still recording or
transcribing, being re-transcribed already, an engine the config does not
name, the agent is not running); 2 usage; 3 the engine failed: the line
carries its error; 4 the record could not be written: the line printed is
not in it.
`

// parseAround parses fs's flags before and after the one positional
// argument a command takes at most; it returns the positional ones.
func parseAround(fs *flag.FlagSet, args []string) ([]string, error) {
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

func resendCmd(args []string) error {
	fs := flag.NewFlagSet("resend", flag.ContinueOnError)
	fs.Usage = func() { fmt.Fprint(os.Stderr, resendUsage) }
	var a resendArgs
	fs.StringVar(&a.To, "to", "front", "")
	fs.BoolVar(&a.Send, "send", false, "")
	fs.StringVar(&a.Text, "text", "delivered", "")
	pos, err := parseAround(fs, args)
	if err != nil || len(pos) > 1 {
		if err == nil {
			fs.Usage()
		}
		return exitCode(2)
	}
	if a.Send && a.To == "clipboard" {
		fmt.Fprintln(os.Stderr, "megavoice: --send has no Enter to press on the clipboard")
		return exitCode(2)
	}
	if len(pos) == 1 {
		a.ID = pos[0]
	}
	a.Caller = parent()
	b, _ := json.Marshal(a)
	de, unrecorded, err := askLine(ctl.Request{Cmd: "resend", Args: b}, 30*time.Second)
	if err != nil {
		return err
	}
	out, _ := json.Marshal(de)
	fmt.Println(string(out))
	switch {
	case unrecorded != "":
		fmt.Fprintln(os.Stderr, "megavoice: "+unrecorded)
		return exitCode(4)
	case !de.OK || de.Err != "":
		return exitCode(3)
	}
	return nil
}

func retranscribeCmd(args []string) error {
	fs := flag.NewFlagSet("retranscribe", flag.ContinueOnError)
	fs.Usage = func() { fmt.Fprint(os.Stderr, retranscribeUsage) }
	var a retranscribeArgs
	fs.StringVar(&a.Engine, "engine", "", "")
	pos, err := parseAround(fs, args)
	if err != nil || len(pos) != 1 || a.Engine == "" {
		if err == nil {
			fs.Usage()
		}
		return exitCode(2)
	}
	a.ID = pos[0]
	b, _ := json.Marshal(a)
	re, unrecorded, err := askLine(ctl.Request{Cmd: "retranscribe", Args: b}, 15*time.Minute)
	if err != nil && strings.Contains(err.Error(), "i/o timeout") {
		return fmt.Errorf("no answer within 15 min; the agent carries on and appends it (megavoice events %s)", a.ID)
	}
	if err != nil {
		return err
	}
	out, _ := json.Marshal(re)
	fmt.Println(string(out))
	switch {
	case unrecorded != "":
		fmt.Fprintln(os.Stderr, "megavoice: "+unrecorded)
		return exitCode(4)
	case re.Err != "":
		return exitCode(3)
	}
	return nil
}

// askLine asks for a resend or a re-transcription and returns the line it
// answers with; unrecorded is the error when the record could not take that
// line, which is then not in it.
func askLine(req ctl.Request, timeout time.Duration) (line store.Event, unrecorded string, err error) {
	resp, err := ctl.Call(sockPath(), req, timeout)
	if err != nil {
		return line, "", err
	}
	if !resp.OK && len(resp.Data) == 0 {
		return line, "", errors.New(resp.Error)
	}
	if err := json.Unmarshal(resp.Data, &line); err != nil {
		return line, "", err
	}
	return line, resp.Error, nil
}

// lineReply answers with the line a resend or a re-transcription returned:
// with its error too when the record could not take the line
// (session.ErrNotRecorded), the error alone for a refusal.
func lineReply(line store.Event, err error) ctl.Response {
	switch {
	case errors.Is(err, session.ErrNotRecorded):
		r := ctl.Reply(line)
		r.OK, r.Error = false, err.Error()
		return r
	case err != nil:
		return ctl.Fail(err)
	}
	return ctl.Reply(line)
}

// parent is the process that ran this command, for a deliver line's caller.
func parent() *store.Caller {
	c := &store.Caller{PID: os.Getppid()}
	if kp, err := unix.SysctlKinfoProc("kern.proc.pid", c.PID); err == nil {
		c.Process = unix.ByteSliceToString(kp.Proc.P_comm[:])
	}
	return c
}

// handleResend answers resend and retranscribe.
func (s *server) handleResend(req ctl.Request) ctl.Response {
	switch req.Cmd {
	case "resend":
		var a resendArgs
		if err := unmarshalArgs(req.Args, &a); err != nil {
			return ctl.Fail(err)
		}
		if a.ID == "" {
			id, err := newestUndelivered(s.cfg.Store.Data, time.Now())
			if err != nil {
				return ctl.Fail(err)
			}
			a.ID = id
		}
		to, err := s.dlv.Resolve(a.To)
		if err != nil {
			return ctl.Fail(err)
		}
		q := session.Resend{ID: a.ID, To: to, Send: a.Send, Text: a.Text, Via: "cli", Caller: a.Caller}
		if a.To == "" || a.To == "front" {
			q.Where = "当前光标"
		}
		return lineReply(s.ctrl.Resend(q))
	case "retranscribe":
		var a retranscribeArgs
		if err := unmarshalArgs(req.Args, &a); err != nil {
			return ctl.Fail(err)
		}
		e, err := s.cfg.Retranscriber(s.opts)(a.Engine)
		if err != nil {
			return ctl.Fail(err)
		}
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
		defer cancel()
		return lineReply(s.ctrl.Retranscribe(ctx, a.ID, e))
	}
	return ctl.Fail(fmt.Errorf("unknown command %q", req.Cmd))
}
