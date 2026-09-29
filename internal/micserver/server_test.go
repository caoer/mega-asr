package micserver

import (
	"context"
	"crypto/tls"
	"encoding/binary"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"net/http/httputil"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/coder/websocket"

	"github.com/caoer/mega-asr/internal/audio"
)

// fakeSource sends chunks, then holds the stream open until Stop. A context
// cancelled before Stop ends it as exec.CommandContext ends XVF's shell:
// the stream never closes, since arecord outlives the shell and holds the
// pipe.
type fakeSource struct {
	chunks  [][]int16
	err     error // Err after the stream ends
	started chan struct{}
	stop    chan struct{}
	once    sync.Once
	killed  atomic.Bool
}

func newFake(chunks ...[]int16) *fakeSource {
	return &fakeSource{chunks: chunks, started: make(chan struct{}), stop: make(chan struct{})}
}

func (f *fakeSource) Start(ctx context.Context) (<-chan []int16, error) {
	ch := make(chan []int16, len(f.chunks))
	for _, c := range f.chunks {
		ch <- c
	}
	close(f.started)
	go func() {
		select {
		case <-f.stop:
			close(ch)
		case <-ctx.Done():
			f.killed.Store(true)
		}
	}()
	return ch, nil
}

func (f *fakeSource) Stop()      { f.once.Do(func() { close(f.stop) }) }
func (f *fakeSource) Err() error { return f.err }

type logBuf struct {
	mu    sync.Mutex
	lines []string
}

func (l *logBuf) logf(format string, a ...any) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.lines = append(l.lines, fmt.Sprintf(format, a...))
}

func (l *logBuf) has(sub ...string) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
next:
	for _, line := range l.lines {
		for _, s := range sub {
			if !strings.Contains(line, s) {
				continue next
			}
		}
		return true
	}
	return false
}

type box struct {
	*Server
	addr string
	log  *logBuf
}

func newBox(t *testing.T, src func() audio.Source) box {
	t.Helper()
	lb := &logBuf{}
	s, err := New(Config{State: filepath.Join(t.TempDir(), "mic"), Name: "micbox", Source: src, Logf: lb.logf})
	if err != nil {
		t.Fatal(err)
	}
	ts := httptest.NewUnstartedServer(s.Handler())
	ts.TLS = s.TLSConfig()
	ts.StartTLS()
	t.Cleanup(ts.Close)
	t.Cleanup(func() { // before the state dir goes: a stream's end writes last_seen
		for range 500 {
			s.mu.Lock()
			n := len(s.live)
			s.mu.Unlock()
			if n == 0 {
				return
			}
			time.Sleep(10 * time.Millisecond)
		}
	})
	return box{s, ts.Listener.Addr().String(), lb}
}

func (b box) pin(t *testing.T) string {
	t.Helper()
	p, err := b.NewPIN()
	if err != nil {
		t.Fatal(err)
	}
	return p.Code
}

func pair(addr, pin, name string) (Paired, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	return Pair(ctx, addr, pin, name)
}

func wrong(pin string) string {
	if pin == "00000000" {
		return "00000001"
	}
	return "00000000"
}

// Pairing and the menu's probe name an address that accepts and closes as
// not the mic server, pointing at the LAN address `mic pair` prints.
func TestPairAcceptAndCloseIsNamed(t *testing.T) {
	l, _ := net.Listen("tcp", "127.0.0.1:0")
	defer l.Close()
	go func() {
		for {
			c, err := l.Accept()
			if err != nil {
				return
			}
			c.Close()
		}
	}()
	for what, err := range map[string]error{
		"Probe": Probe(context.Background(), l.Addr().String()),
		"Pair": func() error {
			_, err := Pair(context.Background(), l.Addr().String(), "12345678", "studio")
			return err
		}(),
	} {
		if err == nil || !strings.Contains(err.Error(), "closed the connection before TLS finished") || !strings.Contains(err.Error(), "megavoice mic pair") {
			t.Errorf("%s = %v", what, err)
		}
	}
}

