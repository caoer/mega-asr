package compare

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/caoer/mega-asr/internal/asr"
	"github.com/caoer/mega-asr/internal/audio"
	"github.com/caoer/mega-asr/internal/post"
	"github.com/caoer/mega-asr/internal/session"
	"github.com/caoer/mega-asr/internal/store"
)

// source streams n samples of a live input in 100 ms blocks until stopped;
// with silent, of digital silence.
type source struct {
	n      int
	silent bool
	ch     chan []int16
	o      sync.Once
}

func (s *source) Start(context.Context) (<-chan []int16, error) {
	s.ch = make(chan []int16, s.n/1600+1)
	for i := 0; i < s.n; i += 1600 {
		b := make([]int16, min(1600, s.n-i))
		for j := range b {
			if !s.silent {
				b[j] = int16(j%32 - 16)
			}
		}
		s.ch <- b
	}
	return s.ch, nil
}
func (s *source) Stop()      { s.o.Do(func() { close(s.ch) }) }
func (s *source) Err() error { return nil }

type deliverer struct {
	mu sync.Mutex
	at time.Time
	to []string
}

type tgt struct{}

func (tgt) String() string { return "app" }

func (d *deliverer) Capture() session.Target { return tgt{} }
func (d *deliverer) Deliver(_ session.Target, text string) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.to, d.at = append(d.to, text), time.Now()
	return nil
}
func (d *deliverer) Submit(session.Target) error { return nil }

type ui struct{}

func (ui) Show(session.View) {}
func (ui) Flash(string)      {}
func (ui) Alert(string)      {}

type asrFunc func(ctx context.Context, wav string, whole bool) (string, error)

func (f asrFunc) Transcribe(ctx context.Context, wav string, whole bool) (string, error) {
	return f(ctx, wav, whole)
}

// streamer answers text for every stream, once it was given samples, and
// words as its timing (asr.Timed).
type streamer struct {
	text  string
	words []asr.Word
	mu    sync.Mutex
	got   int
}

type stream struct{ s *streamer }

func (s *streamer) Open() asr.Stream { return stream{s} }
func (s stream) Write(b []int16)     { s.s.mu.Lock(); s.s.got += len(b); s.s.mu.Unlock() }
func (s stream) Abort()              {}
func (s stream) Words() []asr.Word   { return s.s.words }
func (s stream) Finish(context.Context) (string, error) {
	s.s.mu.Lock()
	defer s.s.mu.Unlock()
	if s.s.got == 0 {
		return "", errors.New("no audio")
	}
	return s.s.text, nil
}

// A secondary that hangs and one that fails never hold up the primary's
// delivery; the record is written once every engine has answered, each
// with its text or its error.
func TestFanoutNeverDelaysThePrimary(t *testing.T) {
	dir := t.TempDir()
	release := make(chan struct{})
	slowCalled := make(chan struct{})
	engines := []Engine{
		{Name: "funasr"}, // the primary: skipped as a secondary
		{Name: "slow", File: func(ctx context.Context, wav string) (string, error) {
			close(slowCalled)
			<-release
			return "late  reply", nil
		}},
		{Name: "broken", File: func(context.Context, string) (string, error) { return "", errors.New("engine down") }},
		{Name: "doubao", Stream: &streamer{text: "流式 text", words: []asr.Word{{Text: "流式", StartMS: 200, EndMS: 640}, {Text: "text", StartMS: 700, EndMS: 1100}}}},
	}
	saved := make(chan Record, 1)
	fan := &Fanout{Primary: "funasr", Local: "funasr", Engines: func() []Engine { return engines },
		Post:  post.Chain{func(s string) string { return strings.Join(strings.Fields(s), " ") }},
		Saved: func(r Record) { saved <- r }}
	del := &deliverer{}
	c := &session.Controller{
		NewSource: func() audio.Source { return &source{n: 2 * audio.Rate} },
		ASR:       asrFunc(func(context.Context, string, bool) (string, error) { return "primary text", nil }),
		Deliver:   del, UI: ui{}, Store: store.Store{Dir: dir}, ChunkDir: t.TempDir(),
		WholeMax: time.Minute, Tee: fan.Tee,
	}
	c.Toggle()
	time.Sleep(50 * time.Millisecond)
	c.Toggle()
	done := make(chan struct{})
	go func() { c.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("the take did not deliver while a secondary hangs")
	}
	<-slowCalled
	if strings.Join(del.to, "|") != "primary text" {
		t.Fatalf("delivered %q", del.to)
	}
	select {
	case r := <-saved:
		t.Fatalf("record written before the slow engine answered: %+v", r)
	case <-time.After(100 * time.Millisecond):
	}
	close(release)
	var rec Record
	select {
	case rec = <-saved:
	case <-time.After(2 * time.Second):
		t.Fatal("no record")
	}
	want := map[string]EngineResult{
		"funasr": {Text: "primary text", Raw: "primary text"},
		"slow":   {Text: "late reply", Raw: "late  reply"},
		"broken": {Error: "engine down"},
		"doubao": {Text: "流式 text", Raw: "流式 text"},
	}
	if len(rec.Engines) != len(want) || rec.Primary != "funasr" || rec.DeliveredBy != "funasr" || rec.DurS < 1.9 {
		t.Fatalf("record %+v", rec)
	}
	for name, w := range want {
		g := rec.Engines[name]
		if g.Text != w.Text || g.Raw != w.Raw || g.Error != w.Error || g.LatencyMS < 0 {
			t.Errorf("%s: %+v, want %+v", name, g, w)
		}
	}
	// the timed stream's words are kept with its answer; the others have none
	if w := rec.Engines["doubao"].Words; len(w) != 2 || w[1] != (asr.Word{Text: "text", StartMS: 700, EndMS: 1100}) {
		t.Errorf("doubao's words %+v", w)
	}
	for _, name := range []string{"funasr", "slow", "broken"} {
		if rec.Engines[name].Words != nil {
			t.Errorf("%s has words %+v", name, rec.Engines[name].Words)
		}
	}
	if rec.Engines["slow"].LatencyMS < 100 {
		t.Errorf("slow latency %d ms: want it measured from the stop to its answer", rec.Engines["slow"].LatencyMS)
	}
	onDisk, err := ReadRecord(rec.WAV)
	if err != nil || onDisk == nil || onDisk.Engines["slow"].Text != "late reply" || len(onDisk.Engines["doubao"].Words) != 2 || filepath.Base(rec.WAV) != rec.ID+".wav" {
		t.Fatalf("record on disk %+v, %v", onDisk, err)
	}
	if txt, _ := os.ReadFile(filepath.Join(dir, rec.ID+".txt")); string(txt) != "primary text\n" {
		t.Fatalf("take text %q", txt)
	}
}

