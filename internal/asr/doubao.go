package asr

import (
	"bytes"
	"compress/gzip"
	"context"
	"crypto/rand"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"sync"
	"time"

	"github.com/coder/websocket"

	"github.com/caoer/mega-asr/internal/audio"
)

// Doubao is Doubao streaming ASR 2.0 (Seed-ASR) on the Volcengine Agent
// Plan: one WebSocket per take, the take's 16 kHz PCM sent as it records in
// 200 ms packets, the final text shortly after the last one. Wire:
// https://www.volcengine.com/docs/6561/1354869.
type Doubao struct {
	URL        string                 // wss://openspeech.bytedance.com/api/v3/plan/sauc/bigmodel_async
	ResourceID string                 // volc.seedasr.sauc.duration
	Key        func() (string, error) // the plan key, read at each take; never logged
	TwoPass    bool                   // enable_nonstream: each closed sentence re-decoded by the nostream model
	DDC        bool                   // enable_ddc: fillers and disfluencies dropped
	// Hotwords returns the terms for a take, read at each one as FunASR
	// does; nil means none. They go in request.corpus.context.
	Hotwords func() []string
	// Timeout bounds the connect, and the wait for the final text once a
	// take ends, plus an eighth of the audio still queued then (0: 10 s).
	Timeout time.Duration
}

// A Stream transcribes one take as it records.
type Stream interface {
	// Write queues samples; it never blocks on the network.
	Write(s []int16)
	// Finish sends what is queued as the last packet and waits for the
	// final text.
	Finish(ctx context.Context) (string, error)
	// Abort ends the stream without a result.
	Abort()
}

// Streamer opens a Stream per take. Open returns at once: the connection is
// made in the background while the take's first blocks queue.
type Streamer interface {
	Open() Stream
}

const (
	doubaoPacket  = audio.Rate * 2 / 5 // bytes in 200 ms of 16 kHz s16le
	doubaoTimeout = 10 * time.Second
)

// Frame types and flags of the binary protocol.
const (
	msgFullRequest  = 0b0001
	msgAudio        = 0b0010
	msgFullResponse = 0b1001
	msgError        = 0b1111

	flagSeq  = 0b0001
	flagLast = 0b0010
	flagEvt  = 0b0100

	serialJSON = 1
	serialNone = 0
	compGzip   = 1
)

// frame is one client message: a 4-byte header (version 1, header size 1;
// type and flags; serialization and gzip; reserved), a big-endian sequence
// number and the gzip'd payload's size, then the payload.
func frame(typ, flags, serial byte, seq int32, payload []byte) []byte {
	var z bytes.Buffer
	w := gzip.NewWriter(&z)
	w.Write(payload)
	w.Close()
	b := make([]byte, 12, 12+z.Len())
	b[0], b[1], b[2] = 0x11, typ<<4|flags, serial<<4|compGzip
	binary.BigEndian.PutUint32(b[4:], uint32(seq))
	binary.BigEndian.PutUint32(b[8:], uint32(z.Len()))
	return append(b, z.Bytes()...)
}

// reply is one server message.
type reply struct {
	Last bool
	Code uint32 // an error frame's code: 45000001 bad params, 45000151 bad format, 55000031 busy…
	Body []byte // JSON
}

func parseReply(msg []byte) (reply, error) {
	if len(msg) < 4 {
		return reply{}, fmt.Errorf("doubao: short frame (%d bytes)", len(msg))
	}
	hsize := int(msg[0]&0x0f) * 4
	typ, flags, comp := msg[1]>>4, msg[1]&0x0f, msg[2]&0x0f
	if hsize < 4 || len(msg) < hsize {
		return reply{}, fmt.Errorf("doubao: bad header size %d", hsize)
	}
	p := msg[hsize:]
	r := reply{Last: flags&flagLast != 0}
	take := func(n int) ([]byte, bool) {
		if len(p) < n {
			return nil, false
		}
		b := p[:n]
		p = p[n:]
		return b, true
	}
	ok := true
	if flags&flagSeq != 0 {
		_, ok = take(4)
	}
	if ok && flags&flagEvt != 0 {
		_, ok = take(4)
	}
	if ok && typ == msgError {
		var c []byte
		if c, ok = take(4); ok {
			r.Code = binary.BigEndian.Uint32(c)
		}
	}
	var size []byte
	if ok {
		size, ok = take(4)
	}
	if !ok {
		return reply{}, fmt.Errorf("doubao: frame type %d cut short", typ)
	}
	n := int(binary.BigEndian.Uint32(size))
	if n > len(p) {
		return reply{}, fmt.Errorf("doubao: payload of %d bytes in a frame with %d", n, len(p))
	}
	p = p[:n]
	if comp == compGzip && n > 0 {
		zr, err := gzip.NewReader(bytes.NewReader(p))
		if err != nil {
			return reply{}, fmt.Errorf("doubao: gzip: %w", err)
		}
		if p, err = io.ReadAll(zr); err != nil {
			return reply{}, fmt.Errorf("doubao: gzip: %w", err)
		}
	}
	r.Body = p
	return r, nil
}

