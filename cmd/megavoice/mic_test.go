//go:build darwin

package main

import (
	"bytes"
	"errors"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/caoer/mega-asr/internal/app"
	"github.com/caoer/mega-asr/internal/micserver"
)

func TestParseAdd(t *testing.T) {
	for _, c := range []struct {
		args       []string
		host, name string
	}{
		{[]string{"micbox"}, "micbox", clientName()},
		{[]string{"micbox:7900", "--name", "studio"}, "micbox:7900", "studio"},
		{[]string{"--name", "studio", "192.0.2.50"}, "192.0.2.50", "studio"},
	} {
		host, name, err := parseAdd(c.args)
		if err != nil || host != c.host || name != c.name {
			t.Errorf("%q: %q %q %v", c.args, host, name, err)
		}
	}
	for _, bad := range [][]string{nil, {"a", "b"}, {"--nope", "a"}} {
		if _, _, err := parseAdd(bad); err == nil {
			t.Errorf("%q parsed", bad)
		}
	}
	for host, want := range map[string]string{"micbox": "micbox:7866", "micbox:7900": "micbox:7900", "192.0.2.50": "192.0.2.50:7866", "[::1]": "[::1]:7866"} {
		if got := withPort(host); got != want {
			t.Errorf("withPort(%q) = %q, want %q", host, got, want)
		}
	}
	if n := clientName(); strings.ContainsAny(n, ". ") || n == "" {
		t.Errorf("clientName = %q", n)
	}
}

// mic add pairs with the PIN typed on stdin (no ssh reaches the box), keeps
// the pairing 0600 and makes it the source; list shows it in use; forget
// drops it and goes back local.
func TestMicAddListForget(t *testing.T) {
	fakeSSH(t, nil, nil)
	dir := filepath.Join(t.TempDir(), "megavoice")
	t.Setenv("XDG_CONFIG_HOME", filepath.Dir(dir))
	t.Setenv("MEGAVOICE_CONFIG", "")
	srv, err := micserver.New(micserver.Config{State: filepath.Join(t.TempDir(), "mic"), Name: "micbox", Logf: t.Logf})
	if err != nil {
		t.Fatal(err)
	}
	ts := httptest.NewUnstartedServer(srv.Handler())
	ts.TLS = srv.TLSConfig()
	ts.StartTLS()
	defer ts.Close()
	addr := ts.Listener.Addr().String()

	pin, _ := srv.NewPIN()
	wrong := "00000000"
	if pin.Code == wrong {
		wrong = "00000001"
	}
	var out bytes.Buffer
	if err := micRemote(app.LoadOpts{}, []string{"add", addr, "--name", "studio"}, strings.NewReader(wrong+"\n"), &out); !errors.Is(err, micserver.ErrRefused) {
		t.Fatalf("wrong PIN: %v", err)
	}
	if _, err := os.Stat(app.RemotesPath()); err == nil {
		t.Fatal("a refused pairing wrote remotes.toml")
	}
	out.Reset()
	if err := micRemote(app.LoadOpts{}, []string{"add", addr, "--name", "studio"}, strings.NewReader(pin.Code+"\n"), &out); err != nil {
		t.Fatalf("mic add: %v\n%s", err, out.String())
	}
	if !strings.Contains(out.String(), "paired with micbox") {
		t.Errorf("output: %s", out.String())
	}
	rs, _ := app.LoadRemotes(app.RemotesPath())
	if len(rs) != 1 || rs[0].Name != "micbox" || rs[0].Addr != addr || rs[0].Fingerprint != srv.Fingerprint() || rs[0].Client != "studio" || rs[0].Token == "" {
		t.Fatalf("remotes.toml: %+v", rs)
	}
	if st, _ := os.Stat(app.RemotesPath()); st.Mode().Perm() != 0o600 {
		t.Errorf("remotes.toml mode %v", st.Mode().Perm())
	}
	if l, err := load(app.LoadOpts{}); err != nil || l.Capture.Source != "remote" || l.Capture.Remote != "micbox" {
		t.Errorf("config after mic add: %+v %v", l.Capture, err)
	}
	if cs := srv.Clients(); len(cs) != 1 || cs[0].Name != "studio" {
		t.Errorf("the box's clients: %+v", cs)
	}

	out.Reset()
	if err := micRemote(app.LoadOpts{}, []string{"list"}, nil, &out); err != nil || !strings.Contains(out.String(), "Y    micbox") {
		t.Errorf("mic list: %v\n%s", err, out.String())
	}

	out.Reset()
	if err := micRemote(app.LoadOpts{}, []string{"forget", "micbox"}, nil, &out); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "local again") || !strings.Contains(out.String(), "mic revoke studio") {
		t.Errorf("mic forget: %s", out.String())
	}
	if rs, _ := app.LoadRemotes(app.RemotesPath()); len(rs) != 0 {
		t.Errorf("remotes after forget: %+v", rs)
	}
	if l, err := load(app.LoadOpts{}); err != nil || l.Capture.Source != "local" {
		t.Errorf("config after forget: %+v %v", l.Capture, err)
	}
}
