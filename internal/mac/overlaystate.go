package mac

import (
	"fmt"
	"math"
	"time"

	"github.com/caoer/mega-asr/internal/audio"
)

// Colour classes of a waveform bar, one CAShapeLayer each.
const (
	colGood    = iota // SNR >= snrGood, or the floor is not known yet: blue
	colWeak           // snrWeak <= SNR < snrGood: amber
	colQuiet          // SNR < snrWeak — at the noise floor: grey
	colClipped        // a sample reached full scale: red
	nColours
)

// SNR thresholds in dB above the noise floor: at snrGood or more a block
// reads as clear speech, under snrWeak as the floor itself, and between
// the two the label warns that the input is weak.
const (
	snrGood = 18.0
	snrWeak = 10.0
)

// clipHold is how long the label reports clipping after a clipped block.
const clipHold = 50 // blocks = 1 s

// colourFor classifies one block. A NaN SNR means the floor is not known yet.
func colourFor(q audio.Quality) int {
	switch {
	case q.Clipped:
		return colClipped
	case math.IsNaN(q.SNR), q.SNR >= snrGood:
		return colGood
	case q.SNR >= snrWeak:
		return colWeak
	default:
		return colQuiet
	}
}

// bar is one drawn waveform bar.
type bar struct {
	level  float64 // dBFS
	colour int
}

// barsFor folds the newest 2*n blocks into n bars, right-aligned so the
// newest block is at the right edge; missing blocks are empty bars at the
// floor. A bar takes the louder block's level and SNR, and is red when
// either block clipped.
func barsFor(q []audio.Quality, n int, floor float64) []bar {
	m := min(len(q), 2*n)
	q = q[len(q)-m:]
	bars := make([]bar, n)
	for i := range bars {
		b := bar{level: floor, colour: colGood}
		var loud *audio.Quality
		clipped := false
		for _, j := range []int{2*i - (2*n - m), 2*i + 1 - (2*n - m)} {
			if j < 0 || j >= m {
				continue
			}
			if loud == nil || q[j].Level > loud.Level {
				loud = &q[j]
			}
			clipped = clipped || q[j].Clipped
		}
		if loud != nil {
			c := *loud
			c.Clipped = clipped
			b = bar{level: max(floor, c.Level), colour: colourFor(c)}
		}
		bars[i] = b
	}
	return bars
}

// clippedRecently reports a clipped block within the newest second.
func clippedRecently(q []audio.Quality) bool {
	for _, b := range q[max(0, len(q)-clipHold):] {
		if b.Clipped {
			return true
		}
	}
	return false
}

// stopwatchText is "transcribing 0:07", with " (~0:03 left)" when the
// estimate is known (left > 0). Elapsed truncates, left rounds up.
func stopwatchText(elapsed, left time.Duration) string {
	s := "transcribing " + clock(elapsed.Truncate(time.Second))
	if left > 0 {
		s += " (~" + clock((left + time.Second - 1).Truncate(time.Second)) + " left)"
	}
	return s
}

// clock formats d as m:ss, or h:mm:ss from an hour.
func clock(d time.Duration) string {
	sec := int(max(d, 0) / time.Second)
	if sec >= 3600 {
		return fmt.Sprintf("%d:%02d:%02d", sec/3600, sec/60%60, sec%60)
	}
	return fmt.Sprintf("%d:%02d", sec/60, sec%60)
}
