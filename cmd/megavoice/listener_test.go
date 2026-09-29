//go:build darwin

package main

import (
	"bytes"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/caoer/mega-asr/internal/app"
	"github.com/caoer/mega-asr/internal/audio"
	"github.com/caoer/mega-asr/internal/ctl"
	"github.com/caoer/mega-asr/internal/takes"
)

// scratchStore is a store with one take, and the key of a scratch state
// directory, short enough for its control socket's path.
func scratchStore(t *testing.T) (dir, key string) {
	state, err := os.MkdirTemp("", "mv")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(state) })
	t.Setenv("XDG_STATE_HOME", state)
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	t.Setenv("XDG_DATA_HOME", t.TempDir())
	dir = t.TempDir()
	if err := audio.SaveWAV(filepath.Join(dir, "20200314-092653.wav"), make([]int16, audio.Rate)); err != nil {
		t.Fatal(err)
	}
	os.WriteFile(filepath.Join(dir, "20200314-092653.txt"), []byte("今天下午整理花园里的番茄架子。\n"), 0o644)
	key, err = keyFile().Load()
	if err != nil {
		t.Fatal(err)
	}
	return dir, key
}

// holdsGuard checks the listener at addr the way a browser reaches it: the
// pages open without the key, everything else wants it in the header.
func holdsGuard(t *testing.T, addr, key string, paths ...string) {
	t.Helper()
	get := func(path, k string) int {
		r, _ := http.NewRequest("GET", "http://"+addr+path, nil)
		r.Header.Set("Sec-Fetch-Site", "same-origin")
		if k != "" {
			r.Header.Set(takes.Header, k)
		}
		resp, err := http.DefaultClient.Do(r)
		if err != nil {
			t.Fatal(err)
		}
		io.Copy(io.Discard, resp.Body)
		resp.Body.Close()
		if len(resp.Header.Values("Set-Cookie")) != 0 || resp.Header.Get("X-Content-Type-Options") != "nosniff" {
			t.Errorf("GET %s: Set-Cookie %q, X-Content-Type-Options %q", path, resp.Header.Values("Set-Cookie"), resp.Header.Get("X-Content-Type-Options"))
		}
		return resp.StatusCode
	}
	if c := get("/", ""); c != http.StatusOK {
		t.Errorf("GET / without the key: %d, want 200", c)
	}
	for _, p := range paths {
		if c := get(p, ""); c != http.StatusForbidden {
			t.Errorf("GET %s without the key: %d, want 403", p, c)
		}
		if c := get(p, strings.Repeat("0", 32)); c != http.StatusForbidden {
			t.Errorf("GET %s with a wrong key: %d, want 403", p, c)
		}
		if c := get(p, key); c != http.StatusOK {
			t.Errorf("GET %s with the key: %d, want 200", p, c)
		}
	}
}

