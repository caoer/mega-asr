package compare

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"math"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"sync"
	"time"
	"unicode"

	"github.com/caoer/mega-asr/internal/audio"
	"github.com/caoer/mega-asr/internal/store"
)

// Row is one label: an asrbench manifest row ({id, wav, ref, lang, dur_s,
// source}, what `asrbench score` and the fine-tune's eval read) plus what
// the take was when it was labelled. labels.jsonl is append-only; a take's
// last row is its label.
type Row struct {
	ID          string                  `json:"id"`
	WAV         string                  `json:"wav"`
	Ref         string                  `json:"ref,omitempty"`  // the user's corrected transcript; absent with a note alone
	Lang        string                  `json:"lang,omitempty"` // zh, en or mixed, from ref
	DurS        float64                 `json:"dur_s"`
	Source      string                  `json:"source"`
	Delivered   string                  `json:"delivered"` // the take's delivered text, <id>.txt
	Primary     string                  `json:"primary,omitempty"`
	DeliveredBy string                  `json:"delivered_by,omitempty"`
	Engines     map[string]EngineResult `json:"engines,omitempty"` // from the compare record, when the take has one; without words
	Note        string                  `json:"note,omitempty"`
	// Correct are the engines whose text is the ref, ignoring spacing,
	// punctuation and case, the clicked ones first; "delivered" names the
	// delivered text of a take without a compare record.
	Correct     []string  `json:"correct,omitempty"`
	LabelSource string    `json:"label_source,omitempty"` // click, typed or cleared
	LabeledAt   time.Time `json:"labeled_at"`
}

// Label is a label as the Takes page posts it: a typed ref and/or note, or the
// engines clicked correct — the first one's text is the ref, and a typed ref
// is ignored — or a clear, which unlabels the take.
type Label struct {
	ID      string   `json:"id"`
	Ref     string   `json:"ref"`
	Note    string   `json:"note"`
	Correct []string `json:"correct"`
	Clear   bool     `json:"clear"`
}

// Label sources.
const (
	ByClick    = "click"
	ByTyping   = "typed"
	ByClearing = "cleared"
)

// Delivered names the delivered text of a take without a compare record.
const Delivered = "delivered"

// Source is a label row's source.
const Source = "megavoice-label"

// Labels is the label file.
type Labels struct {
	Path string
	Now  func() time.Time // nil: time.Now

	mu sync.Mutex
}

// takeID is a take's base name: store.Store.Next's stamp.
var takeID = regexp.MustCompile(`^\d{8}-\d{6}(-\d+)?$`)

// ErrEmpty is a label with neither a transcript nor a note.
var ErrEmpty = errors.New("compare: a label needs a corrected transcript or a note")

// ErrNoTake is a label for a take that is not in the store: a name that is
// not a take's, or a take whose WAV is gone.
var ErrNoTake = errors.New("compare: no such take")

// ErrEngine is a click on an engine the take has no answer from.
var ErrEngine = errors.New("compare: the take has no answer from that engine")

// ErrBackup is a reference for a take whose text was decoded with its
// backup input's audio: the take's WAV lacks what the backup heard, so no
// text pairs with it. A note alone is saved.
var ErrBackup = errors.New("compare: the take's text was decoded with the backup input's audio, which its WAV does not hold; only a note can be saved")

