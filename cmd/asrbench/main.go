// asrbench measures the megavoice ASR path on kept utterances, without the
// array or the app: how long llama-funasr-cli takes and how much memory it
// uses per clip length, what an energy-pause chunking of a long take does to
// the text, the script-check and filler scores, and the level statistics an overlay
// could show. It reads MEGAVOICE_MEGA_ASR for bin/ and models/ like megavoice,
// the working directory when it is unset.
//
//	asrbench concat OUT.wav IN.wav...           join 16 kHz mono clips
//	asrbench time [-n N] [-plain] [-tag T] [-flags F] WAV...
//	                                            run the CLI, print wall/user time, peak RSS, text
//	asrbench split [-pause MS] [-max S] [-min S] [-margin DB] [-whole D] OUTDIR WAV
//	                                            cut at energy pauses, or keep a take ≤ D whole and trimmed; chunk WAVs + cuts.tsv
//	asrbench levels WAV...                      20 ms block RMS percentiles, noise floor, clipping
//	asrbench cer REF.txt HYP.txt                edit distance over CJK chars + Latin words
//	asrbench script [-plain] [-texts T] DIR     the scripted clips DIR/script.tsv names scored against their scripts
//	asrbench stitch [-plain] [-resident] DIR    transcribe a split directory's chunks, join, DIR/stitched.txt
//	asrbench score -manifest M.jsonl [-terms F] [-rows OUT.tsv] HYP.jsonl...
//	                                            CER/WER/MER per zh/en/mixed bucket, punctuation, named terms (score.go)
//	asrbench hyp -manifest M.jsonl -out HYP.jsonl [flags]
//	                                            megavoice's pipeline over a manifest, one stage switchable per flag (hyp.go)
//	asrbench version                            the chunker version (audio.ChunkerVersion) and this build's commit
//
// -resident runs one `llama-funasr-cli --serve` (runtime/funasr-cli-serve.patch)
// for all clips; -gpu N passes --gpu N to it; -bin names another binary.
package main

import (
	"bufio"
	"bytes"
	"context"
	_ "embed"
	"flag"
	"fmt"
	"io"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"syscall"
	"time"
	"unicode"

	"github.com/caoer/mega-asr/internal/app"
	"github.com/caoer/mega-asr/internal/asr"
	"github.com/caoer/mega-asr/internal/audio"
	"github.com/caoer/mega-asr/internal/post"
	"github.com/caoer/mega-asr/internal/score"
	"github.com/caoer/mega-asr/internal/session"
)

func main() {
	if len(os.Args) < 2 {
		fmt.Fprintln(os.Stderr, "usage: asrbench concat|time|split|levels|cer|script|stitch|score|hyp|version ...")
		os.Exit(2)
	}
	var err error
	switch os.Args[1] {
	case "concat":
		err = concat(os.Args[2:])
	case "time":
		err = timeCmd(os.Args[2:])
	case "split":
		err = split(os.Args[2:])
	case "levels":
		err = levels(os.Args[2:])
	case "cer":
		err = cerCmd(os.Args[2:])
	case "script":
		err = scriptCheck(os.Args[2:])
	case "stitch":
		err = stitch(os.Args[2:])
	case "score":
		err = scoreCmd(os.Args[2:])
	case "hyp":
		err = hyp(os.Args[2:])
	case "version":
		fmt.Print(audio.Version())
	default:
		err = fmt.Errorf("unknown command %q", os.Args[1])
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "asrbench:", err)
		os.Exit(1)
	}
}

func root() string {
	if r := os.Getenv("MEGAVOICE_MEGA_ASR"); r != "" {
		return r
	}
	return "."
}

func hotwordsPath() string {
	if d := os.Getenv("XDG_CONFIG_HOME"); d != "" {
		return filepath.Join(d, "megavoice", "hotwords.txt")
	}
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".config", "megavoice", "hotwords.txt")
}

// postChain is megavoice's post chain, built from its config file
// (MEGAVOICE_CONFIG, else config.toml beside hotwords.txt), so asrbench
// gives megavoice's text for the same config.
func postChain() (post.Chain, error) {
	l, err := app.Load(app.LoadOpts{})
	if err != nil {
		return nil, err
	}
	return l.Post(), nil
}

