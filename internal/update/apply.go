package update

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
)

// Apply installs rel over the bundle at appPath ("/Applications/Mega
// Voice.app"): it downloads rel.URL through the page grant into a staging
// folder beside appPath, checks its sha256, unpacks it, then swaps by rename —
// the previous bundle moves to BackupPath(appPath), replacing the one kept
// from the cycle before. The running process keeps executing its old,
// unlinked image; the caller relaunches it (Mega Voice: restart --when-idle).
//
// On ErrChecksum, ErrNotWritable or any other error the bundle at appPath
// is the one that was there.
func (u *Updater) Apply(ctx context.Context, rel *Release, appPath string) error {
	if !u.Enabled() {
		return ErrDisabled
	}
	appPath = filepath.Clean(appPath)
	parent, name := filepath.Dir(appPath), filepath.Base(appPath)
	stale, _ := filepath.Glob(filepath.Join(parent, "."+name+".update-*"))
	for _, s := range stale { // a previous run that died mid-way
		os.RemoveAll(s)
	}
	stage, err := os.MkdirTemp(parent, "."+name+".update-")
	if err != nil {
		return fmt.Errorf("%w: %v", ErrNotWritable, err)
	}
	defer os.RemoveAll(stage)

	archive := filepath.Join(stage, "download")
	if err := u.download(ctx, rel, archive); err != nil {
		return err
	}
	extract := u.cfg.Extract
	if extract == nil {
		extract = extractDefault
	}
	unpacked := filepath.Join(stage, "new")
	if err := os.Mkdir(unpacked, 0o755); err != nil {
		return fmt.Errorf("%w: %v", ErrNotWritable, err)
	}
	newApp, err := extract(ctx, archive, unpacked)
	if err != nil {
		return fmt.Errorf("update: unpack: %w", err)
	}
	return swap(newApp, appPath)
}

// BackupPath is where Apply keeps the previous bundle for one cycle: hidden,
// beside the app, so Finder does not list it.
func BackupPath(appPath string) string {
	appPath = filepath.Clean(appPath)
	return filepath.Join(filepath.Dir(appPath), "."+filepath.Base(appPath)+".bak")
}

func (u *Updater) download(ctx context.Context, rel *Release, dst string) error {
	want, err := hex.DecodeString(strings.ToLower(rel.SHA256))
	if err != nil || len(want) != sha256.Size {
		return fmt.Errorf("%w: feed sha256 %q is not 64 hex digits", ErrChecksum, rel.SHA256)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rel.URL, nil)
	if err != nil {
		return err
	}
	resp, err := u.do(ctx, req) // /f/ answers 302 to a presigned link; the client follows
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("update: download: %s", resp.Status)
	}
	f, err := os.Create(dst)
	if err != nil {
		return fmt.Errorf("%w: %v", ErrNotWritable, err)
	}
	h := sha256.New()
	_, err = io.Copy(io.MultiWriter(f, h), resp.Body)
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		return fmt.Errorf("update: download: %w", err)
	}
	if got := h.Sum(nil); string(got) != string(want) {
		return fmt.Errorf("%w: got %x, feed says %x", ErrChecksum, got, want)
	}
	return nil
}

// swap moves appPath to its backup and newApp to appPath, both renames on one
// filesystem; if the second fails the first is undone.
func swap(newApp, appPath string) error {
	bak := BackupPath(appPath)
	if err := os.RemoveAll(bak); err != nil {
		return fmt.Errorf("%w: %v", ErrNotWritable, err)
	}
	hadOld := true
	if err := os.Rename(appPath, bak); err != nil {
		if !errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("%w: %v", ErrNotWritable, err)
		}
		hadOld = false
	}
	if err := os.Rename(newApp, appPath); err != nil {
		if hadOld {
			os.Rename(bak, appPath)
		}
		return fmt.Errorf("%w: %v", ErrNotWritable, err)
	}
	return nil
}
