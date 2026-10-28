package main

import (
	"bufio"
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/caoer/mega-asr/internal/app"
	"github.com/caoer/mega-asr/internal/names"
)

// namesLine is one line of `megavoice names`: what the gate did to a text.
type namesLine struct {
	ID         string            `json:"id,omitempty"`
	Candidates []names.Candidate `json:"candidates"`
	Fired      bool              `json:"fired"`
	Questions  int               `json:"questions,omitempty"`
	Edits      []names.Edit      `json:"edits"`
	Text       string            `json:"text"`
	GateMS     float64           `json:"gate_ms"`           // the whole gate: roster read, scan and request
	WallMS     float64           `json:"wall_ms,omitempty"` // the Jev request alone
	Tokens     int               `json:"input_tokens,omitempty"`
	Err        string            `json:"err,omitempty"`
}

// namesCmd runs the names gate of the config over each line of in, in
// order, one request at a time, as a take would.
func namesCmd(o app.LoadOpts, args []string, in io.Reader, out io.Writer) error {
	fs := flag.NewFlagSet("names", flag.ContinueOnError)
	scan := fs.Bool("scan", false, "list the candidates; ask nothing")
	ungated := fs.Bool("ungated", false, "ask on any candidate, not only a same-sound one")
	if err := fs.Parse(args); err != nil || fs.NArg() > 0 {
		return exitCode(2)
	}
	c, err := load(o)
	if err != nil {
		return err
	}
	g := c.NamesGate()
	g.Ungated = *ungated
	enc := json.NewEncoder(out)
	enc.SetEscapeHTML(false)
	sc := bufio.NewScanner(in)
	sc.Buffer(make([]byte, 1<<20), 1<<24)
	for sc.Scan() {
		line := sc.Text()
		if strings.TrimSpace(line) == "" {
			continue
		}
		var it struct{ ID, Text string }
		if strings.HasPrefix(line, "{") {
			if err := json.Unmarshal([]byte(line), &it); err != nil {
				return fmt.Errorf("names: %w", err)
			}
		} else {
			it.Text = line
		}
		r, err := names.ReadRoster(g.People)
		if err != nil {
			return err
		}
		l := namesLine{ID: it.ID, Candidates: append([]names.Candidate{}, r.Candidates(it.Text)...), Edits: []names.Edit{}, Text: it.Text}
		if !*scan {
			t0 := time.Now()
			res := g.Correct(context.Background(), it.Text)
			l.GateMS = float64(time.Since(t0).Microseconds()) / 1000
			l.Fired, l.Questions, l.Text, l.Tokens = res.Fired, res.Questions, res.Text, res.Usage.InputTokens
			l.WallMS = float64(res.Wall.Microseconds()) / 1000
			if res.Edits != nil {
				l.Edits = res.Edits
			}
			if res.Err != nil {
				l.Err = res.Err.Error()
			}
		} else {
			l.Fired = names.Fires(l.Candidates)
		}
		if err := enc.Encode(l); err != nil {
			return err
		}
	}
	return sc.Err()
}
