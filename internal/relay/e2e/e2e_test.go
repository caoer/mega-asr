package e2e

import (
	"bytes"
	"crypto/ecdh"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"errors"
	"testing"
	"time"

	"github.com/caoer/mega-asr/internal/relay/auth"
	"github.com/caoer/mega-asr/internal/relay/wire"
)

type fixture struct {
	room   wire.RoomID
	kem    *ecdh.PrivateKey
	phone  ed25519.PrivateKey
	watch  ed25519.PrivateKey
	other  ed25519.PrivateKey
	sid    wire.SID
	start  time.Time
	nonce  [wire.NonceLen]byte
	helloJ []byte
}

func seed(name string) []byte { s := sha256.Sum256([]byte(name)); return s[:] }

func newFixture(t *testing.T) fixture {
	t.Helper()
	kem, err := ecdh.X25519().NewPrivateKey(seed("mac kem"))
	if err != nil {
		t.Fatal(err)
	}
	f := fixture{
		kem:    kem,
		phone:  ed25519.NewKeyFromSeed(seed("phone")),
		watch:  ed25519.NewKeyFromSeed(seed("watch")),
		other:  ed25519.NewKeyFromSeed(seed("other")),
		start:  time.UnixMilli(1_790_000_000_000),
		helloJ: []byte(`{"type":"hello","device":"iPhone 3fa2"}`),
	}
	copy(f.room[:], seed("room"))
	copy(f.sid[:], seed("sid"))
	copy(f.nonce[:], seed("nonce"))
	return f
}

func pub(k ed25519.PrivateKey) ed25519.PublicKey { return k.Public().(ed25519.PublicKey) }

// phoneSession runs a whole phone handshake and returns both ends.
func phoneSession(t *testing.T, f fixture) (dev, mac *Session) {
	t.Helper()
	msg, pd, err := Begin(f.kem.PublicKey(), Phone(f.room, f.sid), f.phone, f.helloJ)
	if err != nil {
		t.Fatal(err)
	}
	o, err := Open(f.kem, Phone(f.room, f.sid), msg)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(o.DeviceKey, pub(f.phone)) || !bytes.Equal(o.First, f.helloJ) {
		t.Fatalf("opened %x %q", o.DeviceKey, o.First)
	}
	reply, mac, err := o.Reply([]byte(`{"type":"hello","mac":"zz"}`))
	if err != nil {
		t.Fatal(err)
	}
	got, dev, err := pd.Finish(reply)
	if err != nil || string(got) != `{"type":"hello","mac":"zz"}` {
		t.Fatalf("finish %q %v", got, err)
	}
	return dev, mac
}

func TestPhoneSessionRecordsBothWays(t *testing.T) {
	f := newFixture(t)
	dev, mac := phoneSession(t, f)
	for i := 0; i < 3; i++ {
		r, err := dev.Seal(KindJSON, []byte(`{"type":"cmd"}`))
		if err != nil {
			t.Fatal(err)
		}
		k, p, err := mac.Open(r)
		if err != nil || k != KindJSON || string(p) != `{"type":"cmd"}` {
			t.Fatalf("mac open %d: %d %q %v", i, k, p, err)
		}
		r, err = mac.Seal(KindAudio, []byte{1, 2, 3})
		if err != nil {
			t.Fatal(err)
		}
		k, p, err = dev.Open(r)
		if err != nil || k != KindAudio || !bytes.Equal(p, []byte{1, 2, 3}) {
			t.Fatalf("dev open %d: %d %x %v", i, k, p, err)
		}
	}
}

