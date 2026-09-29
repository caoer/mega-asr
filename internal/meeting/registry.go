package meeting

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/caoer/mega-asr/internal/pages"
)

// Registry is the meetings page's records and files, as pages.Client
// serves them.
type Registry interface {
	List(ctx context.Context, prefix string) ([]pages.Object, error)
	Record(ctx context.Context, key string) (pages.Object, error)
	CAS(ctx context.Context, key, schema string, data any, version int) (int, error)
	Upload(ctx context.Context, path string, meta map[string]any) (id, sum string, err error)
	DeleteFile(ctx context.Context, fileID string) error
}

// Fetch puts a record's files of the given roles in a local folder and
// returns role → path; a role the record lacks is absent from the map.
type Fetch func(ctx context.Context, r Record, roles ...string) (map[string]string, error)

// Aligner runs align over the registry.
type Aligner struct {
	Reg   Registry
	Fetch Fetch
	Tools Tools
	Data  string // [meeting] data: align/<rec>/align.json, scores.jsonl, train/
	Logf  func(format string, args ...any)
}

// Patch is what an alignment writes onto a local recording's record:
// nothing else of it changes, its state least of all.
type Patch struct {
	Scores   *Scores // loops and rtf stay as processing wrote them
	File     *File   // align.json, in place of the earlier one
	Unpaired bool    // no Feishu minute scored it
}

// Apply writes the patch onto a record's raw value and returns the id of
// the align file it replaced, if any.
func (p Patch) Apply(raw map[string]any, now time.Time) (replaced string, err error) {
	if p.Unpaired {
		raw["align"] = Unpaired
	}
	if p.Scores != nil {
		b, err := json.Marshal(p.Scores)
		if err != nil {
			return "", err
		}
		s, _ := raw["scores"].(map[string]any)
		if s == nil {
			s = map[string]any{}
		}
		if err := json.Unmarshal(b, &s); err != nil {
			return "", err
		}
		raw["scores"] = s
		delete(raw, "align")
	}
	if p.File != nil {
		files, _ := raw["files"].([]any)
		f := map[string]any{}
		b, _ := json.Marshal(p.File)
		json.Unmarshal(b, &f)
		done := false
		for i, x := range files {
			if m, ok := x.(map[string]any); ok && m["role"] == p.File.Role {
				replaced, _ = m["file"].(string)
				files[i], done = f, true
				break
			}
		}
		if !done {
			files = append(files, f)
		}
		raw["files"] = files
	}
	raw["updated"] = now.UTC().Format(time.RFC3339)
	return replaced, nil
}

// Claimed runs the alignments the claimed record owes (Jobs). A patch for
// another record is written onto it here by compare-and-swap; the claimed
// record's own patch is returned for the claim holder to fold into its next
// write (nil when it owes none: a Feishu minute, another source).
func (a *Aligner) Claimed(ctx context.Context, id string, redo bool) (*Patch, error) {
	objs, err := a.Reg.List(ctx, "rec.")
	if err != nil {
		return nil, err
	}
	var all []Record
	var claimed Record
	for _, o := range objs {
		var r Record
		if err := json.Unmarshal(o.Value.Data, &r); err != nil {
			a.logf("align: %s: %v", o.Key, err)
			continue
		}
		if r.State == Deleted {
			continue
		}
		all = append(all, r)
		if r.ID == id {
			claimed = r
		}
	}
	if claimed.ID == "" {
		return nil, fmt.Errorf("align: no record rec.%s", id)
	}
	jobs := Jobs(claimed, all, redo)
	var self *Patch
	if IsLocal(claimed) && len(jobs) == 0 {
		a.logf("align: %s: no Feishu minute overlaps it yet: unpaired", id)
		self = &Patch{Unpaired: true}
	}
	var errs []error
	for _, j := range jobs {
		p, err := a.run(ctx, j)
		if err != nil {
			errs = append(errs, fmt.Errorf("align %s against %s: %w", j.Local.ID, j.Feishu.ID, err))
			continue
		}
		if j.Local.ID == id {
			self = &p
			continue
		}
		if err := a.Write(ctx, j.Local.ID, p); err != nil {
			errs = append(errs, err)
		}
	}
	return self, errors.Join(errs...)
}

// Local tracks, mixed for the offset search.
var trackRoles = []string{"remote", "mic", "beam", "media"}

