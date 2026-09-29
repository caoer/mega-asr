package app

import (
	"context"
	_ "embed"
	"errors"
	"log"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/caoer/mega-asr/internal/asr"
)

// residentRetry is how long the CLI serves alone after the resident process
// failed to start, before a fresh one is tried: a slow Metal start must not
// pin the rest of the app's life to exec.
const residentRetry = time.Minute

// Fallback transcribes with a resident `llama-funasr-cli --serve` process
// and, when that fails for a chunk, with one CLI run for it; a chunk whose
// context is cancelled gets none. A resident
// process that does not start is replaced by a fresh one residentRetry later.
type Fallback struct {
	new  func() *asr.Resident
	exec asr.Transcriber

	mu      sync.Mutex
	res     *asr.Resident
	retryAt time.Time
}

func (f *Fallback) resident() *asr.Resident {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.res == nil && !time.Now().Before(f.retryAt) {
		f.res = f.new()
	}
	return f.res
}

// transcribeResident tries the resident process alone.
func (f *Fallback) transcribeResident(ctx context.Context, wav string, whole bool) (string, error) {
	res := f.resident()
	if res == nil {
		return "", asr.ErrNoServe
	}
	text, err := res.Transcribe(ctx, wav, whole)
	if errors.Is(err, asr.ErrNoServe) {
		f.mu.Lock()
		if f.res == res {
			f.res, f.retryAt = nil, time.Now().Add(residentRetry)
			log.Printf("asr: %v; the CLI runs per chunk for %v", err, residentRetry)
		}
		f.mu.Unlock()
	}
	return text, err
}

func (f *Fallback) Transcribe(ctx context.Context, wav string, whole bool) (string, error) {
	text, err := f.transcribeResident(ctx, wav, whole)
	if err == nil || ctx.Err() != nil { // cancelled: no CLI either
		return text, err
	}
	if !errors.Is(err, asr.ErrNoServe) {
		log.Printf("asr: resident: %v; running the CLI for %s", err, filepath.Base(wav))
	}
	return f.exec.Transcribe(ctx, wav, whole)
}

// Stop ends the resident process, if one runs.
func (f *Fallback) Stop() {
	f.mu.Lock()
	res := f.res
	f.res = nil
	f.mu.Unlock()
	if res != nil {
		res.Stop()
	}
}

//go:embed warmup.wav
var warmupWAV []byte

// Warm starts the resident process at launch and sends it one short clip,
// so the first take pays neither the start nor the first Metal request. The
// first run of a newly installed CLI can miss the [ready] window that later
// runs meet; one more start is tried at once.
func (f *Fallback) Warm(dir string) {
	path := filepath.Join(dir, "warmup.wav")
	if err := os.WriteFile(path, warmupWAV, 0o644); err != nil {
		log.Printf("asr: warm-up: %v", err)
		return
	}
	for try := 1; ; try++ {
		t0 := time.Now()
		text, err := f.transcribeResident(context.Background(), path, false)
		if err == nil {
			log.Printf("asr: warm-up in %.2f s: %q", time.Since(t0).Seconds(), text)
			return
		}
		log.Printf("asr: warm-up: %v", err)
		if try == 2 || !errors.Is(err, asr.ErrNoServe) {
			return
		}
		f.mu.Lock()
		f.retryAt = time.Time{}
		f.mu.Unlock()
	}
}
