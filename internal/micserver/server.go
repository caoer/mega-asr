package micserver

import (
	"context"
	"crypto/rand"
	"crypto/tls"
	"encoding/binary"
	"errors"
	"fmt"
	"log"
	"math/big"
	"net"
	"net/http"
	"os"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/coder/websocket"

	"github.com/caoer/mega-asr/internal/audio"
)

// Config is what a Server needs from its host.
type Config struct {
	Listen string // host:port the server listens on, e.g. ":7866"
	// State is the server's directory, created 0700: key.pem (0600),
	// cert.pem and clients.toml (0600).
	State  string
	Name   string              // this box's name, as a paired Mac lists it
	Source func() audio.Source // a new source for each stream
	// Logf logs every pairing, connect and disconnect; nil is log.Printf.
	Logf func(format string, a ...any)
}

// PINLife and PINTries bound a pairing PIN: it is gone after PINLife, after
// PINTries pairing attempts, or once a Mac pairs with it.
const (
	PINLife  = 2 * time.Minute
	PINTries = 3
)

// FrameSamples is one stream frame: 20 ms of Rate mono, sent as int16
// little-endian (640 bytes).
const FrameSamples = audio.Rate / 50

// ErrUnknownClient is Revoke's error for a name that is not paired.
var ErrUnknownClient = errors.New("no paired client by that name")

// Server is a mic server. Its control operations (NewPIN, Clients, Revoke)
// are safe to call while it serves.
type Server struct {
	cfg  Config
	cert tls.Certificate
	fp   string // CertFingerprint of cert
	logf func(format string, a ...any)
	now  func() time.Time

	mu      sync.Mutex
	clients []*client
	pinw    *pinWindow
	live    map[*liveStream]struct{}

	beforeTrack func() // tests: runs between a stream's token check and its tracking
}

// PIN is a pairing PIN: 8 digits, single-use, live until Expires.
type PIN struct {
	Code    string    `json:"pin"`
	Expires time.Time `json:"expires"`
}

// Client is a paired Mac as the server knows it.
type Client struct {
	Name     string    `json:"name"`
	Paired   time.Time `json:"paired"`
	LastSeen time.Time `json:"last_seen"` // zero until its first stream
	Streams  []Stream  `json:"streams"`   // its live streams
}

// Stream is one live stream of a client.
type Stream struct {
	Addr  string    `json:"addr"` // the Mac's remote address
	Since time.Time `json:"since"`
}

// New loads the server's key, certificate and clients from cfg.State,
// creating the directory, a P-256 key and a self-signed certificate on
// first run.
func New(cfg Config) (*Server, error) {
	if cfg.State == "" {
		return nil, errors.New("micserver: no state directory")
	}
	if err := os.MkdirAll(cfg.State, 0o700); err != nil {
		return nil, err
	}
	if err := os.Chmod(cfg.State, 0o700); err != nil {
		return nil, err
	}
	cert, err := loadCert(cfg.State, cfg.Name)
	if err != nil {
		return nil, fmt.Errorf("micserver: %w", err)
	}
	clients, err := loadClients(cfg.State + "/clients.toml")
	if err != nil {
		return nil, fmt.Errorf("micserver: %w", err)
	}
	s := &Server{
		cfg:     cfg,
		cert:    cert,
		fp:      audio.CertFingerprint(cert.Certificate[0]),
		logf:    cfg.Logf,
		now:     time.Now,
		clients: clients,
		live:    map[*liveStream]struct{}{},
	}
	if s.logf == nil {
		s.logf = log.Printf
	}
	return s, nil
}

// Fingerprint is the SHA-256 of the server certificate's DER, lowercase hex:
// what a paired Mac pins.
func (s *Server) Fingerprint() string { return s.fp }

// TLSConfig serves the server's certificate.
func (s *Server) TLSConfig() *tls.Config {
	return &tls.Config{MinVersion: tls.VersionTLS13, Certificates: []tls.Certificate{s.cert}}
}

// Handler routes POST /pair, POST /pair/confirm and GET /stream.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /pair", s.pairStart)
	mux.HandleFunc("POST /pair/confirm", s.pairConfirm)
	mux.HandleFunc("GET /stream", s.stream)
	return mux
}

