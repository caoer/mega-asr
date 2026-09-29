package session

import (
	"context"
	"errors"
	"log"
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

// evNames is the take's record as its ev names, with a hold's why.
func evNames(t *testing.T, base string) string {
	t.Helper()
	evs, _ := store.Read(base)
	var out []string
	for _, e := range evs {
		n := e.Ev
		if e.Ev == "hold" || e.Ev == "stop" {
			n += " " + e.Why + e.Kind
		}
		out = append(out, n)
	}
	return strings.Join(out, ",")
}

// A take cut before its start line — a WAV left open, no record, no text —
// is repaired, marked, transcribed and saved at startup, never delivered.
func TestRecoverTranscribesOrphan(t *testing.T) {
	r := newRig(t, &fakeSource{}, "recovered text")
	tk, err := r.c.Store.Begin(time.Date(2020, 4, 17, 8, 15, 0, 0, time.Local))
	if err != nil {
		t.Fatal(err)
	}
	tk.Write(noise(audio.Rate)) // no Close: the app died mid-take
	os.MkdirAll(filepath.Join(r.chunks, "stale"), 0o755)
	r.c.Recover()
	r.wait(t)
	base := r.dir + "/20200417-081500"
	txt, _ := os.ReadFile(base + ".txt")
	if string(txt) != "recovered text\n" || r.del.got() != "" || len(r.del.captured) != 0 {
		t.Fatalf("text %q, delivered %q", txt, r.del.got())
	}
	if s, err := audio.ReadWAV(base + ".wav"); err != nil || len(s) != audio.Rate {
		t.Fatalf("repaired wav: %d samples, %v", len(s), err)
	}
	if got := evNames(t, base); got != "recover,hold interrupted,text" {
		t.Fatalf("record %s", got)
	}
	if st := readState(base); st.State != "undelivered" || st.Why != "interrupted" {
		t.Fatalf("state %+v", st)
	}
	if r.ui.String() != "alert:上次被重启打断 0:01 · 未送达 · 见录音历史" || files(t, r.chunks) != "" {
		t.Fatalf("ui %s, chunk dir %q", r.ui, files(t, r.chunks))
	}
}

func readState(base string) store.Status {
	evs, _ := store.Read(base)
	return store.State(evs)
}

// leftTake writes a take of n samples of room noise named at at with the
// record lines evs and, when txt is set, its text.
func leftTake(t *testing.T, dir string, at time.Time, n int, txt string, evs ...store.Event) string {
	t.Helper()
	tk, err := store.Store{Dir: dir}.Begin(at)
	if err != nil {
		t.Fatal(err)
	}
	tk.Write(noise(n))
	tk.Close()
	for _, e := range evs {
		e.At = at
		if err := store.Append(tk.Base(), e); err != nil {
			t.Fatal(err)
		}
	}
	if txt != "" {
		store.Store{}.SaveText(tk.Base(), txt, txt)
	}
	return tk.Base()
}

// A take whose text was saved and which was killed before its turn has a
// record ending in text: Recover marks it undelivered, interrupted, and
// transcribes nothing.
func TestRecoverSavedTextIsUndelivered(t *testing.T) {
	r := newRig(t, &fakeSource{}, "never asked")
	base := leftTake(t, r.dir, time.Date(2020, 4, 17, 9, 0, 0, 0, time.Local), 2*audio.Rate, "saved words",
		store.Event{Ev: "start"}, store.Event{Ev: "stop", Kind: "tap"}, store.Event{Ev: "text", Chars: 11})
	r.c.Recover()
	r.wait(t)
	if st := readState(base); st.State != "undelivered" || st.Why != "interrupted" {
		t.Fatalf("state %+v", st)
	}
	if got := evNames(t, base); got != "start,stop tap,text,recover,hold interrupted" {
		t.Fatalf("record %s", got)
	}
	if r.asr.n() != 0 || r.del.got() != "" {
		t.Fatalf("asr calls %d, delivered %q", r.asr.n(), r.del.got())
	}
	if r.ui.String() != "alert:上次被重启打断 0:02 · 未送达 · 见录音历史" {
		t.Fatalf("ui %s", r.ui)
	}
}

// A take an exit cut that the user dismissed before the next start is
// recovered without the alert.
func TestRecoverDismissedNoAlert(t *testing.T) {
	r := newRig(t, &fakeSource{}, "never asked")
	base := leftTake(t, r.dir, time.Date(2020, 4, 17, 9, 0, 0, 0, time.Local), 2*audio.Rate, "saved words",
		store.Event{Ev: "start"}, store.Event{Ev: "stop", Kind: "tap"}, store.Event{Ev: "text", Chars: 11},
		store.Event{Ev: "hold", Why: "interrupted"}, store.Event{Ev: "dismiss"})
	r.c.Recover()
	r.wait(t)
	if got := evNames(t, base); got != "start,stop tap,text,hold interrupted,dismiss,recover" || r.ui.String() != "" {
		t.Fatalf("record %s, ui %s", got, r.ui)
	}
}

// Each take is recovered once: a second start appends nothing, transcribes
// nothing and says nothing. Covered: a take recording at the kill, one a
// signal marked, one without text, a final one, a short one, one older than
// RecoverWithin by its name with a record and one without, one an hour
// inside the window's edge and one an hour outside it, one whose recovered
// ASR failed, one cancelled and killed before its hold line, which is
// transcribed with no alert, and one whose recovery was cut before its text
// line, which is transcribed again with no alert.
func TestRecoverOnce(t *testing.T) {
	r := newRig(t, &fakeSource{}, "words")
	day := func(h int) time.Time { return time.Date(2020, 4, 17, h, 0, 0, 0, time.Local) }
	since := r.c.Now().Add(-7 * 24 * time.Hour) // RecoverWithin
	inside := leftTake(t, r.dir, since.Add(time.Hour), audio.Rate, "", store.Event{Ev: "start"})
	outside := leftTake(t, r.dir, since.Add(-time.Hour), audio.Rate, "", store.Event{Ev: "start"})
	recording := leftTake(t, r.dir, day(1), audio.Rate, "", store.Event{Ev: "start"})
	signalled := leftTake(t, r.dir, day(2), audio.Rate, "",
		store.Event{Ev: "start"}, store.Event{Ev: "stop", Kind: "shutdown"}, store.Event{Ev: "hold", Why: "interrupted"})
	pasted := leftTake(t, r.dir, day(3), audio.Rate, "done",
		store.Event{Ev: "start"}, store.Event{Ev: "stop", Kind: "tap"}, store.Event{Ev: "text"}, store.Event{Ev: "deliver", OK: true})
	short := leftTake(t, r.dir, day(4), audio.Rate/10, "", store.Event{Ev: "start"})
	old := leftTake(t, r.dir, time.Date(2020, 4, 1, 9, 0, 0, 0, time.Local), audio.Rate, "", store.Event{Ev: "start"})
	failing := leftTake(t, r.dir, day(5), audio.Rate, "", store.Event{Ev: "start"})
	oldWAV := leftTake(t, r.dir, time.Date(2020, 4, 2, 9, 0, 0, 0, time.Local), audio.Rate, "") // written today, named before the window
	cancelled := leftTake(t, r.dir, day(6), audio.Rate, "", store.Event{Ev: "start"}, store.Event{Ev: "stop", Kind: "cancel"})
	resumed := leftTake(t, r.dir, day(7), audio.Rate, "",
		store.Event{Ev: "start"}, store.Event{Ev: "recover"}, store.Event{Ev: "hold", Why: "interrupted"})
	r.asr.fail = map[int]bool{4: true} // the takes are transcribed oldest first: failing is the fourth
	sounds := 0
	r.c.Sound = func() { sounds++ }

	r.c.Recover()
	r.wait(t)
	want := map[string]string{
		recording: "start,recover,hold interrupted,text",
		signalled: "start,stop shutdown,hold interrupted,recover,text",
		pasted:    "start,stop tap,text,deliver",
		short:     "start,recover,hold empty",
		old:       "start",
		failing:   "start,recover,hold interrupted,text",
		oldWAV:    "",
		cancelled: "start,stop cancel,recover,hold interrupted,text",
		inside:    "start,recover,hold interrupted,text",
		outside:   "start",
		resumed:   "start,recover,hold interrupted,text",
	}
	before := map[string]string{}
	for base, w := range want {
		if got := evNames(t, base); got != w {
			t.Errorf("%s: record %s, want %s", filepath.Base(base), got, w)
		}
		b, _ := os.ReadFile(base + ".events.jsonl")
		before[base] = string(b)
	}
	if r.asr.n() != 6 {
		t.Errorf("asr calls %d, want 6", r.asr.n())
	}
	if _, err := os.Stat(short + ".wav"); err == nil {
		t.Error("the short take's audio is kept")
	}
	if r.ui.String() != "alert:上次被重启打断 4 条 · 未送达 · 见录音历史" || sounds != 1 {
		t.Errorf("ui %s, %d sounds", r.ui, sounds)
	}

	r2 := newRig(t, &fakeSource{}, "again")
	r2.c.Store, r2.c.ChunkDir, r2.c.Sound = r.c.Store, r.chunks, func() { sounds++ }
	r2.c.Recover()
	r2.wait(t)
	for base := range want {
		if b, _ := os.ReadFile(base + ".events.jsonl"); string(b) != before[base] {
			t.Errorf("%s: the second start appended %q", filepath.Base(base), strings.TrimPrefix(string(b), before[base]))
		}
	}
	if r2.asr.n() != 0 || r2.ui.String() != "" || sounds != 1 {
		t.Errorf("second start: asr calls %d, ui %s, %d sounds in all", r2.asr.n(), r2.ui, sounds)
	}
}

// A take cut while its main input was dead is recovered from its backup
// track, as the stop path decides it; with a live main input the backup
// track is removed.
func TestRecoverBackupTrack(t *testing.T) {
	for _, c := range []struct {
		name  string
		main  []int16
		text  string
		audio string
		kept  bool
		bt    bool // a Bluetooth input, whose warm-up is dead too
	}{
		{"silent main", make([]int16, audio.Rate*3), "backup words", "backup", true, false},
		{"live main", tone(audio.Rate*3, 1000), "main words", "main", false, false},
		{"a Bluetooth warm-up", slices.Concat(make([]int16, audio.Rate*3/2), tone(audio.Rate*3/2, 1000)), "backup words", "backup", true, true},
		{"a wired main of zeros under DeadAfter", make([]int16, audio.Rate*3/2), "backup words", "backup", true, false},
		{"a Bluetooth main still warming", make([]int16, audio.Rate*4), "backup words", "backup", true, true},
		{"a pop as the main opens, then zeros under DeadAfter", slices.Concat(tone(audio.Block, 1000), make([]int16, audio.Rate*3/2)),
			"backup words", "backup", true, false},
		{"a Bluetooth warm-up ending in digital silence", slices.Concat(make([]int16, audio.Rate*3), digitalSilence(audio.Block), tone(audio.Rate*3/2, 1000)),
			"backup words", "backup", true, true},
	} {
		t.Run(c.name, func(t *testing.T) {
			r := newRig(t, &fakeSource{})
			r.asr.answer = func(s []int16) string {
				if peak(s) < 20000 {
					return "main words"
				}
				return "backup words"
			}
			at := time.Date(2020, 4, 17, 9, 0, 0, 0, time.Local)
			tk, _ := r.c.Store.Begin(at)
			tk.Write(c.main) // left open, as a signal after its last block leaves it
			if c.bt {
				r.c.Store.SaveInput(tk.Base(), audio.TakeInput{Source: "local", Name: "Handheld BT", Transport: "blue"})
			}
			bk, _ := store.Store{Dir: filepath.Join(r.dir, store.BackupDir)}.Create(filepath.Base(tk.Base()))
			bk.Write(speech(audio.Rate*2, 2000)) // opened later than the main track
			store.Append(tk.Base(), store.Event{Ev: "start"})
			r.c.Recover()
			r.wait(t)
			txt, _ := os.ReadFile(tk.Base() + ".txt")
			evs, _ := store.Read(tk.Base())
			te := evs[len(evs)-1]
			if string(txt) != c.text+"\n" || te.Ev != "text" || te.Audio != c.audio {
				t.Fatalf("text %q, last line %+v", txt, te)
			}
			_, err := os.Stat(r.c.backupPath(tk.Base()))
			if kept := err == nil; kept != c.kept {
				t.Fatalf("backup kept %v, want %v", kept, c.kept)
			}
		})
	}
}

// A take killed before its main input delivered a block is too short to be
// speech, but its backup may hold the only copy of what was said: Recover
// keeps it as the stop path does — decoded from the backup, undelivered, in
// the alert — and removes it, backup and all, when the backup holds no
// voice.
func TestRecoverMainNeverDelivered(t *testing.T) {
	for _, c := range []struct {
		name       string
		backup     []int16
		record, ui string
		kept       bool
	}{
		{"the backup heard the speech", speech(audio.Rate*2, 2000), "start,recover,hold interrupted,text", "alert:上次被重启打断 0:02 · 未送达 · 见录音历史", true},
		{"the backup heard room noise", noise(audio.Rate * 2), "start,recover,hold empty", "", false},
	} {
		t.Run(c.name, func(t *testing.T) {
			r := newRig(t, &fakeSource{})
			r.asr.answer = words
			tk, _ := r.c.Store.Begin(time.Date(2020, 4, 17, 9, 0, 0, 0, time.Local)) // no block written
			bk, _ := store.Store{Dir: filepath.Join(r.dir, store.BackupDir)}.Create(filepath.Base(tk.Base()))
			bk.Write(c.backup)
			store.Append(tk.Base(), store.Event{Ev: "start"})
			r.c.Recover()
			r.wait(t)
			if got := evNames(t, tk.Base()); got != c.record || backupKept(tk.Base()) != c.kept || r.ui.String() != c.ui {
				t.Fatalf("record %s, backup kept %v, ui %s", got, backupKept(tk.Base()), r.ui)
			}
			if te := line(tk.Base(), "text"); c.kept && te.Audio != "backup" {
				t.Fatalf("text line %+v", te)
			}
		})
	}
}

// A signal while a take waits for its text marks it interrupted; the text
// that comes after is saved and never pasted, so the record's undelivered
// holds and the next start alerts for it.
func TestSignalBeforeThePaste(t *testing.T) {
	r, base := rig2020(t, &fakeSource{n: audio.Rate}, "words")
	r.asr.block = make(chan struct{})
	r.c.Toggle()
	r.c.Toggle()
	for deadline := time.Now().Add(2 * time.Second); line(base, "stop").Ev == ""; time.Sleep(time.Millisecond) {
		if time.Now().After(deadline) {
			close(r.asr.block)
			t.Fatal("no stop line")
		}
	}
	marked := r.c.Interrupt()
	close(r.asr.block)
	r.wait(t)
	r.del.mu.Lock()
	pastes := len(r.del.delivers)
	r.del.mu.Unlock()
	if got := evNames(t, base); len(marked) != 1 || got != "start,stop tap,hold interrupted,text" || pastes != 0 || state(base) != "undelivered/interrupted" {
		t.Fatalf("marked %v, record %s, %d pastes, state %s", marked, got, pastes, state(base))
	}
}

// heldLog is a log output that holds the first line containing at until
// release is closed, closing held once it holds it.
type heldLog struct {
	at            string
	held, release chan struct{}
	once          sync.Once
}

func (h *heldLog) Write(p []byte) (int, error) {
	if strings.Contains(string(p), h.at) {
		h.once.Do(func() { close(h.held); <-h.release })
	}
	return len(p), nil
}

// A signal after a paste failed, before the take's lines say so, marks it
// interrupted, never delivery_cut: nothing landed, so the next start alerts
// for it and the hotkey offers it.
func TestSignalAfterAFailedPaste(t *testing.T) {
	r, base := rig2020(t, &fakeSource{n: audio.Rate}, "words")
	r.del.deliverErr = errors.New("the target took no paste")
	h := &heldLog{at: "text left on the clipboard", held: make(chan struct{}), release: make(chan struct{})}
	log.SetOutput(h)
	t.Cleanup(func() { log.SetOutput(os.Stderr) })
	r.c.Toggle()
	r.c.Toggle()
	select {
	case <-h.held: // the paste failed; its deliver line is not written yet
	case <-time.After(2 * time.Second):
		t.Fatal("the paste never failed")
	}
	marked := r.c.Interrupt()
	close(h.release)
	r.wait(t)
	if got := evNames(t, base); len(marked) != 1 || !strings.HasPrefix(got, "start,stop tap,text,hold interrupted,") {
		t.Fatalf("marked %v, record %s", marked, got)
	}
}

// A signal while a take's paste goes out marks it delivery_cut: its text
// most likely landed, so the next start neither alerts nor sounds for it,
// the hotkey passes it over and the menu bar does not count it.
func TestSignalDuringDelivery(t *testing.T) {
	r, base := rig2020(t, &fakeSource{n: audio.Rate}, "words")
	entered := make(chan struct{})
	r.del.entered, r.del.block = entered, make(chan struct{})
	r.c.Toggle()
	r.c.Toggle()
	<-entered
	if got := r.c.Interrupt(); len(got) != 1 {
		t.Fatalf("marked %v", got)
	}
	if got := evNames(t, base); got != "start,stop tap,text,hold delivery_cut" {
		t.Fatalf("record %s", got)
	}
	evs, _ := store.Read(base)
	if st := store.State(evs); st.State != "undelivered" || st.Why != "delivery_cut" || st.Counted() {
		t.Fatalf("state %+v, counted %v", st, st.Counted())
	}
	r2 := newRig(t, &fakeSource{}, "again")
	sounds := 0
	r2.c.Store, r2.c.ChunkDir, r2.c.Sound = r.c.Store, t.TempDir(), func() { sounds++ }
	if id, err := r2.c.HotkeyTake(r2.c.Now()); id != "" || err != nil {
		t.Fatalf("the hotkey takes %q, %v", id, err)
	}
	r2.c.Recover()
	r2.wait(t)
	if got := evNames(t, base); got != "start,stop tap,text,hold delivery_cut,recover" || r2.ui.String() != "" || sounds != 0 || r2.asr.n() != 0 {
		t.Fatalf("after the next start: record %s, ui %s, %d sounds, %d decodes", got, r2.ui, sounds, r2.asr.n())
	}
	close(r.del.block)
	r.wait(t)
}

// A take cancelled while its target is still being read has no start line
// yet; a signal then writes the cancel it was stopped with, so the take
// stays cancelled, and the next start transcribes it without an alert.
func TestSignalAfterCancelBeforeStartLine(t *testing.T) {
	r, base := rig2020(t, &fakeSource{n: audio.Rate}, "words")
	r.del.delay = time.Second
	r.c.Toggle()
	for deadline := time.Now().Add(2 * time.Second); ; time.Sleep(time.Millisecond) { // its second on disk
		if st, err := os.Stat(base + ".wav"); err == nil && st.Size() >= 44+2*audio.Rate {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the take's audio never reached its WAV")
		}
	}
	r.c.Cancel()
	if got := r.c.Interrupt(); len(got) != 1 {
		t.Fatalf("marked %v", got)
	}
	evs, _ := store.Read(base)
	if got := evNames(t, base); got != "start,stop cancel,hold interrupted" || store.State(evs).State != "cancelled" {
		t.Fatalf("record %s, state %+v", got, store.State(evs))
	}
	r2 := newRig(t, &fakeSource{}, "again")
	sounds := 0
	r2.c.Store, r2.c.ChunkDir, r2.c.Sound = r.c.Store, t.TempDir(), func() { sounds++ }
	r2.c.Recover()
	r2.wait(t)
	evs, _ = store.Read(base)
	if got := evNames(t, base); got != "start,stop cancel,hold interrupted,recover,text" || store.State(evs).State != "cancelled" || r2.ui.String() != "" || sounds != 0 {
		t.Fatalf("after the next start: record %s, state %+v, ui %s, %d sounds", got, store.State(evs), r2.ui, sounds)
	}
	r.waitUpTo(t, 3*time.Second)
}

// A backup that ended early is lined up by its backup_on line, not by its
// end: speech it heard before the main track died is not put in the later
// dead span, where the main track holds it already.
func TestRecoverBackupEndedEarly(t *testing.T) {
	r := newRig(t, &fakeSource{})
	r.asr.answer = func(s []int16) string {
		if peak(s) < 20000 {
			return "main words"
		}
		return "backup words"
	}
	at := time.Date(2020, 4, 17, 9, 0, 0, 0, time.Local)
	tk, _ := r.c.Store.Begin(at)
	tk.Write(slices.Concat(tone(audio.Rate, 1000), make([]int16, audio.Rate*3))) // live, then dead until the signal
	tk.Close()
	bk, _ := store.Store{Dir: filepath.Join(r.dir, store.BackupDir)}.Create(filepath.Base(tk.Base()))
	bk.Write(speech(audio.Rate, 2000)) // the first second only
	bk.Close()
	for _, e := range []store.Event{
		{Ev: "start", At: at.Add(50 * time.Millisecond), TapAt: at},
		{Ev: "backup_on", AfterS: 0.05},
		{Ev: "backup_off", AfterS: 1.05, Why: "Boom Mic stopped delivering audio"},
		{Ev: "stop", Kind: "shutdown"},
		{Ev: "hold", Why: "interrupted"},
	} {
		store.Append(tk.Base(), e)
	}
	r.c.Recover()
	r.wait(t)
	txt, _ := os.ReadFile(tk.Base() + ".txt")
	if te := line(tk.Base(), "text"); string(txt) != "main words\n" || te.Audio != "main" {
		t.Fatalf("text %q, audio %q", txt, te.Audio)
	}
}

// A signal while the backup records closes both WAVs and marks the take;
// the backup's next block, which finds its WAV closed, adds no line.
func TestSignalWithALiveBackup(t *testing.T) {
	main, bk := newLive(), newLive()
	r, base := backupRig(t, &fakeSource{}, func() audio.Source { return bk })
	r.c.NewSource = func() audio.Source { return main }
	r.c.Toggle()
	main.ch <- tone(audio.Rate, 1000)
	bk.ch <- speech(audio.Rate, 2000)
	for deadline := time.Now().Add(2 * time.Second); line(base, "backup_on").Ev == ""; time.Sleep(time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatal("no backup_on line")
		}
	}
	r.c.mu.Lock()
	b := r.c.rec.backup
	r.c.mu.Unlock()
	r.c.Interrupt()
	bk.ch <- speech(audio.Rate, 2000)
	bk.Stop()
	<-b.done
	main.Stop()
	r.wait(t)
	if got := evNames(t, base); got != "start,backup_on,stop shutdown,hold interrupted" {
		t.Fatalf("record %s", got)
	}
	if s, err := audio.ReadWAV(r.c.backupPath(base)); err != nil || len(s) != audio.Rate {
		t.Fatalf("backup track %d samples, %v", len(s), err)
	}
}

