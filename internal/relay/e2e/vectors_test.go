package e2e

import (
	"bytes"
	"crypto/ecdh"
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"reflect"
	"sort"
	"testing"
	"testing/cryptotest"
	"time"

	"github.com/caoer/mega-asr/internal/relay/auth"
	"github.com/caoer/mega-asr/internal/relay/wire"
)

var update = flag.Bool("update", false, "rewrite testdata/relay-vectors.json")

const vectorFile = "testdata/relay-vectors.json"

// hx is bytes written as lowercase hex.
type hx []byte

func (h hx) MarshalJSON() ([]byte, error) { return json.Marshal(hex.EncodeToString(h)) }
func (h *hx) UnmarshalJSON(b []byte) error {
	var s string
	if err := json.Unmarshal(b, &s); err != nil {
		return err
	}
	d, err := hex.DecodeString(s)
	*h = d
	return err
}

type vRecord struct {
	Dir     string `json:"dir"`
	Counter uint64 `json:"counter"`
	Kind    byte   `json:"kind"`
	Payload hx     `json:"payload"`
	AAD     hx     `json:"aad"`
	Nonce   hx     `json:"nonce"`
	Record  hx     `json:"record"`
}

type vHandshake struct {
	Role        string    `json:"role"`
	Binding     hx        `json:"binding"`
	Ctx         hx        `json:"ctx"`
	Info        hx        `json:"info"`
	HSAADC2M    hx        `json:"hs_aad_c2m"`
	HSAADM2C    hx        `json:"hs_aad_m2c"`
	RecAADC2M   hx        `json:"rec_aad_c2m,omitempty"`
	RecAADM2C   hx        `json:"rec_aad_m2c,omitempty"`
	First       string    `json:"first"`
	EphDevSK    hx        `json:"eph_dev_sk,omitempty"`
	EphDevPK    hx        `json:"eph_dev_pk"`
	Message     hx        `json:"message"`
	Enc         hx        `json:"enc"`
	ExportHSC2M hx        `json:"export_hs_c2m"`
	ExportHSM2C hx        `json:"export_hs_m2c"`
	ExportRec   hx        `json:"export_rec"`
	Transcript  hx        `json:"transcript"`
	TSig        hx        `json:"tsig"`
	Plain       hx        `json:"plain"`
	EphMacSK    hx        `json:"eph_mac_sk,omitempty"`
	EphMacPK    hx        `json:"eph_mac_pk"`
	SS          hx        `json:"ss,omitempty"`
	HKM2C       hx        `json:"hk_m2c"`
	ReplyJSON   string    `json:"reply_json"`
	Reply       hx        `json:"reply"`
	KC2M        hx        `json:"k_c2m,omitempty"`
	KM2C        hx        `json:"k_m2c,omitempty"`
	Records     []vRecord `json:"records,omitempty"`
}

type vWatch struct {
	vHandshake
	MacFresh       hx     `json:"mac_fresh"`
	TS             uint64 `json:"ts"`
	Nonce          hx     `json:"nonce"`
	RequestMessage hx     `json:"request_message"`
	RequestSig     hx     `json:"request_sig"`
	Body           hx     `json:"body"`
}

type vAuth struct {
	Role      string `json:"role"`
	Key       hx     `json:"key"`
	Challenge hx     `json:"challenge"`
	Message   hx     `json:"message"`
	Sig       hx     `json:"sig"`
	Proof     hx     `json:"proof,omitempty"`
	Frame     string `json:"frame"`
}

type vGuardCheck struct {
	Key     string `json:"key"`
	TS      uint64 `json:"ts_ms"`
	Now     int64  `json:"now_ms"`
	Nonce   hx     `json:"nonce"`
	Verdict string `json:"verdict"`
}

type vGuard struct {
	About  string        `json:"about"`
	Start  int64         `json:"start_ms"`
	Checks []vGuardCheck `json:"checks"`
}

type vRefusal struct {
	About   string        `json:"about"`
	Opens   []bool        `json:"opens,omitempty"`
	Records []hx          `json:"records,omitempty"`
	Dir     string        `json:"dir,omitempty"`
	Message hx            `json:"message,omitempty"`
	Reply   hx            `json:"reply,omitempty"`
	OpenAs  string        `json:"open_as,omitempty"`
	Binding hx            `json:"binding,omitempty"`
	Verdict string        `json:"verdict"`
	Extra   map[string]hx `json:"extra,omitempty"`
}

