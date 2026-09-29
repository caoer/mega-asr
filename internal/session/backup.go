package session

import (
	"context"
	"errors"
	"io/fs"
	"log"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/caoer/mega-asr/internal/audio"
	"github.com/caoer/mega-asr/internal/store"
)

// backup is a take's backup track (Controller.NewBackup), recording beside
// the main track on its own goroutine: opening a second input never delays
// the main track's first block.
type backup struct {
	path string        // <store>/backup/<name>.wav
	done chan struct{} // closed once its stream has drained and its WAV is closed

	mu      sync.Mutex
	src     audio.Source // set once it records
	file    *store.Take  // its WAV, set once it records
	stopped bool         // told to stop with the take

	// written before done is closed
	samples int
	first   time.Time // when its first block arrived
}

// span is a stretch [from, to) of the main track's samples in which it
// delivered nothing: the whole take when it was never heard (no run of
// audio.HeardAfter above audio.DeadLevel), however short; a run of digital silence that turned the
// no-signal notice on, from the run's first block; a Bluetooth input's
// warm-up; or the time after its last block when its stream ended on its
// own. to is -1 when it lasted until the main track ended; the backup then
// stands in up to its own end.
type span struct{ from, to int }

// openSpan is dead with a span from from, open at its end: the last span
// runs on instead when it reaches from — a Bluetooth warm-up whose first
// samples are digital silence too. The spans never overlap.
func openSpan(dead []span, from int) []span {
	if n := len(dead); n > 0 && dead[n-1].to >= from {
		dead[n-1].to = -1
		return dead
	}
	return append(dead, span{from: from, to: -1})
}

// endSpans is dead once the main track has ended, total samples long: the
// whole take when it was never heard (no run of audio.HeardAfter above
// audio.DeadLevel) — no block at all, zeros stopped before the notice, a
// pop as the input opened and zeros after it, a Bluetooth input still
// warming — else, when its stream ended on its own (cut), a span after its
// last block unless one is open already.
func endSpans(dead []span, total int, heard, cut bool) []span {
	switch {
	case !heard:
		return []span{{from: 0, to: -1}}
	case cut && (len(dead) == 0 || dead[len(dead)-1].to >= 0):
		return append(dead, span{from: total, to: -1})
	}
	return dead
}

// startBackup opens the take's backup track for its main source src. Why a
// take has no backup, or lost it before the take stopped, is its record's
// backup_off line.
func (c *Controller) startBackup(r *take, src audio.Source) *backup {
	name := filepath.Base(r.base)
	dir := filepath.Join(c.Store.Dir, store.BackupDir)
	b := &backup{path: filepath.Join(dir, name+".wav"), done: make(chan struct{})}
	off := func(why string) {
		log.Printf("session: %s: backup track off: %s", name, why)
		c.note(r, store.Event{Ev: "backup_off", Why: why, AfterS: seconds(time.Since(r.tapAt))})
	}
	go func() {
		defer close(b.done)
		bs := c.NewBackup(src)
		if bs == nil {
			return
		}
		ch, err := bs.Start(context.Background())
		if err != nil {
			off(err.Error())
			return
		}
		file, err := store.Store{Dir: dir}.Create(name)
		if err != nil {
			off(err.Error())
			bs.Stop()
			for range ch {
			}
			return
		}
		b.mu.Lock()
		b.src, b.file = bs, file
		stopped := b.stopped
		b.mu.Unlock()
		if stopped {
			go bs.Stop()
		}
		from := ""
		if d, ok := bs.(audio.Described); ok {
			from = " from " + d.Input().DeviceName()
		}
		var werr error
		for s := range ch {
			if b.samples == 0 && len(s) > 0 {
				b.first = time.Now()
				log.Printf("session: %s: backup track%s", name, from)
				c.note(r, store.Event{Ev: "backup_on", AfterS: seconds(time.Since(r.tapAt))})
			}
			if werr = file.Write(s); werr != nil {
				break
			}
			b.samples += len(s)
		}
		if err := file.Close(); err != nil {
			log.Printf("session: close %s: %v", b.path, err)
		}
		b.mu.Lock()
		ended := !b.stopped
		b.mu.Unlock()
		switch {
		case werr != nil && r.interrupted.Load(): // Interrupt closed the WAV and wrote the take's last lines
		case werr != nil:
			off(werr.Error())
		case ended: // before the take stopped
			why := MsgStreamCut
			if err := bs.Err(); err != nil {
				why = err.Error()
			}
			off(why)
		}
		bs.Stop()
		for range ch {
		}
	}()
	return b
}

