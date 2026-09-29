package audio

import (
	"math"
	"slices"
)

// Trim drops the quiet head and tail of s: 20 ms blocks whose RMS stays under
// the clip's 10th-percentile block level + 8 dB, keeping 200 ms of margin. A
// clip with no loud block comes back unchanged. Decoded as one window, an
// untrimmed take makes the model invent text in its silences.
func Trim(s []int16) []int16 {
	lo, hi := TrimBounds(s)
	return s[lo:hi]
}

// TrimBounds is what Trim keeps of s: s[lo:hi].
func TrimBounds(s []int16) (lo, hi int) {
	var rms []float64
	for i := 0; i+Block <= len(s); i += Block {
		var acc float64
		for _, v := range s[i : i+Block] {
			f := float64(v) / 32768
			acc += f * f
		}
		rms = append(rms, DBFS(math.Sqrt(acc/Block)))
	}
	if len(rms) == 0 {
		return 0, len(s)
	}
	sorted := slices.Clone(rms)
	slices.Sort(sorted)
	thr := sorted[int(0.10*float64(len(sorted)-1))] + 8
	first, last := -1, -1
	for i, v := range rms {
		if v >= thr {
			if first < 0 {
				first = i
			}
			last = i
		}
	}
	if first < 0 {
		return 0, len(s)
	}
	const margin = 10 // blocks
	return max(0, first-margin) * Block, min(len(s), (last+1+margin)*Block)
}
