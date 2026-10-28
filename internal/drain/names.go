package drain

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/caoer/mega-asr/internal/names"
)

// NamesFile is the names pass's record in a pulled directory and on the
// registry (file role "names"): what it changed in our text, the Feishu text
// it corrects (Feishu's file stays as Feishu wrote it), and the edits its
// guards refused.
const NamesFile = "names.json"

// NamesSchema is names.json's schema.
const NamesSchema = "names@1"

// NamesReport is names.json.
type NamesReport struct {
	Schema  string          `json:"schema"`
	At      string          `json:"at"`
	Command string          `json:"command"`
	Effort  string          `json:"effort,omitempty"`
	Index   string          `json:"index_updated,omitempty"` // the people index's updated_at
	People  []string        `json:"people"`                  // the speakers' person slugs the pass knew
	Focus   []string        `json:"focus,omitempty"`         // the people a focused run was for
	Windows int             `json:"windows"`
	Calls   int             `json:"calls"`
	Failed  int             `json:"failed"`
	Refused string          `json:"refused,omitempty"`
	WallS   float64         `json:"wall_s"`
	Changes []names.Change  `json:"changes"`
	Dropped []names.Dropped `json:"dropped"`
}

// Namer is the drain's names pass: after Process, and again for the people
// a speaker was just resolved to, it asks the model (headless, one call per
// window) to repair person names, keeps the guarded edits, rewrites our
// segments' text (raw stays) and records the Feishu corrections in
// names.json.
type Namer struct {
	Command     []string // the headless model CLI; the call's flags are appended
	Effort      string   // passed as --effort; "" passes none
	Concurrency int
	WindowChars int
	Timeout     time.Duration // one call's
	// Index reads the people.index record's value.
	Index func(ctx context.Context) ([]byte, error)
	// Publish puts what the pass left in dir on the record: names.json, and
	// segments.json and transcript.md when ours changed.
	Publish func(ctx context.Context, id, dir string, ours bool) error
	Logf    func(format string, args ...any)
	Now     func() time.Time

	call names.Caller // nil: the CLI
}

// NamesPending says whether rec owes a names pass and for whom: every
// window when it never had one (or the last was refused), else only the
// people its speakers were resolved to since.
func NamesPending(rec map[string]any) (pending bool, focus []string) {
	persons := speakerPersons(rec)
	n, _ := rec["names"].(map[string]any)
	if n == nil {
		return true, nil
	}
	if r, _ := n["refused"].(string); r != "" {
		return true, nil
	}
	had := map[string]bool{}
	if ps, _ := n["people"].([]any); ps != nil {
		for _, p := range ps {
			s, _ := p.(string)
			had[s] = true
		}
	}
	for _, p := range persons {
		if !had[p] {
			focus = append(focus, p)
		}
	}
	return len(focus) > 0, focus
}

func speakerPersons(rec map[string]any) []string {
	sp, _ := rec["speakers"].([]any)
	var out []string
	for _, x := range sp {
		m, _ := x.(map[string]any)
		if p, _ := m["person"].(string); p != "" && !slices.Contains(out, p) {
			out = append(out, p)
		}
	}
	sort.Strings(out)
	return out
}

// Stage runs the pass on the record pulled into dir, publishes its files,
// and returns the record's `names` summary. focus nil runs every window.
func (n *Namer) Stage(ctx context.Context, id, dir string, rec map[string]any, focus []string) (Patch, error) {
	rep, ours, err := n.Run(ctx, dir, rec, focus)
	if err != nil {
		return nil, err
	}
	if err := n.Publish(ctx, id, dir, ours); err != nil {
		return nil, fmt.Errorf("publish: %w", err)
	}
	n.logf("names rec.%s: %d windows, %d calls, %d failed, %d texts changed, %d edits refused by the guards, %.0f s%s",
		id, rep.Windows, rep.Calls, rep.Failed, len(rep.Changes), len(rep.Dropped), rep.WallS, map[bool]string{true: "; refused: " + rep.Refused}[rep.Refused != ""])
	sum := map[string]any{"at": rep.At, "people": rep.People, "changes": len(rep.Changes), "calls": rep.Calls, "failed": rep.Failed}
	if rep.Refused != "" {
		sum["refused"] = rep.Refused
	}
	return func(raw map[string]any) error { raw["names"] = sum; return nil }, nil
}

