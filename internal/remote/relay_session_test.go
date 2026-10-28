package remote

import (
	"bytes"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/json"
	"fmt"
	"runtime"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/caoer/mega-asr/internal/relay/e2e"
	"github.com/caoer/mega-asr/internal/relay/wire"
)

// stubRoom plays the room against a server through a stub link: it
// announces sessions and passes frames the way the relay would, or the way
// a compromised relay could.
type stubRoom struct {
	t  *testing.T
	s  *Server
	f  *fakeTakes
	l  *stubLink
	id Identity
}

func newStubRoom(t *testing.T) *stubRoom {
	t.Helper()
	s, f := up(t)
	l, id := withStubRelay(t, s)
	return &stubRoom{t: t, s: s, f: f, l: l, id: id}
}

// phone pairs a phone named name directly in phones.toml with fresh relay
// keys for it and its watch.
func (r *stubRoom) phone(name string) (phone, watch ed25519.PrivateKey) {
	r.t.Helper()
	_, phone, _ = ed25519.GenerateKey(rand.Reader)
	_, watch, _ = ed25519.GenerateKey(rand.Reader)
	r.s.mu.Lock()
	defer r.s.mu.Unlock()
	if _, err := r.s.addPhone(name); err != nil {
		r.t.Fatal(err)
	}
	p := r.s.phone(name)
	p.SigPK = wire.B64(phone.Public().(ed25519.PublicKey))
	p.WatchSigPK = wire.B64(watch.Public().(ed25519.PublicKey))
	return phone, watch
}

func sidOf(n byte) wire.SID { return wire.SID{n, n, n, n, n, n, n, n} }

func (r *stubRoom) up(sid wire.SID, oneshot bool) {
	r.s.Peer(wire.Peer{SID: sid, Role: wire.RolePhone, Up: true, OneShot: oneshot})
}

// sent is what the Mac sent for sid so far, sids stripped.
func (r *stubRoom) sent(sid wire.SID) [][]byte {
	r.l.mu.Lock()
	defer r.l.mu.Unlock()
	var out [][]byte
	for _, f := range r.l.frames {
		if bytes.Equal(f[:wire.SIDLen], sid[:]) {
			out = append(out, f[wire.SIDLen:])
		}
	}
	return out
}

func (r *stubRoom) waitSent(sid wire.SID, n int) [][]byte {
	r.t.Helper()
	var got [][]byte
	waitFor(r.t, fmt.Sprintf("%d frames for %s", n, sid), func() bool { got = r.sent(sid); return len(got) >= n })
	return got
}

func (r *stubRoom) dropped(sid wire.SID) bool {
	r.l.mu.Lock()
	defer r.l.mu.Unlock()
	for _, d := range r.l.drops {
		if d == sid {
			return true
		}
	}
	return false
}

// handshake runs a phone's handshake on sid and returns the Mac's answer
// and the session.
func (r *stubRoom) handshake(sid wire.SID, sk ed25519.PrivateKey, first string) (map[string]any, *e2e.Session, []byte) {
	r.t.Helper()
	r.up(sid, false)
	msg, pd, err := e2e.Begin(r.id.Kem.PublicKey(), e2e.Phone(r.id.Room, sid), sk, []byte(first))
	if err != nil {
		r.t.Fatal(err)
	}
	r.s.Frame(sid, msg)
	rep := r.waitSent(sid, 1)[0]
	pt, sess, err := pd.Finish(rep)
	if err != nil {
		r.t.Fatalf("the Mac's handshake reply does not open: %v", err)
	}
	var m map[string]any
	if err := json.Unmarshal(pt, &m); err != nil {
		r.t.Fatal(err)
	}
	return m, sess, msg
}

// records opens frames, the next records the Mac sent, in order.
func records(t *testing.T, frames [][]byte, sess *e2e.Session) []map[string]any {
	t.Helper()
	var out []map[string]any
	for i, f := range frames {
		kind, pt, err := sess.Open(f)
		if err != nil || kind != e2e.KindJSON {
			t.Fatalf("record %d does not open in order: %v", i, err)
		}
		var m map[string]any
		json.Unmarshal(pt, &m)
		out = append(out, m)
	}
	return out
}

