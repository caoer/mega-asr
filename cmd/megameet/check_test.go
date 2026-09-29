package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestCheckDir(t *testing.T) {
	dir := t.TempDir()
	w := func(name, body string) {
		os.MkdirAll(filepath.Dir(filepath.Join(dir, name)), 0o700)
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	w("tracks/mic.flac", "mic")
	w("tracks/remote.flac", "remote")
	sum, _, _ := fileSHA(filepath.Join(dir, "tracks/mic.flac"))
	w("meta.toml", `id = "20200815-101500-mac-host-a"
source = "mac"
started = 2020-08-15T10:15:00Z
[[tracks]]
role = "mic"
file = "tracks/mic.flac"
sha256 = "`+sum+`"
bytes = 3
[[tracks]]
role = "remote"
file = "tracks/remote.flac"
`)
	w("rec.json", "{}")
	w("segments.json", `[{"track":"mic","start_s":0,"text":"hi"}]`)
	w("transcript.md", "00:00:00 mic: hi\n")
	if s, missing := checkDir(dir); len(missing) != 0 || !strings.Contains(s, "2 track(s), 1 segment(s)") {
		t.Fatalf("%s %q", s, missing)
	}
	os.Remove(filepath.Join(dir, "tracks/remote.flac"))
	w("tracks/mic.flac", "MIC")
	_, missing := checkDir(dir)
	if len(missing) != 2 || !strings.Contains(missing[0], "sha256") || !strings.Contains(missing[1], "tracks/remote.flac (track remote)") {
		t.Fatalf("%q", missing)
	}
}
