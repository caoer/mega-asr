package audio

import (
	"crypto/sha256"
	"encoding/hex"
	"os"
	"testing"
)

// chunkerSources is the sha256 of chunk.go, level.go and trim.go, in that
// order, for each ChunkerVersion. A change to the cuts takes a new version
// and a new row; an edit to comments alone re-pins its version's row.
var chunkerSources = map[int]string{
	1: "bd5d05dd91e87ce5bf2dcd6bf60401f3a54ae490672ddca60a15db1769c17f46",
}

// The chunker's source is the one its version names.
func TestChunkerVersionPinsSource(t *testing.T) {
	h := sha256.New()
	for _, f := range []string{"chunk.go", "level.go", "trim.go"} {
		b, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		h.Write(b)
	}
	got := hex.EncodeToString(h.Sum(nil))
	want, ok := chunkerSources[ChunkerVersion]
	if !ok || got != want {
		t.Fatalf("chunk.go, level.go and trim.go hash %s, but ChunkerVersion %d pins %q: a change to the cuts takes ChunkerVersion %d and a row for it",
			got, ChunkerVersion, want, ChunkerVersion+1)
	}
}
