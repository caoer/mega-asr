package drain

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"
)

// Ingester runs the ingest (meeting.ingest.command) over a pulled directory: it writes
// the ingest's recording record, runs
// `<command> run <record.json> --out <run> --commit`, reads where the
// meeting was filed, and keeps the registry record beside the audio in the
// assets checkout.
type Ingester struct {
	Command []string // the ingest program; the run's arguments are appended
	Repos   string   // passed to the ingest as REPOS_ROOT, the root it finds its repositories under; "" passes none
	// Wiki is the name whose `commit:` line the drain reads when the run's
	// meeting.json names no primary wiki; "" reads wikis[] only.
	Wiki string
	// Assets is the git-annex checkout the registry record is backed up
	// into, through AssetsRemote's main; "" backs up nothing.
	// AssetsAnnexRemote, when set, receives the backup's content.
	Assets, AssetsRemote, AssetsAnnexRemote string
	Runs                                    string // run folders, <runs>/<id>/<stamp>
	People                                  string // people resolve folders, <people>/<id>/<stamp>
	// Filed says whether the wiki holds a Feishu minute already, and where;
	// a feishu record the wiki holds is not ingested again unless replace.
	// Nil checks nothing.
	Filed  func(token string) (Wiki, bool, error)
	Notify func(line string) // each ingested meeting (its page and questions), and a backup that did not land; may be nil
	Logf   func(format string, args ...any)
	Now    func() time.Time

	mu  sync.Mutex
	out map[string]string // id → its current run folder, for Status
}

// Record is the ingest's input.
type Record struct {
	ID        string            `json:"id"`
	Source    string            `json:"source"`
	Title     string            `json:"title"`
	Started   string            `json:"started"`
	DurationS float64           `json:"duration_s,omitempty"`
	Engine    string            `json:"engine"`
	Speakers  map[string]string `json:"speakers,omitempty"` // label → name
	Persons   map[string]string `json:"-"`                  // label → person page slug, where the record names one
	Turns     []Turn            `json:"turns"`
	Media     []Media           `json:"media"`
	URL       string            `json:"url,omitempty"` // the original: a Feishu minute's page
}

// MarshalJSON writes speakers as the ingest reads them: label → name, or
// {name, person} for a label with a person page ({person} with no name).
func (r Record) MarshalJSON() ([]byte, error) {
	type plain Record
	out := struct {
		plain
		Speakers map[string]any `json:"speakers,omitempty"`
	}{plain: plain(r)}
	for label, p := range r.Persons {
		if out.Speakers == nil {
			out.Speakers = map[string]any{}
		}
		if name := r.Speakers[label]; name != "" {
			out.Speakers[label] = map[string]string{"name": name, "person": p}
		} else {
			out.Speakers[label] = map[string]string{"person": p}
		}
	}
	for label, name := range r.Speakers {
		if _, ok := out.Speakers[label]; !ok {
			if out.Speakers == nil {
				out.Speakers = map[string]any{}
			}
			out.Speakers[label] = name
		}
	}
	return json.Marshal(out)
}

type Turn struct {
	Speaker string  `json:"speaker"`
	StartS  float64 `json:"start_s"`
	Text    string  `json:"text"`
}

type Media struct {
	Name string `json:"name"`
	Ref  string `json:"ref"`
}

