package audio

import (
	"math"
	"testing"
)

func TestLevelsQuality(t *testing.T) {
	l := NewLevels(200, FloorBlocks)
	// 1 s hiss, 2 s speech; the 2 s of speech are the ring's last 100 blocks
	s := signal('q', 50, 's', 100)
	l.Add(s[:len(s)-Block+7]) // a partial block waits for the rest
	l.Add(s[len(s)-Block+7:])
	q := l.Quality()
	if len(q) != 150 {
		t.Fatalf("%d blocks, want 150", len(q))
	}
	for i := range 50 {
		if !math.IsNaN(q[i].SNR) || !math.IsNaN(q[i].Floor) {
			t.Fatalf("block %d: floor %v SNR %v, want NaN in the first second", i, q[i].Floor, q[i].SNR)
		}
	}
	last := q[len(q)-1]
	hiss := DBFS(10.0 / 32768)
	if math.Abs(last.Floor-hiss) > 0.1 || math.Abs(last.Level-DBFS(8000/32768.0/math.Sqrt2)) > 0.5 ||
		math.Abs(last.SNR-(last.Level-last.Floor)) > 1e-9 || last.SNR < 50 || last.Clipped {
		t.Fatalf("speech block %+v, want floor %.1f dBFS and SNR > 50 dB", last, hiss)
	}
	if lv := l.Snapshot(); len(lv) != 150 || lv[149] != last.Level {
		t.Fatalf("Snapshot disagrees with Quality")
	}
}

func TestLevelsClipped(t *testing.T) {
	l := NewLevels(4, FloorBlocks)
	s := make([]int16, 3*Block)
	s[Block+17] = -32768
	s[2*Block+3] = 32700
	l.Add(s)
	q := l.Quality()
	if q[0].Clipped || !q[1].Clipped || !q[2].Clipped {
		t.Fatalf("clipped flags %v %v %v, want false true true", q[0].Clipped, q[1].Clipped, q[2].Clipped)
	}
}

func TestLevelsRing(t *testing.T) {
	l := NewLevels(4, FloorBlocks)
	l.Add(signal('q', 3, 's', 3))
	if lv := l.Snapshot(); len(lv) != 4 || lv[0] > -60 || lv[1] < -20 || lv[3] < -20 {
		t.Fatalf("ring %v, want the last 4 blocks: hiss then 3 speech", lv)
	}
}