func TestWatchRequestRoundTrip(t *testing.T) {
	f := newFixture(t)
	now := f.start.Add(time.Second)
	body, pd, err := SealRequest(f.kem.PublicKey(), f.room, f.watch, uint64(now.UnixMilli()), f.nonce, []byte(`{"type":"cmd","id":"w1"}`))
	if err != nil {
		t.Fatal(err)
	}
	isWatch := func(k ed25519.PublicKey) bool { return bytes.Equal(k, pub(f.watch)) }
	o, r, err := OpenRequest(f.kem, f.room, body, isWatch, NewReplayGuard(f.start), now)
	if err != nil {
		t.Fatal(err)
	}
	if string(o.First) != `{"type":"cmd","id":"w1"}` || r.Nonce != f.nonce || o.Params.Role != wire.RoleWatch {
		t.Fatalf("opened %q %x %s", o.First, r.Nonce, o.Params.Role)
	}
	reply, s, err := o.Reply([]byte(`{"type":"reply","id":"w1","ok":true}`))
	if err != nil || s != nil {
		t.Fatalf("reply: %v, session %v", err, s)
	}
	if reply[0] != freshLen {
		t.Fatalf("one-shot reply carries a fresh value of %d bytes", reply[0])
	}
	got, s, err := pd.Finish(reply)
	if err != nil || s != nil || string(got) != `{"type":"reply","id":"w1","ok":true}` {
		t.Fatalf("finish %q %v %v", got, s, err)
	}
}

// TestS3RoleInInfo: the Mac takes the role from the session kind, and the
// role is in the HPKE info, so a phone's handshake presented as a watch
// request (or the reverse) does not open, and the Mac can send no sealed
// answer to it.
func TestS3RoleInInfo(t *testing.T) {
	f := newFixture(t)
	binding := Params{f.room, wire.RolePhone, f.nonce[:]}
	msg, _, err := Begin(f.kem.PublicKey(), binding, f.phone, f.helloJ)
	if err != nil {
		t.Fatal(err)
	}
	asWatch := Params{f.room, wire.RoleWatch, f.nonce[:]}
	if _, err := Open(f.kem, asWatch, msg); !errors.Is(err, ErrOpen) {
		t.Fatalf("phone handshake opened as watch: %v", err)
	}
	wmsg, _, err := Begin(f.kem.PublicKey(), asWatch, f.watch, []byte(`{}`))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := Open(f.kem, binding, wmsg); !errors.Is(err, ErrOpen) {
		t.Fatalf("watch handshake opened as phone: %v", err)
	}
	if _, err := Open(f.kem, Params{f.room, wire.RoleMac, f.nonce[:]}, wmsg); !errors.Is(err, ErrRole) {
		t.Fatalf("role mac: %v", err)
	}
}

