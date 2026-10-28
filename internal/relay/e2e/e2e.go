// Package e2e is the relay's end-to-end channel (PROTOCOL.md § 7): an HPKE
// handshake from a device to the Mac's recipient key, bound to the room, the
// role and the session, signed by the device; for a phone session an
// ephemeral X25519 exchange that gives the reply and the records forward
// secrecy; records sealed with ChaCha20-Poly1305 under per-direction keys and
// implicit counters; and the Mac's replay guard for one-shot watch requests.
//
// The Mac is the recipient (Open, Opened.Reply, OpenRequest). The device side
// (Begin, Pending.Finish, SealRequest) mirrors the phone's and the watch's
// RelayCore, for tests and for the vectors Swift checks.
package e2e

import (
	"crypto/ecdh"
	"crypto/ed25519"
	"crypto/hkdf"
	"crypto/hpke"
	"crypto/rand"
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"sync/atomic"

	"golang.org/x/crypto/chacha20poly1305"

	"github.com/caoer/mega-asr/internal/relay/wire"
)

// Domain prefixes and labels (PROTOCOL.md § 5.2, § 7.2–7.3).
const (
	PrefixE2E = "megavoice-relay/e2e/v1"
	PrefixHS  = "megavoice-relay/hs/v1"
	PrefixRec = "megavoice-relay/rec/v1"

	ExportHSC2M = "hs-c2m"
	ExportHSM2C = "hs-m2c"
	ExportRec   = "rec"
)

// Dir is a direction: device to Mac or Mac to device.
type Dir string

const (
	C2M Dir = "c2m"
	M2C Dir = "m2c"
)

// Record kinds (PROTOCOL.md § 7.4).
const (
	KindJSON  byte = 0x01
	KindAudio byte = 0x02
)

var (
	ErrFormat    = errors.New("relay e2e: malformed message")
	ErrOpen      = errors.New("relay e2e: does not open")
	ErrSignature = errors.New("relay e2e: transcript signature does not verify")
	ErrLowOrder  = errors.New("relay e2e: low-order ephemeral key")
	ErrRole      = errors.New("relay e2e: role is not phone or watch")
	ErrClosed    = errors.New("relay e2e: session torn down")
	ErrTooBig    = errors.New("relay e2e: record over the limit")
)

// The suite of PROTOCOL.md § 7.1.
var (
	kem  = hpke.DHKEM(ecdh.X25519())
	kdf  = hpke.HKDFSHA256()
	aead = hpke.ChaCha20Poly1305()
)

const (
	encLen   = 32
	keyLen   = 32
	tagLen   = 16
	ephLen   = 32
	freshLen = 32 // the Mac's value in a reply: its ephemeral key, or random bytes for a watch
	plainMin = ed25519.PublicKeySize + ed25519.SignatureSize + 1
)

// Params names a session: the room, the role taken from the session kind, and
// the binding — the sid of a phone session, the nonce of a watch request.
type Params struct {
	Room    wire.RoomID
	Role    wire.Role
	Binding []byte
}

// Phone is a phone session's Params.
func Phone(room wire.RoomID, sid wire.SID) Params {
	return Params{room, wire.RolePhone, append([]byte(nil), sid[:]...)}
}

// Watch is a watch request's Params.
func Watch(room wire.RoomID, nonce [wire.NonceLen]byte) Params {
	return Params{room, wire.RoleWatch, append([]byte(nil), nonce[:]...)}
}

func (p Params) check() error {
	if p.Role != wire.RolePhone && p.Role != wire.RoleWatch {
		return ErrRole
	}
	return nil
}

func (p Params) ephemeral() bool { return p.Role == wire.RolePhone }

// Ctx is room ‖ lp(role) ‖ lp(binding).
func (p Params) Ctx() []byte {
	out := append([]byte(nil), p.Room[:]...)
	out = wire.AppendLP(out, []byte(p.Role))
	return wire.AppendLP(out, p.Binding)
}

// Info is the HPKE info.
func (p Params) Info() []byte { return append([]byte(PrefixE2E), p.Ctx()...) }

// HandshakeAAD is the associated data of a handshake message in direction d.
func (p Params) HandshakeAAD(d Dir) []byte {
	return wire.AppendLP(append([]byte(PrefixHS), p.Ctx()...), []byte(d))
}

