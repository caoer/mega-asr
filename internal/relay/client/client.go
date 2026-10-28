// Package client is the Mac's link to its relay room (relay/PROTOCOL.md § 4):
// one outbound WebSocket to wss://<relay>/v1/ws carrying the room header,
// answered by signing the room's challenge with the Mac's key and, for a room
// not yet provisioned, the provision proof. Once the room says ok, the link
// acknowledges every session the room announces, passes the sessions' frames
// to its Handler, and carries the Mac's frames and control frames (drop,
// devices) back. It pings every 30 s, redials with backoff and jitter after
// any loss, at once after the room closed it as half-open (1012), and checks
// the socket at once when the system wakes from sleep.
package client

import (
	"context"
	"crypto/ed25519"
	"errors"
	"fmt"
	"log"
	"math/rand/v2"
	"net/http"
	"net/url"
	"sync"
	"time"

	"github.com/coder/websocket"

	"github.com/caoer/mega-asr/internal/l10n"
	"github.com/caoer/mega-asr/internal/relay/auth"
	"github.com/caoer/mega-asr/internal/relay/wire"
)

// Handler is told what the room says. Its methods are called in order from
// the link's reader and must not block: Frame and Peer of one session come in
// the room's order, and nothing more arrives until they return.
type Handler interface {
	LinkUp(peers []wire.PeerInfo)
	LinkDown()
	Peer(p wire.Peer)
	Frame(sid wire.SID, data []byte)
}

// Config is a link's relay, room and key.
type Config struct {
	URL          string // https://host; the link dials wss://host/v1/ws
	Room         wire.RoomID
	Key          ed25519.PrivateKey // the Mac's signing key
	ProvisionKey []byte             // 32 bytes: the proof that provisions an empty room
	Handler      Handler
	HTTPClient   *http.Client                  // nil: http.DefaultClient
	Logf         func(format string, a ...any) // nil: log.Printf
}

// The link's timings (PROTOCOL.md § 9).
const (
	dialWait    = 10 * time.Second
	authWait    = wire.AuthWindow
	backoffMin  = 500 * time.Millisecond
	backoffMax  = 30 * time.Second
	wakeCheck   = 5 * time.Second  // how often the clock is compared for a sleep
	wakePong    = 5 * time.Second  // after a wake, a pong comes within this or the link redials
	sleptMargin = 10 * time.Second // the wall clock ahead of the monotonic one by this: the system slept
	queueLen    = 256
)

// ErrOffline is a frame or exchange with no live link.
var ErrOffline = errors.New("relay: not connected")

// Client is the Mac's link. Its methods are safe for concurrent use.
type Client struct {
	cfg   Config
	ws    string // the WebSocket URL
	logf  func(format string, a ...any)
	kick  chan struct{} // Switch: redial now
	wakeC chan struct{}

	mu     sync.Mutex
	room   wire.RoomID
	key    ed25519.PrivateKey
	gen    uint64 // bumped by Switch; a socket dialed under an older one is not used
	cur    *link
	online bool
	why    string

	devMu sync.Mutex // one devices exchange at a time
}

// New checks cfg: the URL is https with a host and nothing else.
func New(cfg Config) (*Client, error) {
	u, err := url.Parse(cfg.URL)
	if err != nil || u.Scheme != "https" || u.Host == "" || (u.Path != "" && u.Path != "/") || u.RawQuery != "" || u.User != nil {
		return nil, fmt.Errorf("relay: url %q is not https://host", cfg.URL)
	}
	if len(cfg.Key) != ed25519.PrivateKeySize || cfg.Handler == nil {
		return nil, errors.New("relay: no key or no handler")
	}
	c := &Client{
		cfg:   cfg,
		ws:    "wss://" + u.Host + wire.PathWS,
		logf:  cfg.Logf,
		kick:  make(chan struct{}, 1),
		wakeC: make(chan struct{}, 1),
		room:  cfg.Room,
		key:   cfg.Key,
		why:   l10n.T("正在连接中继", "Connecting to the relay"),
	}
	if c.logf == nil {
		c.logf = log.Printf
	}
	return c, nil
}

