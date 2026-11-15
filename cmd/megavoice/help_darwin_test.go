package main

import (
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/caoer/mega-asr/internal/app"
	"github.com/caoer/mega-asr/internal/ctl"
)

// fakeAgent answers the control socket and keeps every request it gets.
func fakeAgent(t *testing.T) func() []string {
	state, err := os.MkdirTemp("", "mv") // short: a socket path takes 104 bytes on macOS
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(state) })
	t.Setenv("XDG_STATE_HOME", state)
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	t.Setenv("XDG_DATA_HOME", t.TempDir())
	if err := os.MkdirAll(stateDir(), 0o700); err != nil {
		t.Fatal(err)
	}
	var mu sync.Mutex
	var got []string
	l, err := ctl.Listen(sockPath(), func(r ctl.Request) ctl.Response {
		mu.Lock()
		defer mu.Unlock()
		got = append(got, r.Cmd)
		return ctl.Reply(nil)
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { l.Close() })
	return func() []string {
		mu.Lock()
		defer mu.Unlock()
		return append([]string(nil), got...)
	}
}

// stdoutOf runs f with os.Stdout going to a file and returns what it wrote.
func stdoutOf(t *testing.T, f func()) string {
	path := filepath.Join(t.TempDir(), "stdout")
	w, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	old := os.Stdout
	os.Stdout = w
	defer func() { os.Stdout = old }()
	f()
	w.Close()
	b, _ := os.ReadFile(path)
	return string(b)
}

// `megavoice <subcommand> --help` prints its usage and sends the running
// agent nothing: `toggle --help` once started a take on a running serve.
func TestHelpRunsNothing(t *testing.T) {
	for _, args := range [][]string{
		{"toggle", "--help"}, {"send", "--help"}, {"cancel", "--help"},
		{"quit", "--help"}, {"spike", "--help"}, {"status", "-h"},
		{"start", "--help"}, {"restart", "--help"}, {"paste", "--help"},
		{"resend", "--help"}, {"resend", "20261115-062335", "--help"},
		{"retranscribe", "--help"}, {"takes", "--help"}, {"takes", "url", "--rotate", "-help"},
		{"events", "--help"}, {"update", "--help"}, {"remote", "--help"},
		{"remote", "rotate", "--help"}, {"mic", "add", "box", "--help"},
	} {
		t.Run(strings.Join(args, " "), func(t *testing.T) {
			got := fakeAgent(t)
			var err error
			out := stdoutOf(t, func() { err = dispatch(app.LoadOpts{}, args[0], args[1:]) })
			if err != nil {
				t.Errorf("err = %v, want none", err)
			}
			if !strings.HasPrefix(out, "usage: megavoice") {
				t.Errorf("stdout = %q, want the usage", out)
			}
			if r := got(); len(r) > 0 {
				t.Errorf("the agent got %q", r)
			}
		})
	}
}

// A subcommand that takes no arguments refuses one instead of running.
func TestBareVerbRefusesArgs(t *testing.T) {
	for _, cmd := range []string{"toggle", "send", "cancel", "quit", "spike", "status", "start"} {
		t.Run(cmd, func(t *testing.T) {
			got := fakeAgent(t)
			err := dispatch(app.LoadOpts{}, cmd, []string{"now"})
			if err != exitCode(2) {
				t.Errorf("err = %v, want exit 2", err)
			}
			if r := got(); len(r) > 0 {
				t.Errorf("the agent got %q", r)
			}
		})
	}
}

// `config init --help` writes no file named --help.
func TestConfigInitHelpWritesNothing(t *testing.T) {
	t.Chdir(t.TempDir())
	t.Setenv("MEGAVOICE_CONFIG", filepath.Join(t.TempDir(), "config.toml"))
	if err := dispatch(app.LoadOpts{}, "config", []string{"init", "--help"}); err != nil {
		t.Fatal(err)
	}
	if ents, _ := os.ReadDir("."); len(ents) > 0 {
		t.Errorf("wrote %v", ents[0].Name())
	}
	if _, err := os.Stat(os.Getenv("MEGAVOICE_CONFIG")); err == nil {
		t.Error("wrote the config")
	}
}
