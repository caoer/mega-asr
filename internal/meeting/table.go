package meeting

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"text/tabwriter"
	"time"
)

// Line is one alignment run in the score table, scores.jsonl: the ASR
// regression history, appended to and never rewritten, so a new engine's
// run sits beside the old one's on the same meeting.
type Line struct {
	At          time.Time `json:"at"`
	Rec         string    `json:"rec"`
	Against     string    `json:"against"`
	Engine      string    `json:"engine,omitempty"`
	Root        string    `json:"root,omitempty"`
	RefTokens   int       `json:"ref_tokens"`
	CER         float64   `json:"cer"`
	CERZh       float64   `json:"cer_zh"`
	WEREn       float64   `json:"wer_en"`
	CPCER       float64   `json:"cpcer"`
	TCPCER      float64   `json:"tcpcer"`
	OffsetS     float64   `json:"offset_s"`
	DriftPPM    float64   `json:"drift_ppm"`
	OffsetScore float64   `json:"offset_score"`
}

// Line is the result's row in the score table.
func (r Result) Line() Line {
	s := r.Scores()
	return Line{At: r.At, Rec: r.Rec, Against: r.Against, Engine: r.Engine, Root: r.Root, RefTokens: r.RefTokens,
		CER: s.CER, CERZh: s.CERZh, WEREn: s.WEREn, CPCER: s.CPCER, TCPCER: s.TCPCER,
		OffsetS: s.OffsetS, DriftPPM: s.DriftPPM, OffsetScore: s.OffsetScore}
}

// AppendLine adds one line to the table at path.
func AppendLine(path string, l Line) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	b, err := json.Marshal(l)
	if err != nil {
		return err
	}
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	if _, err := f.Write(append(b, '\n')); err != nil {
		f.Close()
		return err
	}
	return f.Close()
}

// ReadLines reads the table's lines from since on; a missing table is empty.
func ReadLines(path string, since time.Time) ([]Line, error) {
	f, err := os.Open(path)
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	defer f.Close()
	var out []Line
	sc := bufio.NewScanner(f)
	for n := 1; sc.Scan(); n++ {
		var l Line
		if err := json.Unmarshal(sc.Bytes(), &l); err != nil {
			return out, fmt.Errorf("%s:%d: %w", path, n, err)
		}
		if !l.At.Before(since) {
			out = append(out, l)
		}
	}
	return out, sc.Err()
}

// PrintTable prints the lines, one alignment run each: error rates in
// percent, the offset in seconds, the drift in ppm.
func PrintTable(w io.Writer, lines []Line) error {
	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "AT\tREC\tAGAINST\tROOT\tTOKENS\tCER\tZH\tEN\tCPCER\tTCPCER\tOFFSET\tDRIFT\tSCORE")
	for _, l := range lines {
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%d\t%.1f%%\t%.1f%%\t%.1f%%\t%.1f%%\t%.1f%%\t%.2f\t%.0f\t%.1f\n",
			l.At.Local().Format("2006-01-02 15:04"), l.Rec, l.Against, filepath.Base(l.Root), l.RefTokens,
			100*l.CER, 100*l.CERZh, 100*l.WEREn, 100*l.CPCER, 100*l.TCPCER, l.OffsetS, l.DriftPPM, l.OffsetScore)
	}
	return tw.Flush()
}