func (r *stubRoom) send(sid wire.SID, sess *e2e.Session, v any) {
	r.t.Helper()
	b, _ := json.Marshal(v)
	rec, err := sess.Seal(e2e.KindJSON, b)
	if err != nil {
		r.t.Fatal(err)
	}
	r.s.Frame(sid, rec)
}

// A phone's relay session: its hello carries no token, the Mac knows it by
// its signed key, answers the hello with the state, recent and now_ms, and
// then commands and pushes travel as records in order.
func TestRelayPhoneSession(t *testing.T) {
	r := newStubRoom(t)
	sk, _ := r.phone("iPhone A")
	sid := sidOf(1)
	hello, sess, _ := r.handshake(sid, sk, `{"type":"hello","device":"iPhone A"}`)
	if hello["type"] != "hello" || hello["state"].(map[string]any)["epoch"] == nil || hello["recent"] == nil || hello["now_ms"] == nil {
		t.Fatalf("hello %v", hello)
	}
	r.send(sid, sess, cmd("c1", "start", "", time.Now()))
	got := records(t, r.waitSent(sid, 2)[1:], sess)
	if got[0]["type"] != "reply" || got[0]["ok"] != true || got[0]["recent"] == nil {
		t.Fatalf("start over the relay: %v", got[0])
	}
	if l := r.f.log(); l != "start iphone" {
		t.Fatalf("calls %q", l)
	}
	if ph := r.s.Phones(); ph[0].Relay != 1 || ph[0].Direct != 0 || !ph[0].Watch {
		t.Fatalf("phones %+v", ph)
	}
	r.s.Done("take1", "sent", "")
	got = records(t, r.waitSent(sid, 3)[2:], sess)
	if got[0]["type"] != "take" || got[0]["final"] != "sent" {
		t.Fatalf("take push %v", got[0])
	}
	// the room says the phone left: the session ends, nothing more goes out
	r.s.Peer(wire.Peer{SID: sid, Up: false})
	waitFor(t, "the session gone", func() bool { return r.s.Phones()[0].Online == 0 })
	r.s.Done("take2", "sent", "")
	time.Sleep(50 * time.Millisecond)
	if n := len(r.sent(sid)); n != 3 {
		t.Fatalf("%d frames after the phone left", n)
	}
}

// What a relay session refuses: a key the Mac does not know gets a sealed
// token refusal and drop; a first message other than hello (a pair through
// the relay) gets a sealed refusal and acts on nothing; a message that does
// not open gets no answer at all, only drop.
func TestRelayHandshakeRefusals(t *testing.T) {
	r := newStubRoom(t)
	sk, _ := r.phone("iPhone A")
	_, stranger, _ := ed25519.GenerateKey(rand.Reader)

	m, _, _ := r.handshake(sidOf(1), stranger, `{"type":"hello","device":"x"}`)
	if m["type"] != "refused" || m["code"] != "token" || !r.dropped(sidOf(1)) {
		t.Fatalf("an unknown key: %v", m)
	}

	p, err := r.s.NewPairing()
	if err != nil {
		t.Fatal(err)
	}
	pair := fmt.Sprintf(`{"type":"pair","secret":%q,"device":"intruder"}`, secretOf(t, p).Get("s"))
	m, _, _ = r.handshake(sidOf(2), sk, pair)
	if m["type"] != "refused" || m["code"] != "bad_request" || !r.dropped(sidOf(2)) {
		t.Fatalf("a pair through the relay: %v", m)
	}
	r.s.mu.Lock()
	n, live := len(r.s.phones), r.s.pairWindowLocked() != nil
	r.s.mu.Unlock()
	if n != 1 || !live {
		t.Fatalf("a pair through the relay paired or spent the secret: %d phones, window %v", n, live)
	}

	m, _, _ = r.handshake(sidOf(3), sk, `{"type":"cmd","id":"x","action":"start","sent_at":1}`)
	if m["type"] != "refused" || r.f.log() != "" {
		t.Fatalf("a command as the first message: %v; calls %q", m, r.f.log())
	}

	// sealed for another sid: does not open here
	r.up(sidOf(4), false)
	msg, _, _ := e2e.Begin(r.id.Kem.PublicKey(), e2e.Phone(r.id.Room, sidOf(5)), sk, []byte(`{"type":"hello"}`))
	r.s.Frame(sidOf(4), msg)
	waitFor(t, "drop", func() bool { return r.dropped(sidOf(4)) })
	if len(r.sent(sidOf(4))) != 0 {
		t.Fatal("a handshake that does not open was answered")
	}
}