// RecordAAD is the associated data of a record in direction d.
func (p Params) RecordAAD(d Dir) []byte {
	return wire.AppendLP(append([]byte(PrefixRec), p.Ctx()...), []byte(d))
}

// KDF is HKDF-SHA256 with an empty salt, ikm, and info
// "megavoice-relay/rec/v1" ‖ ctx ‖ lp(label), 32 bytes.
func (p Params) KDF(ikm []byte, label string) []byte {
	info := wire.AppendLP(append([]byte(PrefixRec), p.Ctx()...), []byte(label))
	k, err := hkdf.Key(sha256.New, ikm, nil, string(info), keyLen)
	if err != nil {
		panic(err) // only for an output over 255 × 32 bytes
	}
	return k
}

// Transcript is the byte string a device signs in its handshake.
func (p Params) Transcript(enc, ephDev, macKem []byte) []byte {
	out := append([]byte(PrefixE2E), p.Ctx()...)
	out = append(out, enc...)
	out = wire.AppendLP(out, ephDev)
	return append(out, macKem...)
}

// Nonce is the AEAD nonce for counter n: 4 zero bytes ‖ u64be(n).
func Nonce(n uint64) []byte {
	b := make([]byte, chacha20poly1305.NonceSize)
	binary.BigEndian.PutUint64(b[4:], n)
	return b
}

// Seal is ChaCha20-Poly1305 under key with counter n.
func Seal(key []byte, n uint64, aad, plaintext []byte) []byte {
	c, err := chacha20poly1305.New(key)
	if err != nil {
		panic(err) // key is always 32 bytes
	}
	return c.Seal(nil, Nonce(n), plaintext, aad)
}

func open(key []byte, n uint64, aad, ciphertext []byte) ([]byte, error) {
	c, err := chacha20poly1305.New(key)
	if err != nil {
		return nil, ErrOpen
	}
	pt, err := c.Open(nil, Nonce(n), ciphertext, aad)
	if err != nil {
		return nil, ErrOpen
	}
	return pt, nil
}

// x25519 is X25519(sk, pk) refusing the all-zero result of a low-order point.
func x25519(sk *ecdh.PrivateKey, pub []byte) ([]byte, error) {
	pk, err := ecdh.X25519().NewPublicKey(pub)
	if err != nil {
		return nil, ErrLowOrder
	}
	ss, err := sk.ECDH(pk)
	if err != nil {
		return nil, ErrLowOrder
	}
	return ss, nil
}

// exporter is the side of an HPKE context both ends share.
type exporter interface {
	Export(context string, length int) ([]byte, error)
}

type exports struct{ hsC2M, hsM2C, rec []byte }

func export(x exporter) (exports, error) {
	var e exports
	var err error
	if e.hsC2M, err = x.Export(ExportHSC2M, keyLen); err != nil {
		return e, err
	}
	if e.hsM2C, err = x.Export(ExportHSM2C, keyLen); err != nil {
		return e, err
	}
	e.rec, err = x.Export(ExportRec, keyLen)
	return e, err
}

// Opened is a device's handshake message the Mac opened and whose transcript
// signature verified. Whether DeviceKey may command this Mac in this role is
// the caller's to decide (phones.toml); Reply answers it either way.
type Opened struct {
	Params    Params
	DeviceKey ed25519.PublicKey
	First     []byte // the first JSON object
	ex        exports
	ephDev    []byte
	replied   atomic.Bool
}

// Open opens a device's handshake message with the Mac's recipient key under
// p, whose role the caller took from the session kind. It refuses a message
// that does not open under this room, role and binding, a malformed
// plaintext, and a transcript signature that does not verify.
func Open(macKem *ecdh.PrivateKey, p Params, message []byte) (*Opened, error) {
	if err := p.check(); err != nil {
		return nil, err
	}
	if len(message) < encLen+plainMin+tagLen || len(message) > wire.MaxFrame {
		return nil, ErrFormat
	}
	sk, err := hpke.NewDHKEMPrivateKey(macKem)
	if err != nil {
		return nil, err
	}
	enc, sealed := message[:encLen], message[encLen:]
	r, err := hpke.NewRecipient(enc, sk, kdf, aead, p.Info())
	if err != nil {
		return nil, ErrOpen
	}
	ex, err := export(r)
	if err != nil {
		return nil, ErrOpen
	}
	plain, err := open(ex.hsC2M, 0, p.HandshakeAAD(C2M), sealed)
	if err != nil {
		return nil, err
	}
	devKey, sig, eph, first, err := splitPlain(plain)
	if err != nil {
		return nil, err
	}
	if (p.ephemeral() && len(eph) != ephLen) || (!p.ephemeral() && len(eph) != 0) {
		return nil, ErrFormat
	}
	if !ed25519.Verify(devKey, p.Transcript(enc, eph, macKem.PublicKey().Bytes()), sig) {
		return nil, ErrSignature
	}
	return &Opened{Params: p, DeviceKey: devKey, First: first, ex: ex, ephDev: eph}, nil
}

