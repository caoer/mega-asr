package meeting

import (
	"context"
	"fmt"
	"math"
	"sort"
	"strings"
	"time"

	"github.com/caoer/mega-asr/internal/score"
)

// Alignment parameters.
const (
	MinOffsetScore = 10.0  // audio-offset-finder's standard score for a match
	MaxDriftPPM    = 1000  // a larger fitted drift is a bad probe, not a clock
	WindowS        = 300.0 // error rates are summed over 5-min windows
	CollarS        = 5.0   // tcpCER: a word counts within 5 s of its reference
)

// Tools are the Python helpers (scripts/megameet): the offset search and
// meeteval.
type Tools interface {
	Offset(ctx context.Context, feishu string, local []string, expectS float64) (Search, error)
	CP(ctx context.Context, ref, hyp []Utterance, collarS float64) (CPResult, error)
}

// Search is what offset.py found: both files' lengths and, per probe, the
// local time of a Feishu time.
type Search struct {
	FeishuS float64 `json:"feishu_s"`
	LocalS  float64 `json:"local_s"`
	Probes  []Probe `json:"probes"`
}

type Probe struct {
	At      string  `json:"at"` // start | end
	FeishuS float64 `json:"feishu_s"`
	LocalS  float64 `json:"local_s"`
	Score   float64 `json:"score"`
}

// Utterance is one speaker's text over a span, tokens space-separated, for
// meeteval.
type Utterance struct {
	Speaker string  `json:"speaker"`
	Start   float64 `json:"start"`
	End     float64 `json:"end"`
	Words   string  `json:"words"`
}

// CPResult is meeteval's cpWER and tcpWER over the meeting.
type CPResult struct {
	CP  ErrorRate `json:"cp"`
	TCP ErrorRate `json:"tcp"`
}

type ErrorRate struct {
	Errors     int         `json:"errors"`
	Length     int         `json:"length"`
	Rate       float64     `json:"error_rate"`
	Assignment [][2]string `json:"assignment"` // (reference speaker, hypothesis speaker)
}

// Input is one alignment's material, all local.
type Input struct {
	Rec, Against string    // the local record and the Feishu one
	Date         string    // the recording's day, YYYY-MM-DD, for training rows
	Segments     []Segment // the local transcript, local time
	Audio        []string  // local tracks, mixed for the offset search
	Turns        []Turn    // Feishu's transcript, Feishu time
	Media        string    // Feishu's media
	ExpectS      float64   // Feishu's 0 s on the local timeline by the wall clocks
}

// Result is align.json.
type Result struct {
	Rec       string     `json:"rec"`
	Against   string     `json:"against"`
	At        time.Time  `json:"at"`
	Engine    string     `json:"engine,omitempty"`
	Root      string     `json:"root,omitempty"`
	Paired    bool       `json:"paired"`
	Flags     []string   `json:"flags,omitempty"`
	ExpectS   float64    `json:"expect_s"`
	Search    Search     `json:"search"`
	OffsetS   float64    `json:"offset_s"`
	DriftPPM  float64    `json:"drift_ppm"`
	Score     float64    `json:"offset_score"`
	OverlapS  [2]float64 `json:"overlap_s"` // Feishu time
	Windows   []Window   `json:"windows,omitempty"`
	RefTokens int        `json:"ref_tokens"`
	CER       float64    `json:"cer"`
	CERZh     float64    `json:"cer_zh"`
	WEREn     float64    `json:"wer_en"`
	CP        *CPResult  `json:"cp,omitempty"`
	Me        string     `json:"me,omitempty"` // the Feishu speaker the mic track is
	Train     []TrainRow `json:"-"`
}

// Window is one 5-min window's score; times are Feishu's.
type Window struct {
	StartS    float64 `json:"start_s"`
	EndS      float64 `json:"end_s"`
	Lang      string  `json:"lang"`
	RefTokens int     `json:"ref_tokens"`
	Errors    int     `json:"errors"`
	Rate      float64 `json:"rate"`
	HanRef    int     `json:"han_ref"`
	HanErr    int     `json:"han_err"`
	LatRef    int     `json:"lat_ref"`
	LatErr    int     `json:"lat_err"`
}

// Scores are the record's scores from this result.
func (r Result) Scores() Scores {
	s := Scores{CER: r.CER, CERZh: r.CERZh, WEREn: r.WEREn, OffsetS: r.OffsetS, DriftPPM: r.DriftPPM, OffsetScore: r.Score, Against: r.Against}
	if r.CP != nil {
		s.CPCER, s.TCPCER = r.CP.CP.Rate, r.CP.TCP.Rate
	}
	return s
}

