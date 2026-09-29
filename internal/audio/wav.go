package audio

import (
	"encoding/binary"
	"fmt"
	"io"
	"os"
)

// WriteWAV writes 16 kHz mono S16LE samples as a canonical 44-byte-header WAV.
func WriteWAV(w io.Writer, samples []int16) error {
	data := uint32(len(samples) * 2)
	h := make([]byte, 44)
	copy(h[0:], "RIFF")
	binary.LittleEndian.PutUint32(h[4:], 36+data)
	copy(h[8:], "WAVEfmt ")
	binary.LittleEndian.PutUint32(h[16:], 16) // fmt chunk size
	binary.LittleEndian.PutUint16(h[20:], 1)  // PCM
	binary.LittleEndian.PutUint16(h[22:], 1)  // mono
	binary.LittleEndian.PutUint32(h[24:], Rate)
	binary.LittleEndian.PutUint32(h[28:], Rate*2) // byte rate
	binary.LittleEndian.PutUint16(h[32:], 2)      // block align
	binary.LittleEndian.PutUint16(h[34:], 16)     // bits per sample
	copy(h[36:], "data")
	binary.LittleEndian.PutUint32(h[40:], data)
	if _, err := w.Write(h); err != nil {
		return err
	}
	return binary.Write(w, binary.LittleEndian, samples)
}

// ReadWAV reads a canonical 16 kHz mono S16LE WAV as WriteWAV writes it. The
// samples are the ones the header's data size names — what an ASR reader
// sees — or fewer when the file is shorter than its header says.
func ReadWAV(path string) ([]int16, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	le := binary.LittleEndian
	if len(b) < 44 || string(b[0:4]) != "RIFF" || string(b[36:40]) != "data" ||
		le.Uint16(b[20:]) != 1 || le.Uint16(b[22:]) != 1 || le.Uint32(b[24:]) != Rate || le.Uint16(b[34:]) != 16 {
		return nil, fmt.Errorf("%s: not a 16 kHz mono 16-bit WAV", path)
	}
	data := b[44:]
	if n := int(le.Uint32(b[40:])); n < len(data) {
		data = data[:n]
	}
	s := make([]int16, len(data)/2)
	for i := range s {
		s[i] = int16(le.Uint16(data[2*i:]))
	}
	return s, nil
}

// SaveWAV writes samples to path.
func SaveWAV(path string, samples []int16) error {
	f, err := os.Create(path)
	if err != nil {
		return err
	}
	if err := WriteWAV(f, samples); err != nil {
		f.Close()
		return err
	}
	return f.Close()
}
