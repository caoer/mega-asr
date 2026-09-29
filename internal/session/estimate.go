package session

import (
	"sync"
	"time"

	"github.com/caoer/mega-asr/internal/audio"
)

// estimator learns how long this machine takes per second of audio, from
// the chunks it has transcribed: an exponentially weighted mean over about
// the last ewmaN chunks. Load on the machine varies the rate twofold, so a
// fixed fit would mislead.
type estimator struct {
	mu   sync.Mutex
	rate float64 // seconds of ASR per second of audio
	n    int     // chunks measured
}

const (
	ewmaN       = 10
	minMeasured = 3           // no estimate before this many chunks
	minChunk    = time.Second // shorter chunks are mostly fixed cost
)

// add records one chunk: wall time spent on secs of audio.
func (e *estimator) add(wall, secs time.Duration) {
	if secs < minChunk {
		return
	}
	r := wall.Seconds() / secs.Seconds()
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.n == 0 {
		e.rate = r
	} else {
		e.rate += 2 / float64(ewmaN+1) * (r - e.rate)
	}
	e.n++
}

// left is the time the pending samples will take, 0 when unknown.
func (e *estimator) left(pending int64) time.Duration {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.n < minMeasured || pending <= 0 {
		return 0
	}
	return time.Duration(float64(pending) / audio.Rate * e.rate * float64(time.Second))
}
