package remote

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/subtle"
	"crypto/tls"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/coder/websocket"

	"github.com/caoer/mega-asr/internal/audio"
	"github.com/caoer/mega-asr/internal/l10n"
	"github.com/caoer/mega-asr/internal/micserver"
)

// ServiceType is the DNS-SD service the Mac registers for phones to find.
const ServiceType = "_megavoice-ctl._tcp"

// The protocol's bounds (doc.go).
const (
	PairLife   = 2 * time.Minute  // a pairing secret's life
	Fresh      = 6 * time.Second  // a start older than this is refused
	HelloWait  = 10 * time.Second // a connection's first message comes within this
	MaxMessage = 4096             // bytes in one message
	MaxFrame   = 16 * 1024        // direct JSON or kind-prefixed binary frame
	maxConns   = 16               // direct connections at once, paired or not
	remembered = 64               // command ids whose answer a repeat gets
	recentN    = 3                // takes in a hello's or reply's recent
	pingEvery  = 30 * time.Second
)

// State is the Mac's state as a phone sees it. Epoch and Seq are the
// server's: Epoch names this process, Seq the version of (Phase, Take, Owner).
type State struct {
	Phase     string `json:"phase"` // idle, recording, transcribing, delivering
	Take      string `json:"take"`  // the recording take's id, "" when none records
	ElapsedMS int64  `json:"elapsed_ms"`
	Epoch     string `json:"epoch,omitempty"`
	Seq       uint64 `json:"seq,omitempty"`
	// Owner is who started the take that defines Phase: "mac" (the Mac's
	// own input, whatever asked for it) or "stream" (a phone keyboard
	// streams it). The recording take's; with none recording, "mac" while a
	// Mac take is on its way, else "stream"; omitted when idle.
	Owner string `json:"owner,omitempty"`
}

// Recent is one of the last takes as hello and reply carry them: Final is
// the take's final state, "" while it is open; AtMS its start.
type Recent struct {
	Take  string `json:"take"`
	Final string `json:"final"`
	Why   string `json:"why,omitempty"`
	AtMS  int64  `json:"at_ms"`
}

// Takes is what the server drives on this Mac. via is "iphone", "watch" or "keyboard";
// take is the id of the take a stop or cancel ends, and one that is not the
// recording take is refused with code "other_take". A refused command
// returns a *Refusal; any other error is reported as it is.
type Takes interface {
	Start(via string) (take string, err error)
	Stop(via, take string, send bool) error
	Cancel(via, take string) error
	State() State
}

// Refusal is a command refused: a code for the phone's logic and a Chinese
// reason it shows as it is.
type Refusal struct{ Code, Reason string }

func (r *Refusal) Error() string { return r.Code + ": " + r.Reason }

// Refuse is a Refusal as an error.
func Refuse(code, reason string) error { return &Refusal{Code: code, Reason: reason} }

// Config is what a Server needs from its host.
type Config struct {
	Listen string // host:port, e.g. ":7867"
	// State is the server's directory, made 0700: key.pem (0600), cert.pem
	// and phones.toml (0600), and the relay's keys when it is on.
	State string
	Name  string // this Mac's name until the advertisement names it
	Takes Takes
	// Recent is the last takes, newest first, at most n, read from the
	// store; nil sends none.
	Recent func(n int) []Recent
	// Advertise registers the service at port with txt until ctx ends,
	// telling onName the name it holds (mac.Register); nil advertises
	// nothing. A failure is retried every 30 s.
	Advertise func(ctx context.Context, port int, txt []string, onName func(string)) error
	// ASRKey reads a cloud engine's key for a phone that asks (asr_key):
	// called at each request, so the key file is read then; nil answers
	// every request with no_key.
	ASRKey func(engine string) (ASRKey, error)
	Logf   func(format string, a ...any) // nil is log.Printf
}

// ASRKey is a cloud engine's key as a paired phone receives it: the plan
// key, its endpoint and the resource it bills.
type ASRKey struct{ Key, URL, ResourceID string }

// ASREngines are the engines a phone may ask a key for.
var ASREngines = []string{"doubao"}

var engineLabel = map[string]l10n.Pair{"doubao": l10n.Pair{"豆包", "Doubao"}}

// Server is the phones' listener. Its control operations (NewPairing,
// Phones, Revoke, Changed, Done, RotateRelay) are safe to call while it
// serves.
type Server struct {
	cfg   Config
	cert  tls.Certificate
	fp    string
	logf  func(format string, a ...any)
	now   func() time.Time
	wake  chan struct{}
	epoch string // this process's, minted at New

	mu        sync.Mutex
	phones    []*phone
	pairing   *pairWindow
	conns     map[*conn]struct{}
	name      string // the advertised name, once there is one
	addr      string // the bound address, once Serve listens
	serveErr  error  // why Serve stopped
	advertErr error  // why the service is not advertised

	cmdMu   sync.Mutex // one command at a time, so a repeated id acts once
	replies map[string]reply
	order   []string // remembered ids, oldest first

	stMu      sync.Mutex // reads of the state, so seq follows their order
	seq       uint64
	lastPhase string
	lastTake  string
	lastOwner string

	relay    atomic.Pointer[relaySide] // nil while the relay is off
	relayWhy atomic.Pointer[string]    // why the relay [remote.relay] turns on is not running
}

