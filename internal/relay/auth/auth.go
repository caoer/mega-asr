// Package auth is the relay's authentication: room id tags, provision proofs,
// the WebSocket challenge signature, the watch request signature, and the
// order in which a room checks them (PROTOCOL.md § 4.2, § 4.3, § 6.2).
//
// Every refusal is the one error ErrRefused, and every check that refuses has
// run exactly one Ed25519 verify first — against the registered key, or
// against DummyKey with the result discarded — so that a refused unknown key,
// a key of another role and a bad signature cannot be told apart. Only a
// request whose signature verified touches the nonce set or the rate bucket.
package auth

import (
	"crypto/ed25519"
	"crypto/hmac"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"io"
	"strings"
	"sync/atomic"
	"time"

	"github.com/caoer/mega-asr/internal/relay/wire"
)

// Domain prefixes (PROTOCOL.md § 5.2).
const (
	PrefixRoom      = "megavoice-relay/room/v1"
	PrefixProvision = "megavoice-relay/provision/v1"
	PrefixAuth      = "megavoice-relay/auth/v1"
	PrefixCmd       = "megavoice-relay/cmd/v1"
	PrefixDummy     = "megavoice-relay/dummy/v1"
)

// SecretLen is the size of ROOM_TAG_KEY and PROVISION_KEY.
const SecretLen = 32

var (
	// ErrRefused is every refusal: the room closes 1008 or answers 404.
	ErrRefused = errors.New("relay: refused")
	// ErrRate is a verified request over its key's bucket: 429.
	ErrRate = errors.New("relay: over the rate")
)

// DummyKey is verified against, result discarded, where no key is registered
// (PROTOCOL.md § 5.1). Its private key is public.
var DummyKey = func() ed25519.PublicKey {
	seed := sha256.Sum256([]byte(PrefixDummy))
	return ed25519.NewKeyFromSeed(seed[:]).Public().(ed25519.PublicKey)
}()

// ParseSecret reads a secret as stored: 64 hex characters, a trailing newline
// ignored. Anything else is no secret (nil), and what it gates refuses.
func ParseSecret(s string) []byte {
	s = strings.TrimSuffix(strings.TrimSuffix(s, "\n"), "\r")
	if len(s) != 2*SecretLen || strings.ToLower(s) != s {
		return nil
	}
	b, err := hex.DecodeString(s)
	if err != nil {
		return nil
	}
	return b
}

func validSecret(k []byte) bool { return len(k) == SecretLen }

// Tag is a room id's tag for the random part r.
func Tag(roomTagKey, r []byte) []byte {
	m := hmac.New(sha256.New, roomTagKey)
	m.Write([]byte(PrefixRoom))
	m.Write(r)
	return m.Sum(nil)[:wire.RoomTagLen]
}

// MintRoom makes a new room id. It refuses a room tag key that is not 32
// bytes.
func MintRoom(roomTagKey []byte, rnd io.Reader) (wire.RoomID, error) {
	var id wire.RoomID
	if !validSecret(roomTagKey) {
		return id, errors.New("relay: room tag key is not 32 bytes")
	}
	if _, err := io.ReadFull(rnd, id[:wire.RoomRandLen]); err != nil {
		return id, err
	}
	copy(id[wire.RoomRandLen:], Tag(roomTagKey, id.Rand()))
	return id, nil
}

// CheckRoom reports whether the room id's tag is valid. With a room tag key
// that is not 32 bytes every tag is bad.
func CheckRoom(roomTagKey []byte, id wire.RoomID) bool {
	if !validSecret(roomTagKey) {
		return false
	}
	return hmac.Equal(id.Tag(), Tag(roomTagKey, id.Rand()))
}

// ProvisionProof is the proof a Mac shows to provision an empty room.
func ProvisionProof(provisionKey []byte, room wire.RoomID, key ed25519.PublicKey) []byte {
	m := hmac.New(sha256.New, provisionKey)
	m.Write([]byte(PrefixProvision))
	m.Write(room[:])
	m.Write(key)
	return m.Sum(nil)
}