// A replayed phone handshake (PROTOCOL.md § 7.3): the Mac answers it with a
// fresh ephemeral key, so the replayer gets a reply under a key it cannot
// derive, and the recorded records do not open in the new session — the
// session is torn and the recorded command acts on nothing. A second
// handshake on a sid the Mac holds tears that session.
func TestRelayReplayedSession(t *testing.T) {
	r := newStubRoom(t)
	sk, _ := r.phone("iPhone A")
	_, sess, msg := r.handshake(sidOf(1), sk, `{"type":"hello","device":"iPhone A"}`)
	b, _ := json.Marshal(cmd("c1", "start", "", time.Now()))
	rec, _ := sess.Seal(e2e.KindJSON, b)
	r.s.Frame(sidOf(1), rec)
	r.waitSent(sidOf(1), 2)
	first := r.sent(sidOf(1))[0]
	r.s.Peer(wire.Peer{SID: sidOf(1), Up: false})
	if r.f.log() != "start iphone" {
		t.Fatalf("calls %q", r.f.log())
	}

	// the relay replays the recorded handshake and record under the same sid
	r.up(sidOf(1), false)
	r.s.Frame(sidOf(1), msg)
	waitFor(t, "a second reply", func() bool { return len(r.sent(sidOf(1))) == 3 })
	second := r.sent(sidOf(1))[2]
	if bytes.Equal(first[:33], second[:33]) {
		t.Fatal("a replayed handshake got the same ephemeral key")
	}
	r.s.Frame(sidOf(1), rec)
	waitFor(t, "the replayed record tears the session", func() bool { return r.dropped(sidOf(1)) })
	if r.f.log() != "start iphone" {
		t.Fatalf("a replayed command acted: %q", r.f.log())
	}

	// a second handshake on a held sid
	_, _, _ = r.handshake(sidOf(2), sk, `{"type":"hello","device":"iPhone A"}`)
	r.up(sidOf(2), false)
	waitFor(t, "the held session torn", func() bool { return r.dropped(sidOf(2)) })
}