// Close refuses a start and a re-transcription while it holds; Reopen lets
// both through again.
func TestCloseAndReopen(t *testing.T) {
	r := newRig(t, &fakeSource{n: audio.Rate}, "words")
	base := leftTake(t, r.dir, time.Date(2020, 4, 17, 9, 0, 0, 0, time.Local), audio.Rate, "kept",
		store.Event{Ev: "start"}, store.Event{Ev: "stop", Kind: "tap"}, store.Event{Ev: "text"}, store.Event{Ev: "deliver", OK: true})
	id := filepath.Base(base)
	if !r.c.Close("closing for a restart") {
		t.Fatal("an idle controller did not close")
	}
	if _, err := r.c.Retranscribe(context.Background(), id, Engine{Name: "funasr"}); err == nil || err.Error() != "closing for a restart" {
		t.Fatalf("a re-transcription while closed: %v", err)
	}
	r.c.Toggle()
	if st := r.c.State(); st != Idle || !strings.Contains(r.ui.String(), "flash:closing for a restart") {
		t.Fatalf("a start while closed: state %v, ui %s", st, r.ui)
	}
	r.c.Reopen()
	if _, err := r.c.Retranscribe(context.Background(), id, Engine{Name: "funasr"}); err != nil {
		t.Fatalf("a re-transcription after Reopen: %v", err)
	}
	r.c.Toggle()
	if st := r.c.State(); st != Recording {
		t.Fatalf("a start after Reopen: state %v", st)
	}
	r.c.Toggle()
	r.wait(t)
	if got := evNames(t, base); got != "start,stop tap,text,deliver,retranscribe" {
		t.Fatalf("record %s", got)
	}
}

