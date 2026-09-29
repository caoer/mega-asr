package session

import (
	"errors"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/caoer/mega-asr/internal/audio"
	"github.com/caoer/mega-asr/internal/store"
)

// Close refuses, from now on, every take start (a refused start flashes
// msg), every resend and every re-transcription, and reports whether
// nothing was in flight as it did: an exit that follows a true answer cuts
// no take. It never waits for the controller's mutex; while another call
// holds it the answer is false. Reopen lifts the refusal.
func (c *Controller) Close(msg string) bool {
	c.closing.Store(&msg)
	if c.Busy() || !c.mu.TryLock() {
		return false
	}
	defer c.mu.Unlock()
	return !c.Busy()
}

// Reopen lifts Close's refusal.
func (c *Controller) Reopen() { c.closing.Store(nil) }

// closedLocked is Close's refusal of a resend or a re-transcription; c.mu is
// held, in the same hold that counts the call in flight.
func (c *Controller) closedLocked() error {
	if msg := c.closing.Load(); msg != nil {
		return errors.New(*msg)
	}
	return nil
}

// Interrupt is a termination signal's: without waiting for the controller's
// mutex it closes the WAV of every take started and not yet done, its
// backup track's too, so each header gives the samples written; then each
// such take whose record has no final state gets its start line if it has
// none, a stop line if it has none — the one the take was stopped with, as
// a cancel, else kind shutdown — and hold interrupted, or hold delivery_cut
// once its paste had begun: that text most likely landed. A take whose paste
// had not begun is never pasted after it. It returns the names of the takes
// it marked. A take that was recording writes nothing more of its own.
// Recover finds them at the next start.
func (c *Controller) Interrupt() []string {
	c.omu.Lock()
	ts := make([]*take, 0, len(c.open))
	for t := range c.open {
		ts = append(ts, t)
	}
	c.omu.Unlock()
	slices.SortFunc(ts, func(a, b *take) int { return a.seq - b.seq })
	var marked []string
	for _, r := range ts {
		r.interrupted.Store(true)
		r.paste.CompareAndSwap(0, pasteBarred) // a take whose paste has not begun is never pasted now
		if err := r.file.Close(); err != nil {
			log.Printf("session: interrupt: close %s.wav: %v", r.base, err)
		}
		r.backup.closeFile()
		evs, _ := store.Read(r.base)
		st := store.State(evs)
		if st.Final() {
			continue
		}
		r.jmu.Lock()
		if !r.started {
			var t *store.Target
			select {
			case <-r.target.done:
				t = StoreTarget(r.target.t)
			default: // the target is still being read
			}
			c.writeStartLocked(r, t)
		}
		if st.State == "" || st.State == "recording" {
			stop := store.Event{Ev: "stop", Kind: "shutdown"}
			if p := r.stopLine.Load(); p != nil {
				stop = *p
			}
			stop.DurS = seconds(samplesDuration(r.file.Samples()))
			c.mark(r.base, stop)
		}
		why := "interrupted"
		if r.paste.Load() == pasteBegun {
			why = "delivery_cut"
		}
		c.mark(r.base, store.Event{Ev: "hold", Why: why})
		r.jmu.Unlock()
		marked = append(marked, filepath.Base(r.base))
	}
	return marked
}

// RecoverWithin is how far back Recover looks: a take named or started
// before it is never read.
const RecoverWithin = 7 * 24 * time.Hour

// interruptedMsg is the alert at a start that found n takes cut by the last
// exit, the newest d long.
func interruptedMsg(n int, d time.Duration) string {
	what := Clock(d)
	if n > 1 {
		what = fmt.Sprintf("%d 条", n)
	}
	return "上次被重启打断 " + what + " · 未送达 · 见录音历史"
}

// found is a take Recover acts on.
type found struct {
	base    string
	line    bool      // append the recover line
	hold    string    // append hold with this why; "" none
	decode  bool      // transcribe its audio
	cut     bool      // the last exit cut it: it counts in the alert
	samples int       // its length: its WAV's, or its backup's where that stands in
	at      time.Time // when it started, as Store.Recent reads it
}