func splitPlain(plain []byte) (key ed25519.PublicKey, sig, eph, first []byte, err error) {
	if len(plain) < plainMin {
		return nil, nil, nil, nil, ErrFormat
	}
	key = ed25519.PublicKey(append([]byte(nil), plain[:32]...))
	sig = plain[32:96]
	n := int(plain[96])
	rest := plain[97:]
	if n > len(rest) {
		return nil, nil, nil, nil, ErrFormat
	}
	return key, sig, rest[:n], rest[n:], nil
}

// Reply seals the Mac's answer to an opened handshake under a key no earlier
// reply used: for a phone session a fresh ephemeral key, whose exchange also
// gives the session's record keys; for a watch request 32 fresh random bytes
// and no session. So a handshake replayed past every other check still never
// gets a reply under a key used before. An Opened answers once.
func (o *Opened) Reply(replyJSON []byte) ([]byte, *Session, error) {
	if o.Params.ephemeral() {
		eph, err := ecdh.X25519().GenerateKey(rand.Reader)
		if err != nil {
			return nil, nil, err
		}
		return o.reply(eph, nil, replyJSON)
	}
	fresh := make([]byte, freshLen)
	if _, err := rand.Read(fresh); err != nil {
		return nil, nil, err
	}
	return o.reply(nil, fresh, replyJSON)
}

func (o *Opened) reply(eph *ecdh.PrivateKey, fresh, replyJSON []byte) ([]byte, *Session, error) {
	if !o.replied.CompareAndSwap(false, true) {
		return nil, nil, errors.New("relay e2e: handshake already answered")
	}
	p := o.Params
	if !p.ephemeral() {
		if len(fresh) != freshLen {
			return nil, nil, ErrFormat
		}
		hk := p.KDF(append(append([]byte(nil), o.ex.hsM2C...), fresh...), ExportHSM2C)
		out := wire.AppendLP(nil, fresh)
		return append(out, Seal(hk, 0, p.HandshakeAAD(M2C), replyJSON)...), nil, nil
	}
	ss, err := x25519(eph, o.ephDev)
	if err != nil {
		return nil, nil, err
	}
	hk := p.KDF(append(append([]byte(nil), o.ex.hsM2C...), ss...), ExportHSM2C)
	out := wire.AppendLP(nil, eph.PublicKey().Bytes())
	out = append(out, Seal(hk, 0, p.HandshakeAAD(M2C), replyJSON)...)
	s := newSession(p, o.ex.rec, ss, true)
	return out, s, nil
}

// Pending is a device's handshake awaiting the Mac's reply.
type Pending struct {
	p   Params
	ex  exports
	eph *ecdh.PrivateKey
}

// Begin makes a device's handshake message to the Mac's recipient key: an
// HPKE encapsulation under p, a fresh ephemeral key for a phone session, and
// the transcript signature by device.
func Begin(macKem *ecdh.PublicKey, p Params, device ed25519.PrivateKey, first []byte) ([]byte, *Pending, error) {
	var eph *ecdh.PrivateKey
	if p.ephemeral() {
		var err error
		if eph, err = ecdh.X25519().GenerateKey(rand.Reader); err != nil {
			return nil, nil, err
		}
	}
	return begin(macKem, p, p, device, eph, nil, first)
}