func concat(args []string) error {
	if len(args) < 2 {
		return fmt.Errorf("concat OUT.wav IN.wav...")
	}
	var all []int16
	for _, in := range args[1:] {
		s, err := audio.ReadWAV(in)
		if err != nil {
			return err
		}
		all = append(all, s...)
	}
	if err := audio.SaveWAV(args[0], all); err != nil {
		return err
	}
	fmt.Printf("%s: %.1f s from %d clips\n", args[0], float64(len(all))/audio.Rate, len(args)-1)
	return nil
}

// cliFlags are extra llama-funasr-cli flags (time -flags), e.g. thread counts.
var cliFlags []string

// run is one CLI invocation: the same arguments megavoice passes, plus the
// child's rusage.
type run struct {
	Wall, User, Sys time.Duration
	MaxRSS          int64 // bytes
	Segments        int
	Done            string // the CLI's own [done] line
	Text            string
	Err             error
}

func runCLI(wav string, plain bool) run {
	p := func(rel string) string { return filepath.Join(root(), rel) }
	args := []string{"--enc", p("models/funasr-encoder-f16.gguf"), "-m", p("models/qwen3-0.6b-q8_0.gguf"), "--vad", p("models/fsmn-vad.gguf")}
	if !plain {
		if hw := asr.ReadHotwords(hotwordsPath()); len(hw) > 0 {
			args = append(args, "--hotwords", strings.Join(hw, ", "), "--hotwords-cjk")
		}
	}
	args = append(args, cliFlags...)
	cmd := exec.CommandContext(context.Background(), p("bin/llama-funasr-cli"), append(args, "-a", wav)...)
	var out, errb bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errb
	t0 := time.Now()
	err := cmd.Run()
	r := run{Wall: time.Since(t0), Err: err}
	if cmd.ProcessState != nil {
		r.User, r.Sys = cmd.ProcessState.UserTime(), cmd.ProcessState.SystemTime()
		if ru, ok := cmd.ProcessState.SysUsage().(*syscall.Rusage); ok {
			r.MaxRSS = int64(ru.Maxrss)
			if runtime.GOOS == "linux" { // kilobytes there, bytes on macOS
				r.MaxRSS *= 1024
			}
		}
	}
	for _, l := range strings.Split(errb.String(), "\n") {
		if strings.HasPrefix(l, "[vad] ") {
			fmt.Sscanf(l, "[vad] %d segments", &r.Segments)
		}
		if strings.HasPrefix(l, "[done] ") {
			r.Done = l
		}
	}
	if err != nil {
		r.Err = fmt.Errorf("%v: %s", err, lastLine(errb.String()))
	}
	r.Text = asr.Clean(out.String())
	return r
}

// loadavg is the 1-minute load average, so a row carries the machine's state
// when it was measured.
func loadavg() string {
	if b, err := os.ReadFile("/proc/loadavg"); err == nil { // Linux
		if f := strings.Fields(string(b)); len(f) > 0 {
			return f[0]
		}
	}
	out, err := exec.Command("sysctl", "-n", "vm.loadavg").Output()
	if err != nil {
		return "?"
	}
	f := strings.Fields(strings.Trim(strings.TrimSpace(string(out)), "{}"))
	if len(f) > 0 {
		return f[0]
	}
	return "?"
}

func lastLine(s string) string {
	s = strings.TrimSpace(s)
	if i := strings.LastIndexByte(s, '\n'); i >= 0 {
		return s[i+1:]
	}
	return s
}

func dur(path string) float64 {
	st, err := os.Stat(path)
	if err != nil {
		return 0
	}
	return float64(st.Size()-44) / (2 * audio.Rate)
}

// resident is a client of `llama-funasr-cli --serve` (runtime/funasr-cli-serve.patch):
// one request line "wav\thotwords" per clip ("wav\thotwords\twhole" to decode
// it as one window without the VAD), one reply line.
type resident struct {
	cmd    *exec.Cmd
	in     io.WriteCloser
	out    *bufio.Reader
	errs   *bufio.Scanner
	Ready  time.Duration
	Bin    string
	GPU    int
	hwLine string
}

