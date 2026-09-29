// Package drain is megameet's server loop over the meetings registry
// (docs/megameet.md § The drain). A tick lists the queue index q.<id>, claims each
// claimable record with one compare-and-swap, and runs what it owes —
// pull, process, align, ingest — writing the record forward after each
// stage while a heartbeat keeps the claim alive. A lost compare-and-swap
// means another drain has the record; nothing runs twice.
//
// The drain owns state, stage, claim, action, attempts, error and wiki; the
// stages write their own fields (files, scores, engine) and the drain
// re-reads the record before every write, so neither overwrites the other.
package drain

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/caoer/mega-asr/internal/pages"
)

// Registry is the page's records, as *pages.Client serves them.
type Registry interface {
	List(ctx context.Context, prefix string) ([]pages.Object, error)
	Record(ctx context.Context, key string) (pages.Object, error)
	CAS(ctx context.Context, key, schema string, data any, version int) (int, error)
	Delete(ctx context.Context, key string) error
}

const (
	recSchema   = "meeting@1"
	queueSchema = "queue@1"
)

// Patch changes the claimed record's value; the drain folds it into its
// next write.
type Patch func(raw map[string]any) error

// Wiki is where an ingest filed the meeting.
type Wiki struct {
	Page   string `json:"page"`
	Commit string `json:"commit"`
}

// Stages are the work a claimed record owes, in order.
type Stages struct {
	// Pull materializes the record's files in a local directory, sha256
	// verified.
	Pull func(ctx context.Context, id string) (dir string, err error)
	// Process runs ASR over the pulled directory and publishes the results.
	Process func(ctx context.Context, id, dir string) error
	// Align pairs the record with the Feishu minute of the same meeting;
	// redo aligns again what is already aligned.
	Align func(ctx context.Context, id string, redo bool) (Patch, error)
	// Ingest files the meeting in the wiki; replace re-ingests one that is
	// already there. Nil when [meeting.ingest] auto is off.
	Ingest func(ctx context.Context, id, dir string, rec map[string]any, replace bool) (Wiki, error)
	// Summarize writes a processed meeting's display_title, its labels when
	// it has none, and its summary unless it is a Feishu minute (Feishu's
	// summary comes with it); never title. Nil writes none.
	Summarize func(ctx context.Context, id, dir string, rec map[string]any) (Patch, error)
	// IngestStatus is the running ingest's progress for id, as the registry
	// record's `ingest` key carries it; ok is false while there is none.
	// Nil mirrors nothing.
	IngestStatus func(id string) (status map[string]any, ok bool)
	// Resolve turns the speakers named by hand on record id (raw), role →
	// name, into wiki person pages: one row per name, person "" when the
	// name is ambiguous. Nil resolves none ([meeting.ingest] people_resolve).
	Resolve func(ctx context.Context, id string, raw map[string]any, names map[string]string) ([]Person, error)
}

// Drain is one server's loop.
type Drain struct {
	Reg    Registry
	Stages Stages
	By     string // claim.by: this host and process

	Lease   time.Duration // a claim's life without a heartbeat
	Beat    time.Duration // heartbeat period
	Abandon time.Duration // an upload older than this is failed: abandoned
	Mirror  time.Duration // how often a running ingest's progress is copied onto its record
	Sweep   time.Duration // how often the abandoned sweep lists every record
	// PeopleRetry is how long a failed resolve of a named speaker waits
	// before the next.
	PeopleRetry time.Duration

	// IngestSources are the sources whose records the ingest takes on its
	// own; others rest at aligned until a reingest action asks for it.
	IngestSources []string
	// FeishuSince: a feishu record is taken on its own only when Feishu
	// created the minute at or after it and FeishuEligible passes; the
	// minutes before it are the backfill's. Zero takes none.
	FeishuSince time.Time
	// MinDuration: a record of another source shorter than it rests at
	// aligned until a reingest action asks for it. Zero takes any length.
	MinDuration time.Duration

	// Forget removes this host's copies of a deleted record (its pulled
	// files); the sweep calls it for every tombstone. Nil keeps them.
	Forget func(id string)

	Max    int               // claim at most this many records a tick; 0 is no limit
	State  string            // the state file `status` reads; "" keeps none
	Notify func(line string) // one line per failed record; may be nil
	Logf   func(format string, args ...any)
	Now    func() time.Time
}