type pairWindow struct {
	secret  string
	pairing Pairing
}

// New loads the server's key, certificate and phones from cfg.State,
// creating them on first run.
func New(cfg Config) (*Server, error) {
	if cfg.State == "" || cfg.Takes == nil {
		return nil, errors.New("remote: no state directory or no takes")
	}
	if err := os.MkdirAll(cfg.State, 0o700); err != nil {
		return nil, fmt.Errorf("remote: %w", err)
	}
	if err := os.Chmod(cfg.State, 0o700); err != nil {
		return nil, fmt.Errorf("remote: %w", err)
	}
	cert, err := micserver.LoadCert(cfg.State, "megavoice remote "+cfg.Name)
	if err != nil {
		return nil, fmt.Errorf("remote: %w", err)
	}
	phones, err := loadPhones(cfg.State + "/phones.toml")
	if err != nil {
		return nil, fmt.Errorf("remote: %w", err)
	}
	e := make([]byte, 8)
	if _, err := rand.Read(e); err != nil {
		return nil, fmt.Errorf("remote: %w", err)
	}
	s := &Server{
		cfg:     cfg,
		cert:    cert,
		fp:      audio.CertFingerprint(cert.Certificate[0]),
		logf:    cfg.Logf,
		now:     time.Now,
		wake:    make(chan struct{}, 1),
		epoch:   base64.RawURLEncoding.EncodeToString(e),
		phones:  phones,
		conns:   map[*conn]struct{}{},
		replies: map[string]reply{},
	}
	if s.logf == nil {
		s.logf = log.Printf
	}
	return s, nil
}

// Fingerprint is the SHA-256 of the certificate's DER, lowercase hex: what a
// paired phone pins.
func (s *Server) Fingerprint() string { return s.fp }

// TLSConfig serves the certificate over TLS 1.3, HTTP/1.1 only: a WebSocket
// is an HTTP/1.1 upgrade.
func (s *Server) TLSConfig() *tls.Config {
	return &tls.Config{MinVersion: tls.VersionTLS13, Certificates: []tls.Certificate{s.cert}, NextProtos: []string{"http/1.1"}}
}

// Handler answers every path with the phones' WebSocket.
func (s *Server) Handler() http.Handler { return http.HandlerFunc(s.ctl) }

// Serve listens on cfg.Listen over TLS and advertises the service until ctx
// ends, then closes every connection.
func (s *Server) Serve(ctx context.Context) error {
	err := s.serve(ctx)
	s.mu.Lock()
	s.serveErr = err
	s.mu.Unlock()
	return err
}

func (s *Server) serve(ctx context.Context) error {
	l, err := net.Listen("tcp", s.cfg.Listen)
	if err != nil {
		return err
	}
	port := l.Addr().(*net.TCPAddr).Port
	s.mu.Lock()
	s.addr = l.Addr().String()
	s.mu.Unlock()
	hs := &http.Server{
		Handler:           s.Handler(),
		TLSConfig:         s.TLSConfig(),
		ReadHeaderTimeout: 5 * time.Second,
		TLSNextProto:      map[string]func(*http.Server, *tls.Conn, http.Handler){}, // no HTTP/2
		ErrorLog:          log.New(logWriter(s.logf), "remote: ", 0),
	}
	s.logf("remote: listening on %s for paired phones, certificate %s", l.Addr(), s.fp)
	go s.advertise(ctx, port)
	go s.broadcastStates(ctx)
	errc := make(chan error, 1)
	go func() { errc <- hs.ServeTLS(l, "", "") }()
	select {
	case err := <-errc:
		return err
	case <-ctx.Done():
	}
	s.mu.Lock()
	for c := range s.conns {
		c.close(websocket.StatusGoingAway, "megavoice stopping", nil)
	}
	s.mu.Unlock()
	sctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	_ = hs.Shutdown(sctx)
	return nil
}

// advertise keeps the service registered until ctx ends, retrying a failed
// registration every 30 s (Local Network access may be granted meanwhile).
func (s *Server) advertise(ctx context.Context, port int) {
	if s.cfg.Advertise == nil {
		return
	}
	txt := []string{"v=1", "fp=" + s.fp[:16]}
	for ctx.Err() == nil {
		err := s.cfg.Advertise(ctx, port, txt, func(name string) {
			s.mu.Lock()
			s.name, s.advertErr = name, nil
			s.mu.Unlock()
			s.logf("remote: advertised as %q (%s)", name, ServiceType)
		})
		if ctx.Err() != nil {
			return
		}
		s.mu.Lock()
		s.advertErr = err
		s.mu.Unlock()
		s.logf("remote: advertising %s: %v; retrying in 30 s (phones paired by QR still reach its addresses)", ServiceType, err)
		select {
		case <-ctx.Done():
		case <-time.After(30 * time.Second):
		}
	}
}

