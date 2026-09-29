package audio

import (
	"math"
	"slices"
	"testing"
)

// level is n 20 ms blocks of a square wave whose RMS is db dBFS; zeros
// below the 16-bit range.
func level(db float64, n int) []int16 {
	a := int16(math.Round(32768 * math.Pow(10, db/20)))
	s := make([]int16, n*Block)
	for i := range s {
		s[i] = a * int16(1-2*(i%2))
	}
	return s
}

// The floor is the room's level: blocks of digital silence (a device that
// opens with zeros) are no part of the room, and a few quieter moments do
// not set it.
func TestFloor(t *testing.T) {
	for _, c := range []struct {
		name string
		s    []int16
		want float64
	}{
		{"a fifth of the blocks zeros", slices.Concat(level(-120, 100), level(-65, 400)), -65},
		{"a twentieth of the blocks quieter", slices.Concat(level(-65, 190), level(-76, 10)), -65},
		{"digital silence throughout", level(-120, 100), DeadLevel},
		{"shorter than a block", make([]int16, Block-1), DeadLevel},
	} {
		if got := Floor(c.s); math.Abs(got-c.want) > 0.5 {
			t.Errorf("%s: floor %.1f dBFS, want %.1f", c.name, got, c.want)
		}
	}
}

// A voice is 100 ms in a row of blocks VoiceMargin dB over the floor; a
// click or a key press, one loud block, is none however often it comes.
func TestVoiced(t *testing.T) {
	const room = -65
	var clicks []int16
	for range 20 {
		clicks = slices.Concat(clicks, level(-30, 1), level(room, 4))
	}
	for _, c := range []struct {
		name string
		s    []int16
		want bool
	}{
		{"clicks", clicks, false},
		{"a 100 ms syllable", slices.Concat(level(room, 20), level(-40, 5), level(room, 20)), true},
		{"80 ms", slices.Concat(level(room, 20), level(-40, 4), level(room, 20)), false},
		{"room noise", level(room, 50), false},
	} {
		if got := Voiced(c.s, Floor(c.s)); got != c.want {
			t.Errorf("%s: voiced %v, want %v", c.name, got, c.want)
		}
	}
}