// Align finds where the Feishu media sits on the local timeline, then
// scores the local transcript against Feishu's over the overlap. An offset
// score under MinOffsetScore leaves the result unpaired: the two are not
// recordings of the same audio, or the search missed.
func Align(ctx context.Context, tools Tools, in Input) (Result, error) {
	res := Result{Rec: in.Rec, Against: in.Against, At: time.Now().UTC(), ExpectS: in.ExpectS}
	res.Engine, res.Root = engineOf(in.Segments)
	search, err := tools.Offset(ctx, in.Media, in.Audio, in.ExpectS)
	if err != nil {
		return res, fmt.Errorf("offset: %w", err)
	}
	res.Search = search
	if !fit(&res) {
		return res, nil
	}
	segs, turns := place(&res, in.Segments, in.Turns)
	windows(&res, segs, turns)

	ref, hyp := utterances(segs, turns, res.OverlapS[1])
	if len(ref) > 0 && len(hyp) > 0 {
		cp, err := tools.CP(ctx, ref, hyp, CollarS)
		if err != nil {
			return res, fmt.Errorf("meeteval: %w", err)
		}
		res.CP = &cp
		res.Me = micSpeaker(segs, cp.CP.Assignment)
	}
	if res.Me != "" {
		res.Train = trainRows(in, segs, turns, res.Me, res.OffsetS, 1+res.DriftPPM*1e-6)
	}
	return res, nil
}

// Rescore scores another transcript of the audio a paired result aligned
// against the same Feishu turns, at the offset and drift that run fitted:
// Align's 5-min windows without the offset search and without meeteval.
func Rescore(prev Result, segs []Segment, turns []Turn) Result {
	res := Result{Rec: prev.Rec, Against: prev.Against, At: time.Now().UTC(), Paired: true, ExpectS: prev.ExpectS,
		Search: prev.Search, OffsetS: prev.OffsetS, DriftPPM: prev.DriftPPM, Score: prev.Score}
	res.Engine, res.Root = engineOf(segs)
	s, t := place(&res, segs, turns)
	windows(&res, s, t)
	return res
}

// Itself is the alignment of a Feishu minute's own media with its
// transcript, lengthS long: offset 0, no drift. It scores the local
// engine's transcript of the minute against Feishu's.
func Itself(id string, lengthS float64) Result {
	return Result{Rec: id, Against: id, Paired: true, Search: Search{FeishuS: lengthS, LocalS: lengthS}}
}

func engineOf(segs []Segment) (engine, root string) {
	for _, s := range segs {
		if s.Root != "" || s.Engine != "" {
			return s.Engine, s.Root
		}
	}
	return "", ""
}

// place moves the segments onto Feishu's timeline by the fitted offset and
// drift, sets the overlap, and keeps the segments and turns with text that
// start inside it, each in time order.
func place(res *Result, in []Segment, all []Turn) ([]Segment, []Turn) {
	rate := 1 + res.DriftPPM*1e-6
	feishuT := func(localS float64) float64 { return (localS - res.OffsetS) / rate }
	lo, hi := math.Max(0, feishuT(0)), math.Min(res.Search.FeishuS, feishuT(res.Search.LocalS))
	res.OverlapS = [2]float64{lo, hi}
	segs := make([]Segment, 0, len(in))
	for _, s := range in {
		s.StartS, s.EndS = feishuT(s.StartS), feishuT(s.EndS)
		if s.StartS >= lo && s.StartS < hi && strings.TrimSpace(s.Text) != "" {
			segs = append(segs, s)
		}
	}
	sort.SliceStable(segs, func(i, j int) bool { return segs[i].StartS < segs[j].StartS })
	var turns []Turn
	for _, t := range all {
		if t.StartS >= lo && t.StartS < hi && strings.TrimSpace(t.Text) != "" {
			turns = append(turns, t)
		}
	}
	sort.SliceStable(turns, func(i, j int) bool { return turns[i].StartS < turns[j].StartS })
	return segs, turns
}

