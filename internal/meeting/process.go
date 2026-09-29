package meeting

import (
	"context"
	"fmt"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"time"
	"unicode"

	"github.com/caoer/mega-asr/internal/asr"
	"github.com/caoer/mega-asr/internal/audio"
	"github.com/caoer/mega-asr/internal/post"
	"github.com/caoer/mega-asr/internal/session"
	"github.com/caoer/mega-asr/internal/store"
)

// Processor turns a pulled directory's tracks into segments.json and
// transcript.md. Each track runs through its own session.Controller, fed
// from the decoded WAV at full speed; the controllers share one ASR.
type Processor struct {
	ASR     asr.Transcriber
	Post    post.Chain
	Chunker func() session.Chunker // nil: audio.Chunker's defaults
	Engine  string                 // the segments' engine, e.g. funasr
	Root    string                 // the engine root, recorded on each segment
	Out     string                 // where segments.json and transcript.md go; "" is the pulled directory
	Logf    func(format string, args ...any)
}

// Outcome is what a run produced and measured.
type Outcome struct {
	Segments  []Segment
	AudioS    float64 // seconds of audio transcribed, every track
	WallS     float64 // decode to transcript
	Chunks    int     // pieces sent to the ASR
	Failed    int     // pieces the ASR failed on
	Loops     int     // pieces whose final text repeats a run more than loopOver times
	Redecoded int     // pieces whose ASR text repeated a run more than redecodeOver times, decoded again as two halves
	Echoes    int     // mic segments the echo gate dropped
}

// RTF is the processing time per second of audio.
func (o Outcome) RTF() float64 {
	if o.AudioS == 0 {
		return 0
	}
	return o.WallS / o.AudioS
}

// speechRoles are the tracks that are transcribed, in this order.
var speechRoles = []string{"remote", "mic", "beam", "media"}

// track is one track on its way through the ASR.
type track struct {
	role, speaker, wav string
	chunks             []session.Chunk
	samples            []int16 // read when a piece is decoded again
}

// Process transcribes the tracks of the record pulled into dir and applies
// its source's stages: mac — track = speaker and the echo gate; feishu —
// speakers from Feishu's turns by time overlap (Feishu's transcript stays
// canonical; this one is the second engine, cut by its own pauses); phone
// and file — one speaker. Every source gets the loop re-decode.
func (p *Processor) Process(ctx context.Context, dir string) (Outcome, error) {
	t0 := time.Now()
	var o Outcome
	rec, err := ReadPulled(dir)
	if err != nil {
		return o, err
	}
	work := filepath.Join(dir, "work")
	if err := os.RemoveAll(work); err != nil {
		return o, err
	}
	defer os.RemoveAll(work)
	var tracks []*track
	longest := 0 // samples in the longest decoded track
	for _, role := range speechRoles {
		f, ok := rec.File(role)
		if !ok {
			continue
		}
		rel, _ := pulledPath(f.Role, f.Codec)
		t := &track{role: role, speaker: trackSpeaker(rec, role), wav: filepath.Join(work, role+".wav")}
		if err := decode(ctx, filepath.Join(dir, rel), t.wav); err != nil {
			return o, err
		}
		st, err := os.Stat(t.wav)
		if err != nil {
			return o, err
		}
		longest = max(longest, int(st.Size()-44)/2) // decode's canonical 44-byte header
		tracks = append(tracks, t)
	}
	if len(tracks) == 0 && rec.Source != "feishu" { // a minute whose export was denied has only Feishu's transcript
		return o, fmt.Errorf("%s: no track to transcribe", rec.ID)
	}
	if len(tracks) > 0 && audio.TooShort(longest) { // e.g. a Feishu recording stopped as it started
		return o, fmt.Errorf("%s: %.1f s of audio in %d track(s), too short to hold speech", rec.ID, float64(longest)/audio.Rate, len(tracks))
	}
	var wg sync.WaitGroup
	for _, t := range tracks {
		wg.Add(1)
		go func() {
			defer wg.Done()
			t.chunks = p.transcribe(ctx, t.wav, filepath.Join(work, t.role))
		}()
	}
	wg.Wait()
	if err := ctx.Err(); err != nil { // stopped mid-record: the pieces left were not decoded
		return o, fmt.Errorf("%s: %w", rec.ID, err)
	}
	for _, t := range tracks {
		for _, ck := range t.chunks {
			o.Chunks++
			o.AudioS += ck.Length.Seconds()
			if ck.Err != nil {
				o.Failed++
				continue
			}
			raw, text := ck.Raw, ck.Text
			if n := Repeats(raw); n > redecodeOver {
				o.Redecoded++
				if r, err := p.redecode(ctx, t, ck, work); err != nil {
					p.logf("process %s: %s at %.1f s: a run repeated %d times; re-decode: %v", rec.ID, t.role, ck.Offset.Seconds(), n, err)
				} else {
					raw, text = r, p.Post.Apply(r)
					p.logf("process %s: %s at %.1f s: a run repeated %d times; as two halves, %d", rec.ID, t.role, ck.Offset.Seconds(), n, Repeats(raw))
				}
			}
			if Repeats(text) > loopOver {
				o.Loops++
			}
			if strings.TrimSpace(raw) == "" {
				continue
			}
			o.Segments = append(o.Segments, Segment{Track: t.role, Speaker: t.speaker,
				StartS: hundredths(ck.Offset.Seconds()), EndS: hundredths((ck.Offset + ck.Length).Seconds()),
				Raw: raw, Text: text, Engine: p.Engine, Root: p.Root})
		}
	}
	if o.Chunks > 0 && o.Failed == o.Chunks {
		return o, fmt.Errorf("%s: the ASR failed on every piece (%d)", rec.ID, o.Chunks)
	}
	switch rec.Source {
	case "mac":
		o.Segments, o.Echoes = DropEchoes(o.Segments)
	case "feishu":
		turns, err := ReadTurns(filepath.Join(dir, "feishu-transcript.json"))
		if err != nil {
			return o, fmt.Errorf("feishu speakers: %w", err)
		}
		LabelByTurns(o.Segments, turns)
	}
	slices.SortStableFunc(o.Segments, func(a, b Segment) int {
		if a.StartS != b.StartS {
			return cmpFloat(a.StartS, b.StartS)
		}
		return strings.Compare(a.Track, b.Track)
	})
	out := dir
	if p.Out != "" {
		out = p.Out
		if err := os.MkdirAll(out, 0o700); err != nil {
			return o, err
		}
	}
	if err := WriteSegments(filepath.Join(out, "segments.json"), o.Segments); err != nil {
		return o, err
	}
	if err := WriteTranscript(filepath.Join(out, "transcript.md"), o.Segments); err != nil {
		return o, err
	}
	o.WallS = time.Since(t0).Seconds()
	p.logf("process %s: %d tracks, %.0f s of audio in %.0f s (RTF %.3f): %d pieces, %d failed, %d re-decoded, %d loops, %d echoes dropped, %d segments",
		rec.ID, len(tracks), o.AudioS, o.WallS, o.RTF(), o.Chunks, o.Failed, o.Redecoded, o.Loops, o.Echoes, len(o.Segments))
	return o, nil
}