// macName is the name phones see; the caller holds s.mu.
func (s *Server) macName() string {
	if s.name != "" {
		return s.name
	}
	return s.cfg.Name
}

// pairWindowLocked is the live pairing window; the caller holds s.mu.
func (s *Server) pairWindowLocked() *pairWindow {
	if s.pairing != nil && !s.now().Before(s.pairing.pairing.Expires) {
		s.pairing = nil
	}
	return s.pairing
}

// state is the Mac's state now, with this process's epoch and the version
// of (phase, take): bumped whenever a read finds them changed. Reads are
// serialized, so a later read never carries a lower seq.
func (s *Server) state() State {
	s.stMu.Lock()
	defer s.stMu.Unlock()
	st := s.cfg.Takes.State()
	if s.seq == 0 || st.Phase != s.lastPhase || st.Take != s.lastTake || st.Owner != s.lastOwner {
		s.seq++
		s.lastPhase, s.lastTake, s.lastOwner = st.Phase, st.Take, st.Owner
	}
	st.Epoch, st.Seq = s.epoch, s.seq
	return st
}

// recent is the last takes for a hello or a reply; never nil, so the field
// is always present.
func (s *Server) recent() []Recent {
	var r []Recent
	if s.cfg.Recent != nil {
		r = s.cfg.Recent(recentN)
	}
	if r == nil {
		r = []Recent{}
	}
	return r
}

// Changed tells the server the Mac's state may have changed; it never
// blocks, and the phones get the state as it is when the server reads it.
func (s *Server) Changed() {
	select {
	case s.wake <- struct{}{}:
	default:
	}
}

// broadcastStates sends the state to every phone at each Changed.
func (s *Server) broadcastStates(ctx context.Context) {
	for {
		select {
		case <-ctx.Done():
			return
		case <-s.wake:
		}
		s.broadcast(stateMsg{Type: "state", State: s.state()})
	}
}

// Done tells every phone a take's final state: sent, pasted, cancelled,
// empty or undelivered (why says why).
func (s *Server) Done(take, final, why string) {
	s.broadcast(takeMsg{Type: "take", Take: take, Final: final, Why: why})
}

// broadcast queues v for every phone, direct and relay alike: each relay
// session seals its own copy when its writer sends it.
func (s *Server) broadcast(v any) {
	b := encode(v)
	s.mu.Lock()
	defer s.mu.Unlock()
	for c := range s.conns {
		c.send(b)
	}
}

// The messages (doc.go).
type (
	inMsg struct {
		Type    string  `json:"type"`
		Name    string  `json:"name,omitempty"`
		Payload string  `json:"payload,omitempty"`
		Secret  string  `json:"secret,omitempty"`
		Token   string  `json:"token,omitempty"`
		Device  string  `json:"device,omitempty"`
		ID      string  `json:"id,omitempty"`
		Action  string  `json:"action,omitempty"`
		End     string  `json:"end,omitempty"`
		Via     string  `json:"via,omitempty"`
		Take    string  `json:"take,omitempty"`
		SentAt  int64   `json:"sent_at,omitempty"`
		Source  string  `json:"source,omitempty"`
		Stream  string  `json:"stream,omitempty"`
		Format  string  `json:"format,omitempty"`
		LastSeq *uint32 `json:"last_seq,omitempty"`
		Role    string  `json:"role,omitempty"`   // relay_key
		Sig     string  `json:"sig,omitempty"`    // relay_key
		Engine  string  `json:"engine,omitempty"` // asr_key
	}
	refusedMsg struct {
		Type   string `json:"type"` // refused
		Code   string `json:"code"`
		Reason string `json:"reason"`
	}
	pairedMsg struct {
		Type  string `json:"type"` // paired
		Mac   string `json:"mac"`
		Token string `json:"token"`
	}
	helloMsg struct {
		Type   string   `json:"type"` // hello
		Mac    string   `json:"mac"`
		State  State    `json:"state"`
		Recent []Recent `json:"recent"`
	}
	reply struct {
		Type   string   `json:"type"` // reply
		ID     string   `json:"id"`
		OK     bool     `json:"ok"`
		Take   string   `json:"take,omitempty"` // Stable id for a stream start, even after its last record.
		Code   string   `json:"code,omitempty"`
		Reason string   `json:"reason,omitempty"`
		State  State    `json:"state"`
		Recent []Recent `json:"recent"`
	}
	stateMsg struct {
		Type  string `json:"type"` // state
		State State  `json:"state"`
	}
	takeMsg struct {
		Type  string `json:"type"` // take
		Take  string `json:"take"`
		Final string `json:"final"`
		Why   string `json:"why,omitempty"`
	}
	relayMsg struct {
		Type   string `json:"type"` // relay
		URL    string `json:"url"`
		Room   string `json:"room"`
		MacKem string `json:"mac_kem"`
	}
	relayOKMsg struct {
		Type string `json:"type"` // relay_ok
		Role string `json:"role"`
	}
	byeMsg struct {
		Type   string `json:"type"` // bye
		Code   int    `json:"code"`
		Reason string `json:"reason"`
	}
	asrKeyMsg struct {
		Type       string `json:"type"` // asr_key
		ID         string `json:"id"`
		OK         bool   `json:"ok"`
		Engine     string `json:"engine"`
		Key        string `json:"key,omitempty"`
		URL        string `json:"url,omitempty"`
		ResourceID string `json:"resource_id,omitempty"`
		Code       string `json:"code,omitempty"`
		Reason     string `json:"reason,omitempty"`
	}
)

