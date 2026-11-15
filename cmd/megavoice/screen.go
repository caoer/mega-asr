//go:build darwin

package main

import (
	"encoding/json"
	"errors"
	"flag"
	"io"
	"log"
	"os"
	"time"

	"github.com/caoer/mega-asr/internal/app"
	"github.com/caoer/mega-asr/internal/l10n"
	"github.com/caoer/mega-asr/internal/mac"
	"github.com/caoer/mega-asr/internal/refine"
	"github.com/caoer/mega-asr/internal/store"
)

// display is this Mac's screen for the model step's screen source.
func display() refine.Display { return mac.Screen{} }

// askScreen asks for the Screen Recording grant when it is off, as the
// screen source is switched on: the system's prompt the first time. It
// returns what to tell the user while the grant stays off.
func askScreen() error {
	d := mac.Screen{}
	if d.Access() {
		return nil
	}
	granted := d.Request()
	log.Printf("menu: Screen Recording asked for as the screen source was switched on: granted %v", granted)
	if granted {
		return nil
	}
	return errors.New(l10n.T("请在系统设置里允许 Mega Voice 录制屏幕，然后重启它；在那之前转译不带屏幕",
		"Allow Screen Recording for Mega Voice in System Settings, then restart it"))
}

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