type vectors struct {
	About     string `json:"about"`
	Generator string `json:"generator"`
	Keys      struct {
		RoomTagKey   hx `json:"room_tag_key"`
		ProvisionKey hx `json:"provision_key"`
		MacSigSeed   hx `json:"mac_sig_seed"`
		MacSigPK     hx `json:"mac_sig_pk"`
		MacKemSK     hx `json:"mac_kem_sk"`
		MacKemPK     hx `json:"mac_kem_pk"`
		PhoneSeed    hx `json:"phone_seed"`
		PhonePK      hx `json:"phone_pk"`
		WatchSeed    hx `json:"watch_seed"`
		WatchPK      hx `json:"watch_pk"`
		DummySeed    hx `json:"dummy_seed"`
		DummyPK      hx `json:"dummy_pk"`
	} `json:"keys"`
	Room struct {
		R     hx     `json:"r"`
		Tag   hx     `json:"tag"`
		Bytes hx     `json:"bytes"`
		Text  string `json:"text"`
	} `json:"room"`
	RoomRefused []struct {
		About      string `json:"about"`
		RoomTagKey hx     `json:"room_tag_key"`
		Text       string `json:"text"`
	} `json:"room_refused"`
	Auth     []vAuth             `json:"auth"`
	Phone    vHandshake          `json:"phone"`
	Watch    vWatch              `json:"watch"`
	Refusals map[string]vRefusal `json:"refusals"`
	Guards   []vGuard            `json:"guards"`
}

func named(name string) []byte {
	s := sha256.Sum256([]byte("megavoice-relay vector " + name))
	return s[:]
}

func x25519Key(t *testing.T, b []byte) *ecdh.PrivateKey {
	t.Helper()
	k, err := ecdh.X25519().NewPrivateKey(b)
	if err != nil {
		t.Fatal(err)
	}
	return k
}

// vectorInputs are the values the vectors are made from; the HPKE
// encapsulations are random and are the only values read back from the file.
type vectorInputs struct {
	roomTagKey, provisionKey, macSig, macKem, phone, watch []byte
	r, challengeMac, challengePhone, sid, nonce            []byte
	ephDev, ephMac, ephMac2, fresh, fresh2                 []byte
	ts                                                     uint64
	start                                                  int64
}

func inputs() vectorInputs {
	return vectorInputs{
		roomTagKey: named("room tag key"), provisionKey: named("provision key"),
		macSig: named("mac sig"), macKem: named("mac kem"), phone: named("phone"), watch: named("watch"),
		r: named("room")[:16], challengeMac: named("challenge mac"), challengePhone: named("challenge phone"),
		sid: named("sid")[:8], nonce: named("nonce")[:16],
		ephDev: named("eph dev"), ephMac: named("eph mac"), ephMac2: named("eph mac 2"),
		fresh: named("mac fresh"), fresh2: named("mac fresh 2"),
		ts: 1_790_000_123_456, start: 1_790_000_000_000,
	}
}

const (
	phoneFirst = `{"type":"hello","device":"iPhone 3fa2"}`
	phoneReply = `{"type":"hello","mac":"Mac","state":{"phase":"idle","take":"","elapsed_ms":0}}`
	watchFirst = `{"type":"cmd","id":"w-1","action":"start","via":"watch","sent_at":1790000123456}`
	watchReply = `{"type":"reply","id":"w-1","ok":true,"state":{"phase":"recording","take":"20261006-120000","elapsed_ms":0}}`
)

// messages are the values with randomness in them, made once and kept.
type messages struct {
	phone, watchBody, wrongRoleSig, lowOrderDev []byte
}

