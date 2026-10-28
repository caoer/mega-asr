package session

import (
	"errors"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/caoer/mega-asr/internal/audio"
)

func TestStartStreamUsesOnlyItsSourceAndSink(t *testing.T) {
	r, base := rig2020(t, &fakeSource{n: audio.Rate}, "phone 派森")
	sink := &fakeDeliverer{front: "iPhone 键盘"}
	r.c.NewSource = func() audio.Source { t.Error("opened Mac microphone"); return r.src.clone() }
	r.c.NewBackup = func(audio.Source) audio.Source { t.Error("opened Mac backup"); return nil }
	id, err := r.c.StartStream(&fakeSource{n: audio.Rate}, sink, "keyboard")
	if err != nil || id != filepath.Base(base) {
		t.Fatalf("start = %q, %v", id, err)
	}
	if _, err := r.c.StartFrom("iphone"); !errors.Is(err, ErrRecording) {
		t.Fatalf("Mac start during phone take: %v", err)
	}
	if !r.c.StopFrom("keyboard", id) {
		t.Fatal("stop failed")
	}
	r.wait(t)
	if got := sink.got(); !strings.Contains(got, "phone Python") || r.del.got() != "" {
		t.Fatalf("sink %q, Mac %q", got, r.del.got())
	}
	if got := state(base); got != "pasted/" {
		t.Fatalf("final %q", got)
	}
}

func TestMacTakeAfterFailedPhoneStream(t *testing.T) {
	r, base := rig2020(t, &fakeSource{n: audio.Rate}, "phone partial", "Mac text")
	r.del.trace, r.del.received = true, "whole"
	sink := &fakeDeliverer{front: "iPhone 键盘"}
	src := &described{fakeSource: &fakeSource{n: audio.Rate, cut: true, cutErr: errors.New("phone sequence gap")}, in: audio.TakeInput{Source: "phone", Name: "fixture phone"}}
	if _, err := r.c.StartStream(src, sink, "keyboard"); err != nil {
		t.Fatal(err)
	}
	r.wait(t)
	if r.c.State() != Idle {
		t.Fatal("failed phone source held recording")
	}
	id, err := r.c.StartFrom("watch")
	if err != nil {
		t.Fatal(err)
	}
	if !r.c.SendFrom("watch", id) {
		t.Fatal("Mac stop failed")
	}
	r.wait(t)
	if sink.got() != "" || r.del.got() != "telegram|Mac text;telegram|⏎" {
		t.Fatalf("phone %q, Mac %q", sink.got(), r.del.got())
	}
	if state(base) != "undelivered/phone sequence gap" || state(filepath.Join(r.dir, id)) != "sent/" {
		t.Fatalf("phone %s, Mac %s", state(base), state(filepath.Join(r.dir, id)))
	}
	if r.del.starts != 1 || len(r.del.captured) != 1 || !r.del.allReleased() {
		t.Fatal("Mac source/target ownership did not recover")
	}
	if !strings.Contains(strings.Join(r.del.calls, ","), "telegram:received") {
		t.Fatal("Mac readback was not restored")
	}
}

func TestStoppedPhoneDecodeOverlapsNewMacTake(t *testing.T) {
	r, base := rig2020(t, &fakeSource{n: audio.Rate})
	r.c.WholeMax = 30 * time.Second
	r.asr.byTake = map[string]string{filepath.Base(base): "phone text", filepath.Base(base) + "-2": "Mac text"}
	r.asr.block = make(chan struct{})
	sink := &fakeDeliverer{front: "iPhone 键盘"}
	src := &described{fakeSource: &fakeSource{n: audio.Rate}, in: audio.TakeInput{Source: "phone", Name: "fixture phone"}}
	phoneID, err := r.c.StartStream(src, sink, "keyboard")
	if err != nil {
		t.Fatal(err)
	}
	if !r.c.StopFrom("keyboard", phoneID) {
		t.Fatal("phone stop failed")
	}
	macID, err := r.c.StartFrom("watch")
	if err != nil {
		t.Fatal(err)
	}
	if !r.c.StopFrom("watch", macID) {
		t.Fatal("Mac stop failed")
	}
	close(r.asr.block)
	r.wait(t)
	if sink.got() != "iPhone 键盘|phone text" || r.del.got() != "telegram|Mac text" {
		t.Fatalf("phone %q, Mac %q", sink.got(), r.del.got())
	}
	if state(base) != "pasted/" || state(filepath.Join(r.dir, macID)) != "pasted/" {
		t.Fatalf("phone %s, Mac %s", state(base), state(filepath.Join(r.dir, macID)))
	}
	if r.del.starts != 1 || len(r.del.captured) != 1 || !r.del.allReleased() || !sink.allReleased() {
		t.Fatal("per-take source/sink isolation failed")
	}
}

