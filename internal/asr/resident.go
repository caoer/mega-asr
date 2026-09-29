package asr

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
)

// ErrNoServe means llama-funasr-cli did not come up in --serve mode: a binary
// built without runtime/funasr-cli-serve.patch, or no [ready] within 10 s. It
// is sticky — every later Transcribe returns it at once — so the caller
// transcribes with FunASR for the rest of the session.
var ErrNoServe = errors.New("asr: llama-funasr-cli --serve did not start")

const (
	readyTimeout = 10 * time.Second
	replyBase    = 60 * time.Second // reply deadline: this + 2 × the clip's length
)

// Resident keeps one `llama-funasr-cli --serve` process with the models
// loaded, which saves the CLI's fixed 0.45–0.65 s per clip. The process starts
// on first use; requests go one at a time as a line "wav\thotwords", or
// "wav\thotwords\twhole" to decode the file as one window without the VAD,
// and each gets one reply line. A process that exits or misses the reply deadline is
// restarted once and the request re-sent; a second failure is returned, and
// the next Transcribe starts a fresh process. Output is Clean'd as FunASR's.
type Resident struct {
	Root string // mega-asr checkout holding bin/ and models/
	LLM  string // Qwen3 GGUF in place of Root's, as FunASR.LLM
	// Hotwords returns the terms for this clip, read per request as FunASR
	// does; nil means none. Chinese segments only (--hotwords-cjk).
	Hotwords func() []string
	GPU      int  // --gpu N: Qwen3 layers on Metal; 0 decodes on the CPU
	EncGPU   bool // --enc-gpu: the encoder on Metal; the CLI falls back to the CPU and says so on stderr
	VADGPU   bool // --vad-gpu: FSMN-VAD on Metal; the same fallback
	Threads  int  // --threads N: CPU encoder threads; 0 keeps the CLI's 8
	// LLMThreads is --llm-threads N: Qwen3's CPU threads; 0 keeps the CLI's
	// 4 and passes no flag, which a CLI built before the flag needs.
	LLMThreads int

	readyWait, replyWait time.Duration // tests shorten readyTimeout, replyBase

	mu      sync.Mutex
	p       *serveProc
	noServe error
}

func (r *Resident) Transcribe(ctx context.Context, wav string, whole bool) (string, error) {
	if strings.ContainsAny(wav, "\t\n\r") {
		return "", fmt.Errorf("asr: resident: path %q has a tab or newline", wav)
	}
	st, err := os.Stat(wav)
	if err != nil {
		return "", fmt.Errorf("asr: %w", err)
	}
	var hw string
	if r.Hotwords != nil {
		hw = strings.Join(r.Hotwords(), ", ")
		hw = strings.NewReplacer("\t", " ", "\n", " ", "\r", " ").Replace(hw)
	}
	req := wav + "\t" + hw
	if whole {
		req += "\twhole"
	}
	req += "\n"
	clip := time.Duration(float64(max(st.Size()-44, 0)) / (2 * 16000) * float64(time.Second))
	deadline := or(r.replyWait, replyBase) + 2*clip

	r.mu.Lock()
	defer r.mu.Unlock()
	if r.noServe != nil {
		return "", r.noServe
	}
	for attempt := 0; ; attempt++ {
		if err := ctx.Err(); err != nil {
			return "", err
		}
		if r.p == nil {
			p, err := r.start()
			if err != nil {
				r.noServe = err
				return "", err
			}
			r.p = p
		}
		text, err := r.p.request(ctx, req, deadline)
		var failed cliFailure
		switch {
		case err == nil:
			return Clean(text), nil
		case errors.As(err, &failed): // the CLI could not read or segment this file; the process is fine
			return "", err
		}
		r.p.kill()
		r.p = nil
		if ctx.Err() != nil || attempt == 1 {
			return "", err
		}
		log.Printf("%v; restarting and re-sending %s", err, filepath.Base(wav))
	}
}

// Stop closes the process's stdin (the CLI exits at EOF) and waits up to 5 s
// before killing it. A later Transcribe starts a new process.
func (r *Resident) Stop() {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.p == nil {
		return
	}
	r.p.in.Close()
	select {
	case <-r.p.eof:
	case <-time.After(5 * time.Second):
	}
	r.p.kill()
	r.p = nil
}