// A watch request (PROTOCOL.md § 6.3): verified by the Mac against
// phones.toml, acted on once, answered with the command's reply, the state
// and recent; the same body replayed by the relay under another sid gets no
// answer; a phone's key cannot sign a watch request; a request older than
// the process is refused.
func TestRelayWatchRequest(t *testing.T) {
	r := newStubRoom(t)
	phoneSK, watchSK := r.phone("iPhone A")
	post := func(sid wire.SID, sk ed25519.PrivateKey, ts time.Time, nonce byte, c map[string]any) (*e2e.Pending, []byte) {
		b, _ := json.Marshal(c)
		var n [wire.NonceLen]byte
		n[0] = nonce
		body, pd, err := e2e.SealRequest(r.id.Kem.PublicKey(), r.id.Room, sk, uint64(ts.UnixMilli()), n, b)
		if err != nil {
			t.Fatal(err)
		}
		r.up(sid, true)
		r.s.Frame(sid, body)
		return pd, body
	}
	start := cmd("w1", "start", "", time.Now())
	start["via"] = "iphone" // the session's kind wins
	pd, body := post(sidOf(1), watchSK, time.Now(), 1, start)
	pt, _, err := pd.Finish(r.waitSent(sidOf(1), 1)[0])
	if err != nil {
		t.Fatal(err)
	}
	var rep map[string]any
	json.Unmarshal(pt, &rep)
	if rep["type"] != "reply" || rep["id"] != "w1" || rep["ok"] != true || rep["recent"] == nil || rep["now_ms"] == nil {
		t.Fatalf("watch reply %v", rep)
	}
	if r.f.log() != "start watch" {
		t.Fatalf("calls %q", r.f.log())
	}

	r.up(sidOf(2), true)
	r.s.Frame(sidOf(2), body)
	waitFor(t, "the replay dropped", func() bool { return r.dropped(sidOf(2)) })
	if len(r.sent(sidOf(2))) != 0 || r.f.log() != "start watch" {
		t.Fatal("a replayed request was answered")
	}

	post(sidOf(3), phoneSK, time.Now(), 3, cmd("w3", "state", "", time.Now()))
	waitFor(t, "a phone key's request dropped", func() bool { return r.dropped(sidOf(3)) })

	post(sidOf(4), watchSK, time.Now().Add(-30*time.Second), 4, cmd("w4", "state", "", time.Now()))
	waitFor(t, "a request older than the process dropped", func() bool { return r.dropped(sidOf(4)) })
	if len(r.sent(sidOf(3)))+len(r.sent(sidOf(4))) != 0 {
		t.Fatal("a refused request was answered")
	}
}

// The watch hedges a press (PROTOCOL.md § 6.3): the same id on its own
// relay request and through the phone's session, arriving together. The Mac
// acts once and both copies get the same answer.
func TestRelayHedgedPressActsOnce(t *testing.T) {
	r := newStubRoom(t)
	phoneSK, watchSK := r.phone("iPhone A")
	_, sess, _ := r.handshake(sidOf(1), phoneSK, `{"type":"hello","device":"iPhone A"}`)
	r.f.slow = 100 * time.Millisecond // both copies inside command() at once

	press := cmd("w1", "start", "", time.Now())
	press["via"] = "watch"
	b, _ := json.Marshal(press)
	viaPhone, err := sess.Seal(e2e.KindJSON, b)
	if err != nil {
		t.Fatal(err)
	}
	body, pd, err := e2e.SealRequest(r.id.Kem.PublicKey(), r.id.Room, watchSK, uint64(time.Now().UnixMilli()), [wire.NonceLen]byte{1}, b)
	if err != nil {
		t.Fatal(err)
	}
	r.up(sidOf(2), true)

	gate := make(chan struct{})
	var wg sync.WaitGroup
	for sid, frame := range map[wire.SID][]byte{sidOf(1): viaPhone, sidOf(2): body} {
		wg.Go(func() { <-gate; r.s.Frame(sid, frame) })
	}
	close(gate)
	wg.Wait()

	pt, _, err := pd.Finish(r.waitSent(sidOf(2), 1)[0])
	if err != nil {
		t.Fatal(err)
	}
	var watch map[string]any
	json.Unmarshal(pt, &watch)
	phone := records(t, r.waitSent(sidOf(1), 2)[1:], sess)[0]
	if l := r.f.log(); l != "start watch" {
		t.Fatalf("a hedged press acted as %q", l)
	}
	for _, k := range []string{"type", "id", "ok", "code"} {
		if fmt.Sprint(watch[k]) != fmt.Sprint(phone[k]) {
			t.Fatalf("the two copies were answered differently: watch %v, phone %v", watch, phone)
		}
	}
	if watch["ok"] != true {
		t.Fatalf("the press was refused: %v", watch)
	}
}

