//go:build darwin

package main

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/caoer/mega-asr/internal/app"
	"github.com/caoer/mega-asr/internal/audio"
	"github.com/caoer/mega-asr/internal/ctl"
	"github.com/caoer/mega-asr/internal/deliver"
	"github.com/caoer/mega-asr/internal/session"
	"github.com/caoer/mega-asr/internal/store"
)

// fakeDelivery records each delivery as "target|text"; no Mac call is made.
type fakeDelivery struct {
	mu  sync.Mutex
	got []string
}

func (d *fakeDelivery) Capture() session.Target { return &deliver.Target{Clipboard: true} }
func (d *fakeDelivery) Deliver(t session.Target, text string) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.got = append(d.got, t.(*deliver.Target).String()+"|"+text)
	return nil
}
func (d *fakeDelivery) Submit(session.Target) error { return nil }

// againASR answers every chunk with one text.
type againASR string

func (a againASR) Transcribe(context.Context, string, bool) (string, error) { return string(a), nil }

// commandServer is a server whose store holds one delivered take,
// 20200417-104200, and whose config file runs funasr with compare off.
func commandServer(t *testing.T) (s *server, base string, del *fakeDelivery) {
	t.Helper()
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	dir := t.TempDir()
	base = filepath.Join(dir, "20200417-104200")
	tone := make([]int16, audio.Rate) // not digital silence, which is never decoded
	for i := range tone {
		tone[i] = int16(1000 * (i%64 - 32))
	}
	if err := audio.SaveWAV(base+".wav", tone); err != nil {
		t.Fatal(err)
	}
	if err := (store.Store{Dir: dir}).SaveText(base, "a take written for the test", "a take written for the test"); err != nil {
		t.Fatal(err)
	}
	at := time.Date(2020, 4, 17, 10, 42, 0, 0, time.Local)
	for _, e := range []store.Event{
		{At: at, Ev: "start", Trigger: "tap"},
		{At: at.Add(time.Second), Ev: "stop", Kind: "tap", DurS: 1},
		{At: at.Add(2 * time.Second), Ev: "text", Engine: "funasr", Chars: 27},
		{At: at.Add(3 * time.Second), Ev: "deliver", N: 1, Via: "auto", OK: true, Submit: "none"},
	} {
		if err := store.Append(base, e); err != nil {
			t.Fatal(err)
		}
	}
	cfgPath := filepath.Join(t.TempDir(), "config.toml")
	if err := os.WriteFile(cfgPath, []byte("[asr]\nengine = \"funasr\"\n[compare]\non = false\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg := app.Default()
	cfg.Store.Data = dir
	del = &fakeDelivery{}
	s = &server{
		cfg:  cfg,
		opts: app.LoadOpts{Path: cfgPath},
		ctrl: &session.Controller{Store: store.Store{Dir: dir}, Deliver: del, UI: quietUI{}, ASR: againASR("the take again"), ChunkDir: t.TempDir()},
		cmds: &takeCommands{ctrl: &fakeCtl{}, flash: func(string) {}, exit: func(int) {}},
	}
	return s, base, del
}

// ask sends one request to the server's socket handler and decodes its data.
func askServer(t *testing.T, s *server, cmd, args string, out any) ctl.Response {
	t.Helper()
	resp := s.handle(ctl.Request{Cmd: cmd, Args: json.RawMessage(args)})
	if resp.OK && out != nil {
		if err := json.Unmarshal(resp.Data, out); err != nil {
			t.Fatal(err)
		}
	}
	return resp
}

// resend, retranscribe, takes and events over the control socket: a resend
// is via cli with its caller, a cloud engine the config does not run is
// refused, the local engine's answer is appended, and the list and the
// record show both.
func TestTakeCommandsOverTheSocket(t *testing.T) {
	s, base, del := commandServer(t)
	var de store.Event
	if r := askServer(t, s, "resend", `{"id":"20200417-104200","to":"clipboard","caller":{"pid":4242,"process":"zsh"}}`, &de); !r.OK {
		t.Fatalf("resend: %s", r.Error)
	}
	if de.Via != "cli" || de.Caller == nil || de.Caller.PID != 4242 || de.Caller.Process != "zsh" || de.N != 2 ||
		de.Target == nil || de.Target.Kind != "clipboard" || !de.OK {
		t.Fatalf("deliver line %+v", de)
	}
	if got := strings.Join(del.got, ";"); got != "剪贴板|a take written for the test" {
		t.Fatalf("delivered %q", got)
	}
	if r := askServer(t, s, "retranscribe", `{"id":"20200417-104200","engine":"doubao"}`, nil); r.OK || !strings.Contains(r.Error, session.ErrNotNamed.Error()) {
		t.Fatalf("a cloud engine the config does not run: %+v", r)
	}
	var re store.Event
	if r := askServer(t, s, "retranscribe", `{"id":"20200417-104200","engine":"funasr"}`, &re); !r.OK || re.Text != "the take again" || re.N != 1 {
		t.Fatalf("retranscribe: %+v %+v", r, re)
	}
	var rows []takeRow
	if r := askServer(t, s, "takes", `{}`, &rows); !r.OK || len(rows) != 1 || rows[0].ID != "20200417-104200" || rows[0].State != "pasted" {
		t.Fatalf("takes: %+v %+v", r, rows)
	}
	var evs []store.Event
	if r := askServer(t, s, "events", `{"id":"20200417-104200"}`, &evs); !r.OK || len(evs) != 6 || evs[4].Via != "cli" || evs[5].Ev != "retranscribe" {
		t.Fatalf("events: %+v %+v", r, evs)
	}
	if r := askServer(t, s, "events", `{"id":"../20200417-104200"}`, nil); r.OK {
		t.Fatal("events of a path")
	}
	if got := len(mustLines(t, base)); got != 6 {
		t.Fatalf("record holds %d lines", got)
	}
}

// A line the record cannot take comes back as an error with the line, so
// the command prints what went out and says it is not recorded.
func TestResendNotRecordedOverTheSocket(t *testing.T) {
	s, base, _ := commandServer(t)
	if err := os.Remove(base + ".events.jsonl"); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(base+".events.jsonl", 0o755); err != nil {
		t.Fatal(err)
	}
	r := askServer(t, s, "resend", `{"id":"20200417-104200","to":"clipboard"}`, nil)
	var de store.Event
	if r.OK || !strings.Contains(r.Error, session.ErrNotRecorded.Error()) || json.Unmarshal(r.Data, &de) != nil || !de.OK {
		t.Fatalf("%+v", r)
	}
}

func mustLines(t *testing.T, base string) []store.Event {
	t.Helper()
	evs, err := store.Read(base)
	if err != nil {
		t.Fatal(err)
	}
	return evs
}
