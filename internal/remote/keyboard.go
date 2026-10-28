package remote

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"github.com/caoer/mega-asr/internal/audio"
	"github.com/caoer/mega-asr/internal/l10n"
	"github.com/caoer/mega-asr/internal/relay/e2e"
	"github.com/caoer/mega-asr/internal/relay/wire"
)

// TextSink returns a phone's own transcript to the session that supplied it.
// Success means the writer sent the message; there is no phone adoption ACK.
type TextSink interface {
	DeliverText(take, text string) error
}

// StreamTakes is additive to Takes: hosts without it refuse phone streams.
type StreamTakes interface {
	StartStream(src audio.Source, sink TextSink, via string) (string, error)
}

type phoneTake struct {
	src  *PhoneStream
	take string
}

type textDelivery struct {
	message []byte
	done    chan error
}

// MaxTextMessage leaves room for the relay's kind byte and AEAD tag, so
// direct and relay sinks accept the same complete encoded JSON object.
const MaxTextMessage = wire.MaxRecord - 1 - 16

// textMsg is a stream take's text, sent only to the session that streamed it.
type textMsg struct {
	Type string `json:"type"` // text
	Take string `json:"take"`
	Text string `json:"text"`
}

func (c *conn) DeliverText(take, text string) error {
	b := encode(textMsg{"text", take, text})
	if len(b) > MaxTextMessage {
		return errors.New("phone text exceeds one relay record")
	}
	select {
	case <-c.done:
		return errGone
	default:
	}
	d := textDelivery{message: b, done: make(chan error, 1)}
	timer := time.NewTimer(6 * time.Second)
	defer timer.Stop()
	select {
	case <-c.done:
		return errGone
	case c.out <- d:
	case <-timer.C:
		c.close(1013, "text delivery timeout", nil)
		return errors.New("phone text delivery timeout")
	}
	select {
	case err := <-d.done:
		return err
	case <-c.done:
		return errGone
	case <-timer.C:
		c.close(1013, "text delivery timeout", nil)
		return errors.New("phone text delivery timeout")
	}
}

// readRecord preserves Session's JSON API for existing hosts and test doubles.
func readRecord(ctx context.Context, sess Session) (byte, []byte, error) {
	if r, ok := sess.(interface {
		ReadRecord(context.Context) (byte, []byte, error)
	}); ok {
		return r.ReadRecord(ctx)
	}
	b, err := sess.Read(ctx)
	return e2e.KindJSON, b, err
}

func decodeMsg(b []byte) (inMsg, error) {
	var m inMsg
	if err := json.Unmarshal(b, &m); err != nil {
		return m, &json.SyntaxError{}
	}
	return m, nil
}

func (s *Server) startStream(c *conn, m inMsg, via string) error {
	if c == nil || via != "keyboard" || m.Format != StreamFormat {
		return Refuse("bad_request", l10n.T("键盘音频需要 phone session、via keyboard 和 pcm16le-16k-mono", "Keyboard audio needs a phone session, via keyboard and pcm16le-16k-mono"))
	}
	id, err := ParseStreamID(m.Stream)
	if err != nil {
		return Refuse("bad_request", err.Error())
	}
	host, ok := s.cfg.Takes.(StreamTakes)
	if !ok {
		return Refuse("start_failed", l10n.T("Mac 不支持键盘音频", "This Mac doesn't take keyboard audio"))
	}
	c.streamMu.Lock()
	defer c.streamMu.Unlock()
	select {
	case <-c.done:
		return Refuse("start_failed", l10n.T("iPhone 连接已断开", "The iPhone disconnected"))
	default:
	}
	src := NewPhoneStream(id, c.name())
	take, err := host.StartStream(src, c, via)
	if err != nil {
		src.Stop()
		return err
	}
	if c.stream != nil {
		c.stream.src.Stop()
	}
	c.stream = &phoneTake{src: src, take: take}
	return nil
}

func (c *conn) feedStream(payload []byte) {
	c.streamMu.Lock()
	defer c.streamMu.Unlock()
	if c.stream != nil {
		_ = c.stream.src.Feed(append([]byte{StreamKind}, payload...))
	}
}

func (c *conn) hasStream(take string) bool {
	c.streamMu.Lock()
	defer c.streamMu.Unlock()
	return c.stream != nil && c.stream.take == take
}

// streamOwner checks the named take across connections, including the phone
// connection shared by watch commands. Ordinary remote stops must not bypass
// a keyboard stream's tail check by reaching the global Takes.Stop.
func (s *Server) streamOwner(take string) *conn {
	s.mu.Lock()
	conns := make([]*conn, 0, len(s.conns))
	for c := range s.conns {
		conns = append(conns, c)
	}
	s.mu.Unlock()
	for _, c := range conns {
		if c.hasStream(take) {
			return c
		}
	}
	return nil
}

// samePhone reports whether b is a's connection or another one of a's phone.
func samePhone(a, b *conn) bool {
	return a == b || b != nil && a.phone != nil && a.phone == b.phone
}

func (c *conn) finishStream(m inMsg) error {
	id, err := ParseStreamID(m.Stream)
	if err != nil || m.LastSeq == nil || (m.End != "" && m.End != "paste") {
		return Refuse("bad_request", l10n.T("键盘停止需要 stream、last_seq，且不能发送 Enter", "A keyboard stop needs stream and last_seq, and can't send Enter"))
	}
	c.streamMu.Lock()
	defer c.streamMu.Unlock()
	if c.stream == nil || c.stream.take != m.Take || c.stream.src.stream != id {
		return Refuse("other_take", l10n.T("这次没有结束当前键盘录音", "The current keyboard recording was not ended"))
	}
	if err := c.stream.src.Finish(*m.LastSeq); err != nil {
		return Refuse("bad_request", err.Error())
	}
	return nil
}
