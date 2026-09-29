package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/caoer/mega-asr/internal/app"
	"github.com/caoer/mega-asr/internal/asr"
	"github.com/caoer/mega-asr/internal/audio"
	"github.com/caoer/mega-asr/internal/post"
	"github.com/caoer/mega-asr/internal/session"
	"github.com/caoer/mega-asr/internal/store"
)

// replay runs the controller without the app: each WAV streams at real time
// as if spoken, the take stops where the file ends, and its text is printed
// — with -out also written to OUT/<name>.txt — instead of pasted; with
// -chunks each take's chunks go to CHUNKS/<name>/ (writeChunks). It first
// recovers what a killed replay left in -data.
func replay(o app.LoadOpts, args []string) error {
	fs := flag.NewFlagSet("replay", flag.ContinueOnError)
	data := fs.String("data", filepath.Join(stateDir(), "replay"), "where the takes are kept")
	out := fs.String("out", "", "also write each text to OUT/<name>.txt")
	chunks := fs.String("chunks", "", "also write each take's chunk WAVs, edges and texts to CHUNKS/<name>/")
	if err := fs.Parse(args); err != nil {
		return exitCode(2)
	}
	log.SetFlags(log.LstdFlags | log.Lmicroseconds)
	c, err := load(o)
	if err != nil {
		return err
	}
	if *out != "" {
		if err := os.MkdirAll(*out, 0o755); err != nil {
			return err
		}
	}
	tr := c.Transcriber()
	if f, ok := tr.(*app.Fallback); ok {
		f.Warm(os.TempDir()) // as serve does at launch
	}
	fan := c.Fanout(o, tr)
	r := newOffline(c.Config, tr, c.Post(), *data, *out)
	r.ctrl.Tee = fan.Tee
	r.ctrl.Recover()
	r.ctrl.Wait()
	if *chunks != "" {
		r.keepChunks(*chunks)
	}
	fmt.Println("take\taudio_s\tstop_to_text_s\tload\ttext")
	for _, w := range fs.Args() {
		text, at, src := r.take(w, 0)
		if r.chunkErr != nil {
			return r.chunkErr
		}
		after := "-"
		if !at.IsZero() {
			after = fmt.Sprintf("%.2f", at.Sub(src.end()).Seconds())
		}
		fmt.Printf("%s\t%.1f\t%s\t%s\t%s\n", strings.TrimSuffix(filepath.Base(w), ".wav"), float64(src.n())/audio.Rate, after, loadavg(), text)
	}
	fan.Wait() // compare records still being written
	return nil
}

// offline is serve's controller without the app — the same chunk ASR,
// chunker, whole-take rule and post chain — fed from WAV files, with each
// take's text handed back instead of pasted. replay, transcribe and the script
// check all decode through it, so none of them can drift from dictation.
type offline struct {
	ctrl     *session.Controller
	d        *replayDeliverer
	src      *timedFile
	chunkErr error // keepChunks' first failure
}

// newOffline builds the controller over tr and post, keeping its takes in
// data; with out set, each text is also written to OUT/<name>.txt.
func newOffline(c app.Config, tr asr.Transcriber, post post.Chain, data, out string) *offline {
	r := &offline{d: &replayDeliverer{out: out}}
	r.ctrl = &session.Controller{
		NewSource:  func() audio.Source { return r.src },
		NewChunker: c.Chunker,
		WholeMax:   time.Duration(c.Take.WholeMax),
		ASR:        tr,
		Stream:     c.Streamer(),
		Post:       post,
		Deliver:    r.d,
		UI:         logUI{},
		Store:      store.Store{Dir: data},
		ChunkDir:   data + "-chunks",
	}
	return r
}

// keepChunks has each take's chunks written to dir/<name>/ (writeChunks) as
// the take finishes; the first failure is kept in chunkErr.
func (r *offline) keepChunks(dir string) {
	r.ctrl.OnChunks = func(base string, cs []session.Chunk) {
		if err := writeChunks(filepath.Join(dir, r.d.current()), base, cs); err != nil && r.chunkErr == nil {
			r.chunkErr = err
		}
	}
}

// take runs wav as one take — started, streamed at speed (0 is real time,
// math.Inf(1) as fast as the controller reads), stopped where the file ends
// — and returns its delivered text and when it was delivered.
func (r *offline) take(wav string, speed float64) (string, time.Time, *timedFile) {
	r.src = &timedFile{File: audio.File{Path: wav, Speed: speed}}
	r.d.begin(strings.TrimSuffix(filepath.Base(wav), ".wav"))
	r.ctrl.ToggleFrom("replay")
	r.ctrl.Wait()
	text, at := r.d.result()
	return text, at, r.src
}