// encode marshals a message the Mac sends and adds now_ms, the Mac's clock
// at that moment: every Mac message carries it (PROTOCOL.md § 8).
func encode(v any) []byte { return encodeAt(v, time.Now().UnixMilli()) }

// encodeAt is encode with now_ms given (testdata/remote-messages.json).
func encodeAt(v any, nowMS int64) []byte {
	b, err := json.Marshal(v)
	if err != nil || len(b) < 2 || b[len(b)-1] != '}' {
		panic(fmt.Sprintf("remote: %T is not a message", v))
	}
	ms := strconv.FormatInt(nowMS, 10)
	out := make([]byte, 0, len(b)+len(ms)+12)
	out = append(out, b[:len(b)-1]...)
	if !bytes.Equal(b, []byte("{}")) {
		out = append(out, ',')
	}
	out = append(out, `"now_ms":`...)
	out = append(out, ms...)
	return append(out, '}')
}

// The refusals a phone shows as they are, in the Mac's language.
const reasonRevoked = "revoked"

func reasonPair() string {
	return l10n.T("配对码无效或已用过：在 Mac 菜单栏 › iPhone 遥控… 重新生成二维码", "The pairing code is invalid or used: make a new QR code in the Mac menu bar › iPhone Remote…")
}

func reasonToken() string {
	return l10n.T("这台 iPhone 未配对，或已在 Mac 上取消配对：重新扫码配对", "This iPhone isn't paired, or was unpaired on the Mac: scan to pair again")
}
func reasonFirst() string { return l10n.T("先配对或打招呼", "Pair or say hello first") }
func reasonBusy() string  { return l10n.T("Mac 上的连接太多", "Too many connections on the Mac") }
func reasonNoTake() string {
	return l10n.T("命令没有说结束哪一段录音", "The command doesn't say which take to end")
}

// ReasonStreamTake refuses a stop or cancel of a keyboard-stream take
// from anywhere but the keyboard that streams it.
func ReasonStreamTake() string {
	return l10n.T("这段是 iPhone 键盘录音，请从键盘结束", "This take is an iPhone keyboard recording; end it from the keyboard")
}

// conn is one phone's live connection, direct or through the relay.
type conn struct {
	sess     Session
	phone    *phone            // nil until it paired or said hello
	relay    bool              // a relay session
	paired   bool              // direct, and its first message was a successful pair
	keyDone  bool              // the phone's relay key was taken on this connection
	seen     bool              // relay: a record opened, so the phone is there
	out      chan textDelivery // One FIFO for replies, pushes and acknowledged text.
	done     chan struct{}
	closing  sync.Once
	streamMu sync.Mutex
	stream   *phoneTake
}

func newConn(sess Session, relay bool) *conn {
	return &conn{sess: sess, relay: relay, out: make(chan textDelivery, 32), done: make(chan struct{})}
}

// send queues b for the phone; a phone that does not keep up is closed
// with 1013, so it connects again: 1008 is for a token that is no good, and
// a phone stops on it.
func (c *conn) send(b []byte) {
	select {
	case <-c.done:
	case c.out <- textDelivery{message: b}:
	default:
		c.close(websocket.StatusTryAgainLater, "too slow", nil)
	}
}

// close ends the connection once, writing last first when it is not nil.
func (c *conn) close(code websocket.StatusCode, reason string, last []byte) {
	c.closing.Do(func() {
		close(c.done)
		c.streamMu.Lock()
		if c.stream != nil {
			c.stream.src.fail(errGone)
		}
		c.streamMu.Unlock()
		c.sess.Close(code, reason, last)
	})
}

