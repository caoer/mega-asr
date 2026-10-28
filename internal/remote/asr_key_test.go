package remote

import (
	"context"
	"crypto/ed25519"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/caoer/mega-asr/internal/relay/e2e"
	"github.com/caoer/mega-asr/internal/relay/wire"
)

const fakePlanKey = "fake-plan-key-0123456789"

var fakeKey = ASRKey{Key: fakePlanKey, URL: "wss://openspeech.bytedance.com/api/v3/plan/sauc/bigmodel_async", ResourceID: "volc.seedasr.sauc.duration"}

// logLines collects a server's log, to show the key never reaches it.
type logLines struct {
	mu    sync.Mutex
	lines []string
}

func (l *logLines) logf(format string, a ...any) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.lines = append(l.lines, fmt.Sprintf(format, a...))
}

func (l *logLines) text() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return strings.Join(l.lines, "\n")
}

// A paired phone gets the key on its session, direct after pair and after
// hello; a request with no id or another engine is refused; a Mac that
// cannot read the key says so; the key is in no log line and no remembered
// reply.
func TestASRKeyDirect(t *testing.T) {
	logs := &logLines{}
	var fail atomic.Pointer[error]
	s := upConfig(t, Config{Logf: logs.logf, ASRKey: func(engine string) (ASRKey, error) {
		if engine != "doubao" {
			t.Errorf("engine %q", engine)
		}
		if err := fail.Load(); err != nil {
			return fakeKey, *err
		}
		return fakeKey, nil
	}})
	p, err := s.NewPairing()
	if err != nil {
		t.Fatal(err)
	}
	q := secretOf(t, p)
	c, err := dial(t, s, q.Get("fp"))
	if err != nil {
		t.Fatal(err)
	}
	send(t, c, map[string]any{"type": "pair", "secret": q.Get("s"), "device": "iPhone 3fa2"})
	token := recv(t, c, "paired")["token"].(string)

	send(t, c, map[string]any{"type": "asr_key", "id": "k1", "engine": "doubao"})
	got := recv(t, c, "asr_key")
	if got["id"] != "k1" || got["ok"] != true || got["engine"] != "doubao" || got["key"] != fakePlanKey ||
		got["url"] != fakeKey.URL || got["resource_id"] != fakeKey.ResourceID || got["now_ms"] == nil {
		t.Fatalf("answer %v", got)
	}
	c.CloseNow()

	h, err := dial(t, s, s.Fingerprint())
	if err != nil {
		t.Fatal(err)
	}
	defer h.CloseNow()
	send(t, h, map[string]any{"type": "hello", "token": token, "device": "iPhone 3fa2"})
	recv(t, h, "hello")
	for _, tc := range []struct {
		msg        map[string]any
		code, says string
	}{
		{map[string]any{"type": "asr_key", "id": "", "engine": "doubao"}, "bad_request", "id"},
		{map[string]any{"type": "asr_key", "id": "k2", "engine": "openai"}, "bad_request", "openai"},
	} {
		send(t, h, tc.msg)
		got := recv(t, h, "asr_key")
		if got["ok"] != false || got["code"] != tc.code || !strings.Contains(got["reason"].(string), tc.says) || got["key"] != nil {
			t.Fatalf("%v answered %v", tc.msg, got)
		}
	}
	send(t, h, map[string]any{"type": "asr_key", "id": "k3", "engine": "doubao"})
	if got := recv(t, h, "asr_key"); got["key"] != fakePlanKey {
		t.Fatalf("after hello %v", got)
	}
	unreadable := errors.New("open /nowhere/.env: no such file or directory")
	fail.Store(&unreadable)
	send(t, h, map[string]any{"type": "asr_key", "id": "k4", "engine": "doubao"})
	got = recv(t, h, "asr_key")
	if got["ok"] != false || got["code"] != "no_key" || got["reason"] != "Mac 读不到豆包密钥：open /nowhere/.env: no such file or directory" || got["key"] != nil {
		t.Fatalf("unreadable key %v", got)
	}

	if strings.Contains(logs.text(), fakePlanKey[:8]) {
		t.Fatalf("the key reached the log:\n%s", logs.text())
	}
	if !strings.Contains(logs.text(), `doubao key sent to "iPhone 3fa2" (direct)`) {
		t.Fatalf("log:\n%s", logs.text())
	}
	s.cmdMu.Lock()
	defer s.cmdMu.Unlock()
	for id, r := range s.replies {
		b, _ := json.Marshal(r)
		if strings.Contains(string(b), fakePlanKey[:8]) {
			t.Fatalf("reply %s remembers the key", id)
		}
	}
}

func TestASRKeyNoneConfigured(t *testing.T) {
	s, _ := up(t)
	c := paired(t, s)
	send(t, c, map[string]any{"type": "asr_key", "id": "k1", "engine": "doubao"})
	if got := recv(t, c, "asr_key"); got["ok"] != false || got["code"] != "no_key" || got["key"] != nil {
		t.Fatalf("answer %v", got)
	}
}

// Over the relay the phone's session gets the key sealed; a watch's one-shot
// request asking for it gets no key.
func TestASRKeyRelay(t *testing.T) {
	r := newStubRoom(t)
	r.s.cfg.ASRKey = func(string) (ASRKey, error) { return fakeKey, nil }
	phoneSK, watchSK := r.phone("iPhone A")
	sid := sidOf(1)
	_, sess, _ := r.handshake(sid, phoneSK, `{"type":"hello","device":"iPhone A"}`)
	r.send(sid, sess, map[string]any{"type": "asr_key", "id": "k1", "engine": "doubao"})
	got := records(t, r.waitSent(sid, 2)[1:], sess)
	if got[0]["type"] != "asr_key" || got[0]["ok"] != true || got[0]["key"] != fakePlanKey {
		t.Fatalf("relay answer %v", got[0])
	}
	for _, f := range r.sent(sid) {
		if strings.Contains(string(f), fakePlanKey[:8]) {
			t.Fatal("the key crossed the relay unsealed")
		}
	}

	b, _ := json.Marshal(map[string]any{"type": "asr_key", "id": "w1", "engine": "doubao"})
	var n [wire.NonceLen]byte
	n[0] = 9
	body, pd, err := e2e.SealRequest(r.id.Kem.PublicKey(), r.id.Room, ed25519.PrivateKey(watchSK), uint64(time.Now().UnixMilli()), n, b)
	if err != nil {
		t.Fatal(err)
	}
	r.up(sidOf(2), true)
	r.s.Frame(sidOf(2), body)
	pt, _, err := pd.Finish(r.waitSent(sidOf(2), 1)[0])
	if err != nil {
		t.Fatal(err)
	}
	var rep map[string]any
	json.Unmarshal(pt, &rep)
	if rep["type"] != "reply" || rep["ok"] != false || strings.Contains(string(pt), fakePlanKey[:8]) {
		t.Fatalf("watch request answered %s", pt)
	}
}

// upConfig serves a server made from cfg, its listener, state, name and
// takes filled in as up's are.
func upConfig(t *testing.T, cfg Config) *Server {
	t.Helper()
	cfg.Listen, cfg.State, cfg.Name, cfg.Takes = "127.0.0.1:0", t.TempDir(), "testmac", &fakeTakes{}
	s, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { s.Serve(ctx); close(done) }()
	t.Cleanup(func() { cancel(); <-done })
	waitFor(t, "the server listening", func() bool {
		s.mu.Lock()
		defer s.mu.Unlock()
		return s.addr != ""
	})
	return s
}