func makeMessages(t *testing.T, in vectorInputs) messages {
	cryptotest.SetGlobalRandom(t, 20261006)
	kem := x25519Key(t, in.macKem)
	room := roomOf(in)
	var sid wire.SID
	copy(sid[:], in.sid)
	var nonce [wire.NonceLen]byte
	copy(nonce[:], in.nonce)
	p := Phone(room, sid)
	phone := ed25519.NewKeyFromSeed(in.phone)
	var m messages
	var err error
	if m.phone, _, err = begin(kem.PublicKey(), p, p, phone, x25519Key(t, in.ephDev), nil, []byte(phoneFirst)); err != nil {
		t.Fatal(err)
	}
	if m.watchBody, _, err = SealRequest(kem.PublicKey(), room, ed25519.NewKeyFromSeed(in.watch), in.ts, nonce, []byte(watchFirst)); err != nil {
		t.Fatal(err)
	}
	if m.wrongRoleSig, _, err = begin(kem.PublicKey(), p, Params{room, wire.RoleWatch, p.Binding}, phone, x25519Key(t, in.ephDev), nil, []byte(phoneFirst)); err != nil {
		t.Fatal(err)
	}
	if m.lowOrderDev, _, err = begin(kem.PublicKey(), p, p, phone, nil, lowOrder, []byte(phoneFirst)); err != nil {
		t.Fatal(err)
	}
	return m
}

func roomOf(in vectorInputs) wire.RoomID {
	var id wire.RoomID
	copy(id[:], in.r)
	copy(id[16:], auth.Tag(in.roomTagKey, in.r))
	return id
}

func records(t *testing.T, s *Session, dir Dir, p Params, payloads []string) []vRecord {
	var out []vRecord
	for i, pl := range payloads {
		kind := KindJSON
		if i == 1 {
			kind = KindAudio
		}
		r, err := s.Seal(kind, []byte(pl))
		if err != nil {
			t.Fatal(err)
		}
		out = append(out, vRecord{string(dir), uint64(i), kind, hx(pl), p.RecordAAD(dir), Nonce(uint64(i)), r})
	}
	return out
}