func TestStartStreamSinkErrorIsUndelivered(t *testing.T) {
	r, base := rig2020(t, &fakeSource{}, "saved text")
	sink := &fakeDeliverer{front: "iPhone 键盘", deliverErr: errors.New("phone session ended")}
	id, err := r.c.StartStream(&fakeSource{n: audio.Rate}, sink, "keyboard")
	if err != nil {
		t.Fatal(err)
	}
	r.c.StopFrom("keyboard", id)
	r.wait(t)
	if got := state(base); got != "undelivered/deliver_failed" || r.del.got() != "" {
		t.Fatalf("final %q, Mac %q", got, r.del.got())
	}
}

func TestStartStreamSourceErrorNeverDeliversPartialText(t *testing.T) {
	for _, n := range []int{1600, audio.Rate} {
		t.Run(strconv.Itoa(n), func(t *testing.T) {
			r, base := rig2020(t, &fakeSource{}, "partial text")
			sink := &fakeDeliverer{front: "iPhone 键盘"}
			src := &fakeSource{n: n, cut: true, cutErr: errors.New("phone stream sequence gap")}
			if _, err := r.c.StartStream(src, sink, "keyboard"); err != nil {
				t.Fatal(err)
			}
			r.wait(t)
			if got := state(base); got != "undelivered/phone stream sequence gap" || sink.got() != "" || r.del.got() != "" {
				t.Fatalf("final %q, phone %q, Mac %q", got, sink.got(), r.del.got())
			}
		})
	}
}

// The Mac's own paths — Return, Esc, the tap key, its menu and control
// socket, a phone's or watch's iphone/watch command — never end a take a
// phone streams; only the keyboard connection does (remote checks it is the
// owner), and the exit drain may stop it but not cancel it.
func TestMacPathsNeverEndAPhoneStreamTake(t *testing.T) {
	r, base := rig2020(t, &fakeSource{n: audio.Rate}, "phone 派森")
	sink := &fakeDeliverer{front: "iPhone 键盘"}
	id, err := r.c.StartStream(&fakeSource{n: audio.Rate}, sink, "keyboard")
	if err != nil {
		t.Fatal(err)
	}
	if used, why := r.c.EnterWhy(); used || why != OwnerStream {
		t.Fatalf("Return: used %v, why %q", used, why)
	}
	r.c.ToggleAt("tap", time.Now())
	r.c.Cancel()
	for _, via := range []string{"iphone", "watch", "ctl", "menu", ""} {
		if r.c.StopFrom(via, id) || r.c.SendFrom(via, id) || r.c.CancelFrom(via, id) {
			t.Fatalf("via %q ended the phone's take", via)
		}
	}
	if r.c.CancelFrom(ViaExit, id) {
		t.Fatal("the exit drain cancelled the phone's take")
	}
	if r.c.State() != Recording || r.c.KeyPhase() != Idle {
		t.Fatalf("state %s, key phase %s", r.c.State(), r.c.KeyPhase())
	}
	if ph, got, _, owner := r.c.Published(); ph != Recording || got != id || owner != OwnerStream {
		t.Fatalf("published %s %q %q", ph, got, owner)
	}
	if n := strings.Count(r.ui.String(), "flash:"+MsgStreamTake()); n != 1 {
		t.Fatalf("%d stream flashes; ui %s", n, r.ui)
	}
	if !r.c.StopFrom("keyboard", id) {
		t.Fatal("keyboard stop refused")
	}
	r.wait(t)
	if got := sink.got(); got != "iPhone 键盘|phone Python" || r.del.got() != "" {
		t.Fatalf("sink %q, Mac %q", got, r.del.got())
	}
	if got := state(base); got != "pasted/" {
		t.Fatalf("final %q", got)
	}
}

