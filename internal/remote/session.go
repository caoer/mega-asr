package remote

import (
	"context"
	"errors"
	"sync"
	"time"

	"github.com/coder/websocket"

	"github.com/caoer/mega-asr/internal/relay/e2e"
	"github.com/caoer/mega-asr/internal/relay/wire"
)

// Session is one phone's channel to the Mac: the direct WebSocket over the
// pinned TLS listener, or a relay session sealed end to end. The server's
// per-connection loop runs over it the same either way: Read or the optional
// ReadRecord is called from
// the loop's goroutine only and Write from the connection's single writer
// only, so a relay session seals its records in the order they are written.
type Session interface {
	// Read returns the next message, one JSON object. A frame that is not a
	// message (a binary frame on the direct path) is errNotText.
	Read(ctx context.Context) ([]byte, error)
	Write(ctx context.Context, b []byte) error
	// Close ends the session with a close code and reason after writing last,
	// when last is not nil. It does not block and is safe to call from any
	// goroutine, more than once.
	Close(code websocket.StatusCode, reason string, last []byte)
	Addr() string
}

var (
	errNotText = errors.New("remote: not a text message")
	errGone    = errors.New("remote: relay session ended")
)

// wsSession is the direct path: a WebSocket on the pinned TLS listener.
type wsSession struct {
	ws   *websocket.Conn
	addr string
}

func (w *wsSession) Read(ctx context.Context) ([]byte, error) {
	typ, b, err := w.ws.Read(ctx)
	if err != nil {
		return nil, err
	}
	if typ != websocket.MessageText {
		return nil, errNotText
	}
	return b, nil
}

// ReadRecord keeps the JSON text path and admits kind-prefixed binary frames.
func (w *wsSession) ReadRecord(ctx context.Context) (byte, []byte, error) {
	typ, b, err := w.ws.Read(ctx)
	if err != nil {
		return 0, nil, err
	}
	if typ == websocket.MessageText {
		return e2e.KindJSON, b, nil
	}
	if len(b) == 0 {
		return 0, nil, errNotText
	}
	return b[0], b[1:], nil
}

func (w *wsSession) Write(ctx context.Context, b []byte) error {
	return w.ws.Write(ctx, websocket.MessageText, b)
}

func (w *wsSession) Close(code websocket.StatusCode, reason string, last []byte) {
	go func() {
		if last != nil {
			ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
			_ = w.ws.Write(ctx, websocket.MessageText, last)
			cancel()
		}
		w.ws.Close(code, reason)
	}()
}

func (w *wsSession) Addr() string { return w.addr }

// Ping checks a quiet direct connection; relay sessions are the room's to
// keep alive.
func (w *wsSession) Ping(ctx context.Context) error { return w.ws.Ping(ctx) }

// relaySession is a phone's session through the relay after its handshake:
// records of kind 0x01 both ways and 0x02 audio to the Mac under the session's
// keys (PROTOCOL.md § 7.4).
// A record that does not open tears it down: the room is told drop.
type relaySession struct {
	link Link
	sid  wire.SID
	keys *e2e.Session
	in   chan []byte   // records from the room, in arrival order
	gone chan struct{} // closed when the session ends, whoever ends it
	end  sync.Once

	sealMu sync.Mutex // seal and send as one step, so counters follow the room's order
	closed bool       // no more records go out; guarded by sealMu
}

func newRelaySession(link Link, sid wire.SID, keys *e2e.Session) *relaySession {
	return &relaySession{link: link, sid: sid, keys: keys, in: make(chan []byte, 64), gone: make(chan struct{})}
}

// deliver hands a record from the room to the session without blocking; a
// session that does not keep up is torn down.
func (r *relaySession) deliver(rec []byte) {
	select {
	case <-r.gone:
	case r.in <- rec:
	default:
		r.Close(websocket.StatusTryAgainLater, "too slow", nil)
	}
}

// ended marks the session gone without a word to the room: the room said
// the phone left, or the link went down.
func (r *relaySession) ended() {
	r.sealMu.Lock()
	r.closed = true
	r.sealMu.Unlock()
	r.end.Do(func() { close(r.gone) })
	r.keys.Close()
}

// tear ends a session whose record did not open (PROTOCOL.md § 7.4): no
// more records, and the room is told drop; the phone reconnects and resends
// its pending commands by id.
func (r *relaySession) tear() {
	r.sealMu.Lock()
	wasOpen := !r.closed
	r.closed = true
	r.sealMu.Unlock()
	if wasOpen {
		r.link.Drop(r.sid)
	}
	r.end.Do(func() { close(r.gone) })
	r.keys.Close()
}

func (r *relaySession) Read(ctx context.Context) ([]byte, error) {
	for {
		kind, payload, err := r.ReadRecord(ctx)
		if err != nil || kind == e2e.KindJSON {
			return payload, err
		}
	}
}

// ReadRecord opens audio in the same counter sequence as command records.
func (r *relaySession) ReadRecord(ctx context.Context) (byte, []byte, error) {
	for {
		select {
		case <-r.gone: // ended: a record still queued is not opened
			return 0, nil, errGone
		default:
		}
		select {
		case <-ctx.Done():
			return 0, nil, ctx.Err()
		case <-r.gone:
			return 0, nil, errGone
		case rec := <-r.in:
			kind, payload, err := r.keys.Open(rec)
			if err != nil {
				r.tear()
				return 0, nil, err
			}
			if kind == e2e.KindJSON || kind == StreamKind {
				return kind, payload, nil
			}
			// Reserved kinds are opened, counted, dropped (PROTOCOL.md § 7.4).
		}
	}
}

func (r *relaySession) Write(_ context.Context, b []byte) error {
	r.sealMu.Lock()
	defer r.sealMu.Unlock()
	if r.closed {
		return errGone
	}
	return r.sendLocked(b)
}

func (r *relaySession) sendLocked(b []byte) error {
	rec, err := r.keys.Seal(e2e.KindJSON, b)
	if err != nil {
		return err
	}
	return r.link.Send(r.sid, rec)
}

// Close sends last and a bye record, then drop (PROTOCOL.md § 8): the room
// closes the phone's socket and the phone reads the bye's code.
func (r *relaySession) Close(code websocket.StatusCode, reason string, last []byte) {
	r.sealMu.Lock()
	if !r.closed {
		r.closed = true
		if last != nil {
			_ = r.sendLocked(last)
		}
		_ = r.sendLocked(encode(byeMsg{Type: "bye", Code: int(code), Reason: reason}))
		r.link.Drop(r.sid)
	}
	r.sealMu.Unlock()
	r.end.Do(func() { close(r.gone) })
	r.keys.Close()
}

func (r *relaySession) Addr() string { return "relay " + r.sid.String()[:4] }