// derive computes every vector value from the inputs and the kept messages,
// checking on the way that each step accepts or refuses as it must.
func derive(t *testing.T, in vectorInputs, m messages) vectors {
	t.Helper()
	var v vectors
	v.About = "Mega Voice relay protocol vectors (relay/PROTOCOL.md). Hex unless the field is JSON text. Generated by the Go side; checked by Go, Swift and the Worker."
	v.Generator = "go test ./internal/relay/e2e -run TestRelayVectors -update"
	kem := x25519Key(t, in.macKem)
	macSig := ed25519.NewKeyFromSeed(in.macSig)
	phone := ed25519.NewKeyFromSeed(in.phone)
	watch := ed25519.NewKeyFromSeed(in.watch)
	k := &v.Keys
	k.RoomTagKey, k.ProvisionKey = in.roomTagKey, in.provisionKey
	k.MacSigSeed, k.MacSigPK = in.macSig, hx(pub(macSig))
	k.MacKemSK, k.MacKemPK = in.macKem, kem.PublicKey().Bytes()
	k.PhoneSeed, k.PhonePK = in.phone, hx(pub(phone))
	k.WatchSeed, k.WatchPK = in.watch, hx(pub(watch))
	ds := sha256.Sum256([]byte(auth.PrefixDummy))
	k.DummySeed, k.DummyPK = ds[:], hx(auth.DummyKey)

	room := roomOf(in)
	if !auth.CheckRoom(in.roomTagKey, room) {
		t.Fatal("room tag does not check")
	}
	v.Room.R, v.Room.Tag, v.Room.Bytes, v.Room.Text = in.r, room.Tag(), room[:], room.String()
	bad := room
	bad[wire.RoomIDLen-1] ^= 1
	short := in.roomTagKey[:31]
	if auth.CheckRoom(in.roomTagKey, bad) || auth.CheckRoom(short, room) {
		t.Fatal("a refused room checks")
	}
	v.RoomRefused = append(v.RoomRefused,
		struct {
			About      string `json:"about"`
			RoomTagKey hx     `json:"room_tag_key"`
			Text       string `json:"text"`
		}{"tag's last byte flipped", in.roomTagKey, bad.String()},
		struct {
			About      string `json:"about"`
			RoomTagKey hx     `json:"room_tag_key"`
			Text       string `json:"text"`
		}{"room tag key of 31 bytes: every tag is bad", short, room.String()})

	proof := auth.ProvisionProof(in.provisionKey, room, pub(macSig))
	for _, a := range []struct {
		role  wire.Role
		sk    ed25519.PrivateKey
		chal  []byte
		proof []byte
	}{{wire.RoleMac, macSig, in.challengeMac, proof}, {wire.RolePhone, phone, in.challengePhone, nil}} {
		msg := auth.AuthMessage(room, a.role, a.chal)
		frame := auth.AuthFrame(a.sk, room, a.role, a.chal, a.proof)
		v.Auth = append(v.Auth, vAuth{string(a.role), hx(pub(a.sk)), a.chal, msg, ed25519.Sign(a.sk, msg), a.proof, string(frame)})
	}

	// The phone session.
	var sid wire.SID
	copy(sid[:], in.sid)
	pp := Phone(room, sid)
	o, err := Open(kem, pp, m.phone)
	if err != nil {
		t.Fatalf("phone message: %v", err)
	}
	ephDev, ephMac := x25519Key(t, in.ephDev), x25519Key(t, in.ephMac)
	ph := handshake(t, pp, o, m.phone, kem)
	ph.EphDevSK, ph.EphDevPK = in.ephDev, ephDev.PublicKey().Bytes()
	if !bytes.Equal(o.ephDev, ph.EphDevPK) || string(o.First) != phoneFirst || !bytes.Equal(o.DeviceKey, pub(phone)) {
		t.Fatal("phone message carries other values")
	}
	if !bytes.Equal(ph.TSig, ed25519.Sign(phone, ph.Transcript)) {
		t.Fatal("phone transcript signature is not the phone's over the transcript")
	}
	reply, macS, err := o.reply(ephMac, nil, []byte(phoneReply))
	if err != nil {
		t.Fatal(err)
	}
	ss, _ := x25519(ephMac, ph.EphDevPK)
	ph.EphMacSK, ph.EphMacPK, ph.SS = in.ephMac, ephMac.PublicKey().Bytes(), ss
	ph.HKM2C = pp.KDF(append(append([]byte(nil), o.ex.hsM2C...), ss...), ExportHSM2C)
	ph.ReplyJSON, ph.Reply = phoneReply, reply
	ikm := append(append([]byte(nil), o.ex.rec...), ss...)
	ph.KC2M, ph.KM2C = pp.KDF(ikm, string(C2M)), pp.KDF(ikm, string(M2C))
	if !bytes.Equal(macS.recvKey, ph.KC2M) || !bytes.Equal(macS.sendKey, ph.KM2C) {
		t.Fatal("Mac session keys")
	}
	devS := newSession(pp, o.ex.rec, ss, false)
	c2m := records(t, devS, C2M, pp, []string{`{"type":"cmd","id":"p-1","action":"state"}`, "\x00\x01\x02\x03", `{"type":"cmd","id":"p-2"}`})
	m2c := records(t, macS, M2C, pp, []string{`{"type":"reply","id":"p-1","ok":true}`, "\xff\xfe", `{"type":"state"}`})
	macRecv, devRecv := newSession(pp, o.ex.rec, ss, true), newSession(pp, o.ex.rec, ss, false)
	for i := range c2m {
		if _, _, err := macRecv.Open(c2m[i].Record); err != nil {
			t.Fatal(err)
		}
		if _, _, err := devRecv.Open(m2c[i].Record); err != nil {
			t.Fatal(err)
		}
	}
	ph.Records = append(c2m, m2c...)
	v.Phone = ph

	// The watch request.
	var watchReplay vRefusal
	var nonce [wire.NonceLen]byte
	copy(nonce[:], in.nonce)
	isWatch := func(key ed25519.PublicKey) bool { return bytes.Equal(key, pub(watch)) }
	now := time.UnixMilli(int64(in.ts) + 500)
	wo, req, err := OpenRequest(kem, room, m.watchBody, isWatch, NewReplayGuard(time.UnixMilli(in.start)), now)
	if err != nil {
		t.Fatalf("watch body: %v", err)
	}
	if req.TS != in.ts || req.Nonce != nonce || string(wo.First) != watchFirst {
		t.Fatal("watch body carries other values")
	}
	wp := Watch(room, nonce)
	w := vWatch{vHandshake: handshake(t, wp, wo, req.Message(), kem)}
	if !bytes.Equal(w.TSig, ed25519.Sign(watch, w.Transcript)) {
		t.Fatal("watch transcript signature")
	}
	w.EphDevPK = hx{}
	w.TS, w.Nonce = in.ts, nonce[:]
	w.RequestMessage = auth.RequestMessage(room, req.TS, req.Nonce, req.Enc, req.Ciphertext)
	w.RequestSig = ed25519.Sign(watch, w.RequestMessage)
	if !bytes.Equal(w.RequestSig, req.Sig) {
		t.Fatal("watch request signature")
	}
	w.Body = m.watchBody
	wr, ws, err := wo.reply(nil, in.fresh, []byte(watchReply))
	if err != nil || ws != nil {
		t.Fatalf("watch reply: %v", err)
	}
	w.EphMacPK, w.ReplyJSON, w.Reply = hx{}, watchReply, wr
	w.MacFresh = in.fresh
	w.HKM2C = wp.KDF(append(append([]byte(nil), wo.ex.hsM2C...), in.fresh...), ExportHSM2C)
	if !bytes.Equal(wr[:1+freshLen], wire.AppendLP(nil, in.fresh)) {
		t.Fatal("watch reply does not lead with the Mac's fresh value")
	}
	// The same request opened again (a restart inside the clock skew of
	// PROTOCOL.md § 6.3): the second reply shares no key with the first.
	wo2, err := Open(kem, wp, req.Message())
	if err != nil {
		t.Fatal(err)
	}
	wr2, _, err := wo2.reply(nil, in.fresh2, []byte(watchReply))
	if err != nil {
		t.Fatal(err)
	}
	hk2 := wp.KDF(append(append([]byte(nil), wo.ex.hsM2C...), in.fresh2...), ExportHSM2C)
	if bytes.Equal(hk2, w.HKM2C) || bytes.Equal(wr2[1+freshLen:], wr[1+freshLen:]) {
		t.Fatal("two replies to one watch request share a key")
	}
	watchReplay = vRefusal{About: "the watch request answered a second time, with another fresh value: the two replies share no key", Reply: wr2, Verdict: "fresh keys", Extra: map[string]hx{"fresh": in.fresh2, "hk_m2c": hk2}}
	v.Watch = w

	v.Refusals = refusals(t, in, m, kem, room, pp, o, ph)
	v.Refusals["replayed_watch_request"] = watchReplay
	v.Guards = guards(t, in, m.watchBody, kem, room, isWatch)
	return v
}

