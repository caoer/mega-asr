package main

import (
	"bufio"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/caoer/mega-asr/internal/asr"
	"github.com/caoer/mega-asr/internal/audio"
	"github.com/caoer/mega-asr/internal/post"
	"github.com/caoer/mega-asr/internal/session"
)

// hyp transcribes every clip of a manifest the way megavoice does and writes
// the eval contract's hypothesis rows {id, text, latency_ms}.
//
//	asrbench hyp -manifest M.jsonl -out HYP.jsonl [flags]
//
// The defaults are megavoice's own: one resident `llama-funasr-cli --serve`
// (Qwen3 on Metal, encoder and FSMN-VAD on Metal); a clip of 60 s or less is
// one request for the whole take, trimmed by audio.Trim and decoded as one
// window (session.Whole decides, as the controller does); a longer one is cut
// by the live chunker (pause 1.9 s, max 30 s) and the chunk texts joined as
// session.Join does; hotwords for Chinese segments only, then corrections and
// filler cleanup. The flags switch one stage at a time for ablations. latency_ms is
// the ASR compute of the whole clip in the resident process (all chunks, no
// process start, no real-time streaming), not megavoice's stop-to-text.
// -raw-out writes the same run's text before corrections and filler.
// Clips already in -out are skipped, so an interrupted run resumes.
func hyp(args []string) error {
	fs := flag.NewFlagSet("hyp", flag.ContinueOnError)
	manifest := fs.String("manifest", "", "manifest or clip list (jsonl rows with id, wav)")
	out := fs.String("out", "", "hypothesis rows (jsonl, appended)")
	rawOut := fs.String("raw-out", "", "also write the text before corrections and filler")
	mode := fs.String("mode", "chunked", "chunked: megavoice's takes — whole up to -whole-max, else the live chunker, one request per chunk; whole: one request per clip (the CLI's VAD segments it unless -no-vad)")
	wholeMax := fs.Duration("whole-max", time.Minute, "chunked: a clip this long or shorter is one trimmed request decoded as one window (megavoice's take.whole_max); 0 chunks every clip")
	pause := fs.Int("pause", 1900, "chunked: quiet run that ends a chunk, ms")
	maxS := fs.Float64("max", 30, "chunked: longest chunk, s")
	hw := fs.String("hotwords", "cjk", "cjk: Chinese segments only; all: every segment; none")
	hwFile := fs.String("hotwords-file", hotwordsPath(), "hotwords, one per line")
	postOn := fs.Bool("post", true, "corrections and filler cleanup")
	gpu := fs.Int("gpu", 99, "Qwen3 layers on Metal")
	encGPU := fs.Bool("enc-gpu", true, "encoder on Metal")
	vadGPU := fs.Bool("vad-gpu", true, "FSMN-VAD on Metal")
	threads := fs.Int("threads", 0, "CPU encoder threads (0: the CLI's 8)")
	noVAD := fs.Bool("no-vad", false, "no FSMN-VAD: each request is decoded as one window")
	bin := fs.String("bin", "", "llama-funasr-cli (default the repo's)")
	llm := fs.String("llm", "", "Qwen3 GGUF (default models/qwen3-0.6b-q8_0.gguf)")
	enc := fs.String("enc", "", "encoder GGUF (default models/funasr-encoder-f16.gguf)")
	work := fs.String("work", "", "scratch for chunk WAVs and converted audio (default OUT.work)")
	limit := fs.Int("limit", 0, "stop after N clips (0: all)")
	trim := fs.Bool("trim", false, "cut leading and trailing silence off each request's audio (energy: 20 ms blocks under the clip's 10th-percentile level + 8 dB, 200 ms kept either side)")
	if err := fs.Parse(args); err != nil || *manifest == "" || *out == "" {
		return fmt.Errorf("hyp -manifest M.jsonl -out HYP.jsonl [-raw-out F] [-mode chunked|whole] [-whole-max D] [-pause MS] [-max S] [-hotwords cjk|all|none] [-post=false] [-gpu N] [-enc-gpu=false] [-vad-gpu=false] [-threads N] [-no-vad] [-bin P] [-llm P] [-enc P] [-work DIR] [-limit N] [-trim]")
	}
	if *work == "" {
		*work = *out + ".work"
	}
	var rows []manifestRow
	if err := readJSONL(*manifest, func(b []byte) error {
		var m manifestRow
		if err := json.Unmarshal(b, &m); err != nil {
			return err
		}
		rows = append(rows, m)
		return nil
	}); err != nil {
		return err
	}
	done := map[string]bool{}
	if _, err := os.Stat(*out); err == nil {
		prev, err := readHyps(*out)
		if err != nil {
			return err
		}
		for id := range prev {
			done[id] = true
		}
	}
	p := func(rel string) string { return filepath.Join(root(), rel) }
	o := serveOpts{
		bin: or(*bin, p("bin/llama-funasr-cli")), llm: or(*llm, p("models/qwen3-0.6b-q8_0.gguf")),
		enc: or(*enc, p("models/funasr-encoder-f16.gguf")), vad: p("models/fsmn-vad.gguf"),
		gpu: *gpu, encGPU: *encGPU, vadGPU: *vadGPU, hotwordsCJK: *hw == "cjk", threads: *threads,
	}
	if *noVAD {
		o.vad = ""
	}
	var hwLine string
	switch *hw {
	case "cjk", "all":
		hwLine = strings.Join(asr.ReadHotwords(*hwFile), ", ")
	case "none":
	default:
		return fmt.Errorf("-hotwords %q: cjk, all or none", *hw)
	}
	rp, err := startServe(o)
	if err != nil {
		return err
	}
	defer rp.stop()
	rp.hwLine = hwLine
	fmt.Fprintf(os.Stderr, "hyp: resident ready in %.2f s; %d clips, %d already done\n", rp.Ready.Seconds(), len(rows), len(done))
	if err := os.MkdirAll(*work, 0o755); err != nil {
		return err
	}
	w, err := appendFile(*out)
	if err != nil {
		return err
	}
	defer w.Close()
	var rw *os.File
	if *rawOut != "" {
		if rw, err = appendFile(*rawOut); err != nil {
			return err
		}
		defer rw.Close()
	}
	var chain post.Chain
	if *postOn {
		if chain, err = postChain(); err != nil {
			return err
		}
	}
	n := 0
	for _, m := range rows {
		if done[m.ID] {
			continue
		}
		if *limit > 0 && n == *limit {
			break
		}
		n++
		samples, wav, err := loadWAV(m.Wav, *work)
		if err != nil {
			return fmt.Errorf("%s: %w", m.ID, err)
		}
		var parts []string
		var spent time.Duration
		switch {
		case *mode == "whole":
			if *trim {
				wav = filepath.Join(*work, "trimmed.wav")
				if err := audio.SaveWAV(wav, audio.Trim(samples)); err != nil {
					return err
				}
			}
			txt, d, err := rp.transcribe(wav, false)
			if err != nil {
				return fmt.Errorf("%s: %w", m.ID, err)
			}
			parts, spent = []string{txt}, d
		case session.Whole(len(samples), *wholeMax):
			wav = filepath.Join(*work, "trimmed.wav")
			if err := audio.SaveWAV(wav, audio.Trim(samples)); err != nil {
				return err
			}
			txt, d, err := rp.transcribe(wav, true)
			if err != nil {
				return fmt.Errorf("%s: %w", m.ID, err)
			}
			parts, spent = []string{txt}, d
		default:
			ch := audio.Chunker{Pause: time.Duration(*pause) * time.Millisecond, Max: time.Duration(*maxS * float64(time.Second)), Min: 3 * time.Second, Margin: 8}
			for i, g := range segments(samples, 0, ch) {
				cw := filepath.Join(*work, fmt.Sprintf("chunk-%03d.wav", i))
				part := samples[g.Start:g.End]
				if *trim {
					part = audio.Trim(part)
				}
				if err := audio.SaveWAV(cw, part); err != nil {
					return err
				}
				txt, d, err := rp.transcribe(cw, false)
				if err != nil {
					return fmt.Errorf("%s chunk %d: %w", m.ID, i, err)
				}
				parts, spent = append(parts, txt), spent+d
			}
		}
		raw := session.Join(parts)
		text := raw
		if *postOn {
			text = chain.Apply(raw)
		}
		if err := writeHyp(w, m.ID, text, spent); err != nil {
			return err
		}
		if rw != nil {
			if err := writeHyp(rw, m.ID, raw, spent); err != nil {
				return err
			}
		}
		fmt.Fprintf(os.Stderr, "%s\t%.1fs\t%.2fs\t%s\n", m.ID, float64(len(samples))/audio.Rate, spent.Seconds(), text)
	}
	return nil
}