// writer sends what is queued until the connection is done.
func (c *conn) writer() {
	for {
		select {
		case <-c.done:
			return
		case d := <-c.out:
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			err := c.sess.Write(ctx, d.message)
			cancel()
			if d.done != nil {
				d.done <- err
			}
			if err != nil {
				c.close(websocket.StatusInternalError, "write", nil)
				return
			}
		}
	}
}

func (c *conn) name() string {
	if c.phone == nil {
		return "?"
	}
	return c.phone.Name
}

// ctl is one phone's direct WebSocket: its first message pairs or says
// hello, then the session runs until it leaves.
func (s *Server) ctl(w http.ResponseWriter, r *http.Request) {
	// Every connection proves itself by its first message; no browser
	// credential rides it, so the Origin header is not checked.
	ws, err := websocket.Accept(w, r, &websocket.AcceptOptions{InsecureSkipVerify: true})
	if err != nil {
		s.logf("remote: %s: %v", r.RemoteAddr, err)
		return
	}
	ws.SetReadLimit(MaxFrame)
	ses := &wsSession{ws: ws, addr: r.RemoteAddr}
	c := newConn(ses, false)
	if !s.track(c) {
		s.logf("remote: %s refused: %d connections", ses.addr, maxConns)
		writeNow(ws, refusedMsg{"refused", "busy", reasonBusy()})
		ws.Close(websocket.StatusTryAgainLater, "busy")
		return
	}
	defer s.untrack(c)
	// A close (revoke, too slow) runs the closing handshake on its own
	// goroutine; the read below ends once it is through, so the phone gets
	// the close's code and reason.
	ctx, cancel := context.WithCancel(r.Context())
	defer cancel()
	hctx, hcancel := context.WithTimeout(ctx, HelloWait)
	first, err := readMsg(hctx, ses)
	hcancel()
	if err != nil {
		s.logf("remote: %s: no pairing or hello: %v", ses.addr, err)
		c.close(websocket.StatusTryAgainLater, "hello", nil)
		return
	}
	greeting, refusal := s.greet(c, first)
	if refusal != nil {
		code := websocket.StatusNormalClosure
		if refusal.Code == "token" {
			code = websocket.StatusPolicyViolation
		}
		c.close(code, refusal.Code, encode(refusal))
		return
	}
	go c.writer()
	s.queue(c, greeting)
	if first.Type == "pair" {
		s.queue(c, stateMsg{Type: "state", State: s.state()})
	}
	if m, ok := s.relayOffer(); ok {
		s.queue(c, m)
	}
	go s.ping(ctx, c, ses)
	s.loop(ctx, c)
}

// loop reads a session's messages after its greeting until it ends.
func (s *Server) loop(ctx context.Context, c *conn) {
	for {
		kind, b, err := readRecord(ctx, c.sess)
		if err == nil && kind != 1 {
			if kind == StreamKind {
				c.feedStream(b)
			}
			continue
		}
		var m inMsg
		if err == nil {
			m, err = decodeMsg(b)
		}
		if err != nil {
			if !errors.As(err, new(*json.SyntaxError)) {
				s.logf("remote: %s (%s): left: %s", c.name(), c.sess.Addr(), closeReason(err))
				return
			}
			s.queue(c, s.reply(reply{Type: "reply", Code: "bad_request", Reason: l10n.T("消息不是 JSON", "The message isn't JSON")}))
			continue
		}
		if c.relay && !c.seen {
			c.seen = true
			s.touch(c.phone)
		}
		switch m.Type {
		case "cmd":
			s.queue(c, s.commandFor(c, c.name(), m))
		case "relay_key":
			s.queue(c, s.relayKey(c, m))
		case "diagnostic":
			s.queue(c, s.diagnostic(c, m))
		case "asr_key":
			s.queue(c, s.asrKey(c, m))
		default:
			s.queue(c, s.reply(reply{Type: "reply", ID: m.ID, Code: "bad_request", Reason: l10n.T("不认识的消息：", "Unknown message: ") + m.Type}))
		}
	}
}

// diagnostic is reached only after the connection authenticated. Rows are JSON,
// compacted onto one log line; no take command or ASR routing is involved.
func (s *Server) diagnostic(c *conn, m inMsg) any {
	r := reply{Type: "reply", ID: m.ID, Code: "bad_request", Reason: l10n.T("测速结果无效", "Invalid speed test result")}
	if m.ID == "" || len(m.ID) > 128 || m.Name != "cpu_decode" || len(m.Payload) > 3000 || !json.Valid([]byte(m.Payload)) {
		return s.reply(r)
	}
	var compact bytes.Buffer
	if err := json.Compact(&compact, []byte(m.Payload)); err != nil {
		return s.reply(r)
	}
	s.logf("remote: diagnostic cpu_decode from %q id %s: %s", c.name(), m.ID, compact.String())
	r.OK, r.Code, r.Reason = true, "", ""
	return s.reply(r)
}

