package remote

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/coder/websocket"

	"github.com/caoer/mega-asr/internal/relay/auth"
	"github.com/caoer/mega-asr/internal/relay/wire"
)

// stubLink is a link whose room keeps a device list and records frames.
type stubLink struct {
	mu      sync.Mutex
	devices map[string]wire.Role
	frames  [][]byte
	drops   []wire.SID
	changes int
	onSend  func(wire.SID) // called before a frame is recorded; may block
}

func newStubLink() *stubLink { return &stubLink{devices: map[string]wire.Role{}} }

func (l *stubLink) Send(sid wire.SID, data []byte) error {
	l.mu.Lock()
	hook := l.onSend
	l.mu.Unlock()
	if hook != nil {
		hook(sid)
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	l.frames = append(l.frames, wire.JoinSID(sid, data))
	return nil
}

func (l *stubLink) Drop(sid wire.SID) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.drops = append(l.drops, sid)
}

func (l *stubLink) Devices(_ context.Context, c wire.DevicesChange) ([]wire.Device, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if len(c.Add)+len(c.Remove) > 0 {
		l.changes++
	}
	for _, d := range c.Add {
		l.devices[wire.B64(d.Key)] = d.Role
	}
	for _, k := range c.Remove {
		delete(l.devices, wire.B64(k))
	}
	var out []wire.Device
	for k, r := range l.devices {
		b, _ := wire.Unb64(k, ed25519.PublicKeySize)
		out = append(out, wire.Device{Role: r, Key: b})
	}
	sort.Slice(out, func(i, j int) bool { return wire.B64(out[i].Key) < wire.B64(out[j].Key) })
	return out, nil
}

func (l *stubLink) Switch(wire.RoomID, ed25519.PrivateKey) {}
func (l *stubLink) Status() (bool, string)                 { return true, "" }

// list is the room's devices as "role key" lines.
func (l *stubLink) list() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	var out []string
	for k, r := range l.devices {
		out = append(out, string(r)+" "+k)
	}
	sort.Strings(out)
	return strings.Join(out, "\n")
}