// BuildRecord makes the ingest's record from a pulled directory and the
// registry record: Feishu's turns when the minute carries them (Feishu's
// transcript is canonical for ingest), else the local segments; one media
// entry per track. A speaker is its name, or {name, person} when the record
// names its person page, so the ingest takes that page as given.
func BuildRecord(dir string, rec map[string]any) (Record, error) {
	str := func(k string) string { s, _ := rec[k].(string); return s }
	r := Record{ID: str("id"), Source: str("source"), Title: str("title"), Started: str("started"), Engine: engineName(rec)}
	if r.Title == "" {
		r.Title = r.ID
	}
	r.DurationS, _ = rec["duration_s"].(float64)
	if fe, _ := rec["feishu"].(map[string]any); r.Source == "feishu" {
		r.URL, _ = fe["url"].(string)
	}
	if sp, ok := rec["speakers"].([]any); ok {
		for _, x := range sp {
			m, _ := x.(map[string]any)
			name, _ := m["name"].(string)
			role, _ := m["role"].(string)
			person, _ := m["person"].(string)
			if role == "" || name == "" && person == "" {
				continue
			}
			if name != "" {
				if r.Speakers == nil {
					r.Speakers = map[string]string{}
				}
				r.Speakers[role] = name
			}
			if person != "" {
				if r.Persons == nil {
					r.Persons = map[string]string{}
				}
				r.Persons[role] = person
			}
		}
	}
	var err error
	if ft := filepath.Join(dir, "feishu-transcript.json"); r.Source == "feishu" && exists(ft) {
		var t struct {
			Segments []Turn `json:"segments"`
		}
		err = readJSON(ft, &t)
		r.Turns = t.Segments
		r.Engine = "Feishu Minutes"
	} else {
		var segs []struct {
			Speaker string  `json:"speaker"`
			Track   string  `json:"track"`
			StartS  float64 `json:"start_s"`
			Text    string  `json:"text"`
		}
		err = readJSON(filepath.Join(dir, "segments.json"), &segs)
		for _, s := range segs {
			sp := s.Speaker
			if sp == "" {
				sp = s.Track
			}
			r.Turns = append(r.Turns, Turn{Speaker: sp, StartS: s.StartS, Text: s.Text})
		}
	}
	if err != nil {
		return r, err
	}
	r.Turns = nonEmpty(r.Turns)
	if len(r.Turns) == 0 {
		return r, errors.New("no turns to ingest: the transcript is empty")
	}
	sort.SliceStable(r.Turns, func(i, j int) bool { return r.Turns[i].StartS < r.Turns[j].StartS })
	tracks, _ := filepath.Glob(filepath.Join(dir, "tracks", "*"))
	sort.Strings(tracks)
	for _, p := range tracks {
		role := strings.TrimSuffix(filepath.Base(p), filepath.Ext(p))
		if role == "raw" || role == "doa" {
			continue // the room box's 4-channel raw and azimuths are training material, not the meeting's audio
		}
		name := role
		if len(tracks) == 1 {
			name = ""
		}
		r.Media = append(r.Media, Media{Name: name, Ref: p})
	}
	return r, nil
}

func engineName(rec map[string]any) string {
	e, _ := rec["engine"].(map[string]any)
	root, _ := e["root"].(string)
	if root == "" {
		return "megameet funasr-nano"
	}
	s := "megameet " + filepath.Base(root)
	if sha, _ := e["sha"].(string); sha != "" {
		s += "@" + sha
	}
	return s
}

func nonEmpty(ts []Turn) []Turn {
	out := ts[:0]
	for _, t := range ts {
		if strings.TrimSpace(t.Text) != "" {
			out = append(out, t)
		}
	}
	return out
}

// commitRE is one wiki's commit line on the ingest's stdout,
// `commit: <wiki-name> <sha>`.
var commitRE = regexp.MustCompile(`(?m)^commit: (\S+) ([0-9a-f]{7,40})\s*$`)

// runMeeting is what the drain reads from the run's meeting.json.
type runMeeting struct {
	Page   string   `json:"page"`
	Bundle string   `json:"bundle"`
	Audio  []string `json:"audio"`
	Wikis  []struct {
		Name   string `json:"name"`
		Role   string `json:"role"`
		Commit string `json:"commit"`
	} `json:"wikis"`
}

// primaryCommit is the commit the meeting's page landed in: the wikis[]
// entry with role primary (its commit, else its name's commit line), and
// with no such entry the commit line of fallback, the configured wiki's
// name (an ingest that writes no wikis[]).
func primaryCommit(stdout string, m runMeeting, fallback string) (wiki, sha string, ok bool) {
	wiki = fallback
	for _, w := range m.Wikis {
		if w.Role == "primary" && w.Name != "" {
			if w.Commit != "" {
				return w.Name, w.Commit, true
			}
			wiki = w.Name
			break
		}
	}
	for _, l := range commitRE.FindAllStringSubmatch(stdout, -1) {
		if wiki != "" && l[1] == wiki {
			return wiki, l[2], true
		}
	}
	return wiki, "", false
}