func or(s, def string) string {
	if s == "" {
		return def
	}
	return s
}

func appendFile(path string) (*os.File, error) {
	return os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
}

func writeHyp(f *os.File, id, text string, d time.Duration) error {
	b, err := json.Marshal(hypRow{ID: id, Text: text, LatencyMS: float64(d.Milliseconds())})
	if err != nil {
		return err
	}
	_, err = f.Write(append(b, '\n'))
	return err
}

// loadWAV reads a 16 kHz mono 16-bit WAV. A file with another header layout
// (some recorders' WAVs carry JUNK and FLLR chunks) or format is converted once with
// ffmpeg into dir, and the converted path is returned for the CLI.
func loadWAV(path, dir string) ([]int16, string, error) {
	if s, err := audio.ReadWAV(path); err == nil {
		return s, path, nil
	}
	sum := sha256.Sum256([]byte(path))
	conv := filepath.Join(dir, "conv-"+hex.EncodeToString(sum[:8])+".wav")
	if _, err := os.Stat(conv); err != nil {
		out, err := exec.Command("ffmpeg", "-nostdin", "-loglevel", "error", "-y", "-i", path, "-ar", "16000", "-ac", "1", "-c:a", "pcm_s16le", "-map_metadata", "-1", "-fflags", "+bitexact", "-flags:a", "+bitexact", conv).CombinedOutput()
		if err != nil {
			return nil, "", fmt.Errorf("ffmpeg %s: %v: %s", path, err, out)
		}
	}
	s, err := audio.ReadWAV(conv)
	return s, conv, err
}

