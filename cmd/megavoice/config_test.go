package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/caoer/mega-asr/internal/app"
	"github.com/caoer/mega-asr/internal/audio"
)

func TestTapKeyChecked(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "megavoice")
	t.Setenv("XDG_CONFIG_HOME", filepath.Dir(dir))
	t.Setenv("MEGAVOICE_CONFIG", "")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "config.toml"), []byte("[tap]\nkey = \"caps\"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := load(app.LoadOpts{}); err == nil || !strings.Contains(err.Error(), "tap.key") {
		t.Fatalf("got %v, want a tap.key error", err)
	}
}

func TestSetRefusesTapKey(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.toml")
	in := "# dictation\n[tap]\nwindow = \"300ms\"\n"
	if err := os.WriteFile(path, []byte(in), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := set(path, "tap.key", app.Value("caps")); err == nil || !strings.Contains(err.Error(), "tap.key") {
		t.Fatalf("got %v, want a tap.key error", err)
	}
	if b, _ := os.ReadFile(path); string(b) != in {
		t.Errorf("file changed: %s", b)
	}
	if err := set(path, "tap.key", app.Value("right_option")); err != nil {
		t.Fatal(err)
	}
}

// Each source setting builds its own input.
func TestSourceFromConfig(t *testing.T) {
	x, ok := captureSource(app.CaptureConfig{Source: "ssh", Host: "h", Device: "d", Channel: 5}).(*audio.XVF)
	if !ok || x.Host != "h" || x.Device != "d" || x.Channel != 5 {
		t.Errorf("ssh: %+v", x)
	}
	m, ok := captureSource(app.CaptureConfig{Source: "local", Mic: "stage-interface", MicName: "Stage Interface", MicChannel: 4, Host: "h"}).(*audio.Mic)
	if !ok || m.UID != "stage-interface" || m.Name != "Stage Interface" || m.Channel != 4 || m.Tail <= 0 {
		t.Errorf("local: %+v", m)
	}
}

// A take's backup follows capture.backup as the file says when the take
// starts: an input of this Mac beside a local main input; none for "off" or
// "", and none beside an ssh or remote source, which records on another
// machine.
func TestBackupSource(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "megavoice")
	t.Setenv("XDG_CONFIG_HOME", filepath.Dir(dir))
	t.Setenv("MEGAVOICE_CONFIG", "")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	newBackup := backupSource(app.LoadOpts{}, app.Default())
	main := &audio.Mic{UID: "podium-condenser"}
	for _, c := range []struct {
		backup string // "-": no file
		main   audio.Source
		want   string // the backup Mic's UID; "" none
	}{
		{"boom-mic", main, "boom-mic"},
		{"auto", &audio.Remote{Name: "micbox"}, ""},
		{"-", main, "auto"},
		{"off", main, ""},
		{"auto", main, "auto"},
		{"boom-mic", &audio.XVF{Host: "micbox"}, ""},
		{"", main, ""},
	} {
		os.Remove(filepath.Join(dir, "config.toml"))
		if c.backup != "-" {
			if err := os.WriteFile(filepath.Join(dir, "config.toml"), []byte("[capture]\nbackup = \""+c.backup+"\"\n"), 0o644); err != nil {
				t.Fatal(err)
			}
		}
		got := ""
		switch b := newBackup(c.main).(type) {
		case nil:
		case *audio.Mic:
			if !b.Backup {
				t.Errorf("backup %q: a Mic that is not a backup", c.backup)
			}
			got = b.UID
		default:
			t.Errorf("backup %q: %T", c.backup, b)
		}
		if got != c.want {
			t.Errorf("backup %q beside %T: %q, want %q", c.backup, c.main, got, c.want)
		}
	}
}

// A remote source is the paired remote's entry in remotes.toml; one no
// longer paired is refused by config set and fails the take that names it.
func TestRemoteSource(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "megavoice")
	t.Setenv("XDG_CONFIG_HOME", filepath.Dir(dir))
	t.Setenv("MEGAVOICE_CONFIG", "")
	mb := app.Remote{Name: "micbox", Addr: "micbox:7866", Fingerprint: "ab", Token: "tok", Client: "studio"}
	if err := app.SaveRemote(app.RemotesPath(), mb); err != nil {
		t.Fatal(err)
	}
	r, ok := captureSource(app.CaptureConfig{Source: "remote", Remote: "micbox"}).(*audio.Remote)
	if !ok || r.Name != "micbox" || r.Addr != mb.Addr || r.Fingerprint != "ab" || r.Token != "tok" || r.Tail <= 0 {
		t.Errorf("remote: %+v", r)
	}
	if _, err := captureSource(app.CaptureConfig{Source: "remote", Remote: "den"}).Start(t.Context()); err == nil || !strings.Contains(err.Error(), `"den" is paired`) {
		t.Errorf("an unpaired remote's take: %v", err)
	}

	path := filepath.Join(dir, "config.toml")
	if err := set(path, "capture.remote", `"den"`); err != nil {
		t.Fatal(err) // named but not the source: nothing to check yet
	}
	if err := set(path, "capture.source", `"remote"`); err == nil || !strings.Contains(err.Error(), "capture.remote") {
		t.Errorf("source remote with den unpaired: %v", err)
	}
	for _, kv := range remotePick("micbox") {
		if err := set(path, kv[0], kv[1]); err != nil {
			t.Fatal(err)
		}
	}
	if l, err := load(app.LoadOpts{}); err != nil || l.Capture.Source != "remote" || l.Capture.Remote != "micbox" {
		t.Errorf("after the pick: %+v %v", l.Capture, err)
	}
}

// The source is read from the file as each take starts; a file that no
// longer loads keeps serve's.
func TestSourceReadsTheFileAtEachTake(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "megavoice")
	t.Setenv("XDG_CONFIG_HOME", filepath.Dir(dir))
	t.Setenv("MEGAVOICE_CONFIG", "")
	os.MkdirAll(dir, 0o755)
	path := filepath.Join(dir, "config.toml")
	os.WriteFile(path, []byte("[capture]\nhost = \"micbox\"\ndevice = \"hw:Array,0\"\n"), 0o644)
	serve := app.Default()
	next := source(app.LoadOpts{}, serve)
	if _, ok := next().(*audio.Mic); !ok {
		t.Fatal("local: not a Mic")
	}
	if err := set(path, "capture.source", `"ssh"`); err != nil {
		t.Fatal(err)
	}
	if x, ok := next().(*audio.XVF); !ok || x.Host != "micbox" || x.Device != "hw:Array,0" {
		t.Fatalf("after config set capture.source ssh: %+v", next())
	}
	os.WriteFile(path, []byte("[capture\n"), 0o644)
	if _, ok := next().(*audio.Mic); !ok {
		t.Fatal("broken file: not serve's local source")
	}
}
