//go:build darwin

package main

import (
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/caoer/mega-asr/internal/ctl"
	"github.com/caoer/mega-asr/internal/session"
	"github.com/caoer/mega-asr/internal/store"
)

// takeRow is one take as `megavoice takes` lists it.
type takeRow struct {
	ID     string        `json:"id"`
	At     time.Time     `json:"at"`    // its start line's time, else its name's
	DurS   float64       `json:"dur_s"` // its stop line's length, else its WAV's
	State  string        `json:"state"` // store.Status: "" for a take from before the record
	Why    string        `json:"why,omitempty"`
	Seen   bool          `json:"seen,omitempty"`
	Dism   bool          `json:"dismissed,omitempty"` // undelivered, and the user dismissed it
	Target *store.Target `json:"target,omitempty"`    // what it was spoken for
	Text   string        `json:"text"`                // its delivered text
}

// takesArgs are the takes request's arguments.
type takesArgs struct {
	N           int  `json:"n"`           // at most this many; 0 is 20
	Undelivered bool `json:"undelivered"` // only takes whose state is undelivered
}

// takeNames are the store's take names, newest first.
func takeNames(dir string) ([]string, error) {
	var names []string
	for _, pat := range []string{"*.wav", "*.events.jsonl"} {
		paths, err := filepath.Glob(filepath.Join(dir, pat))
		if err != nil {
			return nil, err
		}
		for _, p := range paths {
			name := strings.TrimSuffix(strings.TrimSuffix(filepath.Base(p), ".wav"), ".events.jsonl")
			names = append(names, name)
		}
	}
	slices.Sort(names)
	names = slices.Compact(names)
	slices.Reverse(names)
	return names, nil
}

// takeRowOf reads the take at base.
func takeRowOf(base string) takeRow {
	name := filepath.Base(base)
	evs, _ := store.Read(base)
	st := store.State(evs)
	r := takeRow{ID: name, State: st.State, Why: st.Why, Seen: st.Seen, Dism: st.Dismissed}
	for _, e := range evs {
		switch e.Ev {
		case "start":
			r.At, r.Target = e.At, e.Target
		case "stop":
			r.DurS = e.DurS
		case "deliver":
			if e.Via == "auto" && e.Text != "" && r.Text == "" {
				r.Text = e.Text // no .txt: the partial text its delivery carried
			}
		}
	}
	if r.At.IsZero() {
		r.At, _ = time.ParseInLocation("20060102-150405", name[:min(15, len(name))], time.Local)
	}
	if fi, err := os.Stat(base + ".wav"); err == nil && r.DurS == 0 {
		r.DurS = float64((fi.Size()-44)/2) / 16000
	}
	if b, err := os.ReadFile(base + ".txt"); err == nil {
		r.Text = strings.TrimSuffix(string(b), "\n")
	}
	return r
}

// listTakes is the takes request: the newest takes first, at most n, only
// undelivered ones when asked.
func listTakes(dir string, a takesArgs) ([]takeRow, error) {
	if a.N <= 0 {
		a.N = 20
	}
	names, err := takeNames(dir)
	if err != nil {
		return nil, err
	}
	rows := []takeRow{}
	for _, name := range names {
		r := takeRowOf(filepath.Join(dir, name))
		if a.Undelivered && r.State != "undelivered" {
			continue
		}
		rows = append(rows, r)
		if len(rows) == a.N {
			break
		}
	}
	return rows, nil
}

// newestUndelivered is the take a resend without an id means at now: the
// newest undelivered take that started within the day before and that the
// user did not dismiss.
func newestUndelivered(dir string, now time.Time) (string, error) {
	names, err := takeNames(dir)
	if err != nil {
		return "", err
	}
	since := now.Add(-24 * time.Hour)
	for _, name := range names {
		r := takeRowOf(filepath.Join(dir, name))
		if r.At.Before(since) {
			break
		}
		if r.State == "undelivered" && !r.Dism {
			return name, nil
		}
	}
	return "", errors.New("no undelivered take in the last 24 h; name one (megavoice takes)")
}

const takesUsage = `usage: megavoice takes [-n N] [--undelivered] [--json]
       megavoice takes url [--rotate]

The newest takes first, one line each: name, length, state, target, the
text's first line. The name is the ID resend, retranscribe and events take.

  -n N            at most N takes (default 20)
  --undelivered   only takes whose state is undelivered
  --json          the rows as JSON: id, at, dur_s, state, why, seen, dismissed,
                  target, text

States: sent, pasted, cancelled, empty, undelivered (why: deliver_failed,
asr_failed, interrupted, delivery_cut, incomplete; then dismissed when the
user dismissed it on the Takes page), recording, transcribing; "-" is a take
from before takes kept a record.

takes url prints the Takes page's address with its key; --rotate writes a
new key, and every page opened before is refused.

Exit: 0 listed; 1 the agent is not running or answered an error; 2 usage.
`

