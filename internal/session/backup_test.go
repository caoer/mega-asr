package session

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/caoer/mega-asr/internal/audio"
	"github.com/caoer/mega-asr/internal/store"
)

// noise is n samples of a live input's room noise, about -65 dBFS.
func noise(n int) []int16 { return tone(n, 2) }

// speech is n samples of a voice over room noise: 300 ms of tone at amp
// (peak amp·16) in every 500 ms.
func speech(n, amp int) []int16 {
	s := noise(n)
	for i := 0; i < n; i += audio.Rate / 2 {
		copy(s[i:min(i+audio.Rate*3/10, n)], tone(audio.Rate*3/10, amp))
	}
	return s
}

// words is the fake ASR's answer: "main" for a stretch of the main track's
// voice (peak up to 16000), "backup" for the backup's (above it).
func words(s []int16) string {
	var out []string
	for i := 0; i+audio.Block <= len(s); i += audio.Block {
		w := "backup"
		switch p := peak(s[i : i+audio.Block]); {
		case p < 100:
			continue
		case p <= 16000:
			w = "main"
		}
		if len(out) == 0 || out[len(out)-1] != w {
			out = append(out, w)
		}
	}
	return strings.Join(out, " ")
}

// backupRig is a rig on main whose takes record backup beside it, with an
// ASR that answers words; base is its first take's path.
func backupRig(t *testing.T, main *fakeSource, backup func() audio.Source) (r *rig, base string) {
	r, base = rig2020(t, main)
	r.c.NewBackup = func(audio.Source) audio.Source { return backup() }
	r.asr.answer = words
	return r, base
}

// take records one take from tap to tap and waits for its delivery.
func (r *rig) take(t *testing.T) {
	t.Helper()
	r.c.Toggle()
	r.c.Toggle()
	r.wait(t)
}

// line is the take's first record line of kind ev.
func line(base, ev string) store.Event {
	evs, _ := store.Read(base)
	for _, e := range evs {
		if e.Ev == ev {
			return e
		}
	}
	return store.Event{}
}

// backupKept reports whether the take at base left its backup track.
func backupKept(base string) bool {
	_, err := os.Stat(filepath.Join(filepath.Dir(base), store.BackupDir, filepath.Base(base)+".wav"))
	return err == nil
}

func backupOf(s []int16) func() audio.Source {
	return func() audio.Source { return &fakeSource{blocks: [][]int16{s}} }
}

// A Bluetooth input opened cold records zeros while the headset switches
// into call mode. Under audio.BluetoothWarmup that shows no notice, but the
// main track delivered nothing: the words the backup heard then are kept,
// and room noise leaves the main track as it is.
func TestBackupBluetoothWarmup(t *testing.T) {
	for _, c := range []struct {
		name        string
		backup      []int16
		text, audio string
	}{
		{"backup hears room noise", noise(audio.Rate * 6), "main", "main"},
		{"backup hears the voice", speech(audio.Rate*6, 2000), "backup main", "backup"},
	} {
		t.Run(c.name, func(t *testing.T) {
			r, base := backupRig(t, &fakeSource{blocks: [][]int16{make([]int16, audio.Rate*3), tone(audio.Rate*3, 1000)}}, backupOf(c.backup))
			r.c.NewSource = func() audio.Source { return btSource{r.src.clone()} }
			r.take(t)
			if r.del.got() != "telegram|"+c.text || line(base, "text").Audio != c.audio || r.ui.noSignals() != 0 {
				t.Fatalf("delivered %q, want %q; text audio %q, want %q; ui %s", r.del.got(), c.text, line(base, "text").Audio, c.audio, r.ui)
			}
		})
	}
}

// A main track that delivered nothing over the whole take was dead for all
// of it though no notice came on — zeros stopped before audio.DeadAfter, a
// pop as the input opened and zeros after it, a Bluetooth input still
// warming at the stop — and the backup stands in for all of it.
func TestBackupDeadMainWithoutNotice(t *testing.T) {
	for _, c := range []struct {
		name string
		main []int16
		bt   bool
	}{
		{"a wired input of zeros under DeadAfter", make([]int16, audio.Rate*3/2), false},
		{"a Bluetooth input still warming", make([]int16, audio.Rate*4), true},
		{"a pop as the input opens, then zeros under DeadAfter", slices.Concat(tone(audio.Block, 1000), make([]int16, audio.Rate*3/2)), false},
	} {
		t.Run(c.name, func(t *testing.T) {
			r, base := backupRig(t, &fakeSource{blocks: [][]int16{c.main}}, backupOf(speech(len(c.main), 2000)))
			if c.bt {
				r.c.NewSource = func() audio.Source { return btSource{r.src.clone()} }
			}
			r.take(t)
			if r.del.got() != "telegram|backup" || line(base, "text").Audio != "backup" || !backupKept(base) || r.ui.noSignals() != 0 {
				t.Fatalf("delivered %q, text audio %q, backup kept %v, ui %s", r.del.got(), line(base, "text").Audio, backupKept(base), r.ui)
			}
		})
	}
}

