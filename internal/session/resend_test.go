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

// keptTake records one take of 1 s with text and waits until it is kept;
// deliverErr, when set, makes its own delivery fail.
func keptTake(t *testing.T, deliverErr error, texts ...string) (*rig, string) {
	t.Helper()
	r, base := rig2020(t, &fakeSource{n: audio.Rate}, texts...)
	r.del.setFront("A")
	r.del.deliverErr = deliverErr
	r.c.Toggle()
	r.c.Toggle()
	r.wait(t)
	r.del.mu.Lock()
	r.del.deliverErr = nil
	r.del.mu.Unlock()
	return r, base
}

// An undelivered take resent by a command goes to the target named, and its
// record gains a deliver line via cli with the caller: the take is then
// pasted.
func TestResendUndelivered(t *testing.T) {
	r, base := keptTake(t, errors.New("pane gone"), "a take written for the test")
	if got := state(base); got != "undelivered/deliver_failed" {
		t.Fatalf("before the resend: %s", got)
	}
	caller := &store.Caller{PID: 4242, Process: "zsh"}
	de, err := r.c.Resend(Resend{ID: "20200417-104200", To: &target{name: "B"}, Via: "cli", Caller: caller})
	if err != nil {
		t.Fatal(err)
	}
	if got := r.del.got(); got != "B|a take written for the test" {
		t.Fatalf("delivered %q", got)
	}
	evs, _ := store.Read(base)
	last := evs[len(evs)-1]
	if last.Ev != "deliver" || !last.OK || last.N != 2 || last.Via != "cli" || last.Source != "delivered" ||
		last.Submit != "none" || last.Caller == nil || *last.Caller != *caller || last.Text != "" || last.N != de.N || de.V != 1 || !de.At.Truncate(time.Millisecond).Equal(last.At) {
		t.Fatalf("last line %+v", last)
	}
	if got := state(base); got != "pasted/" {
		t.Fatalf("after the resend: %s", got)
	}
	if !strings.Contains(r.ui.String(), "flash:已粘贴 0:01「a take wri…」→ B") {
		t.Fatalf("ui %s", r.ui)
	}
	r.wait(t) // its readback
	if r.c.Busy() {
		t.Fatal("Busy after the resend")
	}
}

// A resend waits while another delivery holds the lock, and delivers once it
// is free; a second resend of the same take meanwhile is refused.
func TestResendWaitsForADelivery(t *testing.T) {
	r, base := keptTake(t, nil, "a take written for the test")
	unlock, err := r.c.LockDelivery(0)
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() {
		_, err := r.c.Resend(Resend{ID: "20200417-104200", To: &target{name: "B"}, Via: "page"})
		done <- err
	}()
	claimed(t, r.c, base)
	select {
	case err := <-done:
		t.Fatalf("resend returned while the lock was held: %v", err)
	default:
	}
	if _, err := r.c.Resend(Resend{ID: "20200417-104200", To: &target{name: "C"}, Via: "page"}); !errors.Is(err, ErrResending) {
		t.Fatalf("a second resend of the take: %v, want ErrResending", err)
	}
	if got := r.del.got(); got != "A|a take written for the test" {
		t.Fatalf("delivered while the lock was held: %q", got)
	}
	unlock()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("resend did not deliver once the lock was free")
	}
	if got := r.del.got(); got != "A|a take written for the test;B|a take written for the test" {
		t.Fatalf("delivered %q", got)
	}
	if got := strings.Join(mustRead(base), ","); got != "start,stop,text,deliver,deliver" {
		t.Fatalf("record %s", got)
	}
}

