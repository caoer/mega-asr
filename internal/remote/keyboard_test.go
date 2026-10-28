package remote

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/caoer/mega-asr/internal/audio"
	"github.com/caoer/mega-asr/internal/relay/e2e"
	"github.com/coder/websocket"
)

type streamHost struct {
	*fakeTakes
	got chan []int16
	s   *Server
}

func (f *streamHost) StartStream(src audio.Source, sink TextSink, via string) (string, error) {
	id, err := f.Start(via)
	if err != nil {
		return "", err
	}
	ch, err := src.Start(context.Background())
	if err != nil {
		return "", err
	}
	go func() {
		var samples []int16
		for s := range ch {
			samples = append(samples, s...)
		}
		_ = f.Stop(via, id, false)
		final, why := "pasted", ""
		if err := src.Err(); err != nil {
			final, why = "undelivered", err.Error()
		} else if err := sink.DeliverText(id, "fixture text"); err != nil {
			final, why = "undelivered", err.Error()
		}
		f.s.Done(id, final, why)
		f.got <- samples
	}()
	return id, nil
}

func streamStart(id string) map[string]any {
	m := cmd(id, "start", "", time.Now())
	m["via"], m["source"], m["stream"], m["format"] = "keyboard", "stream", "0123456789abcdef", StreamFormat
	return m
}

func pairStreamPhone(t *testing.T, s *Server, name string) *websocket.Conn {
	t.Helper()
	p, err := s.NewPairing()
	if err != nil {
		t.Fatal(err)
	}
	c, err := dial(t, s, s.Fingerprint())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { c.CloseNow() })
	send(t, c, map[string]any{"type": "pair", "secret": secretOf(t, p).Get("s"), "device": name})
	recv(t, c, "paired")
	recv(t, c, "state")
	return c
}

func writeStream(t *testing.T, c *websocket.Conn, record StreamRecord) {
	t.Helper()
	b, err := record.Encode()
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := c.Write(ctx, websocket.MessageBinary, b); err != nil {
		t.Fatal(err)
	}
}

func TestDirectStreamRecordsAndSessionIsolation(t *testing.T) {
	f := &streamHost{fakeTakes: &fakeTakes{}, got: make(chan []int16, 1)}
	s := upWith(t, f)
	f.s = s
	one, two := pairStreamPhone(t, s, "one"), pairStreamPhone(t, s, "two")
	// No registered stream: even malformed audio is dropped, and JSON still works.
	writeStream(t, two, StreamRecord{Last: true})
	send(t, two, cmd("state", "state", "", time.Now()))
	if r := recv(t, two, "reply"); r["ok"] != true {
		t.Fatal(r)
	}
	start := streamStart("s1")
	send(t, one, start)
	// Frames may follow start before its reply has been read.
	writeStream(t, one, StreamRecord{Stream: 0x0123456789abcdef, Samples: fullStreamSamples()})
	writeStream(t, one, StreamRecord{Stream: 0x0123456789abcdef, Sequence: 1, Last: true, Samples: []int16{123}})
	r := recv(t, one, "reply")
	if r["ok"] != true || r["take"] != "take1" {
		t.Fatal(r)
	}
	if m := recv(t, one, "text"); m["take"] != "take1" || m["text"] != "fixture text" {
		t.Fatal(m)
	}
	if m := recv(t, two, "take"); m["take"] != "take1" || m["final"] != "pasted" {
		t.Fatal(m)
	}
	// A state reply is a barrier after the final: no text reached the other phone.
	send(t, two, cmd("barrier", "state", "", time.Now()))
	if m := recv(t, two, "reply"); m["id"] != "barrier" {
		t.Fatal(m)
	}
	select {
	case samples := <-f.got:
		if len(samples) != 1601 || samples[1600] != 123 {
			t.Fatalf("samples %d", len(samples))
		}
	case <-time.After(time.Second):
		t.Fatal("source did not drain")
	}
	send(t, one, start)
	if r := recv(t, one, "reply"); r["take"] != "take1" || f.n != 1 {
		t.Fatalf("duplicate %v", r)
	}
}

