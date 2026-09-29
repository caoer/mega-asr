package meeting

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/caoer/mega-asr/internal/pages"
)

// Deleted is a tombstone's state. A deleted recording's files and queue key
// are gone; rec.<id> stays, so no source registers it again and no drain
// claims it. Every reader of rec. treats a tombstone as absent.
const Deleted = "deleted"

// tombstoneKeeps are the fields a tombstone carries over: what names the
// recording and what dedupes it (feishu.token, reduced to the token).
var tombstoneKeeps = []string{"id", "source", "host", "owner", "title", "started", "stopped", "duration_s", "wiki"}

// Tombstone is rec's tombstone: the kept fields, state deleted, deleted_at
// and deleted_by. Files, summary, speakers, scores, claim and the rest go.
func Tombstone(rec map[string]any, by string, now time.Time) map[string]any {
	t := map[string]any{}
	for _, k := range tombstoneKeeps {
		if v, ok := rec[k]; ok {
			t[k] = v
		}
	}
	if fe, ok := rec["feishu"].(map[string]any); ok && fe["token"] != nil {
		t["feishu"] = map[string]any{"token": fe["token"]}
	}
	at := now.UTC().Format(time.RFC3339)
	t["state"], t["deleted_at"], t["deleted_by"], t["updated"] = Deleted, at, by, at
	return t
}

// Purger is the page as a delete uses it; *pages.Client is the real one.
type Purger interface {
	Record(ctx context.Context, key string) (pages.Object, error)
	CAS(ctx context.Context, key, schema string, data any, version int) (int, error)
	List(ctx context.Context, prefix string) ([]pages.Object, error)
	Delete(ctx context.Context, key string) error
	DeleteFile(ctx context.Context, fileID string) error
}

// Deletion is what Delete did.
type Deletion struct {
	Tombstoned bool     // false: rec.<id> was a tombstone already
	Queue      bool     // q.<id> was there and is gone
	Files      []string // the file ids deleted
}

// Delete makes rec.<id> a tombstone by compare-and-swap, then purges its
// queue key and every file of it: those the record named and any f. whose
// meta.rec is the id. It refuses a record a drain holds a live claim on.
// Run on a tombstone it purges whatever an earlier run left.
func Delete(ctx context.Context, pg Purger, id, by string, now time.Time) (Deletion, error) {
	var d Deletion
	key := "rec." + id
	var files []string
	for try := 0; ; try++ {
		o, err := pg.Record(ctx, key)
		if err != nil {
			return d, fmt.Errorf("%s: %w", key, err)
		}
		var rec map[string]any
		if err := json.Unmarshal(o.Value.Data, &rec); err != nil {
			return d, fmt.Errorf("%s: %w", key, err)
		}
		if rec["state"] == Deleted {
			break
		}
		var r Record
		_ = json.Unmarshal(o.Value.Data, &r)
		if r.Claim != nil && r.Claim.Expires.After(now) {
			return d, fmt.Errorf("%s: claimed by %s until %s: delete it when that finishes", key, r.Claim.By, r.Claim.Expires.Format(time.RFC3339))
		}
		files = files[:0]
		for _, f := range r.Files {
			if f.File != "" {
				files = append(files, f.File)
			}
		}
		_, err = pg.CAS(ctx, key, Schema, Tombstone(rec, by, now), o.Version)
		if pages.Code(err) == "version_conflict" && try < 5 {
			continue
		}
		if err != nil {
			return d, fmt.Errorf("%s: tombstone: %w", key, err)
		}
		d.Tombstoned = true
		break
	}
	var errs []error
	switch err := pg.Delete(ctx, "q."+id); {
	case err == nil:
		d.Queue = true
	case !absent(err):
		errs = append(errs, fmt.Errorf("q.%s: %w", id, err))
	}
	fs, err := pg.List(ctx, "f.")
	if err != nil {
		errs = append(errs, fmt.Errorf("list files: %w", err))
	}
	for _, o := range fs {
		var v struct {
			Meta map[string]any `json:"meta"`
		}
		if json.Unmarshal(o.Value.Data, &v) == nil && v.Meta["rec"] == id {
			files = append(files, strings.TrimPrefix(o.Key, "f."))
		}
	}
	seen := map[string]bool{}
	for _, f := range files {
		if seen[f] {
			continue
		}
		seen[f] = true
		switch err := pg.DeleteFile(ctx, f); {
		case err == nil:
			d.Files = append(d.Files, f)
		case !absent(err):
			errs = append(errs, fmt.Errorf("file %s: %w", f, err))
		}
	}
	return d, errors.Join(errs...)
}

func absent(err error) bool {
	var e *pages.Error
	return errors.As(err, &e) && (e.Status == 404 || e.Code == "no_such_object" || e.Code == "no_such_file" || e.Code == "not_found")
}
