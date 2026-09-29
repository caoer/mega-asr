package main

import (
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/caoer/mega-asr/internal/audio"
	"github.com/caoer/mega-asr/internal/session"
)

// fixtureTake is 30 s of read speech, the chunker's own test take.
const fixtureTake = "../../internal/audio/testdata/take-30s.wav"

// splitRows runs `asrbench split` and returns cuts.tsv's rows as
// start, end, start_reason, end_reason, checking each chunk WAV against s.
func splitRows(t *testing.T, s []int16, wav string, flags ...string) [][4]string {
	t.Helper()
	dir := t.TempDir()
	if err := split(append(flags, dir, wav)); err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(filepath.Join(dir, "cuts.tsv"))
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSpace(string(b)), "\n")
	if lines[0] != "chunk\tstart\tend\tstart_s\tend_s\tlen_s\tstart_reason\tend_reason\treason" {
		t.Fatalf("header %q", lines[0])
	}
	var rows [][4]string
	for _, l := range lines[1:] {
		f := strings.Split(l, "\t")
		start, _ := strconv.Atoi(f[1])
		end, _ := strconv.Atoi(f[2])
		if c, err := audio.ReadWAV(filepath.Join(dir, f[0])); err != nil || !slices.Equal(c, s[start:end]) {
			t.Errorf("%s is not the take's samples [%d, %d) (%v)", f[0], start, end, err)
		}
		rows = append(rows, [4]string{f[1], f[2], f[6], f[7]})
	}
	return rows
}

func row(start, end int, from, to string) [4]string {
	return [4]string{strconv.Itoa(start), strconv.Itoa(end), from, to}
}

// split -whole D keeps a take as megavoice's controller does: session.Whole
// decides — a take exactly D long is whole — and the one chunk is the take's
// audio.TrimBounds, both edges "whole".
func TestSplitWholeIsSessionWhole(t *testing.T) {
	s, err := audio.ReadWAV(fixtureTake)
	if err != nil {
		t.Fatal(err)
	}
	lo, hi := audio.TrimBounds(s)
	if lo == 0 && hi == len(s) {
		t.Fatal("the fixture needs a quiet head or tail to trim")
	}
	if !session.Whole(len(s), 30*time.Second) || session.Whole(len(s), 29*time.Second) {
		t.Fatal("the fixture is not exactly 30 s")
	}
	for _, d := range []string{"30s", "1m"} {
		if got, want := splitRows(t, s, fixtureTake, "-whole", d), [][4]string{row(lo, hi, audio.EdgeWhole, audio.EdgeWhole)}; !slices.Equal(got, want) {
			t.Errorf("-whole %s: %v, want %v", d, got, want)
		}
	}
	if got := splitRows(t, s, fixtureTake, "-whole", "29s"); len(got) < 2 || got[0][2] != audio.EdgeStart {
		t.Errorf("-whole 29s on a 30 s take: %v, want the chunker's cuts", got)
	}
}

// Past -whole, split writes audio.Chunker's cuts, fed as the live stream
// feeds it, with both edges' kinds: the first chunk starts at "start", each
// next one at the kind the one before ended at, and the last ends at "tail".
func TestSplitEdgesAreTheChunkersCuts(t *testing.T) {
	s, err := audio.ReadWAV(fixtureTake)
	if err != nil {
		t.Fatal(err)
	}
	c := audio.Chunker{Pause: 800 * time.Millisecond, Max: 5 * time.Second}
	var want [][4]string
	start, from := 0, audio.EdgeStart
	for i := 0; i < len(s); i += audio.Block {
		for _, cut := range c.Push(s[i:min(i+audio.Block, len(s))]) {
			want = append(want, row(start, cut.End, from, cut.Kind))
			start, from = cut.End, cut.Kind
		}
	}
	want = append(want, row(start, len(s), from, audio.EdgeTail))
	if !slices.ContainsFunc(want, func(r [4]string) bool { return r[3] == audio.EdgeMax }) ||
		!slices.ContainsFunc(want, func(r [4]string) bool { return r[3] == audio.EdgePause }) {
		t.Fatalf("the fixture's cuts %v need both a pause and a max edge", want)
	}
	got := splitRows(t, s, fixtureTake, "-pause", "800", "-max", "5", "-min", "3", "-margin", "8", "-whole", "10s")
	if !slices.Equal(got, want) {
		t.Fatalf("cuts %v, want %v", got, want)
	}
}
