package micserver

import (
	"crypto/hkdf"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/binary"
	"errors"

	"filippo.io/cpace"
)

// The PAKE behind pairing, the one place that names the library: CPace over
// ristretto255 (filippo.io/cpace), with explicit key confirmation added
// here, since that package derives a key and confirms nothing. Both roles
// run it with the same ci; a different PIN or ci gives different keys, and
// only the confirmation tells.
//
// initiator (the Mac): startPAKE → msgA; finish(msgB) → the confirmation it
// sends and the session key. responder (the box): respondPAKE(msgA) →
// msgB; finish(confirmation) → the session key, or errConfirm.

var errConfirm = errors.New("pake: confirmation does not match")

type initiator struct {
	st   *cpace.State
	msgA []byte
}

type responder struct {
	key        []byte
	msgA, msgB []byte
}

// ciFor is the CPace context: the Mac's client name, the server's role, and
// ad (the certificate the side sees, see the package doc).
func ciFor(name string, ad []byte) *cpace.ContextInfo {
	return cpace.NewContextInfo(name, "megavoice mic server", ad)
}

func startPAKE(pin, name string, ad []byte) ([]byte, *initiator, error) {
	msgA, st, err := cpace.Start(pin, ciFor(name, ad))
	if err != nil {
		return nil, nil, err
	}
	return msgA, &initiator{st: st, msgA: msgA}, nil
}

func (i *initiator) finish(msgB []byte) (confirm, key []byte, err error) {
	k, err := i.st.Finish(msgB)
	if err != nil {
		return nil, nil, err
	}
	return confirmTag(k, "mac", i.msgA, msgB), k, nil
}

func respondPAKE(pin, name string, ad, msgA []byte) ([]byte, *responder, error) {
	msgB, key, err := cpace.Exchange(pin, ciFor(name, ad), msgA)
	if err != nil {
		return nil, nil, err
	}
	return msgB, &responder{key: key, msgA: msgA, msgB: msgB}, nil
}

func (r *responder) finish(confirm []byte) ([]byte, error) {
	if !hmac.Equal(confirm, confirmTag(r.key, "mac", r.msgA, r.msgB)) {
		return nil, errConfirm
	}
	return r.key, nil
}

// confirmTag is role's key confirmation: an HMAC under a key derived from
// the session key for that purpose alone, over both PAKE messages.
func confirmTag(key []byte, role string, msgA, msgB []byte) []byte {
	return macOf(derive(key, "confirm "+role), msgA, msgB)
}

// macOf is HMAC-SHA256 under key over parts, each prefixed by its length so
// no two part lists share an input.
func macOf(key []byte, parts ...[]byte) []byte {
	h := hmac.New(sha256.New, key)
	for _, p := range parts {
		h.Write(binary.BigEndian.AppendUint32(nil, uint32(len(p))))
		h.Write(p)
	}
	return h.Sum(nil)
}

// derive is a 32-byte key for purpose, from the session key.
func derive(key []byte, purpose string) []byte {
	k, err := hkdf.Expand(sha256.New, key, "megavoice-pair-v1 "+purpose, 32)
	if err != nil {
		panic(err) // 32 bytes is far inside HKDF-SHA256's limit
	}
	return k
}