// Serve listens on cfg.Listen over TLS until ctx ends, then closes every
// live stream.
func (s *Server) Serve(ctx context.Context) error {
	l, err := net.Listen("tcp", s.cfg.Listen)
	if err != nil {
		return err
	}
	hs := &http.Server{Handler: s.Handler(), TLSConfig: s.TLSConfig(), ReadHeaderTimeout: 5 * time.Second}
	s.logf("mic server %s on %s, certificate %s", s.cfg.Name, l.Addr(), s.fp)
	s.advertise(ctx, l.Addr().(*net.TCPAddr))
	errc := make(chan error, 1)
	go func() { errc <- hs.ServeTLS(l, "", "") }()
	select {
	case err := <-errc:
		return err
	case <-ctx.Done():
	}
	s.mu.Lock()
	for ls := range s.live {
		go ls.conn.Close(websocket.StatusGoingAway, "mic server stopping")
	}
	s.mu.Unlock()
	sctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_ = hs.Shutdown(sctx)
	return nil
}

// pinWindow is the live PIN: its tries so far and the pairing waiting for
// its confirmation.
type pinWindow struct {
	code    string
	expires time.Time
	tries   int
	pending *pendingPair
}

type pendingPair struct {
	id   string
	name string
	r    *responder
}

// NewPIN starts a pairing window with a fresh PIN, replacing any live one.
func (s *Server) NewPIN() (PIN, error) {
	n, err := rand.Int(rand.Reader, big.NewInt(100_000_000))
	if err != nil {
		return PIN{}, err
	}
	p := PIN{Code: fmt.Sprintf("%08d", n.Int64()), Expires: s.now().Add(PINLife)}
	s.mu.Lock()
	s.pinw = &pinWindow{code: p.Code, expires: p.Expires}
	s.mu.Unlock()
	s.logf("pairing PIN issued, live until %s", p.Expires.Format(time.TimeOnly))
	return p, nil
}

// livePIN is the PIN window while it lasts; the caller holds s.mu.
func (s *Server) livePIN() *pinWindow {
	if s.pinw != nil && !s.now().Before(s.pinw.expires) {
		s.pinw = nil
	}
	return s.pinw
}

// Clients lists the paired clients by name, each with its live streams.
func (s *Server) Clients() []Client {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]Client, len(s.clients))
	for i, c := range s.clients {
		out[i] = Client{Name: c.Name, Paired: c.Paired, LastSeen: c.LastSeen, Streams: []Stream{}}
		for ls := range s.live {
			if ls.name == c.Name {
				out[i].Streams = append(out[i].Streams, Stream{Addr: ls.addr, Since: ls.since})
			}
		}
	}
	return out
}

// Revoke forgets a client and closes its live streams; its token is
// refused from then on.
func (s *Server) Revoke(name string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	gone := s.client(name)
	if gone == nil {
		return ErrUnknownClient
	}
	s.clients = slices.DeleteFunc(slices.Clone(s.clients), func(c *client) bool { return c == gone })
	err := s.save()
	s.closeStreams(gone)
	s.logf("revoked %s", name)
	return err
}

