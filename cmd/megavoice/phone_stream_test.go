package main

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"log"
	"net/http"
	"net/url"
	"path/filepath"
	"reflect"
	"sync/atomic"
	"testing"
	"time"

	"github.com/caoer/mega-asr/internal/audio"
	"github.com/caoer/mega-asr/internal/remote"
	"github.com/caoer/mega-asr/internal/session"
	"github.com/caoer/mega-asr/internal/store"
	"github.com/coder/websocket"
)

type phoneEngine struct {
	entered chan struct{}
	resume  chan struct{}
}

func (e phoneEngine) Transcribe(_ context.Context, _ string, _ bool) (string, error) {
	if e.entered != nil {
		close(e.entered)
		<-e.resume
	}
	return "fixture text", nil
}

type phoneUI struct{}

func (phoneUI) Show(session.View) {}
func (phoneUI) Flash(string)      {}
func (phoneUI) Alert(string)      {}

type macDeliveryProbe struct{ calls atomic.Int32 }

func (p *macDeliveryProbe) Capture() session.Target              { p.calls.Add(1); return phoneTarget{} }
func (p *macDeliveryProbe) Deliver(session.Target, string) error { p.calls.Add(1); return nil }
func (p *macDeliveryProbe) Submit(session.Target) error          { p.calls.Add(1); return nil }

func phoneWriteJSON(t *testing.T, c *websocket.Conn, v any) {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if err := c.Write(ctx, websocket.MessageText, b); err != nil {
		t.Fatal(err)
	}
}

func phoneRead(t *testing.T, c *websocket.Conn, typ string, forbidText bool) map[string]any {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	for {
		_, b, err := c.Read(ctx)
		if err != nil {
			t.Fatal(err)
		}
		var m map[string]any
		if err := json.Unmarshal(b, &m); err != nil {
			t.Fatal(err)
		}
		if forbidText && m["type"] == "text" {
			t.Fatalf("text leaked to second phone: %v", m)
		}
		if m["type"] == typ {
			return m
		}
	}
}

func phonePair(t *testing.T, s *remote.Server, name string) *websocket.Conn {
	t.Helper()
	p, err := s.NewPairing()
	deadline := time.Now().Add(3 * time.Second)
	for err != nil && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
		p, err = s.NewPairing()
	}
	if err != nil {
		t.Fatal(err)
	}
	u, err := url.Parse(p.URL)
	if err != nil {
		t.Fatal(err)
	}
	client := &http.Client{Transport: &http.Transport{TLSClientConfig: &tls.Config{InsecureSkipVerify: true}}}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	c, _, err := websocket.Dial(ctx, "wss://127.0.0.1:"+u.Query().Get("port"), &websocket.DialOptions{HTTPClient: client})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { c.CloseNow() })
	phoneWriteJSON(t, c, map[string]any{"type": "pair", "secret": u.Query().Get("s"), "device": name})
	phoneRead(t, c, "paired", false)
	phoneRead(t, c, "state", false)
	return c
}

