//go:build unix

package audio

import (
	"context"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

// A local XVF whose start context is cancelled ends arecord too, not only
// the shell around it: an orphaned arecord holds the PCM and the stdout
// pipe, so the stream never ends and the device stays busy.
func TestXVFCancelEndsArecord(t *testing.T) {
	dir := t.TempDir()
	pidf := filepath.Join(dir, "pid")
	// a stand-in arecord that ignores SIGINT, so only a kill of its
	// process group ends it once the shell is gone
	fake := "#!/bin/sh\necho $$ > " + pidf + "\ntrap '' INT\nwhile :; do sleep 1; done\n"
	if err := os.WriteFile(filepath.Join(dir, "arecord"), []byte(fake), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))

	ctx, cancel := context.WithCancel(context.Background())
	x := &XVF{Device: "fake", Channel: 2}
	ch, err := x.Start(ctx)
	if err != nil {
		t.Fatal(err)
	}
	var pid int
	for deadline := time.Now().Add(3 * time.Second); pid == 0; {
		if b, err := os.ReadFile(pidf); err == nil {
			pid, _ = strconv.Atoi(strings.TrimSpace(string(b)))
		}
		if time.Now().After(deadline) {
			t.Fatal("the stand-in arecord never started")
		}
		time.Sleep(20 * time.Millisecond)
	}
	cancel()
	select {
	case <-drain(ch):
	case <-time.After(8 * time.Second):
		t.Fatal("the stream did not end after its context was cancelled")
	}
	// The group kill leaves arecord a zombie whose parent, the shell, died
	// with it; init reaps it moments later. kill(pid, 0) succeeds on a
	// zombie, so only a pid that stays past the deadline is a live arecord.
	for deadline := time.Now().Add(2 * time.Second); syscall.Kill(pid, 0) == nil; {
		if time.Now().After(deadline) {
			_ = syscall.Kill(pid, syscall.SIGKILL)
			t.Fatalf("arecord (pid %d) outlived the cancelled stream", pid)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func drain(ch <-chan []int16) <-chan struct{} {
	done := make(chan struct{})
	go func() {
		for range ch {
		}
		close(done)
	}()
	return done
}
