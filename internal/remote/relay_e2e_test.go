package remote

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"

	"github.com/caoer/mega-asr/internal/relay/auth"
	"github.com/caoer/mega-asr/internal/relay/client"
	"github.com/caoer/mega-asr/internal/relay/e2e"
	"github.com/caoer/mega-asr/internal/relay/fakeroom"
	"github.com/caoer/mega-asr/internal/relay/wire"
)

// e2eRig is a Mac served through the in-process fake room by the real link.
type e2eRig struct {
	t    *testing.T
	s    *Server
	f    *fakeTakes
	fake *fakeroom.Relay
	ts   *httptest.Server
	id   Identity
}

func newE2E(t *testing.T) *e2eRig {
	t.Helper()
	s, f := up(t)
	tag, prov := testKeys(t)
	fake := fakeroom.New(fakeroom.Config{RoomTagKey: tag, ProvisionKey: prov})
	ts := httptest.NewTLSServer(fake.Handler())
	t.Cleanup(ts.Close)
	id, err := LoadIdentity(s.cfg.State, tag)
	if err != nil {
		t.Fatal(err)
	}
	cl, err := client.New(client.Config{URL: ts.URL, Room: id.Room, Key: id.Sig, ProvisionKey: prov, Handler: s, HTTPClient: ts.Client(), Logf: t.Logf})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	t.Cleanup(func() { cancel(); <-done })
	s.AttachRelay(ctx, Relay{URL: ts.URL, RoomTagKey: tag, Identity: id, Link: cl})
	go func() { cl.Run(ctx); close(done) }()
	waitFor(t, "the Mac in its room", func() bool { return fake.MacConnected(id.Room) })
	return &e2eRig{t: t, s: s, f: f, fake: fake, ts: ts, id: id}
}

func (g *e2eRig) roomKeys() string {
	var out []string
	for _, d := range g.fake.Devices(g.s.relaySide().identity().Room) {
		out = append(out, string(d.Role)+" "+wire.B64(d.Key))
	}
	sort.Strings(out)
	return strings.Join(out, "\n")
}

// pair pairs a phone on the LAN and registers its and its watch's relay keys
// on the pairing connection.
func (g *e2eRig) pair(name string) (phone, watch ed25519.PrivateKey) {
	t := g.t
	t.Helper()
	p, err := g.s.NewPairing()
	if err != nil {
		t.Fatal(err)
	}
	q := secretOf(t, p)
	c, err := dial(t, g.s, q.Get("fp"))
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close(websocket.StatusNormalClosure, "")
	send(t, c, map[string]any{"type": "pair", "secret": q.Get("s"), "device": name})
	recv(t, c, "paired")
	if rm := recv(t, c, "relay"); rm["url"] != g.ts.URL || rm["room"] != g.id.Room.String() {
		t.Fatalf("relay offer %v", rm)
	}
	_, phone, _ = ed25519.GenerateKey(rand.Reader)
	_, watch, _ = ed25519.GenerateKey(rand.Reader)
	send(t, c, relayKeyMsg("phone", wire.B64(phone.Public().(ed25519.PublicKey))))
	recv(t, c, "relay_ok")
	send(t, c, relayKeyMsg("watch", wire.B64(watch.Public().(ed25519.PublicKey))))
	recv(t, c, "relay_ok")
	want := []string{"phone " + wire.B64(phone.Public().(ed25519.PublicKey)), "watch " + wire.B64(watch.Public().(ed25519.PublicKey))}
	waitFor(t, "the keys in the room", func() bool {
		got := g.roomKeys()
		return strings.Contains(got, want[0]) && strings.Contains(got, want[1])
	})
	return phone, watch
}

// relayPhone is a phone's session through the room, as the iPhone runs it.
type relayPhone struct {
	t    *testing.T
	ws   *websocket.Conn
	sess *e2e.Session
}