// serveOpts is one `llama-funasr-cli --serve` configuration.
type serveOpts struct {
	bin, llm, enc, vad string // vad "" runs without FSMN-VAD
	gpu, threads       int
	encGPU, vadGPU     bool
	hotwordsCJK        bool
}

func (o serveOpts) args() []string {
	a := []string{"--enc", o.enc, "-m", o.llm, "--serve"}
	if o.vad != "" {
		a = append(a, "--vad", o.vad)
		if o.vadGPU {
			a = append(a, "--vad-gpu")
		}
	}
	if o.hotwordsCJK {
		a = append(a, "--hotwords-cjk")
	}
	if o.gpu > 0 {
		a = append(a, "--gpu", fmt.Sprint(o.gpu))
	}
	if o.encGPU {
		a = append(a, "--enc-gpu")
	}
	if o.threads > 0 {
		a = append(a, "--threads", fmt.Sprint(o.threads))
	}
	return a
}

func startServe(o serveOpts) (*resident, error) {
	r := &resident{Bin: o.bin, GPU: o.gpu}
	r.cmd = exec.Command(o.bin, o.args()...)
	var err error
	if r.in, err = r.cmd.StdinPipe(); err != nil {
		return nil, err
	}
	stdout, err := r.cmd.StdoutPipe()
	if err != nil {
		return nil, err
	}
	stderr, err := r.cmd.StderrPipe()
	if err != nil {
		return nil, err
	}
	r.out, r.errs = bufio.NewReader(stdout), bufio.NewScanner(stderr)
	t0 := time.Now()
	if err := r.cmd.Start(); err != nil {
		return nil, err
	}
	var tail []string
	for r.errs.Scan() {
		l := r.errs.Text()
		if strings.HasPrefix(l, "[ready]") {
			r.Ready = time.Since(t0)
			go func() { // drain stderr so the process never blocks on it
				for r.errs.Scan() {
				}
			}()
			return r, nil
		}
		if tail = append(tail, l); len(tail) > 5 {
			tail = tail[1:]
		}
	}
	return nil, fmt.Errorf("%s exited before [ready]: %s", o.bin, strings.Join(tail, " | "))
}