// waitFor polls cond for up to 3 s.
func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	for range 300 {
		if cond() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

func testKeys(t *testing.T) (tag, prov []byte) {
	t.Helper()
	tag, prov = make([]byte, 32), make([]byte, 32)
	rand.Read(tag)
	rand.Read(prov)
	return tag, prov
}

// withStubRelay attaches a relay with a stub link to s.
func withStubRelay(t *testing.T, s *Server) (*stubLink, Identity) {
	t.Helper()
	tag, _ := testKeys(t)
	id, err := NewIdentity(tag)
	if err != nil {
		t.Fatal(err)
	}
	l := newStubLink()
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	s.AttachRelay(ctx, Relay{URL: "https://relay.test", RoomTagKey: tag, Identity: id, Link: l})
	return l, id
}

func pubKey(t *testing.T) (ed25519.PublicKey, string) {
	t.Helper()
	pk, _, _ := ed25519.GenerateKey(rand.Reader)
	return pk, wire.B64(pk)
}

func relayKeyMsg(role, key string) map[string]any {
	return map[string]any{"type": "relay_key", "role": role, "sig": key}
}

// refusedKey reads the answer to a relay_key that must be refused: a reply
// with bad_request, never a refused message (a phone unpairs on that).
func refusedKey(t *testing.T, c *websocket.Conn, what string) {
	t.Helper()
	m := recvAny(t, c, "reply", "relay_ok", "refused")
	if m["type"] != "reply" || m["ok"] != false || m["code"] != "bad_request" {
		t.Fatalf("%s: got %v, want a bad_request reply", what, m)
	}
}

// recvAny reads the next message whose type is one of types.
func recvAny(t *testing.T, c *websocket.Conn, types ...string) map[string]any {
	t.Helper()
	for {
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		_, b, err := c.Read(ctx)
		cancel()
		if err != nil {
			t.Fatalf("waiting for %v: %v", types, err)
		}
		var m map[string]any
		if err := json.Unmarshal(b, &m); err != nil {
			t.Fatal(err)
		}
		for _, ty := range types {
			if m["type"] == ty {
				return m
			}
		}
	}
}

// Pairing registers new relay keys. An authenticated direct hello can confirm
// matching keys or fill empty slots; a different registered key needs pairing.
// A key another device holds is refused; room devices follow phones.toml.
func TestRelayKeyRules(t *testing.T) {
	s, _ := up(t)
	l, id := withStubRelay(t, s)
	p, err := s.NewPairing()
	if err != nil {
		t.Fatal(err)
	}
	q := secretOf(t, p)
	c, err := dial(t, s, q.Get("fp"))
	if err != nil {
		t.Fatal(err)
	}
	send(t, c, map[string]any{"type": "pair", "secret": q.Get("s"), "device": "iPhone A"})
	token := recv(t, c, "paired")["token"].(string)
	rm := recv(t, c, "relay")
	if rm["url"] != "https://relay.test" || rm["room"] != id.Room.String() || rm["mac_kem"] != wire.B64(id.Kem.PublicKey().Bytes()) {
		t.Fatalf("relay offer %v", rm)
	}
	phonePK, phoneKey := pubKey(t)
	watchPK, watchKey := pubKey(t)
	_, other := pubKey(t)
	t.Run("PhoneKeyOnThePairingConnection", func(t *testing.T) {
		send(t, c, relayKeyMsg("phone", phoneKey))
		if m := recvAny(t, c, "relay_ok", "reply"); m["type"] != "relay_ok" || m["role"] != "phone" {
			t.Fatalf("phone key on the pairing connection: %v", m)
		}
	})
	t.Run("PhoneKeyOnlyOnce", func(t *testing.T) {
		send(t, c, relayKeyMsg("phone", other))
		refusedKey(t, c, "a second phone key on the pairing connection")
	})
	t.Run("WatchKeyOnThePairingConnection", func(t *testing.T) {
		send(t, c, relayKeyMsg("watch", watchKey))
		if m := recvAny(t, c, "relay_ok", "reply"); m["type"] != "relay_ok" || m["role"] != "watch" {
			t.Fatalf("watch key on the pairing connection: %v", m)
		}
	})
	t.Run("MalformedRefused", func(t *testing.T) {
		send(t, c, relayKeyMsg("tablet", other))
		refusedKey(t, c, "a role that is neither")
		send(t, c, relayKeyMsg("watch", "not-a-key"))
		refusedKey(t, c, "a malformed key")
	})
	t.Run("MacKeyRefused", func(t *testing.T) {
		send(t, c, relayKeyMsg("watch", wire.B64(id.Sig.Public().(ed25519.PublicKey))))
		refusedKey(t, c, "the Mac's own key")
	})
	c.Close(websocket.StatusNormalClosure, "")

	want := "phone " + phoneKey + "\nwatch " + watchKey
	waitFor(t, "the room's devices", func() bool { return l.list() == want })

	// A hello cannot replace the paired phone or watch with a different key.
	h, err := dial(t, s, q.Get("fp"))
	if err != nil {
		t.Fatal(err)
	}
	send(t, h, map[string]any{"type": "hello", "token": token, "device": "iPhone A"})
	recv(t, h, "hello")
	_, k1 := pubKey(t)
	t.Run("NoDifferentPhoneKeyOnAHelloConnection", func(t *testing.T) {
		send(t, h, relayKeyMsg("phone", k1))
		refusedKey(t, h, "a phone key on a hello connection")
	})
	t.Run("NoDifferentWatchKeyOnAHelloConnection", func(t *testing.T) {
		send(t, h, relayKeyMsg("watch", k1))
		refusedKey(t, h, "a watch key on a hello connection")
	})

	// another phone cannot claim A's keys
	p2, _ := s.NewPairing()
	q2 := secretOf(t, p2)
	c2, err := dial(t, s, q2.Get("fp"))
	if err != nil {
		t.Fatal(err)
	}
	send(t, c2, map[string]any{"type": "pair", "secret": q2.Get("s"), "device": "iPhone B"})
	recv(t, c2, "paired")
	t.Run("AnotherDevicesKeyRefused", func(t *testing.T) {
		send(t, c2, relayKeyMsg("phone", watchKey))
		refusedKey(t, c2, "B claiming A's watch key as its own")
		send(t, c2, relayKeyMsg("watch", phoneKey))
		refusedKey(t, c2, "B claiming A's phone key as its watch's")
	})

	s.mu.Lock()
	a := s.phone("iPhone A")
	gotP, gotW := a.SigPK, a.WatchSigPK
	byP, byW := s.byKey(phonePK, false), s.byKey(watchPK, true)
	s.mu.Unlock()
	if gotP != phoneKey || gotW != watchKey || byP != a || byW != a {
		t.Fatalf("phones.toml keys %q %q", gotP, gotW)
	}
	b, _ := os.ReadFile(filepath.Join(s.cfg.State, "phones.toml"))
	if !strings.Contains(string(b), "sig_pk = \""+phoneKey) || !strings.Contains(string(b), "watch_sig_pk = \""+watchKey) {
		t.Fatalf("phones.toml:\n%s", b)
	}

	// revoke: the room drops both keys
	if err := s.Revoke("iPhone A"); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "the revoked keys gone from the room", func() bool { return l.list() == "" })
}