// A resend is in flight from its call to its return: while it waits for
// the delivery lock and after it lets the lock go, the controller is Busy,
// so no exit finds it idle between the two.
func TestResendInFlightAroundTheLock(t *testing.T) {
	r := newRig(t, &fakeSource{})
	base := leftTake(t, r.dir, time.Date(2020, 4, 17, 9, 0, 0, 0, time.Local), audio.Rate, "kept",
		store.Event{Ev: "start"}, store.Event{Ev: "stop", Kind: "tap"}, store.Event{Ev: "text"}, store.Event{Ev: "hold", Why: "deliver_failed"})
	r.c.lockOnce.Do(func() { r.c.dlock = make(chan struct{}, 1) })
	r.c.dlock <- struct{}{} // the lock held with nothing counted in flight
	entered := make(chan struct{})
	r.del.entered, r.del.block = entered, make(chan struct{})
	done := make(chan error, 1)
	go func() {
		_, err := r.c.Resend(Resend{ID: filepath.Base(base), To: &target{name: "scratch"}, Via: "cli"})
		done <- err
	}()
	for deadline := time.Now().Add(2 * time.Second); ; time.Sleep(time.Millisecond) {
		r.c.mu.Lock()
		waiting := r.c.resending[base]
		r.c.mu.Unlock()
		if waiting {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the resend never started")
		}
	}
	if !r.c.Busy() {
		t.Fatal("a resend waiting for the delivery lock is not in flight")
	}
	<-r.c.dlock
	<-entered
	r.c.mu.Lock() // holds the resend between releasing the lock and its return
	close(r.del.block)
	for deadline := time.Now().Add(2 * time.Second); len(r.c.dlock) > 0; time.Sleep(time.Millisecond) {
		if time.Now().After(deadline) {
			r.c.mu.Unlock()
			t.Fatal("the resend never released the delivery lock")
		}
	}
	busy := r.c.Busy()
	r.c.mu.Unlock()
	if err := <-done; err != nil || !busy {
		t.Fatalf("resend %v; busy after the lock and before its return %v", err, busy)
	}
}

