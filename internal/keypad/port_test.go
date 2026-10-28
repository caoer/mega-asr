package keypad

import (
	"errors"
	"io"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

// fdPort's read side on a non-blocking pipe: bytes come through, the far end
// hanging up is io.EOF, and Close ends a Read that is waiting.
func TestFdPortRead(t *testing.T) {
	pipe := func() (*fdPort, int) {
		var fds [2]int
		if err := unix.Pipe(fds[:]); err != nil {
			t.Fatal(err)
		}
		if err := unix.SetNonblock(fds[0], true); err != nil {
			t.Fatal(err)
		}
		return &fdPort{fd: fds[0]}, fds[1]
	}

	p, w := pipe()
	unix.Write(w, []byte("key4\n"))
	buf := make([]byte, 16)
	if n, err := p.Read(buf); err != nil || string(buf[:n]) != "key4\n" {
		t.Fatalf("Read = %q, %v", buf[:n], err)
	}
	unix.Close(w)
	if _, err := p.Read(buf); err != io.EOF {
		t.Fatalf("Read after hang-up = %v, want EOF", err)
	}
	p.Close()

	p, w = pipe()
	defer unix.Close(w)
	got := make(chan error, 1)
	go func() { _, err := p.Read(buf); got <- err }()
	time.Sleep(2 * pollEvery)
	p.Close()
	select {
	case err := <-got:
		if !errors.Is(err, errClosed) {
			t.Fatalf("Read after Close = %v, want errClosed", err)
		}
	case <-time.After(time.Second):
		t.Fatal("Close did not end Read")
	}
	if _, err := p.Write([]byte("x")); !errors.Is(err, errClosed) {
		t.Fatalf("Write after Close = %v", err)
	}
}
