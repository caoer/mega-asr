package takes

import (
	"cmp"
	"errors"
	"fmt"
	"io/fs"
	"log"
	"maps"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/caoer/mega-asr/internal/asr"
	"github.com/caoer/mega-asr/internal/audio"
	"github.com/caoer/mega-asr/internal/compare"
	"github.com/caoer/mega-asr/internal/store"
)

// takeID is a take's name: store.Store.Next's stamp, -N for a second take
// in the same second.
var takeID = regexp.MustCompile(`^\d{8}-\d{6}(-\d+)?$`)

// exts are the files of a take the page reads, in the order Take.Files
// lists them.
var exts = []string{".wav", ".txt", ".raw.txt", ".compare.json", ".input.json", ".events.jsonl"}

// Row is a take as the list shows it.
type Row struct {
	ID        string        `json:"id"`
	At        time.Time     `json:"at"`  // the start line's time, else the name's
	Day       string        `json:"day"` // YYYY-MM-DD, local
	DurS      float64       `json:"dur_s"`
	State     string        `json:"state"` // store.Status's, or unknown: no record
	Why       string        `json:"why,omitempty"`
	Seen      bool          `json:"seen"`
	Dismissed bool          `json:"dismissed"` // undelivered, and the user dismissed it
	Counted   bool          `json:"counted"`   // undelivered, not seen since and not dismissed
	Record    bool          `json:"record"`    // false: a take from before the record existed
	Audio     bool          `json:"audio"`     // the WAV exists (a take under 0.3 s has none)
	Target    *store.Target `json:"target,omitempty"`
	TargetKey string        `json:"target_key,omitempty"` // pane:<id> or app:<bundle id or name>
	Device    string        `json:"device,omitempty"`
	Line      string        `json:"line"` // the text's first line
	Match     *Match        `json:"match,omitempty"`
	Labeled   bool          `json:"labeled,omitempty"` // the take has a label (Take.Label)
}

// Match is where a search found its query in a take: the text it was in
// (delivered, raw, an engine's name, retranscription <n>, deliver <n>) and
// the query with the text around it.
type Match struct {
	Source string `json:"source"`
	Before string `json:"before"`
	Hit    string `json:"hit"`
	After  string `json:"after"`
}

// Take is one take whole, for the detail.
type Take struct {
	Row
	Text    *string          `json:"text"` // .txt; null: not transcribed
	Raw     *string          `json:"raw"`  // .raw.txt
	Answers []Answer         `json:"answers"`
	Events  []store.Event    `json:"events"`
	Input   *audio.TakeInput `json:"input"`
	Files   []string         `json:"files"`
	Track   string           `json:"track"` // main or backup: the WAV the text came from, which the player plays
	Label   *compare.Row     `json:"label"` // the take's label: its last row in the label file; null: none, or cleared
}

// Answer is one engine's text for a take: from its compare record, or a
// re-transcription from its record's retranscribe lines.
type Answer struct {
	Source    string `json:"source"` // compare or retranscription
	Engine    string `json:"engine"`
	N         int    `json:"n,omitempty"`
	Model     string `json:"model,omitempty"`
	Text      string `json:"text"`
	Raw       string `json:"raw,omitempty"`
	LatencyMS int64  `json:"latency_ms,omitempty"`
	Err       string `json:"err,omitempty"`
	Delivered bool   `json:"delivered,omitempty"` // compare: the engine whose text was delivered
	Primary   bool   `json:"primary,omitempty"`   // compare: the record's primary, asr.engine when the take was recorded
	// Words are the words of the engine's text with their times in the
	// take (compare.EngineResult.Words); none from an engine without timing.
	Words []asr.Word `json:"words,omitempty"`
}

// Query selects takes: Q a substring of any of a take's texts, case folded;
// State equal to the row's filter state (filterState); Target (a TargetKey),
// Device and Day equal to the row's; Before the takes older than that id; N
// at most.
type Query struct {
	Q, State, Target, Device, Day, Before string
	N                                     int
	Facets                                bool
}

// Page is a page of rows, newest first.
type Page struct {
	Takes   []Row   `json:"takes"`
	More    bool    `json:"more"`    // older takes match too
	Counted int     `json:"counted"` // undelivered and not seen, in the whole store
	Matched int     `json:"matched"` // the takes the query selects, on every page (Before aside)
	Days    []Day   `json:"days"`    // Matched by day, newest first
	Facets  *Facets `json:"facets,omitempty"`
}

