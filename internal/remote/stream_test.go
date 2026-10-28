package remote

import (
	"bytes"
	"context"
	"encoding/hex"
	"encoding/json"
	"flag"
	"os"
	"reflect"
	"strings"
	"testing"
	"time"
)

var updateStream = flag.Bool("update-stream", false, "regenerate testdata/stream-records.json")

type streamVector struct {
	Name     string  `json:"name"`
	Stream   string  `json:"stream,omitempty"`
	Sequence uint32  `json:"sequence"`
	Last     bool    `json:"last"`
	Samples  []int16 `json:"samples,omitempty"`
	Hex      string  `json:"hex"`
	Reject   bool    `json:"reject"`
}

type streamVectors struct {
	Generator string         `json:"generator"`
	Format    string         `json:"format"`
	Vectors   []streamVector `json:"vectors"`
}

func fullStreamSamples() []int16 {
	s := make([]int16, StreamSamples)
	values := []int16{-32768, -1, 0, 1, 32767}
	for i := range s {
		s[i] = values[i%len(values)]
	}
	return s
}

func makeStreamVectors(t *testing.T) streamVectors {
	t.Helper()
	v := streamVectors{Generator: "go test ./internal/remote -run TestStreamVectors -update-stream", Format: StreamFormat}
	for _, c := range []struct {
		name   string
		record StreamRecord
	}{
		{"full", StreamRecord{Stream: 0x0123456789abcdef, Samples: fullStreamSamples()}},
		{"short-last", StreamRecord{Stream: 0x0123456789abcdef, Sequence: 45, Last: true, Samples: []int16{-32768, -1, 0, 1, 32767}}},
		{"empty-last", StreamRecord{Stream: 0, Sequence: 45, Last: true}},
		{"maximum-fields", StreamRecord{Stream: ^uint64(0), Sequence: ^uint32(0), Last: true, Samples: fullStreamSamples()}},
	} {
		b, err := c.record.Encode()
		if err != nil {
			t.Fatal(err)
		}
		v.Vectors = append(v.Vectors, streamVector{Name: c.name, Stream: hex.EncodeToString(b[1:9]), Sequence: c.record.Sequence,
			Last: c.record.Last, Samples: c.record.Samples, Hex: hex.EncodeToString(b)})
	}
	full, _ := hex.DecodeString(v.Vectors[0].Hex)
	short, _ := hex.DecodeString(v.Vectors[1].Hex)
	for _, name := range []string{"truncated-header", "odd-pcm", "oversize", "unknown-kind", "unknown-flags", "short-without-last"} {
		b := append([]byte(nil), full...)
		switch name {
		case "truncated-header":
			b = b[:13]
		case "odd-pcm":
			b = b[:len(b)-1]
		case "oversize":
			b = append(b, 0, 0)
		case "unknown-kind":
			b[0] = 3
		case "unknown-flags":
			b[13] = 2
		case "short-without-last":
			b = append([]byte(nil), short...)
			b[13] = 0
		}
		v.Vectors = append(v.Vectors, streamVector{Name: name, Hex: hex.EncodeToString(b), Reject: true})
	}
	return v
}