// The agent's listener, as servePanel wires it.
func TestServePanelGuarded(t *testing.T) {
	dir, before := scratchStore(t)
	c := app.Default()
	c.Store.Data, c.Store.Labels, c.Compare.Addr = dir, filepath.Join(t.TempDir(), "labels.jsonl"), "127.0.0.1:0"
	s := &server{cfg: c, opts: app.LoadOpts{Path: filepath.Join(t.TempDir(), "config.toml")}}
	ln := s.servePanel()
	if ln == nil {
		t.Fatal("servePanel: no listener")
	}
	defer closePanel(t, s, ln)
	addr := ln.Addr().String()
	// the key is rotated at the start: the one from before opens nothing
	if c := code(t, addr, "/takes/api/takes", before); c != http.StatusForbidden {
		t.Errorf("the key from before the start: %d, want 403", c)
	}
	key, err := keyFile().Load()
	if err != nil || key == before {
		t.Fatalf("the key after the start: %v, rotated %v", err, key != before)
	}
	holdsGuard(t, addr, key,
		"/takes/api/takes", "/takes/api/take/20200314-092653", "/takes/audio/20200314-092653.wav", "/takes/api/engines")

	// the key is read at every request: a rotation refuses the old one at once
	rotated, err := keyFile().Rotate()
	if err != nil {
		t.Fatal(err)
	}
	holdsGuard(t, addr, rotated, "/takes/api/takes")
	if c := code(t, addr, "/takes/api/takes", key); c != http.StatusForbidden {
		t.Errorf("the key before the rotation: %d, want 403", c)
	}

	// the menu and `megavoice takes url` hand out the listener's address with
	// the key
	page, note := menuRows(s)
	if page != takes.URL(addr, "/takes/", rotated) || note != "" {
		t.Errorf("menu: %q %q, want the page's address with the key", page, note)
	}
	stop := answerListener(t, s)
	defer stop()
	var out bytes.Buffer
	if err := takesCmd([]string{"url"}, &out); err != nil || out.String() != takes.URL(addr, "/takes/", rotated)+"\n" {
		t.Errorf("takes url: %q %v, want the address with the key", out.String(), err)
	}
}

// closePanel closes the listener servePanel returned and waits for the
// rotation that follows, so nothing touches the key file after the test.
func closePanel(t *testing.T, s *server, ln net.Listener) {
	t.Helper()
	key, _ := keyFile().Load()
	ln.Close()
	for deadline := time.Now().Add(5 * time.Second); ; time.Sleep(10 * time.Millisecond) {
		if now, _ := keyFile().Load(); now != key {
			if addr, _ := s.listening(); addr == "" {
				return
			}
		}
		if time.Now().After(deadline) {
			t.Fatal("the key was not rotated when the listener closed")
		}
	}
}

// Every start of megavoice serve rotates the key, a clean start included: an
// open tab's key, which another process holding the port meanwhile may have
// read, opens nothing afterwards. A listener that stops rotates it too.
func TestServePanelKeyPerStart(t *testing.T) {
	dir, _ := scratchStore(t)
	c := app.Default()
	c.Store.Data, c.Store.Labels, c.Compare.Addr = dir, filepath.Join(t.TempDir(), "labels.jsonl"), "127.0.0.1:0"
	var keys []string
	for range 2 {
		s := &server{cfg: c, opts: app.LoadOpts{Path: filepath.Join(t.TempDir(), "config.toml")}}
		ln := s.servePanel()
		if ln == nil {
			t.Fatal("servePanel: no listener")
		}
		key, err := keyFile().Load()
		if err != nil {
			t.Fatal(err)
		}
		keys = append(keys, key)
		closePanel(t, s, ln)
		if now, _ := keyFile().Load(); now == key {
			t.Error("the key survived the listener's stop")
		}
		if page, _ := menuRows(s); page != "" {
			t.Errorf("menu after the listener stopped: %q, want no address", page)
		}
	}
	if keys[0] == keys[1] {
		t.Error("two starts in a row gave the same key")
	}
}

// A host name in compare.addr resolves to more addresses than the one bound:
// the menu and `megavoice takes url` hand out the bound IP literal.
func TestServePanelHandsOutTheBoundAddress(t *testing.T) {
	dir, _ := scratchStore(t)
	c := app.Default()
	c.Store.Data, c.Store.Labels, c.Compare.Addr = dir, filepath.Join(t.TempDir(), "labels.jsonl"), "localhost:0"
	s := &server{cfg: c, opts: app.LoadOpts{Path: filepath.Join(t.TempDir(), "config.toml")}}
	ln := s.servePanel()
	if ln == nil {
		t.Fatal("servePanel: no listener")
	}
	defer closePanel(t, s, ln)
	addr, err := s.listening()
	if err != nil || addr != ln.Addr().String() {
		t.Fatalf("listening: %q %v, want %q", addr, err, ln.Addr())
	}
	if h, _, _ := net.SplitHostPort(addr); net.ParseIP(h) == nil {
		t.Errorf("handed out %q, want an IP literal", addr)
	}
	stop := answerListener(t, s)
	defer stop()
	var out bytes.Buffer
	key, _ := keyFile().Load()
	if err := takesCmd([]string{"url"}, &out); err != nil || out.String() != takes.URL(addr, "/takes/", key)+"\n" {
		t.Errorf("takes url: %q %v, want %s with the key", out.String(), err, addr)
	}
}

