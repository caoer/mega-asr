package wire

import (
	"bytes"
	"crypto/ed25519"
	"crypto/sha256"
	"reflect"
	"strings"
	"testing"
)

func key(name string) ed25519.PublicKey {
	s := sha256.Sum256([]byte(name))
	return ed25519.NewKeyFromSeed(s[:]).Public().(ed25519.PublicKey)
}

func sid(b byte) (s SID) { s[0] = b; return }

func TestRoomText(t *testing.T) {
	var id RoomID
	for i := range id {
		id[i] = byte(i * 7)
	}
	text := id.String()
	if len(text) != RoomTextLen {
		t.Fatalf("%q", text)
	}
	got, err := ParseRoom(text)
	if err != nil || got != id {
		t.Fatalf("round trip: %v", err)
	}
	for _, bad := range []string{"", text[:31], text + "A", strings.Replace(text, text[:1], "+", 1), text[:30] + "=="} {
		if _, err := ParseRoom(bad); err == nil {
			t.Fatalf("%q parsed", bad)
		}
	}
}

func TestUnb64Strict(t *testing.T) {
	k := key("k")
	s := B64(k)
	if b, err := Unb64(s, 32); err != nil || !bytes.Equal(b, k) {
		t.Fatal(err)
	}
	// 32 bytes leave 2 unused bits in the last character; a set one is refused.
	last := s[len(s)-1]
	alt := strings.IndexByte("ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789-_", last) | 1
	if _, err := Unb64(s[:len(s)-1]+string("ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789-_"[alt]), 32); err == nil {
		t.Fatal("non-canonical trailing bits accepted")
	}
	for _, bad := range []string{s + "=", s[:42], strings.Replace(s, s[:1], "/", 1)} {
		if _, err := Unb64(bad, 32); err == nil {
			t.Fatalf("%q accepted", bad)
		}
	}
}

func TestControlRoundTrip(t *testing.T) {
	var nonce [ChalLen]byte
	nonce[3] = 9
	room := []any{
		Challenge{nonce},
		OKPhone{sid(1), true},
		OKPhone{sid(2), false},
		OKMac{Peers: []PeerInfo{{sid(3), RolePhone, key("p")}}},
		OKMac{Peers: []PeerInfo{}},
		Peer{sid(4), RoleWatch, key("w"), true, true},
		DevicesList{Keys: []Device{{RolePhone, key("p")}}},
	}
	for _, f := range room {
		got, err := DecodeFromRoom(Encode(f))
		if err != nil || !reflect.DeepEqual(got, f) {
			t.Fatalf("%T: %+v, %v (%s)", f, got, err, Encode(f))
		}
	}
	mac := []any{
		PeerAck{sid(5)},
		Drop{sid(6)},
		Reset{},
		DevicesChange{Add: []Device{{RoleWatch, key("w")}}, Remove: []ed25519.PublicKey{key("old")}},
	}
	for _, f := range mac {
		got, err := DecodeFromMac(Encode(f))
		if err != nil || !reflect.DeepEqual(got, f) {
			t.Fatalf("%T: %+v, %v (%s)", f, got, err, Encode(f))
		}
	}
	a := Auth{V: 1, Role: RoleMac, Key: key("m"), Sig: make([]byte, 64), Proof: make([]byte, 32)}
	got, err := DecodeAuth(Encode(a))
	if err != nil || !reflect.DeepEqual(got, a) {
		t.Fatalf("auth: %+v %v", got, err)
	}
}

func TestEncodeIsExact(t *testing.T) {
	want := `{"t":"peer","sid":"BwAAAAAAAAA","role":"phone","key":"` + B64(key("p")) + `","up":false,"oneshot":false}`
	if got := string(Encode(Peer{sid(7), RolePhone, key("p"), false, false})); got != want {
		t.Fatalf("%s", got)
	}
	l := DevicesList{Keys: []Device{{RolePhone, key("b")}, {RoleWatch, key("a")}}}
	got, _ := DecodeFromRoom(Encode(l))
	keys := got.(DevicesList).Keys
	if B64(keys[0].Key) > B64(keys[1].Key) {
		t.Fatal("devices list is not sorted by key text")
	}
}

