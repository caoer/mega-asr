package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"log"
	"math"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/caoer/mega-asr/internal/app"
	"github.com/caoer/mega-asr/internal/asr"
	"github.com/caoer/mega-asr/internal/audio"
	"github.com/caoer/mega-asr/internal/score"
	"github.com/caoer/mega-asr/internal/session"
	"github.com/caoer/mega-asr/internal/takes"
)

// record captures from the configured source for a fixed time into a WAV and
// prints its level.
func record(o app.LoadOpts, args []string) error {
	fs := flag.NewFlagSet("record", flag.ContinueOnError)
	secs := fs.Float64("seconds", 5, "how long to record")
	if err := fs.Parse(args); err != nil || fs.NArg() != 1 {
		return exitCode(2)
	}
	c, err := load(o)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(stateDir(), 0o700); err != nil {
		return err
	}
	src := captureSource(c.Capture)
	ch, err := src.Start(context.Background())
	if err != nil {
		return err
	}
	time.AfterFunc(time.Duration(*secs*float64(time.Second)), src.Stop)
	var samples []int16
	for s := range ch {
		samples = append(samples, s...)
	}
	if err := src.Err(); err != nil {
		return err
	}
	if err := audio.SaveWAV(fs.Arg(0), samples); err != nil {
		return err
	}
	rms, peak := audio.Stats(samples)
	from := fmt.Sprintf("%s:%s ch %d", c.Capture.Host, c.Capture.Device, c.Capture.Channel)
	if r, ok := src.(*audio.Remote); ok {
		from = "remote mic " + r.Name + " (" + r.Addr + ")"
	}
	if m, ok := src.(*audio.Mic); ok {
		from = m.Device()
		if c.Capture.MicChannel > 0 {
			from += fmt.Sprintf(" ch %d", c.Capture.MicChannel)
		}
		if n := m.Note(); n != "" {
			from += " (" + n + ")"
		}
	}
	fmt.Printf("%s: %d samples (%.2f s) from %s, RMS %.1f dBFS, peak %.1f dBFS\n",
		fs.Arg(0), len(samples), float64(len(samples))/audio.Rate, from, rms, peak)
	return nil
}

// transcribe prints a WAV's transcript as megavoice would deliver it: the
// WAV is a take through serve's controller and chunk ASR (the resident
// process on Metal, the CLI as fallback, the config file's engine flags),
// with the hotwords and the correction table; --plain skips both. The engine
// is asr.engine, or --engine's; its settings are the config file's, so a
// changed asr.funasr.llm is heard here before a restart of serve.
func transcribe(o app.LoadOpts, args []string) error {
	fs := flag.NewFlagSet("transcribe", flag.ContinueOnError)
	plain := fs.Bool("plain", false, "no hotwords, no corrections")
	engine := fs.String("engine", "", "funasr or doubao (default asr.engine)")
	if err := fs.Parse(args); err != nil || fs.NArg() != 1 {
		return exitCode(2)
	}
	c, err := load(o)
	if err != nil {
		return err
	}
	if *engine == "" {
		*engine = c.ASR.Engine
	}
	d, err := newDecoder(c.Config, *engine, *plain)
	if err != nil {
		return err
	}
	defer d.close()
	text, err := d.text(fs.Arg(0))
	if err != nil {
		return err
	}
	fmt.Println(text)
	return nil
}

// decoder transcribes WAVs as dictation does: each one is a take through
// serve's controller (offline), streamed as fast as it is read, kept in a
// temporary store. The controller's log is dropped, so stdout — which the
// menu's checks show — carries the text alone.
type decoder struct {
	r   *offline
	tr  asr.Transcriber
	ui  *flashes
	tmp string
}

func newDecoder(c app.Config, engine string, plain bool) (*decoder, error) {
	if !slices.Contains(app.Engines, engine) {
		return nil, fmt.Errorf("engine %q: want one of %s", engine, strings.Join(app.Engines, ", "))
	}
	log.SetOutput(io.Discard)
	c.ASR.Engine = engine
	tr, pc := c.Transcriber(), c.Post()
	if plain {
		tr, pc = c.PlainTranscriber(), nil
		c.ASR.Doubao.Hotwords = false
	}
	tmp, err := os.MkdirTemp("", "megavoice-transcribe")
	if err != nil {
		return nil, err
	}
	if f, ok := tr.(*app.Fallback); ok && engine == "funasr" {
		f.Warm(tmp) // as serve does at launch
	}
	d := &decoder{r: newOffline(c, tr, pc, filepath.Join(tmp, "takes"), ""), tr: tr, ui: &flashes{}, tmp: tmp}
	d.r.ctrl.UI = d.ui
	return d, nil
}