// CheckProvision reports whether proof provisions room for key. With a
// provision key that is not 32 bytes every proof is refused.
func CheckProvision(provisionKey []byte, room wire.RoomID, key ed25519.PublicKey, proof []byte) bool {
	if !validSecret(provisionKey) {
		return false
	}
	return hmac.Equal(proof, ProvisionProof(provisionKey, room, key))
}

// AuthMessage is the byte string a client signs to answer a challenge.
func AuthMessage(room wire.RoomID, role wire.Role, nonce []byte) []byte {
	out := []byte(PrefixAuth)
	out = append(out, room[:]...)
	out = wire.AppendLP(out, []byte(role))
	return append(out, nonce...)
}

// AuthFrame is a client's auth frame for a challenge nonce; proof is nil
// unless it provisions.
func AuthFrame(sk ed25519.PrivateKey, room wire.RoomID, role wire.Role, nonce, proof []byte) []byte {
	return wire.Encode(wire.Auth{
		V:     wire.AuthVersion,
		Role:  role,
		Key:   sk.Public().(ed25519.PublicKey),
		Sig:   ed25519.Sign(sk, AuthMessage(room, role, nonce)),
		Proof: proof,
	})
}

// RequestMessage is the byte string a watch signs for a request.
func RequestMessage(room wire.RoomID, ts uint64, nonce [wire.NonceLen]byte, enc, ciphertext []byte) []byte {
	out := make([]byte, 0, len(PrefixCmd)+wire.RoomIDLen+8+wire.NonceLen+len(enc)+len(ciphertext))
	out = append(out, PrefixCmd...)
	out = append(out, room[:]...)
	out = binary.BigEndian.AppendUint64(out, ts)
	out = append(out, nonce[:]...)
	out = append(out, enc...)
	return append(out, ciphertext...)
}

// SignRequest fills r.Key and r.Sig for the watch key sk.
func SignRequest(sk ed25519.PrivateKey, room wire.RoomID, r *wire.Request) {
	r.Key = sk.Public().(ed25519.PublicKey)
	r.Sig = ed25519.Sign(sk, RequestMessage(room, r.TS, r.Nonce, r.Enc, r.Ciphertext))
}

// verifies counts Verify calls, for the tests that hold every refusal to one.
var verifies atomic.Uint64

// Verify runs one Ed25519 verify — under key when known, else under DummyKey —
// and accepts only a known key's valid signature. Every check of a signature
// in this protocol goes through it.
func Verify(key ed25519.PublicKey, known bool, msg, sig []byte) bool {
	verifies.Add(1)
	vk := DummyKey
	if known && len(key) == ed25519.PublicKeySize {
		vk = key
	}
	ok := len(sig) == ed25519.SignatureSize && ed25519.Verify(vk, msg, sig)
	return ok && known
}

// VerifyRequest checks a request's signature under its key, known says whether
// that key is registered with role watch. It is check 2 of PROTOCOL.md § 6.2
// and § 6.3.
func VerifyRequest(room wire.RoomID, r *wire.Request, known bool) bool {
	return Verify(r.Key, known, RequestMessage(room, r.TS, r.Nonce, r.Enc, r.Ciphertext), r.Sig)
}

// Keys is what a room has registered.
type Keys interface {
	// MacKey is the room's Mac key, nil while the room is unprovisioned.
	MacKey() ed25519.PublicKey
	// DeviceRole is the role a device key is registered with.
	DeviceRole(key ed25519.PublicKey) (wire.Role, bool)
}

// Admit is an accepted auth.
type Admit struct {
	Role wire.Role
	Key  ed25519.PublicKey
	// Provision: the room had no Mac key and must store Key as its Mac key.
	Provision bool
}