// Token pairs name with a fresh token for a caller the control socket
// admitted (uid, as the socket reported it): ssh auto-pairing, where the
// Mac runs `megavoice mic token NAME` on this box. A name already paired is
// re-paired: its old token is refused from then on and its live streams
// close, as Revoke does.
func (s *Server) Token(name string, uid int) (string, error) {
	if !nameRE.MatchString(name) {
		return "", fmt.Errorf("client name %q: letters, digits, '.', '_' and '-', up to 64", name)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	old := s.client(name)
	prev := s.clients
	s.clients = slices.DeleteFunc(slices.Clone(prev), func(c *client) bool { return c == old })
	token, err := s.addClient(name)
	if err != nil {
		s.clients = prev
		return "", err
	}
	if old != nil {
		s.closeStreams(old)
	}
	s.logf("paired %s via the control socket (uid %d)", name, uid)
	return token, nil
}

// closeStreams ends c's live streams as revoked; the caller holds s.mu.
func (s *Server) closeStreams(c *client) {
	for ls := range s.live {
		if ls.client == c {
			ls.revoked = true
			go ls.conn.Close(websocket.StatusPolicyViolation, "revoked")
		}
	}
}

// liveStream is one connected Mac.
type liveStream struct {
	name, addr string
	since      time.Time
	conn       *websocket.Conn
	client     *client // the pairing whose token opened it
	revoked    bool    // under Server.mu
}

// stream sends the Mac one source's audio as FrameSamples frames until the
// Mac closes, the client is revoked, or the source ends; a source's error
// closes the connection with it as the reason.
func (s *Server) stream(w http.ResponseWriter, r *http.Request) {
	token, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
	s.mu.Lock()
	c := s.byToken(token)
	s.mu.Unlock()
	if !ok || c == nil {
		s.logf("stream from %s refused: unknown token", r.RemoteAddr)
		http.Error(w, "unknown token: pair again", http.StatusUnauthorized)
		return
	}
	conn, err := websocket.Accept(w, r, nil)
	if err != nil {
		s.logf("stream %s from %s: %v", c.Name, r.RemoteAddr, err)
		return
	}
	ls := &liveStream{name: c.Name, addr: r.RemoteAddr, since: s.now(), conn: conn, client: c}
	s.mu.Lock()
	hook := s.beforeTrack
	s.mu.Unlock()
	if hook != nil {
		hook()
	}
	if !s.admit(ls) {
		s.logf("stream %s from %s: refused, revoked before it started", ls.name, ls.addr)
		conn.Close(websocket.StatusPolicyViolation, "revoked")
		return
	}
	reason := s.pump(conn)
	s.leave(ls, reason)
}

// admit tracks ls if its pairing still stands: a revoke between the token
// check and here would otherwise miss the stream.
func (s *Server) admit(ls *liveStream) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !slices.Contains(s.clients, ls.client) {
		return false
	}
	s.live[ls] = struct{}{}
	s.logf("stream %s from %s: connected", ls.name, ls.addr)
	s.touch(ls.client)
	return true
}

func (s *Server) leave(ls *liveStream, reason string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.live, ls)
	if ls.revoked {
		reason = "revoked"
	}
	s.logf("stream %s from %s: ended after %s (%s)", ls.name, ls.addr, s.now().Sub(ls.since).Round(100*time.Millisecond), reason)
	if slices.Contains(s.clients, ls.client) {
		s.touch(ls.client)
	}
}

// touch sets c's last_seen; the caller holds s.mu.
func (s *Server) touch(c *client) {
	c.LastSeen = s.now().UTC()
	if err := s.save(); err != nil {
		s.logf("clients.toml: %v", err)
	}
}

// pump runs one source over conn and says how the stream ended. The Mac
// leaving stops the source through Stop, never by cancelling the context it
// started with: XVF runs arecord under a shell with exec.CommandContext, and
// a cancel kills the shell and leaves arecord holding the pipe, so the
// stream would never end.
func (s *Server) pump(conn *websocket.Conn) string {
	ctx := conn.CloseRead(context.Background()) // done once the Mac closes or a revoke does
	sctx, cancel := context.WithCancel(context.Background())
	defer cancel() // after the source has closed its stream
	src := s.cfg.Source()
	ch, err := src.Start(sctx)
	if err != nil {
		conn.Close(websocket.StatusInternalError, clip(err.Error()))
		return err.Error()
	}
	done := ctx.Done()
	stopped := false
	stop := func() {
		if !stopped {
			stopped = true
			go src.Stop()
		}
	}
	frame := make([]byte, 0, 2*FrameSamples)
	for {
		select {
		case <-done:
			done = nil
			stop()
		case b, ok := <-ch:
			if !ok {
				if stopped {
					return "closed"
				}
				if len(frame) > 0 {
					_ = conn.Write(ctx, websocket.MessageBinary, frame)
				}
				if err := src.Err(); err != nil {
					conn.Close(websocket.StatusInternalError, clip(err.Error()))
					return err.Error()
				}
				conn.Close(websocket.StatusNormalClosure, "")
				return "source ended"
			}
			if stopped {
				continue // drained after the Mac left
			}
			for _, v := range b {
				frame = binary.LittleEndian.AppendUint16(frame, uint16(v))
				if len(frame) == cap(frame) {
					if err := conn.Write(ctx, websocket.MessageBinary, frame); err != nil {
						stop()
					}
					frame = frame[:0]
				}
			}
		}
	}
}

// clip fits a close reason in a WebSocket close frame's 123 bytes.
func clip(s string) string {
	if len(s) > 120 {
		return s[:120]
	}
	return s
}
