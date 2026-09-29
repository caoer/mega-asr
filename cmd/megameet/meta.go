package main

import (
	"bytes"
	"os"
	"path/filepath"
	"time"

	"github.com/BurntSushi/toml"
)

// Meta is a recording's meta.toml: what was recorded, from where, and the
// local track files. Upload adds the packed files and their sha256s.
type Meta struct {
	ID          string    `toml:"id"`     // YYYYMMDD-HHMMSS-<source>-<host>, the registry's key
	Source      string    `toml:"source"` // mac
	Host        string    `toml:"host"`
	Title       string    `toml:"title"`
	Speakers    int       `toml:"speakers"`       // 0: the diarizer estimates
	Test        bool      `toml:"test,omitempty"` // a test recording: the drain never ingests it
	Apps        []string  `toml:"apps"`           // tapped bundle ids
	Started     time.Time `toml:"started"`
	Stopped     time.Time `toml:"stopped,omitempty"` // zero while recording
	DurationS   float64   `toml:"duration_s"`
	Interrupted bool      `toml:"interrupted,omitempty"` // the recorder died mid-take; serve repaired the tracks
	Error       string    `toml:"error,omitempty"`       // why the take ended on its own
	UploadedAt  time.Time `toml:"uploaded_at,omitempty"` // the record reached uploaded; the local copy goes meeting.upload.retention_days later
	Capture     Capture   `toml:"capture"`
	Tracks      []Track   `toml:"tracks"`
}

// Capture is what the device reported.
type Capture struct {
	Mic         string    `toml:"mic"`
	MicUID      string    `toml:"mic_uid"`
	InputRate   float64   `toml:"input_rate"`
	FirstSample time.Time `toml:"first_sample,omitempty"` // wall clock of sample 0, to one device block
	PaddedS     float64   `toml:"padded_s"`               // remote audio the device did not deliver, filled with silence
	Overruns    int       `toml:"overruns"`               // device blocks dropped from both tracks
}

// Track is one WAV under tracks/.
type Track struct {
	Name    string `toml:"name"`
	Role    string `toml:"role"` // remote | mic
	File    string `toml:"file"` // relative to the recording's directory
	Samples int    `toml:"samples"`
	Bytes   int64  `toml:"bytes"`
}

func metaPath(dir string) string { return filepath.Join(dir, "meta.toml") }

func readMeta(dir string) (Meta, error) {
	var m Meta
	_, err := toml.DecodeFile(metaPath(dir), &m)
	return m, err
}

// writeMeta replaces meta.toml whole: a crash leaves the old or the new one.
func writeMeta(dir string, m Meta) error {
	var b bytes.Buffer
	if err := toml.NewEncoder(&b).Encode(m); err != nil {
		return err
	}
	tmp := metaPath(dir) + ".tmp"
	if err := os.WriteFile(tmp, b.Bytes(), 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, metaPath(dir))
}