// asrKey answers a phone session's asr_key with the engine's key. Only loop
// calls it, so only a phone that paired or said hello with its token (direct,
// TLS pinned) or with its signed key (relay, sealed end to end) gets a key; a
// watch's one-shot request runs commands only. The key is not logged and not
// kept: the answer leaves through the session's writer and nothing else.
func (s *Server) asrKey(c *conn, m inMsg) any {
	refuse := func(code, reason string) any {
		return asrKeyMsg{Type: "asr_key", ID: m.ID, Engine: m.Engine, Code: code, Reason: reason}
	}
	if m.ID == "" || len(m.ID) > 128 {
		return refuse("bad_request", l10n.T("请求没有 id", "The request has no id"))
	}
	if !slices.Contains(ASREngines, m.Engine) {
		return refuse("bad_request", l10n.T("不认识的识别引擎：", "Unknown engine: ")+m.Engine)
	}
	path := "direct"
	if c.relay {
		path = "relay"
	}
	if s.cfg.ASRKey == nil {
		return refuse("no_key", l10n.T("这台 Mac 不给出识别密钥", "This Mac doesn't hand out engine keys"))
	}
	k, err := s.cfg.ASRKey(m.Engine)
	if err == nil && k.Key == "" {
		err = errors.New("empty")
	}
	if err != nil {
		s.logf("remote: %s key for %q (%s) refused: %v", m.Engine, c.name(), path, err)
		return refuse("no_key", l10n.F("Mac 读不到%s密钥：", "The Mac can't read the %s key: ", engineLabel[m.Engine].String())+err.Error())
	}
	s.logf("remote: %s key sent to %q (%s)", m.Engine, c.name(), path)
	return asrKeyMsg{Type: "asr_key", ID: m.ID, OK: true, Engine: m.Engine, Key: k.Key, URL: k.URL, ResourceID: k.ResourceID}
}

// greet answers a direct connection's first message: a pairing or a hello.
// On a refusal the connection closes.
func (s *Server) greet(c *conn, m inMsg) (any, *refusedMsg) {
	switch m.Type {
	case "pair":
		name, err := deviceName(m.Device)
		if err != nil {
			return nil, &refusedMsg{"refused", "bad_request", l10n.T("设备名不对：", "Bad device name: ") + err.Error()}
		}
		s.mu.Lock()
		defer s.mu.Unlock()
		w := s.pairWindowLocked()
		if w == nil || !same(m.Secret, w.secret) {
			s.logf("remote: pairing %q from %s refused: no live pairing with that secret", name, c.sess.Addr())
			return nil, &refusedMsg{"refused", "pair", reasonPair()}
		}
		s.pairing = nil // single use
		token, err := s.addPhone(name)
		if err != nil {
			s.logf("remote: pairing %q from %s: %v", name, c.sess.Addr(), err)
			return nil, &refusedMsg{"refused", "pair", l10n.T("Mac 存不下这次配对：", "The Mac can't save this pairing: ") + err.Error()}
		}
		c.phone = s.phone(name)
		c.paired = true
		s.syncDevices() // a phone paired again leaves its old keys behind
		s.logf("remote: paired %q from %s", name, c.sess.Addr())
		return pairedMsg{Type: "paired", Mac: s.macName(), Token: token}, nil
	case "hello":
		s.mu.Lock()
		p := s.byToken(m.Token)
		if p == nil {
			s.mu.Unlock()
			s.logf("remote: hello %q from %s refused: unknown token", m.Device, c.sess.Addr())
			return nil, &refusedMsg{"refused", "token", reasonToken()}
		}
		c.phone = p
		p.LastSeen = s.now().UTC()
		if err := s.save(); err != nil {
			s.logf("remote: phones.toml: %v", err)
		}
		name := s.macName()
		s.mu.Unlock()
		s.logf("remote: %q connected from %s", p.Name, c.sess.Addr())
		return helloMsg{Type: "hello", Mac: name, State: s.state(), Recent: s.recent()}, nil
	}
	return nil, &refusedMsg{"refused", "bad_request", reasonFirst()}
}

// touch records that p was seen now.
func (s *Server) touch(p *phone) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.phone(p.Name) != p {
		return // revoked or paired again meanwhile
	}
	p.LastSeen = s.now().UTC()
	if err := s.save(); err != nil {
		s.logf("remote: phones.toml: %v", err)
	}
}

// reply fills r's state and recent as they are now.
func (s *Server) reply(r reply) reply {
	r.State, r.Recent = s.state(), s.recent()
	return r
}

// command acts on one command once: a repeated id gets the first answer
// with the current state. A state command acts on nothing and is not
// remembered, so a watch's polls never push real commands out of the
// memory. from names the phone for the log.
func (s *Server) command(from string, m inMsg) reply {
	return s.commandFor(nil, from, m)
}