// New is a drain with the default timings: a 30 min lease renewed every
// 5 min, uploads abandoned after 24 h, swept hourly.
func New(reg Registry, st Stages, by string) *Drain {
	return &Drain{Reg: reg, Stages: st, By: by, IngestSources: []string{"mac", "room", "phone", "file"},
		Lease: 30 * time.Minute, Beat: 5 * time.Minute, Abandon: 24 * time.Hour, Sweep: time.Hour,
		Mirror: 15 * time.Second, PeopleRetry: time.Hour, Now: time.Now}
}

// Result is what a tick did with one queued record.
type Result struct {
	ID      string  `json:"id"`
	Job     string  `json:"job,omitempty"` // full | realign | reingest | ingest
	Outcome string  `json:"outcome"`       // aligned | ingested | parked | failed | lost | skipped | removed
	Error   string  `json:"error,omitempty"`
	Secs    float64 `json:"secs,omitempty"`
}

// Run is one tick.
type Run struct {
	By      string    `json:"by"`
	Started time.Time `json:"started"`
	Ended   time.Time `json:"ended"`
	Queued  int       `json:"queued"`
	Results []Result  `json:"results"`
	Error   string    `json:"error,omitempty"`
}

// State is the drain's state file.
type State struct {
	LastSweep time.Time `json:"last_sweep"`
	Last      *Run      `json:"last,omitempty"`
	// People are the named speakers the resolve left unsettled, by
	// <id>/<role>.
	People map[string]PeopleTry `json:"people,omitempty"`
}

// Tick runs one pass: the abandoned sweep when due, the named speakers'
// people when Resolve is set, then every queued record once, newest first.
func (d *Drain) Tick(ctx context.Context) (Run, error) {
	st := d.load()
	run := Run{By: d.By, Started: d.Now()}
	if d.Now().Sub(st.LastSweep) >= d.Sweep {
		if err := d.sweep(ctx); err != nil {
			d.logf("sweep: %v", err)
		} else {
			st.LastSweep = d.Now()
		}
	}
	if d.Stages.Resolve != nil {
		st.People = d.people(ctx, st.People)
	}
	// Newest first, the queue listed again after each record: a meeting
	// uploaded while a long backfill runs is next, not last. A queue key made
	// again since it was tried (a page action) is new work.
	tried := map[string]int64{} // id → the q.<id> updated_at it was tried at
	claimed := 0
	var err error
	for ctx.Err() == nil && (d.Max == 0 || claimed < d.Max) {
		var qs []pages.Object
		if qs, err = d.Reg.List(ctx, "q."); err != nil {
			run.Error = err.Error()
			break
		}
		if run.Queued == 0 {
			run.Queued = len(qs)
		}
		id := ""
		for i := len(qs) - 1; i >= 0; i-- {
			k := strings.TrimPrefix(qs[i].Key, "q.")
			if at, ok := tried[k]; !ok || at != qs[i].UpdatedAt {
				id, tried[k] = k, qs[i].UpdatedAt
				break
			}
		}
		if id == "" {
			break
		}
		r := d.one(ctx, id)
		if r.Job != "" {
			claimed++
		}
		if r.Outcome != "skipped" {
			d.logf("%s: %s %s%s", r.ID, r.Job, r.Outcome, suffix(r.Error))
		}
		run.Results = append(run.Results, r)
	}
	run.Ended = d.Now()
	st.Last = &run
	d.save(st)
	return run, err
}

func suffix(e string) string {
	if e == "" {
		return ""
	}
	return ": " + e
}

// view is the part of a record the drain decides on.
type view struct {
	ID       string `json:"id"`
	Source   string `json:"source"`
	Title    string `json:"title"`
	State    string `json:"state"`
	Action   string `json:"action"`
	Attempts int    `json:"attempts"`
	Claim    *struct {
		By      string    `json:"by"`
		At      string    `json:"at"`
		Expires time.Time `json:"expires"`
	} `json:"claim"`
	Wiki      *Wiki   `json:"wiki"`
	Updated   string  `json:"updated"`
	Started   string  `json:"started"`
	DurationS float64 `json:"duration_s"`
	Test      bool    `json:"test"`
}