func TestDecodeRefuses(t *testing.T) {
	k := B64(key("k"))
	s := B64(make([]byte, 64))
	auth := []string{
		``, `[]`, `null`, `{"t":"auth"}`, `{"t":"auth","v":1,"role":"phone","key":"` + k + `"}`,
		`{"t":"auth","v":"1","role":"phone","key":"` + k + `","sig":"` + s + `"}`,
		`{"t":"auth","v":1.5,"role":"phone","key":"` + k + `","sig":"` + s + `"}`,
		`{"t":"auth","v":1,"role":null,"key":"` + k + `","sig":"` + s + `"}`,
		`{"t":"auth","v":1,"role":"phone","key":"` + k[:40] + `","sig":"` + s + `"}`,
		`{"t":"auth","v":1,"role":"phone","key":"` + k + `","sig":"` + s + `","provision":{}}`,
		`{"t":"auth","v":1,"role":"phone","key":"` + k + `","sig":"` + s + `"} {}`,
		`{"t":"ok","v":1,"role":"phone","key":"` + k + `","sig":"` + s + `"}`,
	}
	for _, f := range auth {
		if _, err := DecodeAuth([]byte(f)); err == nil {
			t.Fatalf("auth %s decoded", f)
		}
	}
	mac := []string{
		`{"t":"peer_ack"}`, `{"t":"drop","sid":"AAAA"}`, `{"t":"devices","add":[]}`,
		`{"t":"devices","add":[{"role":"mac","key":"` + k + `"}],"remove":[]}`,
		`{"t":"devices","add":null,"remove":[]}`, `{"t":"devices","add":[],"remove":[1]}`,
		`{"t":"hello"}`, `{"t":"ok","sid":"AAAAAAAAAAA","peer":true}`,
		`{"t":"reset"}}`, `{"t":"reset"}]`, `{"t":"reset"} x`,
	}
	for _, f := range mac {
		if _, err := DecodeFromMac([]byte(f)); err == nil {
			t.Fatalf("mac %s decoded", f)
		}
	}
	if _, err := DecodeFromMac([]byte(" {\"t\":\"reset\"}\n ")); err != nil {
		t.Fatalf("whitespace around a frame: %v", err)
	}
	if _, err := DecodeFromRoom([]byte(`{"t":"ok","sid":"AAAAAAAAAAA"}`)); err == nil {
		t.Fatal("ok without peer decoded")
	}
}

func TestRequestBody(t *testing.T) {
	r := &Request{Key: key("w"), TS: 1_790_000_000_123, Sig: bytes.Repeat([]byte{2}, 64), Enc: bytes.Repeat([]byte{3}, 32), Ciphertext: bytes.Repeat([]byte{4}, MinHandshake)}
	r.Nonce[0] = 1
	b := r.Bytes()
	if len(b) != MinRequest {
		t.Fatalf("%d bytes", len(b))
	}
	got, err := ParseRequest(b)
	if err != nil || !reflect.DeepEqual(got, r) {
		t.Fatalf("%+v %v", got, err)
	}
	if !bytes.Equal(got.Message(), append(append([]byte(nil), r.Enc...), r.Ciphertext...)) || got.Time().UnixMilli() != 1_790_000_000_123 {
		t.Fatal("message or time")
	}
	if _, err := ParseRequest(b[:MinRequest-1]); err == nil {
		t.Fatal("short body parsed")
	}
	if _, err := ParseRequest(make([]byte, MaxFrame+1)); err == nil {
		t.Fatal("long body parsed")
	}
}

func TestSID(t *testing.T) {
	f := JoinSID(sid(9), []byte{1, 2})
	s, rest, err := SplitSID(f)
	if err != nil || s != sid(9) || !bytes.Equal(rest, []byte{1, 2}) {
		t.Fatal(s, rest, err)
	}
	if _, _, err := SplitSID(f[:7]); err == nil {
		t.Fatal("7 bytes split")
	}
}
