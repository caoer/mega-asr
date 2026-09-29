package store

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestAppendAfterTornLine(t *testing.T) {
	base := filepath.Join(t.TempDir(), "20200511-090738")
	if err := Append(base, Event{At: at, Ev: "start", Trigger: "tap"}); err != nil {
		t.Fatal(err)
	}
	f, err := os.OpenFile(base+eventsExt, os.O_WRONLY|os.O_APPEND, 0)
	if err != nil {
		t.Fatal(err)
	}
	f.WriteString(`{"v":1,"at":"2020-05-11T09:07:39.4`) // a crash mid-line
	f.Close()
	if err := Append(base, Event{At: at.Add(2 * time.Second), Ev: "deliver", N: 1, Via: "auto"}); err != nil {
		t.Fatal(err)
	}

	evs, err := Read(base)
	if err != nil {
		t.Fatal(err)
	}
	if len(evs) != 2 || evs[0].Ev != "start" || evs[1].Ev != "deliver" || evs[1].N != 1 || evs[1].V != 1 {
		t.Fatalf("read %+v, want start then deliver", evs)
	}
	b, _ := os.ReadFile(base + eventsExt)
	lines := strings.Split(strings.TrimSuffix(string(b), "\n"), "\n")
	want := `{"v":1,"at":"2020-05-11T09:07:40.000Z","ev":"deliver","ok":false,"chars":0,`
	if len(lines) != 3 || !strings.HasPrefix(lines[2], want) {
		t.Fatalf("record:\n%s\nwant its last line to start %s", b, want)
	}
}

func TestState(t *testing.T) {
	start := Event{Ev: "start", Trigger: "tap"}
	stop := Event{Ev: "stop", Kind: "tap"}
	cancel := Event{Ev: "stop", Kind: "cancel"}
	text := Event{Ev: "text", Chars: 40}
	partial := Event{Ev: "text", Chars: 12, FailedChunks: 2}
	hold := func(why string) Event { return Event{Ev: "hold", Why: why} }
	deliver := func(n int, via string, ok bool, submit string) Event {
		return Event{Ev: "deliver", N: n, Via: via, OK: ok, Submit: submit}
	}
	rec := Event{Ev: "recover"}
	seen := Event{Ev: "seen"}

	for _, c := range []struct {
		name    string
		evs     []Event
		state   string
		why     string
		final   bool
		counted bool
	}{
		{"no record: a take from before the record", nil, "", "", false, false},
		{"recording", []Event{start}, "recording", "", false, false},
		{"transcribing", []Event{start, stop}, "transcribing", "", false, false},
		{"delivered with Enter", []Event{start, stop, text, deliver(1, "auto", true, "ok")}, "sent", "", true, false},
		{"delivered, Enter left to the user", []Event{start, stop, text, deliver(1, "auto", true, "none")}, "pasted", "", true, false},
		{"Enter failed", []Event{start, stop, text, deliver(1, "auto", true, "err")}, "pasted", "", true, false},
		{"Esc, then Enter or a tap", []Event{start, cancel, text, hold("cancelled")}, "cancelled", "", true, false},
		{"Esc, then the hotkey before the text", []Event{start, cancel}, "transcribing", "", false, false},
		{"Esc, then the hotkey", []Event{start, cancel, text, hold("cancelled"), deliver(2, "hotkey", true, "none")}, "pasted", "", true, false},
		{"no speech", []Event{start, stop, {Ev: "text"}, hold("empty")}, "empty", "", true, false},
		{"delivery failed", []Event{start, stop, text, deliver(1, "auto", false, ""), hold("deliver_failed")}, "undelivered", "deliver_failed", true, true},
		{"delivery failed, then seen", []Event{start, stop, text, deliver(1, "auto", false, ""), hold("deliver_failed"), seen}, "undelivered", "deliver_failed", true, false},
		{"seen, then a resend failed", []Event{start, stop, text, hold("deliver_failed"), seen, deliver(2, "page", false, "")}, "undelivered", "deliver_failed", true, true},
		{"resent twice", []Event{start, stop, text, hold("deliver_failed"), deliver(2, "page", true, "none"), deliver(3, "cli", true, "ok")}, "sent", "", true, false},
		{"pasted, then a resend failed", []Event{start, stop, text, deliver(1, "auto", true, "none"), deliver(2, "page", false, "")}, "pasted", "", true, false},
		{"resent, then a resend failed", []Event{start, stop, text, hold("deliver_failed"), deliver(2, "cli", true, "ok"), deliver(3, "page", false, "")}, "sent", "", true, false},
		{"incomplete, then a resend failed", []Event{start, stop, partial, hold("asr_failed"), deliver(1, "auto", true, "none"), deliver(2, "page", false, "")}, "undelivered", "incomplete", true, true},
		{"text saved, killed before its turn", []Event{start, stop, text}, "transcribing", "", false, false},
		{"text saved, killed, recovered", []Event{start, stop, text, rec, hold("interrupted")}, "undelivered", "interrupted", true, true},
		{"cut while recording, recovered", []Event{start, rec, hold("interrupted")}, "undelivered", "interrupted", true, true},
		{"cancelled, killed before its hold", []Event{start, cancel, rec, hold("interrupted")}, "cancelled", "", true, false},
		{"chunks failed, no text", []Event{start, stop, {Ev: "text", FailedChunks: 3}, hold("asr_failed")}, "undelivered", "asr_failed", true, true},
		{"chunks failed, the rest delivered", []Event{start, stop, partial, hold("asr_failed"), deliver(1, "auto", true, "none")}, "undelivered", "incomplete", true, true},
		{"incomplete, then resent", []Event{start, stop, partial, hold("asr_failed"), deliver(1, "auto", true, "none"), deliver(2, "page", true, "none")}, "pasted", "", true, false},
		{"re-transcribed does not change the state", []Event{start, stop, text, hold("asr_failed"), {Ev: "retranscribe", N: 1}}, "undelivered", "asr_failed", true, true},
	} {
		s := State(c.evs)
		if s.State != c.state || s.Why != c.why || s.Final() != c.final || s.Counted() != c.counted {
			t.Errorf("%s: got %+v final %v counted %v; want %q %q final %v counted %v",
				c.name, s, s.Final(), s.Counted(), c.state, c.why, c.final, c.counted)
		}
	}
}