func TestPairPinsTheBoxCertificate(t *testing.T) {
	b := newBox(t, nil)
	p, err := pair(b.addr, b.pin(t), "studio")
	if err != nil {
		t.Fatal(err)
	}
	if p.Box != "micbox" || p.Fingerprint != b.Fingerprint() || len(p.Token) < 40 {
		t.Errorf("paired %+v, box fingerprint %s", p, b.Fingerprint())
	}
	cs := b.Clients()
	if len(cs) != 1 || cs[0].Name != "studio" || cs[0].Paired.IsZero() {
		t.Errorf("clients %+v", cs)
	}
	raw, _ := os.ReadFile(filepath.Join(b.cfg.State, "clients.toml"))
	if strings.Contains(string(raw), p.Token) {
		t.Error("clients.toml holds the token itself, not its hash")
	}
	if !b.log.has("paired studio") {
		t.Errorf("no pairing log: %q", b.log.lines)
	}
}

func TestUsedPINCannotPairTwice(t *testing.T) {
	b := newBox(t, nil)
	pin := b.pin(t)
	if _, err := pair(b.addr, pin, "studio"); err != nil {
		t.Fatal(err)
	}
	if _, err := pair(b.addr, pin, "other"); !errors.Is(err, ErrRefused) {
		t.Errorf("second pairing with the PIN: %v, want ErrRefused", err)
	}
}

func TestThreeWrongPINsKillIt(t *testing.T) {
	b := newBox(t, nil)
	pin := b.pin(t)
	for i := range PINTries {
		if _, err := pair(b.addr, wrong(pin), "studio"); !errors.Is(err, ErrRefused) {
			t.Fatalf("wrong PIN %d: %v", i+1, err)
		}
	}
	if _, err := pair(b.addr, pin, "studio"); !errors.Is(err, ErrRefused) {
		t.Errorf("right PIN after %d wrong ones: %v, want ErrRefused", PINTries, err)
	}
}

func TestWrongThenRightPIN(t *testing.T) {
	b := newBox(t, nil)
	pin := b.pin(t)
	if _, err := pair(b.addr, wrong(pin), "studio"); !errors.Is(err, ErrRefused) {
		t.Fatalf("wrong PIN: %v", err)
	}
	if _, err := pair(b.addr, pin, "studio"); err != nil {
		t.Errorf("right PIN on the second try: %v", err)
	}
}

func TestExpiredPINRefused(t *testing.T) {
	b := newBox(t, nil)
	pin := b.pin(t)
	b.now = func() time.Time { return time.Now().Add(PINLife + time.Second) }
	if _, err := pair(b.addr, pin, "studio"); !errors.Is(err, ErrRefused) {
		t.Errorf("expired PIN: %v, want ErrRefused", err)
	}
}

func TestNewPINReplacesTheLiveOne(t *testing.T) {
	b := newBox(t, nil)
	old := b.pin(t)
	cur := b.pin(t)
	if old == cur {
		t.Skip("the same PIN twice")
	}
	if _, err := pair(b.addr, old, "studio"); !errors.Is(err, ErrRefused) {
		t.Errorf("replaced PIN: %v, want ErrRefused", err)
	}
	if _, err := pair(b.addr, cur, "studio"); err != nil {
		t.Errorf("live PIN: %v", err)
	}
}

func TestDuplicateNameRefused(t *testing.T) {
	b := newBox(t, nil)
	if _, err := pair(b.addr, b.pin(t), "studio"); err != nil {
		t.Fatal(err)
	}
	pin := b.pin(t)
	if _, err := pair(b.addr, pin, "studio"); !errors.Is(err, ErrNameTaken) {
		t.Errorf("duplicate name: %v, want ErrNameTaken", err)
	}
	if _, err := pair(b.addr, pin, "other"); err != nil {
		t.Errorf("a refused name used up the PIN: %v", err)
	}
}

// A relay in the path terminates TLS with its own certificate and forwards
// both pairing requests to the box. Each side binds the certificate it sees,
// so the keys differ: the box refuses the Mac's confirmation, the attempt is
// counted, and the PIN keeps its other tries for the real box.
func TestRelayWithItsOwnCertificateCannotPair(t *testing.T) {
	b := newBox(t, nil)
	pin := b.pin(t)
	target, _ := url.Parse("https://" + b.addr)
	rp := httputil.NewSingleHostReverseProxy(target)
	rp.Transport = &http.Transport{TLSClientConfig: &tls.Config{InsecureSkipVerify: true}}
	relay := httptest.NewTLSServer(rp)
	defer relay.Close()

	if _, err := pair(relay.Listener.Addr().String(), pin, "studio"); !errors.Is(err, ErrRefused) {
		t.Fatalf("pairing through the relay: %v, want ErrRefused", err)
	}
	if n := len(b.Clients()); n != 0 {
		t.Fatalf("the relay paired: %d clients", n)
	}
	b.mu.Lock()
	tries := b.pinw.tries
	b.mu.Unlock()
	if tries != 1 {
		t.Errorf("tries after the relay = %d, want 1", tries)
	}
	if _, err := pair(b.addr, pin, "studio"); err != nil {
		t.Errorf("the real box after the relay: %v", err)
	}
}

