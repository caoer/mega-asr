// Package meeting is megameet's work over the meetings registry: the
// meeting@1 record every source writes (docs/megameet.md), and aligning
// a local recording with the Feishu minute of the same meeting — pairing,
// offset and drift, error rates, the score table and training rows.
package meeting

import (
	"encoding/json"
	"fmt"
	"os"
	"time"
)

// Schema is the registry records' schema; a record's key is "rec." + its id.
const Schema = "meeting@1"

// Record states, in order; failed can follow any server state.
const (
	Uploading  = "uploading"
	Uploaded   = "uploaded"
	Processing = "processing"
	Aligned    = "aligned"
	Ingested   = "ingested"
	Failed     = "failed"
	// Deleted (tombstone.go) can follow any state.
)

// Unpaired is a record's align value when no Feishu minute scored it.
const Unpaired = "unpaired"

// Record is rec.<id>: id = YYYYMMDD-HHMMSS-<source>-<host>.
type Record struct {
	ID        string    `json:"id"`
	Source    string    `json:"source"` // mac | room | phone | feishu | file
	Host      string    `json:"host,omitempty"`
	Owner     string    `json:"owner,omitempty"`
	Title     string    `json:"title,omitempty"`
	Started   time.Time `json:"started"`
	Stopped   time.Time `json:"stopped,omitzero"`
	DurationS float64   `json:"duration_s,omitempty"`
	Speakers  []Speaker `json:"speakers,omitempty"`
	Apps      []string  `json:"apps,omitempty"`
	Files     []File    `json:"files,omitempty"`
	Feishu    *Feishu   `json:"feishu,omitempty"`
	State     string    `json:"state"`
	Stage     string    `json:"stage,omitempty"`  // asr | diarize | align | ingest
	Action    string    `json:"action,omitempty"` // retry | reingest | realign
	Claim     *Claim    `json:"claim,omitempty"`
	Attempts  int       `json:"attempts,omitempty"`
	Error     string    `json:"error,omitempty"`
	Align     string    `json:"align,omitempty"` // unpaired
	Scores    *Scores   `json:"scores,omitempty"`
	Wiki      *Wiki     `json:"wiki,omitempty"`
	Test      bool      `json:"test,omitempty"` // a test: processed, never ingested
	Engine    *Engine   `json:"engine,omitempty"`
	Updated   time.Time `json:"updated,omitzero"`
	DeletedAt time.Time `json:"deleted_at,omitzero"`
	DeletedBy string    `json:"deleted_by,omitempty"`
}

type Speaker struct {
	Name string `json:"name,omitempty"`
	Role string `json:"role,omitempty"`
}

// File is one /f/ object of the record; its file@1 record carries
// meta {rec, role}.
type File struct {
	Role     string `json:"role"` // remote | mic | beam | raw | doa | media | feishu-transcript | segments | transcript | align
	File     string `json:"file"` // the /f/ id
	Bytes    int64  `json:"bytes"`
	SHA256   string `json:"sha256"`
	Verified bool   `json:"verified"`
	Codec    string `json:"codec,omitempty"`
}

type Feishu struct {
	Token          string `json:"token"`
	URL            string `json:"url,omitempty"` // the minute's page
	TranscriptFile string `json:"transcript_file,omitempty"`
}

type Claim struct {
	By      string    `json:"by"`
	At      time.Time `json:"at"`
	Expires time.Time `json:"expires"`
}

// Scores are a local recording's error rates against its Feishu minute
// (fractions, not percent) and where that minute sits on its timeline.
// loops and rtf come from processing; align writes the rest.
type Scores struct {
	CER         float64 `json:"cer"`    // all tokens, 5-min windows
	CERZh       float64 `json:"cer_zh"` // Han characters
	WEREn       float64 `json:"wer_en"` // Latin words
	CPCER       float64 `json:"cpcer"`  // meeteval cpWER on characters
	TCPCER      float64 `json:"tcpcer"` // meeteval tcpWER on characters, collar 5 s
	Loops       int     `json:"loops,omitempty"`
	RTF         float64 `json:"rtf,omitempty"`
	OffsetS     float64 `json:"offset_s"`  // local time of Feishu's 0 s
	DriftPPM    float64 `json:"drift_ppm"` // local seconds per Feishu second, minus one, × 1e6
	OffsetScore float64 `json:"offset_score"`
	Against     string  `json:"against"` // the feishu record's id
}

type Wiki struct {
	Page   string `json:"page"`
	Commit string `json:"commit"`
}

type Engine struct {
	Root string `json:"root"`
	LLM  string `json:"llm,omitempty"` // asr.funasr.llm when set: the Qwen3 GGUF in place of Root's
	SHA  string `json:"sha,omitempty"`
}

// End is where the recording stops on the wall clock.
func (r Record) End() time.Time {
	if !r.Stopped.IsZero() {
		return r.Stopped
	}
	return r.Started.Add(time.Duration(r.DurationS * float64(time.Second)))
}

// File returns the record's file of role, if any.
func (r Record) File(role string) (File, bool) {
	for _, f := range r.Files {
		if f.Role == role {
			return f, true
		}
	}
	return File{}, false
}

// Segment is one piece of a local transcript: segments.json holds a list.
type Segment struct {
	Track   string  `json:"track"` // remote | mic | beam | media
	Speaker string  `json:"speaker"`
	StartS  float64 `json:"start_s"`
	EndS    float64 `json:"end_s"`
	Raw     string  `json:"raw"`
	Text    string  `json:"text"`
	Engine  string  `json:"engine"`
	Root    string  `json:"root"`
}

// Turn is one speaker turn of a Feishu transcript. Feishu gives its start
// only; a turn can run on past the next one's start.
type Turn struct {
	Speaker string  `json:"speaker"`
	StartS  float64 `json:"start_s"`
	Text    string  `json:"text"`
}

// ReadSegments reads a segments.json.
func ReadSegments(path string) ([]Segment, error) {
	var s []Segment
	return s, readJSON(path, &s)
}

// ReadTurns reads a Feishu transcript.json ({header, keywords, segments}).
func ReadTurns(path string) ([]Turn, error) {
	var t struct {
		Segments []Turn `json:"segments"`
	}
	return t.Segments, readJSON(path, &t)
}

func readJSON(path string, v any) error {
	b, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	if err := json.Unmarshal(b, v); err != nil {
		return fmt.Errorf("%s: %w", path, err)
	}
	return nil
}
