package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/caoer/mega-asr/internal/meeting"
	"github.com/caoer/mega-asr/internal/pages"
)

// registry is the page as the upload uses it: records by CAS, files by
// upload. *pages.Client is the real one.
type registry interface {
	Create(ctx context.Context, key, schema string, data any) (int, error)
	CAS(ctx context.Context, key, schema string, data any, version int) (int, error)
	Record(ctx context.Context, key string) (pages.Object, error)
	Upload(ctx context.Context, path string, meta map[string]any) (id, sum string, err error)
}

const (
	recSchema   = "meeting@1"
	queueSchema = "queue@1"
)

// job is one recording on its way to the page, as the ledger keeps it: the
// record's fields and the local files. It is written before the first
// request and removed once the record is uploaded, so whatever is left in
// the ledger is resumed.
type job struct {
	ID        string    `json:"id"`
	Source    string    `json:"source"` // mac | file | feishu
	Host      string    `json:"host"`
	Title     string    `json:"title"`
	Started   time.Time `json:"started"`
	Stopped   time.Time `json:"stopped"`
	DurationS float64   `json:"duration_s"`
	Speakers  int       `json:"speakers"`
	Test      bool      `json:"test,omitempty"` // a test: the drain never ingests it
	Apps      []string  `json:"apps,omitempty"`
	Dir       string    `json:"dir,omitempty"` // the recording's directory (mac)
	Files     []jobFile `json:"files"`

	SpeakerNames []string      `json:"speaker_names,omitempty"` // named speakers; Speakers counts unnamed ones
	Feishu       string        `json:"feishu,omitempty"`        // the minute's token (feishu)
	FeishuURL    string        `json:"feishu_url,omitempty"`    // the minute's page (feishu)
	Summary      *string       `json:"summary,omitempty"`       // Feishu's summary (feishu); "" is none, nil not fetched
	Wiki         *meeting.Wiki `json:"wiki,omitempty"`          // where the meeting is already filed
}

// feishu is the record's feishu object: the token, and the URL when known.
func (j job) feishu() map[string]string {
	fe := map[string]string{"token": j.Feishu}
	if j.FeishuURL != "" {
		fe["url"] = j.FeishuURL
	}
	return fe
}

type jobFile struct {
	Role  string `json:"role"`
	Path  string `json:"path"`
	Codec string `json:"codec"`
	From  string `json:"from,omitempty"` // a WAV Path is packed from when Path is missing
}

// File is one entry of the record's files[].
type File struct {
	Role     string `json:"role"`
	File     string `json:"file"` // the /f/ id; "" until uploaded
	Bytes    int64  `json:"bytes"`
	SHA256   string `json:"sha256"`
	Verified bool   `json:"verified"`
	Codec    string `json:"codec"`
}

// ledger is <meeting.data>/pending: one file per job.
type ledger string

func (l ledger) path(id string) string { return filepath.Join(string(l), id) }

func (l ledger) put(j job) error {
	if err := os.MkdirAll(string(l), 0o700); err != nil {
		return err
	}
	b, _ := json.MarshalIndent(j, "", "  ")
	tmp := l.path(j.ID) + ".tmp"
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, l.path(j.ID))
}

func (l ledger) ids() ([]string, error) {
	es, err := os.ReadDir(string(l))
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	var ids []string
	for _, e := range es {
		if !e.IsDir() && !strings.HasSuffix(e.Name(), ".tmp") {
			ids = append(ids, e.Name())
		}
	}
	return ids, err
}

// macJob is the job for a stopped recording: its tracks, packed to FLAC.
func macJob(dir string, m Meta) job {
	j := job{ID: m.ID, Source: m.Source, Host: m.Host, Title: m.Title, Started: m.Started, Stopped: m.Stopped,
		DurationS: m.DurationS, Speakers: m.Speakers, Test: m.Test, Apps: m.Apps, Dir: dir}
	for _, t := range m.Tracks {
		wav := filepath.Join(dir, t.File)
		j.Files = append(j.Files, jobFile{Role: t.Role, Path: strings.TrimSuffix(wav, ".wav") + ".flac", Codec: "flac", From: wav})
	}
	return j
}