// Run is the pass over dir: it reads segments.json and feishu-transcript.json,
// writes segments.json when our text changed (ours reports it) and
// names.json. A focused run starts from the Feishu text the last names.json
// corrected and keeps its changes; a full run starts over.
func (n *Namer) Run(ctx context.Context, dir string, rec map[string]any, focus []string) (NamesReport, bool, error) {
	now := time.Now
	if n.Now != nil {
		now = n.Now
	}
	idx, err := n.Index(ctx)
	if err != nil {
		return NamesReport{}, false, fmt.Errorf("people index: %w", err)
	}
	roster, err := names.FromIndex(idx)
	if err != nil {
		return NamesReport{}, false, err
	}
	rep := NamesReport{Schema: NamesSchema, At: now().UTC().Format(time.RFC3339), Command: strings.Join(n.Command, " "), Effort: n.Effort, Index: roster.Updated, Focus: focus}
	speakers, known := recordSpeakers(rec, roster)
	rep.People = known

	var prev NamesReport
	if focus != nil {
		_ = readJSON(filepath.Join(dir, NamesFile), &prev)
	}
	segPath := filepath.Join(dir, "segments.json")
	var segs []map[string]any
	if err := readJSON(segPath, &segs); err != nil && !errors.Is(err, os.ErrNotExist) {
		return rep, false, err
	}
	var feishu []Turn
	if ft := filepath.Join(dir, "feishu-transcript.json"); exists(ft) {
		var t struct {
			Segments []Turn `json:"segments"`
		}
		if err := readJSON(ft, &t); err != nil {
			return rep, false, err
		}
		feishu = t.Segments
	}
	fo := make([]string, len(feishu))
	for i, t := range feishu {
		fo[i] = t.Text
	}
	fs := ApplyNames(fo, prev.Changes)
	ot := make([]string, len(segs))
	for i, s := range segs {
		ot[i], _ = s["text"].(string)
	}

	var fi []int
	for _, slug := range focus {
		if i, ok := roster.Person(slug); ok && !slices.Contains(fi, i) {
			fi = append(fi, i)
		}
	}
	if focus != nil && len(fi) == 0 {
		// None of them is in the index yet: nothing to focus on.
		rep.Changes, rep.Dropped = prev.Changes, prev.Dropped
		return rep, false, writeNames(dir, rep)
	}
	call := n.call
	if call == nil {
		call = CLI(n.Command, n.Effort, n.Timeout)
	}
	p := names.Pass{Index: roster, Speakers: speakers, Call: call, Concurrency: n.Concurrency, WindowChars: n.WindowChars, Focus: fi}
	res := p.Run(ctx, []names.Transcript{{Name: "ours", Texts: ot}, {Name: "feishu", Texts: fs}})
	if ctx.Err() != nil {
		return rep, false, ctx.Err()
	}
	rep.Windows, rep.Calls, rep.Failed, rep.Refused, rep.WallS = res.Windows, res.Calls, res.Failed, res.Refused, res.WallS
	rep.Changes = mergeChanges(prev.Changes, res.Changes)
	rep.Dropped = append(prev.Dropped, res.Dropped...)
	if rep.Changes == nil {
		rep.Changes = []names.Change{}
	}
	if rep.Dropped == nil {
		rep.Dropped = []names.Dropped{}
	}
	ours := false
	for i, t := range res.Texts["ours"] {
		if t != ot[i] {
			segs[i]["text"], ours = t, true
		}
	}
	if ours {
		b, err := json.MarshalIndent(segs, "", " ")
		if err != nil {
			return rep, false, err
		}
		if err := writeFile(segPath, append(b, '\n')); err != nil {
			return rep, false, err
		}
	}
	return rep, ours, writeNames(dir, rep)
}

// recordSpeakers is the record's speakers by name — a resolved one by its
// person's name in the index — and the person slugs among them the index
// holds.
func recordSpeakers(rec map[string]any, r *names.Index) (speakers, known []string) {
	sp, _ := rec["speakers"].([]any)
	for _, x := range sp {
		m, _ := x.(map[string]any)
		name, _ := m["name"].(string)
		role, _ := m["role"].(string)
		person, _ := m["person"].(string)
		name = strings.TrimSpace(name)
		if i, ok := r.Person(person); person != "" && ok {
			name = r.People[i].Name
			if !slices.Contains(known, person) {
				known = append(known, person)
			}
		}
		if name == "" || name == role && !r.Listed(name) || placeholder.MatchString(name) || slices.Contains(speakers, name) {
			continue
		}
		speakers = append(speakers, name)
	}
	sort.Strings(known)
	if known == nil {
		known = []string{}
	}
	return speakers, known
}

