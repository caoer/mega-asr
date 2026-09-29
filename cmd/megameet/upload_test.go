package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/caoer/mega-asr/internal/pages"
)

// fakeReg is the page's registry in memory; ops logs every write in order.
type fakeReg struct {
	recs       map[string]pages.Object
	ops        []string
	uploads    int
	failUpload int // the n-th upload (1-based) fails once
	failCAS    int // the next n CASes lose to another writer
}

func newFakeReg() *fakeReg { return &fakeReg{recs: map[string]pages.Object{}} }

func (f *fakeReg) CAS(_ context.Context, key, schema string, data any, v int) (int, error) {
	cur, ok := f.recs[key]
	if f.failCAS > 0 && v > 0 {
		f.failCAS--
		cur.Version++
		f.recs[key] = cur
		return 0, &pages.Error{Status: 409, Code: "version_conflict", Version: cur.Version}
	}
	if (v == 0 && ok) || (v > 0 && cur.Version != v) {
		return 0, &pages.Error{Status: 409, Code: "version_conflict", Version: cur.Version}
	}
	b, _ := json.Marshal(data)
	f.recs[key] = pages.Object{Key: key, Value: pages.Value{Schema: schema, Data: b}, Version: v + 1}
	var d struct{ State string }
	json.Unmarshal(b, &d)
	f.ops = append(f.ops, fmt.Sprintf("put %s v%d %s", key, v, d.State))
	return v + 1, nil
}

func (f *fakeReg) Create(ctx context.Context, key, schema string, data any) (int, error) {
	return f.CAS(ctx, key, schema, data, 0)
}

func (f *fakeReg) Record(_ context.Context, key string) (pages.Object, error) {
	o, ok := f.recs[key]
	if !ok {
		return o, &pages.Error{Status: 404, Code: "no_such_object"}
	}
	return o, nil
}

func (f *fakeReg) Upload(_ context.Context, path string, meta map[string]any) (string, string, error) {
	f.uploads++
	if f.uploads == f.failUpload {
		return "", "", errors.New("connection reset")
	}
	sum, _, err := fileSHA(path)
	id := fmt.Sprintf("file%d", f.uploads)
	f.ops = append(f.ops, fmt.Sprintf("upload %s %v", filepath.Base(path), meta["role"]))
	return id, sum, err
}

func (f *fakeReg) rec(t *testing.T, id string) (map[string]any, []File) {
	t.Helper()
	o, ok := f.recs["rec."+id]
	if !ok {
		t.Fatalf("no rec.%s", id)
	}
	var m map[string]any
	json.Unmarshal(o.Value.Data, &m)
	return m, recFiles(m)
}

func testJob(t *testing.T, dir string) job {
	t.Helper()
	j := job{ID: "20200815-101500-mac-host-a", Source: "mac", Host: "host-a", Started: time.Now()}
	for _, role := range []string{"remote", "mic"} {
		p := filepath.Join(dir, role+".flac")
		if err := os.WriteFile(p, []byte("fLaC "+role), 0o644); err != nil {
			t.Fatal(err)
		}
		j.Files = append(j.Files, jobFile{Role: role, Path: p, Codec: "flac"})
	}
	return j
}

func testUploader(reg registry, dir string) *uploader {
	return &uploader{reg: reg, ledger: ledger(filepath.Join(dir, "pending")), now: time.Now}
}

// An upload killed after its first file resumes from the ledger: one
// record, the first file not sent again, and uploaded written only after
// the last file, then the queue key.
func TestKilledUploadResumes(t *testing.T) {
	dir := tempDir(t)
	reg := newFakeReg()
	reg.failUpload = 2
	u := testUploader(reg, dir)
	j := testJob(t, dir)
	if err := u.ledger.put(j); err != nil {
		t.Fatal(err)
	}
	if err := u.resume(context.Background()); err == nil {
		t.Fatal("first run: want the upload's error")
	}
	if _, err := os.Stat(u.ledger.path(j.ID)); err != nil {
		t.Fatalf("ledger entry gone after a failed upload: %v", err)
	}
	if m, _ := reg.rec(t, j.ID); m["state"] != "uploading" {
		t.Fatalf("state %v after a failed upload", m["state"])
	}

	if err := u.resume(context.Background()); err != nil {
		t.Fatal(err)
	}
	var recs int
	for k := range reg.recs {
		if strings.HasPrefix(k, "rec.") {
			recs++
		}
	}
	m, files := reg.rec(t, j.ID)
	if recs != 1 || m["state"] != "uploaded" || reg.recs["q."+j.ID].Version != 1 {
		t.Fatalf("%d records, state %v, queue %+v", recs, m["state"], reg.recs["q."+j.ID])
	}
	if len(files) != 2 || files[0].File != "file1" || files[1].File != "file3" || files[1].SHA256 == "" {
		t.Fatalf("files %+v", files)
	}
	want := []string{
		"put rec." + j.ID + " v0 uploading",
		"upload remote.flac remote",
		"put rec." + j.ID + " v1 uploading",
		"upload mic.flac mic",
		"put rec." + j.ID + " v2 uploading",
		"put rec." + j.ID + " v3 uploaded",
		"put q." + j.ID + " v0 uploaded",
	}
	if strings.Join(reg.ops, "\n") != strings.Join(want, "\n") {
		t.Fatalf("ops:\n%s\nwant:\n%s", strings.Join(reg.ops, "\n"), strings.Join(want, "\n"))
	}
	if _, err := os.Stat(u.ledger.path(j.ID)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("ledger entry still there: %v", err)
	}
}

