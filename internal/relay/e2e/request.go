package e2e

import (
	"crypto/ecdh"
	"crypto/ed25519"
	"crypto/subtle"
	"errors"
	"sync"
	"time"

	"github.com/caoer/mega-asr/internal/relay/auth"
	"github.com/caoer/mega-asr/internal/relay/wire"
)

var (
	ErrRequest     = errors.New("relay e2e: request refused")
	ErrStale       = errors.New("relay e2e: request outside the time window")
	ErrBeforeStart = errors.New("relay e2e: request older than this process")
	ErrReplay      = errors.New("relay e2e: request nonce already seen")
	ErrKeyMismatch = errors.New("relay e2e: handshake key is not the request's key")
)

// SealRequest makes a watch's request body (PROTOCOL.md § 6.1): the handshake
// for role watch bound to nonce, carrying the command, and the outer
// signature. ts is the watch's clock in unix ms.
func SealRequest(macKem *ecdh.PublicKey, room wire.RoomID, watch ed25519.PrivateKey, ts uint64, nonce [wire.NonceLen]byte, cmd []byte) ([]byte, *Pending, error) {
	msg, pd, err := Begin(macKem, Watch(room, nonce), watch, cmd)
	if err != nil {
		return nil, nil, err
	}
	r := &wire.Request{TS: ts, Nonce: nonce, Enc: msg[:encLen], Ciphertext: msg[encLen:]}
	auth.SignRequest(watch, room, r)
	return r.Bytes(), pd, nil
}

// OpenRequest is the Mac's judgement of a request the room forwarded, in the
// order of PROTOCOL.md § 6.3: the body parses; its signature verifies under a
// key isWatch says is a paired watch's (one verify against the dummy key
// otherwise); the guard admits its time and nonce; the handshake opens as role
// watch bound to the nonce; the handshake's key is the request's. A request
// refused before its signature verified leaves the guard untouched.
func OpenRequest(macKem *ecdh.PrivateKey, room wire.RoomID, body []byte, isWatch func(ed25519.PublicKey) bool, guard *ReplayGuard, now time.Time) (*Opened, *wire.Request, error) {
	r, err := wire.ParseRequest(body)
	if err != nil {
		return nil, nil, ErrRequest
	}
	if !auth.VerifyRequest(room, r, isWatch(r.Key)) {
		return nil, nil, ErrRequest
	}
	if err := guard.Check(r.Key, r.TS, r.Nonce, now); err != nil {
		return nil, nil, err
	}
	o, err := Open(macKem, Watch(room, r.Nonce), r.Message())
	if err != nil {
		return nil, nil, err
	}
	if subtle.ConstantTimeCompare(o.DeviceKey, r.Key) != 1 {
		return nil, nil, ErrKeyMismatch
	}
	return o, r, nil
}

// ReplayGuard is the Mac's own replay check for watch requests: a ±60 s window
// on the Mac's clock, no request older than this process's start, and a nonce
// set per watch key whose entries live until ts + 120 s. The set lives in
// memory only: after a restart the start bound refuses every request the
// previous process could have seen, up to the clock skew PROTOCOL.md § 6.3
// states.
type ReplayGuard struct {
	start int64 // unix ms
	mu    sync.Mutex
	seen  map[string]map[[wire.NonceLen]byte]int64
}

// NewReplayGuard makes the guard of a process that started at start.
func NewReplayGuard(start time.Time) *ReplayGuard {
	return &ReplayGuard{start: start.UnixMilli(), seen: map[string]map[[wire.NonceLen]byte]int64{}}
}

// Check admits a request with key, ts (unix ms) and nonce at now, and records
// its nonce. Call it only after the request's signature verified.
func (g *ReplayGuard) Check(key ed25519.PublicKey, ts uint64, nonce [wire.NonceLen]byte, now time.Time) error {
	if ts > 1<<62 {
		return ErrStale
	}
	t, n := int64(ts), now.UnixMilli()
	window := wire.RequestWindow.Milliseconds()
	if t < n-window || t > n+window {
		return ErrStale
	}
	if t < g.start {
		return ErrBeforeStart
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	set := g.seen[string(key)]
	if set == nil {
		set = map[[wire.NonceLen]byte]int64{}
		g.seen[string(key)] = set
	}
	for k, exp := range set {
		if exp <= n {
			delete(set, k)
		}
	}
	if _, ok := set[nonce]; ok {
		return ErrReplay
	}
	set[nonce] = t + wire.NonceLife.Milliseconds()
	return nil
}