func decode(o pages.Object) (map[string]any, view, error) {
	var raw map[string]any
	var v view
	if err := json.Unmarshal(o.Value.Data, &raw); err != nil {
		return nil, v, fmt.Errorf("%s: %w", o.Key, err)
	}
	if err := json.Unmarshal(o.Value.Data, &v); err != nil {
		return nil, v, fmt.Errorf("%s: %w", o.Key, err)
	}
	return raw, v, nil
}

// job is what a claimable record owes; "" when it owes nothing now.
func (d *Drain) job(v view) (job, why string) {
	if v.State == "deleted" {
		return "", "deleted" // a tombstone: never claimed, whatever its action says
	}
	now := d.Now()
	if v.Claim != nil && v.Claim.Expires.After(now) {
		return "", "claimed by " + v.Claim.By + " until " + v.Claim.Expires.Format(time.RFC3339)
	}
	after := v.State == "aligned" || v.State == "ingested"
	switch {
	case v.Action == "realign" && after:
		return "realign", ""
	case v.Action == "reingest" && after:
		return "reingest", ""
	case v.Action != "":
		return "full", ""
	case v.State == "uploaded":
		return "full", ""
	case v.State == "processing":
		return "full", "" // its claim expired: the host that had it died
	case v.State == "aligned" && v.Claim != nil:
		return "ingest", "" // died while ingesting
	}
	return "", "state " + v.State
}

// one claims queued record id and runs its job.
func (d *Drain) one(ctx context.Context, id string) Result {
	res := Result{ID: id}
	key := "rec." + id
	o, err := d.Reg.Record(ctx, key)
	if isAbsent(err) {
		d.unqueue(ctx, id)
		res.Outcome, res.Error = "removed", "no record: queue key dropped"
		return res
	}
	if err != nil {
		res.Outcome, res.Error = "skipped", err.Error()
		return res
	}
	raw, v, err := decode(o)
	if err != nil {
		res.Outcome, res.Error = "skipped", err.Error()
		return res
	}
	job, why := d.job(v)
	if job == "" {
		res.Outcome, res.Error = "skipped", why
		if v.State == "deleted" || v.Claim == nil && v.Action == "" && (v.State == "ingested" || v.State == "failed" || v.State == "aligned") {
			d.unqueue(ctx, id) // nothing left to do: a stale queue key
			res.Outcome = "removed"
		}
		if v.State == "deleted" && d.Forget != nil {
			d.Forget(id)
		}
		return res
	}
	res.Job = job

	// The claim: one compare-and-swap on the version just read.
	now := d.Now()
	at := now.UTC().Format(time.RFC3339Nano)
	raw["claim"] = map[string]any{"by": d.By, "at": at, "expires": now.Add(d.Lease).UTC().Format(time.RFC3339Nano)}
	delete(raw, "action")
	delete(raw, "error")
	raw["attempts"] = v.Attempts + 1
	if job == "full" {
		raw["state"], raw["stage"] = "processing", "pull"
	} else {
		raw["stage"] = map[string]string{"realign": "align", "reingest": "ingest", "ingest": "ingest"}[job]
	}
	raw["updated"] = at
	if _, err := d.Reg.CAS(ctx, key, recSchema, raw, o.Version); err != nil {
		if pages.Code(err) == "version_conflict" {
			d.logf("%s: claim lost (409 version_conflict): another drain has it", key)
			res.Outcome = "lost"
			return res
		}
		res.Outcome, res.Error = "skipped", "claim: "+err.Error()
		return res
	}
	d.logf("%s: claimed for %s (attempt %d)", key, job, v.Attempts+1)

	c := &claim{d: d, key: key, at: at}
	wctx, cancel := context.WithCancelCause(ctx)
	defer cancel(nil)
	stop := c.heartbeat(wctx, cancel)
	t0 := time.Now()
	fin, stage, werr := d.work(wctx, c, job, v)
	stop()
	res.Secs = time.Since(t0).Seconds()
	if errors.Is(werr, errLost) || (werr != nil && errors.Is(context.Cause(wctx), errLost)) {
		res.Outcome, res.Error = "lost", "the claim passed to another drain"
		return res
	}
	if werr != nil && ctx.Err() != nil {
		// Stopped (SIGTERM, a deploy): not the record's fault. Let the claim
		// lapse now so the next drain takes it over at once.
		res.Outcome, res.Error = "interrupted", werr.Error()
		if err := c.write(context.Background(), func(raw map[string]any) error {
			cl, _ := raw["claim"].(map[string]any)
			cl["expires"] = d.Now().UTC().Format(time.RFC3339Nano)
			return nil
		}); err != nil {
			res.Error += "; the claim stays until it expires: " + err.Error()
		}
		return res
	}
	if werr != nil {
		res.Outcome, res.Error = "failed", werr.Error()
		if err := c.write(ctx, func(raw map[string]any) error {
			raw["state"], raw["stage"], raw["error"] = "failed", stage, werr.Error()
			delete(raw, "claim")
			return nil
		}); err != nil {
			res.Error += "; and the failed state was not written: " + err.Error()
		}
		d.unqueue(ctx, id)
		d.notify(fmt.Sprintf("megameet: %s failed at %s: %s (%s)", title(v), stage, oneLine(werr.Error()), d.By))
		return res
	}
	res.Outcome = fin.outcome
	if err := c.write(ctx, func(raw map[string]any) error {
		if fin.patch != nil {
			if err := fin.patch(raw); err != nil {
				return err
			}
		}
		if fin.state != "" {
			raw["state"] = fin.state
		}
		delete(raw, "stage")
		delete(raw, "claim")
		return nil
	}); err != nil {
		res.Outcome, res.Error = "failed", "final write: "+err.Error()
		return res
	}
	if fin.unqueue {
		d.unqueue(ctx, id)
	}
	return res
}