// Day is how many of a query's takes fall on a day, and how many of those
// are undelivered and not dismissed: the list's day heads.
type Day struct {
	Key         string `json:"key"` // YYYY-MM-DD
	N           int    `json:"n"`
	Undelivered int    `json:"undelivered"`
}

// Facets are the values the filters offer, over the whole store.
type Facets struct {
	States  map[string]int `json:"states"` // by filterState
	Targets []Facet        `json:"targets"`
	Devices []Facet        `json:"devices"`
	Days    []Facet        `json:"days"` // newest first
}

// Facet is one filter value, its label and how many takes carry it.
type Facet struct {
	Key   string `json:"key"`
	Label string `json:"label,omitempty"`
	N     int    `json:"n"`
}

// Index is every take of a store in memory: its row and every text it
// holds, for the list, the filters and search. A query more than a second
// after the last read began reads the directory again and reloads each take whose
// files changed in size or time: a record that grew, a text or a compare
// record written.
type Index struct {
	Dir    string
	Labels *compare.Labels // nil: no take has a label

	mu       sync.Mutex
	at       time.Time
	takes    map[string]*entry
	order    []string // newest first
	labels   map[string]compare.Row
	labelSig string // the label file's size and time when labels was read
	labelErr string // the last error reading it, logged once
}

type entry struct {
	sig   string
	row   Row
	texts []text
	sent  []delivery // the take's deliveries that arrived
}

// delivery is where a deliver line of a take's record put its text, and when.
type delivery struct {
	at time.Time
	to store.Target
}

// sentOf is the deliveries that arrived, of a take's record.
func sentOf(evs []store.Event) []delivery {
	var out []delivery
	for _, e := range evs {
		if e.Ev == "deliver" && e.OK && e.Target != nil {
			out = append(out, delivery{e.At, *e.Target})
		}
	}
	return out
}

type text struct {
	source  string
	s, fold string
}

func fold(s string) string { return strings.Map(unicode.ToLower, s) }

// refresh brings the index up to date with the directory; unless force, at
// most once a second, counted from when the last read began: a page that
// asks every 1.1 s then gets a fresh read at every ask, however long a read
// of a large store takes.
func (x *Index) refresh(force bool) error {
	x.mu.Lock()
	defer x.mu.Unlock()
	if !force && x.takes != nil && time.Since(x.at) < time.Second {
		return nil
	}
	began := time.Now()
	des, err := os.ReadDir(x.Dir)
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	sigs := map[string]*strings.Builder{}
	for _, de := range des {
		id, _, _ := strings.Cut(de.Name(), ".")
		if de.IsDir() || !takeID.MatchString(id) {
			continue
		}
		info, err := de.Info()
		if err != nil {
			continue
		}
		b := sigs[id]
		if b == nil {
			b = &strings.Builder{}
			sigs[id] = b
		}
		fmt.Fprintf(b, "%s %d %d;", de.Name(), info.Size(), info.ModTime().UnixNano())
	}
	if x.takes == nil {
		x.takes = map[string]*entry{}
	}
	changed := false
	var todo []string
	for id, b := range sigs {
		e, ok := x.takes[id]
		if ok && e.sig == b.String() {
			continue
		}
		changed = changed || !ok
		todo = append(todo, id)
	}
	// A take is a dozen opens and stats; the first build reads thousands, so
	// they are read side by side.
	loaded := make([]*entry, len(todo))
	var next atomic.Int64
	var wg sync.WaitGroup
	for range min(len(todo), 16) {
		wg.Go(func() {
			for i := int(next.Add(1)) - 1; i < len(todo); i = int(next.Add(1)) - 1 {
				t, texts := read(x.Dir, todo[i])
				loaded[i] = &entry{sig: sigs[todo[i]].String(), row: t.Row, texts: texts, sent: sentOf(t.Events)}
			}
		})
	}
	wg.Wait()
	for i, id := range todo {
		x.takes[id] = loaded[i]
	}
	for id := range x.takes {
		if _, ok := sigs[id]; !ok {
			delete(x.takes, id)
			changed = true
		}
	}
	if changed {
		x.order = slices.SortedFunc(maps.Keys(x.takes), order)
	}
	x.at = began
	x.loadLabels()
	return nil
}