// Records leave in the order they are sealed while replies and pushes race:
// every record the Mac sent opens in arrival order.
func TestRelaySealOrder(t *testing.T) {
	r := newStubRoom(t)
	sk, _ := r.phone("iPhone A")
	sid := sidOf(1)
	_, sess, _ := r.handshake(sid, sk, `{"type":"hello","device":"iPhone A"}`)
	const n = 12 // 2n messages fit the connection's queue (32) even if its writer stalls
	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		for i := range n {
			b, _ := json.Marshal(cmd(fmt.Sprintf("p%d", i), "state", "", time.Now()))
			rec, _ := sess.Seal(e2e.KindJSON, b)
			r.s.Frame(sid, rec)
		}
	}()
	go func() {
		defer wg.Done()
		for i := range n {
			r.s.Done(fmt.Sprintf("t%d", i), "sent", "")
			runtime.Gosched()
		}
	}()
	wg.Wait()
	got := records(t, r.waitSent(sid, 1+2*n)[1:], sess)
	if len(got) != 2*n {
		t.Fatalf("%d records", len(got))
	}
}

// At most 16 relay sessions await a handshake; one more is dropped at once.
func TestRelayPendingCap(t *testing.T) {
	r := newStubRoom(t)
	for i := range wire.MaxHandshake {
		r.up(sidOf(byte(i+1)), i%2 == 0)
	}
	if r.dropped(sidOf(wire.MaxHandshake)) {
		t.Fatal("the 16th was dropped")
	}
	r.up(sidOf(100), false)
	if !r.dropped(sidOf(100)) {
		t.Fatal("the 17th awaiting a handshake was not dropped")
	}
}

// A relay that replays a phone's recorded handshake on new sids holds at most
// 4 sessions: one more is refused busy, and the LAN path, counted apart,
// still has all its 16 slots.
func TestRelaySessionsKeepOffDirectSlots(t *testing.T) {
	r := newStubRoom(t)
	sk, _ := r.phone("iPhone A")
	hello := `{"type":"hello","device":"iPhone A"}`
	for i := range maxRelayConns {
		if m, _, _ := r.handshake(sidOf(byte(i+1)), sk, hello); m["type"] != "hello" {
			t.Fatalf("relay session %d: %v", i+1, m)
		}
	}
	m, _, _ := r.handshake(sidOf(100), sk, hello)
	if m["type"] != "refused" || m["code"] != "busy" || !r.dropped(sidOf(100)) {
		t.Fatalf("one relay session too many: %v", m)
	}
	fp := r.s.Fingerprint()
	for range maxConns {
		c, err := dial(t, r.s, fp)
		if err != nil {
			t.Fatal(err)
		}
		defer c.CloseNow()
	}
	waitFor(t, "every direct slot taken beside the relay sessions", func() bool {
		r.s.mu.Lock()
		defer r.s.mu.Unlock()
		return r.s.count(false) == maxConns
	})
}

// A revoke after a relay session was admitted but before its hello went out:
// the phone opens the hello first, then the token refusal and bye 1008.
func TestRelayRevokeBeforeHelloSent(t *testing.T) {
	r := newStubRoom(t)
	sk, _ := r.phone("iPhone A")
	sid := sidOf(1)
	inside, release := make(chan struct{}), make(chan struct{})
	var first atomic.Bool
	r.l.mu.Lock()
	r.l.onSend = func(s wire.SID) {
		if s == sid && first.CompareAndSwap(false, true) {
			close(inside) // the hello is being sent
			<-release
		}
	}
	r.l.mu.Unlock()
	r.up(sid, false)
	msg, pd, err := e2e.Begin(r.id.Kem.PublicKey(), e2e.Phone(r.id.Room, sid), sk, []byte(`{"type":"hello","device":"iPhone A"}`))
	if err != nil {
		t.Fatal(err)
	}
	r.s.Frame(sid, msg)
	<-inside
	revoked := make(chan error, 1)
	go func() { revoked <- r.s.Revoke("iPhone A") }()
	time.Sleep(50 * time.Millisecond) // the revoke reaches the session
	close(release)
	if err := <-revoked; err != nil {
		t.Fatal(err)
	}
	frames := r.waitSent(sid, 3)
	_, sess, err := pd.Finish(frames[0])
	if err != nil {
		t.Fatalf("the first frame is not the hello: %v", err)
	}
	got := records(t, frames[1:3], sess)
	if got[0]["type"] != "refused" || got[0]["code"] != "token" || got[1]["type"] != "bye" || got[1]["code"] != float64(1008) {
		t.Fatalf("after the hello: %v", got)
	}
}