// timedFile is a replay Source that notes when its stream ended.
type timedFile struct {
	audio.File
	mu      sync.Mutex
	samples int
	ended   time.Time
}

func (t *timedFile) Start(ctx context.Context) (<-chan []int16, error) {
	in, err := t.File.Start(ctx)
	if err != nil {
		return nil, err
	}
	out := make(chan []int16, 16)
	go func() {
		defer close(out)
		for s := range in {
			t.mu.Lock()
			t.samples += len(s)
			t.mu.Unlock()
			out <- s
		}
		t.mu.Lock()
		t.ended = time.Now()
		t.mu.Unlock()
	}()
	return out, nil
}

func (t *timedFile) n() int         { t.mu.Lock(); defer t.mu.Unlock(); return t.samples }
func (t *timedFile) end() time.Time { t.mu.Lock(); defer t.mu.Unlock(); return t.ended }

// replayDeliverer takes the text instead of pasting it.
type replayDeliverer struct {
	out  string
	mu   sync.Mutex
	name string
	text string
	at   time.Time
}

type replayTarget string

func (t replayTarget) String() string { return "replay " + string(t) }

func (d *replayDeliverer) begin(name string) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.name, d.text, d.at = name, "", time.Time{}
}

// current is the name of the take on its way.
func (d *replayDeliverer) current() string {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.name
}

func (d *replayDeliverer) result() (string, time.Time) {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.text, d.at
}

func (d *replayDeliverer) Capture() session.Target {
	d.mu.Lock()
	defer d.mu.Unlock()
	return replayTarget(d.name)
}

func (d *replayDeliverer) Deliver(t session.Target, text string) error {
	d.mu.Lock()
	d.text, d.at = text, time.Now()
	d.mu.Unlock()
	if d.out == "" {
		return nil
	}
	return os.WriteFile(filepath.Join(d.out, string(t.(replayTarget))+".txt"), []byte(text+"\n"), 0o644)
}

func (d *replayDeliverer) Submit(session.Target) error { return nil }

// writeChunks writes a finished take's chunks to dir: chunk-NNN.wav, the
// take's samples [start, end) as the controller queued them, and chunks.tsv,
// one row per chunk: the columns of asrbench split's cuts.tsv up to
// end_reason, then the ASR's text and the post chain's (tabs and line breaks
// as spaces; both empty when the ASR failed on the chunk). A whole take is
// one chunk, its trim bounds, both edges "whole".
func writeChunks(dir, base string, cs []session.Chunk) error {
	s, err := audio.ReadWAV(base + ".wav")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	var b strings.Builder
	b.WriteString("chunk\tstart\tend\tstart_s\tend_s\tlen_s\tstart_reason\tend_reason\traw\ttext\n")
	field := strings.NewReplacer("\t", " ", "\n", " ", "\r", " ")
	for i, c := range cs {
		name := fmt.Sprintf("chunk-%03d.wav", i+1)
		if err := audio.SaveWAV(filepath.Join(dir, name), s[c.Start:c.End]); err != nil {
			return err
		}
		fmt.Fprintf(&b, "%s\t%d\t%d\t%.2f\t%.2f\t%.2f\t%s\t%s\t%s\t%s\n", name, c.Start, c.End, float64(c.Start)/audio.Rate,
			float64(c.End)/audio.Rate, float64(c.End-c.Start)/audio.Rate, c.From, c.To, field.Replace(c.Raw), field.Replace(c.Text))
	}
	return os.WriteFile(filepath.Join(dir, "chunks.tsv"), []byte(b.String()), 0o644)
}

// logUI logs what the overlay would flash.
type logUI struct{}

func (logUI) Show(session.View) {}
func (logUI) Flash(msg string)  { log.Printf("flash: %s", msg) }
func (logUI) Alert(msg string)  { log.Printf("alert: %s", msg) }

// loadavg is the 1-minute load average, so a row carries the machine's state
// when it was measured.
func loadavg() string {
	out, err := exec.Command("sysctl", "-n", "vm.loadavg").Output()
	if err != nil {
		return "?"
	}
	if f := strings.Fields(strings.Trim(strings.TrimSpace(string(out)), "{}")); len(f) > 0 {
		return f[0]
	}
	return "?"
}
