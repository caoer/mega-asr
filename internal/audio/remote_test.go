package audio_test

import (
	"context"
	"errors"
	"io"
	"net"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/caoer/mega-asr/internal/audio"
	"github.com/caoer/mega-asr/internal/micserver"
)

// holdSource sends its chunks and holds the stream open until Stop, or
// ends on its own with err once it has sent them when end is set.
type holdSource struct {
	chunks [][]int16
	end    bool
	err    error
	stop   chan struct{}
	once   sync.Once
}

func (h *holdSource) Start(ctx context.Context) (<-chan []int16, error) {
	h.stop = make(chan struct{})
	ch := make(chan []int16, len(h.chunks))
	for _, c := range h.chunks {
		ch <- c
	}
	go func() {
		if !h.end {
			<-h.stop
		}
		close(ch)
	}()
	return ch, nil
}

func (h *holdSource) Stop()      { h.once.Do(func() { close(h.stop) }) }
func (h *holdSource) Err() error { return h.err }

// box is a mic server in-process, paired as studio.
type box struct {
	srv  *micserver.Server
	addr string
	p    micserver.Paired
}

func newBox(t *testing.T, src func() audio.Source) box {
	t.Helper()
	s, err := micserver.New(micserver.Config{State: filepath.Join(t.TempDir(), "mic"), Name: "micbox", Source: src, Logf: t.Logf})
	if err != nil {
		t.Fatal(err)
	}
	ts := httptest.NewUnstartedServer(s.Handler())
	ts.TLS = s.TLSConfig()
	ts.StartTLS()
	t.Cleanup(ts.Close)
	t.Cleanup(func() { idle(s) }) // before the state dir goes: a stream's end writes last_seen
	pin, _ := s.NewPIN()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	p, err := micserver.Pair(ctx, ts.Listener.Addr().String(), pin.Code, "studio")
	if err != nil {
		t.Fatal(err)
	}
	return box{s, ts.Listener.Addr().String(), p}
}

