package meeting

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"os"
	"path/filepath"
	"slices"
	"time"

	"github.com/BurntSushi/toml"

	"github.com/caoer/mega-asr/internal/pages"
)

// Pages is the meetings page as pull and publish use it; *pages.Client is
// the real one.
type Pages interface {
	Record(ctx context.Context, key string) (pages.Object, error)
	CAS(ctx context.Context, key, schema string, data any, version int) (int, error)
	Get(ctx context.Context, fileID string, off, n int64) (io.ReadCloser, error)
	Upload(ctx context.Context, path string, meta map[string]any) (id, sum string, err error)
	DeleteFile(ctx context.Context, fileID string) error
}

// PullDir is where a record is pulled: <meeting.data>/pull/<id>.
func PullDir(data, id string) string { return filepath.Join(data, "pull", id) }

// The pulled directory is v1's meeting directory contract:
//
//	meta.toml                what was recorded and the tracks with their digests
//	rec.json                 the registry record as pulled
//	tracks/<role>.<codec>    remote, mic, beam, raw, media
//	doa.jsonl                room only
//	feishu-transcript.json   feishu only
//	segments.json            written by Process
//	transcript.md            written by Process
func pulledPath(role, codec string) (string, bool) {
	switch role {
	case "remote", "mic", "beam", "raw", "media":
		name := role
		if codec != "" {
			name += "." + codec
		}
		return filepath.Join("tracks", name), true
	case "doa":
		return "doa.jsonl", true
	case "feishu-transcript":
		return "feishu-transcript.json", true
	}
	return "", false // an output: segments, transcript, align
}

// PullError is a pull that cannot succeed until the record's files change;
// Code is what the record's error says.
type PullError struct {
	Code   string // missing_file | sha_mismatch
	Role   string
	Detail string
}

func (e *PullError) Error() string { return fmt.Sprintf("%s: %s: %s", e.Code, e.Role, e.Detail) }

// Pull materializes rec.<id> in dir: every input file of the record,
// checked against its sha256 (a file already there with the right digest is
// not fetched again), then files[].verified set by CAS, then rec.json and
// meta.toml.
func Pull(ctx context.Context, pg Pages, id, dir string) (Record, error) {
	key := "rec." + id
	o, err := pg.Record(ctx, key)
	if err != nil {
		return Record{}, err
	}
	var rec Record
	if err := json.Unmarshal(o.Value.Data, &rec); err != nil {
		return rec, fmt.Errorf("%s: %w", key, err)
	}
	if rec.State == Deleted {
		return rec, fmt.Errorf("%s is deleted", key)
	}
	var pulled []string
	for _, f := range rec.Files {
		rel, ok := pulledPath(f.Role, f.Codec)
		if !ok {
			continue
		}
		if f.File == "" {
			return rec, &PullError{"missing_file", f.Role, "the record names no file"}
		}
		if err := fetch(ctx, pg, f, filepath.Join(dir, rel)); err != nil {
			return rec, err
		}
		pulled = append(pulled, f.Role)
	}
	if rec, err = markVerified(ctx, pg, key, pulled); err != nil {
		return rec, err
	}
	if err := writeJSON(filepath.Join(dir, "rec.json"), rec); err != nil {
		return rec, err
	}
	return rec, writeMeta(dir, rec)
}

