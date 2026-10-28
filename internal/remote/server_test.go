package remote

import (
	"context"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/coder/websocket"
)

// fakeTakes is a Mac with one take at most.
type fakeTakes struct {
	mu    sync.Mutex
	rec   string
	owner string // State's owner while rec records
	n     int
	calls []string
	slow  time.Duration // Start takes this long, set before the server runs commands
}

func (f *fakeTakes) Start(via string) (string, error) {
	time.Sleep(f.slow)
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, "start "+via)
	if f.rec != "" {
		return "", Refuse("recording", "还在录音")
	}
	f.n++
	f.rec = fmt.Sprintf("take%d", f.n)
	return f.rec, nil
}

func (f *fakeTakes) Stop(via, take string, send bool) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, fmt.Sprintf("stop %s %s send=%v", via, take, send))
	return f.endLocked(take)
}

func (f *fakeTakes) Cancel(via, take string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, "cancel "+via+" "+take)
	return f.endLocked(take)
}

func (f *fakeTakes) endLocked(take string) error {
	switch f.rec {
	case "":
		return Refuse("not_recording", "没有在录音")
	case take:
		f.rec = ""
		return nil
	}
	return Refuse("other_take", "在录另一段")
}

func (f *fakeTakes) State() State {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.rec != "" {
		return State{Phase: "recording", Take: f.rec, Owner: f.owner}
	}
	return State{Phase: "idle"}
}

func (f *fakeTakes) log() string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return strings.Join(f.calls, "; ")
}

// up starts a server on loopback and returns it once it listens.
func up(t *testing.T) (*Server, *fakeTakes) {
	t.Helper()
	f := &fakeTakes{}
	return upWith(t, f), f
}

