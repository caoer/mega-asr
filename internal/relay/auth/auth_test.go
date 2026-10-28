package auth

import (
	"bytes"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/caoer/mega-asr/internal/relay/wire"
)

func seed(name string) []byte { s := sha256.Sum256([]byte(name)); return s[:] }

func key(name string) ed25519.PrivateKey { return ed25519.NewKeyFromSeed(seed(name)) }

func pub(k ed25519.PrivateKey) ed25519.PublicKey { return k.Public().(ed25519.PublicKey) }

type keys struct {
	mac     ed25519.PublicKey
	devices map[string]wire.Role
}

func (k keys) MacKey() ed25519.PublicKey { return k.mac }
func (k keys) DeviceRole(pk ed25519.PublicKey) (wire.Role, bool) {
	r, ok := k.devices[string(pk)]
	return r, ok
}

func TestParseSecret(t *testing.T) {
	good := strings.Repeat("ab", 32)
	if k := ParseSecret(good + "\n"); len(k) != 32 || k[0] != 0xab {
		t.Fatalf("good secret: %x", k)
	}
	for _, s := range []string{"", good[:62], good + "ab", strings.ToUpper(good), strings.Repeat("zz", 32)} {
		if ParseSecret(s) != nil {
			t.Fatalf("%q parsed", s)
		}
	}
}

