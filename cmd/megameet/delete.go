package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/user"
	"path/filepath"
	"strings"
	"time"

	"github.com/caoer/mega-asr/internal/app"
	"github.com/caoer/mega-asr/internal/meeting"
	"github.com/caoer/mega-asr/internal/pages"
)

// deleteCmd makes each record a tombstone (meeting.Delete) and removes this
// host's copies of it: the recording, the pulled directory, the pending
// upload.
func deleteCmd(o app.LoadOpts, args []string) error {
	if len(args) == 0 {
		return errors.New("delete <id>...")
	}
	l, err := app.Load(o)
	if err != nil {
		return err
	}
	cl, err := pages.FromConfig(l.Meeting)
	if err != nil {
		return err
	}
	var errs []error
	for _, a := range args {
		id := strings.TrimPrefix(a, "rec.")
		if err := deleteOne(context.Background(), cl, l.Meeting.Data, id, deletedBy(), time.Now()); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

func deleteOne(ctx context.Context, pg meeting.Purger, data, id, by string, now time.Time) error {
	if id == "" || filepath.Base(id) != id || strings.HasPrefix(id, ".") {
		return fmt.Errorf("delete: %q is not a record id", id)
	}
	d, err := meeting.Delete(ctx, pg, id, by, now)
	if err != nil && !d.Tombstoned && !d.Queue && len(d.Files) == 0 {
		return err // nothing changed: the local copies stay too
	}
	if d.Tombstoned {
		fmt.Printf("rec.%s: deleted (tombstone, deleted_by %s)\n", id, by)
	} else {
		fmt.Printf("rec.%s: a tombstone already\n", id)
	}
	if d.Queue {
		fmt.Printf("  q.%s removed\n", id)
	}
	for _, f := range d.Files {
		fmt.Printf("  file %s removed\n", f)
	}
	errs := []error{err}
	for _, dir := range []string{filepath.Join(data, "recordings", id), meeting.PullDir(data, id), filepath.Join(data, "pending", id)} {
		if _, serr := os.Lstat(dir); serr != nil {
			continue
		}
		if rerr := os.RemoveAll(dir); rerr != nil {
			errs = append(errs, rerr)
			continue
		}
		fmt.Printf("  %s removed\n", dir)
	}
	return errors.Join(errs...)
}

// deletedBy names who deletes: the tool, the user, the host.
func deletedBy() string {
	who := "unknown"
	if u, err := user.Current(); err == nil {
		who = u.Username
	}
	return fmt.Sprintf("megameet delete (%s@%s)", who, hostName())
}
