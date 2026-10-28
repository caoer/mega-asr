package remote

import (
	"context"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/caoer/mega-asr/internal/audio"
)

const (
	StreamKind      byte = 0x02
	StreamFormat         = "pcm16le-16k-mono"
	StreamSamples        = 1600
	StreamHeader         = 14 // Includes kind.
	StreamMaxRecord      = StreamHeader + 2*StreamSamples
	StreamStall          = 3 * time.Second
	StreamStopGrace      = time.Second
)

// StreamRecord is plaintext, including kind, on both direct and relay links.
type StreamRecord struct {
	Stream   uint64
	Sequence uint32
	Last     bool
	Samples  []int16
}

func ParseStreamID(s string) (uint64, error) {
	b, err := hex.DecodeString(s)
	if err != nil || len(b) != 8 {
		return 0, errors.New("stream id must be 16 hex digits")
	}
	return binary.BigEndian.Uint64(b), nil
}

func (r StreamRecord) Encode() ([]byte, error) {
	if len(r.Samples) > StreamSamples || (!r.Last && len(r.Samples) != StreamSamples) {
		return nil, errors.New("stream record sample count")
	}
	b := make([]byte, StreamHeader+2*len(r.Samples))
	b[0] = StreamKind
	binary.BigEndian.PutUint64(b[1:9], r.Stream)
	binary.BigEndian.PutUint32(b[9:13], r.Sequence)
	if r.Last {
		b[13] = 1
	}
	for i, s := range r.Samples {
		binary.LittleEndian.PutUint16(b[StreamHeader+2*i:], uint16(s))
	}
	return b, nil
}

func DecodeStreamRecord(b []byte) (StreamRecord, error) {
	var r StreamRecord
	if len(b) < StreamHeader || len(b) > StreamMaxRecord || (len(b)-StreamHeader)%2 != 0 {
		return r, errors.New("stream record size")
	}
	if b[0] != StreamKind {
		return r, errors.New("stream record kind")
	}
	if b[13]&0xfe != 0 {
		return r, errors.New("stream record flags")
	}
	r.Stream = binary.BigEndian.Uint64(b[1:9])
	r.Sequence = binary.BigEndian.Uint32(b[9:13])
	r.Last = b[13] == 1
	n := (len(b) - StreamHeader) / 2
	if !r.Last && n != StreamSamples {
		return StreamRecord{}, errors.New("stream record sample count")
	}
	r.Samples = make([]int16, n)
	for i := range r.Samples {
		r.Samples[i] = int16(binary.LittleEndian.Uint16(b[StreamHeader+2*i:]))
	}
	return r, nil
}

// PhoneStream is a bounded, single-use audio.Source. The session feeds complete
// plaintext records through Feed; registration/routing belongs to U5c.
type PhoneStream struct {
	stream       uint64
	name         string
	mu           sync.Mutex
	started      bool
	ended        bool
	next         uint64 // Allows the final uint32 sequence without wrapping to zero.
	lastSequence *uint32
	err          error
	chunks       chan []int16
	done         chan struct{}
	lastRecordAt time.Time
	stall        *time.Timer
	grace        *time.Timer
}

var _ audio.Source = (*PhoneStream)(nil)
var _ audio.Described = (*PhoneStream)(nil)

func NewPhoneStream(stream uint64, device string) *PhoneStream {
	return &PhoneStream{stream: stream, name: device, chunks: make(chan []int16, 64), done: make(chan struct{})}
}

func (p *PhoneStream) Input() audio.TakeInput { return audio.TakeInput{Source: "phone", Name: p.name} }

func (p *PhoneStream) Start(ctx context.Context) (<-chan []int16, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.started || p.ended {
		return nil, errors.New("phone stream already started or closed")
	}
	if err := ctx.Err(); err != nil {
		p.endLocked(err)
		return nil, err
	}
	p.started = true
	p.lastRecordAt = time.Now()
	p.stall = time.AfterFunc(StreamStall, p.checkStall)
	out := make(chan []int16)
	go p.pump(ctx, out)
	return out, nil
}