func TestDirectBinaryFrameLimit1009(t *testing.T) {
	s, _ := up(t)
	c := pairStreamPhone(t, s, "one")
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	_ = c.Write(ctx, websocket.MessageBinary, make([]byte, MaxFrame+1))
	_, _, err := c.Read(ctx)
	if websocket.CloseStatus(err) != websocket.StatusMessageTooBig {
		t.Fatalf("close = %v", err)
	}
}

func TestRelayStreamRecordsUseSessionCounters(t *testing.T) {
	f := &streamHost{fakeTakes: &fakeTakes{}, got: make(chan []int16, 1)}
	s := upWith(t, f)
	f.s = s
	l, id := withStubRelay(t, s)
	r := &stubRoom{t: t, s: s, f: f.fakeTakes, l: l, id: id}
	sk, _ := r.phone("one")
	_, sess, _ := r.handshake(sidOf(1), sk, `{"type":"hello","device":"one"}`)
	r.send(sidOf(1), sess, streamStart("s1"))
	b, _ := (StreamRecord{Stream: 0x0123456789abcdef, Last: true, Samples: []int16{1000}}).Encode()
	rec, err := sess.Seal(e2e.KindAudio, b[1:])
	if err != nil {
		t.Fatal(err)
	}
	s.Frame(sidOf(1), rec)
	msgs := records(t, r.waitSent(sidOf(1), 4)[1:], sess)
	var replyOK, textOK, takeOK bool
	for _, m := range msgs {
		switch m["type"] {
		case "reply":
			replyOK = m["ok"] == true && m["take"] == "take1"
		case "text":
			textOK = m["text"] == "fixture text"
		case "take":
			takeOK = m["final"] == "pasted"
		}
	}
	if !replyOK || !textOK || !takeOK {
		t.Fatalf("records %v", msgs)
	}
	select {
	case <-f.got:
	case <-time.After(time.Second):
		t.Fatal("source did not drain")
	}
}

func TestStreamStartValidationAndStopBinding(t *testing.T) {
	f := &streamHost{fakeTakes: &fakeTakes{}, got: make(chan []int16, 1)}
	s := upWith(t, f)
	f.s = s
	c := newConn(&wsSession{}, false)
	for _, field := range []string{"stream", "format", "via"} {
		m, _ := decodeMsg(encode(streamStart("bad")))
		switch field {
		case "stream":
			m.Stream = "no"
		case "format":
			m.Format = "other"
		case "via":
			m.Via = "watch"
		}
		if err := s.actFor(c, m); err == nil {
			t.Fatalf("accepted bad %s", field)
		}
	}
	m, _ := decodeMsg(encode(streamStart("good")))
	if err := s.actFor(c, m); err != nil {
		t.Fatal(err)
	}
	last := uint32(0)
	for _, stop := range []inMsg{
		{Take: "other", Stream: m.Stream, LastSeq: &last},
		{Take: "take1", Stream: "ffffffffffffffff", LastSeq: &last},
		{Take: "take1", Stream: m.Stream},
	} {
		if err := c.finishStream(stop); err == nil {
			t.Fatalf("accepted %+v", stop)
		}
	}
	// A gap sets the source error; it cannot return success as a completed take.
	c.stream.src.fail(errors.New("test cleanup"))
	select {
	case <-f.got:
	case <-time.After(time.Second):
		t.Fatal("source did not drain")
	}
}

func TestPhoneTextGoneAndOversize(t *testing.T) {
	c := newConn(&wsSession{}, false)
	if err := c.DeliverText("take1", strings.Repeat("字", MaxTextMessage)); err == nil {
		t.Fatal("oversize text accepted")
	}
	close(c.done)
	if err := c.DeliverText("take1", "hello"); !errors.Is(err, errGone) {
		t.Fatalf("gone: %v", err)
	}
}
