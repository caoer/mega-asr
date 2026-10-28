package wire

import (
	"bytes"
	"crypto/ed25519"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
)

// Control frame types (the "t" field, PROTOCOL.md § 4).
const (
	TChallenge = "challenge"
	TAuth      = "auth"
	TOK        = "ok"
	TPeer      = "peer"
	TPeerAck   = "peer_ack"
	TDevices   = "devices"
	TDrop      = "drop"
	TReset     = "reset"
)

// AuthVersion is the only auth "v" a room accepts.
const AuthVersion = 1

// Challenge is the room's first frame on every socket.
type Challenge struct{ Nonce [ChalLen]byte }

// Auth is a client's answer to the challenge.
type Auth struct {
	V     int
	Role  Role
	Key   ed25519.PublicKey
	Sig   []byte
	Proof []byte // the provision proof; nil without one
}

// Device is a registered device key.
type Device struct {
	Role Role
	Key  ed25519.PublicKey
}

// OKPhone answers a phone's auth.
type OKPhone struct {
	SID  SID
	Peer bool
}

// OKMac answers the Mac's auth with the phone sessions open at that moment.
type OKMac struct{ Peers []PeerInfo }

// PeerInfo is one entry of OKMac.
type PeerInfo struct {
	SID  SID
	Role Role
	Key  ed25519.PublicKey
}

// Peer tells the Mac a session came or went.
type Peer struct {
	SID     SID
	Role    Role
	Key     ed25519.PublicKey
	Up      bool
	OneShot bool
}

// PeerAck is the Mac's acknowledgement of a Peer with Up.
type PeerAck struct{ SID SID }

// DevicesChange is the Mac's devices frame.
type DevicesChange struct {
	Add    []Device
	Remove []ed25519.PublicKey
}

// DevicesList is the room's answer to DevicesChange: every registered device,
// sorted by key text.
type DevicesList struct{ Keys []Device }

// Drop ends one session.
type Drop struct{ SID SID }

// Reset empties the room.
type Reset struct{}

type jsonDevice struct {
	Role Role   `json:"role"`
	Key  string `json:"key"`
}

type jsonPeerInfo struct {
	SID  string `json:"sid"`
	Role Role   `json:"role"`
	Key  string `json:"key"`
}

// Encode marshals a control frame as compact JSON with its fields in a fixed
// order. It panics on a type that is not a control frame.
func Encode(f any) []byte {
	var v any
	switch f := f.(type) {
	case Challenge:
		v = struct {
			T     string `json:"t"`
			Nonce string `json:"nonce"`
		}{TChallenge, B64(f.Nonce[:])}
	case Auth:
		a := struct {
			T         string `json:"t"`
			V         int    `json:"v"`
			Role      Role   `json:"role"`
			Key       string `json:"key"`
			Sig       string `json:"sig"`
			Provision *struct {
				Proof string `json:"proof"`
			} `json:"provision,omitempty"`
		}{T: TAuth, V: f.V, Role: f.Role, Key: B64(f.Key), Sig: B64(f.Sig)}
		if f.Proof != nil {
			a.Provision = &struct {
				Proof string `json:"proof"`
			}{B64(f.Proof)}
		}
		v = a
	case OKPhone:
		v = struct {
			T    string `json:"t"`
			SID  string `json:"sid"`
			Peer bool   `json:"peer"`
		}{TOK, f.SID.String(), f.Peer}
	case OKMac:
		peers := make([]jsonPeerInfo, 0, len(f.Peers))
		for _, p := range f.Peers {
			peers = append(peers, jsonPeerInfo{p.SID.String(), p.Role, B64(p.Key)})
		}
		v = struct {
			T     string         `json:"t"`
			Peers []jsonPeerInfo `json:"peers"`
		}{TOK, peers}
	case Peer:
		v = struct {
			T       string `json:"t"`
			SID     string `json:"sid"`
			Role    Role   `json:"role"`
			Key     string `json:"key"`
			Up      bool   `json:"up"`
			OneShot bool   `json:"oneshot"`
		}{TPeer, f.SID.String(), f.Role, B64(f.Key), f.Up, f.OneShot}
	case PeerAck:
		v = struct {
			T   string `json:"t"`
			SID string `json:"sid"`
		}{TPeerAck, f.SID.String()}
	case DevicesChange:
		add := make([]jsonDevice, 0, len(f.Add))
		for _, d := range f.Add {
			add = append(add, jsonDevice{d.Role, B64(d.Key)})
		}
		remove := make([]string, 0, len(f.Remove))
		for _, k := range f.Remove {
			remove = append(remove, B64(k))
		}
		v = struct {
			T      string       `json:"t"`
			Add    []jsonDevice `json:"add"`
			Remove []string     `json:"remove"`
		}{TDevices, add, remove}
	case DevicesList:
		keys := make([]jsonDevice, 0, len(f.Keys))
		for _, d := range f.Keys {
			keys = append(keys, jsonDevice{d.Role, B64(d.Key)})
		}
		sort.Slice(keys, func(i, j int) bool { return keys[i].Key < keys[j].Key })
		v = struct {
			T    string       `json:"t"`
			Keys []jsonDevice `json:"keys"`
		}{TDevices, keys}
	case Drop:
		v = struct {
			T   string `json:"t"`
			SID string `json:"sid"`
		}{TDrop, f.SID.String()}
	case Reset:
		v = struct {
			T string `json:"t"`
		}{TReset}
	default:
		panic(fmt.Sprintf("relay: %T is not a control frame", f))
	}
	b, err := json.Marshal(v)
	if err != nil {
		panic(err)
	}
	return b
}