// mergeChanges is prev with next laid over it: a text changed again keeps
// its first before and gains the new names.
func mergeChanges(prev, next []names.Change) []names.Change {
	out := append([]names.Change(nil), prev...)
	for _, c := range next {
		k := slices.IndexFunc(out, func(o names.Change) bool { return o.Transcript == c.Transcript && o.Index == c.Index })
		if k < 0 {
			out = append(out, c)
			continue
		}
		out[k].After = c.After
		out[k].Names = append(out[k].Names, c.Names...)
	}
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].Transcript != out[j].Transcript {
			return out[i].Transcript > out[j].Transcript
		}
		return out[i].Index < out[j].Index
	})
	return out
}

// ApplyNames lays names.json's Feishu changes over Feishu's texts: a change
// applies where the text still reads as it did before the pass.
func ApplyNames(texts []string, changes []names.Change) []string {
	out := append([]string(nil), texts...)
	for _, c := range changes {
		if c.Transcript == "feishu" && c.Index >= 0 && c.Index < len(out) && out[c.Index] == c.Before {
			out[c.Index] = c.After
		}
	}
	return out
}

// feishuNames reads dir's names.json changes; none when there is none.
func feishuNames(dir string) []names.Change {
	var r NamesReport
	if err := readJSON(filepath.Join(dir, NamesFile), &r); err != nil {
		return nil
	}
	return r.Changes
}

func writeNames(dir string, rep NamesReport) error {
	b, err := json.MarshalIndent(rep, "", " ")
	if err != nil {
		return err
	}
	return writeFile(filepath.Join(dir, NamesFile), append(b, '\n'))
}

func writeFile(path string, b []byte) error {
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// limitRE is a reply or error that reads as the account refusing.
var limitRE = regexp.MustCompile(`(?i)rate.?limit|usage.?limit|quota|too many requests|limit reached|\b429\b`)

// CLI calls the model through a headless Claude Code launcher, one process
// a call: the user message on stdin, the prompt as the system prompt, no
// tools, no hooks, no session kept; timeout bounds a call (0 is none).
func CLI(command []string, effort string, timeout time.Duration) names.Caller {
	return func(ctx context.Context, system, user string) (string, error) {
		return cliCall(ctx, command, effort, timeout, system, user)
	}
}

func cliCall(ctx context.Context, command []string, effort string, timeout time.Duration, system, user string) (string, error) {
	if timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, timeout)
		defer cancel()
	}
	args := append(append([]string(nil), command[1:]...), "-p", "--bare", "--tools", "", "--strict-mcp-config", "--disable-slash-commands",
		"--no-session-persistence", "--output-format", "json", "--system-prompt", system)
	if effort != "" {
		args = append(args, "--effort", effort)
	}
	cmd := exec.CommandContext(ctx, command[0], args...)
	cmd.Dir = os.TempDir()
	cmd.Stdin = strings.NewReader(user)
	var out, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &stderr
	runErr := cmd.Run()
	var res struct {
		IsError   bool   `json:"is_error"`
		Result    string `json:"result"`
		APIStatus int    `json:"api_error_status"`
	}
	found := false
	lines := strings.Split(strings.TrimSpace(out.String()), "\n")
	for i := len(lines) - 1; i >= 0 && !found; i-- {
		if l := strings.TrimSpace(lines[i]); strings.HasPrefix(l, "{") {
			found = json.Unmarshal([]byte(l), &res) == nil
		}
	}
	if !found {
		tail := lastLine(stderr.String())
		if limitRE.MatchString(tail) || limitRE.MatchString(out.String()) {
			return "", fmt.Errorf("%w: %s", names.ErrRefused, tail)
		}
		return "", fmt.Errorf("%s: %v: no JSON result: %s", command[0], runErr, tail)
	}
	if res.IsError || res.APIStatus != 0 {
		msg := oneLine(res.Result)
		if res.APIStatus == 429 || limitRE.MatchString(msg) {
			return "", fmt.Errorf("%w: %s", names.ErrRefused, msg)
		}
		return "", fmt.Errorf("%s: %d %s", command[0], res.APIStatus, msg)
	}
	return res.Result, nil
}

func (n *Namer) logf(format string, args ...any) {
	if n.Logf != nil {
		n.Logf(format, args...)
	}
}