// code is the status of GET path on addr from the page, with key.
func code(t *testing.T, addr, path, key string) int {
	t.Helper()
	r, _ := http.NewRequest("GET", "http://"+addr+path, nil)
	r.Header.Set("Sec-Fetch-Site", "same-origin")
	r.Header.Set(takes.Header, key)
	resp, err := http.DefaultClient.Do(r)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	return resp.StatusCode
}

// menuRows are the menu's Takes page row and the note under it, as the
// agent's menu reads them at every open.
func menuRows(s *server) (page, note string) {
	m := s.menu()
	m.readLocal()
	return m.st.Voice.Takes, m.st.Voice.TakesNote
}

// answerListener serves the agent's control socket, where `megavoice takes
// url` asks for the listener.
func answerListener(t *testing.T, s *server) func() {
	l, err := ctl.Listen(sockPath(), s.handle)
	if err != nil {
		t.Fatal(err)
	}
	return func() { l.Close() }
}

// Another process holds compare.addr: servePanel logs why, rotates the key,
// and neither the menu nor `megavoice takes url` hands the key out.
func TestServePanelBusy(t *testing.T) {
	dir, key := scratchStore(t)
	squatter, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer squatter.Close()
	addr := squatter.Addr().String()
	c := app.Default()
	c.Store.Data, c.Store.Labels, c.Compare.Addr = dir, filepath.Join(t.TempDir(), "labels.jsonl"), addr
	s := &server{cfg: c, opts: app.LoadOpts{Path: filepath.Join(t.TempDir(), "config.toml")}}
	var logged bytes.Buffer
	log.SetOutput(&logged)
	defer log.SetOutput(os.Stderr)
	if ln := s.servePanel(); ln != nil {
		ln.Close()
		t.Fatal("servePanel listened on a held address")
	}
	if l := logged.String(); !strings.Contains(l, addr) || !strings.Contains(l, "hand out no key") || !strings.Contains(l, "the key is rotated") {
		t.Errorf("the log says %q, want the address, that no key is handed out, and the rotation", l)
	}
	rotated, err := keyFile().Load()
	if err != nil || rotated == key {
		t.Errorf("the key after a failed bind: %v, rotated %v", err, rotated != key)
	}
	page, note := menuRows(s)
	if page != "" || !strings.Contains(note, "占用") {
		t.Errorf("menu: %q %q, want no address and why", page, note)
	}
	stop := answerListener(t, s)
	defer stop()
	var out bytes.Buffer
	err = takesCmd([]string{"url"}, &out)
	if out.Len() != 0 || err == nil || !strings.Contains(err.Error(), addr) || !strings.Contains(err.Error(), "address already in use") {
		t.Errorf("takes url: printed %q, %v; want nothing printed and why", out.String(), err)
	}
}