// result is the part of a response body a take reads: the text, and with
// show_utterances each sentence's words and their times.
type result struct {
	Result struct {
		Text       string `json:"text"`
		Utterances []struct {
			Words []struct {
				Text      string `json:"text"`
				StartTime int64  `json:"start_time"`
				EndTime   int64  `json:"end_time"`
			} `json:"words"`
		} `json:"utterances"`
	} `json:"result"`
	Error string `json:"error"`
}

// Word is a word of a take's text and when it was said, in ms from the
// start of the take's audio.
type Word struct {
	Text    string `json:"text"`
	StartMS int64  `json:"start_ms"`
	EndMS   int64  `json:"end_ms"`
}

// Timed is a Stream that also says when each word of its text was said,
// once Finish has returned the text; nil when the engine gave no times.
type Timed interface {
	Words() []Word
}

// words is the words of every utterance of a reply, in order; one without
// a time (a negative one, or an end before its start) is left out.
func (b result) words() []Word {
	var out []Word
	for _, u := range b.Result.Utterances {
		for _, w := range u.Words {
			if w.Text != "" && w.StartTime >= 0 && w.EndTime >= w.StartTime {
				out = append(out, Word{Text: w.Text, StartMS: w.StartTime, EndMS: w.EndTime})
			}
		}
	}
	return out
}

// ServerError is an error frame.
type ServerError struct {
	Code uint32
	Msg  string
}

func (e ServerError) Error() string { return fmt.Sprintf("doubao: %d: %s", e.Code, e.Msg) }

func (r reply) err() error {
	var b result
	json.Unmarshal(r.Body, &b)
	msg := b.Error
	if msg == "" {
		msg = string(r.Body)
	}
	return ServerError{r.Code, msg}
}

func (d *Doubao) request() []byte {
	req := map[string]any{
		"user":  map[string]any{"uid": "megavoice"},
		"audio": map[string]any{"format": "pcm", "codec": "raw", "rate": audio.Rate, "bits": 16, "channel": 1},
		"request": map[string]any{
			"model_name": "bigmodel", "enable_itn": true, "enable_punc": true, "enable_ddc": d.DDC,
			"show_utterances": true, "result_type": "full", "enable_nonstream": d.TwoPass,
		},
	}
	if d.Hotwords != nil {
		if hw := d.Hotwords(); len(hw) > 0 {
			words := make([]map[string]string, len(hw))
			for i, w := range hw {
				words[i] = map[string]string{"word": w}
			}
			ctx, _ := json.Marshal(map[string]any{"hotwords": words})
			req["request"].(map[string]any)["corpus"] = map[string]any{"context": string(ctx)}
		}
	}
	b, _ := json.Marshal(req)
	return b
}

func (d *Doubao) timeout() time.Duration {
	if d.Timeout > 0 {
		return d.Timeout
	}
	return doubaoTimeout
}

func (d *Doubao) Open() Stream {
	ctx, cancel := context.WithCancel(context.Background())
	s := &doubaoStream{d: d, ctx: ctx, cancel: cancel, wake: make(chan struct{}, 1), final: make(chan struct{})}
	go s.run()
	return s
}

// Transcribe sends a WAV unpaced (the async endpoint reads faster than real
// time) and returns its text; whole is ignored: the server segments.
func (d *Doubao) Transcribe(ctx context.Context, wav string, whole bool) (string, error) {
	s, err := audio.ReadWAV(wav)
	if err != nil {
		return "", err
	}
	st := d.Open()
	st.Write(s)
	return st.Finish(ctx)
}

// doubaoStream is one take's connection. run dials, sends the request and
// then the queued audio; read takes the server's frames.
type doubaoStream struct {
	d      *Doubao
	ctx    context.Context
	cancel context.CancelFunc
	wake   chan struct{}

	mu       sync.Mutex
	pcm      []byte // queued, not yet sent
	finished bool   // Finish was called: what is queued ends the take
	err      error
	text     string
	words    []Word // the text's, from the same reply
	logid    string
	final    chan struct{} // closed on the last-package frame or a failure
	closed   bool
}

func (s *doubaoStream) Write(samples []int16) {
	s.mu.Lock()
	for _, v := range samples {
		s.pcm = binary.LittleEndian.AppendUint16(s.pcm, uint16(v))
	}
	s.mu.Unlock()
	s.poke()
}

func (s *doubaoStream) poke() {
	select {
	case s.wake <- struct{}{}:
	default:
	}
}

// fail records the first failure and ends the wait for the final text.
func (s *doubaoStream) fail(err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.err == nil && !s.closed {
		s.err = err
	}
	s.closeLocked()
}

func (s *doubaoStream) closeLocked() {
	if !s.closed {
		s.closed = true
		close(s.final)
	}
}