// transcribe runs one track through a controller at full speed and returns
// its pieces; a track too short to hold speech has none. Once ctx is done,
// the ASR call in flight returns and the pieces after it are not sent.
func (p *Processor) transcribe(ctx context.Context, wav, work string) []session.Chunk {
	var got []session.Chunk
	c := &session.Controller{
		Ctx:        ctx,
		NewSource:  func() audio.Source { return &audio.File{Path: wav, Speed: math.Inf(1)} },
		NewChunker: p.Chunker,
		ASR:        p.ASR,
		Post:       p.Post,
		Deliver:    discard{},
		UI:         discard{},
		Store:      store.Store{Dir: work},
		ChunkDir:   work + "-chunks",
		OnChunks:   func(_ string, cs []session.Chunk) { got = cs },
	}
	c.Toggle()
	c.Wait()
	return got
}

// redecode transcribes a looping piece again as two halves, cut at the
// quietest 20 ms block of its middle third.
func (p *Processor) redecode(ctx context.Context, t *track, ck session.Chunk, work string) (string, error) {
	if t.samples == nil {
		s, err := audio.ReadWAV(t.wav)
		if err != nil {
			return "", err
		}
		t.samples = s
	}
	lo := min(int(ck.Offset.Seconds()*audio.Rate), len(t.samples))
	hi := min(lo+int(ck.Length.Seconds()*audio.Rate), len(t.samples))
	s := t.samples[lo:hi]
	cut := quietest(s, len(s)/3, 2*len(s)/3)
	var parts []string
	for i, half := range [][]int16{s[:cut], s[cut:]} {
		path := filepath.Join(work, fmt.Sprintf("redecode-%s-%d.wav", t.role, i))
		if err := audio.SaveWAV(path, half); err != nil {
			return "", err
		}
		text, err := p.ASR.Transcribe(ctx, path, false)
		os.Remove(path)
		if err != nil {
			return "", err
		}
		parts = append(parts, text)
	}
	return session.Join(parts), nil
}

