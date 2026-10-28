package main

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/caoer/mega-asr/internal/audio"
	"github.com/caoer/mega-asr/internal/remote"
	"github.com/caoer/mega-asr/internal/session"
	"github.com/caoer/mega-asr/internal/store"
	"github.com/coder/websocket"
)

type heldMacInput struct {
	ch   chan []int16
	once sync.Once
}

func (m *heldMacInput) Start(context.Context) (<-chan []int16, error) {
	m.ch = make(chan []int16, 1)
	s := make([]int16, audio.Rate)
	for i := range s {
		if i%2 == 0 {
			s[i] = 3000
		} else {
			s[i] = -3000
		}
	}
	m.ch <- s
	return m.ch, nil
}
func (m *heldMacInput) Stop()    { m.once.Do(func() { close(m.ch) }) }
func (*heldMacInput) Err() error { return nil }

func TestPhoneStreamRemoteStopsKeepTakeOwnership(t *testing.T) {
	mac := &macDeliveryProbe{}
	ctrl := &session.Controller{NewSource: func() audio.Source { return &heldMacInput{} }, ASR: phoneEngine{}, Deliver: mac, UI: phoneUI{}, Store: store.Store{Dir: t.TempDir()}, ChunkDir: t.TempDir(), WholeMax: 30 * time.Second}
	s, err := remote.New(remote.Config{Listen: "127.0.0.1:0", State: t.TempDir(), Name: "fixture", Takes: phoneTakes{ctrl: ctrl, flash: func(string) {}}, Logf: t.Logf})
	if err != nil {
		t.Fatal(err)
	}
	ctrl.OnDone = func(base string) { takeDone(s, base) }
	ctx, cancel := context.WithCancel(context.Background())
	served := make(chan struct{})
	go func() { defer close(served); _ = s.Serve(ctx) }()
	t.Cleanup(func() { cancel(); <-served; ctrl.Wait() })
	one, two := phonePair(t, s, "one"), phonePair(t, s, "two")
	phoneWriteJSON(t, one, map[string]any{"type": "cmd", "id": "stream-start", "action": "start", "via": "keyboard", "sent_at": time.Now().UnixMilli(), "source": "stream", "stream": "0123456789abcdef", "format": remote.StreamFormat})
	start := phoneRead(t, one, "reply", false)
	if start["ok"] != true {
		t.Fatal(start)
	}
	phoneID := start["take"].(string)
	if st := start["state"].(map[string]any); st["owner"] != "stream" {
		t.Fatalf("start reply state %v", st)
	}
	for i, c := range []struct {
		conn *websocket.Conn
		via  string
	}{{two, "keyboard"}, {two, "iphone"}, {two, "watch"}, {one, "iphone"}, {one, "watch"}} {
		phoneWriteJSON(t, c.conn, map[string]any{"type": "cmd", "id": time.Now().Format("150405.000000000"), "action": "cancel", "via": c.via, "take": phoneID})
		if r := phoneRead(t, c.conn, "reply", false); r["ok"] != false || r["code"] != "other_take" {
			t.Fatalf("cancel %d: %v", i, r)
		}
		if id, _, ok := ctrl.Recording(); !ok || id != phoneID {
			t.Fatalf("cancel %d ended phone take", i)
		}
	}
	for i, c := range []struct {
		conn             *websocket.Conn
		via, end, stream string
		last             any
	}{
		{two, "iphone", "paste", "", nil},
		{two, "watch", "send", "", nil},
		{one, "watch", "paste", "", nil},
		{one, "watch", "send", "", nil},
		{two, "keyboard", "paste", "0123456789abcdef", uint32(3)},
	} {
		cmd := map[string]any{"type": "cmd", "id": time.Now().Format("150405.000000000"), "action": "stop", "via": c.via, "take": phoneID, "end": c.end}
		if c.stream != "" {
			cmd["stream"], cmd["last_seq"] = c.stream, c.last
		}
		phoneWriteJSON(t, c.conn, cmd)
		r := phoneRead(t, c.conn, "reply", false)
		if r["ok"] != false || r["code"] != "other_take" {
			t.Fatalf("stop %d: %v", i, r)
		}
		if id, _, ok := ctrl.Recording(); !ok || id != phoneID {
			t.Fatalf("stop %d ended phone take", i)
		}
	}
	for seq := range uint32(3) {
		b, _ := (remote.StreamRecord{Stream: 0x0123456789abcdef, Sequence: seq, Samples: makePhoneSamples(1600)}).Encode()
		if err := one.Write(ctx, websocket.MessageBinary, b); err != nil {
			t.Fatal(err)
		}
	}
	phoneWriteJSON(t, one, map[string]any{"type": "cmd", "id": "stream-stop", "action": "stop", "via": "keyboard", "take": phoneID, "stream": "0123456789abcdef", "last_seq": uint32(3)})
	if r := phoneRead(t, one, "reply", false); r["ok"] != true {
		t.Fatal(r)
	}
	tail, _ := (remote.StreamRecord{Stream: 0x0123456789abcdef, Sequence: 3, Last: true, Samples: makePhoneSamples(100)}).Encode()
	if err := one.Write(ctx, websocket.MessageBinary, tail); err != nil {
		t.Fatal(err)
	}
	phoneRead(t, one, "text", false)
	if m := phoneRead(t, two, "take", true); m["final"] != "pasted" {
		t.Fatal(m)
	}
	ctrl.Wait()
	if mac.calls.Load() != 0 {
		t.Fatalf("stream touched Mac deliverer: %d", mac.calls.Load())
	}
	// A stale stream registration on this connection must not swallow a Mac stop.
	phoneWriteJSON(t, one, map[string]any{"type": "cmd", "id": "mac-start", "action": "start", "via": "watch", "sent_at": time.Now().UnixMilli()})
	r := phoneRead(t, one, "reply", false)
	if r["ok"] != true {
		t.Fatal(r)
	}
	macID := r["state"].(map[string]any)["take"].(string)
	phoneWriteJSON(t, one, map[string]any{"type": "cmd", "id": "late-stream-stop", "action": "stop", "via": "keyboard", "take": phoneID, "stream": "0123456789abcdef", "last_seq": uint32(3)})
	if r := phoneRead(t, one, "reply", false); r["ok"] != true {
		t.Fatal(r)
	}
	if id, _, ok := ctrl.Recording(); !ok || id != macID {
		t.Fatal("late stream stop ended new Mac take")
	}
	phoneWriteJSON(t, one, map[string]any{"type": "cmd", "id": "mac-stop", "action": "stop", "via": "watch", "take": macID, "end": "send"})
	if r := phoneRead(t, one, "reply", false); r["ok"] != true {
		t.Fatal(r)
	}
	ctrl.Wait()
	if mac.calls.Load() != 3 {
		t.Fatalf("Mac capture/deliver/submit calls = %d", mac.calls.Load())
	}
}

