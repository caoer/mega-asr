package audio

import (
	"math"
	"slices"
	"sync"
	"time"
)

// Block is the level meter's window: 20 ms.
const Block = Rate / 50

// BlockDuration is one Block's length.
const BlockDuration = 20 * time.Millisecond

// FloorBlocks is the noise-floor window: 10 s of blocks. The floor is the
// 10th percentile of the current block and the FloorBlocks before it,
// refreshed once a second.
const FloorBlocks = 500

// ClipLevel is the magnitude at which a sample counts as clipped.
const ClipLevel = 32700

// meter turns samples into 20 ms blocks: RMS in dBFS and whether any sample
// clipped. A partial block waits for the next Add.
type meter struct {
	acc  float64
	n    int
	clip bool
}

// add feeds samples and calls done for each completed block.
func (m *meter) add(s []int16, done func(level float64, clipped bool)) {
	for _, v := range s {
		f := float64(v) / 32768
		m.acc += f * f
		if v >= ClipLevel || v <= -ClipLevel {
			m.clip = true
		}
		m.n++
		if m.n == Block {
			done(DBFS(math.Sqrt(m.acc/Block)), m.clip)
			m.acc, m.n, m.clip = 0, 0, false
		}
	}
}

// history keeps the last len(v) block levels, addressed by absolute block
// index, and derives the noise floor from them.
type history struct {
	v     []float64
	n     int // blocks pushed
	floor float64
}

func newHistory(size int) history { return history{v: make([]float64, size), floor: -120} }

// push appends block level x (index n) and refreshes the floor when the
// index falls on a whole second.
func (h *history) push(x float64) {
	h.v[h.n%len(h.v)] = x
	if h.n%50 == 0 {
		k := min(h.n+1, len(h.v))
		w := make([]float64, 0, k)
		for i := h.n + 1 - k; i <= h.n; i++ {
			w = append(w, h.v[i%len(h.v)])
		}
		slices.Sort(w)
		h.floor = w[int(0.10*float64(len(w)-1))]
	}
	h.n++
}

// at is block i's level; i must be one of the last len(v) blocks.
func (h *history) at(i int) float64 { return h.v[i%len(h.v)] }

// Levels keeps the quality of each 20 ms block of the last Len blocks — the
// overlay's waveform — judged against the noise floor of a longer window.
// Safe for one writer and any number of readers.
type Levels struct {
	mu   sync.Mutex
	ring []Quality // oldest first once full
	next int
	full bool
	m    meter
	h    history
}

// NewLevels keeps the last blocks blocks (200 = 4 s) and judges each against
// the floor of the last floorBlocks (FloorBlocks = 10 s).
func NewLevels(blocks, floorBlocks int) *Levels {
	return &Levels{ring: make([]Quality, blocks), h: newHistory(floorBlocks + 1)}
}

// Add feeds samples.
func (l *Levels) Add(s []int16) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.m.add(s, func(level float64, clipped bool) {
		l.h.push(level)
		floor := l.h.floor
		if l.h.n <= 50 { // the first second: no floor yet
			floor = math.NaN()
		}
		l.ring[l.next] = Quality{Level: level, Floor: floor, SNR: level - floor, Clipped: clipped}
		l.next = (l.next + 1) % len(l.ring)
		if l.next == 0 {
			l.full = true
		}
	})
}

// Quality returns the kept blocks, oldest first. Floor and SNR are NaN for
// the blocks of the first second, before the floor is known.
func (l *Levels) Quality() []Quality {
	l.mu.Lock()
	defer l.mu.Unlock()
	if !l.full {
		return append([]Quality(nil), l.ring[:l.next]...)
	}
	return append(append([]Quality(nil), l.ring[l.next:]...), l.ring[:l.next]...)
}

// Snapshot returns the kept blocks' levels, oldest first, in dBFS.
func (l *Levels) Snapshot() []float64 {
	q := l.Quality()
	out := make([]float64, len(q))
	for i, b := range q {
		out[i] = b.Level
	}
	return out
}

// DeadLevel is the level at or below which a 20 ms block is digital
// silence: exact zeros read -120 dBFS and one LSB of 16-bit audio -90 dBFS,
// so a block this quiet carries no more than a few LSB, far under the noise
// floor of any live analog path.
const DeadLevel = -80

// DeadAfter is how long a mic stays at DeadLevel before it is called
// silent: no live mic holds that level for a single block, let alone 2 s.
const DeadAfter = 2 * time.Second

// Monitor follows a live stream for a meter: its current level and its
// current run of digitally silent blocks. Safe for one writer and any
// number of readers.
type Monitor struct {
	mu   sync.Mutex
	m    meter
	last [5]float64 // the last 100 ms of block levels, a ring
	n    int        // blocks seen
	run  int        // consecutive blocks at or below DeadLevel, up to the last one
}

// Add feeds samples.
func (d *Monitor) Add(s []int16) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.m.add(s, func(level float64, _ bool) {
		d.last[d.n%len(d.last)] = level
		d.n++
		if level <= DeadLevel {
			d.run++
		} else {
			d.run = 0
		}
	})
}

// Level is the loudest block of the last 100 ms in dBFS; -120 before the
// first block.
func (d *Monitor) Level() float64 {
	d.mu.Lock()
	defer d.mu.Unlock()
	l := -120.0
	for i := range min(d.n, len(d.last)) {
		l = max(l, d.last[i])
	}
	return l
}

// Silent is how long the stream has been digital silence up to now; 0
// once a live block arrives.
func (d *Monitor) Silent() time.Duration {
	d.mu.Lock()
	defer d.mu.Unlock()
	return time.Duration(d.run) * BlockDuration
}

// DBFS converts a linear amplitude (1 = full scale) to dBFS, floored at -120.
func DBFS(a float64) float64 {
	if a <= 1e-6 {
		return -120
	}
	return 20 * math.Log10(a)
}

// Stats is a clip's overall RMS and peak in dBFS.
func Stats(s []int16) (rms, peak float64) {
	var acc float64
	var pk int32
	for _, v := range s {
		f := float64(v) / 32768
		acc += f * f
		a := int32(v)
		if a < 0 {
			a = -a
		}
		pk = max(pk, a)
	}
	if len(s) == 0 {
		return -120, -120
	}
	return DBFS(math.Sqrt(acc / float64(len(s)))), DBFS(float64(pk) / 32768)
}