func title(v view) string {
	if v.Title != "" {
		return fmt.Sprintf("rec.%s «%s»", v.ID, v.Title)
	}
	return "rec." + v.ID
}

func oneLine(s string) string {
	s = strings.Join(strings.Fields(s), " ")
	if len(s) > 300 {
		s = s[:300] + "…"
	}
	return s
}

// finish is the record's last write after a successful job.
type finish struct {
	state   string // "" keeps it
	patch   Patch
	outcome string
	unqueue bool
}

// work runs a job's stages; on failure it names the stage that failed.
func (d *Drain) work(ctx context.Context, c *claim, job string, v view) (finish, string, error) {
	st := d.Stages
	switch job {
	case "realign":
		p, err := st.Align(ctx, v.ID, true)
		if err != nil {
			return finish{}, "align", err
		}
		return finish{patch: p, outcome: "realigned", unqueue: true}, "", nil
	case "reingest", "ingest":
		if v.Test {
			return finish{patch: skipped(testWhy), outcome: "parked", unqueue: true}, "", nil
		}
		if st.Ingest == nil || (job == "ingest" && !slices.Contains(d.IngestSources, v.Source)) {
			return finish{outcome: "parked", unqueue: true}, "", nil
		}
		dir, err := st.Pull(ctx, v.ID)
		if err != nil {
			return finish{}, "pull", err
		}
		if job == "ingest" {
			if ok, why := d.autoIngest(v, dir); !ok {
				return finish{patch: skipped(why), outcome: "parked", unqueue: true}, "", nil
			}
		}
		return d.ingest(ctx, c, dir, job == "reingest")
	}
	dir, err := st.Pull(ctx, v.ID)
	if err != nil {
		return finish{}, "pull", err
	}
	if err := c.write(ctx, stageTo("asr")); err != nil {
		return finish{}, "pull", err
	}
	if err := st.Process(ctx, v.ID, dir); err != nil {
		return finish{}, "asr", err
	}
	d.summarize(ctx, c, v, dir)
	if err := c.write(ctx, stageTo("align")); err != nil {
		return finish{}, "asr", err
	}
	p, err := st.Align(ctx, v.ID, false)
	if err != nil {
		return finish{}, "align", err
	}
	aligned := func(raw map[string]any) error {
		if p != nil {
			if err := p(raw); err != nil {
				return err
			}
		}
		raw["state"] = "aligned"
		return nil
	}
	switch {
	case v.Wiki != nil:
		// Already in the wiki (a minute the backfill filed): never again
		// unless someone asks for reingest.
		return finish{patch: aligned, state: "ingested", outcome: "ingested (in the wiki already)", unqueue: true}, "", nil
	case st.Ingest == nil:
		return finish{patch: aligned, outcome: "aligned", unqueue: true}, "", nil
	}
	if ok, why := d.autoIngest(v, dir); !ok {
		return finish{outcome: "aligned", unqueue: true, patch: func(raw map[string]any) error {
			if err := aligned(raw); err != nil {
				return err
			}
			return skipped(why)(raw)
		}}, "", nil
	}
	if err := c.write(ctx, func(raw map[string]any) error {
		raw["stage"] = "ingest"
		return aligned(raw)
	}); err != nil {
		return finish{}, "align", err
	}
	return d.ingest(ctx, c, dir, false)
}

