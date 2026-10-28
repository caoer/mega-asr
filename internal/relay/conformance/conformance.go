// Package conformance loads the relay's conformance scripts: exchanges with a
// relay from outside, written once, run by the Worker's tests and by the Mac's
// in-process fake room so the two cannot drift. The format is relay/PROTOCOL.md
// § 11; this package parses and checks it, expands repeats, and builds the
// frames and bodies a runner sends. Running a script is the runner's.
package conformance

import (
	"bytes"
	"crypto/ed25519"
	"crypto/sha256"
	"embed"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"path"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/caoer/mega-asr/internal/relay/auth"
	"github.com/caoer/mega-asr/internal/relay/wire"
)

//go:embed *.json
var files embed.FS

// Env is what every script shares (env.json).
type Env struct {
	About        string            `json:"about"`
	RoomTagKey   string            `json:"room_tag_key"`
	ProvisionKey string            `json:"provision_key"`
	TestRNGSeed  string            `json:"test_rng_seed"`
	ClockStartMS int64             `json:"clock_start_ms"`
	Keys         map[string]string `json:"keys"`
	Rooms        map[string]string `json:"rooms"`
}

// Script is one conformance script.
type Script struct {
	Name  string `json:"-"`
	About string `json:"about"`
	Layer string `json:"layer"`
	Setup *Setup `json:"setup,omitempty"`
	Steps []Step `json:"steps"`
}

// Setup provisions a room and registers devices before the steps.
type Setup struct {
	Mac     string   `json:"mac"`
	Room    string   `json:"room"`
	Key     string   `json:"key"`
	Devices []Device `json:"devices"`
}

// Device is a key to register.
type Device struct {
	Role string `json:"role"`
	Key  string `json:"key"`
}

// Step is one action; exactly one field is set.
type Step struct {
	Connect   *Connect  `json:"connect,omitempty"`
	Send      *Send     `json:"send,omitempty"`
	Ping      *Conn     `json:"ping,omitempty"`
	Close     *Close    `json:"close,omitempty"`
	Post      *Post     `json:"post,omitempty"`
	HTTP      *HTTP     `json:"http,omitempty"`
	Expect    *Expect   `json:"expect,omitempty"`
	AdvanceMS *int64    `json:"advance_ms,omitempty"`
	Evict     *struct{} `json:"evict,omitempty"`
	Repeat    *Repeat   `json:"repeat,omitempty"`
}

type Conn struct {
	As string `json:"as"`
}

type Connect struct {
	As     string          `json:"as"`
	Room   string          `json:"room,omitempty"`
	Header json.RawMessage `json:"header,omitempty"` // absent: the room's; null: none; a string: that value
}

type Send struct {
	As     string          `json:"as"`
	Text   json.RawMessage `json:"text,omitempty"`
	Raw    *string         `json:"raw,omitempty"`
	Auth   *AuthSpec       `json:"auth,omitempty"`
	Binary *Binary         `json:"binary,omitempty"`
}

type AuthSpec struct {
	Role      string `json:"role"`
	Key       string `json:"key"`
	Signer    string `json:"signer,omitempty"`
	Room      string `json:"room,omitempty"`
	Nonce     string `json:"nonce,omitempty"`
	Provision string `json:"provision,omitempty"`
	V         *int   `json:"v,omitempty"`
	Pad       int    `json:"pad,omitempty"`
}

type Binary struct {
	SID    string `json:"sid,omitempty"`
	Hex    string `json:"hex,omitempty"`
	Len    *int   `json:"len,omitempty"`
	BodyOf string `json:"body_of,omitempty"`
}

type Close struct {
	As   string `json:"as"`
	Code int    `json:"code"`
}

type Post struct {
	As      string          `json:"as"`
	Room    string          `json:"room,omitempty"`
	Header  json.RawMessage `json:"header,omitempty"`
	Request *RequestSpec    `json:"request,omitempty"`
	BodyLen *int            `json:"body_len,omitempty"`
}

type RequestSpec struct {
	Key    string `json:"key"`
	Signer string `json:"signer,omitempty"`
	TS     int64  `json:"ts,omitempty"`
	Nonce  string `json:"nonce"`
	CtLen  *int   `json:"ct_len,omitempty"`
}

