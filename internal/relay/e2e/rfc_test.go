package e2e

import (
	"bytes"
	"crypto/ecdh"
	"crypto/hpke"
	"encoding/hex"
	"encoding/json"
	"os"
	"testing"

	"golang.org/x/crypto/chacha20poly1305"
)

func unhex(t *testing.T, s string) []byte {
	t.Helper()
	b, err := hex.DecodeString(s)
	if err != nil {
		t.Fatalf("hex %q: %v", s, err)
	}
	return b
}

func readJSON(t *testing.T, name string, v any) {
	t.Helper()
	b, err := os.ReadFile("testdata/" + name)
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(b, v); err != nil {
		t.Fatalf("%s: %v", name, err)
	}
}

type rfc9180 struct {
	Setup struct {
		Mode, KEMID, KDFID, AEADID int
		Info, IkmE, PkEm, SkEm     string
		IkmR, PkRm, SkRm, Enc      string
		Key, BaseNonce             string
	}
	Encryptions []struct {
		Seq                int
		Pt, Aad, Nonce, Ct string
	}
	Exports []struct {
		ExporterContext string `json:"exporter_context"`
		L               int
		ExportedValue   string `json:"exported_value"`
	}
}

func (v *rfc9180) UnmarshalJSON(b []byte) error {
	var raw struct {
		Setup       map[string]any
		Encryptions []map[string]any
		Exports     json.RawMessage
	}
	if err := json.Unmarshal(b, &raw); err != nil {
		return err
	}
	s := raw.Setup
	str := func(k string) string { x, _ := s[k].(string); return x }
	num := func(k string) int { x, _ := s[k].(float64); return int(x) }
	v.Setup.Mode, v.Setup.KEMID, v.Setup.KDFID, v.Setup.AEADID = num("mode"), num("kem_id"), num("kdf_id"), num("aead_id")
	v.Setup.Info, v.Setup.IkmE, v.Setup.PkEm, v.Setup.SkEm = str("info"), str("ikmE"), str("pkEm"), str("skEm")
	v.Setup.IkmR, v.Setup.PkRm, v.Setup.SkRm, v.Setup.Enc = str("ikmR"), str("pkRm"), str("skRm"), str("enc")
	v.Setup.Key, v.Setup.BaseNonce = str("key"), str("base_nonce")
	for _, e := range raw.Encryptions {
		var x struct {
			Seq                int
			Pt, Aad, Nonce, Ct string
		}
		x.Seq = int(e["seq"].(float64))
		x.Pt, x.Aad, x.Nonce, x.Ct = e["pt"].(string), e["aad"].(string), e["nonce"].(string), e["ct"].(string)
		v.Encryptions = append(v.Encryptions, x)
	}
	return json.Unmarshal(raw.Exports, &v.Exports)
}