// fit turns the probes into an offset and a drift: two matching probes fit
// a line, one gives the offset alone. It reports whether the pair holds.
func fit(res *Result) bool {
	var good []Probe
	for _, p := range res.Search.Probes {
		if p.Score >= MinOffsetScore {
			good = append(good, p)
		}
	}
	best := func() Probe {
		b := res.Search.Probes[0]
		for _, p := range res.Search.Probes {
			if p.Score > b.Score {
				b = p
			}
		}
		return b
	}
	switch {
	case len(res.Search.Probes) == 0:
		res.Flags = append(res.Flags, "unpaired: no probe (overlap too short)")
		return false
	case len(good) == 0:
		b := best()
		res.Score = b.Score
		res.Flags = append(res.Flags, fmt.Sprintf("unpaired: offset score %.1f < %.0f", b.Score, MinOffsetScore))
		return false
	}
	res.Paired = true
	p := good[0]
	res.Score, res.OffsetS = p.Score, p.LocalS-p.FeishuS
	if len(good) < 2 {
		return true
	}
	q := good[len(good)-1]
	if q.FeishuS-p.FeishuS < WindowS {
		return true
	}
	rate := (q.LocalS - p.LocalS) / (q.FeishuS - p.FeishuS)
	if drift := (rate - 1) * 1e6; math.Abs(drift) > MaxDriftPPM {
		b := best()
		res.Score, res.OffsetS = b.Score, b.LocalS-b.FeishuS
		res.Flags = append(res.Flags, fmt.Sprintf("drift %.0f ppm rejected: the %s probe alone", drift, b.At))
		return true
	}
	res.DriftPPM = (rate - 1) * 1e6
	res.OffsetS = p.LocalS - p.FeishuS*rate
	res.Score = math.Min(p.Score, q.Score)
	return true
}

// windows scores the local text against Feishu's in 5-min windows of the
// overlap: a turn or a segment belongs to the window its start falls in.
func windows(res *Result, segs []Segment, turns []Turn) {
	lo, hi := res.OverlapS[0], res.OverlapS[1]
	var all score.Tally
	for w0 := lo; w0 < hi; w0 += WindowS {
		w1 := math.Min(w0+WindowS, hi)
		var ref, hyp []string
		for _, t := range turns {
			if t.StartS >= w0 && t.StartS < w1 {
				ref = append(ref, t.Text)
			}
		}
		for _, s := range segs {
			if s.StartS >= w0 && s.StartS < w1 {
				hyp = append(hyp, s.Text)
			}
		}
		r := strings.Join(ref, " ")
		c := score.ScoreClip(r, strings.Join(hyp, " "), nil)
		all.Add(c, 0)
		res.Windows = append(res.Windows, Window{StartS: w0, EndS: w1, Lang: lang(c), RefTokens: c.RefTok, Errors: c.Errors(), Rate: c.Rate(),
			HanRef: c.HanRef, HanErr: c.HanErr, LatRef: c.LatRef, LatErr: c.LatErr})
	}
	res.RefTokens = all.RefTok
	res.CER = ratio(all.Errors(), all.RefTok)
	res.CERZh = ratio(all.HanErr, all.HanRef)
	res.WEREn = ratio(all.LatErr, all.LatRef)
}

func ratio(n, d int) float64 {
	if d == 0 {
		return 0
	}
	return float64(n) / float64(d)
}

// lang is the reference's language: zh, en, mixed, or none.
func lang(c score.Clip) string {
	switch {
	case c.HanRef > 0 && c.LatRef > 0:
		return "mixed"
	case c.HanRef > 0:
		return "zh"
	case c.LatRef > 0:
		return "en"
	}
	return "none"
}

// utterances are both transcripts for meeteval, normalised tokens on
// Feishu's timeline. A Feishu turn ends where the next one starts (its
// end is not given); the last ends with the overlap.
func utterances(segs []Segment, turns []Turn, endS float64) (ref, hyp []Utterance) {
	for i, t := range turns {
		end := endS
		if i+1 < len(turns) {
			end = turns[i+1].StartS
		}
		if w := strings.Join(score.Normalize(t.Text), " "); w != "" {
			ref = append(ref, Utterance{Speaker: t.Speaker, Start: t.StartS, End: math.Max(end, t.StartS), Words: w})
		}
	}
	for _, s := range segs {
		if w := strings.Join(score.Normalize(s.Text), " "); w != "" {
			hyp = append(hyp, Utterance{Speaker: speakerOf(s), Start: s.StartS, End: math.Max(s.EndS, s.StartS), Words: w})
		}
	}
	return ref, hyp
}

func speakerOf(s Segment) string {
	if s.Speaker != "" {
		return s.Speaker
	}
	return s.Track
}

// micSpeaker is the Feishu speaker meeteval assigned to the mic track's
// speaker (the most frequent one on the track): the owner of the recorder.
func micSpeaker(segs []Segment, assignment [][2]string) string {
	n := map[string]int{}
	for _, s := range segs {
		if s.Track == "mic" {
			n[speakerOf(s)]++
		}
	}
	mic, most := "", 0
	for k, v := range n {
		if v > most || (v == most && k < mic) {
			mic, most = k, v
		}
	}
	for _, a := range assignment {
		if mic != "" && a[1] == mic {
			return a[0]
		}
	}
	return ""
}
