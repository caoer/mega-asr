package client

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/coder/websocket"

	"github.com/caoer/mega-asr/internal/relay/auth"
	"github.com/caoer/mega-asr/internal/relay/fakeroom"
	"github.com/caoer/mega-asr/internal/relay/wire"
)

// recorder is a Handler that keeps what it was told.
type recorder struct {
	mu     sync.Mutex
	ups    int
	downs  int
	peers  []wire.Peer
	frames [][]byte
}

func (r *recorder) LinkUp([]wire.PeerInfo) { r.mu.Lock(); r.ups++; r.mu.Unlock() }
func (r *recorder) LinkDown()              { r.mu.Lock(); r.downs++; r.mu.Unlock() }
func (r *recorder) Peer(p wire.Peer)       { r.mu.Lock(); r.peers = append(r.peers, p); r.mu.Unlock() }
func (r *recorder) Frame(sid wire.SID, data []byte) {
	r.mu.Lock()
	r.frames = append(r.frames, wire.JoinSID(sid, data))
	r.mu.Unlock()
}

func (r *recorder) counts() (ups, downs int) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.ups, r.downs
}

func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	for range 500 {
		if cond() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

type rig struct {
	t         *testing.T
	fake      *fakeroom.Relay
	ts        *httptest.Server
	tag, prov []byte
}

func newRig(t *testing.T) *rig {
	t.Helper()
	tag, prov := make([]byte, 32), make([]byte, 32)
	rand.Read(tag)
	rand.Read(prov)
	fake := fakeroom.New(fakeroom.Config{RoomTagKey: tag, ProvisionKey: prov})
	ts := httptest.NewTLSServer(fake.Handler())
	t.Cleanup(ts.Close)
	return &rig{t: t, fake: fake, ts: ts, tag: tag, prov: prov}
}

func (g *rig) room() wire.RoomID {
	id, err := auth.MintRoom(g.tag, rand.Reader)
	if err != nil {
		g.t.Fatal(err)
	}
	return id
}

// start runs a client for room under key until the test ends.
func (g *rig) start(room wire.RoomID, key ed25519.PrivateKey, prov []byte) (*Client, *recorder) {
	g.t.Helper()
	h := &recorder{}
	c, err := New(Config{URL: g.ts.URL, Room: room, Key: key, ProvisionKey: prov, Handler: h, HTTPClient: g.ts.Client(), Logf: g.t.Logf})
	if err != nil {
		g.t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { c.Run(ctx); close(done) }()
	g.t.Cleanup(func() { cancel(); <-done })
	return c, h
}

// phone connects to room as a registered phone and returns its socket once
// it got ok.
func (g *rig) phone(room wire.RoomID, key ed25519.PrivateKey) (*websocket.Conn, wire.OKPhone) {
	g.t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	ws, _, err := websocket.Dial(ctx, "wss"+strings.TrimPrefix(g.ts.URL, "https")+wire.PathWS, &websocket.DialOptions{
		HTTPClient: g.ts.Client(),
		HTTPHeader: http.Header{wire.Header: {room.String()}},
	})
	if err != nil {
		g.t.Fatal(err)
	}
	_, b, err := ws.Read(ctx)
	if err != nil {
		g.t.Fatal(err)
	}
	f, _ := wire.DecodeFromRoom(b)
	ch := f.(wire.Challenge)
	if err := ws.Write(ctx, websocket.MessageText, auth.AuthFrame(key, room, wire.RolePhone, ch.Nonce[:], nil)); err != nil {
		g.t.Fatal(err)
	}
	_, b, err = ws.Read(ctx)
	if err != nil {
		g.t.Fatal(err)
	}
	f, _ = wire.DecodeFromRoom(b)
	return ws, f.(wire.OKPhone)
}

func newKey() ed25519.PrivateKey {
	_, k, _ := ed25519.GenerateKey(rand.Reader)
	return k
}

func pub(k ed25519.PrivateKey) ed25519.PublicKey { return k.Public().(ed25519.PublicKey) }

// The link provisions an empty room with the proof, registers devices,
// acknowledges a phone's session (so the phone hears peer true) and carries
// frames both ways under the session's sid.
func TestLinkProvisionsAndCarries(t *testing.T) {
	g := newRig(t)
	room, mac := g.room(), newKey()
	c, h := g.start(room, mac, g.prov)
	waitFor(t, "online", func() bool { on, _ := c.Status(); return on })
	if !bytes.Equal(g.fake.MacKey(room), pub(mac)) {
		t.Fatal("the room was not provisioned with the Mac's key")
	}
	phone := newKey()
	list, err := c.Devices(context.Background(), wire.DevicesChange{Add: []wire.Device{{Role: wire.RolePhone, Key: pub(phone)}}, Remove: []ed25519.PublicKey{}})
	if err != nil || len(list) != 1 || !bytes.Equal(list[0].Key, pub(phone)) {
		t.Fatalf("devices %v %v", list, err)
	}
	if list, err = c.Devices(context.Background(), wire.DevicesChange{Add: []wire.Device{}, Remove: []ed25519.PublicKey{}}); err != nil || len(list) != 1 {
		t.Fatalf("an empty change lists %v %v", list, err)
	}

	ws, ok := g.phone(room, phone)
	defer ws.CloseNow()
	if !ok.Peer {
		t.Fatal("the phone heard peer false: the link did not acknowledge")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	ws.Write(ctx, websocket.MessageBinary, []byte{1, 2, 3})
	waitFor(t, "the phone's frame", func() bool {
		h.mu.Lock()
		defer h.mu.Unlock()
		return len(h.frames) == 1 && bytes.Equal(h.frames[0], wire.JoinSID(ok.SID, []byte{1, 2, 3}))
	})
	if err := c.Send(ok.SID, []byte{9, 8}); err != nil {
		t.Fatal(err)
	}
	if _, b, err := ws.Read(ctx); err != nil || !bytes.Equal(b, []byte{9, 8}) {
		t.Fatalf("the Mac's frame: %x %v", b, err)
	}
	c.Drop(ok.SID)
	if _, _, err := ws.Read(ctx); websocket.CloseStatus(err) != websocket.StatusNormalClosure {
		t.Fatalf("drop closed the phone with %v", err)
	}
	h.mu.Lock()
	ps := append([]wire.Peer(nil), h.peers...)
	h.mu.Unlock()
	if len(ps) == 0 || !ps[0].Up || ps[0].SID != ok.SID {
		t.Fatalf("peers %+v", ps)
	}
}

// A Mac socket the room closes as half-open (1012) is redialled at once.
func TestLinkRedialsAfterLoss(t *testing.T) {
	g := newRig(t)
	room := g.room()
	c, h := g.start(room, newKey(), g.prov)
	waitFor(t, "online", func() bool { on, _ := c.Status(); return on })
	g.fake.CloseMac(room, wire.CloseMacGone)
	waitFor(t, "down and up again", func() bool { ups, downs := h.counts(); return downs >= 1 && ups >= 2 })
	if !g.fake.MacConnected(room) {
		t.Fatal("not back in the room")
	}
}

// Switch moves the link to a new room under a new key and provisions it.
func TestLinkSwitch(t *testing.T) {
	g := newRig(t)
	room := g.room()
	c, _ := g.start(room, newKey(), g.prov)
	waitFor(t, "online", func() bool { on, _ := c.Status(); return on })
	room2, key2 := g.room(), newKey()
	c.Switch(room2, key2)
	waitFor(t, "the new room", func() bool { return g.fake.MacConnected(room2) && bytes.Equal(g.fake.MacKey(room2), pub(key2)) })
	if g.fake.MacConnected(room) {
		t.Fatal("still in the old room")
	}
}

// A Switch while the link is still dialing the old room: that socket is not
// used, and the link ends in the new room.
func TestLinkSwitchWhileDialing(t *testing.T) {
	g := newRig(t)
	inner := g.fake.Handler()
	gate, reached := make(chan struct{}), make(chan struct{})
	var first sync.Once
	g.ts = httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		first.Do(func() { close(reached); <-gate }) // the first dial waits here
		inner.ServeHTTP(w, r)
	}))
	t.Cleanup(g.ts.Close)
	var release sync.Once
	open := func() { release.Do(func() { close(gate) }) }
	t.Cleanup(open)

	room := g.room()
	c, _ := g.start(room, newKey(), g.prov)
	<-reached
	room2, key2 := g.room(), newKey()
	c.Switch(room2, key2)
	open()
	waitFor(t, "the new room only", func() bool { return g.fake.MacConnected(room2) && !g.fake.MacConnected(room) })
}