// uploader moves jobs from the ledger to the page.
type uploader struct {
	reg    registry
	ledger ledger
	now    func() time.Time
	status func(id, s string) // progress for `status`; may be nil
}

func (u *uploader) report(id, format string, a ...any) {
	s := fmt.Sprintf(format, a...)
	log.Printf("upload %s: %s", id, s)
	if u.status != nil {
		u.status(id, s)
	}
}

// resume pushes every job left in the ledger, oldest first.
func (u *uploader) resume(ctx context.Context) error {
	ids, err := u.ledger.ids()
	if err != nil {
		return err
	}
	var errs []error
	for _, id := range ids {
		if err := u.run(ctx, id); err != nil {
			errs = append(errs, fmt.Errorf("%s: %w", id, err))
		}
	}
	return errors.Join(errs...)
}

// run pushes the ledger's job id under an exclusive lock on its entry, so
// serve and a `megameet upload` never push the same job at once; the entry
// goes once the record is uploaded.
func (u *uploader) run(ctx context.Context, id string) error {
	f, err := os.OpenFile(u.ledger.path(id), os.O_RDWR, 0)
	if errors.Is(err, os.ErrNotExist) {
		return nil // pushed by someone else meanwhile
	}
	if err != nil {
		return err
	}
	defer f.Close()
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		return fmt.Errorf("another upload holds it")
	}
	var j job
	if err := json.NewDecoder(f).Decode(&j); err != nil {
		return fmt.Errorf("ledger: %w", err)
	}
	if err := u.push(ctx, j); err != nil {
		u.report(id, "error: %v", err)
		return err
	}
	if j.Dir != "" {
		markUploaded(j.Dir, u.now())
	}
	u.report(id, "uploaded")
	return os.Remove(u.ledger.path(id))
}

func markUploaded(dir string, at time.Time) {
	m, err := readMeta(dir)
	if err == nil {
		m.UploadedAt = at
		err = writeMeta(dir, m)
	}
	if err != nil {
		log.Printf("upload: %s meta.toml: %v", dir, err)
	}
}

