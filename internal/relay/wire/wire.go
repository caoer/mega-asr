// Package wire holds the relay protocol's names, limits and byte layouts:
// room ids, the control frames of a relay WebSocket, the frame prefix that
// names a session, and the body of a watch's request. It does no
// cryptography; internal/relay/auth and internal/relay/e2e do. The normative
// description is relay/PROTOCOL.md in the canonical repository.
package wire

import (
	"crypto/ed25519"
	"encoding/base64"
	"encoding/binary"
	"errors"
	"fmt"
	"time"
)

// Role is a device's role in a room.
type Role string

const (
	RoleMac   Role = "mac"
	RolePhone Role = "phone"
	RoleWatch Role = "watch"
)

// Valid reports whether r is one of the three roles.
func (r Role) Valid() bool { return r == RoleMac || r == RolePhone || r == RoleWatch }

// The HTTP surface (PROTOCOL.md § 3).
const (
	Header   = "X-MegaVoice-Room"
	PathWS   = "/v1/ws"
	PathCmd  = "/v1/cmd"
	PingText = "ping"
	PongText = "pong"
)

// Limits and timings (PROTOCOL.md § 9).
const (
	MaxFrame     = 16384 // a frame after auth; a request body
	MaxToMac     = SIDLen + MaxFrame
	MaxAuthFrame = 1024
	MaxRecord    = MaxFrame - SIDLen // a sealed record, tag included
	MaxDevices   = 64
	MaxPhones    = 4
	MaxUnauthed  = 8
	MaxPending   = 8
	MaxHandshake = 16 // relay sessions the Mac lets await a handshake

	AuthWindow    = 5 * time.Second
	PeerAckWait   = 1500 * time.Millisecond
	ReplyWait     = 5 * time.Second
	PingEvery     = 30 * time.Second
	PongWait      = 10 * time.Second
	IdleClose     = 90 * time.Second
	AlarmEvery    = 60 * time.Second
	RequestWindow = 60 * time.Second
	NonceLife     = 120 * time.Second
	RoomExpiry    = 180 * 24 * time.Hour
)

// Close codes a relay WebSocket ends with (PROTOCOL.md § 4.6).
const (
	CloseNormal      = 1000
	CloseUnsupported = 1003
	CloseRefused     = 1008
	CloseTooBig      = 1009
	CloseMacGone     = 1012
	CloseFull        = 1013
	CloseSuperseded  = 4001
	CloseRate        = 4029
)

// RoomIDLen is a room id's size: 16 random bytes and an 8-byte tag.
const (
	RoomIDLen   = 24
	RoomRandLen = 16
	RoomTagLen  = 8
	RoomTextLen = 32
	SIDLen      = 8
	NonceLen    = 16 // a request nonce
	ChalLen     = 32 // a challenge nonce
)

// RoomID is a room id's raw bytes.
type RoomID [RoomIDLen]byte

// ParseRoom reads a room id's text form: exactly 32 base64url characters.
func ParseRoom(s string) (RoomID, error) {
	var id RoomID
	if len(s) != RoomTextLen {
		return id, errors.New("relay: room id is not 32 characters")
	}
	b, err := Unb64(s, RoomIDLen)
	if err != nil {
		return id, err
	}
	copy(id[:], b)
	return id, nil
}

// String is the room id's text form.
func (id RoomID) String() string { return B64(id[:]) }

// Rand is the room id's random part.
func (id RoomID) Rand() []byte { return id[:RoomRandLen] }

// Tag is the room id's tag.
func (id RoomID) Tag() []byte { return id[RoomRandLen:] }

// SID names a session inside a room.
type SID [SIDLen]byte

func (s SID) String() string { return B64(s[:]) }

var b64 = base64.RawURLEncoding.Strict()

// B64 is base64url without padding.
func B64(b []byte) string { return b64.EncodeToString(b) }

// Unb64 decodes base64url without padding, refusing padding, foreign
// characters and non-canonical trailing bits, and refuses a result that is not
// n bytes long (n < 0 accepts any length).
func Unb64(s string, n int) ([]byte, error) {
	b, err := b64.DecodeString(s)
	if err != nil {
		return nil, fmt.Errorf("relay: bad base64url: %w", err)
	}
	if n >= 0 && len(b) != n {
		return nil, fmt.Errorf("relay: %d bytes, want %d", len(b), n)
	}
	return b, nil
}

// AppendLP appends one length byte and x. x is at most 255 bytes; every caller
// passes a role, a binding or a label.
func AppendLP(dst, x []byte) []byte {
	if len(x) > 255 {
		panic("relay: lp field over 255 bytes")
	}
	dst = append(dst, byte(len(x)))
	return append(dst, x...)
}

// JoinSID prefixes a frame with its session id: the frames between the Mac and
// its room.
func JoinSID(sid SID, data []byte) []byte {
	out := make([]byte, 0, SIDLen+len(data))
	out = append(out, sid[:]...)
	return append(out, data...)
}

// SplitSID reads a frame between the Mac and its room.
func SplitSID(frame []byte) (SID, []byte, error) {
	var sid SID
	if len(frame) < SIDLen {
		return sid, nil, errors.New("relay: frame shorter than its session id")
	}
	copy(sid[:], frame)
	return sid, frame[SIDLen:], nil
}

// Request is a watch's request body (PROTOCOL.md § 6.1).
type Request struct {
	Key        ed25519.PublicKey
	TS         uint64 // unix ms
	Nonce      [NonceLen]byte
	Sig        []byte
	Enc        []byte
	Ciphertext []byte
}

// RequestHeader is the fixed part of a request body; MinRequest adds the
// smallest handshake ciphertext (an empty first message: 97 bytes and a tag).
const (
	RequestHeader = ed25519.PublicKeySize + 8 + NonceLen + ed25519.SignatureSize + 32
	MinHandshake  = 32 + 64 + 1 + 16
	MinRequest    = RequestHeader + MinHandshake
)

// ParseRequest reads a request body of MinRequest to MaxFrame bytes.
func ParseRequest(body []byte) (*Request, error) {
	if len(body) < MinRequest || len(body) > MaxFrame {
		return nil, fmt.Errorf("relay: request of %d bytes", len(body))
	}
	r := &Request{}
	p := body
	r.Key = ed25519.PublicKey(clone(p[:32]))
	p = p[32:]
	r.TS = binary.BigEndian.Uint64(p[:8])
	p = p[8:]
	copy(r.Nonce[:], p[:NonceLen])
	p = p[NonceLen:]
	r.Sig = clone(p[:64])
	p = p[64:]
	r.Enc = clone(p[:32])
	r.Ciphertext = clone(p[32:])
	return r, nil
}

// Bytes is the request body.
func (r *Request) Bytes() []byte {
	out := make([]byte, 0, RequestHeader+len(r.Ciphertext))
	out = append(out, r.Key...)
	out = binary.BigEndian.AppendUint64(out, r.TS)
	out = append(out, r.Nonce[:]...)
	out = append(out, r.Sig...)
	out = append(out, r.Enc...)
	return append(out, r.Ciphertext...)
}

// Message is the handshake message inside the request: enc ‖ ciphertext.
func (r *Request) Message() []byte {
	out := make([]byte, 0, len(r.Enc)+len(r.Ciphertext))
	out = append(out, r.Enc...)
	return append(out, r.Ciphertext...)
}

// Time is the request's ts.
func (r *Request) Time() time.Time { return time.UnixMilli(int64(r.TS)) }

func clone(b []byte) []byte { return append([]byte(nil), b...) }
