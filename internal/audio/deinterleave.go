package audio

import "encoding/binary"

// Deinterleaver keeps one channel of an interleaved S16LE stream that
// arrives in arbitrary byte chunks; a frame split across chunks is carried
// to the next Push.
type Deinterleaver struct {
	Channels int
	Channel  int // 1-based

	rest []byte
}

// Push consumes a chunk and returns the samples of every frame it completed.
func (d *Deinterleaver) Push(b []byte) []int16 {
	frame := 2 * d.Channels
	if len(d.rest) > 0 {
		b = append(d.rest, b...)
	}
	n := len(b) / frame
	out := make([]int16, n)
	off := 2 * (d.Channel - 1)
	for i := range n {
		out[i] = int16(binary.LittleEndian.Uint16(b[i*frame+off:]))
	}
	d.rest = append(d.rest[:0:0], b[n*frame:]...)
	return out
}
