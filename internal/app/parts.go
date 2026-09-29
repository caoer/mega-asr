package app

import (
	"context"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/caoer/mega-asr/internal/asr"
	"github.com/caoer/mega-asr/internal/audio"
	"github.com/caoer/mega-asr/internal/compare"
	"github.com/caoer/mega-asr/internal/post"
	"github.com/caoer/mega-asr/internal/session"
	"github.com/caoer/mega-asr/internal/takes"
)

// Chunker cuts a take at pauses as it records.
func (c Config) Chunker() session.Chunker {
	return &audio.Chunker{Pause: time.Duration(c.Take.ChunkPause), Max: time.Duration(c.Take.ChunkMax)}
}

// Transcriber is what a controller transcribes chunks with: the resident
// process with the CLI as fallback, or, in exec mode, the CLI alone. The
// hotword list is read at every chunk, so edits take effect at the next one.
func (c Config) Transcriber() asr.Transcriber {
	if c.ASR.FunASR.Mode == "exec" {
		return c.Exec()
	}
	return c.Resident()
}

// PlainTranscriber is Transcriber without the hotwords.
func (c Config) PlainTranscriber() asr.Transcriber {
	if c.ASR.FunASR.Mode == "exec" {
		return c.exec(nil)
	}
	return c.resident(nil)
}

func (c Config) Resident() *Fallback { return c.resident(hotwords) }

func (c Config) resident(hw func() []string) *Fallback {
	f := c.ASR.FunASR
	return &Fallback{
		new: func() *asr.Resident {
			return &asr.Resident{Root: f.Root, LLM: f.LLM, Hotwords: hw, GPU: f.GPULayers, EncGPU: f.Encoder == "gpu", VADGPU: f.VAD == "gpu", Threads: f.Threads, LLMThreads: f.LLMThreads}
		},
		exec: c.exec(hw),
	}
}

func hotwords() []string { return asr.ReadHotwords(HotwordsPath()) }

// Exec runs the CLI once per clip, with the resident process's engine flags.
func (c Config) Exec() asr.FunASR { return c.exec(hotwords) }

func (c Config) exec(hw func() []string) asr.FunASR {
	f := c.ASR.FunASR
	return asr.FunASR{Root: f.Root, LLM: f.LLM, Hotwords: hw, GPU: f.GPULayers, EncGPU: f.Encoder == "gpu", VADGPU: f.VAD == "gpu", Threads: f.Threads, LLMThreads: f.LLMThreads}
}

// Post is the chain applied after ASR: the correction table, then filler
// removal with post.join_words.
func (c Config) Post() post.Chain {
	return post.Chain{post.CorrectionsFile(CorrectionsPath()), post.Filler(c.Postprocess.JoinWords)}
}

// Doubao is the Doubao ASR 2.0 client; its key is read from key_file at
// each take.
func (c Config) Doubao() *asr.Doubao {
	d := c.ASR.Doubao
	a := &asr.Doubao{URL: d.URL, ResourceID: d.ResourceID, Key: d.Key, TwoPass: d.TwoPass, DDC: d.DDC}
	if d.Hotwords {
		a.Hotwords = hotwords
	}
	return a
}

// Streamer is the streaming primary for a controller: Doubao when it is
// asr.engine, else nil (the chunk ASR is the primary).
func (c Config) Streamer() asr.Streamer {
	if c.ASR.Engine == "doubao" {
		return c.Doubao()
	}
	return nil
}

// TranscribeFile transcribes a kept take with tr as the controller would
// have: trimmed and as one window within take.whole_max, else whole file
// with the VAD.
func (c Config) TranscribeFile(ctx context.Context, tr asr.Transcriber, wav string) (string, error) {
	whole := false
	if s, err := audio.ReadWAV(wav); err == nil && session.Whole(len(s), time.Duration(c.Take.WholeMax)) {
		dir, err := os.MkdirTemp("", "megavoice-transcribe")
		if err != nil {
			return "", err
		}
		defer os.RemoveAll(dir)
		wav, whole = filepath.Join(dir, "trimmed.wav"), true
		if err := audio.SaveWAV(wav, audio.Trim(s)); err != nil {
			return "", err
		}
	}
	return tr.Transcribe(ctx, wav, whole)
}