// TestRFC9180A2 checks Go's HPKE against RFC 9180 A.2.1, the suite the relay
// uses: key derivation, the recipient's opens, the AEAD at every listed
// sequence number, and the exporter.
func TestRFC9180A2(t *testing.T) {
	var v rfc9180
	readJSON(t, "rfc9180-a2.json", &v)
	s := v.Setup
	if s.Mode != 0 || s.KEMID != 0x20 || s.KDFID != 1 || s.AEADID != 3 {
		t.Fatalf("vector is for another suite: %+v", s)
	}
	if kem.ID() != 0x20 || kdf.ID() != 1 || aead.ID() != 3 {
		t.Fatalf("relay suite is %#x/%#x/%#x", kem.ID(), kdf.ID(), aead.ID())
	}
	for _, kp := range []struct{ ikm, sk, pk string }{{s.IkmR, s.SkRm, s.PkRm}, {s.IkmE, s.SkEm, s.PkEm}} {
		k, err := kem.DeriveKeyPair(unhex(t, kp.ikm))
		if err != nil {
			t.Fatal(err)
		}
		// Go returns the clamped scalar (RFC 7748 § 5); the vector does not.
		sk, _ := k.Bytes()
		want := unhex(t, kp.sk)
		want[0] &= 248
		want[31] = want[31]&127 | 64
		if !bytes.Equal(sk, want) || !bytes.Equal(k.PublicKey().Bytes(), unhex(t, kp.pk)) {
			t.Fatalf("DeriveKeyPair(%s) = %x, %x", kp.ikm, sk, k.PublicKey().Bytes())
		}
	}
	skR, err := ecdh.X25519().NewPrivateKey(unhex(t, s.SkRm))
	if err != nil {
		t.Fatal(err)
	}
	k, err := hpke.NewDHKEMPrivateKey(skR)
	if err != nil {
		t.Fatal(err)
	}
	r, err := hpke.NewRecipient(unhex(t, s.Enc), k, kdf, aead, unhex(t, s.Info))
	if err != nil {
		t.Fatal(err)
	}
	// Recipient.Open counts successful opens, so it reaches sequence numbers
	// 0, 1 and 2 of the vector; the AEAD check below covers every number.
	for _, e := range v.Encryptions[:3] {
		pt, err := r.Open(unhex(t, e.Aad), unhex(t, e.Ct))
		if err != nil || !bytes.Equal(pt, unhex(t, e.Pt)) {
			t.Fatalf("open seq %d: %x, %v", e.Seq, pt, err)
		}
	}
	c, err := chacha20poly1305.New(unhex(t, s.Key))
	if err != nil {
		t.Fatal(err)
	}
	base := unhex(t, s.BaseNonce)
	for _, e := range v.Encryptions {
		nonce := append([]byte(nil), base...)
		for i := 0; i < 8; i++ {
			nonce[len(nonce)-1-i] ^= byte(uint64(e.Seq) >> (8 * i))
		}
		if !bytes.Equal(nonce, unhex(t, e.Nonce)) {
			t.Fatalf("seq %d nonce %x", e.Seq, nonce)
		}
		if ct := c.Seal(nil, nonce, unhex(t, e.Pt), unhex(t, e.Aad)); !bytes.Equal(ct, unhex(t, e.Ct)) {
			t.Fatalf("seq %d ct %x", e.Seq, ct)
		}
	}
	for _, e := range v.Exports {
		got, err := r.Export(string(unhex(t, e.ExporterContext)), e.L)
		if err != nil || !bytes.Equal(got, unhex(t, e.ExportedValue)) {
			t.Fatalf("export %q: %x, %v", e.ExporterContext, got, err)
		}
	}
}

type rfc7748 struct {
	X25519   []struct{ Scalar, U, Out string }
	Iterated struct {
		Start     string
		After1    string `json:"after_1"`
		After1000 string `json:"after_1000"`
	}
	DH struct {
		AliceSK string `json:"alice_sk"`
		AlicePK string `json:"alice_pk"`
		BobSK   string `json:"bob_sk"`
		BobPK   string `json:"bob_pk"`
		Shared  string
	}
}

func x(t *testing.T, scalar, u []byte) []byte {
	t.Helper()
	sk, err := ecdh.X25519().NewPrivateKey(scalar)
	if err != nil {
		t.Fatal(err)
	}
	out, err := x25519(sk, u)
	if err != nil {
		t.Fatal(err)
	}
	return out
}

// TestRFC7748 checks X25519 against RFC 7748.
func TestRFC7748(t *testing.T) {
	var v rfc7748
	readJSON(t, "rfc7748.json", &v)
	if len(v.X25519) != 2 {
		t.Fatalf("%d function vectors", len(v.X25519))
	}
	for _, e := range v.X25519 {
		if got := x(t, unhex(t, e.Scalar), unhex(t, e.U)); !bytes.Equal(got, unhex(t, e.Out)) {
			t.Fatalf("X25519(%s, %s) = %x", e.Scalar, e.U, got)
		}
	}
	k, u := unhex(t, v.Iterated.Start), unhex(t, v.Iterated.Start)
	for i := 1; i <= 1000; i++ {
		k, u = x(t, k, u), k
		if i == 1 && !bytes.Equal(k, unhex(t, v.Iterated.After1)) {
			t.Fatalf("after 1: %x", k)
		}
	}
	if !bytes.Equal(k, unhex(t, v.Iterated.After1000)) {
		t.Fatalf("after 1000: %x", k)
	}
	d := v.DH
	for _, kp := range [][2]string{{d.AliceSK, d.AlicePK}, {d.BobSK, d.BobPK}} {
		sk, err := ecdh.X25519().NewPrivateKey(unhex(t, kp[0]))
		if err != nil || !bytes.Equal(sk.PublicKey().Bytes(), unhex(t, kp[1])) {
			t.Fatalf("public key of %s: %v", kp[0], err)
		}
	}
	if a, b := x(t, unhex(t, d.AliceSK), unhex(t, d.BobPK)), x(t, unhex(t, d.BobSK), unhex(t, d.AlicePK)); !bytes.Equal(a, unhex(t, d.Shared)) || !bytes.Equal(b, a) {
		t.Fatalf("shared %x %x", a, b)
	}
}