func (s *doubaoStream) run() {
	key, err := s.d.Key()
	if err != nil {
		s.fail(fmt.Errorf("doubao: key: %w", err))
		return
	}
	id := newID()
	h := http.Header{}
	h.Set("X-Api-Key", key)
	h.Set("X-Api-Resource-Id", s.d.ResourceID)
	h.Set("X-Api-Request-Id", id)
	h.Set("X-Api-Connect-Id", id)
	dctx, cancel := context.WithTimeout(s.ctx, s.d.timeout())
	c, resp, err := websocket.Dial(dctx, s.d.URL, &websocket.DialOptions{HTTPHeader: h})
	cancel()
	if err != nil {
		if resp != nil {
			err = fmt.Errorf("%w (HTTP %s, logid %s)", err, resp.Status, resp.Header.Get("X-Tt-Logid"))
		}
		s.fail(fmt.Errorf("doubao: connect: %w", err))
		return
	}
	c.SetReadLimit(-1)
	defer c.CloseNow()
	s.mu.Lock()
	s.logid = resp.Header.Get("X-Tt-Logid")
	s.mu.Unlock()
	if err := c.Write(s.ctx, websocket.MessageBinary, frame(msgFullRequest, flagSeq, serialJSON, 1, s.d.request())); err != nil {
		s.fail(fmt.Errorf("doubao: request: %w", err))
		return
	}
	go s.read(c)
	seq := int32(1)
	send := func(flags byte, seq int32, p []byte) bool {
		if err := c.Write(s.ctx, websocket.MessageBinary, frame(msgAudio, flags, serialNone, seq, p)); err != nil {
			s.fail(fmt.Errorf("doubao: audio: %w", err))
			return false
		}
		return true
	}
	for {
		// Full packets go as they fill; the tail is held back, so the last
		// packet always carries audio.
		s.mu.Lock()
		var packets [][]byte
		for len(s.pcm) > doubaoPacket {
			packets = append(packets, s.pcm[:doubaoPacket])
			s.pcm = s.pcm[doubaoPacket:]
		}
		last := s.finished
		var tail []byte
		if last {
			tail, s.pcm = s.pcm, nil
		}
		s.mu.Unlock()
		for _, p := range packets {
			seq++
			if !send(flagSeq, seq, p) {
				return
			}
		}
		if last {
			seq++
			if send(flagSeq|flagLast, -seq, tail) {
				<-s.final // hold the connection until the answer
			}
			return
		}
		select {
		case <-s.wake:
		case <-s.final: // the server failed the take
			return
		}
	}
}

func (s *doubaoStream) read(c *websocket.Conn) {
	for {
		_, msg, err := c.Read(s.ctx)
		if err != nil {
			s.fail(fmt.Errorf("doubao: %w", err))
			return
		}
		r, err := parseReply(msg)
		if err != nil {
			s.fail(err)
			return
		}
		if r.Code != 0 {
			s.fail(r.err())
			return
		}
		var b result
		if err := json.Unmarshal(r.Body, &b); err != nil {
			s.fail(fmt.Errorf("doubao: response: %w", err))
			return
		}
		s.mu.Lock()
		if b.Result.Text != "" || r.Last {
			s.text, s.words = b.Result.Text, b.words()
		}
		if r.Last {
			s.closeLocked()
			s.mu.Unlock()
			c.Close(websocket.StatusNormalClosure, "")
			return
		}
		s.mu.Unlock()
	}
}

func (s *doubaoStream) Finish(ctx context.Context) (string, error) {
	s.mu.Lock()
	s.finished = true
	queued := time.Duration(len(s.pcm)) * time.Second / (2 * audio.Rate)
	s.mu.Unlock()
	s.poke()
	defer s.cancel()
	// The wait covers a connect still under way, the audio still queued (the
	// server reads it faster than real time) and the last answer.
	wait := s.d.timeout() + queued/8
	timer := time.NewTimer(wait)
	defer timer.Stop()
	select {
	case <-s.final:
	case <-timer.C:
		s.fail(fmt.Errorf("doubao: no final text within %v of the take's end", wait.Round(time.Millisecond)))
	case <-ctx.Done():
		s.fail(ctx.Err())
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.err != nil {
		if s.logid != "" {
			return "", fmt.Errorf("%w (logid %s)", s.err, s.logid)
		}
		return "", s.err
	}
	return s.text, nil
}

// Words is Timed: the words of the text Finish returned, nil after a
// failure.
func (s *doubaoStream) Words() []Word {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.err != nil {
		return nil
	}
	return s.words
}

func (s *doubaoStream) Abort() {
	s.fail(errors.New("doubao: aborted"))
	s.cancel()
}

// newID is a random UUID (v4), the request's and the connection's id.
func newID() string {
	var b [16]byte
	rand.Read(b[:])
	b[6], b[8] = b[6]&0x0f|0x40, b[8]&0x3f|0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:])
}