// CompareEngines are the engines compare mode can run, built from c: funasr
// transcribes the take's WAV with local — the controller's own chunk ASR,
// so its resident process is shared — and doubao streams the take.
func (c Config) CompareEngines(local asr.Transcriber) map[string]compare.Engine {
	return map[string]compare.Engine{
		"funasr": {Name: "funasr", File: func(ctx context.Context, wav string) (string, error) { return c.TranscribeFile(ctx, local, wav) }},
		"doubao": {Name: "doubao", Stream: c.Doubao()},
	}
}

// Retranscribers are the engines a kept take can be decoded again by, for
// session.Controller.Retranscribe: funasr is the controller's own chunk ASR,
// taken through its queue, and doubao, a cloud engine, is Named only when
// this config runs it (Uses).
func (c Config) Retranscribers() []session.Engine {
	f := c.ASR.FunASR
	return []session.Engine{
		{Name: "funasr", Model: filepath.Base(asr.LLMPath(f.Root, f.LLM)), Named: true},
		{Name: "doubao", Model: c.ASR.Doubao.ResourceID, Cloud: true, Named: c.Uses("doubao"),
			File: func(ctx context.Context, wav string) (string, error) { return c.Doubao().Transcribe(ctx, wav, false) }},
	}
}

// Fanout is compare mode for a controller whose chunk ASR is local. Which
// engines run is read from the config file (o) as each take starts, so the
// menu's toggle and `config set compare.on` apply at the next take; how
// each engine runs is c's.
func (c Config) Fanout(o LoadOpts, local asr.Transcriber) *compare.Fanout {
	engines := c.CompareEngines(local)
	var mu sync.Mutex
	last := c.Compare
	return &compare.Fanout{
		Primary: c.ASR.Engine, Local: "funasr", Post: c.Post(),
		Engines: func() []compare.Engine {
			mu.Lock()
			defer mu.Unlock()
			o.Check = nil
			if l, err := Load(o); err != nil {
				log.Printf("compare: %v; keeping compare.on = %v", err, last.On)
			} else {
				last = l.Compare
			}
			if !last.On {
				return nil
			}
			var out []compare.Engine
			for _, name := range last.Engines {
				out = append(out, engines[name])
			}
			return out
		},
	}
}

// cloud names the service a cloud engine's audio goes to.
var cloud = map[string]string{"doubao": "火山引擎"}

// current is the config file (o) as it reads now, c when it no longer
// loads. Retranscriber and TakesEngines read it alike, so the page offers an
// engine exactly when the function accepts it.
func (c Config) current(o LoadOpts) Config {
	o.Check = nil
	if l, err := Load(o); err == nil {
		return l.Config
	}
	return c
}

// Retranscriber finds an engine by name among Retranscribers of the config
// file (o) as it reads at each call: a cloud engine is Named only while the
// config runs it.
func (c Config) Retranscriber(o LoadOpts) func(name string) (session.Engine, error) {
	return func(name string) (session.Engine, error) {
		var names []string
		for _, e := range c.current(o).Retranscribers() {
			if e.Name == name {
				return e, nil
			}
			names = append(names, e.Name)
		}
		return session.Engine{}, fmt.Errorf("engine %q: want one of %s", name, strings.Join(names, ", "))
	}
}

// TakesEngines lists the engines for the Takes page, from Retranscribers of
// the config file (o) read at each call, as Retranscriber reads it.
func (c Config) TakesEngines(o LoadOpts) func() []takes.Engine {
	return func() []takes.Engine {
		cur := c.current(o)
		var out []takes.Engine
		for _, e := range cur.Retranscribers() {
			out = append(out, takes.Engine{Name: e.Name, Local: !e.Cloud, Service: cloud[e.Name],
				Primary: cur.ASR.Engine == e.Name, Named: !e.Cloud || e.Named})
		}
		return out
	}
}

// Labels is the label file, store.labels, which the Takes page reads and
// writes.
func (c Config) Labels() *compare.Labels { return &compare.Labels{Path: c.Store.Labels} }
