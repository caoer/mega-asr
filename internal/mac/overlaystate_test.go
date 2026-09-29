package mac

import (
	"math"
	"testing"
	"time"

	"github.com/caoer/mega-asr/internal/audio"
)

func TestColourFor(t *testing.T) {
	nan := math.NaN()
	for _, c := range []struct {
		name string
		q    audio.Quality
		want int
	}{
		{"speech well above the floor", audio.Quality{Level: -20, Floor: -50, SNR: 30}, colGood},
		{"at the good threshold", audio.Quality{SNR: 18}, colGood},
		{"just under good", audio.Quality{SNR: 17.9}, colWeak},
		{"at the weak threshold", audio.Quality{SNR: 10}, colWeak},
		{"just under weak", audio.Quality{SNR: 9.9}, colQuiet},
		{"at the floor", audio.Quality{Level: -62, Floor: -62, SNR: 0}, colQuiet},
		{"below the floor", audio.Quality{SNR: -3}, colQuiet},
		{"floor not known yet", audio.Quality{Level: -30, Floor: nan, SNR: nan}, colGood},
		{"clipped loud block", audio.Quality{Level: -1, SNR: 44, Clipped: true}, colClipped},
		{"clipped wins over quiet", audio.Quality{SNR: 2, Clipped: true}, colClipped},
		{"clipped with no floor", audio.Quality{SNR: nan, Clipped: true}, colClipped},
	} {
		if got := colourFor(c.q); got != c.want {
			t.Errorf("%s: colourFor(%+v) = %d, want %d", c.name, c.q, got, c.want)
		}
	}
}

func TestBarsFor(t *testing.T) {
	q := []audio.Quality{
		{Level: -50, SNR: 0},                // bar 0, alone
		{Level: -20, SNR: 25},               // bar 1
		{Level: -30, SNR: 15},               // bar 1: quieter than its pair
		{Level: -40, SNR: 5},                // bar 2
		{Level: -45, SNR: 0, Clipped: true}, // bar 2: clipped, quieter
	}
	got := barsFor(q, 3, -60)
	want := []bar{
		{-50, colQuiet},   // blocks -1 (missing) and 0
		{-20, colGood},    // blocks 1, 2: the louder one's SNR
		{-40, colClipped}, // blocks 3, 4: the louder one's level, red for the clip
	}
	if len(got) != len(want) {
		t.Fatalf("barsFor = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("bar %d = %+v, want %+v", i, got[i], want[i])
		}
	}
	for i, b := range barsFor(nil, 2, -60) {
		if b != (bar{-60, colGood}) {
			t.Errorf("empty bar %d = %+v", i, b)
		}
	}
	if b := barsFor([]audio.Quality{{Level: -90, SNR: 20}}, 1, -60)[0]; b.level != -60 {
		t.Errorf("level below the floor drawn at %v, want -60", b.level)
	}
}

func TestClippedRecently(t *testing.T) {
	q := make([]audio.Quality, 200)
	if clippedRecently(q) || clippedRecently(nil) {
		t.Error("no clip reported as clipping")
	}
	q[len(q)-clipHold].Clipped = true
	if !clippedRecently(q) {
		t.Error("clip 1 s ago not reported")
	}
	q[len(q)-clipHold].Clipped, q[len(q)-clipHold-1].Clipped = false, true
	if clippedRecently(q) {
		t.Error("clip older than 1 s still reported")
	}
}

func TestStopwatchText(t *testing.T) {
	s := time.Second
	for _, c := range []struct {
		elapsed, left time.Duration
		want          string
	}{
		{0, 0, "transcribing 0:00"},
		{7*s + 900*time.Millisecond, 0, "transcribing 0:07"},
		{7 * s, 3 * s, "transcribing 0:07 (~0:03 left)"},
		{7 * s, 2*s + time.Millisecond, "transcribing 0:07 (~0:03 left)"},
		{7 * s, 300 * time.Millisecond, "transcribing 0:07 (~0:01 left)"},
		{7 * s, -time.Second, "transcribing 0:07"},
		{75 * s, 125 * s, "transcribing 1:15 (~2:05 left)"},
		{3723 * s, 0, "transcribing 1:02:03"},
		{-s, 0, "transcribing 0:00"},
	} {
		if got := stopwatchText(c.elapsed, c.left); got != c.want {
			t.Errorf("stopwatchText(%v, %v) = %q, want %q", c.elapsed, c.left, got, c.want)
		}
	}
}