func handshake(t *testing.T, p Params, o *Opened, message []byte, kem *ecdh.PrivateKey) vHandshake {
	t.Helper()
	plain, err := open(o.ex.hsC2M, 0, p.HandshakeAAD(C2M), message[encLen:])
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(Seal(o.ex.hsC2M, 0, p.HandshakeAAD(C2M), plain), message[encLen:]) {
		t.Fatal("handshake ciphertext does not re-seal")
	}
	h := vHandshake{
		Role: string(p.Role), Binding: p.Binding, Ctx: p.Ctx(), Info: p.Info(),
		HSAADC2M: p.HandshakeAAD(C2M), HSAADM2C: p.HandshakeAAD(M2C),
		First: string(o.First), EphDevPK: hx(o.ephDev), Message: message, Enc: message[:encLen],
		ExportHSC2M: o.ex.hsC2M, ExportHSM2C: o.ex.hsM2C, ExportRec: o.ex.rec,
		Transcript: p.Transcript(message[:encLen], o.ephDev, kem.PublicKey().Bytes()),
		TSig:       plain[32:96], Plain: plain,
	}
	if p.Role == wire.RolePhone {
		h.RecAADC2M, h.RecAADM2C = p.RecordAAD(C2M), p.RecordAAD(M2C)
	}
	return h
}