// When the primary's stream fails, the local engine's text is what went
// out: the record says so and keeps the primary's error.
func TestFanoutPrimaryStreamFailed(t *testing.T) {
	dir := t.TempDir()
	wav := filepath.Join(dir, "20200314-103000.wav")
	audio.SaveWAV(wav, make([]int16, audio.Rate))
	saved := make(chan Record, 1)
	fan := &Fanout{Primary: "doubao", Local: "funasr", Saved: func(r Record) { saved <- r },
		Engines: func() []Engine {
			return []Engine{{Name: "funasr", File: func(context.Context, string) (string, error) { return "local", nil }}}
		}}
	tee := fan.Tee(strings.TrimSuffix(wav, ".wav"))
	tee.End(time.Now(), wav, audio.Rate)
	tee.Done(session.TakeResult{Raw: "local", Text: "local", Latency: 300 * time.Millisecond, StreamErr: errors.New("doubao: connect: offline")})
	rec := <-saved
	if rec.DeliveredBy != "funasr" || rec.Engines["doubao"].Error != "doubao: connect: offline" || rec.Engines["funasr"].Text != "local" || rec.Engines["funasr"].LatencyMS != 300 {
		t.Fatalf("record %+v", rec)
	}
}

func TestFanoutOff(t *testing.T) {
	for _, es := range [][]Engine{nil, {{Name: "funasr"}}} {
		fan := &Fanout{Primary: "funasr", Engines: func() []Engine { return es }}
		if tee := fan.Tee("/x/20200314-103000"); tee != nil {
			t.Errorf("engines %v: a tee, want none", es)
		}
	}
}

// track streams s in 100 ms blocks until stopped.
type track struct {
	s  []int16
	ch chan []int16
	o  sync.Once
}

func (t *track) Start(context.Context) (<-chan []int16, error) {
	t.ch = make(chan []int16, len(t.s)/1600+1)
	for i := 0; i < len(t.s); i += 1600 {
		t.ch <- t.s[i:min(i+1600, len(t.s))]
	}
	return t.ch, nil
}
func (t *track) Stop()      { t.o.Do(func() { close(t.ch) }) }
func (t *track) Err() error { return nil }

// pop is n samples of zeros after one block of a click as an input opens.
func pop(n int) []int16 {
	s := make([]int16, n)
	for i := range audio.Block {
		s[i] = 1200
	}
	return s
}

// A take whose main track was never heard — digital silence, or a pop as
// its input opened and zeros after it — may be answered with a sentence no
// one said, by a secondary, and by the primary when the take held a pop,
// which is signal and is decoded: the record marks
// the take silent, and no engine's text becomes a label's reference, by a
// click or by typed text that matches it.
func TestSilentTakeOffersNoReference(t *testing.T) {
	const invented = "See you all in the next episode."
	for _, main := range []struct {
		name    string
		src     func() audio.Source
		primary string // digital silence is not decoded; a pop is signal, and it is
	}{
		{"digital silence", func() audio.Source { return &source{n: 2 * audio.Rate, silent: true} }, ""},
		{"a pop, then zeros", func() audio.Source { return &track{s: pop(2 * audio.Rate)} }, invented},
	} {
		t.Run(main.name, func(t *testing.T) { silentTake(t, main.src, invented, main.primary) })
	}
}