// push registers j's record as uploading, uploads each file the record does
// not name yet and writes its id by CAS, then CASes the record to uploaded
// and creates its queue key. A record already at or past uploaded uploads
// nothing.
func (u *uploader) push(ctx context.Context, j job) error {
	files := make([]File, len(j.Files))
	for i, f := range j.Files {
		if _, err := os.Stat(f.Path); errors.Is(err, os.ErrNotExist) && f.From != "" {
			u.report(j.ID, "packing %s", filepath.Base(f.From))
			if err := packFLAC(f.From, f.Path); err != nil {
				return err
			}
		}
		sum, size, err := fileSHA(f.Path)
		if err != nil {
			return err
		}
		files[i] = File{Role: f.Role, Bytes: size, SHA256: sum, Codec: f.Codec}
	}
	key := "rec." + j.ID
	rec := newRecord(j, files, u.now())
	v, err := u.reg.Create(ctx, key, recSchema, rec)
	switch {
	case pages.Code(err) == "version_conflict":
		var state string
		if rec, v, state, err = u.read(ctx, key); err != nil {
			return err
		}
		if state != "uploading" {
			if state == meeting.Deleted {
				u.report(j.ID, "deleted on the page: nothing uploaded")
				return nil
			}
			if state == "uploaded" {
				return u.queue(ctx, j.ID)
			}
			return nil
		}
	case err != nil:
		return err
	}
	have := recFiles(rec)
	for i, f := range j.Files {
		k := i // the record lists the job's files in order; a replaced one is found by role
		if k >= len(have) || have[k].Role != f.Role {
			k = slicesIndexRole(have, f.Role)
		}
		if k >= 0 && have[k].File != "" {
			continue
		}
		u.report(j.ID, "uploading %s (%d/%d, %.1f MB)", filepath.Base(f.Path), i+1, len(j.Files), float64(files[i].Bytes)/1e6)
		t0 := time.Now()
		id, sum, err := u.reg.Upload(ctx, f.Path, map[string]any{"rec": j.ID, "role": f.Role})
		if err != nil {
			return err
		}
		if sum != files[i].SHA256 {
			return fmt.Errorf("%s: the page has sha256 %s, the file %s", f.Path, sum, files[i].SHA256)
		}
		secs := time.Since(t0).Seconds()
		u.report(j.ID, "uploaded %s: %.1f MB in %.1f s (%.2f MB/s)", filepath.Base(f.Path), float64(files[i].Bytes)/1e6, secs, float64(files[i].Bytes)/1e6/max(secs, 1e-3))
		e := files[i]
		e.File = id
		if k >= 0 {
			have[k] = e
		} else {
			have = append(have, e)
		}
		setFiles(rec, have)
		rec["updated"] = u.now()
		if v, err = u.reg.CAS(ctx, key, recSchema, rec, v); err != nil {
			return err
		}
	}
	if j.Feishu != "" {
		fe := j.feishu()
		if k := slicesIndexRole(have, "feishu-transcript"); k >= 0 {
			fe["transcript_file"] = have[k].File
		}
		rec["feishu"] = fe
	}
	rec["state"] = "uploaded"
	rec["updated"] = u.now()
	if _, err := u.reg.CAS(ctx, key, recSchema, rec, v); err != nil {
		return err
	}
	return u.queue(ctx, j.ID)
}

// queue creates q.<id>, the drain's index; one that exists is left alone.
func (u *uploader) queue(ctx context.Context, id string) error {
	_, err := u.reg.Create(ctx, "q."+id, queueSchema, map[string]string{"state": "uploaded"})
	if pages.Code(err) == "version_conflict" {
		return nil
	}
	return err
}

// read returns a record as a generic map, so a CAS keeps every field this
// program does not know (claim, scores, …).
func (u *uploader) read(ctx context.Context, key string) (map[string]any, int, string, error) {
	o, err := u.reg.Record(ctx, key)
	if err != nil {
		return nil, 0, "", err
	}
	var rec map[string]any
	if err := json.Unmarshal(o.Value.Data, &rec); err != nil {
		return nil, 0, "", fmt.Errorf("%s: %w", key, err)
	}
	state, _ := rec["state"].(string)
	return rec, o.Version, state, nil
}

// replace uploads path as the record's file for role and writes it into
// files[] by CAS, retrying on another writer's change; verified goes false.
func (u *uploader) replace(ctx context.Context, id, role, path string) (File, error) {
	sum, size, err := fileSHA(path)
	if err != nil {
		return File{}, err
	}
	fid, got, err := u.reg.Upload(ctx, path, map[string]any{"rec": id, "role": role})
	if err != nil {
		return File{}, err
	}
	if got != sum {
		return File{}, fmt.Errorf("the page has sha256 %s, the file %s", got, sum)
	}
	e := File{Role: role, File: fid, Bytes: size, SHA256: sum, Codec: codecOf(path)}
	key := "rec." + id
	for try := 0; ; try++ {
		rec, v, _, err := u.read(ctx, key)
		if err != nil {
			return e, err
		}
		have := recFiles(rec)
		if k := slicesIndexRole(have, role); k >= 0 {
			have[k] = e
		} else {
			have = append(have, e)
		}
		setFiles(rec, have)
		rec["updated"] = u.now()
		_, err = u.reg.CAS(ctx, key, recSchema, rec, v)
		if pages.Code(err) == "version_conflict" && try < 5 {
			continue
		}
		return e, err
	}
}

