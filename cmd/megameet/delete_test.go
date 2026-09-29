package main

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/caoer/mega-asr/internal/pages"
)

func (f *fakeReg) Delete(_ context.Context, key string) error {
	if _, ok := f.recs[key]; !ok {
		return &pages.Error{Status: 404, Code: "no_such_object"}
	}
	delete(f.recs, key)
	f.ops = append(f.ops, "delete "+key)
	return nil
}

func (f *fakeReg) DeleteFile(_ context.Context, id string) error {
	f.ops = append(f.ops, "delete file "+id)
	return nil
}

// deleteOne refuses a name that is not a record id before it touches
// anything.
func TestDeleteRefusesAPath(t *testing.T) {
	reg := newFakeReg()
	for _, id := range []string{"", "../x", ".hidden", "wx/61"} {
		if err := deleteOne(context.Background(), reg, t.TempDir(), id, "t", time.Now()); err == nil || !strings.Contains(err.Error(), "not a record id") {
			t.Fatalf("%q: %v", id, err)
		}
	}
	if len(reg.ops) != 0 {
		t.Fatalf("writes: %v", reg.ops)
	}
}

// A deleted feishu record stays a tombstone: deleteOne removes the queue key,
// its files and this host's copies, and register-feishu then counts the
// minute as deleted and writes nothing to it, even as the archive and the
// wiki gain what a live record would take. A live minute beside it still
// gets its late fields.
func TestDeletedFeishuRecordStaysATombstone(t *testing.T) {
	w := newFeishuWorld(t)
	data := t.TempDir()
	const goneAt, keptAt = "2020-04-21T16:48:03Z", "2020-04-20T09:27:44Z"
	say := `{"segments": [{"speaker": "Alice Example", "start_s": 0, "text": "Tubeless or tubes?"}]}`
	gone := map[string]any{"title": "Tyre check", "created": goneAt, "state": "done", "fetched": "x"}
	kept := map[string]any{"title": "Gear cables", "created": keptAt, "state": "done", "fetched": "x"}
	w.minutes(map[string]map[string]any{"obcn6p4": gone, "obcn8r5": kept}, map[string]string{"obcn6p4": say, "obcn8r5": say})
	if tl, err := w.run(); err != nil || tl.registered != 2 {
		t.Fatalf("first run: %+v, %v", tl, err)
	}
	id := w.id(goneAt)
	for _, dir := range []string{filepath.Join(data, "recordings", id), filepath.Join(data, "pending", id)} {
		writeFile(t, filepath.Join(dir, "x"), "local copy")
	}

	if err := deleteOne(context.Background(), w.reg, data, id, "a test run", time.Now()); err != nil {
		t.Fatal(err)
	}
	m, files := w.reg.rec(t, id)
	if m["state"] != "deleted" || m["deleted_by"] != "a test run" || len(files) != 0 || m["feishu"].(map[string]any)["token"] != "obcn6p4" {
		t.Fatalf("tombstone %v", m)
	}
	if _, ok := w.reg.recs["q."+id]; ok {
		t.Fatal("the queue key stays")
	}
	if !slices.ContainsFunc(w.reg.ops, func(op string) bool { return strings.HasPrefix(op, "delete file ") }) {
		t.Fatalf("the transcript file stays: %v", w.reg.ops)
	}
	for _, dir := range []string{"recordings", "pending"} {
		if _, err := os.Stat(filepath.Join(data, dir, id)); !os.IsNotExist(err) {
			t.Fatalf("%s copy stays: %v", dir, err)
		}
	}

	gone["summary"], gone["url"] = "Tubes, for now.", "https://tenant.example/minutes/obcn6p4"
	kept["summary"] = "Both cables replaced."
	w.minutes(map[string]map[string]any{"obcn6p4": gone, "obcn8r5": kept}, nil)
	w.file("tyre-check", "obcn6p4", true)
	ops := len(w.reg.ops)
	if tl, err := w.run(); err != nil || tl.deleted != 1 || tl.known != 1 || tl.registered != 0 || tl.summarized != 1 || tl.urlSet != 0 || tl.wikiSet != 0 {
		t.Fatalf("after delete: %+v, %v", tl, err)
	}
	for _, op := range w.reg.ops[ops:] {
		if strings.Contains(op, id) {
			t.Fatalf("wrote the tombstone: %v", w.reg.ops[ops:])
		}
	}
	if m, _ := w.reg.rec(t, id); m["state"] != "deleted" || m["summary"] != nil || m["wiki"] != nil {
		t.Fatalf("tombstone after register-feishu: %v", m)
	}
}

// A job left in the ledger for a record deleted meanwhile uploads nothing.
func TestUploadOfDeletedRecordUploadsNothing(t *testing.T) {
	dir := tempDir(t)
	reg := newFakeReg()
	u := testUploader(reg, dir)
	j := testJob(t, dir)
	reg.CAS(context.Background(), "rec."+j.ID, recSchema, map[string]any{"id": j.ID, "state": "deleted"}, 0)
	if err := u.ledger.put(j); err != nil {
		t.Fatal(err)
	}
	if err := u.run(context.Background(), j.ID); err != nil {
		t.Fatal(err)
	}
	if reg.uploads != 0 || len(reg.ops) != 1 {
		t.Fatalf("ops %v", reg.ops)
	}
	if _, ok := reg.recs["q."+j.ID]; ok {
		t.Fatal("queued a tombstone")
	}
}
