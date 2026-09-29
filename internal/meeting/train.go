package meeting

import (
	"bufio"
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/caoer/mega-asr/internal/audio"
	"github.com/caoer/mega-asr/internal/score"
)

// TrainRow is one row of meetings-train.jsonl, in the segments.jsonl shape of
// mega-asr-loop's recipes/funasr-nano/prepare.py, so its clean.py reads it: a
// piece of the owner's mic track, labelled with Feishu's text for it.
type TrainRow struct {
	ID        string  `json:"id"`
	Clip      string  `json:"clip"` // the recording
	Source    string  `json:"source"`
	Date      string  `json:"date"`
	Wav       string  `json:"wav"`
	DurS      float64 `json:"dur_s"`
	Text      string  `json:"text"` // Feishu's
	Hyp       string  `json:"hyp"`  // the local engine's
	RefTokens int     `json:"ref_tokens"`
	Errors    int     `json:"errors"`
	Cut       bool    `json:"cut"`

	StartS float64 `json:"-"` // on the mic track
	EndS   float64 `json:"-"`
}

// Search reach for a segment's label among the owner's turns, in seconds:
// a turn that starts this long before the segment can still be running.
const (
	labelBefore = 180.0
	labelAfter  = 10.0
)

// trainRows labels each mic-track segment with the stretch of the owner's
// Feishu text that best matches it: the owner's turns near the segment,
// joined, and the local tokens aligned into them with free ends.
func trainRows(in Input, segs []Segment, turns []Turn, me string, offsetS, rate float64) []TrainRow {
	var rows []TrainRow
	n := 0
	for _, s := range segs {
		if s.Track != "mic" {
			continue
		}
		n++
		hyp := units(s.Text)
		if len(hyp) == 0 {
			continue
		}
		var text strings.Builder
		for _, t := range turns {
			if t.Speaker == me && t.StartS >= s.StartS-labelBefore && t.StartS <= s.EndS+labelAfter {
				text.WriteString(t.Text)
			}
		}
		ref := units(text.String())
		i, j, cost := locate(hyp, ref)
		if j <= i {
			continue
		}
		label := slice(text.String(), ref[i].lo, ref[j-1].hi)
		start, end := offsetS+s.StartS*rate, offsetS+s.EndS*rate
		rows = append(rows, TrainRow{
			ID: fmt.Sprintf("%s-mic-%04d", in.Rec, n), Clip: in.Rec, Source: "meeting", Date: in.Date,
			DurS: round2(end - start), Text: label, Hyp: s.Text, RefTokens: j - i, Errors: cost,
			StartS: start, EndS: end,
		})
	}
	return rows
}

func round2(x float64) float64 { return float64(int(x*100+0.5)) / 100 }

type unit struct {
	tok    string
	lo, hi int // byte offsets in the text
}

// units splits text as mega-asr-loop's recipes/funasr-nano/prepare.py does: a Han/kana/hangul character
// is one token, a run of letters or digits is one lowercased word; fillers
// are left out.
func units(text string) []unit {
	var out []unit
	fill := map[string]bool{"嗯": true, "呃": true, "um": true, "uh": true, "uhm": true, "erm": true, "hmm": true, "mm": true, "mhm": true}
	for i := 0; i < len(text); {
		r, w := utf8.DecodeRuneInString(text[i:])
		switch {
		case score.IsCJK(r):
			if !fill[string(r)] {
				out = append(out, unit{string(r), i, i + w})
			}
			i += w
		case unicode.IsLetter(r) || unicode.IsDigit(r):
			j := i
			for j < len(text) {
				r2, w2 := utf8.DecodeRuneInString(text[j:])
				if score.IsCJK(r2) || !(unicode.IsLetter(r2) || unicode.IsDigit(r2) || r2 == '\'') {
					break
				}
				j += w2
			}
			if t := strings.ToLower(text[i:j]); !fill[t] {
				out = append(out, unit{t, i, j})
			}
			i = j
		default:
			i += w
		}
	}
	return out
}