// Ingest files the pulled meeting in the wiki. A run folder without a
// wiki.json is resumed (the ingest picks up after its last finished pass);
// otherwise a new one starts.
func (in *Ingester) Ingest(ctx context.Context, id, dir string, rec map[string]any, replace bool) (Wiki, error) {
	if fs, _ := rec["feishu"].(map[string]any); rec["source"] == "feishu" && !replace && in.Filed != nil {
		token, _ := fs["token"].(string)
		w, filed, err := in.Filed(token)
		if err != nil {
			return Wiki{}, fmt.Errorf("is minute %s filed already: %w", token, err)
		}
		if filed {
			in.logf("ingest %s: the backfill filed minute %s as %s: not ingested again", id, token, w.Page)
			return w, nil
		}
	}
	r, err := BuildRecord(dir, rec)
	if err != nil {
		return Wiki{}, err
	}
	out, err := in.runDir(id)
	if err != nil {
		return Wiki{}, err
	}
	in.mu.Lock()
	if in.out == nil {
		in.out = map[string]string{}
	}
	in.out[id] = out
	in.mu.Unlock()
	src := filepath.Join(out, "record.json")
	b, _ := json.MarshalIndent(r, "", " ")
	if err := os.WriteFile(src, b, 0o600); err != nil {
		return Wiki{}, err
	}
	args := append(append([]string(nil), in.Command[1:]...), "run", src, "--out", out, "--commit")
	if replace {
		args = append(args, "--replace")
	}
	cmd := exec.CommandContext(ctx, in.Command[0], args...)
	cmd.Env = os.Environ()
	if in.Repos != "" {
		cmd.Env = append(cmd.Env, "REPOS_ROOT="+in.Repos)
	}
	logf, err := os.OpenFile(filepath.Join(out, "ingest.log"), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return Wiki{}, err
	}
	defer logf.Close()
	var stdout bytes.Buffer
	var tail tailBuf
	cmd.Stdout = io.MultiWriter(&stdout, logf, &tail)
	cmd.Stderr = io.MultiWriter(logf, &tail)
	in.logf("ingest %s: %s (run folder %s)", id, strings.Join(cmd.Args, " "), out)
	if err := cmd.Run(); err != nil {
		return Wiki{}, fmt.Errorf("ingest: %v: %s", err, tail.String())
	}
	var meeting runMeeting
	if err := readJSON(filepath.Join(out, "meeting.json"), &meeting); err != nil {
		return Wiki{}, err
	}
	name, sha, ok := primaryCommit(stdout.String(), meeting, in.Wiki)
	if !ok {
		return Wiki{}, fmt.Errorf("ingest: exited 0 without a commit for its primary wiki %q (%s/ingest.log)", name, out)
	}
	w := Wiki{Page: meeting.Page, Commit: sha}
	b, _ = json.Marshal(w)
	os.WriteFile(filepath.Join(out, "wiki.json"), b, 0o600)

	// The backup: the record as it will read once ingested, beside the audio.
	backup := map[string]any{}
	for k, v := range rec {
		backup[k] = v
	}
	backup["state"], backup["wiki"] = "ingested", w
	delete(backup, "claim")
	delete(backup, "stage")
	if in.Assets == "" {
		// no assets checkout: nothing to back up into
	} else if err := in.Backup(ctx, meeting.Bundle+"/audio/rec.json", backup); err != nil {
		line := fmt.Sprintf("megameet: rec.%s is in the wiki (%s) but its rec.json backup did not land in %s: %s", id, w.Commit, filepath.Base(in.Assets), oneLine(err.Error()))
		in.logf("%s", line)
		if in.Notify != nil {
			in.Notify(line)
		}
	}
	if in.Notify != nil {
		in.Notify(ingestedLine(id, rec, w, out))
	}
	return w, nil
}