// Recover runs at startup, before the first take. Among the takes of the
// last RecoverWithin, by the time their names give — by record, and by WAV
// for a take cut before its start line — it finds each one the last exit
// cut: a record without a final state, a record Interrupt ended, or a WAV
// with neither a record nor a text. Each such take gets, once, its WAV
// headers repaired, a recover line and, without a final state, hold
// interrupted (hold empty when its audio is too short to be speech and its
// backup track holds none where the main track delivered nothing: both are
// removed); the alert names those left undelivered, with one sound — not
// a take the user cancelled, nor one whose paste was under way; each one
// without a text is transcribed in the background, its text saved and a
// text line appended, and never delivered.
// A recovery that an exit cut before its text line is transcribed again at
// the next start; one whose ASR failed is not (Re-transcribe is the retry).
// Chunk files an exit left are removed first.
func (c *Controller) Recover() {
	if err := os.RemoveAll(c.chunkDir()); err != nil {
		log.Printf("session: recover: %v", err)
	}
	fs, err := c.unfinished(c.now().Add(-RecoverWithin))
	if err != nil {
		log.Printf("session: recover: %v", err)
	}
	var cut []found
	var work []found
	c.mu.Lock()
	for _, f := range fs {
		if f.decode { // before its hold line: the hotkey then waits for its text
			if c.recovering == nil {
				c.recovering = map[string]time.Time{}
			}
			c.recovering[f.base] = f.at
		}
	}
	c.mu.Unlock()
	for _, f := range fs {
		name := filepath.Base(f.base)
		if f.line {
			c.mark(f.base, store.Event{Ev: "recover"})
		}
		if f.hold != "" {
			c.mark(f.base, store.Event{Ev: "hold", Why: f.hold})
		}
		log.Printf("session: recover: %s, %.2f s: recover %v, hold %q, transcribe %v", name, float64(f.samples)/audio.Rate, f.line, f.hold, f.decode)
		if f.cut {
			cut = append(cut, f)
		}
		if f.decode {
			work = append(work, f)
		}
	}
	log.Printf("session: recover: %d takes the last exit cut, %d to transcribe", len(cut), len(work))
	if n := len(cut); n > 0 {
		c.UI.Alert(interruptedMsg(n, samplesDuration(cut[n-1].samples)))
		if c.Sound != nil {
			c.Sound()
		}
	}
	if len(work) == 0 {
		return
	}
	c.wg.Add(1)
	end := c.begin()
	go func() {
		defer c.wg.Done()
		defer end()
		for _, f := range work {
			c.recoverText(f.base)
			c.mu.Lock()
			delete(c.recovering, f.base)
			c.mu.Unlock()
		}
	}()
}

// unfinished lists, oldest first, the takes since since that Recover acts
// on, with what it does to each; their WAVs' headers are repaired, and the
// audio of one too short to be speech is removed.
func (c *Controller) unfinished(since time.Time) ([]found, error) {
	recs, err := c.Store.Recent(0, since)
	evs := map[string][]store.Event{}
	for _, r := range recs {
		evs[r.Base] = r.Events
	}
	wavs, gerr := filepath.Glob(filepath.Join(c.Store.Dir, "*.wav"))
	for _, w := range wavs {
		base := strings.TrimSuffix(w, ".wav")
		if _, ok := evs[base]; ok {
			continue
		}
		if t, ok := store.NameTime(filepath.Base(base)); !ok || t.Before(since) {
			continue
		}
		if e, _ := store.Read(base); len(e) == 0 { // no record: cut before its start line
			evs[base] = nil
		}
	}
	var out []found
	for base, e := range evs {
		if f, ok := c.recoverable(base, e); ok {
			out = append(out, f)
		}
	}
	slices.SortFunc(out, func(a, b found) int { return strings.Compare(a.base, b.base) })
	return out, errors.Join(err, gerr)
}

