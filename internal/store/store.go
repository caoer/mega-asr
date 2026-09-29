// Package store keeps every utterance: <dir>/<YYYYmmdd-HHMMSS>.wav, the
// delivered transcript beside it as .txt, the raw ASR output as .raw.txt and
// the input device that captured it as .input.json, and the take's event
// record as .events.jsonl (Append, Read, State).
// A take is written as it is recorded (Begin); the only deletion is the audio
// of a take too short to be speech (Remove), whose record stays.
package store

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/caoer/mega-asr/internal/audio"
)

// Store is the utterance directory.
type Store struct{ Dir string }

// BackupDir is the store's subdirectory of backup tracks: backup/<name>.wav
// is recorded beside the take <name> on a second input. The store's listings
// read the top level only, so a backup track is never taken for a take.
const BackupDir = "backup"

// Next returns an unused path base for an utterance at t: the timestamp, with
// -2, -3… when a second utterance falls in the same second. A name whose WAV
// or event record exists is used, so a name is never given twice, even after
// Remove.
func (s Store) Next(t time.Time) (string, error) {
	if err := os.MkdirAll(s.Dir, 0o755); err != nil {
		return "", err
	}
	stamp := t.Format("20060102-150405")
	base := filepath.Join(s.Dir, stamp)
	for i := 2; exists(base+".wav") || exists(base+eventsExt); i++ {
		base = filepath.Join(s.Dir, fmt.Sprintf("%s-%d", stamp, i))
	}
	return base, nil
}

// SaveText writes the delivered transcript as <base>.txt and the ASR output
// before corrections as <base>.raw.txt; both are empty when there was no speech.
func (s Store) SaveText(base, raw, text string) error {
	if err := os.WriteFile(base+".raw.txt", []byte(raw+"\n"), 0o644); err != nil {
		return err
	}
	return os.WriteFile(base+".txt", []byte(text+"\n"), 0o644)
}

// SaveInput writes the device that captured the take as <base>.input.json.
func (s Store) SaveInput(base string, in audio.TakeInput) error {
	b, err := json.Marshal(in)
	if err != nil {
		return err
	}
	return os.WriteFile(base+".input.json", append(b, '\n'), 0o644)
}

// LoadInput reads a take's <base>.input.json; a take recorded before the
// record existed has none and returns an error wrapping fs.ErrNotExist.
func LoadInput(base string) (audio.TakeInput, error) {
	var in audio.TakeInput
	b, err := os.ReadFile(base + ".input.json")
	if err != nil {
		return in, err
	}
	return in, json.Unmarshal(b, &in)
}

func exists(p string) bool {
	_, err := os.Stat(p)
	return err == nil
}