// stop ends the backup track with its take; safe on nil and more than once.
func (b *backup) stop() {
	if b == nil {
		return
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.stopped {
		return
	}
	b.stopped = true
	if b.src != nil {
		go b.src.Stop()
	}
}

// closeFile patches the backup WAV's header to the samples written and
// closes it (Interrupt); safe on nil and more than once.
func (b *backup) closeFile() {
	if b == nil {
		return
	}
	b.mu.Lock()
	f := b.file
	b.mu.Unlock()
	if f != nil {
		f.Close()
	}
}

// takeBackup runs once the main track has ended, total samples long, with
// dead the spans in which it delivered nothing. It waits for the backup
// track, which is stopping, and removes it when the main track had no such
// span. Where the backup holds a voice over a span, it queues the main track
// with the backup's audio in that span's place and reports true, however
// little the main track delivered; the main track's chunks already queued
// are left to the ASR and not read. A cancelled take is judged the same
// way, for its kept text.
func (c *Controller) takeBackup(r *take, total int, dead []span) bool {
	b := r.backup
	if b == nil {
		return false
	}
	<-b.done
	name := filepath.Base(r.base)
	remove := func() {
		if err := os.Remove(b.path); err != nil && !errors.Is(err, fs.ErrNotExist) {
			log.Printf("session: %s: %v", name, err)
		}
	}
	if len(dead) == 0 || b.samples == 0 {
		remove()
		return false
	}
	bk, err := audio.ReadWAV(b.path)
	var main []int16
	if err == nil {
		main, err = audio.ReadWAV(r.base + ".wav")
	}
	if err != nil {
		log.Printf("session: %s: backup track: %v", name, err)
		return false
	}
	// The tracks line up by their first blocks' arrival, to within a block:
	// a Bluetooth main can deliver its first a second or more after the
	// backup's. A main track that delivered nothing leaves the backup alone.
	off := 0
	if !r.first.IsZero() {
		off = int(b.first.Sub(r.first) * audio.Rate / time.Second)
	}
	s, used := splice(main, bk, off, dead)
	if used == 0 {
		if audio.TooShort(total) { // the take is not kept, nor its backup
			remove()
			return false
		}
		log.Printf("session: %s: main track dead in %d spans; no voice on the backup track there, kept at %s", name, len(dead), b.path)
		return false
	}
	log.Printf("session: %s: main track dead in %d spans; decoding %.2f s of the backup track %s in their place", name, len(dead), float64(used)/audio.Rate, b.path)
	r.fromBackup = true
	if len(r.chunks) > 0 { // queued while it streamed; their files go with them
		old, dir := r.chunks, r.dir
		c.wg.Add(1)
		go func() {
			defer c.wg.Done()
			for _, ck := range old {
				<-ck.done
			}
			os.RemoveAll(dir)
		}()
		r.chunks, r.dir = nil, r.dir+"-backup"
	}
	r.chunker = c.newChunker() // cut the spliced track from its own first sample
	c.cut(r, s, false)
	return true
}

// splice is the main track with each dead span in which the backup holds a
// voice (audio.Voiced over the whole backup's floor) replaced by the
// backup's samples over the same time; the backup's sample i is the main
// track's off+i, and a span from the main track's first sample takes the
// backup from its own first, recorded before the main track's when off is
// below 0. used counts the backup's samples taken; with 0, out is main.
func splice(main, bk []int16, off int, dead []span) (out []int16, used int) {
	floor := audio.Floor(bk)
	at := 0 // main's samples up to here are in out
	for _, d := range dead {
		to := d.to
		if to < 0 {
			to = max(len(main), off+len(bk))
		}
		lo, hi := min(max(d.from-off, 0), len(bk)), min(max(to-off, 0), len(bk))
		if d.from == 0 {
			lo = 0
		}
		if lo >= hi || !audio.Voiced(bk[lo:hi], floor) {
			continue
		}
		out = append(out, main[at:min(d.from, len(main))]...)
		out = append(out, bk[lo:hi]...)
		at = min(to, len(main))
		used += hi - lo
	}
	if used == 0 {
		return main, 0
	}
	return append(out, main[at:]...), used
}