// recoverable decides what Recover does to the take at base with the
// record evs; ok false: nothing.
func (c *Controller) recoverable(base string, evs []store.Event) (f found, ok bool) {
	f.base = base
	if f.at = (store.Record{Base: base, Events: evs}).Start(); f.at.IsZero() {
		f.at, _ = store.NameTime(filepath.Base(base))
	}
	wav, txt := exists(base+".wav"), exists(base+".txt")
	st := store.State(evs)
	recovered, textAfter, cancelled, hold := false, false, false, ""
	for _, e := range evs {
		switch {
		case e.Ev == "recover":
			recovered, textAfter = true, false
		case e.Ev == "text" && recovered:
			textAfter = true
		case e.Ev == "stop" && e.Kind == "cancel":
			cancelled = true
		case e.Ev == "hold":
			hold = e.Why
		}
	}
	undelivered := st.State == "undelivered" && st.Why == "interrupted"
	// a take the user dismissed is not in the alert
	counts := undelivered && !st.Dismissed
	marked := undelivered || st.State == "cancelled" && hold == "interrupted" || st.Why == "delivery_cut" // Interrupt's hold
	switch {
	case len(evs) == 0 && (txt || !wav):
		return f, false // a take from before the record, which has its text
	case !st.Final():
		f.line, f.hold, f.cut = !recovered, "interrupted", !cancelled
	case marked && !recovered:
		f.line, f.cut = true, counts
	case marked && !textAfter && wav && !txt: // its recovery was cut
	default:
		return f, false
	}
	f.decode = wav && !txt
	if !wav {
		if !txt && f.hold != "" {
			f.hold, f.cut = "empty", false // its audio was too short and is gone
		}
		return f, true
	}
	c.repairTracks(base)
	if st, err := os.Stat(base + ".wav"); err == nil {
		f.samples = int(st.Size()-44) / 2
	}
	if !audio.TooShort(f.samples) {
		return f, true
	}
	// too short to be speech, unless its backup holds speech where the main
	// track delivered nothing, as the stop path keeps it
	if s, err := audio.ReadWAV(base + ".wav"); err == nil {
		if out, used, _, _ := c.withBackup(base, s); used > 0 {
			f.samples = len(out)
			return f, true
		}
	}
	c.removeTracks(base)
	f.hold, f.cut, f.decode = "empty", false, false
	return f, true
}

// backupPath is the backup track of the take at base.
func (c *Controller) backupPath(base string) string {
	return filepath.Join(c.Store.Dir, store.BackupDir, filepath.Base(base)+".wav")
}

// repairTracks patches the headers of a take's WAV and of its backup track.
func (c *Controller) repairTracks(base string) {
	for _, p := range []string{base + ".wav", c.backupPath(base)} {
		if !exists(p) {
			continue
		}
		if err := store.RepairWAV(p); err != nil {
			log.Printf("session: recover: %v", err)
		}
	}
}

// removeTracks removes a take too short to be speech: its WAV, its input
// record and its backup track; its record stays.
func (c *Controller) removeTracks(base string) {
	if err := c.Store.Remove(base); err != nil {
		log.Printf("session: recover: %v", err)
	}
	if err := os.Remove(c.backupPath(base)); err != nil && !errors.Is(err, os.ErrNotExist) {
		log.Printf("session: recover: %v", err)
	}
}

// recoverText transcribes a recovered take as recording would have — with
// the backup track in the main track's dead spans, as takeBackup decides at
// a stop — saves its text and appends its text line; a take without speech
// then ends empty.
func (c *Controller) recoverText(base string) {
	name := filepath.Base(base)
	s, err := audio.ReadWAV(base + ".wav")
	if err != nil {
		log.Printf("session: recover %s: %v", name, err)
		return
	}
	audioOf := "main"
	if spliced, ok := c.recoverBackup(base, s); ok {
		s, audioOf = spliced, "backup"
	}
	t0 := time.Now()
	t := &take{chunker: c.newChunker(), base: base, dir: filepath.Join(c.chunkDir(), name)}
	c.cut(t, s, true)
	raw, failed := c.stitch(t)
	os.RemoveAll(t.dir)
	text := c.Post.Apply(raw)
	if failed == 0 {
		if err := c.Store.SaveText(base, raw, text); err != nil {
			log.Printf("session: recover %s: keep text: %v", name, err)
		}
	}
	engine := c.ChunkEngine
	if engine == "" {
		engine = c.Engine
	}
	c.Record(base, store.Event{Ev: "text", Engine: engine, RawChars: chars(raw), Chars: chars(text), FailedChunks: failed,
		LatencyMS: time.Since(t0).Milliseconds(), SHA256: textSum(text), Audio: audioOf})
	if failed == 0 && text == "" {
		c.Record(base, store.Event{Ev: "hold", Why: "empty"})
	}
	log.Printf("session: recovered %s (%.2f s, %d chunks, %d failed, audio %s): %q", name, float64(len(s))/audio.Rate, len(t.chunks), failed, audioOf, text)
}