func TestStateDismissed(t *testing.T) {
	failed := []Event{{Ev: "start"}, {Ev: "stop", Kind: "tap"}, {Ev: "text", Chars: 40}, {Ev: "hold", Why: "deliver_failed"}}
	dismiss, undismiss := Event{Ev: "dismiss"}, Event{Ev: "undismiss"}
	resend := func(ok bool) Event { return Event{Ev: "deliver", N: 2, Via: "page", OK: ok} }
	with := func(evs ...Event) []Event { return append(append([]Event{}, failed...), evs...) }
	for _, c := range []struct {
		name               string
		evs                []Event
		state              string
		dismissed, counted bool
	}{
		{"dismissed", with(dismiss), "undelivered", true, false},
		{"dismissed, then taken back", with(dismiss, undismiss), "undelivered", false, true},
		{"taken back, then dismissed again", with(dismiss, undismiss, dismiss), "undelivered", true, false},
		{"dismissed, then a resend failed", with(dismiss, resend(false)), "undelivered", true, false},
		{"dismissed, then resent", with(dismiss, resend(true)), "pasted", false, false},
		{"a delivered take is never dismissed", []Event{{Ev: "start"}, {Ev: "deliver", N: 1, Via: "auto", OK: true}, dismiss}, "pasted", false, false},
	} {
		s := State(c.evs)
		if s.State != c.state || s.Why != "deliver_failed" && c.state == "undelivered" || s.Dismissed != c.dismissed || s.Counted() != c.counted {
			t.Errorf("%s: got %+v counted %v; want %q dismissed %v counted %v", c.name, s, s.Counted(), c.state, c.dismissed, c.counted)
		}
	}
}

func TestNextAfterRemovedShortTake(t *testing.T) {
	s := Store{Dir: t.TempDir()}
	tk := writeTake(t, s, 1)
	short := tk.Base()
	if err := Append(short, Event{At: at, Ev: "start"}); err != nil {
		t.Fatal(err)
	}
	if err := tk.Close(); err != nil {
		t.Fatal(err)
	}
	if err := s.Remove(short); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(short + eventsExt); err != nil {
		t.Fatalf("Remove took the record: %v", err)
	}

	next, err := s.Next(at)
	if err != nil {
		t.Fatal(err)
	}
	if next == short || filepath.Base(next) != "20200511-090738-2" {
		t.Fatalf("next take in the same second got %s, the removed one was %s", next, short)
	}
}

func TestRecent(t *testing.T) {
	s := Store{Dir: t.TempDir()}
	now := time.Date(2020, 2, 3, 16, 52, 9, 0, time.Local)
	take := func(name string, started time.Time) {
		t.Helper()
		if err := Append(filepath.Join(s.Dir, name), Event{At: started, Ev: "start"}); err != nil {
			t.Fatal(err)
		}
	}
	// By name the -10 take sorts before -2; by start it is the latest.
	take("20200131-110000", now.AddDate(0, 0, -3))    // skipped by its name
	take("20200203-160709", now.Add(-45*time.Minute)) // before since
	take("20200203-163509", now.Add(-17*time.Minute))
	take("20200203-165209-10", now.Add(900*time.Millisecond))
	take("20200203-165209", now)
	take("20200203-165209-2", now.Add(450*time.Millisecond))

	since := now.Add(-30 * time.Minute)
	recs, err := s.Recent(0, since)
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, r := range recs {
		got = append(got, filepath.Base(r.Base))
	}
	want := "20200203-165209-10 20200203-165209-2 20200203-165209 20200203-163509"
	if strings.Join(got, " ") != want {
		t.Fatalf("Recent(0) = %v, want %s", got, want)
	}

	recs, err = s.Recent(2, since)
	if err != nil {
		t.Fatal(err)
	}
	if len(recs) != 2 || filepath.Base(recs[1].Base) != "20200203-165209-2" || recs[0].Events[0].Ev != "start" {
		t.Fatalf("Recent(2) = %+v", recs)
	}
}