func dial(t *testing.T, addr, token string) (*websocket.Conn, *http.Response, error) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	return websocket.Dial(ctx, "wss://"+addr+"/stream", &websocket.DialOptions{
		HTTPClient: &http.Client{Transport: &http.Transport{TLSClientConfig: &tls.Config{InsecureSkipVerify: true}}},
		HTTPHeader: http.Header{"Authorization": {"Bearer " + token}},
	})
}

func TestBadTokenIs401(t *testing.T) {
	b := newBox(t, func() audio.Source { return newFake() })
	if _, err := pair(b.addr, b.pin(t), "studio"); err != nil {
		t.Fatal(err)
	}
	_, resp, err := dial(t, b.addr, strings.Repeat("A", 43))
	if err == nil || resp == nil || resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("bad token: %v %v, want 401", resp, err)
	}
}

func TestFramesInOrder(t *testing.T) {
	var all []int16
	var chunks [][]int16
	for i, n := range []int{100, 777, 33, 1000, 290, 1} {
		c := make([]int16, n)
		for j := range c {
			c[j] = int16(len(all) + j - 1000*i)
		}
		all = append(all, c...)
		chunks = append(chunks, c)
	}
	src := newFake(chunks...)
	b := newBox(t, func() audio.Source { return src })
	p, err := pair(b.addr, b.pin(t), "studio")
	if err != nil {
		t.Fatal(err)
	}
	c, _, err := dial(t, b.addr, p.Token)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	var got []int16
	for len(got) < len(all)/FrameSamples*FrameSamples {
		typ, m, err := c.Read(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if typ != websocket.MessageBinary || len(m) != 2*FrameSamples {
			t.Fatalf("frame %d: type %v, %d bytes", len(got)/FrameSamples, typ, len(m))
		}
		for i := 0; i < len(m); i += 2 {
			got = append(got, int16(binary.LittleEndian.Uint16(m[i:])))
		}
	}
	for i := range got {
		if got[i] != all[i] {
			t.Fatalf("sample %d = %d, want %d", i, got[i], all[i])
		}
	}
	if cs := b.Clients(); len(cs) != 1 || len(cs[0].Streams) != 1 || cs[0].LastSeen.IsZero() {
		t.Errorf("clients while streaming: %+v", cs)
	}
	c.Close(websocket.StatusNormalClosure, "")
	select {
	case <-src.stop:
	case <-time.After(2 * time.Second):
		t.Fatal("closing the stream did not stop the source")
	}
	waitFor(t, func() bool { cs := b.Clients(); return len(cs[0].Streams) == 0 })
	if src.killed.Load() {
		t.Error("the source's context was cancelled before Stop: an arecord would outlive its shell")
	}
	if !b.log.has("studio", "connected") || !b.log.has("studio", "ended") {
		t.Errorf("connect/disconnect not logged: %q", b.log.lines)
	}
}

func TestRevokeEndsTheStreamAndRefusesTheNext(t *testing.T) {
	src := newFake(make([]int16, FrameSamples))
	b := newBox(t, func() audio.Source { return src })
	p, err := pair(b.addr, b.pin(t), "studio")
	if err != nil {
		t.Fatal(err)
	}
	c, _, err := dial(t, b.addr, p.Token)
	if err != nil {
		t.Fatal(err)
	}
	<-src.started
	if err := b.Revoke("studio"); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	for {
		_, _, err := c.Read(ctx)
		if err == nil {
			continue
		}
		if s := websocket.CloseStatus(err); s != websocket.StatusPolicyViolation {
			t.Errorf("revoked stream ended with %v (status %d), want policy violation", err, s)
		}
		break
	}
	if _, resp, err := dial(t, b.addr, p.Token); err == nil || resp == nil || resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("revoked token: %v %v, want 401", resp, err)
	}
	if err := b.Revoke("studio"); !errors.Is(err, ErrUnknownClient) {
		t.Errorf("second revoke: %v", err)
	}
}