// text is wav's delivered text; "" when the take holds no speech.
func (d *decoder) text(wav string) (string, error) {
	if _, err := audio.ReadWAV(wav); err != nil {
		return "", err
	}
	d.ui.take()
	text, _, _ := d.r.take(wav, math.Inf(1))
	if msg := d.ui.take(); msg == session.MsgASRFailed || msg == session.MsgIncomplete {
		return "", fmt.Errorf("%s: %s", filepath.Base(wav), msg)
	}
	return text, nil
}

func (d *decoder) close() {
	if f, ok := d.tr.(*app.Fallback); ok {
		f.Stop()
	}
	os.RemoveAll(d.tmp)
}

// flashes keeps the last message the overlay would flash.
type flashes struct {
	mu   sync.Mutex
	last string
}

func (*flashes) Show(session.View) {}

func (f *flashes) Flash(msg string) { f.mu.Lock(); f.last = msg; f.mu.Unlock() }
func (f *flashes) Alert(msg string) { f.Flash(msg) }

// take returns the last message and forgets it.
func (f *flashes) take() string {
	f.mu.Lock()
	defer f.mu.Unlock()
	m := f.last
	f.last = ""
	return m
}

// scriptCheck is the script check: the scripted clips DIR/script.tsv names (their takes in
// DIR, default store.data) transcribed as transcribe does and scored
// against the script.
func scriptCheck(o app.LoadOpts, args []string) error {
	fs := flag.NewFlagSet("script", flag.ContinueOnError)
	plain := fs.Bool("plain", false, "no hotwords, no corrections")
	if err := fs.Parse(args); err != nil || fs.NArg() > 1 {
		return exitCode(2)
	}
	c, err := load(o)
	if err != nil {
		return err
	}
	dir := c.Store.Data
	if fs.NArg() == 1 {
		dir = fs.Arg(0)
	}
	clips, err := score.ScriptClips(dir)
	if err != nil {
		return err
	}
	f := c.ASR.FunASR
	if c.ASR.Engine == "funasr" {
		fmt.Printf("model: %s\n", asr.LLMPath(f.Root, f.LLM))
	} else {
		fmt.Printf("engine: %s\n", c.ASR.Engine)
	}
	d, err := newDecoder(c.Config, c.ASR.Engine, *plain)
	if err != nil {
		return err
	}
	defer d.close()
	fmt.Println("clip\tref_units\terrors\ttext")
	var total, errs int
	for _, clip := range clips {
		text, err := d.text(filepath.Join(dir, clip.Stamp+".wav"))
		if err != nil {
			return err
		}
		ref := score.ScriptUnits(clip.Script)
		d := score.EditDistance(ref, score.ScriptUnits(text))
		total, errs = total+len(ref), errs+d
		fmt.Printf("%s\t%d\t%d\t%s\n", clip.Stamp, len(ref), d, text)
	}
	fmt.Printf("total\t%d\t%d\t%.1f%%\n", total, errs, 100*float64(errs)/float64(total))
	return nil
}

// panel serves the Takes page over DIR's takes (default store.data) and
// their labels, without the agent, which serves its own at compare.addr: a
// replay's takes, say. A resend or a re-transcription needs the agent and
// is refused here.
func panel(o app.LoadOpts, args []string) error {
	fs := flag.NewFlagSet("panel", flag.ContinueOnError)
	data := fs.String("data", "", "the takes (default store.data)")
	labels := fs.String("labels", "", "the label file (default store.labels)")
	addr := fs.String("addr", "127.0.0.1:0", "listen address, on the loopback")
	if err := fs.Parse(args); err != nil || fs.NArg() != 0 {
		return exitCode(2)
	}
	c, err := load(o)
	if err != nil {
		return err
	}
	if *data != "" {
		c.Store.Data = *data
	}
	if *labels != "" {
		c.Store.Labels = *labels
	}
	if h, _, err := net.SplitHostPort(*addr); err != nil || !(h == "localhost" || net.ParseIP(h).IsLoopback()) {
		return fmt.Errorf("-addr %q: want a loopback host:port", *addr)
	}
	ln, err := net.Listen("tcp", *addr)
	if err != nil {
		return err
	}
	key, err := keyFile().Load()
	if err != nil {
		return err
	}
	fmt.Printf("%s  (takes %s, labels %s)\n", takes.URL(ln.Addr().String(), "/takes/", key), c.Store.Data, c.Store.Labels)
	return http.Serve(ln, guard(takes.New(c.Store.Data, c.Labels(), c.TakesEngines(o)), ln))
}