// Resolve runs `<command> people resolve <record.json> --out <dir> --commit`
// over the named speakers of record id, role → name, and returns the
// people.json it writes there: one Person per speaker. The record it writes
// carries those speakers only: the others are placeholders or have their
// person.
func (in *Ingester) Resolve(ctx context.Context, id string, rec map[string]any, names map[string]string) ([]Person, error) {
	now := time.Now
	if in.Now != nil {
		now = in.Now
	}
	out := filepath.Join(in.People, id, now().UTC().Format("20060102-150405"))
	if err := os.MkdirAll(out, 0o700); err != nil {
		return nil, err
	}
	str := func(k string) string { s, _ := rec[k].(string); return s }
	b, _ := json.MarshalIndent(map[string]any{"id": id, "source": str("source"), "title": str("title"), "started": str("started"), "speakers": names}, "", " ")
	src := filepath.Join(out, "record.json")
	if err := os.WriteFile(src, b, 0o600); err != nil {
		return nil, err
	}
	ctx, cancel := context.WithTimeout(ctx, 15*time.Minute)
	defer cancel()
	args := append(append([]string(nil), in.Command[1:]...), "people", "resolve", src, "--out", out, "--commit")
	cmd := exec.CommandContext(ctx, in.Command[0], args...)
	cmd.Env = os.Environ()
	if in.Repos != "" {
		cmd.Env = append(cmd.Env, "REPOS_ROOT="+in.Repos)
	}
	logf, err := os.OpenFile(filepath.Join(out, "people.log"), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return nil, err
	}
	defer logf.Close()
	var tail tailBuf
	cmd.Stdout = io.MultiWriter(logf, &tail)
	cmd.Stderr = io.MultiWriter(logf, &tail)
	in.logf("people %s: %s", id, strings.Join(cmd.Args, " "))
	if err := cmd.Run(); err != nil {
		return nil, fmt.Errorf("people resolve: %v: %s", err, tail.String())
	}
	var rows []Person
	if err := readJSON(filepath.Join(out, "people.json"), &rows); err != nil {
		return nil, err
	}
	return rows, nil
}

// Status is the progress of id's current ingest run, read from the run's
// ingest.json, which the ingest writes and keeps current, as the registry record's
// `ingest` key: {run, state, step, usd, page_slug, wiki_page, error,
// updated_at}. state is queued, running, done, partial or failed; step the
// pass a running ingest is in. ok is false before the run writes the file.
func (in *Ingester) Status(id string) (map[string]any, bool) {
	in.mu.Lock()
	out := in.out[id]
	in.mu.Unlock()
	if out == "" {
		return nil, false
	}
	var r map[string]any
	if readJSON(filepath.Join(out, "ingest.json"), &r) != nil || r == nil {
		return nil, false
	}
	st, _ := r["state"].(string)
	s := map[string]any{"state": st}
	switch st {
	case "queued", "done", "partial", "failed":
	default:
		s["state"], s["step"] = "running", st
	}
	if step, ok := r["step"].(string); ok && step != "" {
		s["step"] = step
	}
	for _, k := range []string{"run", "usd", "page_slug", "wiki_page", "updated_at"} {
		if v, ok := r[k]; ok && v != nil {
			s[k] = v
		}
	}
	if e, ok := r["error"].(string); ok && e != "" {
		s["error"] = oneLine(e)
	}
	return s, true
}

// ingestedLine is the one message per ingested meeting: where it was filed
// and the run's open questions (notes.md, at most five).
func ingestedLine(id string, rec map[string]any, w Wiki, out string) string {
	title, _ := rec["title"].(string)
	if title == "" {
		title = id
	}
	commit := w.Commit
	if len(commit) > 9 {
		commit = commit[:9]
	}
	notes, _ := os.ReadFile(filepath.Join(out, "notes.md"))
	q := strings.TrimSpace(string(notes))
	if q == "" || strings.EqualFold(strings.Trim(q, ". "), "none") {
		q = "none"
	}
	line := fmt.Sprintf("megameet: «%s» is in the wiki: %s (%s, rec.%s)\nQuestions:\n%s", title, w.Page, commit, id, q)
	if r := []rune(line); len(r) > 3500 { // under Telegram's 4096
		line = string(r[:3500]) + "…"
	}
	return line
}