// A Mac take keeps its Return: while a phone's take records or transcribes
// after it, the key phase is the Mac take's, and Return turns that take into
// a send and leaves the phone's take as it is. Published names the
// recording take's owner, then the Mac's while its take is on its way, and
// OnState sees each change of phase, owner or key phase stored.
func TestReturnKeepsTheMacTake(t *testing.T) {
	r, base := rig2020(t, &fakeSource{n: audio.Rate})
	r.c.WholeMax = 30 * time.Second
	phoneBase := base + "-2"
	r.asr.byTake = map[string]string{filepath.Base(base): "mac text", filepath.Base(phoneBase): "phone text"}
	var seen []string
	r.c.OnState = func(s State) {
		ph, _, _, owner := r.c.Published()
		if ph != s {
			t.Errorf("OnState %s, published %s", s, ph)
		}
		seen = append(seen, s.String()+"/"+owner+"/"+r.c.KeyPhase().String())
	}
	r.asr.block = make(chan struct{})
	r.c.Toggle()
	r.c.Toggle() // the Mac take transcribes, held by the ASR
	sink := &fakeDeliverer{front: "iPhone 键盘"}
	id, err := r.c.StartStream(&fakeSource{n: audio.Rate}, sink, "keyboard")
	if err != nil {
		t.Fatal(err)
	}
	if ph, _, _, owner := r.c.Published(); ph != Recording || owner != OwnerStream || r.c.KeyPhase() != Transcribing {
		t.Fatalf("phone take recording: %s %q, key phase %s", ph, owner, r.c.KeyPhase())
	}
	if used, why := r.c.EnterWhy(); !used {
		t.Fatalf("Return while the phone's take records: %s", why)
	}
	if r.c.State() != Recording {
		t.Fatal("Return ended the phone's take")
	}
	if !r.c.StopFrom("keyboard", id) {
		t.Fatal("keyboard stop refused")
	}
	if ph, _, _, owner := r.c.Published(); ph != Transcribing || owner != OwnerMac || r.c.KeyPhase() != Transcribing {
		t.Fatalf("both transcribing: %s %q, key phase %s", ph, owner, r.c.KeyPhase())
	}
	if used, _ := r.c.EnterWhy(); !used {
		t.Fatal("a second Return found no Mac take")
	}
	close(r.asr.block)
	r.wait(t)
	if got := r.del.got(); got != "telegram|mac text;telegram|⏎" {
		t.Fatalf("Mac %q", got)
	}
	if got := sink.got(); got != "iPhone 键盘|phone text" {
		t.Fatalf("phone %q", got)
	}
	if state(base) != "sent/" || state(phoneBase) != "pasted/" {
		t.Fatalf("Mac %s, phone %s", state(base), state(phoneBase))
	}
	// After the Mac take delivers, the phone's take may start delivering
	// before the Mac take leaves the list (done closes its turn first).
	got := strings.Join(seen, ",")
	want := "recording/mac/recording,transcribing/mac/transcribing,recording/stream/transcribing,transcribing/mac/transcribing,delivering/mac/delivering,"
	if !strings.HasPrefix(got, want) || !strings.HasSuffix(got, "/stream/idle,idle//idle") {
		t.Fatalf("OnState saw %s\nwant %s…/stream/idle,idle//idle", got, want)
	}
}