// fetch puts f at path unless a file with its digest is there already.
func fetch(ctx context.Context, pg Pages, f File, path string) error {
	if sum, _, err := digest(path); err == nil && sum == f.SHA256 {
		return nil
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	body, err := pg.Get(ctx, f.File, 0, 0)
	var pe *pages.Error
	if errors.As(err, &pe) && pe.Status == 404 {
		return &PullError{"missing_file", f.Role, err.Error()}
	}
	if err != nil {
		return err
	}
	defer body.Close()
	tmp, err := os.CreateTemp(filepath.Dir(path), ".pull-*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	h := sha256.New()
	n, err := io.Copy(io.MultiWriter(tmp, h), body)
	if cerr := tmp.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		return fmt.Errorf("pull %s: %w", f.Role, err)
	}
	if sum := hex.EncodeToString(h.Sum(nil)); sum != f.SHA256 {
		return &PullError{"sha_mismatch", f.Role, fmt.Sprintf("%d bytes with sha256 %s, the record says %s", n, sum, f.SHA256)}
	}
	return os.Rename(tmp.Name(), path)
}

// markVerified sets verified on the files of the given roles and returns
// the record as written.
func markVerified(ctx context.Context, pg Pages, key string, roles []string) (Record, error) {
	var rec Record
	err := update(ctx, pg, key, func(raw map[string]any) (bool, error) {
		files := rawFiles(raw)
		changed := false
		for i := range files {
			if slices.Contains(roles, files[i].Role) && !files[i].Verified {
				files[i].Verified, changed = true, true
			}
		}
		raw["files"] = files
		b, _ := json.Marshal(raw)
		rec = Record{}
		return changed, json.Unmarshal(b, &rec)
	})
	return rec, err
}

// update applies change to the record's current value and writes it by
// CAS, re-reading on another writer's change; change reports whether there
// is anything to write. Fields it does not touch stay as they are.
func update(ctx context.Context, pg Pages, key string, change func(raw map[string]any) (bool, error)) error {
	for try := 0; ; try++ {
		o, err := pg.Record(ctx, key)
		if err != nil {
			return err
		}
		var raw map[string]any
		if err := json.Unmarshal(o.Value.Data, &raw); err != nil {
			return fmt.Errorf("%s: %w", key, err)
		}
		write, err := change(raw)
		if err != nil || !write {
			return err
		}
		_, err = pg.CAS(ctx, key, Schema, raw, o.Version)
		if pages.Code(err) == "version_conflict" && try < 5 {
			continue
		}
		return err
	}
}

func rawFiles(raw map[string]any) []File {
	b, _ := json.Marshal(raw["files"])
	var fs []File
	_ = json.Unmarshal(b, &fs)
	return fs
}

// Publish uploads Process's segments.json and transcript.md from dir with
// meta {rec, role} and writes them onto rec.<id> by CAS: the files[] entry
// of each role (the file it replaces is deleted), engine, and scores.loops
// and scores.rtf. It touches nothing else of the record.
// An empty result (no speech: transcript.md has no bytes) is not uploaded —
// the page refuses a zero-byte file — and an earlier file of its role goes.
func Publish(ctx context.Context, pg Pages, id, dir string, o Outcome, eng Engine) error {
	var up []File
	var empty []string
	for _, f := range []struct{ role, name, codec string }{
		{"segments", "segments.json", "json"}, {"transcript", "transcript.md", "md"},
	} {
		path := filepath.Join(dir, f.name)
		sum, size, err := digest(path)
		if err != nil {
			return err
		}
		if size == 0 {
			empty = append(empty, f.role)
			continue
		}
		fid, got, err := pg.Upload(ctx, path, map[string]any{"rec": id, "role": f.role})
		if err != nil {
			return fmt.Errorf("upload %s: %w", f.name, err)
		}
		if got != sum {
			return fmt.Errorf("upload %s: the page has sha256 %s, the file %s", f.name, got, sum)
		}
		up = append(up, File{Role: f.role, File: fid, Bytes: size, SHA256: sum, Verified: true, Codec: f.codec})
	}
	var replaced []string
	err := update(ctx, pg, "rec."+id, func(raw map[string]any) (bool, error) {
		files := rawFiles(raw)
		replaced = replaced[:0]
		files = slices.DeleteFunc(files, func(f File) bool {
			gone := slices.Contains(empty, f.Role)
			if gone && f.File != "" {
				replaced = append(replaced, f.File)
			}
			return gone
		})
		for _, u := range up {
			k := slices.IndexFunc(files, func(f File) bool { return f.Role == u.Role })
			if k < 0 {
				files = append(files, u)
				continue
			}
			if files[k].File != "" && files[k].File != u.File {
				replaced = append(replaced, files[k].File)
			}
			files[k] = u
		}
		raw["files"] = files
		raw["engine"] = eng
		s, _ := raw["scores"].(map[string]any)
		if s == nil {
			s = map[string]any{}
		}
		s["loops"], s["rtf"] = o.Loops, round3(o.RTF())
		raw["scores"] = s
		raw["updated"] = time.Now().UTC()
		return true, nil
	})
	if err != nil {
		return err
	}
	for _, fid := range replaced {
		if err := pg.DeleteFile(ctx, fid); err != nil {
			return fmt.Errorf("delete the replaced file %s: %w", fid, err)
		}
	}
	return nil
}

func round3(x float64) float64 { return math.Round(x*1000) / 1000 }

func digest(path string) (string, int64, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", 0, err
	}
	defer f.Close()
	h := sha256.New()
	n, err := io.Copy(h, f)
	return hex.EncodeToString(h.Sum(nil)), n, err
}