// idle waits, up to 5 s, until the server has no live stream.
func idle(s *micserver.Server) {
	for range 500 {
		live := false
		for _, c := range s.Clients() {
			live = live || len(c.Streams) > 0
		}
		if !live {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func (b box) remote(addr string) *audio.Remote {
	return &audio.Remote{Name: b.p.Box, Addr: addr, Fingerprint: b.p.Fingerprint, Token: b.p.Token}
}

func ramp(n, from int) []int16 {
	s := make([]int16, n)
	for i := range s {
		s[i] = int16(from + i)
	}
	return s
}

// drain reads ch until it closes or n samples have arrived.
func drain(t *testing.T, ch <-chan []int16, n int) []int16 {
	t.Helper()
	var got []int16
	timeout := time.After(5 * time.Second)
	for n < 0 || len(got) < n {
		select {
		case s, ok := <-ch:
			if !ok {
				return got
			}
			got = append(got, s...)
		case <-timeout:
			t.Fatalf("timed out with %d samples", len(got))
		}
	}
	return got
}

func TestRemoteFramesMatch(t *testing.T) {
	src := &holdSource{chunks: [][]int16{ramp(1000, 0), ramp(283, 1000), ramp(1917, 1283)}}
	b := newBox(t, func() audio.Source { return src })
	r := b.remote(b.addr)
	ch, err := r.Start(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	got := drain(t, ch, 3200)
	for i, v := range got {
		if v != int16(i) {
			t.Fatalf("sample %d = %d", i, v)
		}
	}
	r.Stop()
	drain(t, ch, -1)
	if err := r.Err(); err != nil {
		t.Errorf("Err after Stop = %v", err)
	}
}

func TestRemoteRefusesAnotherCertificate(t *testing.T) {
	b := newBox(t, func() audio.Source { return &holdSource{} })
	r := b.remote(b.addr)
	r.Fingerprint = strings.Repeat("0", 64)
	if _, err := r.Start(context.Background()); !errors.Is(err, audio.ErrNotPinned) {
		t.Errorf("Start with another fingerprint: %v, want ErrNotPinned", err)
	}
}

func TestRemoteRevoked(t *testing.T) {
	b := newBox(t, func() audio.Source { return &holdSource{chunks: [][]int16{ramp(320, 0)}} })
	r := b.remote(b.addr)
	ch, err := r.Start(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	drain(t, ch, 320)
	if err := b.srv.Revoke("studio"); err != nil {
		t.Fatal(err)
	}
	drain(t, ch, -1)
	if err := r.Err(); err == nil || err.Error() != "remote mic micbox refused" {
		t.Errorf("revoked mid-stream: %v", err)
	}
	if _, err := b.remote(b.addr).Start(context.Background()); err == nil || err.Error() != "remote mic micbox refused — pair again" {
		t.Errorf("revoked at connect: %v", err)
	}
}

func TestRemoteCarriesTheBoxError(t *testing.T) {
	b := newBox(t, func() audio.Source {
		return &holdSource{end: true, err: errors.New("array unavailable: no such device")}
	})
	r := b.remote(b.addr)
	ch, err := r.Start(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	drain(t, ch, -1)
	if err := r.Err(); err == nil || err.Error() != "remote mic micbox: array unavailable: no such device" {
		t.Errorf("Err = %v", err)
	}
}

// cutLink relays TCP to target until cut, then holds every connection open
// and forwards nothing: a box whose network is gone.
type cutLink struct {
	l   net.Listener
	cut atomic.Bool
}

func newCutLink(t *testing.T, target string) *cutLink {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { l.Close() })
	c := &cutLink{l: l}
	go func() {
		for {
			in, err := l.Accept()
			if err != nil {
				return
			}
			out, err := net.Dial("tcp", target)
			if err != nil {
				in.Close()
				continue
			}
			t.Cleanup(func() { in.Close(); out.Close() })
			go c.copy(in, out)
			go c.copy(out, in)
		}
	}()
	return c
}

func (c *cutLink) copy(dst io.Writer, src io.Reader) {
	buf := make([]byte, 4096)
	for {
		n, err := src.Read(buf)
		if err != nil {
			return
		}
		if !c.cut.Load() {
			dst.Write(buf[:n])
		}
	}
}

func TestRemoteLinkCutEndsAfterTwoSeconds(t *testing.T) {
	src := &holdSource{chunks: [][]int16{ramp(640, 0)}}
	b := newBox(t, func() audio.Source { return src })
	link := newCutLink(t, b.addr)
	r := b.remote(link.l.Addr().String())
	ch, err := r.Start(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	drain(t, ch, 640)
	link.cut.Store(true)
	cut := time.Now()
	drain(t, ch, -1)
	if d := time.Since(cut); d < audio.RemoteStall || d > audio.RemoteStall+time.Second {
		t.Errorf("stream ended %v after the cut, want about %v", d, audio.RemoteStall)
	}
	if err := r.Err(); err == nil || err.Error() != "remote mic micbox: no audio for 2 s" {
		t.Errorf("Err = %v", err)
	}
}

// Something that accepts and closes (a proxy's fake IP) is named as not the
// mic server, not left as a bare EOF.
func TestRemoteAcceptAndCloseIsNamed(t *testing.T) {
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
	r := &audio.Remote{Name: "micbox", Addr: l.Addr().String(), Fingerprint: strings.Repeat("0", 64), Token: "t"}
	if _, err := r.Start(context.Background()); err == nil || !strings.Contains(err.Error(), "closed the connection before TLS finished") {
		t.Errorf("Start = %v", err)
	}
}

func TestRemoteUnreachable(t *testing.T) {
	l, _ := net.Listen("tcp", "127.0.0.1:0")
	addr := l.Addr().String()
	l.Close()
	r := &audio.Remote{Name: "micbox", Addr: addr, Fingerprint: strings.Repeat("0", 64), Token: "t"}
	if _, err := r.Start(context.Background()); err == nil || !strings.HasPrefix(err.Error(), "remote mic micbox unreachable") {
		t.Errorf("Start = %v", err)
	}
}

// A take's log line names the remote mic it records from, as it names a
// local input (the session asks its source for Device).
func TestRemoteNamesItsDevice(t *testing.T) {
	var src audio.Source = &audio.Remote{Name: "micbox"}
	d, ok := src.(interface{ Device() string })
	if !ok || d.Device() != "remote mic micbox" {
		t.Errorf("Device = %v, %v", ok, d)
	}
}