type HTTP struct {
	As      string          `json:"as"`
	Method  string          `json:"method"`
	Path    string          `json:"path"`
	Room    string          `json:"room,omitempty"`
	Header  json.RawMessage `json:"header,omitempty"`
	Upgrade bool            `json:"upgrade,omitempty"`
}

type Expect struct {
	As      string          `json:"as"`
	Frame   json.RawMessage `json:"frame,omitempty"`
	Raw     *string         `json:"raw,omitempty"`
	Binary  *Binary         `json:"binary,omitempty"`
	Close   *int            `json:"close,omitempty"`
	Status  *int            `json:"status,omitempty"`
	Hex     *string         `json:"hex,omitempty"`
	Nothing bool            `json:"nothing,omitempty"`
}

type Repeat struct {
	Times int    `json:"times"`
	Steps []Step `json:"steps"`
}

func strict(data []byte, v any) error {
	d := json.NewDecoder(bytes.NewReader(data))
	d.DisallowUnknownFields()
	if err := d.Decode(v); err != nil {
		return err
	}
	if d.More() {
		return fmt.Errorf("data after the object")
	}
	return nil
}

// LoadEnv reads env.json.
func LoadEnv() (*Env, error) {
	b, err := files.ReadFile("env.json")
	if err != nil {
		return nil, err
	}
	var e Env
	if err := strict(b, &e); err != nil {
		return nil, fmt.Errorf("env.json: %w", err)
	}
	return &e, nil
}