// ErrMalformed is a text frame the protocol does not allow.
var ErrMalformed = errors.New("relay: malformed control frame")

// fields is a decoded JSON object whose getters refuse an absent field or one
// of the wrong type.
type fields map[string]json.RawMessage

func object(data []byte) (fields, string, error) {
	var f fields
	d := json.NewDecoder(bytes.NewReader(data))
	if err := d.Decode(&f); err != nil || f == nil || len(bytes.TrimSpace(data[d.InputOffset():])) != 0 {
		return nil, "", ErrMalformed
	}
	var t string
	if err := f.str("t", &t); err != nil {
		return nil, "", err
	}
	return f, t, nil
}

func (f fields) str(name string, out *string) error {
	raw, ok := f[name]
	if !ok || json.Unmarshal(raw, out) != nil || bytes.HasPrefix(bytes.TrimSpace(raw), []byte("null")) {
		return fmt.Errorf("%w: %s", ErrMalformed, name)
	}
	return nil
}

func (f fields) boolean(name string, out *bool) error {
	raw, ok := f[name]
	if !ok || json.Unmarshal(raw, out) != nil || bytes.HasPrefix(bytes.TrimSpace(raw), []byte("null")) {
		return fmt.Errorf("%w: %s", ErrMalformed, name)
	}
	return nil
}

func (f fields) bytesOf(name string, n int) ([]byte, error) {
	var s string
	if err := f.str(name, &s); err != nil {
		return nil, err
	}
	b, err := Unb64(s, n)
	if err != nil {
		return nil, fmt.Errorf("%w: %s", ErrMalformed, name)
	}
	return b, nil
}

func (f fields) sid(name string) (SID, error) {
	var s SID
	b, err := f.bytesOf(name, SIDLen)
	if err != nil {
		return s, err
	}
	copy(s[:], b)
	return s, nil
}

func (f fields) role(name string) (Role, error) {
	var s string
	if err := f.str(name, &s); err != nil {
		return "", err
	}
	return Role(s), nil
}

func (f fields) list(name string) ([]json.RawMessage, error) {
	raw, ok := f[name]
	var out []json.RawMessage
	if !ok || json.Unmarshal(raw, &out) != nil || out == nil {
		return nil, fmt.Errorf("%w: %s", ErrMalformed, name)
	}
	return out, nil
}

func device(raw json.RawMessage) (Device, error) {
	f, err := sub(raw)
	if err != nil {
		return Device{}, err
	}
	r, err := f.role("role")
	if err != nil {
		return Device{}, err
	}
	k, err := f.bytesOf("key", ed25519.PublicKeySize)
	if err != nil {
		return Device{}, err
	}
	return Device{r, k}, nil
}

func sub(raw json.RawMessage) (fields, error) {
	var f fields
	if json.Unmarshal(raw, &f) != nil || f == nil {
		return nil, ErrMalformed
	}
	return f, nil
}