func writeJSON(path string, v any) error {
	b, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return err
	}
	return writeWhole(path, append(b, '\n'))
}

// writeWhole replaces path: a crash leaves the old file or the new one.
func writeWhole(path string, b []byte) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// Meta is the pulled directory's meta.toml, the contract's summary of the
// record for the ingest.
type Meta struct {
	ID        string        `toml:"id"`
	Source    string        `toml:"source"`
	Host      string        `toml:"host,omitempty"`
	Owner     string        `toml:"owner,omitempty"`
	Title     string        `toml:"title,omitempty"`
	Started   time.Time     `toml:"started"`
	Stopped   time.Time     `toml:"stopped,omitempty"`
	DurationS float64       `toml:"duration_s"`
	Apps      []string      `toml:"apps,omitempty"`
	Speakers  []MetaSpeaker `toml:"speakers,omitempty"`
	Tracks    []MetaTrack   `toml:"tracks"`
	Feishu    *MetaFeishu   `toml:"feishu,omitempty"`
}

type MetaSpeaker struct {
	Name string `toml:"name,omitempty"`
	Role string `toml:"role,omitempty"`
}

// MetaTrack is one pulled file: tracks/ and the side files alike.
type MetaTrack struct {
	Name   string `toml:"name"`
	Role   string `toml:"role"`
	File   string `toml:"file"` // relative to the directory
	SHA256 string `toml:"sha256"`
	Bytes  int64  `toml:"bytes"`
}

type MetaFeishu struct {
	Token string `toml:"token"`
}

func writeMeta(dir string, rec Record) error {
	m := Meta{ID: rec.ID, Source: rec.Source, Host: rec.Host, Owner: rec.Owner, Title: rec.Title,
		Started: rec.Started, Stopped: rec.Stopped, DurationS: rec.DurationS, Apps: rec.Apps}
	for _, s := range rec.Speakers {
		m.Speakers = append(m.Speakers, MetaSpeaker{s.Name, s.Role})
	}
	for _, f := range rec.Files {
		if rel, ok := pulledPath(f.Role, f.Codec); ok {
			m.Tracks = append(m.Tracks, MetaTrack{Name: f.Role, Role: f.Role, File: rel, SHA256: f.SHA256, Bytes: f.Bytes})
		}
	}
	if rec.Feishu != nil {
		m.Feishu = &MetaFeishu{Token: rec.Feishu.Token}
	}
	var b bytes.Buffer
	if err := toml.NewEncoder(&b).Encode(m); err != nil {
		return err
	}
	return writeWhole(filepath.Join(dir, "meta.toml"), b.Bytes())
}

// ReadPulled reads the record a pull left in dir.
func ReadPulled(dir string) (Record, error) {
	var rec Record
	return rec, readJSON(filepath.Join(dir, "rec.json"), &rec)
}
