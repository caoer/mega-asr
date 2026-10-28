package ctl

import (
	"errors"
	"net"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// A socket path with nothing behind it, and a socket file whose server is
// gone, are ErrNotRunning at once: the dial's share of the call's timeout
// is for a busy host, not for a stopped server.
func TestCallNotRunningAtOnce(t *testing.T) {
	dir, err := os.MkdirTemp("", "ctl") // short: a unix socket path has ~100 bytes
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })

	stale := filepath.Join(dir, "stale.sock")
	l, err := net.Listen("unix", stale)
	if err != nil {
		t.Fatal(err)
	}
	l.(*net.UnixListener).SetUnlinkOnClose(false)
	l.Close()

	for name, path := range map[string]string{"missing": filepath.Join(dir, "none.sock"), "refused": stale} {
		t0 := time.Now()
		_, err := Call(path, Request{Cmd: "status"}, time.Hour)
		if d := time.Since(t0); !errors.Is(err, ErrNotRunning) || d > time.Minute {
			t.Errorf("%s: %v after %s; want ErrNotRunning at once", name, err, d)
		}
	}
}

func TestCallCarriesDeadline(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "ctl.sock")
	deadlines := make(chan time.Time, 1)
	l, err := Listen(path, func(r Request) Response { deadlines <- r.Deadline; return Reply("ok") })
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	deadline := time.Now().Add(time.Second)
	if _, err := Call(path, Request{Cmd: "paste", Deadline: deadline}, time.Minute); err != nil {
		t.Fatal(err)
	}
	got := <-deadlines
	if !got.Equal(deadline) {
		t.Fatalf("deadline %v, want %v", got, deadline)
	}
}