func upWith(t *testing.T, f Takes) *Server {
	t.Helper()
	s, err := New(Config{Listen: "127.0.0.1:0", State: t.TempDir(), Name: "testmac", Takes: f, Logf: t.Logf})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { s.Serve(ctx); close(done) }()
	t.Cleanup(func() {
		s.mu.Lock()
		var sockets []*websocket.Conn
		for c := range s.conns {
			if w, ok := c.sess.(*wsSession); ok {
				sockets = append(sockets, w.ws)
			}
		}
		s.mu.Unlock()
		for _, c := range sockets {
			c.CloseNow()
		}
		cancel()
		<-done
		waitFor(t, "closed direct connections", func() bool { s.mu.Lock(); defer s.mu.Unlock(); return len(s.conns) == 0 })
	})
	for range 100 {
		s.mu.Lock()
		addr := s.addr
		s.mu.Unlock()
		if addr != "" {
			return s
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("server did not listen")
	return nil
}

// dial connects as a phone that pins fp.
func dial(t *testing.T, s *Server, fp string) (*websocket.Conn, error) {
	t.Helper()
	s.mu.Lock()
	addr := s.addr
	s.mu.Unlock()
	tc := &tls.Config{
		MinVersion:         tls.VersionTLS13,
		InsecureSkipVerify: true, // pinned below, as the phone does
		VerifyPeerCertificate: func(raw [][]byte, _ [][]*x509.Certificate) error {
			h := sha256.Sum256(raw[0])
			if hex.EncodeToString(h[:]) != fp {
				return errors.New("pin mismatch")
			}
			return nil
		},
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	c, _, err := websocket.Dial(ctx, "wss://"+addr+"/ctl", &websocket.DialOptions{HTTPClient: httpClient(tc)})
	return c, err
}

func send(t *testing.T, c *websocket.Conn, v any) {
	t.Helper()
	b, _ := json.Marshal(v)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if err := c.Write(ctx, websocket.MessageText, b); err != nil {
		t.Fatal(err)
	}
}

// recv reads the next message of type typ, skipping state and take pushes
// unless asked for.
func recv(t *testing.T, c *websocket.Conn, typ string) map[string]any {
	t.Helper()
	for {
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		_, b, err := c.Read(ctx)
		cancel()
		if err != nil {
			t.Fatalf("waiting for %s: %v", typ, err)
		}
		var m map[string]any
		if err := json.Unmarshal(b, &m); err != nil {
			t.Fatal(err)
		}
		if m["type"] == typ {
			return m
		}
	}
}

func cmd(id, action, end string, sentAt time.Time) map[string]any {
	return map[string]any{"type": "cmd", "id": id, "action": action, "end": end, "via": "iphone", "sent_at": sentAt.UnixMilli()}
}

// end is a stop (or cancel, end "") of take.
func end(id, action, end, take string) map[string]any {
	m := cmd(id, action, end, time.Now())
	m["take"] = take
	return m
}

// secretOf reads the secret out of a pairing URL, as the phone does.
func secretOf(t *testing.T, p Pairing) url.Values {
	t.Helper()
	u, err := url.Parse(p.URL)
	if err != nil || u.Scheme != "megavoice" || u.Host != "pair" {
		t.Fatalf("pairing URL %q: %v", p.URL, err)
	}
	return u.Query()
}

// The whole path: pair with the QR's secret, pinned; the secret is spent;
// hello with the token; start, a repeated id, a stale start, stop to send;
// revoke closes the phone and its token is refused.
func TestPairHelloCommandsRevoke(t *testing.T) {
	s, f := up(t)
	p, err := s.NewPairing()
	if err != nil {
		t.Fatal(err)
	}
	q := secretOf(t, p)
	if q.Get("v") != "1" || q.Get("fp") != s.Fingerprint() || q.Get("name") != "testmac" || len(q.Get("s")) != 22 || q.Get("port") == "" {
		t.Fatalf("pairing URL %s", p.URL)
	}
	if p.QR.Size == 0 || len(p.QR.Bits) != p.QR.Size*p.QR.Size {
		t.Fatalf("QR %d modules for size %d", len(p.QR.Bits), p.QR.Size)
	}

	// a phone pinning another certificate never gets to send the secret
	if _, err := dial(t, s, strings.Repeat("0", 64)); err == nil {
		t.Fatal("a connection with the wrong pin succeeded")
	}

	c, err := dial(t, s, q.Get("fp"))
	if err != nil {
		t.Fatal(err)
	}
	send(t, c, map[string]any{"type": "pair", "secret": q.Get("s"), "device": "iPhone 3fa2"})
	paired := recv(t, c, "paired")
	token, _ := paired["token"].(string)
	if paired["mac"] != "testmac" || len(token) != 43 {
		t.Fatalf("paired %v", paired)
	}
	if st := recv(t, c, "state"); st["state"].(map[string]any)["phase"] != "idle" {
		t.Fatalf("state after pairing %v", st)
	}
	c.Close(websocket.StatusNormalClosure, "")

	// the secret pairs one phone only
	c2, err := dial(t, s, q.Get("fp"))
	if err != nil {
		t.Fatal(err)
	}
	send(t, c2, map[string]any{"type": "pair", "secret": q.Get("s"), "device": "intruder"})
	if r := recv(t, c2, "refused"); r["code"] != "pair" {
		t.Fatalf("a spent secret got %v", r)
	}

	c3, err := dial(t, s, q.Get("fp"))
	if err != nil {
		t.Fatal(err)
	}
	send(t, c3, map[string]any{"type": "hello", "token": token, "device": "iPhone 3fa2"})
	if h := recv(t, c3, "hello"); h["mac"] != "testmac" {
		t.Fatalf("hello %v", h)
	}
	now := time.Now()
	send(t, c3, cmd("a1", "start", "", now))
	if r := recv(t, c3, "reply"); r["id"] != "a1" || r["ok"] != true || r["state"].(map[string]any)["take"] != "take1" {
		t.Fatalf("start reply %v", r)
	}
	send(t, c3, cmd("a1", "start", "", now)) // a retry of the same press
	if r := recv(t, c3, "reply"); r["ok"] != true {
		t.Fatalf("repeated start reply %v", r)
	}
	send(t, c3, cmd("a2", "start", "", now)) // another press while recording
	if r := recv(t, c3, "reply"); r["ok"] != false || r["code"] != "recording" {
		t.Fatalf("second start reply %v", r)
	}
	send(t, c3, end("a3", "stop", "send", "take1"))
	if r := recv(t, c3, "reply"); r["ok"] != true || r["state"].(map[string]any)["phase"] != "idle" {
		t.Fatalf("stop reply %v", r)
	}
	send(t, c3, cmd("a4", "start", "", now.Add(-9*time.Second)))
	if r := recv(t, c3, "reply"); r["ok"] != false || r["code"] != "stale" {
		t.Fatalf("stale start reply %v", r)
	}
	send(t, c3, end("a5", "stop", "", "take1"))
	if r := recv(t, c3, "reply"); r["ok"] != false || r["code"] != "not_recording" {
		t.Fatalf("stop with nothing recording %v", r)
	}
	if got, want := f.log(), "start iphone; start iphone; stop iphone take1 send=true; stop iphone take1 send=false"; got != want {
		t.Fatalf("calls %q, want %q", got, want)
	}

	s.Done("take1", "sent", "")
	if m := recv(t, c3, "take"); m["take"] != "take1" || m["final"] != "sent" {
		t.Fatalf("take push %v", m)
	}
	s.Changed()
	if m := recv(t, c3, "state"); m["state"].(map[string]any)["phase"] != "idle" {
		t.Fatalf("state push %v", m)
	}

	if ph := s.Phones(); len(ph) != 1 || ph[0].Name != "iPhone 3fa2" || ph[0].Online != 1 {
		t.Fatalf("phones %+v", ph)
	}
	if err := s.Revoke("iPhone 3fa2"); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	for {
		if _, _, err := c3.Read(ctx); err != nil {
			if websocket.CloseStatus(err) != websocket.StatusPolicyViolation {
				t.Fatalf("revoke closed the phone with %v", err)
			}
			break
		}
	}
	c4, err := dial(t, s, q.Get("fp"))
	if err != nil {
		t.Fatal(err)
	}
	send(t, c4, map[string]any{"type": "hello", "token": token, "device": "iPhone 3fa2"})
	if r := recv(t, c4, "refused"); r["code"] != "token" {
		t.Fatalf("a revoked token got %v", r)
	}
}

// Without a pairing or a token nothing is driven: a command first, a wrong
// secret, a made-up token.
func TestNothingWithoutToken(t *testing.T) {
	s, f := up(t)
	p, _ := s.NewPairing()
	fp := secretOf(t, p).Get("fp")
	for _, first := range []map[string]any{
		cmd("x", "start", "", time.Now()),
		{"type": "pair", "secret": "AAAAAAAAAAAAAAAAAAAAAA", "device": "x"},
		{"type": "hello", "token": strings.Repeat("A", 43), "device": "x"},
	} {
		c, err := dial(t, s, fp)
		if err != nil {
			t.Fatal(err)
		}
		send(t, c, first)
		recv(t, c, "refused")
		send(t, c, cmd("y", "start", "", time.Now()))
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		for {
			if _, _, err := c.Read(ctx); err != nil {
				break
			}
		}
		cancel()
	}
	if got := f.log(); got != "" {
		t.Fatalf("an unpaired connection drove the Mac: %s", got)
	}
}

// A pairing window ends after PairLife.
func TestPairingExpires(t *testing.T) {
	s, _ := up(t)
	if _, err := s.NewPairing(); err != nil {
		t.Fatal(err)
	}
	s.mu.Lock()
	s.now = func() time.Time { return time.Now().Add(PairLife + time.Second) }
	w := s.pairWindowLocked()
	s.mu.Unlock()
	if w != nil {
		t.Fatal("pairing window live after PairLife")
	}
}

// phones.toml keeps the token's hash only, 0600, and a restart keeps the
// pairing.
func TestPhonesFile(t *testing.T) {
	dir := t.TempDir() + "/state/remote" // created on first run, 0700
	s, err := New(Config{State: dir, Name: "m", Takes: &fakeTakes{}})
	if err != nil {
		t.Fatal(err)
	}
	s.mu.Lock()
	token, err := s.addPhone("iPhone 1")
	s.mu.Unlock()
	if err != nil {
		t.Fatal(err)
	}
	if st, err := os.Stat(dir); err != nil || st.Mode().Perm() != 0o700 {
		t.Fatalf("state dir: %v %v", st, err)
	}
	b, _ := os.ReadFile(dir + "/phones.toml")
	st, _ := os.Stat(dir + "/phones.toml")
	if strings.Contains(string(b), token) || !strings.Contains(string(b), tokenHash(token)) || st.Mode().Perm() != 0o600 {
		t.Fatalf("phones.toml %o:\n%s", st.Mode().Perm(), b)
	}
	s2, err := New(Config{State: dir, Name: "m", Takes: &fakeTakes{}})
	if err != nil {
		t.Fatal(err)
	}
	if p := s2.byToken(token); p == nil || p.Name != "iPhone 1" || s2.Fingerprint() != s.Fingerprint() {
		t.Fatal("a restart lost the pairing or the certificate")
	}
}

func TestDeviceName(t *testing.T) {
	for in, ok := range map[string]bool{"iPhone 3fa2": true, "我的 iPhone": true, "": false, " ": false, "a\nb": false, strings.Repeat("x", 65): false} {
		if _, err := deviceName(in); (err == nil) != ok {
			t.Errorf("deviceName(%q) = %v", in, err)
		}
	}
}

func httpClient(tc *tls.Config) *http.Client {
	return &http.Client{Transport: &http.Transport{TLSClientConfig: tc, ForceAttemptHTTP2: false}}
}

// The pairing page's API: off says why; on, a POST pair shows a QR code and
// a revoke of an unknown phone is a 404.
func TestPageHandler(t *testing.T) {
	get := func(h http.Handler, method, path, body string) (int, map[string]any) {
		r := httptest.NewRequest(method, path, strings.NewReader(body))
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		var m map[string]any
		json.Unmarshal(w.Body.Bytes(), &m)
		return w.Code, m
	}
	if code, m := get(PageHandler(nil, "off"), "GET", "/takes/api/remote", ""); code != 200 || m["enabled"] != false || m["why"] != "off" {
		t.Fatalf("off: %d %v", code, m)
	}
	s, _ := up(t)
	h := PageHandler(s, "")
	if _, m := get(h, "GET", "/takes/api/remote", ""); m["enabled"] != true || m["pairing"] != nil {
		t.Fatalf("on: %v", m)
	}
	if code, m := get(h, "POST", "/takes/api/remote/pair", "{}"); code != 200 || m["pairing"] == nil {
		t.Fatalf("pair: %d %v", code, m)
	}
	if code, _ := get(h, "POST", "/takes/api/remote/revoke", `{"name":"nobody"}`); code != 404 {
		t.Fatalf("revoke unknown: %d", code)
	}
}

// paired pairs a phone and returns its connection, ready for commands.
func paired(t *testing.T, s *Server) *websocket.Conn {
	t.Helper()
	p, err := s.NewPairing()
	if err != nil {
		t.Fatal(err)
	}
	q := secretOf(t, p)
	c, err := dial(t, s, q.Get("fp"))
	if err != nil {
		t.Fatal(err)
	}
	send(t, c, map[string]any{"type": "pair", "secret": q.Get("s"), "device": "iPhone 3fa2"})
	recv(t, c, "paired")
	return c
}

// closeOf reads c until it closes and returns the close status.
func closeOf(t *testing.T, c *websocket.Conn) websocket.StatusCode {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	for {
		if _, _, err := c.Read(ctx); err != nil {
			return websocket.CloseStatus(err)
		}
	}
}

func TestKeyboardCommands(t *testing.T) {
	for _, tc := range []struct {
		name, action, end, call string
	}{
		{"paste", "stop", "paste", "stop keyboard take1 send=false"},
		{"send", "stop", "send", "stop keyboard take1 send=true"},
		{"cancel", "cancel", "", "cancel keyboard take1"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s, f := up(t)
			c := paired(t, s)
			start := cmd("k1", "start", "", time.Now())
			start["via"] = "keyboard"
			send(t, c, start)
			if r := recv(t, c, "reply"); r["ok"] != true || r["state"].(map[string]any)["take"] != "take1" {
				t.Fatalf("keyboard start %v", r)
			}
			stop := end("k2", tc.action, tc.end, "take1")
			stop["via"] = "keyboard"
			send(t, c, stop)
			if r := recv(t, c, "reply"); r["ok"] != true || r["state"].(map[string]any)["phase"] != "idle" {
				t.Fatalf("keyboard %s %v", tc.name, r)
			}
			if got, want := f.log(), "start keyboard; "+tc.call; got != want {
				t.Fatalf("calls %q, want %q", got, want)
			}
		})
	}
}

func TestUnknownViaRefused(t *testing.T) {
	s, f := up(t)
	c := paired(t, s)
	for _, action := range []string{"start", "stop", "cancel"} {
		m := end("unknown-"+action, action, "", "take1")
		m["via"] = "unknown"
		send(t, c, m)
		if r := recv(t, c, "reply"); r["ok"] != false || r["code"] != "bad_request" || r["reason"] != "via 只能是 iphone、watch 或 keyboard" {
			t.Fatalf("unknown via on %s %v", action, r)
		}
	}
	if got := f.log(); got != "" {
		t.Fatalf("unknown via drove takes: %q", got)
	}
}

// A stop or cancel ends only the take it names: the phone's stop for take1,
// sent again after the Mac stopped take1 and started take2 at its keyboard,
// leaves take2 recording.
func TestStopNamesTake(t *testing.T) {
	s, f := up(t)
	c := paired(t, s)
	send(t, c, cmd("s1", "start", "", time.Now()))
	if r := recv(t, c, "reply"); r["ok"] != true {
		t.Fatalf("start %v", r)
	}
	f.mu.Lock()
	f.rec = "take2" // stopped and started again at the Mac
	f.mu.Unlock()
	for _, m := range []map[string]any{end("x1", "stop", "send", "take1"), end("x2", "cancel", "", "take1")} {
		send(t, c, m)
		if r := recv(t, c, "reply"); r["ok"] != false || r["code"] != "other_take" || r["state"].(map[string]any)["take"] != "take2" {
			t.Fatalf("%s of take1 while take2 records: %v", m["action"], r)
		}
	}
	send(t, c, cmd("x3", "stop", "", time.Now())) // no take named
	if r := recv(t, c, "reply"); r["ok"] != false || r["code"] != "bad_request" {
		t.Fatalf("stop naming no take %v", r)
	}
	send(t, c, end("x4", "stop", "", "take2"))
	if r := recv(t, c, "reply"); r["ok"] != true || r["state"].(map[string]any)["phase"] != "idle" {
		t.Fatalf("stop of take2 %v", r)
	}
	if got, want := f.log(), "start iphone; stop iphone take1 send=true; cancel iphone take1; stop iphone take2 send=false"; got != want {
		t.Fatalf("calls %q, want %q", got, want)
	}
}

// A full Mac refuses a connection "busy" and closes with 1013, not 1008, so
// the phone connects again; once a slot frees, its hello is answered. A
// token refusal alone closes with 1008.
func TestBusyIsRetried(t *testing.T) {
	s, _ := up(t)
	s.mu.Lock()
	token, err := s.addPhone("iPhone 3fa2")
	s.mu.Unlock()
	if err != nil {
		t.Fatal(err)
	}
	fp := s.Fingerprint()
	bad, err := dial(t, s, fp)
	if err != nil {
		t.Fatal(err)
	}
	send(t, bad, map[string]any{"type": "hello", "token": strings.Repeat("A", 43), "device": "x"})
	if r := recv(t, bad, "refused"); r["code"] != "token" {
		t.Fatalf("unknown token got %v", r)
	}
	if st := closeOf(t, bad); st != websocket.StatusPolicyViolation {
		t.Fatalf("token refusal closed with %d, want 1008", st)
	}
	var held []*websocket.Conn
	for range maxConns {
		h, err := dial(t, s, fp)
		if err != nil {
			t.Fatal(err)
		}
		held = append(held, h)
	}
	for {
		s.mu.Lock()
		n := len(s.conns)
		s.mu.Unlock()
		if n == maxConns {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	b, err := dial(t, s, fp)
	if err != nil {
		t.Fatal(err)
	}
	if r := recv(t, b, "refused"); r["code"] != "busy" {
		t.Fatalf("17th connection got %v", r)
	}
	if st := closeOf(t, b); st != websocket.StatusTryAgainLater {
		t.Fatalf("busy closed with %d, want 1013", st)
	}
	held[0].Close(websocket.StatusNormalClosure, "")
	for {
		s.mu.Lock()
		n := len(s.conns)
		s.mu.Unlock()
		if n < maxConns {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	again, err := dial(t, s, fp)
	if err != nil {
		t.Fatal(err)
	}
	send(t, again, map[string]any{"type": "hello", "token": token, "device": "iPhone 3fa2"})
	recv(t, again, "hello")
}

// A cancel of a keyboard-stream take names its owner: from another phone,
// or from the owning one by any via but keyboard, it is refused other_take
// and reaches no take; the owning phone's keyboard cancel does, on the
// connection that streams it or on a later one of the same phone.
func TestCancelNamesItsOwner(t *testing.T) {
	for _, later := range []bool{false, true} {
		t.Run(fmt.Sprintf("later connection %v", later), func(t *testing.T) { cancelNamesItsOwner(t, later) })
	}
}

func cancelNamesItsOwner(t *testing.T, later bool) {
	f := &streamHost{fakeTakes: &fakeTakes{}, got: make(chan []int16, 1)}
	s := upWith(t, f)
	f.s = s
	p, err := s.NewPairing()
	if err != nil {
		t.Fatal(err)
	}
	one, err := dial(t, s, s.Fingerprint())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { one.CloseNow() })
	send(t, one, map[string]any{"type": "pair", "secret": secretOf(t, p).Get("s"), "device": "one"})
	token := recv(t, one, "paired")["token"].(string)
	recv(t, one, "state")
	two := pairStreamPhone(t, s, "two")
	send(t, one, streamStart("s1"))
	if r := recv(t, one, "reply"); r["ok"] != true || r["take"] != "take1" {
		t.Fatal(r)
	}
	for i, c := range []struct {
		conn *websocket.Conn
		via  string
	}{{two, "keyboard"}, {two, "iphone"}, {two, "watch"}, {one, "iphone"}, {one, "watch"}} {
		m := end(fmt.Sprintf("c%d", i), "cancel", "", "take1")
		m["via"] = c.via
		send(t, c.conn, m)
		if r := recv(t, c.conn, "reply"); r["ok"] != false || r["code"] != "other_take" || r["reason"] != ReasonStreamTake() {
			t.Fatalf("cancel %d via %s: %v", i, c.via, r)
		}
	}
	if got := f.log(); got != "start keyboard" {
		t.Fatalf("a refused cancel reached the take: %q", got)
	}
	own := one
	if later { // the phone lost its connection; the Mac still holds it
		again, err := dial(t, s, s.Fingerprint())
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { again.CloseNow() })
		send(t, again, map[string]any{"type": "hello", "token": token, "device": "one"})
		recv(t, again, "hello")
		own = again
	}
	m := end("own", "cancel", "", "take1")
	m["via"] = "keyboard"
	send(t, own, m)
	if r := recv(t, own, "reply"); r["ok"] != true {
		t.Fatal(r)
	}
	if got := f.log(); got != "start keyboard; cancel keyboard take1" {
		t.Fatalf("calls %q", got)
	}
}