// quietest is the start of the quietest 20 ms block of s[from:to], or the
// middle of s when the span holds no whole block.
func quietest(s []int16, from, to int) int {
	best, bestE := len(s)/2, math.Inf(1)
	for i := from - from%audio.Block; i+audio.Block <= to; i += audio.Block {
		if i < from {
			continue
		}
		var e float64
		for _, v := range s[i : i+audio.Block] {
			e += float64(v) * float64(v)
		}
		if e < bestE {
			best, bestE = i, e
		}
	}
	return best
}

const (
	// redecodeOver: a piece whose ASR text repeats a run more often than
	// this is decoded again as two halves.
	redecodeOver = 4
	// loopOver: a piece whose final text still repeats a run more often
	// than this is a loop; a short stutter stays under it.
	loopOver = 5
)

// Repeats is the most copies, back to back, of any run of 1–16 tokens (a
// Han character or a Latin word each) in text; 1 when nothing repeats.
func Repeats(text string) int {
	t := loopTokens(text)
	most := min(len(t), 1)
	for n := 1; n <= 16 && 2*n <= len(t); n++ {
		same := 0
		for i := n; i < len(t); i++ {
			if t[i] != t[i-n] {
				same = 0
				continue
			}
			same++
			most = max(most, 1+same/n)
		}
	}
	return most
}

func loopTokens(text string) []string {
	var out []string
	var word []rune
	flush := func() {
		if len(word) > 0 {
			out = append(out, string(word))
			word = word[:0]
		}
	}
	for _, r := range strings.ToLower(text) {
		switch {
		case unicode.In(r, unicode.Han, unicode.Hiragana, unicode.Katakana, unicode.Hangul):
			flush()
			out = append(out, string(r))
		case unicode.IsLetter(r) || unicode.IsDigit(r) || r == '\'':
			word = append(word, r)
		default:
			flush()
		}
	}
	flush()
	return out
}

// trackSpeaker names a track's speaker: the record's speaker with the track's
// role, else the owner for the mic, else the first named speaker when the
// track holds everyone (media, beam), else the role.
func trackSpeaker(rec Record, role string) string {
	for _, s := range rec.Speakers {
		if s.Role == role && s.Name != "" {
			return s.Name
		}
	}
	switch {
	case role == "mic" && rec.Owner != "":
		return rec.Owner
	case role == "media" || role == "beam":
		for _, s := range rec.Speakers {
			if s.Name != "" {
				return s.Name
			}
		}
		return "Speaker 1"
	}
	return role
}

// LabelByTurns names each segment's speaker after the Feishu turn it
// overlaps most; a turn runs from its start to the next turn's start. A
// segment that overlaps none takes the turn before it.
func LabelByTurns(segs []Segment, turns []Turn) {
	if len(turns) == 0 {
		return
	}
	for i := range segs {
		s := &segs[i]
		best, most := -1, 0.0
		for j, t := range turns {
			end := math.Inf(1)
			if j+1 < len(turns) {
				end = turns[j+1].StartS
			}
			if ov := math.Min(s.EndS, end) - math.Max(s.StartS, t.StartS); ov > most {
				best, most = j, ov
			}
			if t.StartS <= s.StartS && best < 0 {
				best = j // the turn before, unless an overlap is found
			}
		}
		if best < 0 {
			best = 0
		}
		s.Speaker = turns[best].Speaker
	}
}

// decode writes a 16 kHz mono WAV of any media file ffmpeg reads.
func decode(ctx context.Context, in, out string) error {
	if err := os.MkdirAll(filepath.Dir(out), 0o700); err != nil {
		return err
	}
	// -bitexact and no metadata: the canonical 44-byte header audio.ReadWAV reads
	cmd := exec.CommandContext(ctx, "ffmpeg", "-nostdin", "-loglevel", "error", "-y", "-i", in,
		"-vn", "-ac", "1", "-ar", "16000", "-c:a", "pcm_s16le", "-map_metadata", "-1", "-bitexact", "-f", "wav", out)
	if b, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("ffmpeg %s: %v %s", filepath.Base(in), err, strings.TrimSpace(string(b)))
	}
	return nil
}

func (p *Processor) logf(format string, args ...any) {
	if p.Logf != nil {
		p.Logf(format, args...)
	}
}

func hundredths(x float64) float64 { return math.Round(x*100) / 100 }

func cmpFloat(a, b float64) int {
	switch {
	case a < b:
		return -1
	case a > b:
		return 1
	}
	return 0
}

// discard is the controller's delivery and view: a meeting's text goes to
// its segments, not into an app.
type discard struct{}

type noTarget struct{}

func (noTarget) String() string                      { return "meeting" }
func (discard) Capture() session.Target              { return noTarget{} }
func (discard) Deliver(session.Target, string) error { return nil }
func (discard) Submit(session.Target) error          { return nil }
func (discard) Show(session.View)                    {}
func (discard) Flash(string)                         {}
func (discard) Alert(string)                         {}