// TestS3RoleInSignature: the role is in the transcript signature, so a
// signature made for one role is refused under another even when the HPKE
// layer opens.
func TestS3RoleInSignature(t *testing.T) {
	f := newFixture(t)
	phone := Phone(f.room, f.sid)
	watchRole := Params{f.room, wire.RoleWatch, phone.Binding}
	eph, _ := ecdh.X25519().GenerateKey(rand.Reader)
	msg, _, err := begin(f.kem.PublicKey(), phone, watchRole, f.phone, eph, nil, f.helloJ)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := Open(f.kem, phone, msg); !errors.Is(err, ErrSignature) {
		t.Fatalf("phone session signed for role watch: %v", err)
	}
	w := Watch(f.room, f.nonce)
	msg, _, err = begin(f.kem.PublicKey(), w, Params{f.room, wire.RolePhone, w.Binding}, f.watch, nil, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := Open(f.kem, w, msg); !errors.Is(err, ErrSignature) {
		t.Fatalf("watch request signed for role phone: %v", err)
	}
}

func TestHandshakeBoundToRoomAndSession(t *testing.T) {
	f := newFixture(t)
	msg, _, err := Begin(f.kem.PublicKey(), Phone(f.room, f.sid), f.phone, f.helloJ)
	if err != nil {
		t.Fatal(err)
	}
	otherSID := f.sid
	otherSID[0] ^= 1
	otherRoom := f.room
	otherRoom[0] ^= 1
	for name, p := range map[string]Params{"sid": Phone(f.room, otherSID), "room": Phone(otherRoom, f.sid)} {
		if _, err := Open(f.kem, p, msg); !errors.Is(err, ErrOpen) {
			t.Fatalf("other %s: %v", name, err)
		}
	}
	otherKem, _ := ecdh.X25519().GenerateKey(rand.Reader)
	if _, err := Open(otherKem, Phone(f.room, f.sid), msg); !errors.Is(err, ErrOpen) {
		t.Fatalf("other Mac key: %v", err)
	}
	for i := range msg {
		bad := append([]byte(nil), msg...)
		bad[i] ^= 0x80
		if _, err := Open(f.kem, Phone(f.room, f.sid), bad); err == nil {
			t.Fatalf("flipped byte %d opened", i)
		}
	}
}

// TestReplayedHandshakeGetsFreshKeys: a handshake the relay replays makes the
// Mac answer under a new ephemeral key: the two replies share no key, and the
// records of the first session do not open in the second.
func TestReplayedHandshakeGetsFreshKeys(t *testing.T) {
	f := newFixture(t)
	p := Phone(f.room, f.sid)
	msg, pd, err := Begin(f.kem.PublicKey(), p, f.phone, f.helloJ)
	if err != nil {
		t.Fatal(err)
	}
	o1, _ := Open(f.kem, p, msg)
	o2, err := Open(f.kem, p, msg)
	if err != nil {
		t.Fatal(err)
	}
	r1, mac1, err := o1.Reply([]byte(`{}`))
	if err != nil {
		t.Fatal(err)
	}
	r2, mac2, err := o2.Reply([]byte(`{}`))
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Equal(r1[1:33], r2[1:33]) || bytes.Equal(r1[33:], r2[33:]) {
		t.Fatal("two replies to one handshake share an ephemeral or a ciphertext")
	}
	if bytes.Equal(mac1.sendKey, mac2.sendKey) || bytes.Equal(mac1.recvKey, mac2.recvKey) {
		t.Fatal("two replies to one handshake share a record key")
	}
	_, dev, err := pd.Finish(r1)
	if err != nil {
		t.Fatal(err)
	}
	rec, _ := dev.Seal(KindJSON, []byte(`{"type":"cmd"}`))
	if _, _, err := mac2.Open(rec); !errors.Is(err, ErrOpen) {
		t.Fatalf("a record of the first session opened in the replayed one: %v", err)
	}
	if _, _, err := o1.Reply([]byte(`{}`)); err == nil {
		t.Fatal("an Opened answered twice")
	}
}

func TestRecordRefusals(t *testing.T) {
	f := newFixture(t)
	t.Run("repeated counter", func(t *testing.T) {
		dev, mac := phoneSession(t, f)
		r0, _ := dev.Seal(KindJSON, []byte(`{}`))
		if _, _, err := mac.Open(r0); err != nil {
			t.Fatal(err)
		}
		if _, _, err := mac.Open(r0); !errors.Is(err, ErrOpen) {
			t.Fatalf("repeated record: %v", err)
		}
		r1, _ := dev.Seal(KindJSON, []byte(`{}`))
		if _, _, err := mac.Open(r1); !errors.Is(err, ErrClosed) {
			t.Fatalf("after a refusal: %v", err)
		}
	})
	t.Run("reordered", func(t *testing.T) {
		dev, mac := phoneSession(t, f)
		dev.Seal(KindJSON, []byte(`{}`))
		r1, _ := dev.Seal(KindJSON, []byte(`{}`))
		if _, _, err := mac.Open(r1); !errors.Is(err, ErrOpen) {
			t.Fatalf("record 1 opened as record 0: %v", err)
		}
	})
	t.Run("own direction", func(t *testing.T) {
		dev, _ := phoneSession(t, f)
		r0, _ := dev.Seal(KindJSON, []byte(`{}`))
		if _, _, err := dev.Open(r0); !errors.Is(err, ErrOpen) {
			t.Fatalf("a c2m record opened as m2c: %v", err)
		}
	})
	t.Run("handshake key", func(t *testing.T) {
		p := Phone(f.room, f.sid)
		for _, n := range []uint64{0, 1} {
			msg, _, _ := Begin(f.kem.PublicKey(), p, f.phone, f.helloJ)
			o, _ := Open(f.kem, p, msg)
			_, mac, _ := o.Reply([]byte(`{}`))
			for _, hk := range [][]byte{o.ex.hsC2M, o.ex.hsM2C} {
				r := Seal(hk, n, p.RecordAAD(C2M), []byte{KindJSON, '{', '}'})
				if _, _, err := mac.Open(r); !errors.Is(err, ErrOpen) {
					t.Fatalf("a record under a handshake key at %d: %v", n, err)
				}
				_, mac, _ = phoneSessionFrom(t, f, p)
			}
		}
	})
	t.Run("no kind byte", func(t *testing.T) {
		dev, mac := phoneSession(t, f)
		r := Seal(dev.sendKey, 0, dev.sendAAD, nil)
		if _, _, err := mac.Open(r); !errors.Is(err, ErrFormat) {
			t.Fatalf("empty record: %v", err)
		}
	})
	t.Run("too big", func(t *testing.T) {
		dev, _ := phoneSession(t, f)
		if _, err := dev.Seal(KindJSON, make([]byte, wire.MaxRecord-tagLen)); !errors.Is(err, ErrTooBig) {
			t.Fatalf("over the limit: %v", err)
		}
		if r, err := dev.Seal(KindJSON, make([]byte, wire.MaxRecord-tagLen-1)); err != nil || len(r) != wire.MaxRecord {
			t.Fatalf("at the limit: %d %v", len(r), err)
		}
	})
}

func phoneSessionFrom(t *testing.T, f fixture, p Params) (*Opened, *Session, *Pending) {
	t.Helper()
	msg, pd, err := Begin(f.kem.PublicKey(), p, f.phone, f.helloJ)
	if err != nil {
		t.Fatal(err)
	}
	o, err := Open(f.kem, p, msg)
	if err != nil {
		t.Fatal(err)
	}
	_, mac, err := o.Reply([]byte(`{}`))
	if err != nil {
		t.Fatal(err)
	}
	return o, mac, pd
}

// lowOrder is a point of order 8 on Curve25519; X25519 with it is all zeros.
var lowOrder = []byte{
	0xe0, 0xeb, 0x7a, 0x7c, 0x3b, 0x41, 0xb8, 0xae, 0x16, 0x56, 0xe3, 0xfa, 0xf1, 0x9f, 0xc4, 0x6a,
	0xda, 0x09, 0x8d, 0xeb, 0x9c, 0x32, 0xb1, 0xfd, 0x86, 0x62, 0x05, 0x16, 0x5f, 0x49, 0xb8, 0x00,
}

func TestLowOrderEphemeralRefused(t *testing.T) {
	f := newFixture(t)
	p := Phone(f.room, f.sid)
	msg, _, err := begin(f.kem.PublicKey(), p, p, f.phone, nil, lowOrder, f.helloJ)
	if err != nil {
		t.Fatal(err)
	}
	o, err := Open(f.kem, p, msg)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := o.Reply([]byte(`{}`)); !errors.Is(err, ErrLowOrder) {
		t.Fatalf("Mac with a low-order device ephemeral: %v", err)
	}
	_, pd, _ := Begin(f.kem.PublicKey(), p, f.phone, f.helloJ)
	reply := append(wire.AppendLP(nil, lowOrder), make([]byte, 40)...)
	if _, _, err := pd.Finish(reply); !errors.Is(err, ErrLowOrder) {
		t.Fatalf("device with a low-order Mac ephemeral: %v", err)
	}
}

func TestEphemeralLengthByRole(t *testing.T) {
	f := newFixture(t)
	w := Watch(f.room, f.nonce)
	eph, _ := ecdh.X25519().GenerateKey(rand.Reader)
	msg, _, _ := begin(f.kem.PublicKey(), w, w, f.watch, eph, nil, nil)
	if _, err := Open(f.kem, w, msg); !errors.Is(err, ErrFormat) {
		t.Fatalf("watch with an ephemeral: %v", err)
	}
	p := Phone(f.room, f.sid)
	msg, _, _ = begin(f.kem.PublicKey(), p, p, f.phone, nil, nil, f.helloJ)
	if _, err := Open(f.kem, p, msg); !errors.Is(err, ErrFormat) {
		t.Fatalf("phone without an ephemeral: %v", err)
	}
	_, pd, _ := Begin(f.kem.PublicKey(), w, f.watch, nil)
	if _, _, err := pd.Finish(append([]byte{0}, make([]byte, 64)...)); !errors.Is(err, ErrFormat) {
		t.Fatalf("watch reply without the Mac's fresh value: %v", err)
	}
}

func TestReplayGuard(t *testing.T) {
	f := newFixture(t)
	g := NewReplayGuard(f.start)
	now := f.start.Add(2 * time.Minute)
	ms := func(t time.Time) uint64 { return uint64(t.UnixMilli()) }
	k := pub(f.watch)
	n := func(b byte) (x [wire.NonceLen]byte) { x[0] = b; return }
	if err := g.Check(k, ms(now.Add(-61*time.Second)), n(1), now); !errors.Is(err, ErrStale) {
		t.Fatalf("61 s old: %v", err)
	}
	if err := g.Check(k, ms(now.Add(61*time.Second)), n(2), now); !errors.Is(err, ErrStale) {
		t.Fatalf("61 s ahead: %v", err)
	}
	if err := g.Check(k, ms(now.Add(-60*time.Second)), n(3), now); err != nil {
		t.Fatalf("60 s old: %v", err)
	}
	if err := g.Check(k, ms(now), n(4), now); err != nil {
		t.Fatal(err)
	}
	if err := g.Check(k, ms(now), n(4), now.Add(59*time.Second)); !errors.Is(err, ErrReplay) {
		t.Fatalf("reused nonce: %v", err)
	}
	if err := g.Check(pub(f.other), ms(now), n(4), now); err != nil {
		t.Fatalf("the same nonce from another watch: %v", err)
	}
	if err := g.Check(k, 1<<63, n(5), now); !errors.Is(err, ErrStale) {
		t.Fatalf("ts past int64: %v", err)
	}
}

// TestS4RequestBeforeProcessStart: a request whose ts is before the Mac
// process started is refused even inside the time window, so an empty nonce
// set after a restart admits no request the previous process saw.
func TestS4RequestBeforeProcessStart(t *testing.T) {
	f := newFixture(t)
	g := NewReplayGuard(f.start)
	k := pub(f.watch)
	if err := g.Check(k, uint64(f.start.UnixMilli()-1), f.nonce, f.start.Add(time.Second)); !errors.Is(err, ErrBeforeStart) {
		t.Fatalf("1 ms before start: %v", err)
	}
	if err := g.Check(k, uint64(f.start.UnixMilli()), f.nonce, f.start.Add(time.Second)); err != nil {
		t.Fatalf("at start: %v", err)
	}
}

// TestS4ReplayAfterRestart replays a recorded request to a restarted Mac
// within its 60 s window: the new process's guard has an empty nonce set and
// still refuses it.
func TestS4ReplayAfterRestart(t *testing.T) {
	f := newFixture(t)
	sent := f.start.Add(10 * time.Second)
	body, _, err := SealRequest(f.kem.PublicKey(), f.room, f.watch, uint64(sent.UnixMilli()), f.nonce, []byte(`{"type":"cmd","action":"start"}`))
	if err != nil {
		t.Fatal(err)
	}
	isWatch := func(k ed25519.PublicKey) bool { return bytes.Equal(k, pub(f.watch)) }
	if _, _, err := OpenRequest(f.kem, f.room, body, isWatch, NewReplayGuard(f.start), sent); err != nil {
		t.Fatal(err)
	}
	restarted := sent.Add(3 * time.Second)
	if _, _, err := OpenRequest(f.kem, f.room, body, isWatch, NewReplayGuard(restarted), restarted.Add(time.Second)); !errors.Is(err, ErrBeforeStart) {
		t.Fatalf("replay after a restart: %v", err)
	}
}

// TestS4SkewResidualFreshReplyKey: a request the old process answered, whose
// ts is not before the new process's start (the watch's clock ran ahead),
// is admitted again — the residual PROTOCOL.md § 6.3 states — but the second
// reply is sealed under a key the first did not use, so the relay holding
// both learns nothing from the pair.
func TestS4SkewResidualFreshReplyKey(t *testing.T) {
	f := newFixture(t)
	sent := f.start.Add(10 * time.Second)
	body, pd, err := SealRequest(f.kem.PublicKey(), f.room, f.watch, uint64(sent.UnixMilli()), f.nonce, []byte(`{"type":"cmd"}`))
	if err != nil {
		t.Fatal(err)
	}
	isWatch := func(k ed25519.PublicKey) bool { return bytes.Equal(k, pub(f.watch)) }
	o1, _, err := OpenRequest(f.kem, f.room, body, isWatch, NewReplayGuard(f.start), sent)
	if err != nil {
		t.Fatal(err)
	}
	r1, _, _ := o1.Reply([]byte(`{"type":"reply","n":1}`))
	restart := sent.Add(-time.Second) // the new process started before ts on its own clock
	o2, _, err := OpenRequest(f.kem, f.room, body, isWatch, NewReplayGuard(restart), sent.Add(2*time.Second))
	if err != nil {
		t.Fatalf("the skew residual: %v", err)
	}
	r2, _, _ := o2.Reply([]byte(`{"type":"reply","n":2}`))
	if bytes.Equal(r1[:1+freshLen], r2[:1+freshLen]) {
		t.Fatal("two replies share the Mac's fresh value")
	}
	k1 := Watch(f.room, f.nonce).KDF(append(append([]byte(nil), o1.ex.hsM2C...), r1[1:1+freshLen]...), ExportHSM2C)
	if _, err := open(k1, 0, Watch(f.room, f.nonce).HandshakeAAD(M2C), r2[1+freshLen:]); err == nil {
		t.Fatal("the second reply opens under the first reply's key")
	}
	for _, r := range [][]byte{r1, r2} {
		if _, _, err := pd.Finish(r); err != nil {
			t.Fatalf("the watch opens either reply: %v", err)
		}
	}
}

// TestOpenRequestSignatureFirst: at the Mac a request's signature is verified
// before the guard is touched, and an unknown key, a key the Mac does not
// hold as a watch's and a bad signature are one refusal.
func TestOpenRequestSignatureFirst(t *testing.T) {
	f := newFixture(t)
	now := f.start.Add(time.Second)
	ts := uint64(now.UnixMilli())
	cmd := []byte(`{"type":"cmd"}`)
	body, _, err := SealRequest(f.kem.PublicKey(), f.room, f.watch, ts, f.nonce, cmd)
	if err != nil {
		t.Fatal(err)
	}
	isWatch := func(k ed25519.PublicKey) bool { return bytes.Equal(k, pub(f.watch)) }
	g := NewReplayGuard(f.start)
	forged, _ := wire.ParseRequest(body)
	forged.Sig = ed25519.Sign(f.other, auth.RequestMessage(f.room, forged.TS, forged.Nonce, forged.Enc, forged.Ciphertext))
	unknown, _, _ := SealRequest(f.kem.PublicKey(), f.room, f.other, ts, f.nonce, cmd)
	for name, b := range map[string][]byte{"bad signature": forged.Bytes(), "unknown key": unknown, "short": body[:wire.MinRequest-1]} {
		if _, _, err := OpenRequest(f.kem, f.room, b, isWatch, g, now); !errors.Is(err, ErrRequest) {
			t.Fatalf("%s: %v", name, err)
		}
	}
	if _, _, err := OpenRequest(f.kem, f.room, body, isWatch, g, now); err != nil {
		t.Fatalf("the genuine request after refusals with its nonce: %v", err)
	}
	// A request signed by the paired watch whose handshake another key signed.
	r := &wire.Request{TS: ts, Nonce: f.nonce}
	r.Nonce[15] ^= 1
	msg, _, _ := Begin(f.kem.PublicKey(), Watch(f.room, r.Nonce), f.other, cmd)
	r.Enc, r.Ciphertext = msg[:32], msg[32:]
	auth.SignRequest(f.watch, f.room, r)
	if _, _, err := OpenRequest(f.kem, f.room, r.Bytes(), isWatch, g, now); !errors.Is(err, ErrKeyMismatch) {
		t.Fatalf("handshake by another key: %v", err)
	}
}