func startResident(plain bool, gpu int, bin string) (*resident, error) {
	p := func(rel string) string { return filepath.Join(root(), rel) }
	if bin == "" {
		bin = p("bin/llama-funasr-cli")
	}
	args := []string{"--enc", p("models/funasr-encoder-f16.gguf"), "-m", p("models/qwen3-0.6b-q8_0.gguf"), "--vad", p("models/fsmn-vad.gguf"), "--serve"}
	r := &resident{Bin: bin, GPU: gpu}
	if !plain {
		if hw := asr.ReadHotwords(hotwordsPath()); len(hw) > 0 {
			r.hwLine = strings.Join(hw, ", ")
			args = append(args, "--hotwords-cjk")
		}
	}
	if gpu > 0 {
		args = append(args, "--gpu", fmt.Sprint(gpu))
	}
	args = append(args, cliFlags...)
	r.cmd = exec.Command(bin, args...)
	var err error
	if r.in, err = r.cmd.StdinPipe(); err != nil {
		return nil, err
	}
	out, err := r.cmd.StdoutPipe()
	if err != nil {
		return nil, err
	}
	errp, err := r.cmd.StderrPipe()
	if err != nil {
		return nil, err
	}
	r.out, r.errs = bufio.NewReader(out), bufio.NewScanner(errp)
	t0 := time.Now()
	if err := r.cmd.Start(); err != nil {
		return nil, err
	}
	for r.errs.Scan() {
		if strings.HasPrefix(r.errs.Text(), "[ready]") {
			r.Ready = time.Since(t0)
			go func() { // drain stderr so the process never blocks on it
				for r.errs.Scan() {
					if l := r.errs.Text(); strings.HasPrefix(l, "[vad]") || strings.HasPrefix(l, "[done]") {
						fmt.Fprintln(os.Stderr, l)
					}
				}
			}()
			return r, nil
		}
	}
	return nil, fmt.Errorf("%s exited before [ready]", bin)
}

func (r *resident) transcribe(wav string, whole bool) (string, time.Duration, error) {
	t0 := time.Now()
	req := wav + "\t" + r.hwLine
	if whole {
		req += "\twhole"
	}
	if _, err := fmt.Fprintln(r.in, req); err != nil {
		return "", 0, err
	}
	line, err := r.out.ReadString('\n')
	if err != nil {
		return "", 0, fmt.Errorf("resident: %w", err)
	}
	return asr.Clean(line), time.Since(t0), nil
}

func (r *resident) rss() int64 {
	out, err := exec.Command("ps", "-o", "rss=", "-p", fmt.Sprint(r.cmd.Process.Pid)).Output()
	if err != nil {
		return 0
	}
	var kb int64
	fmt.Sscan(strings.TrimSpace(string(out)), &kb)
	return kb * 1024
}

func (r *resident) stop() {
	r.in.Close()
	r.cmd.Wait()
}

func timeCmd(args []string) error {
	fs := flag.NewFlagSet("time", flag.ContinueOnError)
	n := fs.Int("n", 1, "runs per clip")
	plain := fs.Bool("plain", false, "no hotwords")
	tag := fs.String("tag", "", "label in the output")
	text := fs.Bool("text", false, "print the transcript too")
	res := fs.Bool("resident", false, "one --serve process for all runs")
	gpu := fs.Int("gpu", 0, "resident: --gpu layers")
	bin := fs.String("bin", "", "resident: the llama-funasr-cli to run (default the repo's)")
	flags := fs.String("flags", "", `extra llama-funasr-cli flags, e.g. "--threads 16 --llm-threads 16"`)
	if err := fs.Parse(args); err != nil || fs.NArg() == 0 {
		return fmt.Errorf("time [-n N] [-plain] [-tag T] [-text] [-flags F] [-resident [-gpu N] [-bin PATH]] WAV...")
	}
	cliFlags = strings.Fields(*flags)
	fmt.Println("tag\tclip\taudio_s\trun\tsegments\twall_s\tuser_s\tmax_rss_mb\tcli_done")
	var rp *resident
	if *res {
		var err error
		if rp, err = startResident(*plain, *gpu, *bin); err != nil {
			return err
		}
		defer rp.stop()
		fmt.Printf("%s\t(ready)\t0\t0\t0\t%.2f\t0\t%.0f\tresident start\n", *tag, rp.Ready.Seconds(), float64(rp.rss())/1e6)
	}
	for _, wav := range fs.Args() {
		for i := 1; i <= *n; i++ {
			var r run
			if rp != nil {
				txt, d, err := rp.transcribe(wav, false)
				if err != nil {
					return fmt.Errorf("%s: %w", wav, err)
				}
				r = run{Wall: d, Text: txt, MaxRSS: rp.rss(), Done: "[done] resident"}
			} else {
				r = runCLI(wav, *plain)
			}
			if r.Err != nil {
				return fmt.Errorf("%s: %w", wav, r.Err)
			}
			fmt.Printf("%s\t%s\t%.1f\t%d\t%d\t%.2f\t%.2f\t%.0f\t%s\tload %s\n", *tag, filepath.Base(wav), dur(wav), i, r.Segments,
				r.Wall.Seconds(), r.User.Seconds(), float64(r.MaxRSS)/1e6, strings.TrimPrefix(r.Done, "[done] "), loadavg())
			if *text {
				fmt.Printf("text\t%s\n", r.Text)
			}
		}
	}
	return nil
}