// Status says whether the link is up, and why not.
func (c *Client) Status() (bool, string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.online, c.why
}

func (c *Client) setStatus(online bool, why string) {
	c.mu.Lock()
	changed := c.online != online || c.why != why
	c.online, c.why = online, why
	room := c.room
	c.mu.Unlock()
	if !changed {
		return
	}
	if online {
		c.logf("relay: online in room %s…", room.String()[:4])
	} else {
		c.logf("relay: offline: %s", why)
	}
}

// Switch moves the link to another room under another key: the socket
// closes and the link dials the new room at once.
func (c *Client) Switch(room wire.RoomID, key ed25519.PrivateKey) {
	c.mu.Lock()
	c.room, c.key = room, key
	c.gen++
	cur := c.cur
	c.mu.Unlock()
	if cur != nil {
		cur.close(websocket.StatusNormalClosure, "switch")
	}
	select {
	case c.kick <- struct{}{}:
	default:
	}
}

// Wake tells the link the system woke: it pings at once and redials when no
// pong comes within 5 s. Run also notices a sleep by itself.
func (c *Client) Wake() {
	select {
	case c.wakeC <- struct{}{}:
	default:
	}
}

func (c *Client) live() *link {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.cur
}

// Send queues a binary frame for session sid.
func (c *Client) Send(sid wire.SID, data []byte) error {
	l := c.live()
	if l == nil {
		return ErrOffline
	}
	return l.queue(websocket.MessageBinary, wire.JoinSID(sid, data))
}

// Drop tells the room to end session sid.
func (c *Client) Drop(sid wire.SID) {
	if l := c.live(); l != nil {
		_ = l.queue(websocket.MessageText, wire.Encode(wire.Drop{SID: sid}))
	}
}

// Devices sends a devices frame and returns the room's answer: every device
// it has registered.
func (c *Client) Devices(ctx context.Context, change wire.DevicesChange) ([]wire.Device, error) {
	c.devMu.Lock()
	defer c.devMu.Unlock()
	l := c.live()
	if l == nil {
		return nil, ErrOffline
	}
	ans := make(chan wire.DevicesList, 1)
	l.mu.Lock()
	l.devices = ans
	l.mu.Unlock()
	if err := l.queue(websocket.MessageText, wire.Encode(change)); err != nil {
		return nil, err
	}
	select {
	case <-ctx.Done():
		// a late answer must not be taken for the next exchange's
		l.close(websocket.StatusGoingAway, "devices")
		return nil, ctx.Err()
	case <-l.done:
		return nil, ErrOffline
	case d := <-ans:
		return d.Keys, nil
	}
}

// Run keeps the link up until ctx ends.
func (c *Client) Run(ctx context.Context) {
	go c.watchSleep(ctx)
	attempt := 0
	for ctx.Err() == nil {
		up, code := c.connect(ctx)
		if ctx.Err() != nil {
			break
		}
		var wait time.Duration
		switch {
		case code == wire.CloseSuperseded:
			// another socket of this Mac holds the room: stay closed until
			// a switch
			c.setStatus(false, l10n.T("另一个连接顶替了这台 Mac 的中继连接", "Another connection took over this Mac's relay connection"))
			select {
			case <-ctx.Done():
			case <-c.kick:
			}
			attempt = 0
			continue
		case code == wire.CloseMacGone && up:
			wait = 0 // the room found the socket half-open: redial at once
			attempt = 0
		case up:
			attempt = 0
			wait = backoff(0)
		default:
			attempt++
			wait = backoff(attempt)
		}
		select {
		case <-ctx.Done():
		case <-c.kick:
			attempt = 0
		case <-time.After(wait):
		}
	}
	c.setStatus(false, l10n.T("已停止", "Stopped"))
}

// backoff is the wait before attempt n: 0.5 s doubling to 30 s, half of it
// random.
func backoff(n int) time.Duration {
	d := backoffMin
	for range n {
		if d >= backoffMax/2 {
			d = backoffMax
			break
		}
		d *= 2
	}
	d = min(d, backoffMax)
	return d/2 + rand.N(d/2+1)
}