func refusals(t *testing.T, in vectorInputs, m messages, kem *ecdh.PrivateKey, room wire.RoomID, pp Params, o *Opened, ph vHandshake) map[string]vRefusal {
	t.Helper()
	out := map[string]vRefusal{}
	c2m0, m2c0 := ph.Records[0].Record, ph.Records[3].Record
	for _, c := range []struct {
		name string
		dir  Dir
		r    []byte
	}{{"repeated_counter_c2m", C2M, c2m0}, {"repeated_counter_m2c", M2C, m2c0}} {
		s := newSession(pp, o.ex.rec, ph.SS, c.dir == C2M)
		_, _, e1 := s.Open(c.r)
		_, _, e2 := s.Open(c.r)
		if e1 != nil || e2 == nil {
			t.Fatalf("%s: %v %v", c.name, e1, e2)
		}
		out[c.name] = vRefusal{About: "record 0 delivered twice: the second does not open as record 1", Dir: string(c.dir), Records: []hx{c.r, c.r}, Opens: []bool{true, false}, Verdict: "torn down"}
	}
	hsRec := map[Dir][]byte{
		C2M: Seal(o.ex.hsC2M, 0, pp.RecordAAD(C2M), []byte(`{}`)),
		M2C: Seal(ph.HKM2C, 0, pp.RecordAAD(M2C), []byte(`{}`)),
	}
	for d, r := range hsRec {
		s := newSession(pp, o.ex.rec, ph.SS, d == C2M)
		if _, _, err := s.Open(r); err == nil {
			t.Fatalf("handshake-key record %s opened", d)
		}
		key := ph.ExportHSC2M
		if d == M2C {
			key = ph.HKM2C
		}
		out["handshake_key_record_"+string(d)] = vRefusal{About: "record 0 sealed under the handshake key of its direction", Dir: string(d), Records: []hx{r}, Opens: []bool{false}, Verdict: "torn down", Extra: map[string]hx{"key": key}}
	}
	o2, err := Open(kem, pp, m.phone)
	if err != nil {
		t.Fatal(err)
	}
	eph2 := x25519Key(t, in.ephMac2)
	reply2, s2, err := o2.reply(eph2, nil, []byte(phoneReply))
	if err != nil {
		t.Fatal(err)
	}
	ss2, _ := x25519(eph2, ph.EphDevPK)
	hk2 := pp.KDF(append(append([]byte(nil), o.ex.hsM2C...), ss2...), ExportHSM2C)
	if bytes.Equal(hk2, ph.HKM2C) || bytes.Equal(s2.sendKey, ph.KM2C) || bytes.Equal(s2.recvKey, ph.KC2M) {
		t.Fatal("a replayed handshake shares a key")
	}
	if _, _, err := s2.Open(c2m0); err == nil {
		t.Fatal("a record of the first session opened in the replayed one")
	}
	out["replayed_handshake"] = vRefusal{
		About: "the phone message answered a second time, with another Mac ephemeral: no key is shared with the first reply, and the first session's record 0 does not open in the second",
		Reply: reply2, Records: []hx{c2m0}, Opens: []bool{false}, Verdict: "fresh keys",
		Extra: map[string]hx{"eph_mac_sk": in.ephMac2, "eph_mac_pk": eph2.PublicKey().Bytes(), "ss": ss2, "hk_m2c": hk2, "k_c2m": s2.recvKey, "k_m2c": s2.sendKey},
	}
	if _, err := Open(kem, pp, m.wrongRoleSig); !errors.Is(err, ErrSignature) {
		t.Fatalf("wrong-role signature: %v", err)
	}
	out["wrong_role_signature"] = vRefusal{About: "a phone message whose transcript was signed for role watch: it opens as phone, the signature does not verify", Message: m.wrongRoleSig, OpenAs: "phone", Binding: pp.Binding, Verdict: "signature"}
	if _, err := Open(kem, Params{room, wire.RoleWatch, pp.Binding}, m.phone); !errors.Is(err, ErrOpen) {
		t.Fatalf("wrong-role info: %v", err)
	}
	out["wrong_role_info"] = vRefusal{About: "the phone message opened as role watch with the same binding: HPKE info differs, it does not open", Message: m.phone, OpenAs: "watch", Binding: pp.Binding, Verdict: "open"}
	lo, err := Open(kem, pp, m.lowOrderDev)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := lo.Reply([]byte(`{}`)); !errors.Is(err, ErrLowOrder) {
		t.Fatalf("low-order device ephemeral: %v", err)
	}
	out["low_order_device"] = vRefusal{About: "a phone message whose ephemeral is a point of order 8: the Mac refuses to reply", Message: m.lowOrderDev, OpenAs: "phone", Binding: pp.Binding, Verdict: "low order", Extra: map[string]hx{"eph_pk": lowOrder}}
	loReply := append(wire.AppendLP(nil, lowOrder), Seal(make([]byte, 32), 0, nil, []byte(`{}`))...)
	out["low_order_mac"] = vRefusal{About: "a reply to the phone message whose Mac ephemeral is a point of order 8: the phone refuses it", Reply: loReply, Verdict: "low order", Extra: map[string]hx{"eph_pk": lowOrder}}
	return out
}