func (r *Resident) start() (*serveProc, error) {
	p := func(rel string) string { return filepath.Join(r.Root, rel) }
	args := []string{
		"--enc", p("models/funasr-encoder-f16.gguf"),
		"-m", LLMPath(r.Root, r.LLM),
		"--vad", p("models/fsmn-vad.gguf"),
		"--hotwords-cjk", "--serve",
	}
	if r.GPU > 0 {
		args = append(args, "--gpu", strconv.Itoa(r.GPU))
	}
	if r.EncGPU {
		args = append(args, "--enc-gpu")
	}
	if r.VADGPU {
		args = append(args, "--vad-gpu")
	}
	if r.Threads > 0 {
		args = append(args, "--threads", strconv.Itoa(r.Threads))
	}
	if r.LLMThreads > 0 {
		args = append(args, "--llm-threads", strconv.Itoa(r.LLMThreads))
	}
	cmd := exec.Command(p("bin/llama-funasr-cli"), args...)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true} // kill reaches anything it spawned
	in, err := cmd.StdinPipe()
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrNoServe, err)
	}
	out, err := cmd.StdoutPipe()
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrNoServe, err)
	}
	errp, err := cmd.StderrPipe()
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrNoServe, err)
	}
	t0 := time.Now()
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("%w: %v", ErrNoServe, err)
	}
	sp := &serveProc{cmd: cmd, in: in, out: make(chan string, 1), errs: make(chan string, 64),
		eof: make(chan struct{}), done: make(chan struct{})}
	go pump(out, sp.out, sp.done, nil)
	go pump(errp, sp.errs, sp.done, sp.eof)
	wait := or(r.readyWait, readyTimeout)
	timer := time.NewTimer(wait)
	defer timer.Stop()
	var last string
	enc, vad := "CPU", "CPU" // the CLI's [enc] and [vad] lines before [ready] name the backends
	for {
		select {
		case l, ok := <-sp.errs:
			switch {
			case !ok:
				sp.kill()
				return nil, fmt.Errorf("%w: exited before [ready]: %s", ErrNoServe, last)
			case strings.HasPrefix(l, "[ready]"):
				log.Printf("asr: resident ready in %.2fs (pid %d, gpu %d, enc %s, vad %s, threads %d, llm threads %d)",
					time.Since(t0).Seconds(), cmd.Process.Pid, r.GPU, enc, vad, r.Threads, r.LLMThreads)
				return sp, nil
			case strings.HasPrefix(l, "[enc] "):
				enc = strings.TrimPrefix(l, "[enc] ")
			case strings.HasPrefix(l, "[vad] "):
				vad = strings.TrimPrefix(l, "[vad] ")
			case strings.TrimSpace(l) != "":
				last = l
			}
		case <-timer.C:
			sp.kill()
			return nil, fmt.Errorf("%w: no [ready] within %v", ErrNoServe, wait)
		}
	}
}

// serveProc is one running `--serve` process; stdout and stderr arrive as
// lines on out and errs, each closed at EOF (eof also closes with stderr).
type serveProc struct {
	cmd       *exec.Cmd
	in        io.WriteCloser
	out, errs chan string
	eof, done chan struct{}
	killed    sync.Once
}

// cliFailure is a request the CLI answered with an empty reply and no
// [stage] line: it could not read or segment the file (the reason is on
// stderr). The process itself is healthy.
type cliFailure struct{ reason string }

func (e cliFailure) Error() string { return "llama-funasr-cli: " + e.reason }

// request sends one line and waits for its reply on stdout and its [done] on
// stderr — the two streams arrive in either order. A [stage] line between
// them marks a transcribed file; without one the reply is a failure.
func (p *serveProc) request(ctx context.Context, req string, deadline time.Duration) (string, error) {
	if _, err := io.WriteString(p.in, req); err != nil {
		return "", fmt.Errorf("asr: resident: write: %w", err)
	}
	timer := time.NewTimer(deadline)
	defer timer.Stop()
	var reply, last string
	var replied, done, stage bool
	for !replied || !done {
		select {
		case l, ok := <-p.out:
			if !ok {
				return "", fmt.Errorf("asr: resident: process exited mid-request: %s", last)
			}
			reply, replied = l, true
		case l, ok := <-p.errs:
			switch {
			case !ok:
				return "", fmt.Errorf("asr: resident: process exited mid-request: %s", last)
			case strings.HasPrefix(l, "[stage]"):
				stage = true
			case strings.HasPrefix(l, "[done]"):
				done = true
			case strings.TrimSpace(l) != "" && !strings.HasPrefix(l, "["):
				last = l
			}
		case <-timer.C:
			return "", fmt.Errorf("asr: resident: no reply within %v", deadline)
		case <-ctx.Done():
			return "", ctx.Err()
		}
	}
	if !stage {
		return "", cliFailure{last}
	}
	return reply, nil
}

// kill ends the process group and reaps it; the pumps exit on EOF or done.
func (p *serveProc) kill() {
	p.killed.Do(func() {
		close(p.done)
		syscall.Kill(-p.cmd.Process.Pid, syscall.SIGKILL)
		p.cmd.Wait()
	})
}

// pump sends r's lines, without the newline, until EOF or done.
func pump(r io.Reader, ch chan<- string, done <-chan struct{}, eof chan<- struct{}) {
	defer close(ch)
	if eof != nil {
		defer close(eof)
	}
	br := bufio.NewReader(r)
	for {
		l, err := br.ReadString('\n')
		if l = strings.TrimRight(l, "\r\n"); l != "" || err == nil {
			select {
			case ch <- l:
			case <-done:
				return
			}
		}
		if err != nil {
			return
		}
	}
}

func or(d, def time.Duration) time.Duration {
	if d > 0 {
		return d
	}
	return def
}
