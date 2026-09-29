package audio

import (
	"bytes"
	"encoding/binary"
	"testing"
)

func TestWAVHeader(t *testing.T) {
	var b bytes.Buffer
	if err := WriteWAV(&b, []int16{1, -1, 300}); err != nil {
		t.Fatal(err)
	}
	h := b.Bytes()
	if len(h) != 44+6 || string(h[0:4]) != "RIFF" || string(h[8:16]) != "WAVEfmt " || string(h[36:40]) != "data" {
		t.Fatalf("bad header % x", h[:44])
	}
	le := binary.LittleEndian
	if le.Uint32(h[4:]) != 36+6 || le.Uint32(h[24:]) != 16000 || le.Uint16(h[22:]) != 1 ||
		le.Uint16(h[34:]) != 16 || le.Uint32(h[40:]) != 6 {
		t.Fatalf("bad fields % x", h[:44])
	}
	if int16(le.Uint16(h[46:])) != -1 {
		t.Fatalf("bad data % x", h[44:])
	}
}