// A re-transcription by the local engine goes through the controller's queue
// and appends its answer; the take's stored texts stay byte for byte.
func TestRetranscribeKeepsStoredText(t *testing.T) {
	r, base := keptTake(t, nil, "a take written for the test", "the take again 派森")
	compareJSON := `{"id":"20200417-104200","engines":{"funasr":{"text":"x"}}}` + "\n"
	if err := os.WriteFile(base+".compare.json", []byte(compareJSON), 0o644); err != nil {
		t.Fatal(err)
	}
	before := map[string][]byte{}
	for _, ext := range []string{".txt", ".raw.txt", ".compare.json"} {
		before[ext], _ = os.ReadFile(base + ext)
	}
	re, err := r.c.Retranscribe(context.Background(), "20200417-104200", Engine{Name: "funasr", Model: "m.gguf"})
	if err != nil {
		t.Fatal(err)
	}
	evs, _ := store.Read(base)
	last := evs[len(evs)-1]
	if last.Ev != "retranscribe" || last.N != 1 || last.Engine != "funasr" || last.Model != "m.gguf" ||
		last.Raw != "the take again 派森" || last.Text != "the take again Python" || last.Err != "" || re.N != 1 || re.V != 1 || !re.At.Truncate(time.Millisecond).Equal(last.At) {
		t.Fatalf("last line %+v", last)
	}
	for ext, b := range before {
		if now, _ := os.ReadFile(base + ext); string(now) != string(b) {
			t.Errorf("%s changed: %q → %q", ext, b, now)
		}
	}
	if got := state(base); got != "pasted/" {
		t.Errorf("state %s", got)
	}
	if r.c.Busy() || files(t, r.chunks) != "" {
		t.Errorf("busy %v, chunk dir %q", r.c.Busy(), files(t, r.chunks))
	}
}

// A cloud engine the config does not name is refused before the audio goes
// anywhere, and the record gains nothing.
func TestRetranscribeRefusesUnnamedCloudEngine(t *testing.T) {
	r, base := keptTake(t, nil, "a take written for the test")
	cloud := Engine{Name: "cloudasr", Cloud: true, File: func(context.Context, string) (string, error) {
		t.Error("the cloud engine was called")
		return "", nil
	}}
	if _, err := r.c.Retranscribe(context.Background(), "20200417-104200", cloud); !errors.Is(err, ErrNotNamed) {
		t.Fatalf("err %v, want ErrNotNamed", err)
	}
	if got := strings.Join(mustRead(base), ","); got != "start,stop,text,deliver" {
		t.Fatalf("record %s", got)
	}
}

// An engine that fails leaves its error in a retranscribe line.
func TestRetranscribeEngineFailure(t *testing.T) {
	r, base := keptTake(t, nil, "a take written for the test")
	bad := Engine{Name: "cloudasr", Cloud: true, Named: true, File: func(context.Context, string) (string, error) {
		return "", errors.New("engine down")
	}}
	re, err := r.c.Retranscribe(context.Background(), "20200417-104200", bad)
	if err != nil {
		t.Fatal(err)
	}
	evs, _ := store.Read(base)
	last := evs[len(evs)-1]
	if last.Ev != "retranscribe" || last.N != 1 || last.Err != "engine down" || last.Text != "" || re.Err != "engine down" {
		t.Fatalf("last line %+v", last)
	}
}

