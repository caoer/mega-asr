package audio

import (
	"context"
	"errors"
	"math"
	"path/filepath"
	"testing"
	"time"
)

func replay(t *testing.T, samples int) *File {
	t.Helper()
	p := filepath.Join(t.TempDir(), "r.wav")
	s := make([]int16, samples)
	for i := range s {
		s[i] = int16(i)
	}
	if err := SaveWAV(p, s); err != nil {
		t.Fatal(err)
	}
	return &File{Path: p}
}

func TestFileReplaysAtRealTime(t *testing.T) {
	f := replay(t, 10*Block+5) // 205 ms
	t0 := time.Now()
	ch, err := f.Start(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	var got []int16
	for s := range ch {
		got = append(got, s...)
	}
	if d := time.Since(t0); d < 200*time.Millisecond {
		t.Errorf("205 ms replayed in %v", d)
	}
	if len(got) != 10*Block+5 || got[10*Block+4] != int16(10*Block+4) {
		t.Fatalf("got %d samples", len(got))
	}
	if !errors.Is(f.Err(), ErrReplayEnd) {
		t.Fatalf("Err = %v, want ErrReplayEnd", f.Err())
	}
}

func TestFileAtFullSpeed(t *testing.T) {
	f := replay(t, 60*Rate)
	f.Speed = math.Inf(1)
	t0 := time.Now()
	ch, err := f.Start(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	n := 0
	for s := range ch {
		n += len(s)
	}
	if d := time.Since(t0); d > 5*time.Second || n != 60*Rate {
		t.Fatalf("60 s at full speed: %d samples in %v", n, d)
	}
}

func TestFileStop(t *testing.T) {
	f := replay(t, 10*Rate)
	ch, err := f.Start(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	<-ch
	f.Stop()
	f.Stop()
	n := 0
	for range ch {
		n++
	}
	if n > 2 || f.Err() != nil {
		t.Fatalf("after Stop: %d more blocks, Err %v", n, f.Err())
	}
}
