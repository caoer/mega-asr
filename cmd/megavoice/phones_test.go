package main

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/caoer/mega-asr/internal/remote"
	"github.com/caoer/mega-asr/internal/session"
	"github.com/caoer/mega-asr/internal/store"
)

type fakePhoneCtrl struct {
	startErr error
	ok       bool
	rec      string // the recording take; "" when none records
	owner    string // its owner
	calls    []string
}

func (f *fakePhoneCtrl) StartFrom(trigger string) (string, error) {
	f.calls = append(f.calls, "start "+trigger)
	return "20200101-120000", f.startErr
}
func (f *fakePhoneCtrl) StopFrom(via, id string) bool {
	f.calls = append(f.calls, "stop "+via+" "+id)
	return f.ok
}
func (f *fakePhoneCtrl) SendFrom(via, id string) bool {
	f.calls = append(f.calls, "send "+via+" "+id)
	return f.ok
}
func (f *fakePhoneCtrl) CancelFrom(via, id string) bool {
	f.calls = append(f.calls, "cancel "+via+" "+id)
	return f.ok
}
func (f *fakePhoneCtrl) Published() (session.State, string, time.Duration, string) {
	if f.rec == "" {
		return session.Idle, "", 0, ""
	}
	return session.Recording, f.rec, 1500 * time.Millisecond, f.owner
}

func refusalCode(err error) string {
	var r *remote.Refusal
	if errors.As(err, &r) {
		return r.Code
	}
	if err != nil {
		return err.Error()
	}
	return ""
}

// The phone's commands reach the controller by name, never a toggle; each
// refusal comes back with the code the phone acts on; a start flashes where
// it came from, and a start is refused while an exit waits.
func TestPhoneTakes(t *testing.T) {
	var flashes []string
	exiting := false
	const take = "20200101-120000"
	f := &fakePhoneCtrl{ok: true, rec: take, owner: session.OwnerMac}
	p := phoneTakes{ctrl: f, refuseTake: func() bool { return exiting }, flash: func(m string) { flashes = append(flashes, m) }}
	if _, err := p.Start("watch"); err != nil {
		t.Fatal(err)
	}
	if err := p.Stop("iphone", take, true); err != nil {
		t.Fatal(err)
	}
	if err := p.Stop("iphone", take, false); err != nil {
		t.Fatal(err)
	}
	if err := p.Cancel("iphone", take); err != nil {
		t.Fatal(err)
	}
	if got := len(f.calls); got != 4 || f.calls[0] != "start watch" || f.calls[1] != "send iphone "+take || f.calls[2] != "stop iphone "+take || f.calls[3] != "cancel iphone "+take {
		t.Fatalf("calls %q", f.calls)
	}
	if len(flashes) != 1 || flashes[0] != "手表：开始" {
		t.Fatalf("flashes %q", flashes)
	}
	if st := p.State(); st.Phase != "recording" || st.Take != "20200101-120000" || st.ElapsedMS != 1500 || st.Owner != "mac" {
		t.Fatalf("state %+v", st)
	}
	f.ok = false // the controller ends nothing: another take records, then none
	if c := refusalCode(p.Stop("iphone", "20200101-115900", true)); c != "other_take" {
		t.Fatalf("stop of an earlier take while another records: %s", c)
	}
	if c := refusalCode(p.Cancel("iphone", "20200101-115900")); c != "other_take" {
		t.Fatalf("cancel of an earlier take while another records: %s", c)
	}
	f.owner = session.OwnerStream // the controller refused the phone's own stream take
	if err := p.Cancel("iphone", take); refusalCode(err) != "other_take" || !strings.Contains(err.Error(), remote.ReasonStreamTake()) {
		t.Fatalf("cancel of a stream take from a non-owner: %v", err)
	}
	f.rec = ""
	if st := p.State(); st.Phase != "idle" || st.Take != "" || st.Owner != "" {
		t.Fatalf("idle state %+v", st)
	}
	if c := refusalCode(p.Stop("iphone", take, false)); c != "not_recording" {
		t.Fatalf("stop with nothing recording: %s", c)
	}
	if c := refusalCode(p.Cancel("iphone", take)); c != "not_recording" {
		t.Fatalf("cancel with nothing recording: %s", c)
	}
	f.rec = take
	for err, want := range map[error]string{session.ErrRecording: "recording", session.ErrClosing: "exiting", errors.New("mic busy"): "start_failed"} {
		f.startErr = err
		if c := refusalCode(func() error { _, err := p.Start("iphone"); return err }()); c != want {
			t.Errorf("start refused by %v: %s, want %s", err, c, want)
		}
	}
	f.startErr, exiting = nil, true
	n := len(f.calls)
	if c := refusalCode(func() error { _, err := p.Start("iphone"); return err }()); c != "exiting" || len(f.calls) != n {
		t.Fatalf("start while exiting: %s, calls %q", c, f.calls[n:])
	}
}