// An id that names no take fails and writes nothing, in the store or beside
// it: a take outside the store is not reached by a path.
func TestUnknownTakeAppendsNothing(t *testing.T) {
	r, base := keptTake(t, nil, "a take written for the test")
	outside := filepath.Join(filepath.Dir(r.dir), "20200417-104300")
	for _, ext := range []string{".wav", ".txt"} {
		b, err := os.ReadFile(base + ext)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(outside+ext, b, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	before, beside := files(t, r.dir), files(t, filepath.Dir(r.dir))
	named := Engine{Name: "funasr"}
	for _, id := range []string{"20200417-105959", "../20200417-104200", "../20200417-104300", ""} {
		if _, err := r.c.Resend(Resend{ID: id, To: &target{name: "B"}, Via: "cli"}); !errors.Is(err, ErrNoTake) {
			t.Errorf("resend %q: %v, want ErrNoTake", id, err)
		}
		if _, err := r.c.Retranscribe(context.Background(), id, named); !errors.Is(err, ErrNoTake) {
			t.Errorf("retranscribe %q: %v, want ErrNoTake", id, err)
		}
	}
	if got := files(t, r.dir); got != before {
		t.Fatalf("store %q, was %q", got, before)
	}
	if got := files(t, filepath.Dir(r.dir)); got != beside {
		t.Fatalf("beside the store %q, was %q", got, beside)
	}
	if r.del.got() != "A|a take written for the test" {
		t.Fatalf("delivered %q", r.del.got())
	}
}

// claimed waits until a resend of the take at base has claimed it.
func claimed(t *testing.T, c *Controller, base string) {
	t.Helper()
	for deadline := time.Now().Add(5 * time.Second); ; time.Sleep(time.Millisecond) {
		c.mu.Lock()
		ok := c.resending[base]
		c.mu.Unlock()
		if ok {
			return
		}
		if time.Now().After(deadline) {
			t.Fatal("the resend never claimed the take")
		}
	}
}

// A resend whose line the record cannot take, and a re-transcription the
// same, say so with ErrNotRecorded: the text went out, the answer came back,
// and the record lacks them.
func TestResendReportsARecordNotWritten(t *testing.T) {
	r, base := keptTake(t, nil, "a take written for the test", "the take again")
	if err := os.Remove(base + ".events.jsonl"); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(base+".events.jsonl", 0o755); err != nil { // no line can be appended to it
		t.Fatal(err)
	}
	de, err := r.c.Resend(Resend{ID: "20200417-104200", To: &target{name: "B"}, Via: "cli"})
	if !errors.Is(err, ErrNotRecorded) || !de.OK {
		t.Fatalf("resend: %+v, %v; want the line with ErrNotRecorded", de, err)
	}
	if got := r.del.got(); got != "A|a take written for the test;B|a take written for the test" {
		t.Fatalf("delivered %q", got)
	}
	re, err := r.c.Retranscribe(context.Background(), "20200417-104200", Engine{Name: "funasr"})
	if !errors.Is(err, ErrNotRecorded) || re.Text != "the take again" {
		t.Fatalf("retranscribe: %+v, %v; want the line with ErrNotRecorded", re, err)
	}
	if !strings.Contains(r.ui.String(), "alert:"+MsgRecordFailed) {
		t.Fatalf("ui %s", r.ui)
	}
}

// A resend that fails leaves a take that has reached a target where it was:
// its deliver line with ok false, no hold, the state unchanged.
func TestFailedResendKeepsADeliveredTake(t *testing.T) {
	r, base := keptTake(t, nil, "a take written for the test")
	r.del.deliverErr = errors.New("pane gone")
	de, err := r.c.Resend(Resend{ID: "20200417-104200", To: &target{name: "B"}, Via: "page"})
	if err != nil || de.OK || de.Err != "pane gone" {
		t.Fatalf("%+v, %v", de, err)
	}
	if got := strings.Join(mustRead(base), ","); got != "start,stop,text,deliver,deliver" {
		t.Fatalf("record %s", got)
	}
	if got := state(base); got != "pasted/" {
		t.Fatalf("state %s", got)
	}
	if !strings.HasSuffix(r.ui.String(), "alert:"+MsgResendFailed) {
		t.Fatalf("ui %s", r.ui)
	}
}

// A resend counts as in flight from its call, while it waits for the
// delivery lock too: a restart waiting for Busy does not cut it.
func TestResendIsBusyWhileItWaits(t *testing.T) {
	r, base := keptTake(t, nil, "a take written for the test")
	r.c.lockOnce.Do(func() { r.c.dlock = make(chan struct{}, 1) })
	r.c.dlock <- struct{}{} // the lock, taken by nothing that counts itself
	done := make(chan error, 1)
	go func() {
		_, err := r.c.Resend(Resend{ID: "20200417-104200", To: &target{name: "B"}, Via: "cli"})
		done <- err
	}()
	claimed(t, r.c, base)
	if !r.c.Busy() {
		t.Fatal("not Busy while a resend waits for the delivery lock")
	}
	<-r.c.dlock
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	r.wait(t) // its readback
	if r.c.Busy() {
		t.Fatal("Busy after the resend")
	}
}

// A re-transcription counts as in flight while it decodes, and a second one
// of the same take meanwhile is refused; once it ends, the take can be
// re-transcribed again.
func TestRetranscribeOnceAtATime(t *testing.T) {
	r, base := keptTake(t, nil, "a take written for the test", "the take again")
	entered, release := make(chan struct{}), make(chan struct{})
	slow := Engine{Name: "cloudasr", Cloud: true, Named: true, File: func(context.Context, string) (string, error) {
		close(entered)
		<-release
		return "the take in the cloud", nil
	}}
	done := make(chan error, 1)
	go func() {
		_, err := r.c.Retranscribe(context.Background(), "20200417-104200", slow)
		done <- err
	}()
	<-entered
	if !r.c.Busy() {
		t.Error("not Busy while a re-transcription decodes")
	}
	if _, err := r.c.Retranscribe(context.Background(), "20200417-104200", Engine{Name: "funasr"}); !errors.Is(err, ErrRetranscribing) {
		t.Errorf("a second re-transcription of the take: %v, want ErrRetranscribing", err)
	}
	close(release)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if r.c.Busy() {
		t.Error("Busy after the re-transcription")
	}
	re, err := r.c.Retranscribe(context.Background(), "20200417-104200", Engine{Name: "funasr"})
	if err != nil || re.N != 2 || re.Text != "the take again" {
		t.Fatalf("again: %+v, %v", re, err)
	}
	if got := strings.Join(mustRead(base), ","); got != "start,stop,text,deliver,retranscribe,retranscribe" {
		t.Fatalf("record %s", got)
	}
}

// yieldASR decodes a re-transcription's chunk once release is closed, or
// fails it with its context's error when that ends first; a live take's
// chunk it decodes at once.
type yieldASR struct {
	mu      sync.Mutex
	calls   []string
	entered chan struct{} // closed at the first re-transcription call
	once    sync.Once
	release chan struct{}
}

func (a *yieldASR) note(s string) { a.mu.Lock(); a.calls = append(a.calls, s); a.mu.Unlock() }

func (a *yieldASR) Transcribe(ctx context.Context, wav string, whole bool) (string, error) {
	if !strings.Contains(filepath.Base(filepath.Dir(wav)), ".retranscribe-") {
		a.note("live")
		return "a live take written for the test", nil
	}
	a.note("re")
	a.once.Do(func() { close(a.entered) })
	select {
	case <-ctx.Done():
		a.note("re cancelled")
		return "", ctx.Err()
	case <-a.release:
		return "the take again", nil
	}
}

// A take started while a local re-transcription decodes does not wait for
// it: the re-transcription yields, the take is delivered, and the
// re-transcription is decoded again after it and appends its answer.
func TestLiveTakeDoesNotWaitForARetranscription(t *testing.T) {
	r, base := keptTake(t, nil, "a take written for the test")
	a := &yieldASR{entered: make(chan struct{}), release: make(chan struct{})}
	r.c.ASR = a
	done := make(chan store.Event, 1)
	go func() {
		re, err := r.c.Retranscribe(context.Background(), "20200417-104200", Engine{Name: "funasr"})
		if err != nil {
			t.Error(err)
		}
		done <- re
	}()
	<-a.entered
	r.c.Toggle()
	r.c.Toggle()
	r.wait(t) // the live take, delivered while the re-transcription is held
	if got := r.del.got(); got != "A|a take written for the test;A|a live take written for the test" {
		t.Fatalf("delivered %q", got)
	}
	select {
	case re := <-done:
		t.Fatalf("the re-transcription ended before its decode was let go: %+v", re)
	default:
	}
	close(a.release)
	select {
	case re := <-done:
		if re.Text != "the take again" || re.Err != "" || re.N != 1 {
			t.Fatalf("retranscribe line %+v", re)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("the re-transcription never ended")
	}
	a.mu.Lock()
	calls := strings.Join(a.calls, ",")
	a.mu.Unlock()
	if calls != "re,re cancelled,live,re" {
		t.Fatalf("ASR calls %s", calls)
	}
	if got := strings.Join(mustRead(base), ","); got != "start,stop,text,deliver,retranscribe" {
		t.Fatalf("record %s", got)
	}
}

// A local re-transcription waits while a take records, and its context
// bounds that wait: past it the answer is the context's error, and nothing
// of it is left in the queue.
func TestRetranscribeWaitIsBounded(t *testing.T) {
	r, base := keptTake(t, nil, "a take written for the test", "a live take written for the test", "the take again")
	r.c.Toggle() // a take records
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	start := time.Now()
	type answer struct {
		re  store.Event
		err error
	}
	done := make(chan answer, 1)
	go func() {
		re, err := r.c.Retranscribe(ctx, "20200417-104200", Engine{Name: "funasr"})
		done <- answer{re, err}
	}()
	select {
	case a := <-done:
		if a.err != nil || !strings.Contains(a.re.Err, context.DeadlineExceeded.Error()) || a.re.Text != "" || time.Since(start) > time.Second {
			t.Fatalf("%+v, %v after %v; want the deadline's error", a.re, a.err, time.Since(start))
		}
	case <-time.After(2 * time.Second):
		r.c.Toggle()
		t.Fatal("the re-transcription waited past its context")
	}
	r.c.Toggle()
	r.wait(t)
	if r.asr.n() != 2 {
		t.Fatalf("%d ASR calls, want 2: the kept take and the live one", r.asr.n())
	}
	re, err := r.c.Retranscribe(context.Background(), "20200417-104200", Engine{Name: "funasr"})
	if err != nil || re.N != 2 || re.Text != "the take again" || re.Err != "" {
		t.Fatalf("again: %+v, %v", re, err)
	}
	if r.c.Busy() || files(t, r.chunks) != "" {
		t.Fatalf("busy %v, chunk dir %q", r.c.Busy(), files(t, r.chunks))
	}
	if got := strings.Join(mustRead(base), ","); got != "start,stop,text,deliver,retranscribe,retranscribe" {
		t.Fatalf("record %s", got)
	}
}

// A resend waits for a delivery under way at most its Wait, 10 s when 0,
// then fails with ErrDeliveryBusy and appends nothing; the take is not left
// marked as being resent, and resends once the lock is free, n 2 then 3.
func TestResendWaitIsBounded(t *testing.T) {
	r, base := keptTake(t, nil, "a take written for the test")
	defer func(w time.Duration) { resendWait = w }(resendWait)
	resendWait = 50 * time.Millisecond
	unlock, err := r.c.LockDelivery(0)
	if err != nil {
		t.Fatal(err)
	}
	for _, wait := range []time.Duration{0, 20 * time.Millisecond} {
		done := make(chan error, 1)
		go func() {
			_, err := r.c.Resend(Resend{ID: "20200417-104200", To: &target{name: "B"}, Via: "cli", Wait: wait})
			done <- err
		}()
		select {
		case err := <-done:
			if !errors.Is(err, ErrDeliveryBusy) {
				t.Fatalf("wait %v: %v, want ErrDeliveryBusy", wait, err)
			}
		case <-time.After(2 * time.Second):
			t.Fatalf("wait %v: the resend waited past its bound", wait)
		}
	}
	unlock()
	if got := strings.Join(mustRead(base), ","); got != "start,stop,text,deliver" {
		t.Fatalf("record %s", got)
	}
	for n := 2; n <= 3; n++ {
		if de, err := r.c.Resend(Resend{ID: "20200417-104200", To: &target{name: "B"}, Via: "cli"}); err != nil || de.N != n {
			t.Fatalf("resend %d: %+v, %v", n, de, err)
		}
	}
	if _, err := r.c.Resend(Resend{ID: "20200417-104200", To: &target{name: "B"}, Text: "retranscription 9", Via: "cli"}); !errors.Is(err, ErrNoText) {
		t.Fatalf("no such text: %v", err)
	}
	if de, err := r.c.Resend(Resend{ID: "20200417-104200", To: &target{name: "B"}, Via: "cli"}); err != nil || de.N != 4 {
		t.Fatalf("after a refusal: %+v, %v", de, err)
	}
}

// Send presses Enter after the paste: submit ok and sent; an Enter that
// fails is submit err with its error, the text pasted.
func TestResendSend(t *testing.T) {
	r, base := keptTake(t, nil, "a take written for the test")
	de, err := r.c.Resend(Resend{ID: "20200417-104200", To: &target{name: "B"}, Send: true, Via: "page"})
	if err != nil || de.Submit != "ok" || de.Err != "" {
		t.Fatalf("%+v, %v", de, err)
	}
	if got := r.del.got(); !strings.HasSuffix(got, "B|a take written for the test;B|⏎") {
		t.Fatalf("delivered %q", got)
	}
	if got := state(base); got != "sent/" || !strings.Contains(r.ui.String(), "flash:已发送") {
		t.Fatalf("state %s, ui %s", got, r.ui)
	}
	r.del.submitErr = errors.New("lost focus")
	de, err = r.c.Resend(Resend{ID: "20200417-104200", To: &target{name: "C"}, Send: true, Via: "page"})
	if err != nil || !de.OK || de.Submit != "err" || de.Err != "lost focus" {
		t.Fatalf("%+v, %v", de, err)
	}
	if !strings.HasSuffix(r.ui.String(), "alert:"+MsgNotSent+"lost focus") {
		t.Fatalf("ui %s", r.ui)
	}
}

// Text picks the text: raw is .raw.txt, retranscription N the Nth
// re-transcription's answer; the line carries a text that differs from .txt.
func TestResendText(t *testing.T) {
	r, _ := keptTake(t, nil, "a take 派森 written for the test", "the first answer", "the second answer")
	for range 2 {
		if _, err := r.c.Retranscribe(context.Background(), "20200417-104200", Engine{Name: "funasr"}); err != nil {
			t.Fatal(err)
		}
	}
	for _, c := range []struct{ text, source, want, line string }{
		{"", "delivered", "a take Python written for the test", ""},
		{"raw", "raw", "a take 派森 written for the test", "a take 派森 written for the test"},
		{"retranscription 1", "retranscription 1", "the first answer", "the first answer"},
		{"retranscription 2", "retranscription 2", "the second answer", "the second answer"},
	} {
		de, err := r.c.Resend(Resend{ID: "20200417-104200", To: &target{name: "B"}, Text: c.text, Via: "cli"})
		if err != nil || de.Source != c.source || de.Text != c.line {
			t.Fatalf("text %q: %+v, %v", c.text, de, err)
		}
		if got := r.del.got(); !strings.HasSuffix(got, "B|"+c.want) {
			t.Fatalf("text %q: delivered %q", c.text, got)
		}
	}
	if _, err := r.c.Resend(Resend{ID: "20200417-104200", To: &target{name: "B"}, Text: "retranscription 3", Via: "cli"}); !errors.Is(err, ErrNoText) {
		t.Fatalf("retranscription 3: %v, want ErrNoText", err)
	}
}

// A take that still records, then one that still transcribes, is refused by
// both, and its record gains nothing from them.
func TestRefusesATakeOnItsWay(t *testing.T) {
	r, base := rig2020(t, &fakeSource{n: audio.Rate}, "a take written for the test")
	r.asr.block = make(chan struct{})
	r.c.Toggle()
	for deadline := time.Now().Add(5 * time.Second); ; time.Sleep(time.Millisecond) {
		if _, err := os.Stat(base + ".wav"); err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the take never started")
		}
	}
	try := func(want error) {
		t.Helper()
		if _, err := r.c.Resend(Resend{ID: "20200417-104200", To: &target{name: "B"}, Via: "cli"}); !errors.Is(err, want) {
			t.Errorf("resend: %v, want %v", err, want)
		}
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		if _, err := r.c.Retranscribe(ctx, "20200417-104200", Engine{Name: "funasr"}); !errors.Is(err, want) {
			t.Errorf("retranscribe: %v, want %v", err, want)
		}
	}
	try(ErrRecording)
	r.c.Toggle()
	try(ErrTranscribing)
	close(r.asr.block)
	r.wait(t)
	if got := strings.Join(mustRead(base), ","); got != "start,stop,text,deliver" {
		t.Fatalf("record %s", got)
	}
	if r.asr.n() != 1 {
		t.Fatalf("%d ASR calls, want the take's own", r.asr.n())
	}
}

// A resend's readback runs after it returns: the delivery lock is free at
// once, the deliver line's at is the delivery's time, and the verdict
// follows on its own line.
func TestResendReadbackAfterTheLock(t *testing.T) {
	r, base := keptTake(t, nil, "a take written for the test")
	r.del.received, r.del.receivedWhy, r.del.readFor = "short", "first 12 of 22", 1500*time.Millisecond
	start := time.Now()
	de, err := r.c.Resend(Resend{ID: "20200417-104200", To: &target{name: "B"}, Via: "cli"})
	if err != nil || time.Since(start) > 500*time.Millisecond {
		t.Fatalf("%v after %v; the resend waited for its readback", err, time.Since(start))
	}
	unlock, err := r.c.LockDelivery(time.Millisecond)
	if err != nil {
		t.Fatal("the delivery lock is held for the readback")
	}
	unlock()
	r.del.mu.Lock()
	delivered := r.del.delivers[len(r.del.delivers)-1]
	r.del.mu.Unlock()
	if d := de.At.Sub(delivered); d < 0 || d > 100*time.Millisecond {
		t.Fatalf("deliver at %v, %v after the delivery", de.At, d)
	}
	if !r.c.Busy() {
		t.Error("not Busy while the readback's line is to come")
	}
	r.waitUpTo(t, 5*time.Second)
	evs, _ := store.Read(base)
	if got := strings.Join(mustRead(base), ","); got != "start,stop,text,deliver,deliver,received" {
		t.Fatalf("record %s", got)
	}
	rl := evs[len(evs)-1]
	if rl.N != 2 || rl.Received != "short" || rl.Why != "first 12 of 22" || rl.At.Sub(de.At) < time.Second {
		t.Fatalf("received %+v after deliver at %v", rl, de.At)
	}
	if r.c.Busy() {
		t.Error("Busy after the readback")
	}
}

// A re-transcription decodes the audio the take's text came from: the main
// track, or, when the text line says audio backup, the main track with the
// backup's audio in its dead spans, rebuilt as Recover rebuilds it; what
// only the main track heard stays in. Its line names the track. The local
// engine decodes the same audio.
func TestRetranscribeDecodesTheTextsTrack(t *testing.T) {
	const s = audio.Rate
	mainOnly := tone(2*s, 1000)
	shortMain, shortBk := slices.Concat(tone(2*s, 1000), make([]int16, 3*s)), speech(4*s, 2000)
	startMain, startBk := slices.Concat(make([]int16, 3*s), tone(2*s, 1000)), speech(5*s, 2000)
	earlyMain := slices.Concat(tone(s, 1000), make([]int16, 3*s))
	for _, c := range []struct {
		name      string
		main, bk  []int16 // bk nil: no backup track, no text line of audio backup
		evs       []store.Event
		want      []int16
		wantAudio string
	}{
		{"main", mainOnly, nil, nil, mainOnly, "main"},
		// the backup opened a second after the main track, which died at 2 s
		{"a backup shorter than main", shortMain, shortBk, nil, slices.Concat(shortMain[:2*s], shortBk[s:]), "backup"},
		{"a dead span only at the start", startMain, startBk, nil, slices.Concat(startBk[:3*s], startMain[3*s:]), "backup"},
		// the main track has no dead span: the backup stands in nowhere
		{"a backup that stands in nowhere", mainOnly, speech(2*s, 2000), nil, mainOnly, "main"},
		// the backup heard the first second, where the main track was live,
		// and ended: lined up by its backup_on line, it stands in nowhere
		{"a backup that ended early", earlyMain, speech(s, 2000),
			[]store.Event{{Ev: "backup_on"}, {Ev: "backup_off", AfterS: 1, Why: "Boom Mic stopped delivering audio"}}, earlyMain, "main"},
	} {
		t.Run(c.name, func(t *testing.T) {
			r, base := keptTake(t, nil, "a take written for the test")
			if err := audio.SaveWAV(base+".wav", c.main); err != nil {
				t.Fatal(err)
			}
			if c.bk != nil {
				backup := filepath.Join(r.dir, store.BackupDir, "20200417-104200.wav")
				if err := os.MkdirAll(filepath.Dir(backup), 0o755); err != nil {
					t.Fatal(err)
				}
				if err := audio.SaveWAV(backup, c.bk); err != nil {
					t.Fatal(err)
				}
				for _, e := range c.evs {
					if err := store.Append(base, e); err != nil {
						t.Fatal(err)
					}
				}
				if err := store.Append(base, store.Event{Ev: "text", Audio: "backup", Chars: 27}); err != nil {
					t.Fatal(err)
				}
			}
			var got []int16
			file := Engine{Name: "cloudasr", Cloud: true, Named: true, File: func(_ context.Context, wav string) (string, error) {
				s, err := audio.ReadWAV(wav)
				got = s
				return "an answer", err
			}}
			re, err := r.c.Retranscribe(context.Background(), "20200417-104200", file)
			if err != nil || re.Audio != c.wantAudio || !slices.Equal(got, c.want) {
				t.Fatalf("%+v, %v; decoded %d samples, want %d (equal %v)", re, err, len(got), len(c.want), slices.Equal(got, c.want))
			}
			var decoded []int16
			var mu sync.Mutex
			r.asr.answer = func(s []int16) string {
				mu.Lock()
				defer mu.Unlock()
				decoded = append(decoded, s...)
				return "words"
			}
			re, err = r.c.Retranscribe(context.Background(), "20200417-104200", Engine{Name: "funasr"})
			mu.Lock()
			defer mu.Unlock()
			if err != nil || re.Audio != c.wantAudio || len(decoded) != len(c.want) || peak(decoded) != peak(c.want) {
				t.Fatalf("the local engine: %+v, %v; decoded %d samples, want %d", re, err, len(decoded), len(c.want))
			}
		})
	}
}