// Without the relay a pairing is offered nothing and a relay key is refused.
func TestRelayOff(t *testing.T) {
	s, _ := up(t)
	c := paired(t, s)
	recv(t, c, "state")
	_, k := pubKey(t)
	send(t, c, relayKeyMsg("phone", k))
	m := recvAny(t, c, "relay", "reply", "relay_ok")
	if m["type"] != "reply" || m["code"] != "bad_request" {
		t.Fatalf("relay key with the relay off: %v", m)
	}
}

// Version 2 fields (PROTOCOL.md § 8): every message carries now_ms; hello
// and replies carry recent; the state carries this process's epoch and a seq
// that changes with (phase, take) only; a restart is a new epoch.
func TestStateVersion(t *testing.T) {
	recent := []Recent{{Take: "t2", Final: "", AtMS: 2000}, {Take: "t1", Final: "sent", AtMS: 1000}}
	f := &fakeTakes{}
	s, err := New(Config{Listen: "127.0.0.1:0", State: t.TempDir(), Name: "m", Takes: f, Logf: t.Logf, Recent: func(n int) []Recent { return recent[:min(n, len(recent))] }})
	if err != nil {
		t.Fatal(err)
	}
	st1 := s.state()
	st2 := s.state()
	if len(st1.Epoch) != 11 || st1.Seq == 0 || st2.Seq != st1.Seq {
		t.Fatalf("idle twice: %+v %+v", st1, st2)
	}
	f.Start("iphone")
	st3 := s.state()
	if st3.Seq != st1.Seq+1 || st3.Take != "take1" {
		t.Fatalf("after a start: %+v", st3)
	}
	if b := encode(stateMsg{Type: "state", State: st1}); bytes.Contains(b, []byte(`"owner"`)) {
		t.Fatalf("idle state names an owner: %s", b)
	}
	f.mu.Lock()
	f.owner = "stream" // same phase and take, another owner
	f.mu.Unlock()
	st5 := s.state()
	if st5.Seq != st3.Seq+1 || st5.Owner != "stream" {
		t.Fatalf("after an owner change: %+v", st5)
	}
	if b := encode(stateMsg{Type: "state", State: st5}); !bytes.Contains(b, []byte(`"owner":"stream"`)) {
		t.Fatalf("state push %s", b)
	}
	f.Stop("iphone", "take1", false)
	if st4 := s.state(); st4.Seq != st5.Seq+1 || st4.Owner != "" {
		t.Fatalf("after a stop: %+v", st4)
	}
	s2, _ := New(Config{State: t.TempDir(), Name: "m", Takes: f})
	if s2.state().Epoch == st1.Epoch {
		t.Fatal("a new process kept the epoch")
	}

	b := encode(helloMsg{Type: "hello", Mac: "m", State: st1, Recent: s.recent()})
	for _, want := range []string{`"recent":[{"take":"t2","final":"","at_ms":2000},{"take":"t1","final":"sent","at_ms":1000}]`, `"epoch":"` + st1.Epoch + `"`, `"now_ms":`} {
		if !bytes.Contains(b, []byte(want)) {
			t.Fatalf("hello %s lacks %s", b, want)
		}
	}
	if m := encode(stateMsg{Type: "state", State: st1}); !bytes.Contains(m, []byte(`"now_ms":`)) {
		t.Fatalf("state push %s", m)
	}
}