// Save appends the label of the take in dir and returns its row.
func (l *Labels) Save(dir string, in Label) (Row, error) {
	id, ref, note := in.ID, strings.TrimSpace(in.Ref), strings.TrimSpace(in.Note)
	if len(in.Correct) > 0 || in.Clear {
		ref = ""
	}
	if ref == "" && note == "" && len(in.Correct) == 0 && !in.Clear {
		return Row{}, ErrEmpty
	}
	if !takeID.MatchString(id) {
		return Row{}, fmt.Errorf("%w: %q is not a take's name", ErrNoTake, id)
	}
	wav := filepath.Join(dir, id+".wav")
	st, err := os.Stat(wav)
	if errors.Is(err, fs.ErrNotExist) {
		return Row{}, fmt.Errorf("%w: %w", ErrNoTake, err)
	}
	if err != nil {
		return Row{}, fmt.Errorf("compare: %w", err)
	}
	now := time.Now
	if l.Now != nil {
		now = l.Now
	}
	row := Row{ID: id, WAV: wav, DurS: wavSeconds(st.Size()), Source: Source,
		Delivered: readText(filepath.Join(dir, id+".txt")), LabeledAt: now()}
	rec, err := ReadRecord(wav)
	if err != nil {
		return Row{}, err
	}
	// answers: every engine's text, or the delivered text alone; none for a
	// silent take, whose engine texts are invented
	answers := map[string]string{}
	if rec != nil {
		row.Primary, row.DeliveredBy, row.Engines = rec.Primary, rec.DeliveredBy, map[string]EngineResult{}
		for name, r := range rec.Engines {
			r.Words = nil // a label row keeps its shape: the engines' texts, not their timing
			row.Engines[name] = r
			if !rec.Silent && r.Error == "" && strings.TrimSpace(r.Text) != "" {
				answers[name] = strings.TrimSpace(r.Text)
			}
		}
	} else if row.Delivered != "" {
		answers[Delivered] = row.Delivered
	}
	switch {
	case in.Clear:
		row.LabelSource = ByClearing
	case len(in.Correct) > 0:
		for _, name := range in.Correct {
			if _, ok := answers[name]; !ok {
				return Row{}, fmt.Errorf("%w: %q", ErrEngine, name)
			}
			if !slices.Contains(row.Correct, name) {
				row.Correct = append(row.Correct, name)
			}
		}
		ref, row.LabelSource = answers[in.Correct[0]], ByClick
	default:
		row.LabelSource = ByTyping
	}
	if ref != "" && backupText(filepath.Join(dir, id)) {
		return Row{}, ErrBackup
	}
	if ref != "" {
		names := make([]string, 0, len(answers))
		for name := range answers {
			names = append(names, name)
		}
		slices.Sort(names)
		for _, name := range names {
			if !slices.Contains(row.Correct, name) && same(answers[name], ref) {
				row.Correct = append(row.Correct, name)
			}
		}
	}
	row.Ref, row.Lang, row.Note = ref, Lang(ref), note
	b, err := json.Marshal(row)
	if err != nil {
		return Row{}, err
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if err := os.MkdirAll(filepath.Dir(l.Path), 0o755); err != nil {
		return Row{}, err
	}
	f, err := os.OpenFile(l.Path, os.O_WRONLY|os.O_APPEND|os.O_CREATE, 0o644)
	if err != nil {
		return Row{}, err
	}
	if _, err := f.Write(append(b, '\n')); err != nil {
		f.Close()
		return Row{}, err
	}
	return row, f.Close()
}

// backupText reports whether the text of the take at base was decoded with
// its backup input's audio: its record's last text line says audio backup
// (store.AudioOf).
func backupText(base string) bool {
	evs, _ := store.Read(base)
	for i := len(evs) - 1; i >= 0; i-- {
		if evs[i].Ev == "text" {
			return evs[i].Audio == "backup"
		}
	}
	return false
}

// Latest is each take's label: its last row, unless that row clears it. A
// missing file has none; a line that does not parse is skipped.
func (l *Labels) Latest() (map[string]Row, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	out := map[string]Row{}
	f, err := os.Open(l.Path)
	if os.IsNotExist(err) {
		return out, nil
	}
	if err != nil {
		return nil, err
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	sc.Buffer(nil, 16<<20)
	for sc.Scan() {
		var r Row
		switch {
		case json.Unmarshal(sc.Bytes(), &r) != nil || r.ID == "":
		case r.LabelSource == ByClearing:
			delete(out, r.ID)
		default:
			out[r.ID] = r
		}
	}
	return out, sc.Err()
}

// Lang is a text's language as asrbench buckets it: zh (Han only), en
// (Latin letters only), mixed, or "" with neither.
func Lang(s string) string {
	var han, lat bool
	for _, r := range s {
		han = han || unicode.Is(unicode.Han, r)
		lat = lat || unicode.Is(unicode.Latin, r)
	}
	switch {
	case han && lat:
		return "mixed"
	case han:
		return "zh"
	case lat:
		return "en"
	}
	return ""
}

// same reports whether two transcripts say the same words: equal ignoring
// spacing, punctuation, symbols and case.
func same(a, b string) bool { return words(a) == words(b) }

func words(s string) string {
	return strings.Map(func(r rune) rune {
		if unicode.IsSpace(r) || unicode.IsPunct(r) || unicode.IsSymbol(r) {
			return -1
		}
		return unicode.ToLower(r)
	}, s)
}

// wavSeconds is a take WAV's length from its size, to the hundredth.
func wavSeconds(size int64) float64 {
	return math.Round(float64(max(size-44, 0)/2)/audio.Rate*100) / 100
}

func readText(path string) string {
	b, _ := os.ReadFile(path)
	return strings.TrimSpace(string(b))
}