// watchSleep notices the system waking: the wall clock runs on through a
// sleep and the monotonic clock does not.
func (c *Client) watchSleep(ctx context.Context) {
	t := time.NewTicker(wakeCheck)
	defer t.Stop()
	last := time.Now()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
		now := time.Now()
		if now.Round(0).Sub(last.Round(0))-now.Sub(last) > sleptMargin {
			c.Wake()
		}
		last = now
	}
}

// link is one socket to the room.
type link struct {
	ws      *websocket.Conn
	out     chan frame
	done    chan struct{}
	closing sync.Once

	mu       sync.Mutex
	devices  chan wire.DevicesList // the devices exchange waiting for its answer
	lastPong time.Time
}

type frame struct {
	typ  websocket.MessageType
	data []byte
}

func (l *link) queue(typ websocket.MessageType, data []byte) error {
	select {
	case <-l.done:
		return ErrOffline
	case l.out <- frame{typ, data}:
		return nil
	default:
		l.close(websocket.StatusTryAgainLater, "queue full")
		return ErrOffline
	}
}

func (l *link) close(code websocket.StatusCode, reason string) {
	l.closing.Do(func() {
		close(l.done)
		go l.ws.Close(code, reason)
	})
}

func (l *link) writer() {
	for {
		select {
		case <-l.done:
			return
		case f := <-l.out:
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			err := l.ws.Write(ctx, f.typ, f.data)
			cancel()
			if err != nil {
				l.close(websocket.StatusGoingAway, "write")
				return
			}
		}
	}
}

// pinger sends the text ping every 30 s and closes a link whose pong does
// not come within 10 s; after a wake, at once and within 5 s.
func (l *link) pinger(wakeC <-chan struct{}) {
	t := time.NewTicker(wire.PingEvery)
	defer t.Stop()
	for {
		wait := wire.PongWait
		select {
		case <-l.done:
			return
		case <-t.C:
		case <-wakeC:
			wait = wakePong
		}
		sent := time.Now()
		if l.queue(websocket.MessageText, []byte(wire.PingText)) != nil {
			return
		}
		select {
		case <-l.done:
			return
		case <-time.After(wait):
		}
		l.mu.Lock()
		ok := !l.lastPong.Before(sent)
		l.mu.Unlock()
		if !ok {
			l.close(websocket.StatusGoingAway, "no pong")
			return
		}
	}
}

// connect runs one socket: dial, challenge, auth, then the room's frames
// until it closes. up says it reached ok; code is the close code, or -1.
func (c *Client) connect(ctx context.Context) (up bool, code int) {
	c.mu.Lock()
	room, key, gen := c.room, c.key, c.gen
	c.mu.Unlock()
	dctx, cancel := context.WithTimeout(ctx, dialWait)
	ws, _, err := websocket.Dial(dctx, c.ws, &websocket.DialOptions{
		HTTPClient: c.cfg.HTTPClient,
		HTTPHeader: http.Header{wire.Header: {room.String()}},
	})
	cancel()
	if err != nil {
		c.setStatus(false, l10n.T("连不上中继：", "Can't reach the relay: ")+err.Error())
		return false, -1
	}
	ws.SetReadLimit(wire.MaxToMac)
	stop := context.AfterFunc(ctx, func() { ws.Close(websocket.StatusGoingAway, "stopping") })
	defer stop()

	actx, cancel := context.WithTimeout(ctx, authWait)
	ok, err := c.authenticate(actx, ws, room, key)
	cancel()
	if err != nil {
		st := int(websocket.CloseStatus(err))
		why := l10n.T("中继没有接受这台 Mac：", "The relay didn't accept this Mac: ") + err.Error()
		if st == wire.CloseRefused {
			why = l10n.T("中继拒绝了这台 Mac 的钥匙（房间属于另一把钥匙，或中继的配置密钥未设好）", "The relay refused this Mac's key (the room belongs to another key, or the relay's config key isn't set)")
		}
		c.setStatus(false, why)
		ws.Close(websocket.StatusPolicyViolation, "")
		return false, st
	}

	l := &link{ws: ws, out: make(chan frame, queueLen), done: make(chan struct{}), lastPong: time.Now()}
	c.mu.Lock()
	if c.gen != gen {
		// a Switch while this socket dialed: it is in the old room, and
		// Switch's kick redials the new one at once
		c.mu.Unlock()
		ws.Close(websocket.StatusNormalClosure, "switch")
		return false, -1
	}
	c.cur = l
	c.mu.Unlock()
	c.setStatus(true, "")
	c.cfg.Handler.LinkUp(ok.Peers)
	go l.writer()
	go l.pinger(c.wakeC)

	err = c.read(ctx, l)
	l.close(websocket.StatusGoingAway, "")
	c.mu.Lock()
	if c.cur == l {
		c.cur = nil
	}
	c.mu.Unlock()
	c.cfg.Handler.LinkDown()
	code = int(websocket.CloseStatus(err))
	why := l10n.T("与中继的连接断了", "The relay connection dropped")
	if code != -1 {
		why = l10n.F("中继关闭了连接（%d）", "The relay closed the connection (%d)", code)
	}
	c.setStatus(false, why)
	return true, code
}

