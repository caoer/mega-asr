// Package ctl is the control socket between `megavoice serve` and its
// subcommands: one JSON request line, one JSON response line. The server does
// the work, so TCC answers for the launchd-started app, not the caller's
// terminal.
package ctl

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"time"
)

// Request names a command and its arguments.
type Request struct {
	Cmd  string `json:"cmd"`
	Text string `json:"text,omitempty"`
	PID  int32  `json:"pid,omitempty"`
	// Args carries a command's structured arguments, as the command defines them.
	Args json.RawMessage `json:"args,omitempty"`
}

// Response carries the command's result or its error.
type Response struct {
	OK    bool            `json:"ok"`
	Error string          `json:"error,omitempty"`
	Data  json.RawMessage `json:"data,omitempty"`
}

// Reply builds a successful response around v.
func Reply(v any) Response {
	b, err := json.Marshal(v)
	if err != nil {
		return Fail(err)
	}
	return Response{OK: true, Data: b}
}

// Fail builds an error response.
func Fail(err error) Response { return Response{Error: err.Error()} }

// Listen serves h on a unix socket at path (mode 0600) until the listener is
// closed. The caller holds the instance lock, so a stale socket is removed.
func Listen(path string, h func(Request) Response) (net.Listener, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, err
	}
	_ = os.Remove(path)
	l, err := net.Listen("unix", path)
	if err != nil {
		return nil, err
	}
	if err := os.Chmod(path, 0o600); err != nil {
		l.Close()
		return nil, err
	}
	go func() {
		for {
			c, err := l.Accept()
			if err != nil {
				return
			}
			go ServeConn(c, h)
		}
	}()
	return l, nil
}

// ServeConn answers one request on c with h, then closes c.
func ServeConn(c net.Conn, h func(Request) Response) {
	defer c.Close()
	var req Request
	line, err := bufio.NewReader(c).ReadBytes('\n')
	var resp Response
	if err != nil {
		resp = Fail(fmt.Errorf("read request: %w", err))
	} else if err := json.Unmarshal(line, &req); err != nil {
		resp = Fail(fmt.Errorf("bad request: %w", err))
	} else {
		resp = h(req)
	}
	b, _ := json.Marshal(resp)
	_, _ = c.Write(append(b, '\n'))
}

// ErrNotRunning means nothing listens on the socket.
var ErrNotRunning = errors.New("megavoice serve is not running")

// Call sends one request and waits up to timeout for the response. A socket
// with no listener refuses the dial at once; the dial shares the call's
// timeout, so a busy host that is slow to connect is not taken for a
// stopped server.
func Call(path string, req Request, timeout time.Duration) (Response, error) {
	c, err := net.DialTimeout("unix", path, timeout)
	if err != nil {
		return Response{}, fmt.Errorf("%w (%s)", ErrNotRunning, path)
	}
	defer c.Close()
	_ = c.SetDeadline(time.Now().Add(timeout))
	b, _ := json.Marshal(req)
	if _, err := c.Write(append(b, '\n')); err != nil {
		return Response{}, err
	}
	line, err := bufio.NewReader(c).ReadBytes('\n')
	if err != nil {
		return Response{}, fmt.Errorf("read response: %w", err)
	}
	var resp Response
	if err := json.Unmarshal(line, &resp); err != nil {
		return Response{}, err
	}
	return resp, nil
}