// runDir is the id's unfinished run folder, or a new one.
func (in *Ingester) runDir(id string) (string, error) {
	base := filepath.Join(in.Runs, id)
	runs, _ := filepath.Glob(filepath.Join(base, "*"))
	sort.Strings(runs)
	if n := len(runs); n > 0 && !exists(filepath.Join(runs[n-1], "wiki.json")) {
		return runs[n-1], nil
	}
	now := time.Now
	if in.Now != nil {
		now = in.Now
	}
	dir := filepath.Join(base, now().UTC().Format("20060102-150405"))
	return dir, os.MkdirAll(dir, 0o700)
}

// Backup writes value as JSON at rel in the assets checkout through a
// throwaway worktree at the remote's main, annexes it, copies it to the
// annex remote when one is set, and pushes main and git-annex.
func (in *Ingester) Backup(ctx context.Context, rel string, value any) error {
	repo, remote := in.Assets, in.AssetsRemote
	if _, err := git(ctx, repo, "fetch", "-q", remote); err != nil {
		return err
	}
	wt, err := os.MkdirTemp(in.Runs, ".assets-")
	if err != nil {
		return err
	}
	os.Remove(wt)
	if _, err := git(ctx, repo, "worktree", "add", "-q", "--detach", wt, remote+"/main"); err != nil {
		return err
	}
	defer func() {
		git(context.Background(), repo, "worktree", "remove", "--force", wt)
		os.RemoveAll(wt)
		git(context.Background(), repo, "worktree", "prune")
	}()
	path := filepath.Join(wt, rel)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	b, _ := json.MarshalIndent(value, "", " ")
	if err := os.WriteFile(path, append(b, '\n'), 0o644); err != nil {
		return err
	}
	steps := [][]string{{"annex", "add", "--", rel}}
	if in.AssetsAnnexRemote != "" {
		steps = append(steps, []string{"annex", "copy", "--to", in.AssetsAnnexRemote, "--", rel})
	}
	steps = append(steps, []string{"commit", "-q", "-m", "megameet: " + rel + " — the registry record beside its audio (backup)"})
	for _, args := range steps {
		if _, err := git(ctx, wt, args...); err != nil {
			return err
		}
	}
	for try := 0; ; try++ {
		_, err := git(ctx, wt, "push", "-q", remote, "HEAD:main", "git-annex")
		if err == nil || try == 2 {
			return err
		}
		if _, err := git(ctx, wt, "fetch", "-q", remote); err != nil {
			return err
		}
		if _, err := git(ctx, wt, "merge", "-q", "--no-edit", remote+"/main"); err != nil {
			return err
		}
	}
}

func git(ctx context.Context, dir string, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, "git", args...)
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	if err != nil {
		return string(out), fmt.Errorf("git %s: %v: %s", strings.Join(args, " "), err, strings.TrimSpace(string(out)))
	}
	return string(out), nil
}

func (in *Ingester) logf(format string, args ...any) {
	if in.Logf != nil {
		in.Logf(format, args...)
	}
}

func exists(p string) bool { _, err := os.Stat(p); return err == nil }

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

// tailBuf keeps the last 2 KiB written, for an error message.
// stdout and stderr write it from two goroutines.
type tailBuf struct {
	mu sync.Mutex
	b  []byte
}

func (t *tailBuf) Write(p []byte) (int, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.b = append(t.b, p...)
	if len(t.b) > 2048 {
		t.b = t.b[len(t.b)-2048:]
	}
	return len(p), nil
}

func (t *tailBuf) String() string {
	t.mu.Lock()
	defer t.mu.Unlock()
	return strings.TrimSpace(string(t.b))
}