// begin takes the signature's Params apart from the HPKE info's, and an
// ephemeral public key apart from eph, so that a test can sign for one role and
// encapsulate for another, or send a key it holds no private key for.
func begin(macKem *ecdh.PublicKey, p, sigParams Params, device ed25519.PrivateKey, eph *ecdh.PrivateKey, ephPub, first []byte) ([]byte, *Pending, error) {
	if err := p.check(); err != nil {
		return nil, nil, err
	}
	pk, err := hpke.NewDHKEMPublicKey(macKem)
	if err != nil {
		return nil, nil, err
	}
	enc, s, err := hpke.NewSender(pk, kdf, aead, p.Info())
	if err != nil {
		return nil, nil, err
	}
	ex, err := export(s)
	if err != nil {
		return nil, nil, err
	}
	if eph != nil && ephPub == nil {
		ephPub = eph.PublicKey().Bytes()
	}
	sig := ed25519.Sign(device, sigParams.Transcript(enc, ephPub, macKem.Bytes()))
	plain := append([]byte(nil), device.Public().(ed25519.PublicKey)...)
	plain = append(plain, sig...)
	plain = wire.AppendLP(plain, ephPub)
	plain = append(plain, first...)
	msg := append(append([]byte(nil), enc...), Seal(ex.hsC2M, 0, p.HandshakeAAD(C2M), plain)...)
	return msg, &Pending{p: p, ex: ex, eph: eph}, nil
}

// Finish opens the Mac's reply. For a phone session it returns the session.
func (pd *Pending) Finish(reply []byte) ([]byte, *Session, error) {
	if len(reply) < 1 {
		return nil, nil, ErrFormat
	}
	n := int(reply[0])
	rest := reply[1:]
	p := pd.p
	if n != freshLen || len(rest) < freshLen {
		return nil, nil, ErrFormat
	}
	if !p.ephemeral() {
		hk := p.KDF(append(append([]byte(nil), pd.ex.hsM2C...), rest[:freshLen]...), ExportHSM2C)
		pt, err := open(hk, 0, p.HandshakeAAD(M2C), rest[freshLen:])
		return pt, nil, err
	}
	ss, err := x25519(pd.eph, rest[:ephLen])
	if err != nil {
		return nil, nil, err
	}
	hk := p.KDF(append(append([]byte(nil), pd.ex.hsM2C...), ss...), ExportHSM2C)
	pt, err := open(hk, 0, p.HandshakeAAD(M2C), rest[ephLen:])
	if err != nil {
		return nil, nil, err
	}
	return pt, newSession(p, pd.ex.rec, ss, false), nil
}

// Session is a phone session's records after the handshake. One goroutine may
// Seal while another Opens; neither is safe for concurrent use with itself.
// Any record that does not open tears the session down: every later call
// fails with ErrClosed.
type Session struct {
	sendKey, recvKey []byte
	sendAAD, recvAAD []byte
	sendN, recvN     uint64
	dead             atomic.Bool
}

func newSession(p Params, rec, ss []byte, mac bool) *Session {
	ikm := append(append([]byte(nil), rec...), ss...)
	c2m, m2c := p.KDF(ikm, string(C2M)), p.KDF(ikm, string(M2C))
	s := &Session{}
	if mac {
		s.sendKey, s.recvKey = m2c, c2m
		s.sendAAD, s.recvAAD = p.RecordAAD(M2C), p.RecordAAD(C2M)
	} else {
		s.sendKey, s.recvKey = c2m, m2c
		s.sendAAD, s.recvAAD = p.RecordAAD(C2M), p.RecordAAD(M2C)
	}
	return s
}

// Seal seals the next record in this side's direction.
func (s *Session) Seal(kind byte, payload []byte) ([]byte, error) {
	if s.dead.Load() {
		return nil, ErrClosed
	}
	if 1+len(payload)+tagLen > wire.MaxRecord {
		return nil, ErrTooBig
	}
	if s.sendN == ^uint64(0) {
		s.dead.Store(true)
		return nil, ErrClosed
	}
	pt := append([]byte{kind}, payload...)
	out := Seal(s.sendKey, s.sendN, s.sendAAD, pt)
	s.sendN++
	return out, nil
}

// Open opens the next record from the other side. A record that does not open
// as the next one, or has no kind byte, tears the session down.
func (s *Session) Open(record []byte) (byte, []byte, error) {
	if s.dead.Load() {
		return 0, nil, ErrClosed
	}
	pt, err := open(s.recvKey, s.recvN, s.recvAAD, record)
	if err != nil || len(pt) < 1 {
		s.dead.Store(true)
		if err == nil {
			err = ErrFormat
		}
		return 0, nil, err
	}
	s.recvN++
	return pt[0], pt[1:], nil
}

// Close tears the session down.
func (s *Session) Close() { s.dead.Store(true) }
