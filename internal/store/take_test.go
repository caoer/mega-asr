package store

import (
	"bytes"
	"encoding/binary"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/caoer/mega-asr/internal/audio"
)

var at = time.Date(2020, 5, 11, 9, 7, 38, 0, time.UTC)

func block(k int) []int16 {
	s := make([]int16, audio.Block)
	for i := range s {
		s[i] = int16(k*1000 + i)
	}
	return s
}

func writeTake(t *testing.T, s Store, blocks int) *Take {
	t.Helper()
	tk, err := s.Begin(at)
	if err != nil {
		t.Fatal(err)
	}
	for k := range blocks {
		if err := tk.Write(block(k)); err != nil {
			t.Fatal(err)
		}
	}
	return tk
}

func checkSamples(t *testing.T, base string, blocks int) {
	t.Helper()
	got, err := audio.ReadWAV(base + ".wav")
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != blocks*audio.Block {
		t.Fatalf("%d samples, want %d", len(got), blocks*audio.Block)
	}
	for k := range blocks {
		if got[k*audio.Block+7] != block(k)[7] {
			t.Fatalf("block %d: sample %d, want %d", k, got[k*audio.Block+7], block(k)[7])
		}
	}
}

// A take the process never closed — killed mid-recording — is whole after
// Repair and is reported as an orphan.
func TestKilledTakeRepaired(t *testing.T) {
	s := Store{Dir: t.TempDir()}
	tk := writeTake(t, s, 3)
	tk.f.Close() // the kill: the fd goes, the header is never patched
	if got, _ := audio.ReadWAV(tk.Base() + ".wav"); len(got) != 0 {
		t.Fatalf("unrepaired header already names %d samples", len(got))
	}
	orphans, err := s.Repair()
	if err != nil {
		t.Fatal(err)
	}
	if len(orphans) != 1 || orphans[0] != tk.Base() {
		t.Fatalf("orphans %v, want [%s]", orphans, tk.Base())
	}
	checkSamples(t, tk.Base(), 3)
	h, _ := os.ReadFile(tk.Base() + ".wav")
	if riff := binary.LittleEndian.Uint32(h[4:]); riff != uint32(36+3*2*audio.Block) {
		t.Fatalf("RIFF size %d", riff)
	}
}

// A kill mid-write leaves half a sample; Repair drops it.
func TestRepairOddTail(t *testing.T) {
	s := Store{Dir: t.TempDir()}
	tk := writeTake(t, s, 2)
	tk.f.Write([]byte{0x7f})
	tk.f.Close()
	if _, err := s.Repair(); err != nil {
		t.Fatal(err)
	}
	checkSamples(t, tk.Base(), 2)
	if st, _ := os.Stat(tk.Base() + ".wav"); st.Size() != 44+2*2*audio.Block {
		t.Fatalf("size %d", st.Size())
	}
}

// A closed take needs no repair, and one with its text is no orphan.
func TestClosedTake(t *testing.T) {
	s := Store{Dir: t.TempDir()}
	tk := writeTake(t, s, 3)
	if err := tk.Close(); err != nil {
		t.Fatal(err)
	}
	if err := tk.Write(block(9)); err == nil {
		t.Fatal("write after Close succeeded")
	}
	checkSamples(t, tk.Base(), 3)
	before, _ := os.ReadFile(tk.Base() + ".wav")
	if err := s.SaveText(tk.Base(), "raw", "text"); err != nil {
		t.Fatal(err)
	}
	second := writeTake(t, s, 1) // same second: <stamp>-2
	second.Close()
	orphans, err := s.Repair()
	if err != nil {
		t.Fatal(err)
	}
	if len(orphans) != 1 || orphans[0] != second.Base() {
		t.Fatalf("orphans %v, want [%s]", orphans, second.Base())
	}
	after, _ := os.ReadFile(tk.Base() + ".wav")
	if !bytes.Equal(before, after) {
		t.Fatal("Repair rewrote a closed take")
	}
}

func TestRemove(t *testing.T) {
	s := Store{Dir: t.TempDir()}
	tk := writeTake(t, s, 1)
	tk.Close()
	if err := s.Remove(tk.Base()); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(tk.Base() + ".wav"); !os.IsNotExist(err) {
		t.Fatalf("wav still there: %v", err)
	}
	if err := s.Remove(tk.Base()); err != nil {
		t.Fatalf("second Remove: %v", err)
	}
}

// A take's input record round-trips, and Remove deletes it with the WAV.
func TestInputRecord(t *testing.T) {
	s := Store{Dir: t.TempDir()}
	tk := writeTake(t, s, 1)
	tk.Close()
	in := audio.TakeInput{Source: "ssh", Host: "garage-pi", PCM: "plughw:1,0", Channel: 1}
	if err := s.SaveInput(tk.Base(), in); err != nil {
		t.Fatal(err)
	}
	if got, err := LoadInput(tk.Base()); err != nil || got != in {
		t.Fatalf("LoadInput = %+v, %v", got, err)
	}
	if err := s.Remove(tk.Base()); err != nil {
		t.Fatal(err)
	}
	for _, ext := range []string{".wav", ".input.json"} {
		if _, err := os.Stat(tk.Base() + ext); !os.IsNotExist(err) {
			t.Errorf("%s after Remove: %v", ext, err)
		}
	}
}

// A take's backup track, backup/<name>.wav, is not a take: Repair lists only
// the take, and the backup's open header is left as it is.
func TestRepairSkipsBackup(t *testing.T) {
	s := Store{Dir: t.TempDir()}
	tk := writeTake(t, s, 2)
	b, err := Store{Dir: filepath.Join(s.Dir, BackupDir)}.Create(filepath.Base(tk.Base()))
	if err != nil {
		t.Fatal(err)
	}
	b.Write(block(1))
	orphans, err := s.Repair()
	if err != nil || len(orphans) != 1 || orphans[0] != tk.Base() {
		t.Fatalf("orphans %v, %v; want the take alone", orphans, err)
	}
}