func TestRoomTag(t *testing.T) {
	tk := seed("tag key")
	id, err := MintRoom(tk, rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	if !CheckRoom(tk, id) {
		t.Fatal("minted room does not check")
	}
	parsed, err := wire.ParseRoom(id.String())
	if err != nil || parsed != id {
		t.Fatalf("text round trip: %v", err)
	}
	for i := range id {
		bad := id
		bad[i] ^= 1
		if CheckRoom(tk, bad) {
			t.Fatalf("byte %d flipped checks", i)
		}
	}
	for _, k := range [][]byte{nil, tk[:31], append(tk, 0)} {
		if CheckRoom(k, id) {
			t.Fatalf("key of %d bytes checks a tag", len(k))
		}
		if _, err := MintRoom(k, rand.Reader); err == nil {
			t.Fatalf("key of %d bytes mints", len(k))
		}
	}
}

func TestProvision(t *testing.T) {
	pk := seed("provision key")
	var room wire.RoomID
	copy(room[:], seed("room"))
	mac := pub(key("mac"))
	proof := ProvisionProof(pk, room, mac)
	if !CheckProvision(pk, room, mac, proof) {
		t.Fatal("proof does not check")
	}
	other := room
	other[0] ^= 1
	if CheckProvision(pk, other, mac, proof) || CheckProvision(pk, room, pub(key("x")), proof) || CheckProvision(seed("other"), room, mac, proof) {
		t.Fatal("a proof checks for another room, key or provision key")
	}
	for _, k := range [][]byte{nil, pk[:31]} {
		if CheckProvision(k, room, mac, ProvisionProof(k, room, mac)) {
			t.Fatalf("provision key of %d bytes provisions", len(k))
		}
	}
}

type authCase struct {
	name  string
	frame []byte
}

// TestCheckAuthRefusalsAreOne: every refused auth is the same ErrRefused, and
// a good one admits with the role.
func TestCheckAuthRefusalsAreOne(t *testing.T) {
	var room, other wire.RoomID
	copy(room[:], seed("room"))
	copy(other[:], seed("other room"))
	prov := seed("provision key")
	mac, phone, watch, stranger := key("mac"), key("phone"), key("watch"), key("stranger")
	nonce := seed("challenge")
	ks := keys{pub(mac), map[string]wire.Role{string(pub(phone)): wire.RolePhone, string(pub(watch)): wire.RoleWatch}}

	a, err := CheckAuth(prov, room, nonce, AuthFrame(phone, room, wire.RolePhone, nonce, nil), ks)
	if err != nil || a.Role != wire.RolePhone || !bytes.Equal(a.Key, pub(phone)) || a.Provision {
		t.Fatalf("phone: %+v %v", a, err)
	}
	a, err = CheckAuth(prov, room, nonce, AuthFrame(mac, room, wire.RoleMac, nonce, []byte("ignored provision proof here 32b")), ks)
	if err != nil || a.Role != wire.RoleMac || a.Provision {
		t.Fatalf("mac with an ignored provision: %+v %v", a, err)
	}

	forged := wire.Auth{V: 1, Role: wire.RolePhone, Key: pub(phone), Sig: ed25519.Sign(stranger, AuthMessage(room, wire.RolePhone, nonce))}
	v2 := wire.Auth{V: 2, Role: wire.RolePhone, Key: pub(phone), Sig: ed25519.Sign(phone, AuthMessage(room, wire.RolePhone, nonce))}
	cases := []authCase{
		{"unregistered key", AuthFrame(stranger, room, wire.RolePhone, nonce, nil)},
		{"phone key as mac", AuthFrame(phone, room, wire.RoleMac, nonce, nil)},
		{"mac key as phone", AuthFrame(mac, room, wire.RolePhone, nonce, nil)},
		{"watch key on a socket", AuthFrame(watch, room, wire.RoleWatch, nonce, nil)},
		{"watch key as phone", AuthFrame(watch, room, wire.RolePhone, nonce, nil)},
		{"signature over another room", AuthFrame(phone, other, wire.RolePhone, nonce, nil)},
		{"signature over another nonce", AuthFrame(phone, room, wire.RolePhone, seed("old"), nil)},
		{"bad signature", wire.Encode(forged)},
		{"version 2", wire.Encode(v2)},
		{"malformed", []byte(`{"t":"auth"`)},
		{"another frame", []byte(`{"t":"ping"}`)},
		{"role over 255 bytes", wire.Encode(wire.Auth{V: 1, Role: wire.Role(strings.Repeat("r", 300)), Key: pub(phone), Sig: make([]byte, 64)})},
		{"oversize", append(AuthFrame(phone, room, wire.RolePhone, nonce, nil), bytes.Repeat([]byte(" "), wire.MaxAuthFrame)...)},
	}
	for _, c := range cases {
		before := verifies.Load()
		if _, err := CheckAuth(prov, room, nonce, c.frame, ks); err != ErrRefused {
			t.Fatalf("%s: %v", c.name, err)
		}
		if n := verifies.Load() - before; n != 1 {
			t.Fatalf("%s: %d verifies, want the one every refusal costs", c.name, n)
		}
	}
	empty := keys{nil, map[string]wire.Role{}}
	if _, err := CheckAuth(prov, room, nonce, AuthFrame(phone, room, wire.RolePhone, nonce, nil), empty); err != ErrRefused {
		t.Fatalf("phone in an unprovisioned room: %v", err)
	}
}

func TestProvisioningAuth(t *testing.T) {
	var room wire.RoomID
	copy(room[:], seed("room"))
	prov := seed("provision key")
	mac, stranger := key("mac"), key("stranger")
	nonce := seed("challenge")
	empty := keys{nil, map[string]wire.Role{}}
	good := ProvisionProof(prov, room, pub(mac))
	a, err := CheckAuth(prov, room, nonce, AuthFrame(mac, room, wire.RoleMac, nonce, good), empty)
	if err != nil || !a.Provision || !bytes.Equal(a.Key, pub(mac)) {
		t.Fatalf("provision: %+v %v", a, err)
	}
	refused := map[string]struct {
		prov  []byte
		frame []byte
	}{
		"no proof":           {prov, AuthFrame(mac, room, wire.RoleMac, nonce, nil)},
		"proof for a key":    {prov, AuthFrame(stranger, room, wire.RoleMac, nonce, good)},
		"bad signature":      {prov, wire.Encode(wire.Auth{V: 1, Role: wire.RoleMac, Key: pub(mac), Sig: ed25519.Sign(stranger, AuthMessage(room, wire.RoleMac, nonce)), Proof: good})},
		"no provision key":   {nil, AuthFrame(mac, room, wire.RoleMac, nonce, ProvisionProof(nil, room, pub(mac)))},
		"short":              {prov[:31], AuthFrame(mac, room, wire.RoleMac, nonce, ProvisionProof(prov[:31], room, pub(mac)))},
		"phone provisioning": {prov, AuthFrame(mac, room, wire.RolePhone, nonce, good)},
	}
	for name, c := range refused {
		if _, err := CheckAuth(c.prov, room, nonce, c.frame, empty); err != ErrRefused {
			t.Fatalf("%s: %v", name, err)
		}
	}
	provisioned := keys{pub(mac), map[string]wire.Role{}}
	if _, err := CheckAuth(prov, room, nonce, AuthFrame(stranger, room, wire.RoleMac, nonce, ProvisionProof(prov, room, pub(stranger))), provisioned); err != ErrRefused {
		t.Fatalf("a valid proof for another key in a provisioned room: %v", err)
	}
}

// spyNonces and spyLimit record every touch.
type spyNonces struct {
	seen    map[[wire.NonceLen]byte]time.Time
	touched int
}

func (s *spyNonces) Has(n [wire.NonceLen]byte, now time.Time) bool {
	s.touched++
	exp, ok := s.seen[n]
	return ok && exp.After(now)
}

func (s *spyNonces) Add(n [wire.NonceLen]byte, exp time.Time) {
	s.touched++
	s.seen[n] = exp
}

type spyLimit struct {
	bucket  Bucket
	touched int
}

func (s *spyLimit) Allow(_ ed25519.PublicKey, now time.Time) bool {
	s.touched++
	return s.bucket.Allow(now)
}

type requestFixture struct {
	room         wire.RoomID
	watch, phone ed25519.PrivateKey
	stranger     ed25519.PrivateKey
	ks           keys
	now          time.Time
}

func newRequestFixture() requestFixture {
	f := requestFixture{watch: key("watch"), phone: key("phone"), stranger: key("stranger"), now: time.UnixMilli(1_790_000_000_000)}
	copy(f.room[:], seed("room"))
	f.ks = keys{pub(key("mac")), map[string]wire.Role{string(pub(f.watch)): wire.RoleWatch, string(pub(f.phone)): wire.RolePhone}}
	return f
}

func (f requestFixture) body(signer ed25519.PrivateKey, asKey ed25519.PublicKey, ts time.Time, nonce byte) []byte {
	r := &wire.Request{TS: uint64(ts.UnixMilli()), Enc: make([]byte, 32), Ciphertext: make([]byte, 128)}
	r.Nonce[0] = nonce
	SignRequest(signer, f.room, r)
	if asKey != nil {
		r.Key = asKey
	}
	return r.Bytes()
}

// TestS6SignatureBeforeNonceAndBucket: a request whose signature does not
// verify is refused before the nonce set or the bucket is touched, so it
// writes nothing and spends nothing.
func TestS6SignatureBeforeNonceAndBucket(t *testing.T) {
	f := newRequestFixture()
	nonces := &spyNonces{seen: map[[wire.NonceLen]byte]time.Time{}}
	limit := &spyLimit{bucket: Bucket{Rate: 2, Burst: 10}}
	refused := map[string][]byte{
		"bad signature": f.body(f.stranger, pub(f.watch), f.now, 1),
		"unknown key":   f.body(f.stranger, nil, f.now, 1),
		"phone key":     f.body(f.phone, nil, f.now, 1),
		"mac key":       f.body(key("mac"), nil, f.now, 1),
		"short":         f.body(f.watch, nil, f.now, 1)[:wire.MinRequest-1],
		"over 16 KiB":   append(f.body(f.watch, nil, f.now, 1), make([]byte, wire.MaxFrame)...),
		"another room":  func() []byte { g := f; g.room[0] ^= 1; return g.body(f.watch, nil, f.now, 1) }(),
	}
	for name, b := range refused {
		before := verifies.Load()
		if _, err := CheckRequest(f.room, b, f.ks, nonces, limit, f.now); err != ErrRefused {
			t.Fatalf("%s: %v", name, err)
		}
		// A body of the wrong size is refused on its length alone, which its
		// sender knows; every other refusal costs one verify.
		if n, sized := verifies.Load()-before, name == "short" || name == "over 16 KiB"; (sized && n != 0) || (!sized && n != 1) {
			t.Fatalf("%s: %d verifies", name, n)
		}
	}
	if nonces.touched != 0 || limit.touched != 0 {
		t.Fatalf("refused requests touched the nonce set %d times and the bucket %d times", nonces.touched, limit.touched)
	}
	if _, err := CheckRequest(f.room, f.body(f.watch, nil, f.now, 1), f.ks, nonces, limit, f.now); err != nil {
		t.Fatalf("the watch's request with nonce 1 after the refusals: %v", err)
	}
	if nonces.touched == 0 || limit.touched != 1 {
		t.Fatalf("an accepted request: nonce set %d, bucket %d", nonces.touched, limit.touched)
	}
	before := limit.touched
	for name, ts := range map[string]time.Time{"61 s old": f.now.Add(-61 * time.Second), "61 s ahead": f.now.Add(61 * time.Second)} {
		if _, err := CheckRequest(f.room, f.body(f.watch, nil, ts, 7), f.ks, nonces, limit, f.now); err != ErrRefused {
			t.Fatalf("%s: %v", name, err)
		}
	}
	if _, err := CheckRequest(f.room, f.body(f.watch, nil, f.now, 1), f.ks, nonces, limit, f.now.Add(time.Second)); err != ErrRefused {
		t.Fatalf("replayed nonce: %v", err)
	}
	if limit.touched != before {
		t.Fatal("a stale or replayed request spent a token")
	}
}

// TestS6NoRateOracle: a flood of requests under a paired watch's key with bad
// signatures never meets a 429, and leaves the watch's whole burst; only
// verified requests spend it.
func TestS6NoRateOracle(t *testing.T) {
	f := newRequestFixture()
	nonces := &spyNonces{seen: map[[wire.NonceLen]byte]time.Time{}}
	limit := &spyLimit{bucket: Bucket{Rate: 2, Burst: 10}}
	for i := 0; i < 50; i++ {
		if _, err := CheckRequest(f.room, f.body(f.stranger, pub(f.watch), f.now, byte(i)), f.ks, nonces, limit, f.now); err != ErrRefused {
			t.Fatalf("forged %d: %v", i, err)
		}
		if _, err := CheckRequest(f.room, f.body(f.stranger, nil, f.now, byte(i)), f.ks, nonces, limit, f.now); err != ErrRefused {
			t.Fatalf("unknown %d: %v", i, err)
		}
	}
	for i := 0; i < 10; i++ {
		if _, err := CheckRequest(f.room, f.body(f.watch, nil, f.now, byte(i)), f.ks, nonces, limit, f.now); err != nil {
			t.Fatalf("verified %d: %v", i, err)
		}
	}
	if _, err := CheckRequest(f.room, f.body(f.watch, nil, f.now, 10), f.ks, nonces, limit, f.now); !errors.Is(err, ErrRate) {
		t.Fatalf("the 11th verified request: %v", err)
	}
	if _, err := CheckRequest(f.room, f.body(f.watch, nil, f.now, 10), f.ks, nonces, limit, f.now.Add(500*time.Millisecond)); err != nil {
		t.Fatalf("after 500 ms the nonce refused by the bucket: %v", err)
	}
}

func TestBucket(t *testing.T) {
	b := Bucket{Rate: 20, Burst: 60}
	now := time.UnixMilli(0)
	for i := 0; i < 60; i++ {
		if !b.Allow(now) {
			t.Fatalf("burst %d", i)
		}
	}
	if b.Allow(now) {
		t.Fatal("61st at once")
	}
	if !b.Allow(now.Add(50*time.Millisecond)) || b.Allow(now.Add(50*time.Millisecond)) {
		t.Fatal("one token per 50 ms")
	}
	if !b.Allow(now.Add(time.Hour)) {
		t.Fatal("refilled")
	}
}

func TestDummyKey(t *testing.T) {
	s := sha256.Sum256([]byte(PrefixDummy))
	sk := ed25519.NewKeyFromSeed(s[:])
	if !bytes.Equal(DummyKey, pub(sk)) {
		t.Fatal("dummy key")
	}
	msg := []byte("m")
	if Verify(DummyKey, false, msg, ed25519.Sign(sk, msg)) || Verify(nil, false, msg, ed25519.Sign(sk, msg)) {
		t.Fatal("a check accepted on the dummy key")
	}
	if !Verify(DummyKey, true, msg, ed25519.Sign(sk, msg)) {
		t.Fatal("a known key's signature")
	}
}