// bursts is n samples of a quiet voice (peak 320) in runs of on samples,
// each followed by gap samples of gapOf: a gated input.
func bursts(n, on, gap int, gapOf func(int) []int16) []int16 {
	var s []int16
	for len(s) < n {
		s = slices.Concat(s, tone(on, 20), gapOf(gap))
	}
	return s[:n]
}

// A quiet or gated main track whose voice comes in runs shorter than
// audio.HeardAfter between digital silence is never heard as a whole, yet
// its chunks are decoded: without a backup its words are delivered from
// the main track; with one, the backup stands in for the whole take.
func TestGatedMainDecoded(t *testing.T) {
	zeros := func(n int) []int16 { return make([]int16, n) }
	for _, c := range []struct {
		name string
		main []int16
	}{
		{"one short word, then zeros", slices.Concat(tone(audio.Rate/4, 20), zeros(audio.Rate))},
		{"bursts between zeros", bursts(audio.Rate*3, audio.Rate/5, audio.Rate*3/10, zeros)},
		{"bursts between ±1 LSB", bursts(audio.Rate*3, audio.Rate/4, audio.Rate/4, digitalSilence)},
	} {
		t.Run(c.name+", no backup", func(t *testing.T) {
			r, base := rig2020(t, &fakeSource{blocks: [][]int16{c.main}})
			r.asr.answer = words
			r.take(t)
			if r.del.got() != "telegram|main" || line(base, "text").Audio != "main" {
				t.Fatalf("delivered %q, text audio %q", r.del.got(), line(base, "text").Audio)
			}
		})
		t.Run(c.name+", backup", func(t *testing.T) {
			r, base := backupRig(t, &fakeSource{blocks: [][]int16{c.main}}, backupOf(speech(len(c.main), 2000)))
			r.take(t)
			if r.del.got() != "telegram|backup" || line(base, "text").Audio != "backup" || !backupKept(base) {
				t.Fatalf("delivered %q, text audio %q, backup kept %v", r.del.got(), line(base, "text").Audio, backupKept(base))
			}
		})
	}
	t.Run("a chunk of bursts inside a heard take", func(t *testing.T) {
		main := slices.Concat(tone(audio.Rate, 20), bursts(audio.Rate*60, audio.Rate/5, audio.Rate*3/5, zeros), tone(audio.Rate/2, 20))
		r, _ := rig2020(t, &fakeSource{blocks: [][]int16{main}})
		r.asr.answer = words
		var got []Chunk
		r.c.OnChunks = func(_ string, cs []Chunk) { got = cs }
		r.take(t)
		if len(got) < 3 {
			t.Fatalf("%d chunks, want the middle one apart from the ends", len(got))
		}
		for i, ck := range got {
			if ck.Raw != "main" {
				t.Errorf("chunk %d [%v, %v): %q, want main", i+1, ck.Offset, ck.Offset+ck.Length, ck.Raw)
			}
		}
	})
}

// digitalSilence is n samples of ±1 LSB: not zeros, still digital silence.
func digitalSilence(n int) []int16 {
	s := make([]int16, n)
	for i := range s {
		s[i] = int16(i%2*2 - 1)
	}
	return s
}

// A Bluetooth warm-up whose first samples after the zeros are still digital
// silence runs on into the no-signal span: the spans do not overlap, and the
// backup's words stand in for both.
func TestBackupWarmupEndsInDigitalSilence(t *testing.T) {
	r, base := backupRig(t, &fakeSource{blocks: [][]int16{make([]int16, audio.Rate*3), digitalSilence(audio.Block), tone(audio.Rate, 1000)}},
		backupOf(speech(audio.Rate*4+audio.Block, 2000)))
	r.c.NewSource = func() audio.Source { return btSource{r.src.clone()} }
	r.take(t)
	if r.del.got() != "telegram|backup main" || line(base, "text").Audio != "backup" {
		t.Fatalf("delivered %q, text audio %q", r.del.got(), line(base, "text").Audio)
	}
}