func (g *e2eRig) connect(sk ed25519.PrivateKey) (*relayPhone, map[string]any) {
	t := g.t
	t.Helper()
	room := g.id.Room
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	ws, _, err := websocket.Dial(ctx, "wss"+strings.TrimPrefix(g.ts.URL, "https")+wire.PathWS, &websocket.DialOptions{HTTPClient: g.ts.Client(), HTTPHeader: http.Header{wire.Header: {room.String()}}})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ws.CloseNow() })
	_, b, err := ws.Read(ctx)
	if err != nil {
		t.Fatal(err)
	}
	f, _ := wire.DecodeFromRoom(b)
	ch := f.(wire.Challenge)
	ws.Write(ctx, websocket.MessageText, auth.AuthFrame(sk, room, wire.RolePhone, ch.Nonce[:], nil))
	_, b, err = ws.Read(ctx)
	if err != nil {
		t.Fatal(err)
	}
	okf, _ := wire.DecodeFromRoom(b)
	ok := okf.(wire.OKPhone)
	if !ok.Peer {
		t.Fatal("peer false with the Mac in the room")
	}
	msg, pd, err := e2e.Begin(g.id.Kem.PublicKey(), e2e.Phone(room, ok.SID), sk, []byte(`{"type":"hello","device":"x"}`))
	if err != nil {
		t.Fatal(err)
	}
	ws.Write(ctx, websocket.MessageBinary, msg)
	_, b, err = ws.Read(ctx)
	if err != nil {
		t.Fatal(err)
	}
	pt, sess, err := pd.Finish(b)
	if err != nil {
		t.Fatal(err)
	}
	var hello map[string]any
	json.Unmarshal(pt, &hello)
	return &relayPhone{t: t, ws: ws, sess: sess}, hello
}

