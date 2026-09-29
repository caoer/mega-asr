package audio

import (
	"math"
	"testing"
)

func tone(rate, hz float64, n int, amp float64) []float32 {
	s := make([]float32, n)
	for i := range s {
		s[i] = float32(amp * math.Sin(2*math.Pi*hz*float64(i)/rate))
	}
	return s
}

// resampleAll feeds in in odd-sized pieces, as an IOProc would.
func resampleAll(r *resampler, in []float32) []int16 {
	var out []int16
	for i := 0; i < len(in); i += 509 {
		out = append(out, r.push(in[i:min(i+509, len(in))])...)
	}
	return out
}

func TestResampleKeepsPassband(t *testing.T) {
	for _, rate := range []float64{48000, 44100, 16000} {
		out := resampleAll(newResampler(rate), tone(rate, 1000, int(rate), 0.5))
		if d := len(out) - Rate; d > 0 || d < -64 {
			t.Errorf("%v Hz: %d samples out of 1 s, want ≈ %d", rate, len(out), Rate)
		}
		got, _ := Stats(out[Rate/10 : len(out)-Rate/10])
		if want := DBFS(0.5 / math.Sqrt2); math.Abs(got-want) > 0.1 {
			t.Errorf("%v Hz: 1 kHz at %.2f dBFS, want %.2f", rate, got, want)
		}
	}
}

func TestResampleRejectsAboveNyquist(t *testing.T) {
	out := resampleAll(newResampler(48000), tone(48000, 12000, 48000, 0.5))
	if db, _ := Stats(out[Rate/10 : len(out)-Rate/10]); db > -70 {
		t.Errorf("12 kHz tone aliases at %.1f dBFS, want below −70", db)
	}
}

func TestResampleTwoTracksStayAligned(t *testing.T) {
	a, b := newResampler(48000), newResampler(48000)
	var na, nb int
	for i, n := range []int{512, 512, 471, 1, 4096, 333} {
		na += len(a.push(make([]float32, n)))
		nb += len(b.push(tone(48000, 440, n, 0.3)))
		if na != nb {
			t.Fatalf("after push %d: %d vs %d samples", i, na, nb)
		}
	}
}