// takesList is `megavoice takes` without url: the agent's list of takes.
func takesList(args []string) error {
	fs := flag.NewFlagSet("takes", flag.ContinueOnError)
	fs.Usage = func() { fmt.Fprint(os.Stderr, takesUsage) }
	var a takesArgs
	fs.IntVar(&a.N, "n", 20, "")
	fs.BoolVar(&a.Undelivered, "undelivered", false, "")
	asJSON := fs.Bool("json", false, "")
	if err := fs.Parse(args); err != nil || fs.NArg() > 0 {
		if err == nil {
			fs.Usage()
		}
		return exitCode(2)
	}
	b, _ := json.Marshal(a)
	var rows []takeRow
	if err := ask(ctl.Request{Cmd: "takes", Args: b}, 10*time.Second, &rows); err != nil {
		return err
	}
	if *asJSON {
		out, _ := json.MarshalIndent(rows, "", "  ")
		fmt.Println(string(out))
		return nil
	}
	w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	for _, r := range rows {
		state := r.State
		switch {
		case state == "":
			state = "-"
		case r.Why != "":
			state += " " + r.Why
		}
		if r.Dism {
			state += " dismissed"
		}
		line, _, _ := strings.Cut(r.Text, "\n")
		if rs := []rune(line); len(rs) > 60 {
			line = string(rs[:60]) + "…"
		}
		fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\n", r.ID, session.Clock(time.Duration(r.DurS*float64(time.Second))), state, targetName(r.Target), line)
	}
	return w.Flush()
}

// targetName is a record's target in a few words.
func targetName(t *store.Target) string {
	switch {
	case t == nil:
		return "-"
	case t.Kind == "herdr":
		return "pane " + t.Pane
	case t.Kind == "clipboard":
		return "clipboard"
	case t.App != "":
		return t.App
	}
	return t.Kind
}

const eventsUsage = `usage: megavoice events ID

Prints the record of the take named ID (megavoice takes lists the names),
<ID>.events.jsonl: one JSON event per line, as it was appended.

Exit: 0 printed; 1 no such take, or the agent is not running; 2 usage.
`

// eventsArgs are the events request's arguments.
type eventsArgs struct {
	ID string `json:"id"`
}

func eventsCmd(args []string) error {
	fs := flag.NewFlagSet("events", flag.ContinueOnError)
	fs.Usage = func() { fmt.Fprint(os.Stderr, eventsUsage) }
	if err := fs.Parse(args); err != nil || fs.NArg() != 1 {
		if err == nil {
			fs.Usage()
		}
		return exitCode(2)
	}
	b, _ := json.Marshal(eventsArgs{ID: fs.Arg(0)})
	var evs []json.RawMessage
	if err := ask(ctl.Request{Cmd: "events", Args: b}, 10*time.Second, &evs); err != nil {
		return err
	}
	for _, e := range evs {
		fmt.Println(string(e))
	}
	return nil
}

// ask sends one request and decodes its data into out.
func ask(req ctl.Request, timeout time.Duration, out any) error {
	resp, err := ctl.Call(sockPath(), req, timeout)
	if err != nil {
		return err
	}
	if !resp.OK {
		return errors.New(resp.Error)
	}
	return json.Unmarshal(resp.Data, out)
}

// handleTakes answers takes and events.
func (s *server) handleTakes(req ctl.Request) ctl.Response {
	switch req.Cmd {
	case "takes":
		var a takesArgs
		if err := unmarshalArgs(req.Args, &a); err != nil {
			return ctl.Fail(err)
		}
		rows, err := listTakes(s.cfg.Store.Data, a)
		if err != nil {
			return ctl.Fail(err)
		}
		return ctl.Reply(rows)
	case "events":
		var a eventsArgs
		if err := unmarshalArgs(req.Args, &a); err != nil {
			return ctl.Fail(err)
		}
		base, err := s.ctrl.Base(a.ID)
		if err != nil {
			return ctl.Fail(err)
		}
		evs, err := store.Read(base)
		if errors.Is(err, os.ErrNotExist) {
			return ctl.Fail(fmt.Errorf("%s has no record: it is from before takes kept one", a.ID))
		}
		if err != nil {
			return ctl.Fail(err)
		}
		return ctl.Reply(evs)
	}
	return ctl.Fail(fmt.Errorf("unknown command %q", req.Cmd))
}

// unmarshalArgs decodes a request's arguments; none leaves v as it is.
func unmarshalArgs(raw json.RawMessage, v any) error {
	if len(raw) == 0 {
		return nil
	}
	if err := json.Unmarshal(raw, v); err != nil {
		return fmt.Errorf("bad arguments: %w", err)
	}
	return nil
}