func newRecord(j job, files []File, now time.Time) map[string]any {
	speakers := make([]map[string]string, max(j.Speakers, len(j.SpeakerNames)))
	for i := range speakers {
		speakers[i] = map[string]string{}
		if i < len(j.SpeakerNames) {
			speakers[i]["name"] = j.SpeakerNames[i]
		}
	}
	rec := map[string]any{
		"id": j.ID, "source": j.Source, "host": j.Host, "title": j.Title,
		"started": j.Started, "stopped": j.Stopped, "duration_s": j.DurationS,
		"speakers": speakers, "files": files, "state": "uploading", "attempts": 0, "updated": now,
	}
	if len(j.Apps) > 0 {
		rec["apps"] = j.Apps
	}
	if j.Test {
		rec["test"] = true
	}
	if j.Feishu != "" {
		rec["feishu"] = j.feishu()
	}
	if j.Wiki != nil {
		rec["wiki"] = j.Wiki
	}
	if j.Summary != nil {
		rec["summary"] = *j.Summary
	}
	return rec
}

// recFiles decodes the record's files[].
func recFiles(rec map[string]any) []File {
	b, _ := json.Marshal(rec["files"])
	var fs []File
	_ = json.Unmarshal(b, &fs)
	return fs
}

func setFiles(rec map[string]any, fs []File) { rec["files"] = fs }

func slicesIndexRole(fs []File, role string) int {
	for i, f := range fs {
		if f.Role == role {
			return i
		}
	}
	return -1
}

func fileSHA(path string) (string, int64, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", 0, err
	}
	defer f.Close()
	h := sha256.New()
	n, err := io.Copy(h, f)
	return hex.EncodeToString(h.Sum(nil)), n, err
}

func codecOf(path string) string {
	return strings.TrimPrefix(strings.ToLower(filepath.Ext(path)), ".")
}

// packFLAC encodes a WAV to FLAC with ffmpeg, through a temporary file so a
// crash never leaves a partial FLAC under the final name.
func packFLAC(wav, flac string) error {
	tmp := flac + ".part"
	cmd := exec.Command("ffmpeg", "-nostdin", "-loglevel", "error", "-y", "-i", wav, "-c:a", "flac", "-compression_level", "8", "-f", "flac", tmp)
	if out, err := cmd.CombinedOutput(); err != nil {
		os.Remove(tmp)
		return fmt.Errorf("ffmpeg %s: %v %s", filepath.Base(wav), err, strings.TrimSpace(string(out)))
	}
	return os.Rename(tmp, flac)
}

// probeDuration is a media file's length by ffprobe; 0 when it cannot say.
func probeDuration(path string) float64 {
	out, err := exec.Command("ffprobe", "-v", "error", "-show_entries", "format=duration", "-of", "csv=p=0", path).Output()
	if err != nil {
		return 0
	}
	d, _ := strconv.ParseFloat(strings.TrimSpace(string(out)), 64)
	return d
}

// prune removes recordings uploaded more than days ago; 0 keeps them all.
// A recording still in the ledger is never removed.
func prune(root string, l ledger, days int, now time.Time) {
	if days <= 0 {
		return
	}
	metas, _ := filepath.Glob(filepath.Join(root, "*", "meta.toml"))
	for _, p := range metas {
		dir := filepath.Dir(p)
		m, err := readMeta(dir)
		if err != nil || m.UploadedAt.IsZero() || now.Sub(m.UploadedAt) < time.Duration(days)*24*time.Hour {
			continue
		}
		if _, err := os.Stat(l.path(m.ID)); err == nil {
			continue
		}
		if err := os.RemoveAll(dir); err != nil {
			log.Printf("prune %s: %v", dir, err)
			continue
		}
		log.Printf("prune %s: uploaded %s, past %d days", m.ID, m.UploadedAt.Format(time.DateOnly), days)
	}
}