// A signal while a take records ends it as shutdown, even when its recording
// goroutine, finding the WAV Interrupt closed, ends before Interrupt writes
// the stop line: a closed WAV is no stream that ended on its own.
func TestSignalBeforeTheRecordingEnds(t *testing.T) {
	main := newLive()
	r, base := rig2020(t, &fakeSource{})
	r.c.NewSource = func() audio.Source { return main }
	r.c.Toggle()
	main.ch <- noise(audio.Rate / 10)
	r.c.mu.Lock()
	tk := r.c.rec
	r.c.mu.Unlock()
	<-tk.logged
	tk.jmu.Lock() // holds Interrupt before its stop line
	done := make(chan []string, 1)
	go func() { done <- r.c.Interrupt() }()
	for deadline := time.Now().Add(2 * time.Second); tk.file.Write(nil) == nil; time.Sleep(time.Millisecond) {
		if time.Now().After(deadline) {
			tk.jmu.Unlock()
			t.Fatal("Interrupt never closed the take's WAV")
		}
	}
	main.ch <- noise(audio.Rate / 10) // its write fails: the recording goroutine ends first
	r.wait(t)
	tk.jmu.Unlock()
	<-done
	if got := evNames(t, base); got != "start,stop shutdown,hold interrupted" {
		t.Fatalf("record %s", got)
	}
}