// blocks is the per-20 ms RMS in dBFS and the count of clipped samples.
func blocks(s []int16) (rms []float64, clipped int) {
	for i := 0; i+audio.Block <= len(s); i += audio.Block {
		var acc float64
		for _, v := range s[i : i+audio.Block] {
			f := float64(v) / 32768
			acc += f * f
			if v >= 32700 || v <= -32700 {
				clipped++
			}
		}
		rms = append(rms, audio.DBFS(math.Sqrt(acc/audio.Block)))
	}
	return rms, clipped
}

func percentile(sorted []float64, p float64) float64 {
	if len(sorted) == 0 {
		return -120
	}
	i := int(p * float64(len(sorted)-1))
	return sorted[i]
}

func levels(args []string) error {
	if len(args) == 0 {
		return fmt.Errorf("levels WAV...")
	}
	fmt.Println("clip\taudio_s\tp05\tp10\tp50\tp90\tp99\tpeak\tclipped\tfloor(p10)\tsnr(p90-p10)")
	for _, wav := range args {
		s, err := audio.ReadWAV(wav)
		if err != nil {
			return err
		}
		rms, clipped := blocks(s)
		sorted := append([]float64(nil), rms...)
		sort.Float64s(sorted)
		_, peak := audio.Stats(s)
		p10, p90 := percentile(sorted, 0.10), percentile(sorted, 0.90)
		fmt.Printf("%s\t%.1f\t%.1f\t%.1f\t%.1f\t%.1f\t%.1f\t%.1f\t%d\t%.1f\t%.1f\n", filepath.Base(wav), dur(wav),
			percentile(sorted, 0.05), p10, percentile(sorted, 0.5), p90, percentile(sorted, 0.99), peak, clipped, p10, p90-p10)
	}
	return nil
}