// recoverBackup decides a recovered take's track as the stop path does: the
// backup track, when there is one, stands in wherever the main track s was
// dead and the backup holds a voice. Without a dead span the backup track
// is removed.
func (c *Controller) recoverBackup(base string, s []int16) ([]int16, bool) {
	out, used, dead, err := c.withBackup(base, s)
	if err != nil {
		return nil, false
	}
	if dead == 0 {
		if err := os.Remove(c.backupPath(base)); err != nil {
			log.Printf("session: recover: %v", err)
		}
		return nil, false
	}
	log.Printf("session: recover %s: main track dead in %d spans; %.2f s of the backup track in their place", filepath.Base(base), dead, float64(used)/audio.Rate)
	return out, used > 0
}

// withBackup is the kept take's main track s with its backup track standing
// in wherever s was dead and the backup holds a voice (splice), the backup
// lined up by recoveredOffset: the audio a take's text labelled audio
// backup came from. used counts the backup's samples taken, dead the spans
// in which s delivered nothing; with no span, or an empty backup track, out
// is s. err is the backup track's, when it cannot be read.
func (c *Controller) withBackup(base string, s []int16) (out []int16, used, dead int, err error) {
	bk, err := audio.ReadWAV(c.backupPath(base))
	if err != nil {
		return s, 0, 0, err
	}
	bt := false
	if in, err := store.LoadInput(base); err == nil {
		bt = in.Bluetooth()
	}
	evs, _ := store.Read(base)
	spans := deadSpans(s, bt, streamEnded(evs))
	if len(spans) == 0 || len(bk) == 0 {
		return s, 0, 0, nil
	}
	out, used = splice(s, bk, recoveredOffset(evs, len(s), len(bk)), spans)
	return out, used, len(spans), nil
}

// streamEnded reports whether a take's record evs says its main stream
// ended on its own: its stop line is of kind stream_end.
func streamEnded(evs []store.Event) bool {
	for _, e := range evs {
		if e.Ev == "stop" {
			return e.Kind == "stream_end"
		}
	}
	return false
}

// recoveredOffset is where a kept take's backup track, bk samples long,
// starts on its main track, n samples long (splice's off), by its record
// evs. When both tracks recorded until the take stopped or was cut, their
// ends line up. When one ended first — the main's stream on its own (a stop
// of kind stream_end), or the backup (a backup_off line after its
// backup_on) — the backup starts where its backup_on line puts it, counted
// from the start line's moment, the main track's first block's within a
// block on a wired input.
func recoveredOffset(evs []store.Event, n, bk int) int {
	var start, on *store.Event
	early := false
	for i, e := range evs {
		switch {
		case e.Ev == "start" && start == nil:
			start = &evs[i]
		case e.Ev == "backup_on" && on == nil:
			on = &evs[i]
		case e.Ev == "backup_off" && on != nil, e.Ev == "stop" && e.Kind == "stream_end":
			early = true
		}
	}
	if !early || start == nil || on == nil || start.TapAt.IsZero() {
		return n - bk
	}
	at := time.Duration(on.AfterS*float64(time.Second)) - start.At.Sub(start.TapAt)
	return int(at * audio.Rate / time.Second)
}

// deadSpans is where the main track s delivered nothing, judged block by
// block as record judges it while the take records: a run of digital
// silence of audio.DeadAfter, audio.BluetoothWarmup on a Bluetooth input
// until its first live sample, from the run's first block, and a Bluetooth
// input's warm-up itself; then as the track ends (endSpans), the whole take
// when none of it was heard, and the time after its last block when its
// stream ended on its own (cut). It is heard once a run of
// audio.HeardAfter above audio.DeadLevel has arrived.
func deadSpans(s []int16, bluetooth, cut bool) []span {
	var mon audio.Monitor
	var dead []span
	var hearing audio.Hearing
	warming, on := bluetooth, false
	for total := 0; total < len(s); {
		b := s[total:min(total+audio.Block, len(s))]
		if warming && slices.ContainsFunc(b, func(v int16) bool { return v != 0 }) {
			warming = false
			if !on && total > 0 {
				dead = append(dead, span{from: 0, to: total})
			}
		}
		after := audio.DeadAfter
		if warming {
			after = audio.BluetoothWarmup
		}
		mon.Add(b)
		hearing.Add(b)
		silent := mon.Silent()
		if now := silent >= after; now != on {
			on = now
			if on {
				judged := (total + len(b)) / audio.Block
				dead = openSpan(dead, (judged-int(silent/audio.BlockDuration))*audio.Block)
			} else {
				dead[len(dead)-1].to = total
			}
		}
		total += len(b)
	}
	return endSpans(dead, len(s), hearing.Heard(), cut)
}

func exists(p string) bool {
	_, err := os.Stat(p)
	return err == nil
}