// summarize writes the processed meeting's display title, labels and
// summary before the ingest reads the record. A failure is logged and
// reported, never the record's: what it would write stays absent.
func (d *Drain) summarize(ctx context.Context, c *claim, v view, dir string) {
	if d.Stages.Summarize == nil {
		return
	}
	err := func() error {
		o, err := d.Reg.Record(ctx, c.key)
		if err != nil {
			return err
		}
		raw, _, err := decode(o)
		if err != nil {
			return err
		}
		p, err := d.Stages.Summarize(ctx, v.ID, dir, raw)
		if err != nil {
			return err
		}
		return c.write(ctx, p)
	}()
	if err == nil || errors.Is(err, errLost) || ctx.Err() != nil {
		return // a lost claim or a stop shows at the next write
	}
	d.logf("%s: summary: %v", c.key, err)
	d.notify(fmt.Sprintf("megameet: %s not titled or summarized: %s (%s)", title(v), oneLine(err.Error()), d.By))
}

// ingest runs the ingest, once more after a failure, and writes wiki.
func (d *Drain) ingest(ctx context.Context, c *claim, dir string, replace bool) (finish, string, error) {
	var w Wiki
	var err error
	for try := 0; try < 2; try++ {
		var o pages.Object
		if o, err = d.Reg.Record(ctx, c.key); err != nil {
			continue
		}
		var raw map[string]any
		if raw, _, err = decode(o); err != nil {
			break
		}
		stop := d.mirror(ctx, c)
		w, err = d.Stages.Ingest(ctx, strings.TrimPrefix(c.key, "rec."), dir, raw, replace || raw["wiki"] != nil)
		stop()
		if err == nil {
			break
		}
		d.logf("%s: ingest attempt %d: %v", c.key, try+1, err)
		if ctx.Err() != nil {
			break
		}
	}
	if err != nil {
		return finish{}, "ingest", err
	}
	return finish{state: "ingested", outcome: "ingested", unqueue: true, patch: func(raw map[string]any) error {
		raw["wiki"] = map[string]any{"page": w.Page, "commit": w.Commit}
		delete(raw, "ingest_skipped")
		return nil
	}}, "", nil
}

// mirror copies the running ingest's progress onto the record's `ingest`
// key every Mirror until stop, which copies it once more so the last state
// lands. Each copy is a claim write: re-read, CAS, only that key changed
// (and `updated`, as every drain write). A copy that fails is logged and the
// next carries the state.
func (d *Drain) mirror(ctx context.Context, c *claim) (stop func()) {
	if d.Stages.IngestStatus == nil {
		return func() {}
	}
	id := strings.TrimPrefix(c.key, "rec.")
	var last string
	copyOnce := func() {
		s, ok := d.Stages.IngestStatus(id)
		if !ok {
			return
		}
		b, _ := json.Marshal(s)
		if string(b) == last {
			return
		}
		err := c.write(ctx, func(raw map[string]any) error { raw["ingest"] = s; return nil })
		if err != nil {
			if !errors.Is(err, errLost) && ctx.Err() == nil {
				d.logf("%s: ingest mirror: %v", c.key, err)
			}
			return
		}
		last = string(b)
	}
	every := d.Mirror
	if every <= 0 {
		every = 15 * time.Second
	}
	done := make(chan struct{})
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		t := time.NewTicker(every)
		defer t.Stop()
		for {
			select {
			case <-done:
				return
			case <-ctx.Done():
				return
			case <-t.C:
				copyOnce()
			}
		}
	}()
	return func() { close(done); wg.Wait(); copyOnce() }
}