// loadLabels reads the label file again when it changed in size or time
// since it was last read; x.mu is held. A file that cannot be read keeps
// the labels last read, and the list is shown without the new ones: the
// error is logged once.
func (x *Index) loadLabels() {
	if x.Labels == nil {
		return
	}
	sig := ""
	if st, err := os.Stat(x.Labels.Path); err == nil {
		sig = fmt.Sprintf("%d %d", st.Size(), st.ModTime().UnixNano())
	}
	if x.labels != nil && sig == x.labelSig {
		return
	}
	m, err := x.Labels.Latest()
	if err != nil {
		if err.Error() != x.labelErr {
			log.Printf("takes: labels: %v", err)
			x.labelErr = err.Error()
		}
		return
	}
	x.labels, x.labelSig, x.labelErr = m, sig, ""
}

// order sorts ids newest first: by stamp, then by the -N of a second take
// in the same second.
func order(a, b string) int {
	if c := cmp.Compare(b[:15], a[:15]); c != 0 {
		return c
	}
	return cmp.Compare(seq(b), seq(a))
}

func seq(id string) int {
	n, err := strconv.Atoi(strings.TrimPrefix(id[15:], "-"))
	if err != nil {
		return 1
	}
	return n
}

// List returns the rows that match q.
func (x *Index) List(q Query) (Page, error) {
	if err := x.refresh(false); err != nil {
		return Page{}, err
	}
	x.mu.Lock()
	defer x.mu.Unlock()
	needle := fold(strings.TrimSpace(q.Q))
	pg := Page{Takes: []Row{}, Days: []Day{}}
	days := map[string]int{} // key → index in pg.Days
	for _, e := range x.takes {
		if e.row.Counted {
			pg.Counted++
		}
	}
	if q.Facets {
		pg.Facets = x.facets()
	}
	for _, id := range x.order {
		e := x.takes[id]
		if q.State != "" && filterState(e.row) != q.State ||
			q.Target != "" && e.row.TargetKey != q.Target ||
			q.Device != "" && e.row.Device != q.Device ||
			q.Day != "" && e.row.Day != q.Day {
			continue
		}
		row := e.row
		_, row.Labeled = x.labels[id]
		if needle != "" {
			if row.Match = find(e.texts, needle); row.Match == nil {
				continue
			}
		}
		pg.Matched++
		i, ok := days[row.Day]
		if !ok {
			i = len(pg.Days)
			days[row.Day] = i
			pg.Days = append(pg.Days, Day{Key: row.Day})
		}
		pg.Days[i].N++
		if filterState(row) == "undelivered" {
			pg.Days[i].Undelivered++
		}
		switch {
		case q.Before != "" && takeID.MatchString(q.Before) && order(q.Before, id) >= 0:
		case len(pg.Takes) == q.N:
			pg.More = true
		default:
			pg.Takes = append(pg.Takes, row)
		}
	}
	slices.SortFunc(pg.Days, func(a, b Day) int { return cmp.Compare(b.Key, a.Key) })
	return pg, nil
}

// filterState is the state a take is filtered and counted by: its state, or
// dismissed for an undelivered take the user dismissed, so that 未送达
// counts only the takes still waiting on the user.
func filterState(r Row) string {
	if r.Dismissed {
		return "dismissed"
	}
	return r.State
}

// Undelivered is the ids of the takes undelivered and not dismissed, over
// the whole store, newest first.
func (x *Index) Undelivered() ([]string, error) {
	if err := x.refresh(false); err != nil {
		return nil, err
	}
	x.mu.Lock()
	defer x.mu.Unlock()
	var ids []string
	for _, id := range x.order {
		if filterState(x.takes[id].row) == "undelivered" {
			ids = append(ids, id)
		}
	}
	return ids, nil
}

// Counted is how many takes of the whole store are undelivered, not seen and
// not dismissed.
func (x *Index) Counted() (int, error) {
	if err := x.refresh(false); err != nil {
		return 0, err
	}
	x.mu.Lock()
	defer x.mu.Unlock()
	n := 0
	for _, e := range x.takes {
		if e.row.Counted {
			n++
		}
	}
	return n, nil
}

