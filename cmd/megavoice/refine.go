package main

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"strings"

	"github.com/caoer/mega-asr/internal/app"
	"github.com/caoer/mega-asr/internal/session"
	"github.com/caoer/mega-asr/internal/store"
)

// refineLine is one line of `megavoice refine`: what the model step did to
// a text.
type refineLine struct {
	ID     string       `json:"id,omitempty"`
	Text   string       `json:"text"`   // the text a take would deliver
	Refine *store.Event `json:"refine"` // the take's refine line
}

// refineCmd runs the model step of the config over each line of in, in
// order, one call at a time, as a take would.
func refineCmd(o app.LoadOpts, args []string, in io.Reader, out io.Writer) error {
	fs := flag.NewFlagSet("refine", flag.ContinueOnError)
	if err := fs.Parse(args); err != nil || fs.NArg() > 0 {
		return exitCode(2)
	}
	c, err := load(o)
	if err != nil {
		return err
	}
	if !c.Refine.On {
		return errors.New("refine: refine.on is false; --set refine.on=true runs the step")
	}
	stage := c.RefineStage(o)
	stage.Display = display()
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
				return fmt.Errorf("refine: %w", err)
			}
		} else {
			it.Text = line
		}
		l := refineLine{ID: it.ID}
		take := session.RefineInput{Text: it.Text}
		if wait := stage.Screen(); wait != nil { // refine.context.screen: the window in front now
			take.Screen = wait(context.Background())
		}
		l.Text, l.Refine = c.Refine.RunTake(context.Background(), take)
		if err := enc.Encode(l); err != nil {
			return err
		}
	}
	return sc.Err()
}