// A revoke that lands after the token is checked and before the stream is
// tracked still ends the stream before any audio.
func TestRevokeBeforeTrackClosesTheStream(t *testing.T) {
	var sourced atomic.Bool
	b := newBox(t, func() audio.Source { sourced.Store(true); return newFake(make([]int16, FrameSamples)) })
	p, err := pair(b.addr, b.pin(t), "studio")
	if err != nil {
		t.Fatal(err)
	}
	b.mu.Lock()
	b.beforeTrack = func() {
		if err := b.Revoke("studio"); err != nil {
			t.Error(err)
		}
	}
	b.mu.Unlock()
	c, _, err := dial(t, b.addr, p.Token)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if _, _, err := c.Read(ctx); websocket.CloseStatus(err) != websocket.StatusPolicyViolation {
		t.Errorf("stream revoked before tracking: %v, want policy violation", err)
	}
	if sourced.Load() {
		t.Error("the source started for a revoked client")
	}
	if cs := b.Clients(); len(cs) != 0 {
		t.Errorf("clients %+v", cs)
	}
}

// Anyone on the LAN can spend a try or displace a pending pairing; the box
// logs every attempt with its address, and the displaced confirmation fails
// cleanly without spending another try.
func TestThirdPartyPairBetweenStartAndConfirm(t *testing.T) {
	b := newBox(t, nil)
	pin := b.pin(t)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	hc := &http.Client{Transport: &http.Transport{TLSClientConfig: audio.PinnedTLS(b.Fingerprint())}}
	ad := adFor(b.Fingerprint())

	msgA, mac, _ := startPAKE(pin, "studio", ad)
	var sr startResp
	if err := post(ctx, hc, b.addr, "/pair", startReq{Name: "studio", A: msgA}, &sr); err != nil {
		t.Fatal(err)
	}
	msgX, _, _ := startPAKE(wrong(pin), "intruder", ad)
	var sx startResp
	if err := post(ctx, hc, b.addr, "/pair", startReq{Name: "intruder", A: msgX}, &sx); err != nil {
		t.Fatal(err)
	}
	confirm, _, _ := mac.finish(sr.B)
	var cr confirmResp
	if err := post(ctx, hc, b.addr, "/pair/confirm", confirmReq{ID: sr.ID, Confirm: confirm}, &cr); !errors.Is(err, ErrRefused) {
		t.Fatalf("displaced confirmation: %v, want ErrRefused", err)
	}
	b.mu.Lock()
	tries, live := 0, b.pinw != nil
	if live {
		tries = b.pinw.tries
	}
	b.mu.Unlock()
	if !live || tries != 2 {
		t.Errorf("after two runs: live %v, tries %d, want live with 2", live, tries)
	}
	if !b.log.has("studio from 127.0.0.1", "try 1 of 3") || !b.log.has("intruder from 127.0.0.1", "try 2 of 3", "displaces studio") {
		t.Errorf("attempts not logged with their address: %q", b.log.lines)
	}
	if _, err := pair(b.addr, pin, "studio"); err != nil {
		t.Errorf("the last try: %v", err)
	}
}

func TestStateSurvivesARestart(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "mic")
	s, err := New(Config{State: dir, Name: "micbox"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.addClient("studio"); err != nil {
		t.Fatal(err)
	}
	s2, err := New(Config{State: dir, Name: "micbox"})
	if err != nil {
		t.Fatal(err)
	}
	if s2.Fingerprint() != s.Fingerprint() || len(s2.Clients()) != 1 {
		t.Errorf("after restart: fingerprint %s → %s, clients %+v", s.Fingerprint(), s2.Fingerprint(), s2.Clients())
	}
	for name, want := range map[string]os.FileMode{"": 0o700, "key.pem": 0o600, "clients.toml": 0o600} {
		st, err := os.Stat(filepath.Join(dir, name))
		if err != nil || st.Mode().Perm() != want {
			t.Errorf("%s: %v %v, want %v", name, st.Mode().Perm(), err, want)
		}
	}
}

func waitFor(t *testing.T, ok func() bool) {
	t.Helper()
	for range 200 {
		if ok() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("timed out")
}