func makePhoneSamples(n int) []int16 {
	s := make([]int16, n)
	for i := range s {
		if i%2 == 0 {
			s[i] = 3000
		} else {
			s[i] = -3000
		}
	}
	return s
}

// textProbe is a phone sink that keeps the text it gets.
type textProbe struct {
	mu   sync.Mutex
	text []string
}

func (p *textProbe) Capture() session.Target { return phoneTarget{} }
func (p *textProbe) Deliver(_ session.Target, text string) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.text = append(p.text, text)
	return nil
}
func (*textProbe) Submit(session.Target) error { return errors.New("no Mac Enter on a phone") }

// A menu quit or restart that asked to cancel the recording take stops a take
// an iPhone keyboard streams instead, and its text reaches the phone.
func TestExitCancelStopsAPhoneStreamTake(t *testing.T) {
	mac := &macDeliveryProbe{}
	dir := t.TempDir()
	ctrl := &session.Controller{ASR: phoneEngine{}, Deliver: mac, UI: phoneUI{}, Store: store.Store{Dir: dir}, ChunkDir: t.TempDir(), WholeMax: 30 * time.Second}
	sink := &textProbe{}
	id, err := ctrl.StartStream(&heldMacInput{}, sink, "keyboard")
	if err != nil {
		t.Fatal(err)
	}
	exited := make(chan []string, 1)
	endAndExit(ctrl, "cancel", id, 3*time.Second, func(left []string, answered bool) {
		if !answered {
			left = append(left, "no answer")
		}
		exited <- left
	})
	select {
	case left := <-exited:
		if len(left) != 0 {
			t.Fatalf("exit left %q", left)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("no exit")
	}
	ctrl.Wait()
	sink.mu.Lock()
	got := strings.Join(sink.text, ";")
	sink.mu.Unlock()
	if got != "fixture text" || mac.calls.Load() != 0 {
		t.Fatalf("phone %q, Mac calls %d", got, mac.calls.Load())
	}
	evs, err := store.Read(filepath.Join(dir, id))
	if err != nil {
		t.Fatal(err)
	}
	if st := store.State(evs); st.State != "pasted" {
		t.Fatalf("final %+v", st)
	}
}

// heldEngine answers every chunk once release is closed.
type heldEngine struct{ release chan struct{} }

func (e heldEngine) Transcribe(context.Context, string, bool) (string, error) {
	<-e.release
	return "fixture text", nil
}

// The owner on the wire while a phone's own stream transcribes behind an
// earlier Mac take is mac: with none recording, a Mac take on its way names
// the phase. The streaming phone keeps its own take (Unit 7 admits a
// mac-owned take only into an empty or mac-owned slot); while its stream
// records, owner is stream and take is its id.
func TestWireOwnerBehindAMacTake(t *testing.T) {
	release := make(chan struct{})
	ctrl := &session.Controller{NewSource: func() audio.Source { return &heldMacInput{} }, ASR: heldEngine{release}, Deliver: &macDeliveryProbe{}, UI: phoneUI{}, Store: store.Store{Dir: t.TempDir()}, ChunkDir: t.TempDir(), WholeMax: 30 * time.Second}
	p := phoneTakes{ctrl: ctrl, flash: func(string) {}}
	released := false
	t.Cleanup(func() {
		if !released {
			close(release)
		}
		ctrl.Wait()
	})
	check := func(when, phase, take, owner string) {
		t.Helper()
		if st := p.State(); st.Phase != phase || st.Take != take || st.Owner != owner {
			t.Fatalf("%s: wire state %+v, want %s %q owner %q", when, st, phase, take, owner)
		}
	}
	macID, err := ctrl.StartFrom("iphone")
	if err != nil {
		t.Fatal(err)
	}
	check("Mac take records", "recording", macID, "mac")
	if !ctrl.StopFrom("iphone", macID) {
		t.Fatal("Mac stop refused")
	}
	check("Mac take transcribes", "transcribing", "", "mac")
	phoneID, err := ctrl.StartStream(&heldMacInput{}, &textProbe{}, "keyboard")
	if err != nil {
		t.Fatal(err)
	}
	check("phone stream records", "recording", phoneID, "stream")
	if !ctrl.StopFrom("keyboard", phoneID) {
		t.Fatal("keyboard stop refused")
	}
	check("phone stream transcribes behind the Mac take", "transcribing", "", "mac")
	close(release)
	released = true
	ctrl.Wait()
	check("both done", "idle", "", "")
}