// DecodeAuth reads the frame a client answers the challenge with. It checks
// shape only: version, role and keys are the room's to judge.
func DecodeAuth(data []byte) (Auth, error) {
	var a Auth
	if len(data) > MaxAuthFrame {
		return a, ErrMalformed
	}
	f, t, err := object(data)
	if err != nil {
		return a, err
	}
	if t != TAuth {
		return a, ErrMalformed
	}
	var v float64
	raw, ok := f["v"]
	if !ok || json.Unmarshal(raw, &v) != nil || v != float64(int(v)) {
		return a, ErrMalformed
	}
	a.V = int(v)
	if a.Role, err = f.role("role"); err != nil {
		return a, err
	}
	if a.Key, err = f.bytesOf("key", ed25519.PublicKeySize); err != nil {
		return a, err
	}
	if a.Sig, err = f.bytesOf("sig", ed25519.SignatureSize); err != nil {
		return a, err
	}
	if raw, ok := f["provision"]; ok {
		p, err := sub(raw)
		if err != nil {
			return a, err
		}
		if a.Proof, err = p.bytesOf("proof", 32); err != nil {
			return a, err
		}
	}
	return a, nil
}

// DecodeFromRoom reads a text frame the room sends: Challenge, OKPhone, OKMac,
// Peer or DevicesList.
func DecodeFromRoom(data []byte) (any, error) {
	f, t, err := object(data)
	if err != nil {
		return nil, err
	}
	switch t {
	case TChallenge:
		var c Challenge
		b, err := f.bytesOf("nonce", ChalLen)
		if err != nil {
			return nil, err
		}
		copy(c.Nonce[:], b)
		return c, nil
	case TOK:
		if _, ok := f["peers"]; ok {
			list, err := f.list("peers")
			if err != nil {
				return nil, err
			}
			m := OKMac{Peers: []PeerInfo{}}
			for _, raw := range list {
				pf, err := sub(raw)
				if err != nil {
					return nil, err
				}
				var p PeerInfo
				if p.SID, err = pf.sid("sid"); err != nil {
					return nil, err
				}
				if p.Role, err = pf.role("role"); err != nil {
					return nil, err
				}
				if p.Key, err = pf.bytesOf("key", ed25519.PublicKeySize); err != nil {
					return nil, err
				}
				m.Peers = append(m.Peers, p)
			}
			return m, nil
		}
		var o OKPhone
		if o.SID, err = f.sid("sid"); err != nil {
			return nil, err
		}
		if err = f.boolean("peer", &o.Peer); err != nil {
			return nil, err
		}
		return o, nil
	case TPeer:
		var p Peer
		if p.SID, err = f.sid("sid"); err != nil {
			return nil, err
		}
		if p.Role, err = f.role("role"); err != nil {
			return nil, err
		}
		if p.Key, err = f.bytesOf("key", ed25519.PublicKeySize); err != nil {
			return nil, err
		}
		if err = f.boolean("up", &p.Up); err != nil {
			return nil, err
		}
		if err = f.boolean("oneshot", &p.OneShot); err != nil {
			return nil, err
		}
		return p, nil
	case TDevices:
		list, err := f.list("keys")
		if err != nil {
			return nil, err
		}
		l := DevicesList{Keys: []Device{}}
		for _, raw := range list {
			d, err := device(raw)
			if err != nil {
				return nil, err
			}
			l.Keys = append(l.Keys, d)
		}
		return l, nil
	}
	return nil, ErrMalformed
}

// DecodeFromMac reads a text frame the Mac sends after ok: PeerAck,
// DevicesChange, Drop or Reset. A devices frame whose roles are not phone or
// watch is malformed.
func DecodeFromMac(data []byte) (any, error) {
	if len(data) > MaxFrame {
		return nil, ErrMalformed
	}
	f, t, err := object(data)
	if err != nil {
		return nil, err
	}
	switch t {
	case TPeerAck:
		s, err := f.sid("sid")
		if err != nil {
			return nil, err
		}
		return PeerAck{s}, nil
	case TDrop:
		s, err := f.sid("sid")
		if err != nil {
			return nil, err
		}
		return Drop{s}, nil
	case TReset:
		return Reset{}, nil
	case TDevices:
		add, err := f.list("add")
		if err != nil {
			return nil, err
		}
		remove, err := f.list("remove")
		if err != nil {
			return nil, err
		}
		c := DevicesChange{Add: []Device{}, Remove: []ed25519.PublicKey{}}
		for _, raw := range add {
			d, err := device(raw)
			if err != nil {
				return nil, err
			}
			if d.Role != RolePhone && d.Role != RoleWatch {
				return nil, ErrMalformed
			}
			c.Add = append(c.Add, d)
		}
		for _, raw := range remove {
			var s string
			if json.Unmarshal(raw, &s) != nil {
				return nil, ErrMalformed
			}
			k, err := Unb64(s, ed25519.PublicKeySize)
			if err != nil {
				return nil, ErrMalformed
			}
			c.Remove = append(c.Remove, k)
		}
		return c, nil
	}
	return nil, ErrMalformed
}