// The fixture lives once beside Go; Swift reads this exact generated file.
func TestStreamVectors(t *testing.T) {
	want, err := json.MarshalIndent(makeStreamVectors(t), "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	want = append(want, '\n')
	const path = "testdata/stream-records.json"
	if *updateStream {
		if err := os.MkdirAll("testdata", 0755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, want, 0644); err != nil {
			t.Fatal(err)
		}
	}
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, want) {
		t.Fatal("stream vectors differ: regenerate with -update-stream")
	}
	var vectors streamVectors
	if err := json.Unmarshal(got, &vectors); err != nil {
		t.Fatal(err)
	}
	for _, v := range vectors.Vectors {
		t.Run(v.Name, func(t *testing.T) {
			b, err := hex.DecodeString(v.Hex)
			if err != nil {
				t.Fatal(err)
			}
			r, err := DecodeStreamRecord(b)
			if v.Reject {
				if err == nil {
					t.Fatal("accepted malformed vector")
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			id, err := ParseStreamID(v.Stream)
			if err != nil {
				t.Fatal(err)
			}
			if r.Stream != id || r.Sequence != v.Sequence || r.Last != v.Last || !reflect.DeepEqual(r.Samples, append([]int16{}, v.Samples...)) {
				t.Fatal("decoded record differs")
			}
			encoded, err := r.Encode()
			if err != nil || !bytes.Equal(b, encoded) {
				t.Fatal("round trip differs", err)
			}
		})
	}
}

func TestStreamIDsAndEncoderBounds(t *testing.T) {
	for _, s := range []string{"", "abcdef", "0123456789abcdeg", "+123456789abcdef", "0123456789abcdef00"} {
		if _, err := ParseStreamID(s); err == nil {
			t.Fatalf("accepted %q", s)
		}
	}
	if id, err := ParseStreamID("FEDCBA9876543210"); err != nil || id != 0xfedcba9876543210 {
		t.Fatal(id, err)
	}
	for _, r := range []StreamRecord{{Samples: []int16{1}}, {Last: true, Samples: make([]int16, StreamSamples+1)}} {
		if _, err := r.Encode(); err == nil {
			t.Fatal("accepted sample count")
		}
	}
}

func recordBytes(t *testing.T, seq uint32, last bool, samples []int16) []byte {
	t.Helper()
	b, err := (StreamRecord{Stream: 7, Sequence: seq, Last: last, Samples: samples}).Encode()
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func startPhoneStream(t *testing.T) (*PhoneStream, <-chan []int16) {
	t.Helper()
	p := NewPhoneStream(7, "iPhone fixture")
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	t.Cleanup(p.Stop)
	ch, err := p.Start(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if p.Input().Source != "phone" || p.Input().Name != "iPhone fixture" {
		t.Fatal(p.Input())
	}
	return p, ch
}

func drainPhoneStream(t *testing.T, ch <-chan []int16) []int16 {
	t.Helper()
	var samples []int16
	timeout := time.After(5 * time.Second)
	for {
		select {
		case s, ok := <-ch:
			if !ok {
				return samples
			}
			samples = append(samples, s...)
		case <-timeout:
			t.Fatal("phone stream did not close")
			return nil
		}
	}
}

func TestPhoneStreamFourAndAHalfSecondsAndLastTail(t *testing.T) {
	for _, tail := range [][]int16{nil, {-32768, 32767, 1}} {
		p, ch := startPhoneStream(t)
		for seq := uint32(0); seq < 45; seq++ {
			if err := p.Feed(recordBytes(t, seq, false, fullStreamSamples())); err != nil {
				t.Fatal(err)
			}
		}
		if err := p.Feed(recordBytes(t, 45, true, tail)); err != nil {
			t.Fatal(err)
		}
		if err := p.Finish(45); err != nil {
			t.Fatal(err)
		}
		got := drainPhoneStream(t, ch)
		if len(got) != 45*StreamSamples+len(tail) || p.Err() != nil {
			t.Fatal(len(got), p.Err())
		}
		if !reflect.DeepEqual(got[:StreamSamples], fullStreamSamples()) {
			t.Fatal("PCM changed")
		}
		if _, err := p.Start(context.Background()); err == nil {
			t.Fatal("restarted a closed source")
		}
	}
}

func TestPhoneStreamRefusesGapsRepeatsIDsOversizeAndFlagMismatch(t *testing.T) {
	for _, kind := range []string{"gap", "repeat", "id", "oversize", "last-mismatch"} {
		t.Run(kind, func(t *testing.T) {
			p, ch := startPhoneStream(t)
			b := recordBytes(t, 0, false, fullStreamSamples())
			switch kind {
			case "gap":
				b = recordBytes(t, 1, false, fullStreamSamples())
			case "repeat":
				if err := p.Feed(b); err != nil {
					t.Fatal(err)
				}
			case "id":
				b[8] = 8
			case "oversize":
				b = append(b, 0, 0)
			case "last-mismatch":
				if err := p.Finish(1); err != nil {
					t.Fatal(err)
				}
				b = recordBytes(t, 0, true, []int16{1})
			}
			if err := p.Feed(b); err == nil {
				t.Fatal("accepted invalid stream")
			}
			drainPhoneStream(t, ch)
			if p.Err() == nil {
				t.Fatal("missing source error")
			}
		})
	}
}

func TestPhoneStreamStopGraceAndStall(t *testing.T) {
	t.Run("grace-arrival", func(t *testing.T) {
		p, ch := startPhoneStream(t)
		if err := p.Finish(0); err != nil {
			t.Fatal(err)
		}
		if err := p.Finish(0); err != nil {
			t.Fatal(err)
		}
		if err := p.Feed(recordBytes(t, 0, false, fullStreamSamples())); err != nil {
			t.Fatal(err)
		}
		if len(drainPhoneStream(t, ch)) != StreamSamples || p.Err() != nil {
			t.Fatal(p.Err())
		}
	})
	t.Run("grace-missing", func(t *testing.T) {
		p, ch := startPhoneStream(t)
		if err := p.Finish(0); err != nil {
			t.Fatal(err)
		}
		drainPhoneStream(t, ch)
		if p.Err() == nil || !strings.Contains(p.Err().Error(), "last_seq") {
			t.Fatal(p.Err())
		}
	})
	t.Run("stall", func(t *testing.T) {
		p, ch := startPhoneStream(t)
		drainPhoneStream(t, ch)
		if p.Err() == nil || !strings.Contains(p.Err().Error(), "3 s") {
			t.Fatal(p.Err())
		}
	})
}

func TestPhoneStreamBackpressureAndCancellation(t *testing.T) {
	p, ch := startPhoneStream(t)
	var err error
	for seq := uint32(0); seq < 100 && err == nil; seq++ {
		err = p.Feed(recordBytes(t, seq, false, fullStreamSamples()))
	}
	if err == nil {
		t.Fatal("unbounded queue")
	}
	drainPhoneStream(t, ch)
	if p.Err() == nil {
		t.Fatal("missing overflow error")
	}
	cancelled := NewPhoneStream(7, "fixture")
	ctx, cancel := context.WithCancel(context.Background())
	out, err := cancelled.Start(ctx)
	if err != nil {
		t.Fatal(err)
	}
	cancel()
	drainPhoneStream(t, out)
	if cancelled.Err() != context.Canceled {
		t.Fatal(cancelled.Err())
	}
}
