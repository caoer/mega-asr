package audio

import "time"

// HeardAfter is how long a run of blocks above DeadLevel lasts before a
// stream counts as heard. A live analog path puts every block above
// DeadLevel, so a live input passes it within its first MinSamples; a pop,
// a click or a stale buffer as a device opens lasts a few blocks, and a
// stream that gave no more than that has delivered nothing worth a take.
const HeardAfter = MinSamples * time.Second / Rate

// Hearing follows whether a stream has been heard: a run of HeardAfter
// above DeadLevel, block by block. For one goroutine.
type Hearing struct {
	m     meter
	live  int  // consecutive blocks above DeadLevel, up to the last one
	quiet bool // a block at or below DeadLevel has arrived
	heard bool
}

// Add feeds samples.
func (h *Hearing) Add(s []int16) {
	h.m.add(s, func(level float64, _ bool) {
		if level <= DeadLevel {
			h.live, h.quiet = 0, true
			return
		}
		h.live++
		h.heard = h.heard || time.Duration(h.live)*BlockDuration >= HeardAfter
	})
}

// Heard reports whether the stream has been heard so far.
func (h *Hearing) Heard() bool { return h.heard }
