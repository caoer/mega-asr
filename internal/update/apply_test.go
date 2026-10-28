package update

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
)

// fakeExtract stands in for ditto/hdiutil: the bundle it unpacks holds the
// downloaded bytes in Contents/marker.
func fakeExtract(_ context.Context, archive, dir string) (string, error) {
	b, err := os.ReadFile(archive)
	if err != nil {
		return "", err
	}
	app := filepath.Join(dir, "Mega Voice.app")
	if err := os.MkdirAll(filepath.Join(app, "Contents"), 0o755); err != nil {
		return "", err
	}
	return app, os.WriteFile(filepath.Join(app, "Contents", "marker"), b, 0o644)
}

func installed(t *testing.T, content string) string {
	t.Helper()
	app := filepath.Join(t.TempDir(), "Mega Voice.app")
	os.MkdirAll(filepath.Join(app, "Contents"), 0o755)
	os.WriteFile(filepath.Join(app, "Contents", "marker"), []byte(content), 0o644)
	return app
}

func marker(t *testing.T, app string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(app, "Contents", "marker"))
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// onlyApp asserts the app's folder holds the app and, when bak, its backup —
// no staging folder left behind.
func onlyApp(t *testing.T, app string, bak bool) {
	t.Helper()
	entries, _ := os.ReadDir(filepath.Dir(app))
	want := 1
	if bak {
		want = 2
	}
	if len(entries) != want {
		var names []string
		for _, e := range entries {
			names = append(names, e.Name())
		}
		t.Fatalf("app folder holds %q, want %d entries", names, want)
	}
}

func updater(t *testing.T, p *pages) *Updater {
	cfg := p.config(t, "v0.1.0")
	cfg.Extract = fakeExtract
	return mustNew(t, cfg)
}

func TestApplySwapsKeepingOneBackup(t *testing.T) {
	p := newPages(t, "v0.2.0")
	u := updater(t, p)
	app := installed(t, "v0.1.0 bytes")
	rel, err := u.Check(context.Background())
	if err != nil || rel == nil {
		t.Fatal(rel, err)
	}
	if err := u.Apply(context.Background(), rel, app); err != nil {
		t.Fatal(err)
	}
	if got := marker(t, app); got != "new bundle bytes" {
		t.Fatalf("installed = %q", got)
	}
	if got := marker(t, BackupPath(app)); got != "v0.1.0 bytes" {
		t.Fatalf("backup = %q", got)
	}
	onlyApp(t, app, true)

	// the next cycle replaces the backup with the bundle it swaps out
	p.mu.Lock()
	p.blob = []byte("third bytes")
	p.mu.Unlock()
	p.setFeed("v0.3.0")
	rel, _ = u.Check(context.Background())
	if err := u.Apply(context.Background(), rel, app); err != nil {
		t.Fatal(err)
	}
	if marker(t, app) != "third bytes" || marker(t, BackupPath(app)) != "new bundle bytes" {
		t.Fatalf("after second apply: app %q, backup %q", marker(t, app), marker(t, BackupPath(app)))
	}
	onlyApp(t, app, true)
}

func TestApplyBadSHAKeepsOldApp(t *testing.T) {
	p := newPages(t, "v0.2.0")
	u := updater(t, p)
	app := installed(t, "v0.1.0 bytes")
	rel, err := u.Check(context.Background())
	if err != nil || rel == nil {
		t.Fatal(rel, err)
	}
	p.mu.Lock()
	p.blob = []byte("tampered") // the bytes no longer match the feed's sha256
	p.mu.Unlock()
	err = u.Apply(context.Background(), rel, app)
	if !errors.Is(err, ErrChecksum) || UserMessage(err) != "更新校验失败，下次再试" {
		t.Fatalf("err = %v, want ErrChecksum", err)
	}
	if got := marker(t, app); got != "v0.1.0 bytes" {
		t.Fatalf("installed = %q, want the old app", got)
	}
	onlyApp(t, app, false)
}

func TestApplyUnwritableFolderKeepsOldApp(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root writes anywhere")
	}
	p := newPages(t, "v0.2.0")
	u := updater(t, p)
	app := installed(t, "v0.1.0 bytes")
	rel, _ := u.Check(context.Background())
	dir := filepath.Dir(app)
	os.Chmod(dir, 0o555)
	t.Cleanup(func() { os.Chmod(dir, 0o755) })
	err := u.Apply(context.Background(), rel, app)
	if !errors.Is(err, ErrNotWritable) || UserMessage(err) != "请从页面重新安装" {
		t.Fatalf("err = %v, want ErrNotWritable", err)
	}
	if got := marker(t, app); got != "v0.1.0 bytes" {
		t.Fatalf("installed = %q", got)
	}
}