func (x *Index) facets() *Facets {
	f := &Facets{States: map[string]int{}}
	targets, devices, days := map[string]*Facet{}, map[string]*Facet{}, map[string]*Facet{}
	add := func(m map[string]*Facet, key, label string) {
		if key == "" {
			return
		}
		if m[key] == nil {
			m[key] = &Facet{Key: key, Label: label}
		}
		m[key].N++
	}
	for _, e := range x.takes {
		f.States[filterState(e.row)]++
		add(targets, e.row.TargetKey, targetLabel(e.row.Target))
		add(devices, e.row.Device, "")
		add(days, e.row.Day, "")
	}
	list := func(m map[string]*Facet, by func(a, b *Facet) int) []Facet {
		fs := slices.SortedFunc(maps.Values(m), by)
		out := make([]Facet, len(fs))
		for i, p := range fs {
			out[i] = *p
		}
		return out
	}
	most := func(a, b *Facet) int { return cmp.Or(cmp.Compare(b.N, a.N), cmp.Compare(a.Key, b.Key)) }
	f.Targets, f.Devices = list(targets, most), list(devices, most)
	f.Days = list(days, func(a, b *Facet) int { return cmp.Compare(b.Key, a.Key) })
	return f
}

// sentFrom is how many of the newest takes Sent reads.
const sentFrom = 500

// Sent is where the newest deliveries of the store went, newest first, each
// target once (by its TargetKey; a clipboard has none and is left out), n at
// most, read from the newest sentFrom takes.
func (x *Index) Sent(n int) ([]store.Target, error) {
	if err := x.refresh(false); err != nil {
		return nil, err
	}
	x.mu.Lock()
	var all []delivery
	for _, id := range x.order[:min(len(x.order), sentFrom)] {
		all = append(all, x.takes[id].sent...)
	}
	x.mu.Unlock()
	slices.SortStableFunc(all, func(a, b delivery) int { return b.at.Compare(a.at) })
	var out []store.Target
	seen := map[string]bool{}
	for _, d := range all {
		if k := targetKey(&d.to); k != "" && !seen[k] && len(out) < n {
			seen[k] = true
			out = append(out, d.to)
		}
	}
	return out, nil
}

// Take reads the take id from its files, and brings its entry up to date.
func (x *Index) Take(id string) (Take, bool) {
	if !takeID.MatchString(id) {
		return Take{}, false
	}
	t, texts := read(x.Dir, id)
	if t.Files == nil {
		return Take{}, false
	}
	x.mu.Lock()
	x.loadLabels()
	if l, ok := x.labels[id]; ok {
		t.Label, t.Labeled = &l, true
	}
	if x.takes != nil {
		if _, ok := x.takes[id]; !ok {
			x.at = time.Time{} // a take the index has not seen: the next list reads the directory
		} else {
			x.takes[id] = &entry{row: t.Row, texts: texts, sent: sentOf(t.Events)} // no sig: the next refresh reloads it
		}
	}
	x.mu.Unlock()
	return t, true
}

// matchLead is how many characters of text a match shows ahead of the query:
// the row's text column holds about 16, so the query stays in view.
const matchLead = 7

// find is the first of texts that holds needle, with the text around it.
func find(texts []text, needle string) *Match {
	for _, t := range texts {
		i := strings.Index(t.fold, needle)
		if i < 0 {
			continue
		}
		// fold maps rune to rune, so rune offsets agree between s and fold.
		rs := []rune(strings.ReplaceAll(t.s, "\n", " "))
		a := utf8.RuneCountInString(t.fold[:i])
		b := a + utf8.RuneCountInString(needle)
		m := &Match{Source: t.source, Hit: string(rs[a:b])}
		m.Before = string(rs[max(0, a-matchLead):a])
		if a > matchLead {
			m.Before = "…" + m.Before
		}
		m.After = string(rs[b:min(len(rs), b+48)])
		if len(rs) > b+48 {
			m.After += "…"
		}
		return m
	}
	return nil
}