// Real server, PhoneStream, controller and phoneSink; only the ASR is fake.
// The WAV is synthesized here and never contains a user's recording.
func TestPhoneStreamWAVToOriginatingSession(t *testing.T) {
	for _, scenario := range []string{"last_seq", "last_flag", "sequence_gap", "missing_last_seq", "session_gone"} {
		t.Run(scenario, func(t *testing.T) {
			dir := t.TempDir()
			engine := phoneEngine{}
			if scenario == "session_gone" {
				engine.entered, engine.resume = make(chan struct{}), make(chan struct{})
			}
			mac := &macDeliveryProbe{}
			ctrl := &session.Controller{
				NewSource: func() audio.Source { t.Error("Mac source opened"); return nil },
				Deliver:   mac, UI: phoneUI{}, ASR: engine, Store: store.Store{Dir: dir},
				ChunkDir: t.TempDir(), WholeMax: 30 * time.Second,
			}
			s, err := remote.New(remote.Config{Listen: "127.0.0.1:0", State: t.TempDir(), Name: "fixture", Takes: phoneTakes{ctrl: ctrl, flash: func(string) {}}, Logf: log.Printf})
			if err != nil {
				t.Fatal(err)
			}
			ctrl.OnDone = func(base string) { takeDone(s, base) }
			ctx, cancel := context.WithCancel(context.Background())
			served := make(chan struct{})
			go func() { defer close(served); _ = s.Serve(ctx) }()
			t.Cleanup(func() { cancel(); <-served })
			one, two := phonePair(t, s, "one"), phonePair(t, s, "two")
			const stream = uint64(0x0123456789abcdef)
			phoneWriteJSON(t, one, map[string]any{"type": "cmd", "id": "start", "action": "start", "via": "keyboard", "sent_at": time.Now().UnixMilli(), "source": "stream", "stream": "0123456789abcdef", "format": remote.StreamFormat})
			fixture := make([]int16, 4*remote.StreamSamples+100)
			for i := range fixture {
				if i%2 == 0 {
					fixture[i] = 3000
				} else {
					fixture[i] = -3000
				}
			}
			fixturePath := filepath.Join(t.TempDir(), "fixture.wav")
			if err := audio.SaveWAV(fixturePath, fixture); err != nil {
				t.Fatal(err)
			}
			fixture, err = audio.ReadWAV(fixturePath)
			if err != nil {
				t.Fatal(err)
			}
			write := func(seq uint32, samples []int16, last bool) {
				t.Helper()
				b, err := (remote.StreamRecord{Stream: stream, Sequence: seq, Samples: samples, Last: last}).Encode()
				if err != nil {
					t.Fatal(err)
				}
				ctx, cancel := context.WithTimeout(context.Background(), time.Second)
				defer cancel()
				if err := one.Write(ctx, websocket.MessageBinary, b); err != nil {
					t.Fatal(err)
				}
			}
			// Send the first blocks before consuming the start reply.
			for seq := range uint32(4) {
				write(seq, fixture[int(seq)*1600:int(seq+1)*1600], false)
			}
			r := phoneRead(t, one, "reply", false)
			if r["ok"] != true || r["take"] == nil {
				t.Fatalf("start %v", r)
			}
			id := r["take"].(string)
			stop := map[string]any{"type": "cmd", "id": "stop", "action": "stop", "take": id, "via": "keyboard", "stream": "0123456789abcdef", "last_seq": uint32(4)}
			switch scenario {
			case "sequence_gap":
				write(5, fixture[6400:], true)
			case "missing_last_seq":
				phoneWriteJSON(t, one, stop)
				phoneRead(t, one, "reply", false)
			case "last_flag", "session_gone":
				write(4, fixture[6400:], true)
			case "last_seq":
				// stop reaches the source first; the named last record arrives in grace.
				phoneWriteJSON(t, one, stop)
				if r := phoneRead(t, one, "reply", false); r["ok"] != true {
					t.Fatal(r)
				}
				write(4, fixture[6400:], true)
			}
			if scenario == "session_gone" {
				select {
				case <-engine.entered:
				case <-time.After(3 * time.Second):
					t.Fatal("ASR not entered")
				}
				one.CloseNow()
				deadline := time.Now().Add(2 * time.Second)
				for len(s.Phones()) == 2 && s.Phones()[0].Online+s.Phones()[1].Online != 1 && time.Now().Before(deadline) {
					time.Sleep(time.Millisecond)
				}
				close(engine.resume)
			}
			done := make(chan struct{})
			go func() { ctrl.Wait(); close(done) }()
			select {
			case <-done:
			case <-time.After(5 * time.Second):
				t.Fatal("controller did not finish")
			}
			m := phoneRead(t, two, "take", true)
			success := scenario == "last_seq" || scenario == "last_flag"
			if success {
				if m["final"] != "pasted" {
					t.Fatal(m)
				}
				if text := phoneRead(t, one, "text", false); text["take"] != id || text["text"] != "fixture text" {
					t.Fatal(text)
				}
				got, err := audio.ReadWAV(filepath.Join(dir, id) + ".wav")
				if err != nil || !reflect.DeepEqual(got, fixture) {
					t.Fatalf("stored audio: %d samples, %v", len(got), err)
				}
			} else if m["final"] != "undelivered" || m["why"] == "" {
				t.Fatal(m)
			}
			phoneWriteJSON(t, two, map[string]any{"type": "cmd", "id": "barrier", "action": "state"})
			phoneRead(t, two, "reply", true)
			if mac.calls.Load() != 0 {
				t.Fatalf("Mac delivery calls %d", mac.calls.Load())
			}
			if input, err := store.LoadInput(filepath.Join(dir, id)); err != nil || input.Source != "phone" || input.Name != "one" {
				t.Fatalf("input %+v: %v", input, err)
			}
		})
	}
}
