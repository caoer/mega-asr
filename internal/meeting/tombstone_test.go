package meeting

import (
	"context"
	"slices"
	"strings"
	"testing"
	"time"
)

// A delete leaves rec.<id> as a tombstone that still dedupes (feishu.token)
// and names who deleted it; q.<id> and every file of the record go, an
// orphan f. whose meta.rec is the id included. A second delete purges
// again and keeps the first tombstone.
func TestDeleteTombstones(t *testing.T) {
	now := time.Date(2020, 5, 19, 8, 45, 0, 0, time.UTC)
	r := Record{ID: "x", Source: "feishu", Title: "Shelf audit", Started: now.Add(-time.Hour), DurationS: 600, State: Aligned,
		Files:    []File{{Role: "media", File: "f1"}, {Role: "feishu-transcript", File: "f2"}},
		Feishu:   &Feishu{Token: "obcnx", TranscriptFile: "f2"},
		Scores:   &Scores{CER: 0.1},
		Claim:    &Claim{By: "host-b:1", At: now.Add(-2 * time.Hour), Expires: now.Add(-time.Hour)},
		Speakers: []Speaker{{Role: "media"}}}
	f := newFakeReg(t, r)
	ctx := context.Background()
	f.CAS(ctx, "q.x", "queue@1", map[string]string{"state": "uploaded"}, 0)
	f.CAS(ctx, "f.orphan", "file@1", map[string]any{"meta": map[string]any{"rec": "x", "role": "segments"}}, 0)
	f.CAS(ctx, "f.other", "file@1", map[string]any{"meta": map[string]any{"rec": "y", "role": "media"}}, 0)

	d, err := Delete(ctx, f, "x", "test delete", now)
	if err != nil {
		t.Fatal(err)
	}
	if !d.Tombstoned || !d.Queue || !slices.Equal(d.Files, []string{"f1", "f2", "orphan"}) {
		t.Fatalf("deletion %+v", d)
	}
	m := f.raw(t, "rec.x")
	if m["state"] != Deleted || m["deleted_by"] != "test delete" || m["deleted_at"] != "2020-05-19T08:45:00Z" || m["title"] != "Shelf audit" {
		t.Fatalf("tombstone %v", m)
	}
	if fe := m["feishu"].(map[string]any); fe["token"] != "obcnx" || len(fe) != 1 {
		t.Fatalf("tombstone feishu %v", fe)
	}
	for _, k := range []string{"files", "scores", "claim", "speakers", "summary"} {
		if _, ok := m[k]; ok {
			t.Fatalf("tombstone keeps %s: %v", k, m)
		}
	}
	if _, err := f.Record(ctx, "q.x"); err == nil {
		t.Fatal("q.x stays")
	}
	if slices.Contains(f.deleted, "other") {
		t.Fatal("another record's file went")
	}
	if _, err := Pull(ctx, f, "x", t.TempDir()); err == nil || !strings.Contains(err.Error(), "deleted") {
		t.Fatalf("pull of a tombstone: %v", err)
	}

	d, err = Delete(ctx, f, "x", "someone else", now.Add(time.Hour))
	if err != nil || d.Tombstoned || d.Queue {
		t.Fatalf("second delete: %+v %v", d, err)
	}
	if m := f.raw(t, "rec.x"); m["deleted_by"] != "test delete" {
		t.Fatalf("second delete rewrote the tombstone: %v", m)
	}
}

// A record a drain holds is not deleted from under it.
func TestDeleteRefusesLiveClaim(t *testing.T) {
	now := time.Date(2020, 5, 19, 8, 45, 0, 0, time.UTC)
	f := newFakeReg(t, Record{ID: "x", Source: "mac", State: Processing, Files: []File{{Role: "mic", File: "f1"}},
		Claim: &Claim{By: "host-b:1", At: now, Expires: now.Add(time.Minute)}})
	if _, err := Delete(context.Background(), f, "x", "t", now); err == nil || !strings.Contains(err.Error(), "claimed by host-b:1") {
		t.Fatalf("delete under a live claim: %v", err)
	}
	if m := f.raw(t, "rec.x"); m["state"] != Processing || len(f.deleted) != 0 {
		t.Fatalf("record %v, files deleted %v", m, f.deleted)
	}
}

// Align never pairs against a tombstone: a deleted minute is absent.
func TestAlignIgnoresTombstones(t *testing.T) {
	now := time.Date(2020, 5, 18, 14, 0, 0, 0, time.UTC)
	local := Record{ID: "l", Source: "mac", State: Processing, Started: now, DurationS: 1800}
	gone := Record{ID: "g", Source: "feishu", State: Deleted, Started: now, DurationS: 1800}
	a := &Aligner{Reg: newFakeReg(t, local, gone)}
	p, err := a.Claimed(context.Background(), "l", false)
	if err != nil || p == nil || !p.Unpaired {
		t.Fatalf("patch %+v, %v: want unpaired", p, err)
	}
}
