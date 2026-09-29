package audio

import (
	"math"
	"testing"
	"time"
)

// signal builds blocks from a pattern of (kind, blocks): 's' a −15 dBFS tone,
// 'q' a −70 dBFS hiss, 'd' a −50 dBFS soft block.
func signal(pattern ...any) []int16 {
	var s []int16
	for i := 0; i < len(pattern); i += 2 {
		kind, n := pattern[i].(rune), pattern[i+1].(int)
		for range n * Block {
			k := len(s)
			switch kind {
			case 's':
				s = append(s, int16(8000*math.Sin(2*math.Pi*440*float64(k)/Rate)))
			case 'q':
				s = append(s, int16(10*(1-2*(k%2))))
			case 'd':
				s = append(s, int16(100*(1-2*(k%2))))
			}
		}
	}
	return s
}

// feed pushes s in 20 ms blocks.
func feed(c *Chunker, s []int16) []Cut {
	var cuts []Cut
	for i := 0; i < len(s); i += Block {
		cuts = append(cuts, c.Push(s[i:min(i+Block, len(s))])...)
	}
	return cuts
}

func ends(cuts []Cut) []int {
	var e []int
	for _, c := range cuts {
		e = append(e, c.End/Block)
	}
	return e
}

func equal(a, b []int) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func TestChunkerPauses(t *testing.T) {
	s := signal('q', 50, 's', 250, 'q', 100, 's', 25, 'q', 100, 's', 150, 'q', 100, 's', 50)
	cuts := feed(&Chunker{}, s)
	// pause 1: quiet from block 300, reaches 95 blocks at 394 → cut at its
	// midpoint, 394-47+1. Pause 2 (425–524) would leave a 2.5 s chunk: no cut.
	// Pause 3: quiet from 675, 95 blocks at 769 → 723.
	if got, want := ends(cuts), []int{348, 723}; !equal(got, want) {
		t.Fatalf("cuts at blocks %v, want %v (%v)", got, want, cuts)
	}
	if cuts[0].Reason != "pause 1900 ms" {
		t.Errorf("reason %q", cuts[0].Reason)
	}
}

func TestChunkerMax(t *testing.T) {
	// 40 s of speech with a soft block every 5th (so no quiet run reaches a
	// pause) and one hiss block at 1434 (28.7 s), the quietest of the last
	// 3 s before 30 s
	var p []any
	for b := 0; b < 2000; b += 5 {
		if b == 1430 {
			p = append(p, 's', 4, 'q', 1)
			continue
		}
		p = append(p, 's', 4, 'd', 1)
	}
	cuts := feed(&Chunker{}, signal(p...))
	if len(cuts) == 0 {
		t.Fatal("no cut in 40 s without a pause")
	}
	if got := cuts[0].End / Block; got != 1435 {
		t.Fatalf("first cut at block %d (%s), want 1435 — after the dip at 1434", got, cuts[0].Reason)
	}
	for i, c := range cuts {
		prev := 0
		if i > 0 {
			prev = cuts[i-1].End
		}
		if c.End-prev > 30*Rate {
			t.Errorf("chunk %d is %v long", i, time.Duration(c.End-prev)*time.Second/Rate)
		}
	}
}

// testdata/take-30s.wav (30 s of read Mandarin, LibriVox, public domain) cut
// at 800 ms pauses ends its chunks at 8.08, 13.02, 18.94 and 24.80 s: each
// inside a pause ffmpeg's silencedetect (-40 dB, 0.5 s) finds, at 7.63–8.53,
// 12.59–13.58, 18.49–19.38 and 24.36–25.37 s.
func TestChunkerBaseline(t *testing.T) {
	s, err := ReadWAV("testdata/take-30s.wav")
	if err != nil {
		t.Fatal(err)
	}
	cuts := feed(&Chunker{Pause: 800 * time.Millisecond}, s)
	want := []int{404, 651, 947, 1240} // seconds × 50
	if got := ends(cuts); !equal(got, want) {
		t.Fatalf("cuts at blocks %v, want %v", got, want)
	}
}

// Push accepts any length: the cuts do not depend on how the stream arrives.
func TestChunkerAnyLength(t *testing.T) {
	s, err := ReadWAV("testdata/take-30s.wav")
	if err != nil {
		t.Fatal(err)
	}
	c := Chunker{Pause: 800 * time.Millisecond}
	var cuts []Cut
	for i := 0; i < len(s); i += 1234 {
		cuts = append(cuts, c.Push(s[i:min(i+1234, len(s))])...)
	}
	if got, want := ends(cuts), []int{404, 651, 947, 1240}; !equal(got, want) {
		t.Fatalf("cuts at blocks %v, want %v", got, want)
	}
}