func TestViaName(t *testing.T) {
	for via, want := range map[string]string{"iphone": "iPhone", "watch": "手表", "keyboard": "iPhone 键盘"} {
		if got := viaName(via); got != want {
			t.Errorf("viaName(%q) = %q, want %q", via, got, want)
		}
	}
}

func TestPhoneTakesKeyboard(t *testing.T) {
	const take = "20200101-120000"
	var flashes []string
	f := &fakePhoneCtrl{ok: true, rec: take}
	p := phoneTakes{ctrl: f, flash: func(m string) { flashes = append(flashes, m) }}
	if id, err := p.Start("keyboard"); err != nil || id != take {
		t.Fatalf("keyboard start = %q, %v", id, err)
	}
	if err := p.Stop("keyboard", take, false); err != nil {
		t.Fatal(err)
	}
	if err := p.Stop("keyboard", take, true); err != nil {
		t.Fatal(err)
	}
	if err := p.Cancel("keyboard", take); err != nil {
		t.Fatal(err)
	}
	want := []string{"start keyboard", "stop keyboard " + take, "send keyboard " + take, "cancel keyboard " + take}
	if !reflect.DeepEqual(f.calls, want) {
		t.Fatalf("calls %q, want %q", f.calls, want)
	}
	if len(flashes) != 1 || flashes[0] != "iPhone 键盘：开始" {
		t.Fatalf("flashes %q", flashes)
	}
}

// recent is read from the store, so a restarted Mac still says what became
// of its last takes: newest first, final empty while a take is open, at_ms
// its start.
func TestRecentTakes(t *testing.T) {
	dir := t.TempDir()
	base := func(name string) string { return filepath.Join(dir, name) }
	t0 := time.Date(2026, 10, 6, 12, 0, 0, 0, time.Local)
	write := func(name string, at time.Time, evs ...store.Event) {
		for i, e := range evs {
			e.At = at.Add(time.Duration(i) * time.Second)
			if err := store.Append(base(name), e); err != nil {
				t.Fatal(err)
			}
		}
	}
	write("20261006-120000", t0, store.Event{Ev: "start"}, store.Event{Ev: "stop"}, store.Event{Ev: "deliver", OK: true, Submit: "ok"})
	write("20261006-120100", t0.Add(time.Minute), store.Event{Ev: "start"}, store.Event{Ev: "stop", Kind: "cancel"}, store.Event{Ev: "hold", Why: "cancelled"})
	write("20261006-120200", t0.Add(2*time.Minute), store.Event{Ev: "start"}, store.Event{Ev: "stop"}, store.Event{Ev: "hold", Why: "asr_failed"})
	write("20261006-120300", t0.Add(3*time.Minute), store.Event{Ev: "start"})
	got := recentTakes(dir, 3)
	want := []remote.Recent{
		{Take: "20261006-120300", Final: "", AtMS: t0.Add(3 * time.Minute).UnixMilli()},
		{Take: "20261006-120200", Final: "undelivered", Why: "asr_failed", AtMS: t0.Add(2 * time.Minute).UnixMilli()},
		{Take: "20261006-120100", Final: "cancelled", AtMS: t0.Add(time.Minute).UnixMilli()},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("recent\n%+v\nwant\n%+v", got, want)
	}
	if got := recentTakes(t.TempDir(), 3); len(got) != 0 {
		t.Fatalf("an empty store: %+v", got)
	}

	// takes of one second, as Next names them: -10 is the newest
	t1 := t0.Add(time.Hour)
	for i, name := range []string{"20261006-130000", "20261006-130000-2", "20261006-130000-9", "20261006-130000-10"} {
		write(name, t1.Add(time.Duration(i)*time.Millisecond), store.Event{Ev: "start"})
	}
	var names []string
	for _, r := range recentTakes(dir, 3) {
		names = append(names, r.Take)
	}
	if want := []string{"20261006-130000-10", "20261006-130000-9", "20261006-130000-2"}; !reflect.DeepEqual(names, want) {
		t.Fatalf("takes of one second: %v, want %v", names, want)
	}
}

// The relay's fake room, with its seeded randomness and its clock, is for
// tests only: nothing the binary links imports it.
func TestFakeRoomNotInTheBinary(t *testing.T) {
	// The relay client is linked from darwin-only files, so list the darwin
	// build whatever host runs the test (CI runs on linux).
	cmd := exec.Command("go", "list", "-deps", "-f", "{{.ImportPath}}", ".")
	cmd.Env = append(os.Environ(), "GOOS=darwin", "GOARCH=arm64", "CGO_ENABLED=1")
	out, err := cmd.Output()
	if err != nil {
		t.Skipf("go list: %v", err)
	}
	if strings.Contains(string(out), "internal/relay/fakeroom") {
		t.Fatal("megavoice links internal/relay/fakeroom")
	}
	if !strings.Contains(string(out), "internal/relay/client") {
		t.Fatal("go list does not show the relay client: the check checks nothing")
	}
}
