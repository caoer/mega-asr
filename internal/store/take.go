package store

import (
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/caoer/mega-asr/internal/audio"
)

const header = 44 // canonical WAV header, as audio.WriteWAV writes it

// Take is a recording streamed to <base>.wav as it arrives: the header's
// sizes stay 0 until Close patches them, so a crash leaves every written
// block on disk for Repair.
type Take struct {
	mu   sync.Mutex
	f    *os.File
	base string
	n    int // samples written
}

// Begin creates the take's WAV at the next free base for t.
func (s Store) Begin(t time.Time) (*Take, error) {
	base, err := s.Next(t)
	if err != nil {
		return nil, err
	}
	return create(base)
}

// Create creates the take <dir>/<name>.wav; it must not exist yet.
func (s Store) Create(name string) (*Take, error) {
	if err := os.MkdirAll(s.Dir, 0o755); err != nil {
		return nil, err
	}
	return create(filepath.Join(s.Dir, name))
}

func create(base string) (*Take, error) {
	f, err := os.OpenFile(base+".wav", os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o644)
	if err != nil {
		return nil, err
	}
	if err := audio.WriteWAV(f, nil); err != nil {
		f.Close()
		return nil, err
	}
	return &Take{f: f, base: base}, nil
}

// Base is the take's path without extension.
func (t *Take) Base() string { return t.base }

// Samples is how many samples have been written.
func (t *Take) Samples() int {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.n
}

// Write appends samples with one write, unbuffered: once it returns, a crash
// of this process no longer loses them.
func (t *Take) Write(samples []int16) error {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.f == nil {
		return errors.New("store: write to a closed take")
	}
	b := make([]byte, 2*len(samples))
	for i, v := range samples {
		binary.LittleEndian.PutUint16(b[2*i:], uint16(v))
	}
	if _, err := t.f.Write(b); err != nil {
		return err
	}
	t.n += len(samples)
	return nil
}

// Close patches the header's sizes, flushes the file to the disk itself
// (Sync is F_FULLFSYNC on macOS) and closes it.
func (t *Take) Close() error {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.f == nil {
		return nil
	}
	f := t.f
	t.f = nil
	err := errors.Join(patch(f, uint32(2*t.n)), f.Sync())
	return errors.Join(err, f.Close())
}

// patch writes the RIFF and data sizes for data bytes of samples.
func patch(f *os.File, data uint32) error {
	var b [4]byte
	binary.LittleEndian.PutUint32(b[:], 36+data)
	if _, err := f.WriteAt(b[:], 4); err != nil {
		return err
	}
	binary.LittleEndian.PutUint32(b[:], data)
	_, err := f.WriteAt(b[:], 40)
	return err
}

// Repair runs at startup. Every WAV whose header disagrees with its size — a
// take the app did not Close — gets its header patched to the samples on
// disk. It returns the bases that have no .txt: takes never transcribed.
func (s Store) Repair() ([]string, error) {
	wavs, err := filepath.Glob(filepath.Join(s.Dir, "*.wav"))
	if err != nil {
		return nil, err
	}
	var orphans, errs []string
	for _, w := range wavs {
		if err := repair(w); err != nil {
			errs = append(errs, err.Error())
			continue
		}
		base := strings.TrimSuffix(w, ".wav")
		if !exists(base + ".txt") {
			orphans = append(orphans, base)
		}
	}
	if len(errs) > 0 {
		return orphans, fmt.Errorf("store: repair: %s", strings.Join(errs, "; "))
	}
	return orphans, nil
}

// RepairWAV patches the header of the WAV at path to the samples on disk,
// as Repair does for each of the store's.
func RepairWAV(path string) error { return repair(path) }

// repair patches one WAV's header when its data size is not the file's.
// A file cut before its header was whole gets a fresh empty header; a stray
// half sample at the end is dropped.
func repair(path string) error {
	f, err := os.OpenFile(path, os.O_RDWR, 0)
	if err != nil {
		return err
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil {
		return err
	}
	if st.Size() < header {
		if err := f.Truncate(0); err != nil {
			return err
		}
		if err := audio.WriteWAV(f, nil); err != nil {
			return err
		}
		return f.Sync()
	}
	var h [header]byte
	if _, err := io.ReadFull(f, h[:]); err != nil {
		return err
	}
	if string(h[0:4]) != "RIFF" || string(h[36:40]) != "data" {
		return fmt.Errorf("%s: not a WAV", path)
	}
	data := uint32(st.Size()-header) &^ 1
	if binary.LittleEndian.Uint32(h[40:]) == data && binary.LittleEndian.Uint32(h[4:]) == 36+data {
		return nil
	}
	if int64(data) != st.Size()-header {
		if err := f.Truncate(header + int64(data)); err != nil {
			return err
		}
	}
	if err := patch(f, data); err != nil {
		return err
	}
	return f.Sync()
}

// Remove deletes a take's WAV and input record — the one deletion: a take
// under 0.3 s is a mistaken tap, not speech. The event record stays, and
// Next never gives the take's name again.
func (s Store) Remove(base string) error {
	var errs []error
	for _, ext := range []string{".wav", ".input.json"} {
		if err := os.Remove(base + ext); err != nil && !errors.Is(err, os.ErrNotExist) {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}
