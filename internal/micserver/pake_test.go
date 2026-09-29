package micserver

import (
	"bytes"
	"testing"
)

func TestPAKEAgrees(t *testing.T) {
	ad := []byte("cert A")
	msgA, i, err := startPAKE("12345678", "studio", ad)
	if err != nil {
		t.Fatal(err)
	}
	msgB, r, err := respondPAKE("12345678", "studio", ad, msgA)
	if err != nil {
		t.Fatal(err)
	}
	confirm, kMac, err := i.finish(msgB)
	if err != nil {
		t.Fatal(err)
	}
	kBox, err := r.finish(confirm)
	if err != nil || !bytes.Equal(kMac, kBox) {
		t.Fatalf("keys differ or confirmation refused: %v", err)
	}
	sealed, tag := boxSeal(kBox, msgA, msgB, "micbox", "studio", "tok")
	if tok, err := macOpen(kMac, msgA, msgB, "micbox", "studio", sealed, tag); err != nil || tok != "tok" {
		t.Errorf("open: %q %v", tok, err)
	}
}

// Each side binds the certificate it sees. With different ones, even a
// right PIN confirms on neither side: the box refuses the Mac's confirmation,
// and the Mac refuses what the box would have sent.
func TestPAKEDifferentCertificatesConfirmOnNeitherSide(t *testing.T) {
	msgA, i, _ := startPAKE("12345678", "studio", []byte("relay cert"))
	msgB, r, _ := respondPAKE("12345678", "studio", []byte("box cert"), msgA)
	confirm, kMac, err := i.finish(msgB)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := r.finish(confirm); err != errConfirm {
		t.Errorf("box accepted the Mac's confirmation: %v", err)
	}
	sealed, tag := boxSeal(r.key, msgA, msgB, "micbox", "studio", "tok")
	if _, err := macOpen(kMac, msgA, msgB, "micbox", "studio", sealed, tag); err == nil {
		t.Error("Mac accepted the box's confirmation")
	}
}

func TestPAKEWrongPIN(t *testing.T) {
	ad := []byte("cert")
	msgA, i, _ := startPAKE("12345678", "studio", ad)
	msgB, r, _ := respondPAKE("87654321", "studio", ad, msgA)
	confirm, _, _ := i.finish(msgB)
	if _, err := r.finish(confirm); err != errConfirm {
		t.Errorf("wrong PIN confirmed: %v", err)
	}
}