func (s *Server) commandFor(c *conn, from string, m inMsg) reply {
	return s.commandWhile(c, from, m, nil)
}

func (s *Server) commandWhile(c *conn, from string, m inMsg, live func() bool) reply {
	s.cmdMu.Lock()
	defer s.cmdMu.Unlock()
	if m.ID == "" || len(m.ID) > 128 {
		return s.reply(reply{Type: "reply", ID: m.ID, Code: "bad_request", Reason: l10n.T("命令没有 id", "The command has no id")})
	}
	if r, ok := s.replies[m.ID]; ok {
		return s.reply(r)
	}
	r := reply{Type: "reply", ID: m.ID, OK: true}
	var err error
	if live != nil && !live() {
		err = Refuse("stale", l10n.T("这次手表请求已结束，请查看当前状态", "This watch request is over; check the current state"))
	} else {
		err = s.actFor(c, m)
	}
	if err != nil {
		r.OK = false
		var rf *Refusal
		if errors.As(err, &rf) {
			r.Code, r.Reason = rf.Code, rf.Reason
		} else {
			r.Code, r.Reason = "failed", err.Error()
		}
	}
	if r.OK && c != nil && m.Action == "start" && m.Source == "stream" {
		c.streamMu.Lock()
		r.Take = c.stream.take
		c.streamMu.Unlock()
	}
	r = s.reply(r)
	if m.Action == "state" {
		return r
	}
	outcome := "ok"
	if !r.OK {
		outcome = r.Code + " (" + r.Reason + ")"
	}
	s.logf("remote: %s from %q via %s, end %q, id %s: %s; now %s", m.Action, from, m.Via, m.End, m.ID, outcome, r.State.Phase)
	s.replies[m.ID] = r
	s.order = append(s.order, m.ID)
	if len(s.order) > remembered {
		delete(s.replies, s.order[0])
		s.order = s.order[1:]
	}
	return r
}

func (s *Server) act(m inMsg) error {
	return s.actFor(nil, m)
}

func (s *Server) actFor(c *conn, m inMsg) error {
	via := m.Via
	switch via {
	case "":
		via = "iphone"
	case "iphone", "watch", "keyboard":
	default:
		return Refuse("bad_request", l10n.T("via 只能是 iphone、watch 或 keyboard", "via must be iphone, watch or keyboard"))
	}
	switch m.Action {
	case "start":
		if m.SentAt == 0 {
			return Refuse("bad_request", l10n.T("开始命令没有 sent_at", "The start command has no sent_at"))
		}
		age := s.now().Sub(time.UnixMilli(m.SentAt))
		switch {
		case age > Fresh:
			return Refuse("stale", l10n.F("这次按下已过去 %.0f 秒，没有开始录音", "That press was %.0f s ago; recording did not start", age.Seconds()))
		case age < -Fresh:
			return Refuse("stale", l10n.F("iPhone 和 Mac 的时间相差超过 %.0f 秒，没有开始录音", "The iPhone and Mac clocks differ by more than %.0f s; recording did not start", Fresh.Seconds()))
		}
		if m.Source == "stream" {
			return s.startStream(c, m, via)
		}
		if m.Source != "" {
			return Refuse("bad_request", l10n.T("不认识的录音来源", "Unknown recording source"))
		}
		_, err := s.cfg.Takes.Start(via)
		return err
	case "stop":
		if m.Take == "" {
			return Refuse("bad_request", reasonNoTake())
		}
		if m.Stream != "" {
			if c == nil || via != "keyboard" {
				return Refuse("bad_request", l10n.T("请从 iPhone 键盘结束键盘录音", "End a keyboard recording from the iPhone keyboard"))
			}
			return c.finishStream(m)
		}
		if owner := s.streamOwner(m.Take); owner != nil {
			if owner == c && via == "keyboard" {
				return c.finishStream(m) // Missing stream/last_seq is a malformed owner stop.
			}
			return Refuse("other_take", ReasonStreamTake())
		}
		switch m.End {
		case "", "paste":
			return s.cfg.Takes.Stop(via, m.Take, false)
		case "send":
			return s.cfg.Takes.Stop(via, m.Take, true)
		}
		return Refuse("bad_request", l10n.T("end 只能是 paste 或 send", "end must be paste or send"))
	case "cancel":
		if m.Take == "" {
			return Refuse("bad_request", reasonNoTake())
		}
		// A keyboard-stream take is cancelled only from the keyboard of the
		// phone that streams it: on that connection, or on a later one of the
		// same phone while the Mac still holds the first (a phone that lost its
		// connection cancels from the next).
		if owner := s.streamOwner(m.Take); owner != nil && (via != "keyboard" || !samePhone(owner, c)) {
			return Refuse("other_take", ReasonStreamTake())
		}
		return s.cfg.Takes.Cancel(via, m.Take)
	case "state":
		return nil
	}
	return Refuse("bad_request", l10n.T("不认识的命令：", "Unknown command: ")+m.Action)
}

