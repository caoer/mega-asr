package asr

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// fakeResident runs testdata/fake-serve.sh as the root's bin/llama-funasr-cli.
func fakeResident(t *testing.T, mode string) (*Resident, string) {
	dir := t.TempDir()
	fake, err := filepath.Abs("testdata/fake-serve.sh")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(dir, "bin"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(fake, filepath.Join(dir, "bin", "llama-funasr-cli")); err != nil {
		t.Fatal(err)
	}
	t.Setenv("FAKE_DIR", dir)
	t.Setenv("FAKE_MODE", mode)
	r := &Resident{Root: dir, readyWait: 5 * time.Second, replyWait: 5 * time.Second}
	t.Cleanup(r.Stop)
	return r, dir
}

func wavFile(t *testing.T, dir, name string, size int) string {
	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, make([]byte, size), 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

func starts(t *testing.T, dir string) int {
	b, err := os.ReadFile(filepath.Join(dir, "starts"))
	if err != nil {
		t.Fatal(err)
	}
	return strings.Count(string(b), "\n")
}

func TestResidentRequests(t *testing.T) {
	r, dir := fakeResident(t, "ok")
	r.GPU, r.EncGPU, r.VADGPU, r.Threads = 99, true, true, 4
	lists := [][]string{{"marmot", "ibex", "zebu"}, {"gnu\tyak"}, nil}
	n := 0
	r.Hotwords = func() []string { n++; return lists[n-1] }
	ctx := context.Background()

	got, err := r.Transcribe(ctx, wavFile(t, dir, "a.wav", 1000), false)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(got, "a.wav hw=[marmot, ibex, zebu] opt=; args=[--enc "+dir) || !strings.HasSuffix(got, "--hotwords-cjk --serve --gpu 99 --enc-gpu --vad-gpu --threads 4]") {
		t.Errorf("reply %q", got)
	}
	if got, err = r.Transcribe(ctx, wavFile(t, dir, "b.wav", 1000), true); err != nil || !strings.HasPrefix(got, "b.wav hw=[gnu yak] opt=whole; ") {
		t.Errorf("second request: %q, %v", got, err)
	}
	// an unreadable clip fails that request only; the process stays
	r.Hotwords = nil
	_, err = r.Transcribe(ctx, wavFile(t, dir, "empty.wav", 0), false)
	var cf cliFailure
	if !errors.As(err, &cf) || !strings.Contains(err.Error(), "failed to read audio") {
		t.Errorf("empty clip: %v", err)
	}
	if got, err = r.Transcribe(ctx, wavFile(t, dir, "c.wav", 1000), false); err != nil || !strings.HasPrefix(got, "c.wav hw=[] opt=; ") {
		t.Errorf("after a failure: %q, %v", got, err)
	}
	if s := starts(t, dir); s != 1 {
		t.Errorf("%d process starts, want 1", s)
	}
}

func TestResidentRestart(t *testing.T) {
	for _, mode := range []string{"die-once", "hang-once"} {
		t.Run(mode, func(t *testing.T) {
			r, dir := fakeResident(t, mode)
			r.replyWait = 300 * time.Millisecond
			got, err := r.Transcribe(context.Background(), wavFile(t, dir, "a.wav", 1000), false)
			if err != nil || !strings.HasPrefix(got, "a.wav ") {
				t.Fatalf("got %q, %v", got, err)
			}
			if s := starts(t, dir); s != 2 {
				t.Errorf("%d process starts, want 2", s)
			}
		})
	}
}

func TestResidentNoServe(t *testing.T) {
	r, dir := fakeResident(t, "noready")
	r.readyWait = 300 * time.Millisecond
	wav := wavFile(t, dir, "a.wav", 1000)
	if _, err := r.Transcribe(context.Background(), wav, false); !errors.Is(err, ErrNoServe) {
		t.Fatalf("err %v, want ErrNoServe", err)
	}
	t0 := time.Now()
	if _, err := r.Transcribe(context.Background(), wav, false); !errors.Is(err, ErrNoServe) || time.Since(t0) > 100*time.Millisecond {
		t.Errorf("second call: %v after %v, want ErrNoServe at once", err, time.Since(t0))
	}
	if s := starts(t, dir); s != 1 {
		t.Errorf("%d process starts, want 1", s)
	}
}
