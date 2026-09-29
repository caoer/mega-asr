package store

import (
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// eventsExt is the suffix of a take's record: <base>.events.jsonl.
const eventsExt = ".events.jsonl"

// atLayout is the ms-precision RFC 3339 time with offset that `at` and `tap_at`
// carry.
const atLayout = "2006-01-02T15:04:05.000Z07:00"

// Event is one line of a take's record <base>.events.jsonl: append-only, one
// JSON object per line, {"v":1,"at":…,"ev":…} and the ev's own fields. The
// ev names and JSON field names are contract I1, read by mega-asr-loop:
//
//	start         trigger tap/ctl/replay, tap_at, target, engine, model, build, prev
//	mic_live, no_signal, backup_on
//	              after_s
//	backup_off    after_s, why: the take has no backup track, or lost it
//	stop          kind tap/enter/cancel/auto/stream_end/write_error/shutdown, why,
//	              via menu/ctl (absent: a key or the take's own end), dur_s
//	send_on       Enter during transcription
//	text          engine, raw_chars, chars, failed_chunks, latency_ms, stream_err,
//	              sha256, audio main/backup
//	hold          why cancelled/deliver_failed/asr_failed/interrupted/delivery_cut/empty
//	deliver       n, via auto/hotkey/page/cli, caller, target, ok, err, chars,
//	              submit none/ok/err, source delivered/raw/<engine>/retranscription <n>;
//	              text only when it differs from .txt; at is when the text
//	              went out
//	received      n (the deliver line read back), received whole/short/unknown,
//	              why: the target read back after that delivery, recorded only
//	recover       a start found the take without a final state
//	retranscribe  n, engine, model, raw, text, latency_ms, err, audio main/backup
//	seen          the take was opened on the page
//	dismiss       the user dismissed the undelivered take: it no longer counts
//	              as undelivered
//	undismiss     the user took a dismiss back
//
// V is the event format's version, not schema 2's row v. A field left at its
// zero value is not written, and a field absent from a line reads as its zero
// value; `ok` on a deliver line, `chars` on text and deliver lines and
// `raw_chars` on a text line are always written.
type Event struct {
	V  int       `json:"v"`
	At time.Time `json:"at"`
	Ev string    `json:"ev"`

	Trigger string    `json:"trigger,omitempty"`
	TapAt   time.Time `json:"tap_at"`
	Target  *Target   `json:"target,omitempty"`
	Engine  string    `json:"engine,omitempty"`
	Model   string    `json:"model,omitempty"`
	Build   string    `json:"build,omitempty"`
	Prev    *Prev     `json:"prev,omitempty"`

	AfterS float64 `json:"after_s,omitempty"`

	Kind string  `json:"kind,omitempty"`
	Why  string  `json:"why,omitempty"`
	DurS float64 `json:"dur_s,omitempty"`

	RawChars     int    `json:"raw_chars,omitempty"`
	Chars        int    `json:"chars,omitempty"`
	FailedChunks int    `json:"failed_chunks,omitempty"`
	LatencyMS    int64  `json:"latency_ms,omitempty"`
	StreamErr    string `json:"stream_err,omitempty"`
	SHA256       string `json:"sha256,omitempty"`
	Audio        string `json:"audio,omitempty"`

	N        int     `json:"n,omitempty"`
	Via      string  `json:"via,omitempty"`
	Caller   *Caller `json:"caller,omitempty"`
	OK       bool    `json:"ok,omitempty"`
	Err      string  `json:"err,omitempty"`
	Received string  `json:"received,omitempty"`
	Submit   string  `json:"submit,omitempty"`
	Source   string  `json:"source,omitempty"`
	Text     string  `json:"text,omitempty"`
	Raw      string  `json:"raw,omitempty"`
}

// Target is where a take was spoken for or delivered to: kind herdr or app,
// a herdr pane and its workspace, or an app by name, pid and bundle id, with
// its front window's title.
type Target struct {
	Kind      string `json:"kind,omitempty"`
	Pane      string `json:"pane,omitempty"`
	Workspace string `json:"workspace,omitempty"`
	App       string `json:"app,omitempty"`
	PID       int    `json:"pid,omitempty"`
	BundleID  string `json:"bundle_id,omitempty"`
	Title     string `json:"title,omitempty"`
}

// Prev is the take before this one and the seconds between them.
type Prev struct {
	ID   string  `json:"id"`
	GapS float64 `json:"gap_s"`
}

// Caller is the process of a command that delivered a take.
type Caller struct {
	PID     int    `json:"pid,omitempty"`
	Process string `json:"process,omitempty"`
}

// MarshalJSON writes v, at and ev first, times as RFC 3339 with milliseconds
// and offset, and omits tap_at when it is zero.
func (e Event) MarshalJSON() ([]byte, error) {
	type fields Event
	w := struct {
		V        int    `json:"v"`
		At       string `json:"at"`
		Ev       string `json:"ev"`
		TapAt    string `json:"tap_at,omitempty"`
		OK       *bool  `json:"ok,omitempty"`
		RawChars *int   `json:"raw_chars,omitempty"`
		Chars    *int   `json:"chars,omitempty"`
		*fields
	}{V: e.V, At: e.At.Format(atLayout), Ev: e.Ev, fields: (*fields)(&e)}
	if !e.TapAt.IsZero() {
		w.TapAt = e.TapAt.Format(atLayout)
	}
	if e.Ev == "deliver" {
		w.OK = &e.OK
	}
	if e.Ev == "text" {
		w.RawChars = &e.RawChars
	}
	if e.Ev == "text" || e.Ev == "deliver" {
		w.Chars = &e.Chars
	}
	return json.Marshal(w)
}

// syncs are the evs Append flushes to the disk itself before it returns: the
// ones a take's state rests on.
var syncs = map[string]bool{"stop": true, "text": true, "hold": true, "deliver": true, "dismiss": true, "undismiss": true}

// Append adds e as one line to the record of the take at base (its path
// without extension), creating the record if needed. It sets V to 1 and a
// zero At to now. The line is one write with O_APPEND; when the record does
// not end in a newline (a line torn by a crash) a newline goes first, so the
// new event never joins the torn one. stop, text, hold, deliver, dismiss and
// undismiss are on the disk itself (F_FULLFSYNC on macOS) when Append returns.
func Append(base string, e Event) error {
	if e.Ev == "" {
		return errors.New("store: event without ev")
	}
	e.V = 1
	if e.At.IsZero() {
		e.At = time.Now()
	}
	line, err := json.Marshal(e)
	if err != nil {
		return err
	}
	f, err := os.OpenFile(base+eventsExt, os.O_RDWR|os.O_APPEND|os.O_CREATE, 0o644)
	if err != nil {
		return err
	}
	st, err := f.Stat()
	if err != nil {
		f.Close()
		return err
	}
	if n := st.Size(); n > 0 {
		var last [1]byte
		if _, err := f.ReadAt(last[:], n-1); err != nil {
			f.Close()
			return err
		}
		if last[0] != '\n' {
			line = append([]byte{'\n'}, line...)
		}
	}
	if _, err := f.Write(append(line, '\n')); err != nil {
		f.Close()
		return err
	}
	if syncs[e.Ev] {
		if err := f.Sync(); err != nil {
			f.Close()
			return err
		}
	}
	return f.Close()
}

// Read returns the events of the take at base in the order they were
// appended. A line that does not parse as an event, such as one torn by a
// crash, is skipped. A take without a record returns an error wrapping
// fs.ErrNotExist.
func Read(base string) ([]Event, error) {
	b, err := os.ReadFile(base + eventsExt)
	if err != nil {
		return nil, err
	}
	var evs []Event
	for _, line := range bytes.Split(b, []byte{'\n'}) {
		var e Event
		if len(bytes.TrimSpace(line)) == 0 || json.Unmarshal(line, &e) != nil || e.Ev == "" {
			continue
		}
		evs = append(evs, e)
	}
	return evs, nil
}

// Status is a take's state, derived from its record by State.
//
// State is one of:
//
//	""             no record, or none of its lines sets a state: a take
//	               from before the record existed
//	"recording"    started, not stopped
//	"transcribing" stopped, no final state yet (transcribing or waiting its turn)
//	"sent"         delivered and Enter pressed
//	"pasted"       delivered without Enter
//	"cancelled"    the user cancelled it
//	"empty"        no speech
//	"undelivered"  with Why: deliver_failed, asr_failed, interrupted,
//	               delivery_cut (an exit cut it while its paste went out, so
//	               the text most likely landed), or incomplete (its own
//	               delivery went out with failed chunks)
//
// Seen is true when a seen line follows the line that set the final state.
// Dismissed is true when the take is undelivered and the later of its
// dismiss and undismiss lines is a dismiss: the user saw it and it does not
// matter. It keeps its state and why.
type Status struct {
	State     string
	Why       string
	Seen      bool
	Dismissed bool
}

// Final reports whether the take has reached a final state.
func (s Status) Final() bool {
	switch s.State {
	case "sent", "pasted", "cancelled", "empty", "undelivered":
		return true
	}
	return false
}

// Counted reports whether the take counts as undelivered for the menu bar:
// undelivered, not seen since and not dismissed. A cancel the user made
// never counts, nor a take whose paste was under way when an exit cut it.
func (s Status) Counted() bool {
	return s.State == "undelivered" && s.Why != "delivery_cut" && !s.Seen && !s.Dismissed
}

// State derives a take's state from its events, a pure function of the
// lines. start makes it recording and stop transcribing; the last hold or
// deliver line sets the final state, so a resend moves a held take to sent
// or pasted. A take that has reached a target stays where that left it: a
// failed resend of it changes nothing.
//
//   - hold why: cancelled → cancelled, empty → empty, any other → undelivered
//     with that why; interrupted on a take stopped by cancel stays cancelled.
//   - deliver ok: false → undelivered, deliver_failed, unless an earlier
//     deliver line is ok.
//   - deliver ok: true → sent when submit is ok, else pasted; but the take's
//     own delivery (via auto) of a text with failed chunks → undelivered,
//     incomplete.
//
// A take under 0.3 s ends with hold empty like any take without speech.
// The later of a dismiss and an undismiss line decides Dismissed, whatever
// lines came between.
func State(evs []Event) Status {
	var s Status
	cancelled, partial, reached, dismissed := false, false, false, false
	set := func(state, why string) { s = Status{State: state, Why: why} }
	for _, e := range evs {
		switch e.Ev {
		case "start":
			if s.State == "" {
				s.State = "recording"
			}
		case "stop":
			cancelled = cancelled || e.Kind == "cancel"
			if s.State == "" || s.State == "recording" {
				s.State = "transcribing"
			}
		case "text":
			partial = e.FailedChunks > 0
			if s.State == "" || s.State == "recording" {
				s.State = "transcribing"
			}
		case "hold":
			switch {
			case e.Why == "cancelled", e.Why == "interrupted" && cancelled:
				set("cancelled", "")
			case e.Why == "empty":
				set("empty", "")
			default:
				set("undelivered", e.Why)
			}
		case "deliver":
			switch {
			case !e.OK && reached:
			case !e.OK:
				set("undelivered", "deliver_failed")
			case partial && (e.Via == "" || e.Via == "auto"):
				set("undelivered", "incomplete")
			case e.Submit == "ok":
				set("sent", "")
			default:
				set("pasted", "")
			}
			reached = reached || e.OK
		case "seen":
			s.Seen = true
		case "dismiss", "undismiss":
			dismissed = e.Ev == "dismiss"
		}
	}
	s.Dismissed = dismissed && s.State == "undelivered"
	return s
}

// AudioOf is the WAV of the track a take's text leans on, and the track's
// name. A text line's audio backup says the main input went dead in some
// spans and the text was decoded from the main track with the backup
// input's audio in those spans; the backup input's own WAV is
// backup/<name>.wav beside the take, which holds none of what only the main
// input heard. AudioOf answers that WAV and backup when the record's last
// text line says audio backup and the file exists; else the main WAV,
// <base>.wav, and main. A decode rebuilds the spliced audio from both
// tracks (session's Retranscribe).
func AudioOf(base string, evs []Event) (wav, track string) {
	for i := len(evs) - 1; i >= 0; i-- {
		if evs[i].Ev != "text" {
			continue
		}
		if evs[i].Audio == "backup" {
			p := filepath.Join(filepath.Dir(base), BackupDir, filepath.Base(base)+".wav")
			if _, err := os.Stat(p); err == nil {
				return p, "backup"
			}
		}
		break
	}
	return base + ".wav", "main"
}

// Record is a take's base and its events.
type Record struct {
	Base   string
	Events []Event
}

// Start is when the take started: its start line's at, else its first
// line's.
func (r Record) Start() time.Time {
	for _, e := range r.Events {
		if e.Ev == "start" {
			return e.At
		}
	}
	if len(r.Events) > 0 {
		return r.Events[0].At
	}
	return time.Time{}
}

// Recent returns the records of the takes that started at or after since,
// newest start first, at most n of them (all when n <= 0). A record without
// a parseable line is skipped. Only records whose name is within a day of
// since are opened, so the cost follows the window, not the store.
func (s Store) Recent(n int, since time.Time) ([]Record, error) {
	paths, err := filepath.Glob(filepath.Join(s.Dir, "*"+eventsExt))
	if err != nil {
		return nil, err
	}
	var recs []Record
	for _, p := range paths {
		base := strings.TrimSuffix(p, eventsExt)
		if named, ok := NameTime(filepath.Base(base)); ok && named.Before(since.Add(-24*time.Hour)) {
			continue
		}
		evs, err := Read(base)
		if err != nil || len(evs) == 0 {
			continue
		}
		r := Record{Base: base, Events: evs}
		if r.Start().Before(since) {
			continue
		}
		recs = append(recs, r)
	}
	sort.SliceStable(recs, func(i, j int) bool { return recs[i].Start().After(recs[j].Start()) })
	if n > 0 && len(recs) > n {
		recs = recs[:n]
	}
	return recs, nil
}

// NameTime reads the local time a take's name gives: YYYYmmdd-HHMMSS, with
// an optional -N.
func NameTime(name string) (time.Time, bool) {
	if len(name) < 15 {
		return time.Time{}, false
	}
	t, err := time.ParseInLocation("20060102-150405", name[:15], time.Local)
	return t, err == nil
}