// locate aligns all of hyp into ref with free ends on the ref side and
// returns the ref tokens [i, j) it spans and the edit cost.
func locate(hyp, ref []unit) (i, j, cost int) {
	n, m := len(hyp), len(ref)
	if m == 0 {
		return 0, 0, n
	}
	d := make([][]int32, n+1)
	for a := range d {
		d[a] = make([]int32, m+1)
		d[a][0] = int32(a)
	}
	for a := 1; a <= n; a++ {
		for b := 1; b <= m; b++ {
			c := int32(1)
			if hyp[a-1].tok == ref[b-1].tok {
				c = 0
			}
			d[a][b] = min(d[a-1][b-1]+c, d[a-1][b]+1, d[a][b-1]+1)
		}
	}
	j = 0
	for b := 1; b <= m; b++ {
		if d[n][b] < d[n][j] {
			j = b
		}
	}
	cost = int(d[n][j])
	// Walk back; of equal-cost steps a match comes first, then skipping a
	// reference token, so a tie at the start keeps the earlier match.
	a, b := n, j
	for a > 0 && b > 0 {
		switch {
		case hyp[a-1].tok == ref[b-1].tok && d[a][b] == d[a-1][b-1]:
			a, b = a-1, b-1
		case d[a][b] == d[a][b-1]+1:
			b--
		case d[a][b] == d[a-1][b-1]+1:
			a, b = a-1, b-1
		default:
			a--
		}
	}
	return b, j, cost
}

// slice is text[lo:hi] with the punctuation that closes it.
func slice(text string, lo, hi int) string {
	for hi < len(text) {
		r, w := utf8.DecodeRuneInString(text[hi:])
		if !strings.ContainsRune("，。、；：？！,.;:?!…", r) {
			break
		}
		hi += w
	}
	return text[lo:hi]
}

// WriteTrain cuts each row's piece of the mic track into dir/clips/<rec>/
// and puts the rows into dir/meetings-train.jsonl in place of the
// recording's earlier ones. The owner's voice only; both stay private
// (0700 folders, 0600 files).
func WriteTrain(ctx context.Context, dir, rec, mic string, rows []TrainRow) error {
	clips := filepath.Join(dir, "clips", rec)
	if err := os.RemoveAll(clips); err != nil {
		return err
	}
	if len(rows) > 0 {
		if err := os.MkdirAll(clips, 0o700); err != nil {
			return err
		}
		pcm, err := decodePCM(ctx, mic)
		if err != nil {
			return err
		}
		for i := range rows {
			r := &rows[i]
			a, b := int(r.StartS*audio.Rate), int(r.EndS*audio.Rate)
			a, b = max(0, min(a, len(pcm))), max(0, min(b, len(pcm)))
			r.Wav = filepath.Join(clips, r.ID+".wav")
			if err := audio.SaveWAV(r.Wav, pcm[a:b]); err != nil {
				return err
			}
			if err := os.Chmod(r.Wav, 0o600); err != nil {
				return err
			}
		}
	}
	path := filepath.Join(dir, "meetings-train.jsonl")
	var out bytes.Buffer
	if f, err := os.Open(path); err == nil {
		sc := bufio.NewScanner(f)
		sc.Buffer(make([]byte, 1<<20), 16<<20)
		for sc.Scan() {
			var r struct {
				Clip string `json:"clip"`
			}
			if json.Unmarshal(sc.Bytes(), &r) == nil && r.Clip == rec {
				continue
			}
			out.Write(sc.Bytes())
			out.WriteByte('\n')
		}
		f.Close()
		if err := sc.Err(); err != nil {
			return err
		}
	} else if !os.IsNotExist(err) {
		return err
	}
	enc := json.NewEncoder(&out)
	enc.SetEscapeHTML(false)
	for _, r := range rows {
		if err := enc.Encode(r); err != nil {
			return err
		}
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, out.Bytes(), 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// decodePCM decodes any file ffmpeg reads to 16 kHz mono samples.
func decodePCM(ctx context.Context, path string) ([]int16, error) {
	cmd := exec.CommandContext(ctx, "ffmpeg", "-nostdin", "-loglevel", "error", "-i", path, "-vn", "-ac", "1", "-ar", fmt.Sprint(audio.Rate), "-f", "s16le", "-")
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	b, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("ffmpeg %s: %v: %s", path, err, strings.TrimSpace(stderr.String()))
	}
	s := make([]int16, len(b)/2)
	for i := range s {
		s[i] = int16(binary.LittleEndian.Uint16(b[2*i:]))
	}
	return s, nil
}