func (p *PhoneStream) pump(ctx context.Context, out chan<- []int16) {
	defer close(out)
	send := func(s []int16) bool {
		select {
		case out <- s:
			return true
		case <-ctx.Done():
			p.fail(ctx.Err())
			return false
		}
	}
	for {
		select {
		case s := <-p.chunks:
			if !send(s) {
				return
			}
		case <-p.done:
			// Every accepted block is delivered before a clean or failed end.
			for {
				select {
				case s := <-p.chunks:
					if !send(s) {
						return
					}
				default:
					return
				}
			}
		case <-ctx.Done():
			p.fail(ctx.Err())
			return
		}
	}
}

// Feed refuses malformed records, wrong ids, sequence gaps/repeats and an
// overflowing consumer queue. No session reader blocks behind a slow engine.
func (p *PhoneStream) Feed(b []byte) error {
	r, err := DecodeStreamRecord(b)
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.ended {
		return errors.New("phone stream closed")
	}
	if !p.started {
		return errors.New("phone stream not started")
	}
	if err == nil && r.Stream != p.stream {
		err = errors.New("phone stream id mismatch")
	}
	if err == nil && uint64(r.Sequence) != p.next {
		err = fmt.Errorf("phone stream sequence: got %d, want %d", r.Sequence, p.next)
	}
	if err == nil && p.lastSequence != nil && r.Sequence > *p.lastSequence {
		err = errors.New("phone stream past last_seq")
	}
	if err == nil && r.Last && p.lastSequence != nil && r.Sequence != *p.lastSequence {
		err = errors.New("phone stream last flag disagrees with last_seq")
	}
	if err != nil {
		p.endLocked(err)
		return err
	}
	if len(r.Samples) != 0 {
		select {
		case p.chunks <- r.Samples:
		default:
			err = errors.New("phone stream consumer too slow")
			p.endLocked(err)
			return err
		}
	}
	p.next++
	p.lastRecordAt = time.Now()
	if r.Last || (p.lastSequence != nil && r.Sequence == *p.lastSequence) {
		p.endLocked(nil)
	} else {
		p.stall.Reset(StreamStall)
	}
	return nil
}

// Finish is stop{last_seq}. It allows one second for the named record; the
// last flag can already have closed the source before the stop command arrives.
func (p *PhoneStream) Finish(lastSequence uint32) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.ended {
		if p.err == nil && p.next == uint64(lastSequence)+1 {
			return nil
		}
		return errors.New("phone stream closed before last_seq")
	}
	if !p.started {
		return errors.New("phone stream not started")
	}
	if p.lastSequence != nil {
		if *p.lastSequence == lastSequence {
			return nil
		}
		err := errors.New("phone stream conflicting last_seq")
		p.endLocked(err)
		return err
	}
	if p.next > uint64(lastSequence)+1 {
		err := errors.New("phone stream last_seq precedes received audio")
		p.endLocked(err)
		return err
	}
	if p.next == uint64(lastSequence)+1 {
		p.endLocked(nil)
		return nil
	}
	p.lastSequence = new(uint32)
	*p.lastSequence = lastSequence
	p.grace = time.AfterFunc(StreamStopGrace, func() { p.fail(errors.New("phone stream missing last_seq after 1 s")) })
	return nil
}

// A timer callback may already be queued when Feed resets it. Recheck the
// last accepted record under the same lock before calling the source stalled.
func (p *PhoneStream) checkStall() {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.ended {
		return
	}
	remaining := StreamStall - time.Since(p.lastRecordAt)
	if remaining > 0 {
		p.stall.Reset(remaining)
		return
	}
	p.endLocked(errors.New("phone stream: no audio for 3 s"))
}

func (p *PhoneStream) Stop()          { p.mu.Lock(); defer p.mu.Unlock(); p.endLocked(nil) }
func (p *PhoneStream) Err() error     { p.mu.Lock(); defer p.mu.Unlock(); return p.err }
func (p *PhoneStream) fail(err error) { p.mu.Lock(); defer p.mu.Unlock(); p.endLocked(err) }
func (p *PhoneStream) endLocked(err error) {
	if p.ended {
		return
	}
	p.ended = true
	p.err = err
	if p.stall != nil {
		p.stall.Stop()
	}
	if p.grace != nil {
		p.grace.Stop()
	}
	close(p.done)
}
