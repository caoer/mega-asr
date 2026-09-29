package audio

import (
	"math"
	"slices"
)

// VoiceMargin and VoiceRun judge a voice over a noise floor: VoiceRun 20 ms
// blocks in a row, each VoiceMargin dB or more above the floor. Room noise
// holds no such run, and a click or a key press is shorter than one.
const (
	VoiceMargin = 8
	VoiceRun    = 5
)

// Floor is s's noise floor: the 10th-percentile level of its live 20 ms
// blocks, those above DeadLevel, in dBFS; DeadLevel when it has none. Digital
// silence (a device opening with zeros) is no part of the room, so the floor
// never falls below DeadLevel.
func Floor(s []int16) float64 {
	var l []float64
	for _, x := range blockLevels(s) {
		if x > DeadLevel {
			l = append(l, x)
		}
	}
	if len(l) == 0 {
		return DeadLevel
	}
	slices.Sort(l)
	return l[int(0.10*float64(len(l)-1))]
}

// Voiced reports whether s holds a voice over floor (see VoiceRun).
func Voiced(s []int16, floor float64) bool {
	run := 0
	for _, l := range blockLevels(s) {
		if l < floor+VoiceMargin {
			run = 0
			continue
		}
		if run++; run >= VoiceRun {
			return true
		}
	}
	return false
}

// Dead reports whether s is digital silence throughout: no 20 ms block of
// it, a partial last one included, is above DeadLevel.
func Dead(s []int16) bool {
	for i := 0; i < len(s); i += Block {
		b := s[i:min(i+Block, len(s))]
		var acc float64
		for _, v := range b {
			f := float64(v) / 32768
			acc += f * f
		}
		if DBFS(math.Sqrt(acc/float64(len(b)))) > DeadLevel {
			return false
		}
	}
	return true
}

// blockLevels is the RMS of each whole 20 ms block of s, in dBFS.
func blockLevels(s []int16) []float64 {
	var out []float64
	for i := 0; i+Block <= len(s); i += Block {
		var acc float64
		for _, v := range s[i : i+Block] {
			f := float64(v) / 32768
			acc += f * f
		}
		out = append(out, DBFS(math.Sqrt(acc/Block)))
	}
	return out
}