// A backup that opens with digital silence holds room noise, not a voice:
// its zeros are no part of the room's floor, so the main track is decoded
// alone.
func TestBackupOpensWithZeros(t *testing.T) {
	bk := slices.Concat(make([]int16, audio.Rate), noise(audio.Rate*5))
	r, base := backupRig(t, &fakeSource{blocks: [][]int16{tone(audio.Rate*3, 1000), make([]int16, audio.Rate*3)}}, backupOf(bk))
	r.asr.answer = func(s []int16) string {
		if p := peak(s); p > 0 && p < 100 {
			return "Hmm." // a model's answer to room noise
		}
		return words(s)
	}
	r.take(t)
	if r.del.got() != "telegram|main" || line(base, "text").Audio != "main" {
		t.Fatalf("delivered %q, text audio %q", r.del.got(), line(base, "text").Audio)
	}
}

// live is a source the test feeds; it stays open until Stop.
type live struct {
	ch   chan []int16
	once sync.Once
}

func newLive() *live                                          { return &live{ch: make(chan []int16, 16)} }
func (l *live) Start(context.Context) (<-chan []int16, error) { return l.ch, nil }
func (l *live) Stop()                                         { l.once.Do(func() { close(l.ch) }) }
func (l *live) Err() error                                    { return nil }

// The tracks are lined up by when each delivered its first block, and the
// backup stands in for a dead span over the same time whichever input
// opened first: a wired backup that opens late, or a Bluetooth main whose
// first block comes after the backup's — the words said before it are the
// backup's alone.
func TestBackupLinesUp(t *testing.T) {
	sec := audio.Rate
	for _, c := range []struct {
		name      string
		mainFirst bool // the main track delivers first, the backup a second later; else the other way round
		main      [][]int16
		backup    []int16
		text      string
	}{
		{"backup opens a second late", true,
			[][]int16{tone(sec, 1000), make([]int16, 3*sec), tone(sec, 1000)},
			slices.Concat(noise(sec/2), speech(sec/2, 1500), noise(3*sec)), "main early main"},
		{"main opens a second late", false,
			[][]int16{make([]int16, 3*sec), tone(sec, 1000)},
			slices.Concat(speech(sec/2, 1500), noise(3*sec), speech(sec/2, 2000), noise(sec)), "early late main"},
	} {
		t.Run(c.name, func(t *testing.T) {
			main, bk := newLive(), newLive()
			r, base := backupRig(t, &fakeSource{}, func() audio.Source { return bk })
			r.c.NewSource = func() audio.Source { return main }
			r.asr.answer = func(s []int16) string { // by peak: the main's tone, the early and the late speech
				var out []string
				for i := 0; i+audio.Block <= len(s); i += audio.Block {
					w := "late"
					switch p := peak(s[i : i+audio.Block]); {
					case p < 100:
						continue
					case p <= 16000:
						w = "main"
					case p <= 24000:
						w = "early"
					}
					if len(out) == 0 || out[len(out)-1] != w {
						out = append(out, w)
					}
				}
				return strings.Join(out, " ")
			}
			r.c.Toggle()
			rest := c.main
			if c.mainFirst {
				main.ch <- rest[0]
				rest = rest[1:]
			} else {
				bk.ch <- c.backup
			}
			time.Sleep(time.Second)
			if c.mainFirst {
				bk.ch <- c.backup
			}
			for _, b := range rest {
				main.ch <- b
			}
			r.c.Toggle()
			r.wait(t)
			if r.del.got() != "telegram|"+c.text || line(base, "text").Audio != "backup" {
				t.Fatalf("delivered %q, want %q; text audio %q", r.del.got(), c.text, line(base, "text").Audio)
			}
		})
	}
}

// A main track that has delivered nothing when the take is stopped (an
// input that never delivered a block) was dead for the whole take: the backup
// stands in for all of it.
func TestBackupMainNeverStarted(t *testing.T) {
	r, base := backupRig(t, &fakeSource{}, backupOf(speech(audio.Rate*3, 2000)))
	r.c.NewSource = func() audio.Source { return newLive() }
	r.c.Toggle()
	r.backupRecording(t)
	r.c.Toggle()
	r.wait(t)
	if r.del.got() != "telegram|backup" || line(base, "text").Audio != "backup" || state(base) != "pasted/" {
		t.Fatalf("delivered %q, text audio %q, state %s", r.del.got(), line(base, "text").Audio, state(base))
	}
}