// read reads the take id from its files: the whole take, and the texts
// search runs over. A take none of whose files exists has nil Files.
func read(dir, id string) (Take, []text) {
	base := filepath.Join(dir, id)
	t := Take{Row: Row{ID: id}, Answers: []Answer{}}
	for _, ext := range exts {
		if _, err := os.Stat(base + ext); err == nil {
			t.Files = append(t.Files, base+ext)
		}
	}
	evs, _ := store.Read(base)
	wav, track := wavOf(dir, id, evs)
	if track == "backup" {
		t.Files = append(t.Files, wav)
	}
	t.Track = track
	t.Events = evs
	if t.Events == nil {
		t.Events = []store.Event{}
	}
	st := store.State(evs)
	t.State, t.Why, t.Seen, t.Dismissed, t.Counted, t.Record = cmp.Or(st.State, "unknown"), st.Why, st.Seen, st.Dismissed, st.Counted(), len(evs) > 0
	t.At = store.Record{Base: base, Events: evs}.Start()
	if t.At.IsZero() {
		t.At, _ = time.ParseInLocation("20060102-150405", id[:15], time.Local)
	}
	t.Day = t.At.In(time.Local).Format("2006-01-02")
	for _, e := range evs {
		switch {
		case e.Ev == "start" && e.Target != nil:
			t.Target, t.TargetKey = e.Target, targetKey(e.Target)
		case e.Ev == "stop" && e.DurS > 0:
			t.DurS = e.DurS
		}
	}
	if st, err := os.Stat(base + ".wav"); err == nil {
		t.Audio = true
		t.DurS = float64(max(0, st.Size()-44)/2) / audio.Rate
	}
	t.DurS = float64(int(t.DurS*100+0.5)) / 100
	if in, err := store.LoadInput(base); err == nil {
		t.Input, t.Device = &in, in.DeviceName()
	}

	var texts []text
	add := func(source, s string) {
		if s = strings.TrimSpace(s); s != "" {
			texts = append(texts, text{source: source, s: s, fold: fold(s)})
		}
	}
	if b, err := os.ReadFile(base + ".txt"); err == nil {
		s := strings.TrimSpace(string(b))
		t.Text = &s
		add("delivered", s)
	}
	if b, err := os.ReadFile(base + ".raw.txt"); err == nil {
		s := strings.TrimSpace(string(b))
		t.Raw = &s
		add("raw", s)
	}
	if rec, _ := compare.ReadRecord(base + ".wav"); rec != nil {
		for _, name := range slices.Sorted(maps.Keys(rec.Engines)) {
			r := rec.Engines[name]
			t.Answers = append(t.Answers, Answer{Source: "compare", Engine: name, Text: r.Text, Raw: r.Raw, LatencyMS: r.LatencyMS, Err: r.Error,
				Delivered: name == rec.DeliveredBy, Primary: name == rec.Primary, Words: r.Words})
			add(name, r.Text)
			add(name, r.Raw)
		}
	}
	for _, e := range evs {
		switch e.Ev {
		case "retranscribe":
			t.Answers = append(t.Answers, Answer{Source: "retranscription", Engine: e.Engine, N: e.N, Model: e.Model, Text: e.Text, Raw: e.Raw, LatencyMS: e.LatencyMS, Err: e.Err})
			add(fmt.Sprintf("retranscription %d", e.N), e.Text)
			add(fmt.Sprintf("retranscription %d", e.N), e.Raw)
		case "deliver":
			add(fmt.Sprintf("deliver %d", e.N), e.Text)
		}
	}
	if len(texts) > 0 {
		line, _, _ := strings.Cut(texts[0].s, "\n")
		if rs := []rune(line); len(rs) > 120 {
			line = string(rs[:120]) + "…"
		}
		t.Line = line
	}
	return t, texts
}

// wavOf is the WAV a take's text came from and its track: the backup
// input's, backup/<id>.wav, when the record's last text line says audio
// backup (the main input delivered nothing) and that file exists; else the
// main WAV.
func wavOf(dir, id string, evs []store.Event) (path, track string) {
	return store.AudioOf(filepath.Join(dir, id), evs)
}

func targetKey(t *store.Target) string {
	switch {
	case t.Pane != "":
		return "pane:" + t.Pane
	case t.BundleID != "":
		return "app:" + t.BundleID
	case t.App != "":
		return "app:" + t.App
	}
	return ""
}

func targetLabel(t *store.Target) string {
	switch {
	case t == nil:
		return ""
	case t.Pane != "" && t.Workspace != "":
		return t.Workspace + " › " + t.Pane
	case t.Pane != "":
		return t.Pane
	}
	return cmp.Or(t.App, t.BundleID)
}
