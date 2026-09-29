package audio

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"net/http"
	"sync"
	"sync/atomic"
	"time"

	"github.com/coder/websocket"
)

// Remote streams a paired mic server's mic (internal/micserver) over
// WebSocket-over-TLS, trusting only the certificate pairing pinned.
type Remote struct {
	Name        string        // the box's name, as messages say it
	Addr        string        // host:port
	Fingerprint string        // the pinned certificate (CertFingerprint)
	Token       string        // the bearer token pairing gave
	Tail        time.Duration // kept streaming after Stop: the last syllable is in flight

	conn     *websocket.Conn
	stopping atomic.Bool
	errMu    sync.Mutex
	err      error
}

// RemoteStall ends a stream that delivers nothing for this long; remoteDial
// bounds the connect.
const (
	RemoteStall = 2 * time.Second
	remoteDial  = 2 * time.Second
)

// Device names the remote mic, for the take's log line.
func (r *Remote) Device() string { return "remote mic " + r.Name }

// Input names the paired box and the address it streams from.
func (r *Remote) Input() TakeInput { return TakeInput{Source: "remote", Name: r.Name, Host: r.Addr} }

func (r *Remote) Start(ctx context.Context) (<-chan []int16, error) {
	dctx, cancel := context.WithTimeout(ctx, remoteDial)
	defer cancel()
	hc := &http.Client{Transport: &http.Transport{TLSClientConfig: PinnedTLS(r.Fingerprint)}}
	conn, resp, err := websocket.Dial(dctx, "wss://"+r.Addr+"/stream", &websocket.DialOptions{
		HTTPClient: hc,
		HTTPHeader: http.Header{"Authorization": {"Bearer " + r.Token}},
	})
	switch {
	case err == nil:
	case resp != nil && resp.StatusCode == http.StatusUnauthorized:
		return nil, fmt.Errorf("remote mic %s refused — pair again", r.Name)
	case errors.Is(err, ErrNotPinned):
		return nil, fmt.Errorf("remote mic %s: %w — pair again", r.Name, ErrNotPinned)
	default:
		return nil, fmt.Errorf("remote mic %s unreachable: %w", r.Name, DialError(r.Addr, err))
	}
	r.conn = conn
	ch := make(chan []int16, 256)
	go r.pump(ctx, ch)
	return ch, nil
}

func (r *Remote) pump(ctx context.Context, ch chan<- []int16) {
	defer close(ch)
	for {
		rctx, cancel := context.WithTimeout(ctx, RemoteStall)
		typ, b, err := r.conn.Read(rctx)
		stalled := errors.Is(rctx.Err(), context.DeadlineExceeded)
		cancel()
		if err != nil {
			r.end(err, stalled)
			return
		}
		if typ != websocket.MessageBinary {
			continue
		}
		s := make([]int16, len(b)/2)
		for i := range s {
			s[i] = int16(binary.LittleEndian.Uint16(b[2*i:]))
		}
		ch <- s
	}
}

// end records why the stream ended: nothing after Stop; else the box's
// close, or the link.
func (r *Remote) end(err error, stalled bool) {
	if r.stopping.Load() {
		return
	}
	var ce websocket.CloseError
	switch {
	case stalled:
		err = fmt.Errorf("remote mic %s: no audio for %d s", r.Name, int(RemoteStall/time.Second))
	case !errors.As(err, &ce):
		err = fmt.Errorf("remote mic %s: connection lost: %v", r.Name, err)
	case ce.Code == websocket.StatusPolicyViolation:
		err = fmt.Errorf("remote mic %s refused", r.Name)
	case ce.Reason != "":
		err = fmt.Errorf("remote mic %s: %s", r.Name, ce.Reason)
	default:
		err = fmt.Errorf("remote mic %s: the box ended the stream", r.Name)
	}
	r.errMu.Lock()
	r.err = err
	r.errMu.Unlock()
}

// Stop keeps the stream open for Tail, then closes it; the box stops its
// source and the channel closes once what was in flight has arrived.
func (r *Remote) Stop() {
	if r.conn == nil || r.stopping.Swap(true) {
		return
	}
	time.Sleep(r.Tail)
	go r.conn.Close(websocket.StatusNormalClosure, "")
}

func (r *Remote) Err() error {
	r.errMu.Lock()
	defer r.errMu.Unlock()
	return r.err
}