// slowOpen is a backup whose input takes until opened to start.
type slowOpen struct {
	fakeSource
	opened chan struct{}
}

func (s *slowOpen) Start(ctx context.Context) (<-chan []int16, error) {
	<-s.opened
	return s.fakeSource.Start(ctx)
}

// A main stream that ends on its own stops the take at once, while its
// backup is still opening; the take is decoded once the backup has ended.
func TestStreamEndWhileTheBackupOpens(t *testing.T) {
	opened := make(chan struct{})
	r, base := backupRig(t, &fakeSource{}, func() audio.Source {
		return &slowOpen{fakeSource: fakeSource{blocks: [][]int16{speech(audio.Rate, 2000)}}, opened: opened}
	})
	main := &endable{fakeSource: fakeSource{blocks: [][]int16{tone(audio.Rate*3, 1000)}}}
	r.c.NewSource = func() audio.Source { return main }
	r.c.Toggle()
	main.end(errors.New("Podium Condenser stopped delivering audio"))
	for deadline := time.Now().Add(2 * time.Second); r.c.State() == Recording; time.Sleep(time.Millisecond) {
		if time.Now().After(deadline) {
			close(opened)
			t.Fatalf("the take records on while its backup opens; ui %s", r.ui)
		}
	}
	close(opened)
	r.wait(t)
	if st := line(base, "stop"); st.Kind != "stream_end" || r.del.got() != "telegram|main" {
		t.Fatalf("stop line %+v, delivered %q", st, r.del.got())
	}
}

// A backup above -80 dBFS that holds no voice never stands in for a dead
// main track: the main track is decoded, and the backup is kept.
func TestBackupWithoutVoice(t *testing.T) {
	r, base := backupRig(t, &fakeSource{blocks: [][]int16{make([]int16, audio.Rate*3)}}, backupOf(noise(audio.Rate*3)))
	r.take(t)
	if r.del.got() != "" || line(base, "text").Audio != "main" || state(base) != "empty/" || !backupKept(base) {
		t.Fatalf("delivered %q, text audio %q, state %s, backup kept %v", r.del.got(), line(base, "text").Audio, state(base), backupKept(base))
	}
}

// Speech the main track recorded is always delivered; the backup stands in
// for the dead span alone, and only where it holds a voice.
func TestBackupPartlyDeadMain(t *testing.T) {
	zeros := make([]int16, audio.Rate*3)
	for _, c := range []struct {
		name         string
		main, backup []int16
		text, audio  string
	}{
		{"dies mid-take, backup hears room noise", slices.Concat(tone(audio.Rate*3, 1000), zeros), noise(audio.Rate * 6), "main", "main"},
		{"dies mid-take, backup hears the voice", slices.Concat(tone(audio.Rate*3, 1000), zeros), speech(audio.Rate*6, 2000), "main backup", "backup"},
		{"main silent first, live later", slices.Concat(zeros, tone(audio.Rate*3, 1000)), speech(audio.Rate*6, 2000), "backup main", "backup"},
	} {
		t.Run(c.name, func(t *testing.T) {
			half := len(c.main) / 2
			r, base := backupRig(t, &fakeSource{blocks: [][]int16{c.main[:half], c.main[half:]}}, backupOf(c.backup))
			r.take(t)
			if r.del.got() != "telegram|"+c.text || line(base, "text").Audio != c.audio || !backupKept(base) {
				t.Fatalf("delivered %q, want %q; text audio %q, want %q; backup kept %v", r.del.got(), c.text, line(base, "text").Audio, c.audio, backupKept(base))
			}
		})
	}
}

// backupRecording waits until the recording take's backup source records:
// a stop from here on goes through the source, not the flag its opening
// reads.
func (r *rig) backupRecording(t *testing.T) {
	t.Helper()
	for deadline := time.Now().Add(2 * time.Second); time.Now().Before(deadline); time.Sleep(time.Millisecond) {
		r.c.mu.Lock()
		var b *backup
		if r.c.rec != nil {
			b = r.c.rec.backup
		}
		r.c.mu.Unlock()
		if b != nil {
			b.mu.Lock()
			on := b.src != nil
			b.mu.Unlock()
			if on {
				return
			}
		}
	}
	t.Fatal("the backup track never recorded")
}

// endable streams its blocks and stays open until Stop, or until end: the
// stream then ends on its own, Err saying why.
type endable struct {
	fakeSource
	err error
}

