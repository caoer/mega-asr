package update

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"syscall"
)

// osVersion is the running macOS product version ("15.1").
func osVersion() string {
	v, _ := syscall.Sysctl("kern.osproductversion")
	return v
}

// extractDefault unpacks a zip (ditto -c -k --keepParent) with ditto, or a
// dmg by attaching it read-only and copying its .app with ditto, which keeps
// the bundle's symlinks, modes and signature intact.
func extractDefault(ctx context.Context, archive, dir string) (string, error) {
	head := make([]byte, 4)
	if f, err := os.Open(archive); err == nil {
		f.Read(head)
		f.Close()
	}
	if bytes.Equal(head, []byte("PK\x03\x04")) {
		if err := run(ctx, "/usr/bin/ditto", "-x", "-k", archive, dir); err != nil {
			return "", err
		}
		return findApp(dir)
	}
	mnt := filepath.Join(filepath.Dir(dir), "mnt")
	if err := os.Mkdir(mnt, 0o755); err != nil {
		return "", err
	}
	if err := run(ctx, "/usr/bin/hdiutil", "attach", "-nobrowse", "-readonly", "-noautoopen", "-mountpoint", mnt, archive); err != nil {
		return "", err
	}
	defer run(context.Background(), "/usr/bin/hdiutil", "detach", mnt, "-force")
	src, err := findApp(mnt)
	if err != nil {
		return "", err
	}
	dst := filepath.Join(dir, filepath.Base(src))
	if err := run(ctx, "/usr/bin/ditto", src, dst); err != nil {
		return "", err
	}
	return dst, nil
}

func run(ctx context.Context, name string, args ...string) error {
	out, err := exec.CommandContext(ctx, name, args...).CombinedOutput()
	if err != nil {
		return fmt.Errorf("%s: %w: %s", filepath.Base(name), err, bytes.TrimSpace(out))
	}
	return nil
}
