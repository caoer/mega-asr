package keypad

import (
	"errors"
	"io"
	"sync"
	"time"

	"golang.org/x/sys/unix"
)

// openPort opens the tty for reading and writing, raw, non-blocking — so a
// keypad that stops reading costs a dropped snapshot instead of a stuck
// writer — and exclusive, so a second megavoice cannot interleave its lines.
// Opening raises DTR, which the firmware reads as megavoice being present;
// closing drops it.
func openPort(path string) (io.ReadWriteCloser, error) {
	fd, err := unix.Open(path, unix.O_RDWR|unix.O_NOCTTY|unix.O_NONBLOCK|unix.O_CLOEXEC, 0)
	if err != nil {
		return nil, err
	}
	if err := unix.IoctlSetInt(fd, unix.TIOCEXCL, 0); err != nil {
		unix.Close(fd)
		return nil, err
	}
	if err := makeRaw(fd); err != nil {
		unix.Close(fd)
		return nil, err
	}
	return &fdPort{fd: fd}, nil
}

// makeRaw turns off echo, line editing and every byte translation, so the
// keypad's lines arrive as sent and nothing is echoed back at it.
func makeRaw(fd int) error {
	t, err := unix.IoctlGetTermios(fd, ioctlGetTermios)
	if err != nil {
		return err
	}
	t.Iflag &^= unix.IGNBRK | unix.BRKINT | unix.PARMRK | unix.ISTRIP | unix.INLCR | unix.IGNCR | unix.ICRNL | unix.IXON
	t.Oflag &^= unix.OPOST
	t.Lflag &^= unix.ECHO | unix.ECHONL | unix.ICANON | unix.ISIG | unix.IEXTEN
	t.Cflag &^= unix.CSIZE | unix.PARENB
	t.Cflag |= unix.CS8 | unix.CREAD | unix.CLOCAL
	t.Cc[unix.VMIN], t.Cc[unix.VTIME] = 1, 0
	return unix.IoctlSetTermios(fd, ioctlSetTermios, t)
}

// pollEvery bounds how long a Read waits before it looks at whether the port
// was closed.
const pollEvery = 100 * time.Millisecond

var errClosed = errors.New("port closed")

// fdPort works the descriptor directly: an *os.File would park a
// non-blocking write in the runtime poller until the keypad reads, where
// this returns EAGAIN. Read polls in short slices so Close, from another
// goroutine, ends it; the descriptor is only touched under mu while open, so
// a closed number reused by another file is never read.
type fdPort struct {
	mu     sync.Mutex
	fd     int
	closed bool
}

func (p *fdPort) Write(b []byte) (int, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.closed {
		return 0, errClosed
	}
	n, err := unix.Write(p.fd, b)
	return max(n, 0), err
}

// Read blocks until bytes arrive. It returns io.EOF when the device hangs up
// and errClosed after Close.
func (p *fdPort) Read(b []byte) (int, error) {
	for {
		p.mu.Lock()
		if p.closed {
			p.mu.Unlock()
			return 0, errClosed
		}
		fds := []unix.PollFd{{Fd: int32(p.fd), Events: unix.POLLIN}}
		p.mu.Unlock()
		if _, err := unix.Poll(fds, int(pollEvery/time.Millisecond)); err != nil && !errors.Is(err, unix.EINTR) {
			return 0, err
		}
		if fds[0].Revents == 0 {
			continue
		}
		p.mu.Lock()
		if p.closed {
			p.mu.Unlock()
			return 0, errClosed
		}
		n, err := unix.Read(p.fd, b)
		p.mu.Unlock()
		switch {
		case errors.Is(err, unix.EAGAIN) || errors.Is(err, unix.EINTR):
		case err != nil:
			return 0, err
		case n == 0:
			return 0, io.EOF
		default:
			return n, nil
		}
	}
}

func (p *fdPort) Close() error {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.closed {
		return nil
	}
	p.closed = true
	return unix.Close(p.fd)
}
