package audio

import (
	"fmt"
	"time"
)

// Chunker cuts a live stream into chunks for the ASR, at pauses long enough
// that the CLI's own VAD would end a segment there. A block is quiet when its
// RMS is below the noise floor (see FloorBlocks) + Margin. A run of quiet
// blocks reaching Pause ends the chunk at the run's midpoint, once the chunk
// before that point holds Min; a chunk reaching Max is cut after its quietest
// block of the last 3 s. Zero fields take the defaults: Pause 1900 ms, Min 3 s,
// Max 30 s, Margin 8 dB. One writer; the zero value is ready to use.
type Chunker struct {
	Pause, Min, Max time.Duration
	Margin          float64

	m            meter
	h            history
	start, quiet int // current chunk's first block; quiet blocks in a row
}

// Cut ends a chunk: End is the sample offset, counted from the first sample
// ever pushed, where the next chunk begins. Kind is EdgePause or EdgeMax;
// Reason says the same for a person.
type Cut struct {
	End    int
	Kind   string
	Reason string
}

// A chunk's edges, each of one kind: the take's first sample, a pause's
// midpoint, a forced cut at Max, the take's last sample, or a whole take's
// trim bound.
const (
	EdgeStart = "start"
	EdgePause = "pause"
	EdgeMax   = "max"
	EdgeTail  = "tail"
	EdgeWhole = "whole"
)

const lookback = 150 // a max cut searches the last 3 s of blocks

// Push feeds samples, any length, and returns the cuts they complete, in
// order — at most one per Min of audio. A cut can lie before the start of s:
// the midpoint of a pause is behind the block that completes it.
func (c *Chunker) Push(s []int16) []Cut {
	if c.h.v == nil {
		c.h = newHistory(FloorBlocks + 1)
		if c.Pause == 0 {
			c.Pause = 1900 * time.Millisecond
		}
		if c.Min == 0 {
			c.Min = 3 * time.Second
		}
		if c.Max == 0 {
			c.Max = 30 * time.Second
		}
		if c.Margin == 0 {
			c.Margin = 8
		}
	}
	pause, minB, maxB := int(c.Pause/BlockDuration), int(c.Min/BlockDuration), int(c.Max/BlockDuration)
	var cuts []Cut
	c.m.add(s, func(level float64, _ bool) {
		c.h.push(level)
		i := c.h.n - 1
		if level < c.h.floor+c.Margin {
			c.quiet++
		} else {
			c.quiet = 0
		}
		n := i - c.start + 1
		switch {
		case c.quiet >= pause && n-c.quiet/2 >= minB:
			end := i - c.quiet/2 + 1
			cuts = append(cuts, Cut{end * Block, EdgePause, fmt.Sprintf("pause %d ms", c.quiet*20)})
			c.start, c.quiet = end, 0
		case n >= maxB:
			best := i
			for j := max(c.start, i-lookback); j <= i; j++ {
				if c.h.at(j) < c.h.at(best) {
					best = j
				}
			}
			cuts = append(cuts, Cut{(best + 1) * Block, EdgeMax, fmt.Sprintf("max, quietest block %.1f dBFS", c.h.at(best))})
			c.start, c.quiet = best+1, 0
		}
	})
	return cuts
}