func silentTake(t *testing.T, src func() audio.Source, invented, primary string) {
	dir := t.TempDir()
	saved := make(chan Record, 1)
	fan := &Fanout{Primary: "funasr", Local: "funasr", Saved: func(r Record) { saved <- r },
		Engines: func() []Engine {
			return []Engine{{Name: "funasr"}, {Name: "other", File: func(context.Context, string) (string, error) { return invented, nil }}}
		}}
	c := &session.Controller{
		NewSource: src,
		ASR:       asrFunc(func(context.Context, string, bool) (string, error) { return invented, nil }),
		Deliver:   &deliverer{}, UI: ui{}, Store: store.Store{Dir: dir}, ChunkDir: t.TempDir(),
		WholeMax: time.Minute, Tee: fan.Tee,
	}
	c.Toggle()
	time.Sleep(50 * time.Millisecond)
	c.Toggle()
	c.Wait()
	var rec Record
	select {
	case rec = <-saved:
	case <-time.After(2 * time.Second):
		t.Fatal("no record")
	}
	if !rec.Silent || rec.Engines["other"].Text != invented || rec.Engines["funasr"].Text != primary {
		t.Fatalf("record %+v", rec)
	}
	l := &Labels{Path: filepath.Join(t.TempDir(), "labels.jsonl")}
	if _, err := l.Save(dir, Label{ID: rec.ID, Correct: []string{"other"}}); !errors.Is(err, ErrEngine) {
		t.Fatalf("a click on an engine's text: %v, want %v", err, ErrEngine)
	}
	if row, err := l.Save(dir, Label{ID: rec.ID, Ref: invented}); err != nil || row.Correct != nil {
		t.Fatalf("typed text an engine said: %+v, %v", row, err)
	}
}

// voice is n samples of room noise with 300 ms of a voice in every 500 ms.
func voice(n int) []int16 {
	s := make([]int16, n)
	for i := range s {
		a := 1
		if i%(audio.Rate/2) < audio.Rate*3/10 {
			a = 150
		}
		s[i] = int16((i%32 - 16) * a)
	}
	return s
}

// A take whose main track was dead and whose text was decoded from its
// backup track pairs no text with its WAV, which holds none of the words: a
// typed reference, a click on the delivered text, and a click on the
// primary's answer are refused, with a compare record or without one; a
// note alone is saved.
func TestBackupTakeOffersNoReference(t *testing.T) {
	const said = "The copper teapot whistles."
	for _, tee := range []bool{true, false} {
		t.Run(map[bool]string{true: "with a compare record", false: "without one"}[tee], func(t *testing.T) {
			dir := t.TempDir()
			c := &session.Controller{
				NewSource: func() audio.Source { return &track{s: pop(2 * audio.Rate)} },
				NewBackup: func(audio.Source) audio.Source { return &track{s: voice(2 * audio.Rate)} },
				ASR:       asrFunc(func(context.Context, string, bool) (string, error) { return said, nil }),
				Deliver:   &deliverer{}, UI: ui{}, Store: store.Store{Dir: dir}, ChunkDir: t.TempDir(),
				WholeMax: time.Minute,
			}
			saved := make(chan Record, 1)
			if tee {
				fan := &Fanout{Primary: "funasr", Local: "funasr", Saved: func(r Record) { saved <- r },
					Engines: func() []Engine {
						return []Engine{{Name: "funasr"}, {Name: "other", File: func(context.Context, string) (string, error) { return "", nil }}}
					}}
				c.Tee = fan.Tee
			}
			c.Toggle()
			time.Sleep(50 * time.Millisecond)
			c.Toggle()
			c.Wait()
			wavs, _ := filepath.Glob(filepath.Join(dir, "*.wav"))
			if len(wavs) != 1 {
				t.Fatalf("takes %v", wavs)
			}
			id := strings.TrimSuffix(filepath.Base(wavs[0]), ".wav")
			if _, track := store.AudioOf(filepath.Join(dir, id), mustRead(t, filepath.Join(dir, id))); track != "backup" {
				t.Fatalf("text decoded from %q, want backup", track)
			}
			click := Delivered
			if tee {
				select {
				case <-saved:
				case <-time.After(2 * time.Second):
					t.Fatal("no record")
				}
				click = "funasr"
			}
			l := &Labels{Path: filepath.Join(t.TempDir(), "labels.jsonl")}
			if _, err := l.Save(dir, Label{ID: id, Ref: said}); !errors.Is(err, ErrBackup) {
				t.Fatalf("typed reference: %v, want %v", err, ErrBackup)
			}
			if _, err := l.Save(dir, Label{ID: id, Correct: []string{click}}); err == nil {
				t.Fatalf("a click on %s was saved", click)
			}
			if row, err := l.Save(dir, Label{ID: id, Note: "teapot"}); err != nil || row.Ref != "" {
				t.Fatalf("a note: %+v, %v", row, err)
			}
		})
	}
}

func mustRead(t *testing.T, base string) []store.Event {
	t.Helper()
	evs, err := store.Read(base)
	if err != nil {
		t.Fatal(err)
	}
	return evs
}