func guards(t *testing.T, in vectorInputs, body []byte, kem *ecdh.PrivateKey, room wire.RoomID, isWatch func(ed25519.PublicKey) bool) []vGuard {
	t.Helper()
	n := func(b byte) hx { x := make(hx, wire.NonceLen); x[0] = b; return x }
	watchKey := hex.EncodeToString(ed25519.NewKeyFromSeed(in.watch).Public().(ed25519.PublicKey))
	s := in.start
	run := []vGuard{
		{About: "one process: the window, the start bound and nonce reuse", Start: s, Checks: []vGuardCheck{
			{watchKey, uint64(s + 10_000), s + 10_500, n(1), "ok"},
			{watchKey, uint64(s + 10_000), s + 11_000, n(1), "replay"},
			{watchKey, uint64(s - 1), s + 1_000, n(2), "before_start"},
			{watchKey, uint64(s + 100_000), s + 160_001, n(3), "stale"},
			{watchKey, uint64(s + 300_001), s + 240_000, n(4), "stale"},
			{watchKey, uint64(s + 100_000), s + 160_000, n(5), "ok"},
		}},
		{About: "after a restart: the request accepted at ts by a process that started earlier is replayed to one that started after ts", Start: int64(in.ts) + 2_000, Checks: []vGuardCheck{
			{watchKey, in.ts, int64(in.ts) + 3_000, hx(in.nonce), "before_start"},
		}},
	}
	for _, g := range run {
		guard := NewReplayGuard(time.UnixMilli(g.Start))
		for _, c := range g.Checks {
			key, _ := hex.DecodeString(c.Key)
			var nn [wire.NonceLen]byte
			copy(nn[:], c.Nonce)
			if got := verdict(guard.Check(key, c.TS, nn, time.UnixMilli(c.Now))); got != c.Verdict {
				t.Fatalf("%s: ts %d now %d: %s, want %s", g.About, c.TS, c.Now, got, c.Verdict)
			}
		}
	}
	// The restart case is the watch body itself, replayed.
	if _, _, err := OpenRequest(kem, room, body, isWatch, NewReplayGuard(time.UnixMilli(run[1].Start)), time.UnixMilli(run[1].Checks[0].Now)); !errors.Is(err, ErrBeforeStart) {
		t.Fatalf("watch body after a restart: %v", err)
	}
	return run
}

func verdict(err error) string {
	switch {
	case err == nil:
		return "ok"
	case errors.Is(err, ErrStale):
		return "stale"
	case errors.Is(err, ErrBeforeStart):
		return "before_start"
	case errors.Is(err, ErrReplay):
		return "replay"
	}
	return err.Error()
}

func encodeVectors(t *testing.T, v vectors) []byte {
	b, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	return append(b, '\n')
}

// TestRelayVectors keeps testdata/relay-vectors.json equal to what this
// package derives from the vector inputs and the recorded HPKE messages, so
// the Swift and Worker suites that read the file check the Go implementation.
func TestRelayVectors(t *testing.T) {
	in := inputs()
	if *update {
		v := derive(t, in, makeMessages(t, in))
		if err := os.WriteFile(vectorFile, encodeVectors(t, v), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	raw, err := os.ReadFile(vectorFile)
	if err != nil {
		t.Fatal(err)
	}
	var kept vectors
	if err := json.Unmarshal(raw, &kept); err != nil {
		t.Fatal(err)
	}
	m := messages{
		phone:        kept.Phone.Message,
		watchBody:    kept.Watch.Body,
		wrongRoleSig: kept.Refusals["wrong_role_signature"].Message,
		lowOrderDev:  kept.Refusals["low_order_device"].Message,
	}
	got := encodeVectors(t, derive(t, in, m))
	if !bytes.Equal(got, raw) {
		var a, b any
		json.Unmarshal(got, &a)
		json.Unmarshal(raw, &b)
		t.Fatalf("%s differs from the derivation (run with -update after a deliberate change): %v", vectorFile, diffPaths("", a, b))
	}
}

func diffPaths(at string, a, b any) []string {
	if reflect.DeepEqual(a, b) {
		return nil
	}
	am, aok := a.(map[string]any)
	bm, bok := b.(map[string]any)
	if !aok || !bok {
		return []string{at}
	}
	keys := map[string]bool{}
	for k := range am {
		keys[k] = true
	}
	for k := range bm {
		keys[k] = true
	}
	var names []string
	for k := range keys {
		names = append(names, k)
	}
	sort.Strings(names)
	var out []string
	for _, k := range names {
		out = append(out, diffPaths(fmt.Sprintf("%s.%s", at, k), am[k], bm[k])...)
	}
	return out
}