// A state command is answered and not remembered, so polls never push a
// real command out of the memory of ids; a start older than Fresh (6 s) is
// stale.
func TestStatePollsAndFresh(t *testing.T) {
	s, f := up(t)
	c := paired(t, s)
	now := time.Now()
	send(t, c, cmd("a1", "start", "", now))
	if r := recv(t, c, "reply"); r["ok"] != true {
		t.Fatalf("start %v", r)
	}
	for i := range remembered + 5 {
		send(t, c, cmd("poll"+string(rune('a'+i%26))+string(rune('a'+i/26)), "state", "", now))
		recv(t, c, "reply")
	}
	send(t, c, cmd("a1", "start", "", now))
	if r := recv(t, c, "reply"); r["ok"] != true {
		t.Fatalf("the start's id was forgotten behind polls: %v", r)
	}
	if got := f.log(); got != "start iphone" {
		t.Fatalf("calls %q", got)
	}
	send(t, c, end("s1", "stop", "", "take1"))
	recv(t, c, "reply")
	send(t, c, cmd("a2", "start", "", time.Now().Add(-7*time.Second)))
	if r := recv(t, c, "reply"); r["ok"] != false || r["code"] != "stale" {
		t.Fatalf("a 7 s old start: %v", r)
	}
}

// The identity is minted once into three 0600 files and read back the same;
// a room whose tag the room-tag key does not verify is minted anew under the
// same keys.
func TestIdentityFiles(t *testing.T) {
	dir := t.TempDir()
	tag, _ := testKeys(t)
	a, err := LoadIdentity(dir, tag)
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range []string{roomFile, kemFile, sigFile} {
		st, err := os.Stat(filepath.Join(dir, f))
		if err != nil || st.Mode().Perm() != 0o600 {
			t.Fatalf("%s: %v %v", f, st, err)
		}
	}
	b, err := LoadIdentity(dir, tag)
	if err != nil || b.Room != a.Room || !bytes.Equal(b.Kem.Bytes(), a.Kem.Bytes()) || !bytes.Equal(b.Sig, a.Sig) || !auth.CheckRoom(tag, b.Room) {
		t.Fatalf("read back %v", err)
	}
	tag2, _ := testKeys(t)
	c, err := LoadIdentity(dir, tag2)
	if err != nil || c.Room == a.Room || !auth.CheckRoom(tag2, c.Room) || !bytes.Equal(c.Sig, a.Sig) {
		t.Fatalf("a new room-tag key: %v", err)
	}
	if _, err := LoadIdentity(dir, nil); err == nil {
		t.Fatal("a room-tag key that is not 32 bytes minted a room")
	}
}