// ping keeps a quiet direct connection checked: a phone that stopped
// answering is closed.
func (s *Server) ping(ctx context.Context, c *conn, w *wsSession) {
	t := time.NewTicker(pingEvery)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			pctx, cancel := context.WithTimeout(ctx, 10*time.Second)
			err := w.Ping(pctx)
			cancel()
			if err != nil && ctx.Err() == nil {
				c.close(websocket.StatusGoingAway, "no pong", nil)
				return
			}
		}
	}
}

func (s *Server) queue(c *conn, v any) { c.send(encode(v)) }

// writeNow writes v before a close: the connection's writer is not running.
func writeNow(ws *websocket.Conn, v any) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	_ = ws.Write(ctx, websocket.MessageText, encode(v))
}

// readMsg reads one message; a frame that is not a JSON object is a
// *json.SyntaxError, answered and skipped by the loop.
func readMsg(ctx context.Context, sess Session) (inMsg, error) {
	var m inMsg
	b, err := sess.Read(ctx)
	if errors.Is(err, errNotText) {
		return m, &json.SyntaxError{}
	}
	if err != nil {
		return m, err
	}
	if err := json.Unmarshal(b, &m); err != nil {
		var se *json.SyntaxError
		if errors.As(err, &se) {
			return m, se
		}
		return m, &json.SyntaxError{}
	}
	return m, nil
}

func closeReason(err error) string {
	if st := websocket.CloseStatus(err); st != -1 {
		return fmt.Sprintf("closed %d", st)
	}
	return err.Error()
}

// track admits a direct connection if fewer than maxConns are open.
func (s *Server) track(c *conn) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.count(false) >= maxConns {
		return false
	}
	s.conns[c] = struct{}{}
	return true
}

// count is how many connections of one kind are open; the caller holds s.mu.
func (s *Server) count(relay bool) int {
	n := 0
	for c := range s.conns {
		if c.relay == relay {
			n++
		}
	}
	return n
}

func (s *Server) untrack(c *conn) {
	s.mu.Lock()
	delete(s.conns, c)
	s.mu.Unlock()
	c.close(websocket.StatusNormalClosure, "", nil)
}

// closeConns closes p's connections as revoked; the caller holds s.mu. A
// relay session hears a sealed token refusal first, then bye 1008.
func (s *Server) closeConns(p *phone) {
	for c := range s.conns {
		if c.phone != p {
			continue
		}
		var last []byte
		if c.relay {
			last = encode(refusedMsg{"refused", "token", reasonToken()})
		}
		c.close(websocket.StatusPolicyViolation, reasonRevoked, last)
	}
}

// Phone is a paired phone as the pairing page lists it.
type Phone struct {
	Name     string    `json:"name"`
	Paired   time.Time `json:"paired"`
	LastSeen time.Time `json:"last_seen"`
	Online   int       `json:"online"` // its live connections
	Direct   int       `json:"direct"` // of them, on the LAN
	Relay    int       `json:"relay"`  // of them, through the relay
	RelayKey bool      `json:"relay_key"`
	Watch    bool      `json:"watch"` // its watch's relay key is registered
}

// Phones lists the paired phones by name.
func (s *Server) Phones() []Phone {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]Phone, len(s.phones))
	for i, p := range s.phones {
		out[i] = Phone{Name: p.Name, Paired: p.Paired, LastSeen: p.LastSeen, RelayKey: p.SigPK != "", Watch: p.WatchSigPK != ""}
		for c := range s.conns {
			if c.phone != p {
				continue
			}
			out[i].Online++
			if c.relay {
				out[i].Relay++
			} else {
				out[i].Direct++
			}
		}
	}
	return out
}

// Revoke forgets a phone and closes its connections; its token and its and
// its watch's relay keys are refused from then on, and the relay drops the
// keys.
func (s *Server) Revoke(name string) error {
	s.mu.Lock()
	gone := s.phone(name)
	if gone == nil {
		s.mu.Unlock()
		return ErrUnknownPhone
	}
	s.closeConns(gone)
	s.phones = slices.DeleteFunc(slices.Clone(s.phones), func(p *phone) bool { return p == gone })
	err := s.save()
	s.mu.Unlock()
	s.logf("remote: revoked %q", name)
	s.syncDevices()
	return err
}

func same(a, b string) bool { return subtle.ConstantTimeCompare([]byte(a), []byte(b)) == 1 }

// logWriter sends the HTTP server's own lines (TLS handshake errors) to logf.
type logWriter func(format string, a ...any)

func (w logWriter) Write(p []byte) (int, error) {
	w("%s", strings.TrimRight(string(p), "\n"))
	return len(p), nil
}