// A listener that cannot be had for another reason than a busy port, or a
// key that cannot be rotated: no address is handed out, and the menu's note
// gives the error.
func TestServePanelOtherFailures(t *testing.T) {
	dir, _ := scratchStore(t)
	c := app.Default()
	c.Store.Data, c.Store.Labels = dir, filepath.Join(t.TempDir(), "labels.jsonl")
	log.SetOutput(io.Discard)
	defer log.SetOutput(os.Stderr)
	stuck := func() {
		// a directory in the key file's place: the rename of a new key fails
		os.Remove(string(keyFile()))
		os.MkdirAll(filepath.Join(string(keyFile()), "held"), 0o700)
	}
	for _, tc := range []struct {
		name, addr, want string
		setup            func()
	}{
		{"an address not on this machine", "192.0.2.50:0", "can't assign requested address", nil},
		{"a key not rotated", "127.0.0.1:0", "rotating the key", stuck},
	} {
		if tc.setup != nil {
			tc.setup()
		}
		c.Compare.Addr = tc.addr
		s := &server{cfg: c, opts: app.LoadOpts{Path: filepath.Join(t.TempDir(), "config.toml")}}
		if ln := s.servePanel(); ln != nil {
			ln.Close()
			if tc.setup == nil {
				t.Logf("%s: %s binds on this machine", tc.name, tc.addr)
				continue
			}
			t.Fatalf("%s: servePanel listened", tc.name)
		}
		page, note := menuRows(s)
		if page != "" || !strings.Contains(note, "打不开") || !strings.Contains(note, tc.want) {
			t.Errorf("%s: menu %q %q, want no address and the error", tc.name, page, note)
		}
		stop := answerListener(t, s)
		var out bytes.Buffer
		err := takesCmd([]string{"url"}, &out)
		stop()
		if out.Len() != 0 || err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: takes url printed %q, %v; want nothing printed and why", tc.name, out.String(), err)
		}
	}
}

// `megavoice panel` on a held address prints no address and says why.
func TestPanelCommandBusy(t *testing.T) {
	dir, _ := scratchStore(t)
	cfg := filepath.Join(t.TempDir(), "config.toml")
	os.WriteFile(cfg, nil, 0o600)
	squatter, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer squatter.Close()
	r, w, _ := os.Pipe()
	stdout := os.Stdout
	os.Stdout = w
	err = panel(app.LoadOpts{Path: cfg}, []string{"-data", dir, "-labels", filepath.Join(t.TempDir(), "labels.jsonl"), "-addr", squatter.Addr().String()})
	os.Stdout = stdout
	w.Close()
	printed, _ := io.ReadAll(r)
	if len(printed) != 0 || err == nil || !strings.Contains(err.Error(), "address already in use") {
		t.Errorf("megavoice panel on a held address: printed %q, %v; want nothing printed and why", printed, err)
	}
}

// With no megavoice serve running, `megavoice takes url` hands out no key,
// --rotate included, and says why.
func TestTakesURLWithoutServe(t *testing.T) {
	_, key := scratchStore(t)
	for _, args := range [][]string{{"url"}, {"url", "--rotate"}} {
		var out bytes.Buffer
		err := takesCmd(args, &out)
		if out.Len() != 0 || err == nil || !strings.Contains(err.Error(), "not running") {
			t.Errorf("takes %q: printed %q, %v; want nothing printed and why", args, out.String(), err)
		}
	}
	if now, _ := keyFile().Load(); now == key {
		t.Error("takes url --rotate kept the key")
	}
}

// `megavoice panel`, as the command wires it: the Takes page over the
// directory, behind the guard.
func TestPanelCommandGuarded(t *testing.T) {
	dir, key := scratchStore(t)
	cfg := filepath.Join(t.TempDir(), "config.toml")
	os.WriteFile(cfg, nil, 0o600)
	free, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := free.Addr().String()
	free.Close()
	go panel(app.LoadOpts{Path: cfg}, []string{"-data", dir, "-labels", filepath.Join(t.TempDir(), "labels.jsonl"), "-addr", addr})
	for deadline := time.Now().Add(10 * time.Second); ; time.Sleep(50 * time.Millisecond) {
		if c, err := net.Dial("tcp", addr); err == nil {
			c.Close()
			break
		} else if time.Now().After(deadline) {
			t.Fatalf("megavoice panel did not listen on %s: %v", addr, err)
		}
	}
	holdsGuard(t, addr, key, "/takes/api/takes", "/takes/audio/20200314-092653.wav")
}