func (a *Aligner) run(ctx context.Context, j Job) (Patch, error) {
	lf, err := a.Fetch(ctx, j.Local, append([]string{"segments"}, trackRoles...)...)
	if err != nil {
		return Patch{}, err
	}
	ff, err := a.Fetch(ctx, j.Feishu, "media", "feishu-transcript")
	if err != nil {
		return Patch{}, err
	}
	if lf["segments"] == "" {
		return Patch{}, fmt.Errorf("%s has no segments file", j.Local.ID)
	}
	segs, err := ReadSegments(lf["segments"])
	if err != nil {
		return Patch{}, err
	}
	turns, err := ReadTurns(ff["feishu-transcript"])
	if err != nil {
		return Patch{}, err
	}
	in := Input{
		Rec: j.Local.ID, Against: j.Feishu.ID, Date: j.Local.Started.Local().Format("2006-01-02"),
		Segments: segs, Turns: turns, Media: ff["media"],
		ExpectS: j.Feishu.Started.Sub(j.Local.Started).Seconds(),
	}
	for _, r := range trackRoles {
		if lf[r] != "" {
			in.Audio = append(in.Audio, lf[r])
		}
	}
	res, err := Align(ctx, a.Tools, in)
	if err != nil {
		return Patch{}, err
	}
	if len(j.Others) > 0 {
		res.Flags = append(res.Flags, "also overlaps "+strings.Join(j.Others, ", "))
	}
	if !res.Paired {
		a.logf("align: %s against %s: %s", j.Local.ID, j.Feishu.ID, strings.Join(res.Flags, "; "))
		return Patch{Unpaired: true}, nil
	}
	path, err := a.Keep(ctx, res, lf["mic"])
	if err != nil {
		return Patch{}, err
	}
	fid, sum, err := a.Reg.Upload(ctx, path, map[string]any{"rec": res.Rec, "role": "align"})
	if err != nil {
		return Patch{}, err
	}
	st, err := os.Stat(path)
	if err != nil {
		return Patch{}, err
	}
	s := res.Scores()
	a.logf("align: %s against %s: CER %.1f%%, cpCER %.1f%%, offset %.2f s (score %.1f), drift %.0f ppm",
		res.Rec, res.Against, 100*s.CER, 100*s.CPCER, s.OffsetS, s.OffsetScore, s.DriftPPM)
	return Patch{Scores: &s, File: &File{Role: "align", File: fid, Bytes: st.Size(), SHA256: sum, Verified: true, Codec: "json"}}, nil
}

// Keep writes a paired result's local traces: align.json under
// align/<rec>/, one line in scores.jsonl, and the training rows when the
// owner's mic track is at hand. It returns align.json's path.
func (a *Aligner) Keep(ctx context.Context, res Result, mic string) (string, error) {
	dir := filepath.Join(a.Data, "align", res.Rec)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", err
	}
	b, err := json.MarshalIndent(res, "", " ")
	if err != nil {
		return "", err
	}
	path := filepath.Join(dir, "align.json")
	if err := os.WriteFile(path, append(b, '\n'), 0o600); err != nil {
		return "", err
	}
	if mic != "" && len(res.Train) > 0 {
		if err := WriteTrain(ctx, filepath.Join(a.Data, "train"), res.Rec, mic, res.Train); err != nil {
			return "", fmt.Errorf("training rows: %w", err)
		}
	}
	return path, AppendLine(filepath.Join(a.Data, "scores.jsonl"), res.Line())
}

// Write applies a patch to rec.<id> by compare-and-swap, re-reading on a
// conflict, and then deletes the align file it replaced.
func (a *Aligner) Write(ctx context.Context, id string, p Patch) error {
	key := "rec." + id
	for try := 0; ; try++ {
		o, err := a.Reg.Record(ctx, key)
		if err != nil {
			return err
		}
		var raw map[string]any
		if err := json.Unmarshal(o.Value.Data, &raw); err != nil {
			return fmt.Errorf("%s: %w", key, err)
		}
		old, err := p.Apply(raw, time.Now())
		if err != nil {
			return err
		}
		_, err = a.Reg.CAS(ctx, key, Schema, raw, o.Version)
		if pages.Code(err) == "version_conflict" && try < 3 {
			continue
		}
		if err != nil {
			return fmt.Errorf("%s: %w", key, err)
		}
		if old != "" && (p.File == nil || old != p.File.File) {
			if err := a.Reg.DeleteFile(ctx, old); err != nil {
				a.logf("align: %s: the replaced align file %s stays: %v", id, old, err)
			}
		}
		return nil
	}
}

func (a *Aligner) logf(format string, args ...any) {
	if a.Logf != nil {
		a.Logf(format, args...)
	}
}