// testWhy is ingest_skipped on a test record: the drain never ingests one,
// not even on a reingest action.
const testWhy = "a test record (test: true): never ingested"

// autoIngest says whether the ingest takes v's record, pulled into dir,
// without being asked; why names the reason when a record of a source it
// takes rests at aligned instead ("" for a source it does not take).
func (d *Drain) autoIngest(v view, dir string) (ok bool, why string) {
	if v.Test {
		return false, testWhy
	}
	if !slices.Contains(d.IngestSources, v.Source) {
		return false, ""
	}
	if v.Source != "feishu" {
		if v.DurationS < d.MinDuration.Seconds() {
			return false, fmt.Sprintf("%.0f s: shorter than %s", v.DurationS, minutes(d.MinDuration))
		}
		return true, ""
	}
	if d.FeishuSince.IsZero() {
		return false, "no meeting.ingest.feishu_since: every Feishu minute is the backfill's"
	}
	created, err := time.Parse(time.RFC3339, v.Started)
	if err != nil {
		return false, fmt.Sprintf("started %q: %v", v.Started, err)
	}
	if created.Before(d.FeishuSince) {
		return false, "created before meeting.ingest.feishu_since " + d.FeishuSince.Format(time.RFC3339) + ": the backfill's"
	}
	return FeishuEligible(dir, v.DurationS)
}

// FeishuEligible is the backfill's rule for a Feishu minute pulled into dir:
// 5 min or more, Feishu's
// transcript, and two speaker labels or more in it — a solo recording has
// one.
func FeishuEligible(dir string, durationS float64) (bool, string) {
	if durationS < 300 {
		return false, fmt.Sprintf("%.0f s: shorter than 5 min", durationS)
	}
	var t struct {
		Segments []struct {
			Speaker string `json:"speaker"`
		} `json:"segments"`
	}
	if err := readJSON(filepath.Join(dir, "feishu-transcript.json"), &t); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return false, "no Feishu transcript"
		}
		return false, err.Error()
	}
	speakers := map[string]bool{}
	for _, s := range t.Segments {
		speakers[s.Speaker] = true
	}
	if len(speakers) < 2 {
		return false, fmt.Sprintf("%d speaker(s) in Feishu's transcript: a solo recording", len(speakers))
	}
	return true, ""
}

// minutes reads a whole number of minutes as FeishuEligible's reason does
// ("5 min"); any other length in Go's form.
func minutes(d time.Duration) string {
	if d%time.Minute == 0 {
		return fmt.Sprintf("%d min", d/time.Minute)
	}
	return d.String()
}

// skipped writes why a record the ingest could take rests at aligned.
func skipped(why string) Patch {
	return func(raw map[string]any) error {
		if why != "" {
			raw["ingest_skipped"] = why
		}
		return nil
	}
}

func stageTo(s string) Patch {
	return func(raw map[string]any) error { raw["stage"] = s; return nil }
}

var errLost = errors.New("claim lost")

// claim is a record this drain holds: every write re-reads it, checks the
// claim is still this one, and swaps it at the version read.
type claim struct {
	d   *Drain
	key string
	at  string // claim.at as written: with By, the claim's identity
	mu  sync.Mutex
}

func (c *claim) write(ctx context.Context, p Patch) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	for try := 0; ; try++ {
		o, err := c.d.Reg.Record(ctx, c.key)
		if err != nil {
			return err
		}
		raw, v, err := decode(o)
		if err != nil {
			return err
		}
		if v.Claim == nil || v.Claim.By != c.d.By || v.Claim.At != c.at {
			return errLost
		}
		if err := p(raw); err != nil {
			return err
		}
		raw["updated"] = c.d.Now().UTC().Format(time.RFC3339Nano)
		_, err = c.d.Reg.CAS(ctx, c.key, recSchema, raw, o.Version)
		if pages.Code(err) == "version_conflict" && try < 5 {
			continue // a stage or the page wrote in between
		}
		return err
	}
}

