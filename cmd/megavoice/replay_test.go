package main

import (
	"context"
	"encoding/csv"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"testing"
	"time"

	"github.com/caoer/mega-asr/internal/app"
	"github.com/caoer/mega-asr/internal/audio"
	"github.com/caoer/mega-asr/internal/post"
)

// fixtureTake is 30 s of dictation, the chunker's own test take.
const fixtureTake = "../../internal/audio/testdata/take-30s.wav"

// nameASR answers each chunk with its file name.
type nameASR struct{}

func (nameASR) Transcribe(_ context.Context, wav string, whole bool) (string, error) {
	return fmt.Sprintf("%s whole=%v", filepath.Base(wav), whole), nil
}

// edge is one chunk: samples [start, end) of the take and its edges' kinds.
type edge struct {
	start, end int
	from, to   string
}

// chunkerEdges is what audio.Chunker cuts s into, fed in 20 ms blocks, with
// the tail.
func chunkerEdges(s []int16, c audio.Chunker) []edge {
	var out []edge
	start, from := 0, audio.EdgeStart
	for i := 0; i < len(s); i += audio.Block {
		for _, cut := range c.Push(s[i:min(i+audio.Block, len(s))]) {
			out = append(out, edge{start, cut.End, from, cut.Kind})
			start, from = cut.End, cut.Kind
		}
	}
	if start < len(s) {
		out = append(out, edge{start, len(s), from, audio.EdgeTail})
	}
	return out
}

// replay -chunks writes the chunks the controller decoded. On the fixture
// take their sample offsets are audio.Chunker's cuts at the config's pause
// and max, their edges the cuts' kinds, and each WAV the take's samples
// between them; within take.whole_max it is one chunk at audio.TrimBounds,
// decoded whole.
func TestReplayChunksAreTheChunkersCuts(t *testing.T) {
	s, err := audio.ReadWAV(fixtureTake)
	if err != nil {
		t.Fatal(err)
	}
	lo, hi := audio.TrimBounds(s)
	cut := chunkerEdges(s, audio.Chunker{Pause: 800 * time.Millisecond, Max: 5 * time.Second})
	if !slices.ContainsFunc(cut, func(e edge) bool { return e.to == audio.EdgeMax }) ||
		!slices.ContainsFunc(cut, func(e edge) bool { return e.to == audio.EdgePause }) {
		t.Fatalf("the fixture's cuts %v need both a pause and a max edge", cut)
	}
	for _, c := range []struct {
		name     string
		wholeMax time.Duration
		want     []edge
	}{
		{"chunked", 10 * time.Second, cut},
		{"whole", time.Minute, []edge{{lo, hi, audio.EdgeWhole, audio.EdgeWhole}}},
	} {
		t.Run(c.name, func(t *testing.T) {
			cfg := app.Default()
			cfg.Take.WholeMax = app.Duration(c.wholeMax)
			cfg.Take.ChunkPause = app.MS(800)
			cfg.Take.ChunkMax = app.Duration(5 * time.Second)
			tmp := t.TempDir()
			r := newOffline(cfg, nameASR{}, post.Chain{}, filepath.Join(tmp, "data"), "")
			dir := filepath.Join(tmp, "chunks")
			r.keepChunks(dir)
			r.take(fixtureTake, math.Inf(1))
			if r.chunkErr != nil {
				t.Fatal(r.chunkErr)
			}
			rows := readTSV(t, filepath.Join(dir, "take-30s", "chunks.tsv"))
			if len(rows) != len(c.want) {
				t.Fatalf("%d chunks, want %d: %v", len(rows), len(c.want), rows)
			}
			for i, w := range c.want {
				g := rows[i]
				start, _ := strconv.Atoi(g["start"])
				end, _ := strconv.Atoi(g["end"])
				if got := (edge{start, end, g["start_reason"], g["end_reason"]}); got != w {
					t.Errorf("chunk %d = %v, want %v", i+1, got, w)
				}
				if want := fmt.Sprintf("%03d.wav whole=%v", i+1, w.to == audio.EdgeWhole); g["raw"] != want {
					t.Errorf("chunk %d raw %q, want %q", i+1, g["raw"], want)
				}
				wav, err := audio.ReadWAV(filepath.Join(dir, "take-30s", g["chunk"]))
				if err != nil || !slices.Equal(wav, s[w.start:w.end]) {
					t.Errorf("chunk %d: %s is not the take's samples [%d, %d) (%v)", i+1, g["chunk"], w.start, w.end, err)
				}
			}
		})
	}
}

// readTSV reads a headed TSV into one map per row.
func readTSV(t *testing.T, path string) []map[string]string {
	t.Helper()
	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	cr := csv.NewReader(f)
	cr.Comma, cr.LazyQuotes = '\t', true
	recs, err := cr.ReadAll()
	if err != nil || len(recs) == 0 {
		t.Fatalf("%s: %v", path, err)
	}
	var rows []map[string]string
	for _, rec := range recs[1:] {
		m := map[string]string{}
		for i, h := range recs[0] {
			m[h] = rec[i]
		}
		rows = append(rows, m)
	}
	return rows
}