// authenticate answers the room's challenge (PROTOCOL.md § 4.2–4.3); the
// provision proof rides every auth, and a provisioned room ignores it.
func (c *Client) authenticate(ctx context.Context, ws *websocket.Conn, room wire.RoomID, key ed25519.PrivateKey) (wire.OKMac, error) {
	typ, b, err := ws.Read(ctx)
	if err != nil {
		return wire.OKMac{}, err
	}
	f, err := wire.DecodeFromRoom(b)
	ch, isCh := f.(wire.Challenge)
	if typ != websocket.MessageText || err != nil || !isCh {
		return wire.OKMac{}, errors.New("the room's first frame is not a challenge")
	}
	var proof []byte
	if len(c.cfg.ProvisionKey) == auth.SecretLen {
		proof = auth.ProvisionProof(c.cfg.ProvisionKey, room, key.Public().(ed25519.PublicKey))
	}
	if err := ws.Write(ctx, websocket.MessageText, auth.AuthFrame(key, room, wire.RoleMac, ch.Nonce[:], proof)); err != nil {
		return wire.OKMac{}, err
	}
	typ, b, err = ws.Read(ctx)
	if err != nil {
		return wire.OKMac{}, err
	}
	f, err = wire.DecodeFromRoom(b)
	ok, isOK := f.(wire.OKMac)
	if typ != websocket.MessageText || err != nil || !isOK {
		return wire.OKMac{}, errors.New("the room's answer to auth is not ok")
	}
	return ok, nil
}

// read passes the room's frames on until the socket ends.
func (c *Client) read(ctx context.Context, l *link) error {
	h := c.cfg.Handler
	for {
		typ, b, err := l.ws.Read(ctx)
		if err != nil {
			return err
		}
		if typ == websocket.MessageBinary {
			sid, data, err := wire.SplitSID(b)
			if err == nil {
				h.Frame(sid, data)
			}
			continue
		}
		if string(b) == wire.PongText {
			l.mu.Lock()
			l.lastPong = time.Now()
			l.mu.Unlock()
			continue
		}
		f, err := wire.DecodeFromRoom(b)
		if err != nil {
			c.logf("relay: a frame from the room that is not the protocol's; ignored")
			continue
		}
		switch f := f.(type) {
		case wire.Peer:
			if f.Up {
				// acknowledge first: the room is waiting to hear the
				// socket is alive (PROTOCOL.md § 4.4)
				_ = l.queue(websocket.MessageText, wire.Encode(wire.PeerAck{SID: f.SID}))
			}
			h.Peer(f)
		case wire.DevicesList:
			l.mu.Lock()
			ans := l.devices
			l.devices = nil
			l.mu.Unlock()
			if ans != nil {
				ans <- f
			}
		}
	}
}