// Load reads every script, sorted by name, with repeats as written.
func Load() ([]*Script, error) {
	names, err := files.ReadDir(".")
	if err != nil {
		return nil, err
	}
	var out []*Script
	for _, n := range names {
		if n.Name() == "env.json" {
			continue
		}
		b, err := files.ReadFile(n.Name())
		if err != nil {
			return nil, err
		}
		var s Script
		if err := strict(b, &s); err != nil {
			return nil, fmt.Errorf("%s: %w", n.Name(), err)
		}
		s.Name = strings.TrimSuffix(n.Name(), path.Ext(n.Name()))
		out = append(out, &s)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}

var iter = regexp.MustCompile(`\{i(\+1)?\}`)

// Expand returns the steps with every repeat unrolled and {i}, {i+1}
// replaced.
func Expand(steps []Step) ([]Step, error) {
	var out []Step
	for _, st := range steps {
		if st.Repeat == nil {
			out = append(out, st)
			continue
		}
		raw, err := json.Marshal(st.Repeat.Steps)
		if err != nil {
			return nil, err
		}
		for i := 0; i < st.Repeat.Times; i++ {
			text := iter.ReplaceAllStringFunc(string(raw), func(m string) string {
				if m == "{i+1}" {
					return strconv.Itoa(i + 1)
				}
				return strconv.Itoa(i)
			})
			var inner []Step
			if err := strict([]byte(text), &inner); err != nil {
				return nil, err
			}
			flat, err := Expand(inner)
			if err != nil {
				return nil, err
			}
			out = append(out, flat...)
		}
	}
	return out, nil
}

func secret(s string) ([]byte, error) {
	k := auth.ParseSecret(s)
	if k == nil {
		return nil, fmt.Errorf("not 64 lowercase hex characters")
	}
	return k, nil
}

// RoomTagKey32 and ProvisionKey32 are the env's secrets as bytes.
func (e *Env) RoomTagKey32() []byte   { k, _ := secret(e.RoomTagKey); return k }
func (e *Env) ProvisionKey32() []byte { k, _ := secret(e.ProvisionKey); return k }

// Key is a named key.
func (e *Env) Key(name string) (ed25519.PrivateKey, error) {
	s, ok := e.Keys[name]
	if !ok {
		return nil, fmt.Errorf("no key %q", name)
	}
	b, err := hex.DecodeString(s)
	if err != nil || len(b) != ed25519.SeedSize {
		return nil, fmt.Errorf("key %q is not a 32-byte hex seed", name)
	}
	return ed25519.NewKeyFromSeed(b), nil
}

// Room is a named room.
func (e *Env) Room(name string) (wire.RoomID, error) {
	s, ok := e.Rooms[name]
	if !ok {
		return wire.RoomID{}, fmt.Errorf("no room %q", name)
	}
	return wire.ParseRoom(s)
}

var keyRef = regexp.MustCompile(`@([a-z0-9]+)`)

// Resolve replaces @key with that key's base64url public key and a whole
// "$name" with its bound value.
func (e *Env) Resolve(s string, vars map[string]string) (string, error) {
	if strings.HasPrefix(s, "$") {
		v, ok := vars[s[1:]]
		if !ok {
			return "", fmt.Errorf("%s is not bound", s)
		}
		return v, nil
	}
	var err error
	out := keyRef.ReplaceAllStringFunc(s, func(m string) string {
		k, kerr := e.Key(m[1:])
		if kerr != nil {
			err = kerr
			return m
		}
		return wire.B64(k.Public().(ed25519.PublicKey))
	})
	return out, err
}

// NonceOf is a request nonce label's bytes: SHA-256(label)[0:16].
func NonceOf(label string) [wire.NonceLen]byte {
	h := sha256.Sum256([]byte(label))
	var n [wire.NonceLen]byte
	copy(n[:], h[:])
	return n
}

// AuthFrame builds the frame of a send auth step on a connection to
// connRoom whose last challenge was challenge (a "$name" nonce is resolved by
// the caller and passed as challenge).
func (e *Env) AuthFrame(a AuthSpec, connRoom string, challenge []byte) ([]byte, error) {
	k, err := e.Key(a.Key)
	if err != nil {
		return nil, err
	}
	signer := k
	if a.Signer != "" {
		if signer, err = e.Key(a.Signer); err != nil {
			return nil, err
		}
	}
	roomName := connRoom
	if a.Room != "" {
		roomName = a.Room
	}
	room, err := e.Room(roomName)
	if err != nil {
		return nil, err
	}
	f := wire.Auth{V: wire.AuthVersion, Role: wire.Role(a.Role), Key: k.Public().(ed25519.PublicKey)}
	if a.V != nil {
		f.V = *a.V
	}
	f.Sig = ed25519.Sign(signer, auth.AuthMessage(room, f.Role, challenge))
	switch a.Provision {
	case "":
	case "valid":
		f.Proof = auth.ProvisionProof(e.ProvisionKey32(), room, f.Key)
	case "bad":
		f.Proof = auth.ProvisionProof(make([]byte, auth.SecretLen), room, f.Key)
	default:
		return nil, fmt.Errorf("provision %q", a.Provision)
	}
	out := wire.Encode(f)
	return append(out, bytes.Repeat([]byte(" "), a.Pad)...), nil
}

// RequestBody builds the body of a post step to room at the room's clock now.
func (e *Env) RequestBody(p Post, now time.Time) ([]byte, error) {
	if p.BodyLen != nil {
		return make([]byte, *p.BodyLen), nil
	}
	r := p.Request
	if r == nil {
		return nil, fmt.Errorf("post %s has neither request nor body_len", p.As)
	}
	k, err := e.Key(r.Key)
	if err != nil {
		return nil, err
	}
	signer := k
	if r.Signer != "" {
		if signer, err = e.Key(r.Signer); err != nil {
			return nil, err
		}
	}
	room, err := e.Room(p.Room)
	if err != nil {
		return nil, err
	}
	ct := 128
	if r.CtLen != nil {
		ct = *r.CtLen
	}
	req := &wire.Request{
		TS:         uint64(now.UnixMilli() + r.TS),
		Nonce:      NonceOf(r.Nonce),
		Enc:        make([]byte, 32),
		Ciphertext: make([]byte, ct),
	}
	auth.SignRequest(signer, room, req)
	req.Key = k.Public().(ed25519.PublicKey)
	return req.Bytes(), nil
}

// BinaryBytes is a send or expect binary's payload after its sid.
func BinaryBytes(b Binary, bodies map[string][]byte) ([]byte, error) {
	switch {
	case b.BodyOf != "":
		body, ok := bodies[b.BodyOf]
		if !ok {
			return nil, fmt.Errorf("no post %q", b.BodyOf)
		}
		return body, nil
	case b.Len != nil:
		return make([]byte, *b.Len), nil
	}
	return hex.DecodeString(b.Hex)
}