// The menu's quit and restart stop a take a phone streams — an exit that
// asked to cancel it too — and its text reaches the phone.
func TestExitStopsAStreamTake(t *testing.T) {
	r, base := rig2020(t, &fakeSource{n: audio.Rate}, "phone text")
	sink := &fakeDeliverer{front: "iPhone 键盘"}
	id, err := r.c.StartStream(&fakeSource{n: audio.Rate}, sink, "keyboard")
	if err != nil {
		t.Fatal(err)
	}
	if r.c.CancelFrom(ViaExit, id) || !r.c.StopFrom(ViaExit, "") {
		t.Fatal("the exit drain cancelled, or did not stop, the phone's take")
	}
	r.wait(t)
	if got := sink.got(); got != "iPhone 键盘|phone text" || r.del.got() != "" {
		t.Fatalf("sink %q, Mac %q", got, r.del.got())
	}
	if got := state(base); got != "pasted/" {
		t.Fatalf("final %q", got)
	}
}

// A Return after an Esc is the app's, whatever a phone does next: a phone
// take starting, stopping or being cancelled after the cancel leaves the
// earlier Mac take a paste. Only the Mac's own takes set or clear it.
func TestReturnAfterACancelStaysTheApps(t *testing.T) {
	for _, phoneEnd := range []string{"stop", "cancel"} {
		t.Run(phoneEnd, func(t *testing.T) {
			r, base := rig2020(t, &fakeSource{n: audio.Rate})
			r.c.WholeMax = 30 * time.Second
			r.asr.byTake = map[string]string{filepath.Base(base): "mac text", filepath.Base(base) + "-2": "cancelled text", filepath.Base(base) + "-3": "phone text"}
			r.asr.block = make(chan struct{})
			r.c.Toggle()
			r.c.Toggle() // Mac take A transcribes
			r.c.Toggle()
			r.c.Cancel() // Mac take B: Esc
			sink := &fakeDeliverer{front: "iPhone 键盘"}
			id, err := r.c.StartStream(&fakeSource{n: audio.Rate}, sink, "keyboard")
			if err != nil {
				t.Fatal(err)
			}
			if used, why := r.c.EnterWhy(); used || why != "after a cancel" {
				t.Fatalf("Return while the phone records: used %v, why %q", used, why)
			}
			if phoneEnd == "stop" && !r.c.StopFrom("keyboard", id) || phoneEnd == "cancel" && !r.c.CancelFrom("keyboard", id) {
				t.Fatalf("keyboard %s refused", phoneEnd)
			}
			if used, why := r.c.EnterWhy(); used || why != "after a cancel" {
				t.Fatalf("Return after the phone's %s: used %v, why %q", phoneEnd, used, why)
			}
			close(r.asr.block)
			r.wait(t)
			if got := r.del.got(); got != "telegram|mac text" {
				t.Fatalf("Mac %q: the cancel's Return reached take A", got)
			}
		})
	}
}

// A phone take cancelled from the keyboard does not make the next Return
// the app's: it still sends the Mac take on its way.
func TestPhoneCancelLeavesTheMacReturn(t *testing.T) {
	r, base := rig2020(t, &fakeSource{n: audio.Rate})
	r.c.WholeMax = 30 * time.Second
	r.asr.byTake = map[string]string{filepath.Base(base): "mac text", filepath.Base(base) + "-2": "phone text"}
	r.asr.block = make(chan struct{})
	r.c.Toggle()
	r.c.Toggle() // Mac take A transcribes
	id, err := r.c.StartStream(&fakeSource{n: audio.Rate}, &fakeDeliverer{front: "iPhone 键盘"}, "keyboard")
	if err != nil {
		t.Fatal(err)
	}
	if !r.c.CancelFrom("keyboard", id) {
		t.Fatal("keyboard cancel refused")
	}
	if used, why := r.c.EnterWhy(); !used {
		t.Fatalf("Return after a phone cancel: %s", why)
	}
	close(r.asr.block)
	r.wait(t)
	if got := r.del.got(); got != "telegram|mac text;telegram|⏎" {
		t.Fatalf("Mac %q", got)
	}
}
