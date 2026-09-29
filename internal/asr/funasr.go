package asr

import (
	"bytes"
	"context"
	"fmt"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"unicode"
	"unicode/utf8"
)

// FunASR runs Fun-ASR-Nano with FSMN-VAD from a mega-asr checkout, one
// process per clip: the GGUF weights are mmapped, so the page cache keeps
// them warm between clips.
type FunASR struct {
	Root string // mega-asr checkout holding bin/ and models/
	// Hotwords returns the terms for this clip (read per clip, so edits take
	// effect at once); nil means none. They go into the prompt for Chinese
	// segments only (--hotwords-cjk): on English speech the Chinese hotword
	// prompt derails Fun-ASR-Nano.
	Hotwords   func() []string
	GPU        int    // --gpu N: Qwen3 layers on Metal, as Resident; 0 decodes on the CPU
	EncGPU     bool   // --enc-gpu: the encoder on Metal, as Resident
	VADGPU     bool   // --vad-gpu: FSMN-VAD on Metal, as Resident
	Threads    int    // --threads N, as Resident; 0 keeps the CLI's
	LLMThreads int    // --llm-threads N, as Resident; 0 passes no flag
	LLM        string // Qwen3 GGUF in place of Root's models/qwen3-0.6b-q8_0.gguf; "" keeps Root's
}

// LLMPath is the Qwen3 GGUF the CLI loads: llm when set, else root's.
func LLMPath(root, llm string) string {
	if llm != "" {
		return llm
	}
	return filepath.Join(root, "models/qwen3-0.6b-q8_0.gguf")
}

// command is one CLI run; whole leaves out --vad, so the CLI decodes the
// file as one window.
func (f FunASR) command(ctx context.Context, wav string, whole bool) *exec.Cmd {
	p := func(rel string) string { return filepath.Join(f.Root, rel) }
	args := []string{
		"--enc", p("models/funasr-encoder-f16.gguf"),
		"-m", LLMPath(f.Root, f.LLM),
	}
	if !whole {
		args = append(args, "--vad", p("models/fsmn-vad.gguf"))
	}
	if f.Hotwords != nil {
		if hw := f.Hotwords(); len(hw) > 0 {
			args = append(args, "--hotwords", strings.Join(hw, ", "), "--hotwords-cjk")
		}
	}
	if f.EncGPU {
		args = append(args, "--enc-gpu")
	}
	if f.VADGPU && !whole {
		args = append(args, "--vad-gpu")
	}
	if f.GPU > 0 {
		args = append(args, "--gpu", strconv.Itoa(f.GPU))
	}
	if f.Threads > 0 {
		args = append(args, "--threads", strconv.Itoa(f.Threads))
	}
	if f.LLMThreads > 0 {
		args = append(args, "--llm-threads", strconv.Itoa(f.LLMThreads))
	}
	return exec.CommandContext(ctx, p("bin/llama-funasr-cli"), append(args, "-a", wav)...)
}

func (f FunASR) Transcribe(ctx context.Context, wav string, whole bool) (string, error) {
	cmd := f.command(ctx, wav, whole)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		return "", fmt.Errorf("llama-funasr-cli: %v: %s", err, lastLine(stderr.String()))
	}
	return Clean(string(out)), nil
}

// Clean turns the CLI's stdout into one line: VAD segments are joined, the
// silence marker /sil and tags such as [breath] and [noise] are dropped,
// whitespace is collapsed, and a space with
// CJK on both sides — a line break the model wrote between two sentences of
// one window — is dropped.
func Clean(out string) string {
	f := strings.Fields(tag.ReplaceAllString(strings.ReplaceAll(out, "/sil", " "), " "))
	var b strings.Builder
	for i, w := range f {
		if i > 0 {
			last, _ := utf8.DecodeLastRuneInString(f[i-1])
			first, _ := utf8.DecodeRuneInString(w)
			if !CJK(last) || !CJK(first) {
				b.WriteByte(' ')
			}
		}
		b.WriteString(w)
	}
	return b.String()
}

// tag is a non-speech marker the model writes: [breath], [noise], [SPK].
var tag = regexp.MustCompile(`\[[A-Za-z_]+\]`)

// CJK reports a Han, kana or Hangul letter, or CJK or full-width
// punctuation such as "，" and "。".
func CJK(r rune) bool {
	return unicode.In(r, unicode.Han, unicode.Hiragana, unicode.Katakana, unicode.Hangul) ||
		(r >= 0x3000 && r <= 0x303f) || (r >= 0xff00 && r <= 0xffef)
}

func lastLine(s string) string {
	s = strings.TrimSpace(s)
	if i := strings.LastIndexByte(s, '\n'); i >= 0 {
		return s[i+1:]
	}
	return s
}