// split cuts a take as megavoice does and writes the chunks and their cut
// list: with -whole D, a take no longer than D is one chunk, trimmed
// (session.Whole, audio.TrimBounds); else audio.Chunker's cuts, the live
// chunker. cuts.tsv gives each chunk's samples [start, end) of the take, the
// same in seconds, and the kinds of its two edges (audio.EdgeStart …
// audio.EdgeWhole); reason is the chunker's own word for the end edge.
func split(args []string) error {
	fs := flag.NewFlagSet("split", flag.ContinueOnError)
	pause := fs.Int("pause", 800, "quiet run that ends a chunk, ms")
	maxS := fs.Float64("max", 30, "longest chunk, s")
	minS := fs.Float64("min", 3, "shortest chunk a pause may end, s")
	margin := fs.Float64("margin", 8, "quiet = below floor + margin dB")
	whole := fs.Duration("whole", 0, "a take this long or shorter is one trimmed chunk, as megavoice's take.whole_max (0: always cut)")
	if err := fs.Parse(args); err != nil || fs.NArg() != 2 {
		return fmt.Errorf("split [-pause MS] [-max S] [-min S] [-margin DB] [-whole D] OUTDIR WAV")
	}
	dir, wav := fs.Arg(0), fs.Arg(1)
	s, err := audio.ReadWAV(wav)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	segs := segments(s, *whole, audio.Chunker{
		Pause:  time.Duration(*pause) * time.Millisecond,
		Max:    time.Duration(*maxS * float64(time.Second)),
		Min:    time.Duration(*minS * float64(time.Second)),
		Margin: *margin,
	})
	f, err := os.Create(filepath.Join(dir, "cuts.tsv"))
	if err != nil {
		return err
	}
	defer f.Close()
	fmt.Fprintln(f, "chunk\tstart\tend\tstart_s\tend_s\tlen_s\tstart_reason\tend_reason\treason")
	for i, g := range segs {
		name := filepath.Join(dir, fmt.Sprintf("chunk-%03d.wav", i+1))
		if err := audio.SaveWAV(name, s[g.Start:g.End]); err != nil {
			return err
		}
		fmt.Fprintf(f, "%s\t%d\t%d\t%.2f\t%.2f\t%.2f\t%s\t%s\t%s\n", filepath.Base(name), g.Start, g.End,
			float64(g.Start)/audio.Rate, float64(g.End)/audio.Rate, float64(g.End-g.Start)/audio.Rate, g.From, g.To, g.Reason)
	}
	fmt.Printf("%s: %d chunks in %s (cuts.tsv)\n", filepath.Base(wav), len(segs), dir)
	return nil
}

// segment is one chunk of a take: its samples [Start, End), the kinds of its
// edges, and the chunker's reason for the end edge.
type segment struct {
	Start, End int
	From, To   string
	Reason     string
}

// segments is what megavoice's controller queues for a finished take of s:
// within whole, one chunk of s's trim bounds; else c's cuts, fed as the live
// stream feeds it, one 20 ms block at a time, and the tail.
func segments(s []int16, whole time.Duration, c audio.Chunker) []segment {
	if session.Whole(len(s), whole) {
		lo, hi := audio.TrimBounds(s)
		return []segment{{lo, hi, audio.EdgeWhole, audio.EdgeWhole, audio.EdgeWhole}}
	}
	var out []segment
	start, from := 0, audio.EdgeStart
	for i := 0; i < len(s); i += audio.Block {
		for _, cut := range c.Push(s[i:min(i+audio.Block, len(s))]) {
			end := min(max(cut.End, start), len(s))
			out = append(out, segment{start, end, from, cut.Kind, cut.Reason})
			start, from = end, cut.Kind
		}
	}
	if start < len(s) {
		out = append(out, segment{start, len(s), from, audio.EdgeTail, audio.EdgeTail})
	}
	return out
}

func cerCmd(args []string) error {
	if len(args) != 2 {
		return fmt.Errorf("cer REF.txt HYP.txt")
	}
	ref, err := os.ReadFile(args[0])
	if err != nil {
		return err
	}
	hyp, err := os.ReadFile(args[1])
	if err != nil {
		return err
	}
	r, h := score.ScriptUnits(string(ref)), score.ScriptUnits(string(hyp))
	d := score.EditDistance(r, h)
	fmt.Printf("ref_units\t%d\thyp_units\t%d\terrors\t%d\trate\t%.3f\n", len(r), len(h), d, float64(d)/float64(len(r)))
	return nil
}