// A record already past uploaded clears the ledger and uploads nothing.
func TestResumeOfUploadedRecordUploadsNothing(t *testing.T) {
	dir := tempDir(t)
	reg := newFakeReg()
	u := testUploader(reg, dir)
	j := testJob(t, dir)
	reg.Create(context.Background(), "rec."+j.ID, recSchema, map[string]any{"id": j.ID, "state": "processing"})
	reg.ops = nil
	u.ledger.put(j)
	if err := u.resume(context.Background()); err != nil {
		t.Fatal(err)
	}
	if reg.uploads != 0 || len(reg.ops) != 0 {
		t.Fatalf("uploads %d, ops %v", reg.uploads, reg.ops)
	}
	if _, err := os.Stat(u.ledger.path(j.ID)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("ledger entry still there: %v", err)
	}
}

// replace puts a new file in its role's slot, through lost races, with a
// fresh digest and verified cleared; a role the record lacks is appended.
// Fields the uploader does not know stay.
func TestReplaceFile(t *testing.T) {
	dir := tempDir(t)
	reg := newFakeReg()
	u := testUploader(reg, dir)
	j := testJob(t, dir)
	ctx := context.Background()
	if err := u.push(ctx, j); err != nil {
		t.Fatal(err)
	}
	m, v, _, _ := u.read(ctx, "rec."+j.ID)
	m["review"] = map[string]any{"due": "2020-05-02"}
	m["files"].([]any)[0].(map[string]any)["verified"] = true
	reg.CAS(ctx, "rec."+j.ID, recSchema, m, v)

	fresh := func(name, body string) string {
		p := filepath.Join(dir, name)
		if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
		return p
	}
	reg.failCAS = 2
	remote, err := u.replace(ctx, j.ID, "remote", fresh("remote-2.flac", "fLaC second remote"))
	if err != nil {
		t.Fatal(err)
	}
	beam, err := u.replace(ctx, j.ID, "beam", fresh("beam-take.wav", "RIFF beam"))
	if err != nil {
		t.Fatal(err)
	}

	m, files := reg.rec(t, j.ID)
	var got []string
	for _, f := range files {
		got = append(got, fmt.Sprintf("%s=%s verified=%v", f.Role, f.File, f.Verified))
	}
	want := []string{"remote=" + remote.File + " verified=false", "mic=file2 verified=false", "beam=" + beam.File + " verified=false"}
	if strings.Join(got, ", ") != strings.Join(want, ", ") {
		t.Fatalf("files %v, want %v", got, want)
	}
	sum, _, _ := fileSHA(filepath.Join(dir, "remote-2.flac"))
	if files[0].SHA256 != sum || files[2].Codec != "wav" || m["review"] == nil || m["state"] != "uploaded" {
		t.Fatalf("after replace: %+v, review %v, state %v", files, m["review"], m["state"])
	}
}

// --test on start reaches the record through meta.toml and the job;
// a record without it carries no test key.
func TestTestFlagReachesTheRecord(t *testing.T) {
	rec := newRecord(macJob(t.TempDir(), Meta{ID: "20200816-220000-mac-host-a", Source: "mac", Test: true}), nil, time.Now())
	if rec["test"] != true {
		t.Fatalf("record %v", rec)
	}
	if _, ok := newRecord(job{ID: "x", Source: "file"}, nil, time.Now())["test"]; ok {
		t.Fatal("test written on a record that is not one")
	}
}