func (e *endable) end(err error) { e.err = err; e.stopOnce.Do(func() { close(e.ch) }) }
func (e *endable) Err() error    { return e.err }

// When the main stream ends by itself, the take ends at once as
// stream_end with the stream's reason, and its backup stops with it, whether
// or not the backup had started; the backup's audio from past the main
// track's last block is decoded and delivered after it, also when the main
// track delivered less than a take or nothing at all. A re-transcription
// rebuilds the same track from the record.
func TestBackupStreamEnd(t *testing.T) {
	ends := func(why string) func(r *rig, src audio.Source) {
		return func(r *rig, src audio.Source) {
			r.backupRecording(t)
			src.(*endable).end(errors.New(why))
		}
	}
	endsAfter := func(main []int16) func(*rig) audio.Source {
		return func(*rig) audio.Source { return &endable{fakeSource: fakeSource{blocks: [][]int16{main}}} }
	}
	endsLater := endsAfter(tone(audio.Rate*3, 1000))
	for _, c := range []struct {
		name string
		main func(r *rig) audio.Source
		end  func(r *rig, src audio.Source)
		why  string
		text string
	}{
		{"a local input stops while the backup records", endsLater, ends("Podium Condenser stopped delivering audio"),
			"Podium Condenser stopped delivering audio", "main backup"},
		{"a remote box drops while the backup records", endsLater, ends("remote mic micbox: connection reset by peer"),
			"remote mic micbox: connection reset by peer", "main backup"},
		{"cut before the backup opens", func(*rig) audio.Source {
			return &fakeSource{blocks: [][]int16{tone(audio.Rate*3, 1000)}, cut: true}
		}, func(*rig, audio.Source) {}, "stream cut", "main backup"},
		{"the input stops before its first block", endsAfter(nil), ends("Podium Condenser stopped delivering audio"),
			"Podium Condenser stopped delivering audio", "backup"},
		{"the input stops within 0.3 s, never heard", endsAfter(tone(audio.Rate/10, 1000)), ends("Podium Condenser stopped delivering audio"),
			"Podium Condenser stopped delivering audio", "backup"},
	} {
		t.Run(c.name, func(t *testing.T) {
			r, base := backupRig(t, &fakeSource{}, backupOf(speech(audio.Rate*4, 2000)))
			src := c.main(r)
			r.c.NewSource = func() audio.Source { return src }
			r.c.Toggle()
			c.end(r, src)
			r.wait(t)
			if st := line(base, "stop"); st.Kind != "stream_end" || st.Why != c.why {
				t.Fatalf("stop line %+v, want stream_end %q", st, c.why)
			}
			if r.del.got() != "telegram|"+c.text || line(base, "text").Audio != "backup" || !backupKept(base) {
				t.Fatalf("delivered %q, want %q; text audio %q, backup kept %v", r.del.got(), c.text, line(base, "text").Audio, backupKept(base))
			}
			if re, err := r.c.Retranscribe(context.Background(), filepath.Base(base), Engine{Name: "funasr"}); err != nil || re.Raw != c.text || re.Audio != "backup" {
				t.Fatalf("re-transcribed %q from %q, %v; want %q", re.Raw, re.Audio, err, c.text)
			}
		})
	}
}

// A cancelled take's backup is judged as any take's — kept and decoded where
// the main track was dead, for the take's text; removed when the main track
// was live — and the take is never delivered.
func TestBackupCancel(t *testing.T) {
	for _, c := range []struct {
		name  string
		main  []int16
		audio string
		kept  bool
	}{
		{"dead main", make([]int16, audio.Rate*3), "backup", true},
		{"live main", tone(audio.Rate*3, 1000), "main", false},
	} {
		t.Run(c.name, func(t *testing.T) {
			r, base := backupRig(t, &fakeSource{blocks: [][]int16{c.main}}, backupOf(speech(audio.Rate*3, 2000)))
			r.c.Toggle()
			r.backupRecording(t)
			r.c.Cancel()
			r.wait(t)
			if r.del.got() != "" || state(base) != "cancelled/" || line(base, "text").Audio != c.audio || backupKept(base) != c.kept {
				t.Fatalf("delivered %q, state %s, text audio %q, backup kept %v", r.del.got(), state(base), line(base, "text").Audio, backupKept(base))
			}
		})
	}
}