// A second socket of the same Mac supersedes the first (4001), which stays
// closed; a key the room does not hold is refused and the link says so.
func TestLinkSupersededAndRefused(t *testing.T) {
	g := newRig(t)
	room, mac := g.room(), newKey()
	a, _ := g.start(room, mac, g.prov)
	waitFor(t, "a online", func() bool { on, _ := a.Status(); return on })
	b, _ := g.start(room, mac, g.prov)
	waitFor(t, "b online", func() bool { on, _ := b.Status(); return on })
	waitFor(t, "a superseded", func() bool { on, why := a.Status(); return !on && strings.Contains(why, "顶替") })
	time.Sleep(1500 * time.Millisecond)
	if on, _ := a.Status(); on {
		t.Fatal("a superseded link dialled again")
	}
	if on, _ := b.Status(); !on {
		t.Fatal("the newer link lost the room")
	}

	other, _ := g.start(room, newKey(), g.prov)
	waitFor(t, "refused", func() bool { on, why := other.Status(); return !on && strings.Contains(why, "拒绝") })
	if on, _ := b.Status(); !on {
		t.Fatal("a refused key disturbed the room's Mac")
	}

	none, _ := g.start(g.room(), newKey(), nil)
	waitFor(t, "an unprovisioned room refused without the proof", func() bool { on, why := none.Status(); return !on && strings.Contains(why, "拒绝") })
}

func TestNewChecksURL(t *testing.T) {
	for _, u := range []string{"http://voice.example", "wss://voice.example", "https://", "https://voice.example/v1", "https://u@voice.example", "https://voice.example?x=1"} {
		if _, err := New(Config{URL: u, Key: newKey(), Handler: &recorder{}}); err == nil {
			t.Errorf("%s accepted", u)
		}
	}
	if _, err := New(Config{URL: "https://voice.example", Key: newKey(), Handler: &recorder{}}); err != nil {
		t.Fatal(err)
	}
}

func TestBackoff(t *testing.T) {
	for n, want := range map[int]time.Duration{0: backoffMin, 1: 2 * backoffMin, 6: backoffMax, 20: backoffMax} {
		for range 50 {
			if d := backoff(n); d < want/2 || d > want {
				t.Fatalf("backoff(%d) = %v, want %v/2..%v", n, d, want, want)
			}
		}
	}
}