func (p *relayPhone) send(v any) {
	b, _ := json.Marshal(v)
	rec, err := p.sess.Seal(e2e.KindJSON, b)
	if err != nil {
		p.t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if err := p.ws.Write(ctx, websocket.MessageBinary, rec); err != nil {
		p.t.Fatal(err)
	}
}

// recv opens records until one of type typ.
func (p *relayPhone) recv(typ string) map[string]any {
	p.t.Helper()
	for {
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		_, b, err := p.ws.Read(ctx)
		cancel()
		if err != nil {
			p.t.Fatalf("waiting for %s: %v", typ, err)
		}
		kind, pt, err := p.sess.Open(b)
		if err != nil || kind != e2e.KindJSON {
			p.t.Fatalf("a record does not open: %v", err)
		}
		var m map[string]any
		json.Unmarshal(pt, &m)
		if m["type"] == typ {
			return m
		}
	}
}

func (g *e2eRig) post(watch ed25519.PrivateKey, nonce byte, c map[string]any) (int, map[string]any) {
	t := g.t
	t.Helper()
	b, _ := json.Marshal(c)
	body, pd, err := e2e.SealRequest(g.id.Kem.PublicKey(), g.id.Room, watch, uint64(time.Now().UnixMilli()), [wire.NonceLen]byte{nonce, 1}, b)
	if err != nil {
		t.Fatal(err)
	}
	req, _ := http.NewRequest("POST", g.ts.URL+wire.PathCmd, bytes.NewReader(body))
	req.Header.Set(wire.Header, g.id.Room.String())
	resp, err := g.ts.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	out, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != 200 {
		return resp.StatusCode, nil
	}
	pt, _, err := pd.Finish(out)
	if err != nil {
		t.Fatalf("the watch's reply does not open: %v", err)
	}
	var m map[string]any
	json.Unmarshal(pt, &m)
	return 200, m
}

// The whole relay path on the Mac: a phone pairs on the LAN and its keys
// reach the room; through the room it says hello without a token, starts a
// take, hears the push, and registers a new watch key on the relay session;
// the watch posts a stop over HTTPS and gets the reply with the state; a
// revoke ends the session with a sealed refusal and bye 1008 and empties the
// room.
func TestRelayEndToEnd(t *testing.T) {
	g := newE2E(t)
	phone, watch := g.pair("iPhone A")
	p, hello := g.connect(phone)
	if hello["type"] != "hello" || hello["now_ms"] == nil {
		t.Fatalf("hello %v", hello)
	}
	p.send(cmd("c1", "start", "", time.Now()))
	if r := p.recv("reply"); r["ok"] != true || r["state"].(map[string]any)["take"] != "take1" {
		t.Fatalf("start through the relay: %v", r)
	}
	g.s.Changed()
	if st := p.recv("state"); st["state"].(map[string]any)["phase"] != "recording" {
		t.Fatalf("push %v", st)
	}
	if ph := g.s.Phones(); ph[0].Relay != 1 || !ph[0].Watch {
		t.Fatalf("phones %+v", ph)
	}

	// a new watch key, registered on the relay session; the old one leaves
	// the room
	_, watch2, _ := ed25519.GenerateKey(rand.Reader)
	t.Run("WatchKeyOnARelaySession", func(t *testing.T) {
		p.send(relayKeyMsg("watch", wire.B64(watch2.Public().(ed25519.PublicKey))))
		if m := p.recv("relay_ok"); m["role"] != "watch" {
			t.Fatalf("watch key on the relay: %v", m)
		}
	})
	t.Run("NoPhoneKeyOnARelaySession", func(t *testing.T) {
		_, k := pubKey(t)
		p.send(relayKeyMsg("phone", k))
		if m := p.recv("reply"); m["code"] != "bad_request" {
			t.Fatalf("a phone key on the relay: %v", m)
		}
	})
	waitFor(t, "the old watch key gone", func() bool {
		return !strings.Contains(g.roomKeys(), wire.B64(watch.Public().(ed25519.PublicKey))) && strings.Contains(g.roomKeys(), wire.B64(watch2.Public().(ed25519.PublicKey)))
	})

	stop := end("w1", "stop", "send", "take1")
	code, rep := g.post(watch2, 1, stop)
	if code != 200 || rep["ok"] != true || rep["id"] != "w1" || rep["recent"] == nil {
		t.Fatalf("watch stop: %d %v", code, rep)
	}
	if got := g.f.log(); got != "start iphone; stop watch take1 send=true" {
		t.Fatalf("calls %q", got)
	}
	if code, _ := g.post(watch, 2, cmd("w2", "state", "", time.Now())); code != 404 {
		t.Fatalf("the replaced watch key got %d", code)
	}

	if err := g.s.Revoke("iPhone A"); err != nil {
		t.Fatal(err)
	}
	if m := p.recv("refused"); m["code"] != "token" {
		t.Fatalf("revoke: %v", m)
	}
	if m := p.recv("bye"); m["code"] != float64(1008) {
		t.Fatalf("bye %v", m)
	}
	waitFor(t, "an empty room", func() bool { return g.roomKeys() == "" })
}

// Rotation: a new room and keys, provisioned; every relay key forgotten, so
// the old keys reach nothing; the direct pairing stays.
func TestRelayRotate(t *testing.T) {
	g := newE2E(t)
	phone, _ := g.pair("iPhone A")
	g.connect(phone)
	prefix, err := g.s.RotateRelay()
	if err != nil {
		t.Fatal(err)
	}
	nid := g.s.relaySide().identity()
	if nid.Room == g.id.Room || !strings.HasPrefix(nid.Room.String(), prefix) {
		t.Fatalf("rotated to %s (%s)", nid.Room, prefix)
	}
	waitFor(t, "the new room", func() bool {
		return g.fake.MacConnected(nid.Room) && bytes.Equal(g.fake.MacKey(nid.Room), nid.Sig.Public().(ed25519.PublicKey))
	})
	if b, err := os.ReadFile(filepath.Join(g.s.cfg.State, roomFile)); err != nil || strings.TrimSpace(string(b)) != nid.Room.String() {
		t.Fatalf("relay-room on disk %q %v", b, err)
	}
	ph := g.s.Phones()
	if len(ph) != 1 || ph[0].RelayKey || ph[0].Watch {
		t.Fatalf("phones after rotation %+v", ph)
	}
	if g.roomKeys() != "" {
		t.Fatalf("the new room has devices: %s", g.roomKeys())
	}
}