// heartbeat renews the claim every Beat until stop; a lost claim cancels
// the work.
func (c *claim) heartbeat(ctx context.Context, cancel context.CancelCauseFunc) (stop func()) {
	done := make(chan struct{})
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		t := time.NewTicker(c.d.Beat)
		defer t.Stop()
		for {
			select {
			case <-done:
				return
			case <-ctx.Done():
				return
			case <-t.C:
				err := c.write(ctx, func(raw map[string]any) error {
					cl, _ := raw["claim"].(map[string]any)
					cl["expires"] = c.d.Now().Add(c.d.Lease).UTC().Format(time.RFC3339Nano)
					return nil
				})
				if errors.Is(err, errLost) {
					c.d.logf("%s: heartbeat: the claim is no longer ours; stopping", c.key)
					cancel(errLost)
					return
				}
				if err != nil {
					c.d.logf("%s: heartbeat: %v", c.key, err)
				}
			}
		}
	}()
	return func() { close(done); wg.Wait() }
}

// sweep marks every upload older than Abandon failed: abandoned, and
// forgets every tombstone's local copies.
func (d *Drain) sweep(ctx context.Context) error {
	objs, err := d.Reg.List(ctx, "rec.")
	if err != nil {
		return err
	}
	now := d.Now()
	for _, o := range objs {
		raw, v, err := decode(o)
		if err == nil && v.State == "deleted" && d.Forget != nil {
			d.Forget(v.ID)
		}
		if err != nil || v.State != "uploading" {
			continue
		}
		since := parseTime(v.Updated)
		if since.IsZero() {
			since = parseTime(v.Started)
		}
		if since.IsZero() || now.Sub(since) < d.Abandon {
			continue
		}
		raw["state"], raw["error"] = "failed", "abandoned"
		raw["updated"] = now.UTC().Format(time.RFC3339Nano)
		if _, err := d.Reg.CAS(ctx, o.Key, recSchema, raw, o.Version); err != nil {
			d.logf("sweep %s: %v", o.Key, err)
			continue
		}
		d.logf("sweep %s: uploading since %s: failed: abandoned", o.Key, since.Format(time.RFC3339))
	}
	return nil
}

func parseTime(s string) time.Time {
	t, _ := time.Parse(time.RFC3339Nano, s)
	return t
}

func (d *Drain) unqueue(ctx context.Context, id string) {
	if err := d.Reg.Delete(ctx, "q."+id); err != nil && !isAbsent(err) {
		d.logf("q.%s: %v", id, err)
	}
}

func isAbsent(err error) bool {
	var e *pages.Error
	return errors.As(err, &e) && (e.Status == 404 || e.Code == "no_such_object" || e.Code == "not_found")
}

func (d *Drain) notify(line string) {
	d.logf("notify: %s", line)
	if d.Notify != nil {
		d.Notify(line)
	}
}

func (d *Drain) logf(format string, args ...any) {
	if d.Logf != nil {
		d.Logf(format, args...)
	}
}

// Load reads a state file; a missing or unreadable one is empty.
func Load(path string) State {
	var st State
	if path == "" {
		return st
	}
	if b, err := os.ReadFile(path); err == nil {
		_ = json.Unmarshal(b, &st)
	}
	return st
}

func (d *Drain) load() State { return Load(d.State) }

func (d *Drain) save(st State) {
	if d.State == "" {
		return
	}
	b, _ := json.MarshalIndent(st, "", "  ")
	if err := os.MkdirAll(filepath.Dir(d.State), 0o700); err == nil {
		tmp := d.State + ".tmp"
		if err = os.WriteFile(tmp, append(b, '\n'), 0o600); err == nil {
			err = os.Rename(tmp, d.State)
		}
		if err != nil {
			d.logf("state: %v", err)
		}
	}
}