// CheckAuth judges the first frame of a socket against its challenge nonce
// (PROTOCOL.md § 4.2–4.3). Every refusal is ErrRefused after one verify.
func CheckAuth(provisionKey []byte, room wire.RoomID, challenge []byte, frame []byte, keys Keys) (Admit, error) {
	a, err := wire.DecodeAuth(frame)
	if err != nil {
		// Same cost as any refusal: one verify against the dummy key.
		Verify(nil, false, AuthMessage(room, wire.RoleMac, challenge), make([]byte, ed25519.SignatureSize))
		return Admit{}, ErrRefused
	}
	shapeOK := a.V == wire.AuthVersion && (a.Role == wire.RoleMac || a.Role == wire.RolePhone)
	msgRole := a.Role
	if !shapeOK {
		msgRole = wire.RoleMac // a role of any length must not reach the lp encoding
	}
	msg := AuthMessage(room, msgRole, challenge)
	var known, provision bool
	switch {
	case !shapeOK:
	case a.Role == wire.RoleMac:
		if mk := keys.MacKey(); mk != nil {
			known = subtle.ConstantTimeCompare(mk, a.Key) == 1
		} else {
			// An empty room: the presented key signs, the proof provisions.
			known, provision = true, true
		}
	default:
		role, ok := keys.DeviceRole(a.Key)
		known = ok && role == wire.RolePhone
	}
	sigOK := Verify(a.Key, known, msg, a.Sig)
	if !shapeOK || !sigOK {
		return Admit{}, ErrRefused
	}
	if provision && !CheckProvision(provisionKey, room, a.Key, a.Proof) {
		return Admit{}, ErrRefused
	}
	return Admit{Role: a.Role, Key: a.Key, Provision: provision}, nil
}

// NonceSet is a room's set of request nonces seen and not yet expired.
type NonceSet interface {
	Has(nonce [wire.NonceLen]byte, now time.Time) bool
	Add(nonce [wire.NonceLen]byte, expiry time.Time)
}

// Limiter is the per-key request bucket.
type Limiter interface {
	Allow(key ed25519.PublicKey, now time.Time) bool
}

// CheckRequest judges a watch's request body in the room's order
// (PROTOCOL.md § 6.2, checks 1–6): size, signature, time, nonce, then the
// bucket, then the nonce is stored. A request that fails before the signature
// verified touches neither nonces nor limit. The error is ErrRefused (404) or
// ErrRate (429).
func CheckRequest(room wire.RoomID, body []byte, keys Keys, nonces NonceSet, limit Limiter, now time.Time) (*wire.Request, error) {
	r, err := wire.ParseRequest(body)
	if err != nil {
		return nil, ErrRefused
	}
	role, ok := keys.DeviceRole(r.Key)
	if !VerifyRequest(room, r, ok && role == wire.RoleWatch) {
		return nil, ErrRefused
	}
	ts := r.Time()
	if d := now.Sub(ts); d > wire.RequestWindow || d < -wire.RequestWindow {
		return nil, ErrRefused
	}
	if nonces.Has(r.Nonce, now) {
		return nil, ErrRefused
	}
	if !limit.Allow(r.Key, now) {
		return nil, ErrRate
	}
	nonces.Add(r.Nonce, ts.Add(wire.NonceLife))
	return r, nil
}

// Bucket is a token bucket: Burst tokens, refilled continuously at Rate per
// second, one spent per Allow. The relay's buckets (PROTOCOL.md § 4.7, § 6.2)
// all have this shape.
type Bucket struct {
	Rate   float64
	Burst  float64
	tokens float64
	last   time.Time
	primed bool
}

// Allow spends a token if there is one.
func (b *Bucket) Allow(now time.Time) bool {
	if !b.primed {
		b.tokens, b.last, b.primed = b.Burst, now, true
	}
	if dt := now.Sub(b.last).Seconds(); dt > 0 {
		b.tokens = min(b.Burst, b.tokens+dt*b.Rate)
		b.last = now
	}
	if b.tokens < 1 {
		return false
	}
	b.tokens--
	return true
}