// scriptCheck scores the scripted clips DIR/script.tsv names against their script
// (internal/score).
func scriptCheck(args []string) error {
	fs := flag.NewFlagSet("script", flag.ContinueOnError)
	plain := fs.Bool("plain", false, "no hotwords, no corrections")
	texts := fs.String("texts", "", "score TEXTS/<clip>.txt (e.g. megavoice replay -out) instead of running the CLI")
	if err := fs.Parse(args); err != nil || fs.NArg() != 1 {
		return fmt.Errorf("script [-plain] [-texts DIR] UTTERANCE_DIR")
	}
	var chain post.Chain
	if !*plain && *texts == "" {
		var err error
		if chain, err = postChain(); err != nil {
			return err
		}
	}
	clips, err := score.ScriptClips(fs.Arg(0))
	if err != nil {
		return err
	}
	var total, errs int
	fmt.Println("clip\tref_units\terrors\ttext")
	for _, c := range clips {
		var text string
		if *texts != "" {
			b, err := os.ReadFile(filepath.Join(*texts, c.Stamp+".txt"))
			if err != nil {
				return err
			}
			text = strings.TrimSpace(string(b))
		} else {
			r := runCLI(filepath.Join(fs.Arg(0), c.Stamp+".wav"), *plain)
			if r.Err != nil {
				return r.Err
			}
			text = r.Text
			if !*plain {
				text = chain.Apply(text)
			}
		}
		ref := score.ScriptUnits(c.Script)
		d := score.EditDistance(ref, score.ScriptUnits(text))
		total, errs = total+len(ref), errs+d
		fmt.Printf("%s\t%d\t%d\t%s\n", c.Stamp, len(ref), d, text)
	}
	fmt.Printf("total\t%d\t%d\t%.1f%%\n", total, errs, 100*float64(errs)/float64(total))
	return nil
}

// join stitches chunk texts: nothing between CJK neighbours (the CLI's own
// behaviour between VAD segments), one space between two Latin/digit sides.
func join(parts []string) string {
	var b strings.Builder
	for _, p := range parts {
		p = strings.TrimSpace(p)
		if p == "" {
			continue
		}
		if b.Len() > 0 {
			last, _ := lastRune(b.String()), 0
			first := []rune(p)[0]
			if !unicode.Is(unicode.Han, last) && !unicode.Is(unicode.Han, first) && !unicode.IsPunct(last) {
				b.WriteByte(' ')
			}
		}
		b.WriteString(p)
	}
	return b.String()
}

func lastRune(s string) rune {
	rs := []rune(s)
	if len(rs) == 0 {
		return 0
	}
	return rs[len(rs)-1]
}

// stitch transcribes every chunk of a split directory in order, prints the
// per-chunk times and writes the joined text to DIR/stitched.txt.
func stitch(args []string) error {
	fs := flag.NewFlagSet("stitch", flag.ContinueOnError)
	plain := fs.Bool("plain", false, "no hotwords")
	res := fs.Bool("resident", false, "one --serve process")
	gpu := fs.Int("gpu", 0, "resident: --gpu layers")
	bin := fs.String("bin", "", "resident: binary")
	if err := fs.Parse(args); err != nil || fs.NArg() != 1 {
		return fmt.Errorf("stitch [-plain] [-resident [-gpu N] [-bin PATH]] DIR")
	}
	dir := fs.Arg(0)
	chunks, err := filepath.Glob(filepath.Join(dir, "chunk-*.wav"))
	if err != nil {
		return err
	}
	sort.Strings(chunks)
	var rp *resident
	if *res {
		if rp, err = startResident(*plain, *gpu, *bin); err != nil {
			return err
		}
		defer rp.stop()
	}
	fmt.Println("chunk\taudio_s\twall_s\ttext")
	var parts []string
	var total, audio_s float64
	for _, c := range chunks {
		var txt string
		var d time.Duration
		if rp != nil {
			if txt, d, err = rp.transcribe(c, false); err != nil {
				return err
			}
		} else {
			r := runCLI(c, *plain)
			if r.Err != nil {
				return r.Err
			}
			txt, d = r.Text, r.Wall
		}
		parts = append(parts, txt)
		total += d.Seconds()
		audio_s += dur(c)
		fmt.Printf("%s\t%.1f\t%.2f\t%s\n", filepath.Base(c), dur(c), d.Seconds(), txt)
	}
	out := join(parts)
	if err := os.WriteFile(filepath.Join(dir, "stitched.txt"), []byte(out+"\n"), 0o644); err != nil {
		return err
	}
	fmt.Printf("total\t%.1f\t%.2f\t%d chunks; last chunk %.2f s (the stop-to-text tail)\n", audio_s, total, len(chunks), lastDur(chunks, rp, plain))
	return nil
}

// lastDur is a placeholder for the tail figure: the last chunk's wall time is
// already in the table; the summary repeats its audio length.
func lastDur(chunks []string, _ *resident, _ *bool) float64 {
	if len(chunks) == 0 {
		return 0
	}
	return dur(chunks[len(chunks)-1])
}