// A revoke while a phone's relay handshake is being answered: the session
// is refused with a sealed token refusal and never runs.
func TestRelayRevokeDuringHandshake(t *testing.T) {
	r := newStubRoom(t)
	sk, _ := r.phone("iPhone A")
	inside, release := make(chan struct{}), make(chan struct{})
	var first atomic.Bool
	r.s.cfg.Recent = func(int) []Recent {
		if first.CompareAndSwap(false, true) {
			close(inside)
			<-release
		}
		return nil
	}
	sid := sidOf(1)
	r.up(sid, false)
	msg, pd, err := e2e.Begin(r.id.Kem.PublicKey(), e2e.Phone(r.id.Room, sid), sk, []byte(`{"type":"hello","device":"iPhone A"}`))
	if err != nil {
		t.Fatal(err)
	}
	r.s.Frame(sid, msg)
	<-inside
	if err := r.s.Revoke("iPhone A"); err != nil {
		t.Fatal(err)
	}
	close(release)
	pt, _, err := pd.Finish(r.waitSent(sid, 1)[0])
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]any
	json.Unmarshal(pt, &m)
	if m["type"] != "refused" || m["code"] != "token" {
		t.Fatalf("a phone revoked during its handshake: %v", m)
	}
	waitFor(t, "drop", func() bool { return r.dropped(sid) })
	r.s.mu.Lock()
	n := len(r.s.conns)
	r.s.mu.Unlock()
	if n != 0 {
		t.Fatalf("%d connections after the refusal", n)
	}
}

// Revoke ends a phone's relay sessions with a sealed token refusal, then
// bye 1008, then drop; the watch's later requests are refused.
func TestRelayRevoke(t *testing.T) {
	r := newStubRoom(t)
	sk, watchSK := r.phone("iPhone A")
	sid := sidOf(1)
	_, sess, _ := r.handshake(sid, sk, `{"type":"hello","device":"iPhone A"}`)
	if err := r.s.Revoke("iPhone A"); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "drop", func() bool { return r.dropped(sid) })
	got := records(t, r.waitSent(sid, 3)[1:], sess)
	if got[0]["type"] != "refused" || got[0]["code"] != "token" || got[1]["type"] != "bye" || got[1]["code"] != float64(1008) {
		t.Fatalf("revoke over the relay: %v", got)
	}
	b, _ := json.Marshal(cmd("w1", "state", "", time.Now()))
	body, _, _ := e2e.SealRequest(r.id.Kem.PublicKey(), r.id.Room, watchSK, uint64(time.Now().UnixMilli()), [wire.NonceLen]byte{9}, b)
	r.up(sidOf(2), true)
	r.s.Frame(sidOf(2), body)
	waitFor(t, "the revoked watch dropped", func() bool { return r.dropped(sidOf(2)) })
	if len(r.sent(sidOf(2))) != 0 {
		t.Fatal("a revoked watch was answered")
	}
}

func TestWatchCommandEndedWhileQueuedDoesNotAct(t *testing.T) {
	r := newStubRoom(t)
	_, sk := r.phone("iPhone A")
	sid := sidOf(61)
	b, _ := json.Marshal(cmd("expired-watch", "start", "", time.Now()))
	var nonce [wire.NonceLen]byte
	nonce[0] = 61
	body, _, err := e2e.SealRequest(r.id.Kem.PublicKey(), r.id.Room, sk, uint64(time.Now().UnixMilli()), nonce, b)
	if err != nil {
		t.Fatal(err)
	}
	r.s.cmdMu.Lock()
	r.up(sid, true)
	r.s.Frame(sid, body)
	r.s.Peer(wire.Peer{SID: sid, Up: false, OneShot: true})
	r.s.cmdMu.Unlock()
	r.waitSent(sid, 1)
	if calls := r.f.log(); calls != "" {
		t.Fatalf("expired request acted: %s", calls)
	}
}
