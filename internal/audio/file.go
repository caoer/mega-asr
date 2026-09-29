package audio

import (
	"context"
	"errors"
	"sync"
	"time"
)

// ErrReplayEnd is a File's Err when the WAV ran out before Stop.
var ErrReplayEnd = errors.New("replay ended")

// File replays a 16 kHz mono WAV as a Source, one 20 ms block per 20 ms of
// wall time, so a kept take reproduces a recording without a speaker. Stop
// ends it; reaching the end of the file ends it too, with ErrReplayEnd.
type File struct {
	Path string
	// Speed is the replay rate: 0 or 1 is real time, 2 twice as fast, and
	// math.Inf(1) sends each block as soon as the reader takes the last one.
	Speed float64

	stop    chan struct{}
	stopped sync.Once
	mu      sync.Mutex
	err     error
}

// Input names the WAV replayed; the take it came from keeps its own record.
func (f *File) Input() TakeInput { return TakeInput{Source: "file", File: f.Path} }

func (f *File) Start(ctx context.Context) (<-chan []int16, error) {
	s, err := ReadWAV(f.Path)
	if err != nil {
		return nil, err
	}
	f.stop = make(chan struct{})
	ch := make(chan []int16, 256)
	go func() {
		defer close(ch)
		f.setErr(f.play(ctx, s, ch))
	}()
	return ch, nil
}

// play sends s block by block at Speed; nil when stopped.
func (f *File) play(ctx context.Context, s []int16, ch chan<- []int16) error {
	every := BlockDuration
	if f.Speed > 0 {
		every = time.Duration(float64(BlockDuration) / f.Speed)
	}
	var tick <-chan time.Time
	if every > 0 {
		t := time.NewTicker(every)
		defer t.Stop()
		tick = t.C
	}
	for i := 0; i < len(s); i += Block {
		select {
		case ch <- s[i:min(i+Block, len(s))]:
		case <-f.stop:
			return nil
		case <-ctx.Done():
			return ctx.Err()
		}
		if tick == nil {
			continue
		}
		select {
		case <-tick:
		case <-f.stop:
			return nil
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	return ErrReplayEnd
}

func (f *File) setErr(err error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.err = err
}

func (f *File) Stop() {
	if f.stop != nil {
		f.stopped.Do(func() { close(f.stop) })
	}
}

func (f *File) Err() error {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.err
}