// A streaming primary heard the main track's silence: it is aborted, and the
// chunk engine decodes the backup.
func TestBackupAbortsTheStream(t *testing.T) {
	r, base := backupRig(t, &fakeSource{blocks: [][]int16{make([]int16, audio.Rate*3)}}, backupOf(speech(audio.Rate*3, 2000)))
	st := &fakeStreamer{text: "hollow reply"} // what the fake model says to silence
	r.c.Stream, r.c.Engine, r.c.ChunkEngine = st, "doubao", "funasr"
	r.take(t)
	if st.aborted != 1 || r.del.got() != "telegram|backup" || line(base, "text").Engine != "funasr" {
		t.Fatalf("stream aborted %d times, delivered %q, text engine %q", st.aborted, r.del.got(), line(base, "text").Engine)
	}
}

// A take longer than WholeMax decoded from its backup is cut from the
// backup's own first sample — no chunk is empty or longer than the
// chunker's 30 s — and the main track's chunks queued while it recorded are
// not read.
func TestBackupLongTake(t *testing.T) {
	var bk []int16
	for range 8 {
		bk = slices.Concat(bk, speech(audio.Rate*4, 2000), noise(audio.Rate*2))
	}
	r, base := backupRig(t, &fakeSource{blocks: [][]int16{make([]int16, audio.Rate*40)}}, backupOf(bk))
	r.c.WholeMax = 5 * time.Second
	var mu sync.Mutex
	var lens []int
	r.asr.answer = func(s []int16) string {
		mu.Lock()
		lens = append(lens, len(s))
		mu.Unlock()
		if peak(s) == 0 {
			return "hollow reply" // what the fake model says to silence
		}
		return words(s)
	}
	r.take(t)
	mu.Lock()
	defer mu.Unlock()
	if slices.ContainsFunc(lens, func(n int) bool { return n == 0 || n > 30*audio.Rate }) || line(base, "text").Audio != "backup" || !strings.HasPrefix(r.del.got(), "telegram|backup") || strings.Contains(r.del.got(), "hollow") {
		t.Fatalf("chunk lengths %v; text audio %q; delivered %q", lens, line(base, "text").Audio, r.del.got())
	}
}

// A backup that never opens, or ends before the take stops, leaves a
// backup_off line with its reason, and the main track is delivered as usual.
func TestBackupOff(t *testing.T) {
	for _, c := range []struct {
		name   string
		backup *fakeSource
		why    string
	}{
		{"ends early with no reason of its own", &fakeSource{blocks: [][]int16{speech(audio.Rate/2, 2000)}, cut: true}, "stream cut"},
		{"ends early naming its device", &fakeSource{blocks: [][]int16{speech(audio.Rate, 2000)}, cut: true, cutErr: errors.New("Boom Mic stopped delivering audio")},
			"Boom Mic stopped delivering audio"},
		{"refused when opened", &fakeSource{startErr: errors.New("backup Handheld BT is not a wired input")}, "backup Handheld BT is not a wired input"},
	} {
		t.Run(c.name, func(t *testing.T) {
			r, base := backupRig(t, &fakeSource{blocks: [][]int16{tone(audio.Rate*3, 1000)}}, func() audio.Source { return c.backup })
			r.c.Toggle()
			r.c.mu.Lock()
			b := r.c.rec.backup
			r.c.mu.Unlock()
			<-b.done
			r.c.Toggle()
			r.wait(t)
			if off := line(base, "backup_off"); off.Why != c.why || r.del.got() != "telegram|main" {
				t.Fatalf("backup_off %+v, delivered %q", off, r.del.got())
			}
		})
	}
}

// The backup's sample i is the main track's off+i: a dead span takes the
// backup's samples over the same time, and a span open at the end runs to
// the backup's end.
func TestSplice(t *testing.T) {
	sec := audio.Rate
	main := slices.Concat(tone(sec, 1000), make([]int16, 2*sec), tone(sec, 1000))
	bk := speech(4*sec, 2000) // opened 0.5 s into the main track
	out, used := splice(main, bk, sec/2, []span{{sec, 3 * sec}})
	if want := slices.Concat(main[:sec], bk[sec/2:5*sec/2], main[3*sec:]); used != 2*sec || !slices.Equal(out, want) {
		t.Fatalf("closed span: %d samples, %d used", len(out), used)
	}
	out, used = splice(main[:3*sec], bk, sec/2, []span{{sec, -1}})
	if want := slices.Concat(main[:sec], bk[sec/2:]); used != 7*sec/2 || !slices.Equal(out, want) {
		t.Fatalf("open span: %d samples, %d used", len(out), used)
	}
}
