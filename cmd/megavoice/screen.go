//go:build darwin

package main

import (
	"encoding/json"
	"flag"
	"io"
	"log"
	"os"
	"time"

	"github.com/caoer/mega-asr/internal/app"
	"github.com/caoer/mega-asr/internal/mac"
	"github.com/caoer/mega-asr/internal/refine"
	"github.com/caoer/mega-asr/internal/store"
)

// display is this Mac's screen for the model step's screen source.
func display() refine.Display { return mac.Screen{} }

// screenLine is `megavoice screen`'s line: the screen source as a refine
// line records it, and the <context> a call would carry.
type screenLine struct {
	Screen  *store.Screen `json:"screen"`
	Context string        `json:"context,omitempty"`
}

// screenCmd runs the model step's screen source once on the window in
// front and prints what it took.
func screenCmd(o app.LoadOpts, args []string, out io.Writer) error {
	fs := flag.NewFlagSet("screen", flag.ContinueOnError)
	mode := fs.String("mode", refine.ScreenOCR, "ocr (the window's text) or image (its picture)")
	edge := fs.Int("edge", 0, "the picture's long edge in pixels (default the step's)")
	after := fs.Duration("after", 0, "wait this long first, to bring a window in front")
	file := fs.String("out", "", "write the text or the JPEG here")
	if err := fs.Parse(args); err != nil || fs.NArg() != 0 || *mode != refine.ScreenOCR && *mode != refine.ScreenImage {
		return exitCode(2)
	}
	c, err := load(o)
	if err != nil {
		return err
	}
	time.Sleep(*after)
	if why := askGrant(display()); why != nil {
		log.Print(why)
	}
	sc := refine.ScreenSource{Mode: *mode, Exclude: c.Refine.Context.Exclude, Edge: *edge, Display: mac.Screen{}}.Take()
	if *file != "" && sc.Withheld == "" {
		data := []byte(sc.Text + "\n")
		if sc.Image != nil {
			data = sc.Image.Data
		}
		if err := os.WriteFile(*file, data, 0o644); err != nil {
			return err
		}
	}
	enc := json.NewEncoder(out)
	enc.SetEscapeHTML(false)
	return enc.Encode(screenLine{Screen: app.ScreenRecord(sc), Context: sc.Context()})
}