func TestRelayHelloRenewsOnlyEmptyOrMatchingKeys(t *testing.T) {
	s, _ := up(t)
	_, id := withStubRelay(t, s)
	p, _ := s.NewPairing()
	q := secretOf(t, p)
	c, err := dial(t, s, q.Get("fp"))
	if err != nil {
		t.Fatal(err)
	}
	send(t, c, map[string]any{"type": "pair", "secret": q.Get("s"), "device": "renewal phone"})
	token := recv(t, c, "paired")["token"].(string)
	recv(t, c, "relay")
	c.Close(websocket.StatusNormalClosure, "")
	hello := func() *websocket.Conn {
		h, err := dial(t, s, q.Get("fp"))
		if err != nil {
			t.Fatal(err)
		}
		send(t, h, map[string]any{"type": "hello", "token": token, "device": "renewal phone"})
		recv(t, h, "hello")
		offer := recv(t, h, "relay")
		if offer["room"] != id.Room.String() {
			t.Fatalf("offer %+v", offer)
		}
		return h
	}
	_, key := pubKey(t)
	h := hello()
	send(t, h, relayKeyMsg("phone", key))
	if m := recvAny(t, h, "relay_ok", "reply"); m["type"] != "relay_ok" {
		t.Fatalf("empty slot: %+v", m)
	}
	h.Close(websocket.StatusNormalClosure, "")
	h = hello()
	defer h.Close(websocket.StatusNormalClosure, "")
	_, other := pubKey(t)
	send(t, h, relayKeyMsg("phone", other))
	refusedKey(t, h, "hello replacing registered key")
	send(t, h, relayKeyMsg("phone", key))
	if m := recvAny(t, h, "relay_ok", "reply"); m["type"] != "relay_ok" {
		t.Fatalf("matching key: %+v", m)
	}
}

func TestRelayReconcileBoundsLegacyKeys(t *testing.T) {
	s, _ := up(t)
	l, _ := withStubRelay(t, s)
	s.mu.Lock()
	for i := 0; i < wire.MaxDevices+1; i++ {
		_, k := pubKey(t)
		s.phones = append(s.phones, &phone{Name: k, SigPK: k, LastSeen: time.Unix(int64(i), 0)})
	}
	newest := s.phones[len(s.phones)-1].SigPK
	s.mu.Unlock()
	if err := s.reconcile(context.Background(), s.relaySide()); err != nil {
		t.Fatal(err)
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if len(l.devices) != wire.MaxDevices || l.devices[newest] != wire.RolePhone {
		t.Fatalf("devices %d, newest role %q", len(l.devices), l.devices[newest])
	}
}

func TestRotatedRoomOfferedToReturningDirectPhone(t *testing.T) {
	s, _ := up(t)
	_, old := withStubRelay(t, s)
	p, _ := s.NewPairing()
	q := secretOf(t, p)
	c, err := dial(t, s, q.Get("fp"))
	if err != nil {
		t.Fatal(err)
	}
	send(t, c, map[string]any{"type": "pair", "secret": q.Get("s"), "device": "offline phone"})
	token := recv(t, c, "paired")["token"].(string)
	recv(t, c, "relay")
	_, key := pubKey(t)
	send(t, c, relayKeyMsg("phone", key))
	recv(t, c, "relay_ok")
	c.Close(websocket.StatusNormalClosure, "")
	if _, err := s.RotateRelay(); err != nil {
		t.Fatal(err)
	}
	h, err := dial(t, s, q.Get("fp"))
	if err != nil {
		t.Fatal(err)
	}
	defer h.Close(websocket.StatusNormalClosure, "")
	send(t, h, map[string]any{"type": "hello", "token": token, "device": "offline phone"})
	recv(t, h, "hello")
	offer := recv(t, h, "relay")
	if offer["room"] == old.Room.String() || offer["room"] != s.relaySide().identity().Room.String() {
		t.Fatalf("stale offer %+v", offer)
	}
	send(t, h, relayKeyMsg("phone", key))
	if got := recvAny(t, h, "relay_ok", "reply"); got["type"] != "relay_ok" {
		t.Fatalf("returning phone key %+v", got)
	}
}
