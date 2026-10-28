package fakeroom

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"io"
	"net"
	"net/http"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/caoer/mega-asr/internal/relay/auth"
	"github.com/caoer/mega-asr/internal/relay/wire"
	"github.com/coder/websocket"
)

type Config struct {
	RoomTagKey, ProvisionKey []byte
	Clock                    Clock
	Rand                     io.Reader
}
type Relay struct {
	mu        sync.Mutex
	cfg       Config
	clock     Clock
	rnd       io.Reader
	rooms     map[wire.RoomID]*room
	addresses map[string]*auth.Bucket
}
type room struct {
	id                wire.RoomID
	macKey            ed25519.PublicKey
	devices           map[string]wire.Device
	sockets           map[*Socket]bool
	mac               *Socket
	nonces            map[[16]byte]time.Time
	watchBuckets      map[string]*auth.Bucket
	pending           map[wire.SID]*request
	phoneAcks         map[wire.SID]*Socket
	created, lastSeen time.Time
}
type Socket struct {
	r                                   *Relay
	room                                *room
	role                                wire.Role
	key                                 ed25519.PublicKey
	sid                                 wire.SID
	authed, closed, knownByMac, oneshot bool
	out                                 chan Result
	challenge                           [32]byte
	timeout                             func() bool
	acks                                map[wire.SID]chan bool
	lastPing                            time.Time
	badTag                              bool
	pendingOK                           bool
	frameBucket                         auth.Bucket
}
type Result struct {
	Kind   string
	Data   []byte
	Code   int
	Reason string
	Status int
	Header http.Header
}
type request struct {
	sid   wire.SID
	key   ed25519.PublicKey
	body  []byte
	out   chan Result
	ack   chan bool
	acked bool
	timer func() bool
}

func New(cfg Config) *Relay {
	if cfg.Clock == nil {
		cfg.Clock = RealClock()
	}
	if cfg.Rand == nil {
		cfg.Rand = rand.Reader
	}
	return &Relay{cfg: cfg, clock: cfg.Clock, rnd: cfg.Rand, rooms: map[wire.RoomID]*room{}, addresses: map[string]*auth.Bucket{}}
}
func (r *Relay) random(b []byte) bool { _, e := io.ReadFull(r.rnd, b); return e == nil }
func (r *Relay) getRoom(id wire.RoomID, create bool) *room {
	q := r.rooms[id]
	if q == nil && create {
		q = &room{id: id, devices: map[string]wire.Device{}, sockets: map[*Socket]bool{}, nonces: map[[16]byte]time.Time{}, watchBuckets: map[string]*auth.Bucket{}, pending: map[wire.SID]*request{}, phoneAcks: map[wire.SID]*Socket{}, created: r.clock.Now(), lastSeen: r.clock.Now()}
		r.rooms[id] = q
	}
	if q != nil {
		q.lastSeen = r.clock.Now()
	}
	return q
}
func (r *Relay) newSocket(q *room) *Socket {
	s := &Socket{r: r, room: q, out: make(chan Result, 256), acks: map[wire.SID]chan bool{}, lastPing: r.clock.Now()}
	q.sockets[s] = true
	r.random(s.challenge[:])
	s.push(Result{Kind: "text", Data: wire.Encode(wire.Challenge{Nonce: s.challenge})})
	s.timeout = r.clock.AfterFunc(wire.AuthWindow, func() {
		r.mu.Lock()
		defer r.mu.Unlock()
		if !s.authed && !s.closed {
			r.closeLocked(s, wire.CloseRefused)
		}
	})
	r.idleAlarm(s)
	return s
}
func (r *Relay) idleAlarm(s *Socket) {
	r.clock.AfterFunc(wire.AlarmEvery, func() {
		r.mu.Lock()
		defer r.mu.Unlock()
		if s.closed {
			return
		}
		if r.clock.Now().Sub(s.lastPing) > wire.IdleClose {
			r.closeLocked(s, wire.CloseNormal)
			return
		}
		r.idleAlarm(s)
	})
}
func (s *Socket) push(x Result) {
	if s.closed && x.Kind != "close" {
		return
	}
	select {
	case s.out <- x:
	default:
	}
}
func (r *Relay) closeLocked(s *Socket, code int) {
	if s.closed {
		return
	}
	s.closed = true
	if s.timeout != nil {
		s.timeout()
	}
	q := s.room
	delete(q.sockets, s)
	s.push(Result{Kind: "close", Code: code})
	if s.role == wire.RoleMac && q.mac == s {
		q.mac = nil
		for p := range q.sockets {
			if p.authed && p.role == wire.RolePhone {
				r.closeLocked(p, wire.CloseMacGone)
			}
		}
		for sid, req := range q.pending {
			if req.timer != nil {
				req.timer()
			}
			req.out <- Result{Kind: "http", Status: 503}
			delete(q.pending, sid)
		}
		return
	}
	if s.role == wire.RolePhone && s.authed && s.knownByMac && q.mac != nil {
		q.mac.push(Result{Kind: "text", Data: wire.Encode(wire.Peer{SID: s.sid, Role: wire.RolePhone, Key: s.key, Up: false})})
	}
	if s.oneshot && s.authed && s.knownByMac && q.mac != nil {
		q.mac.push(Result{Kind: "text", Data: wire.Encode(wire.Peer{SID: s.sid, Role: wire.RoleWatch, Key: s.key, Up: false, OneShot: true})})
	}
}
func (r *Relay) Connect(id wire.RoomID) (*Socket, bool) { return r.ConnectTagged(id, true) }
func (r *Relay) ConnectTagged(id wire.RoomID, validTag bool) (*Socket, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	q := r.getRoom(id, validTag)
	if q == nil {
		q = &room{id: id, devices: map[string]wire.Device{}, sockets: map[*Socket]bool{}, nonces: map[[16]byte]time.Time{}, watchBuckets: map[string]*auth.Bucket{}, pending: map[wire.SID]*request{}, phoneAcks: map[wire.SID]*Socket{}}
	}
	n := 0
	for s := range q.sockets {
		if !s.authed {
			n++
		}
	}
	if n >= wire.MaxUnauthed {
		s := &Socket{r: r, room: q, out: make(chan Result, 1), closed: true}
		s.push(Result{Kind: "close", Code: wire.CloseFull})
		return s, true
	}
	s := r.newSocket(q)
	s.badTag = !validTag
	return s, true
}
func (s *Socket) Receive(kind string, data []byte) {
	r := s.r
	r.mu.Lock()
	defer r.mu.Unlock()
	if s.closed {
		return
	}
	q := s.room
	if kind == "text" && bytes.Equal(data, []byte(wire.PingText)) {
		s.lastPing = r.clock.Now()
		s.push(Result{Kind: "text", Data: []byte(wire.PongText)})
		return
	}
	if !s.authed {
		r.authLocked(s, kind, data)
		return
	}
	if s.pendingOK && s.role != wire.RoleMac {
		r.closeLocked(s, wire.CloseRefused)
		return
	}
	if s.role == wire.RoleMac {
		if s.frameBucket.Rate == 0 {
			s.frameBucket = auth.Bucket{Rate: 100, Burst: 200}
		}
		if !s.frameBucket.Allow(r.clock.Now()) {
			r.closeLocked(s, wire.CloseRate)
			return
		}
	}
	if s.role == wire.RolePhone {
		if s.frameBucket.Rate == 0 {
			s.frameBucket = auth.Bucket{Rate: 20, Burst: 60}
		}
		if !s.frameBucket.Allow(r.clock.Now()) {
			r.closeLocked(s, wire.CloseRate)
			return
		}
	}
	maxFrame := wire.MaxFrame
	if s.role == wire.RoleMac && kind == "binary" {
		maxFrame = wire.MaxToMac
	}
	if len(data) > maxFrame {
		r.closeLocked(s, wire.CloseTooBig)
		return
	}
	if s.role == wire.RolePhone {
		if kind != "binary" {
			r.closeLocked(s, wire.CloseUnsupported)
			return
		}
		if q.mac != nil {
			q.mac.push(Result{Kind: "binary", Data: wire.JoinSID(s.sid, data)})
		}
		return
	}
	if kind == "binary" {
		if len(data) < wire.SIDLen {
			r.closeLocked(s, wire.CloseUnsupported)
			return
		}
		sid, b, e := wire.SplitSID(data)
		if e != nil {
			return
		}
		if req := q.pending[sid]; req != nil && req.acked {
			if req.timer != nil {
				req.timer()
			}
			delete(q.pending, sid)
			req.out <- Result{Kind: "http", Status: 200, Data: b, Header: http.Header{"Content-Type": []string{"application/octet-stream"}}}
			r.peerDownLocked(q, req)
			return
		}
		for p := range q.sockets {
			if p.role == wire.RolePhone && p.authed && p.sid == sid {
				p.push(Result{Kind: "binary", Data: b})
				return
			}
		}
		return
	}
	if kind != "text" {
		r.closeLocked(s, wire.CloseUnsupported)
		return
	}
	f, e := wire.DecodeFromMac(data)
	if e != nil {
		r.closeLocked(s, wire.CloseUnsupported)
		return
	}
	switch v := f.(type) {
	case wire.PeerAck:
		if ch := s.acks[v.SID]; ch != nil {
			delete(s.acks, v.SID)
			select {
			case ch <- true:
			default:
			}
			if p := q.phoneAcks[v.SID]; p != nil {
				delete(q.phoneAcks, v.SID)
				if !p.closed {
					p.pendingOK = false
					p.push(Result{Kind: "text", Data: wire.Encode(wire.OKPhone{SID: p.sid, Peer: true})})
				}
			}
		}
		if req := q.pending[v.SID]; req != nil && req.ack != nil && !req.acked {
			req.acked = true
			select {
			case req.ack <- true:
			default:
			}
			if req.timer != nil {
				req.timer()
			}
			req.timer = r.clock.AfterFunc(wire.ReplyWait, func() {
				r.mu.Lock()
				defer r.mu.Unlock()
				if q.pending[v.SID] == req {
					delete(q.pending, v.SID)
					req.out <- Result{Kind: "http", Status: 504}
					r.peerDownLocked(q, req)
				}
			})
			s.push(Result{Kind: "binary", Data: wire.JoinSID(v.SID, req.body)})
		}
	case wire.DevicesChange:
		r.devicesLocked(s, v)
	case wire.Drop:
		if req := q.pending[v.SID]; req != nil {
			delete(q.pending, v.SID)
			if req.timer != nil {
				req.timer()
			}
			req.out <- Result{Kind: "http", Status: 504}
			r.peerDownLocked(q, req)
		} else {
			for p := range q.sockets {
				if p.sid == v.SID && p.role == wire.RolePhone {
					r.closeLocked(p, wire.CloseNormal)
				}
			}
		}
	case wire.Reset:
		s.closed = true
		delete(q.sockets, s)
		q.mac = nil
		s.push(Result{Kind: "close", Code: wire.CloseNormal})
		for p := range q.sockets {
			if p.role == wire.RolePhone {
				r.closeLocked(p, wire.CloseRefused)
			}
		}
		for _, req := range q.pending {
			if req.timer != nil {
				req.timer()
			}
			req.out <- Result{Kind: "http", Status: 503}
		}
		q.pending = map[wire.SID]*request{}
		q.macKey = nil
		q.devices = map[string]wire.Device{}
	default:
		r.closeLocked(s, wire.CloseUnsupported)
	}
}
func (r *Relay) peerDownLocked(q *room, req *request) {
	if q.mac != nil {
		q.mac.push(Result{Kind: "text", Data: wire.Encode(wire.Peer{SID: req.sid, Role: wire.RoleWatch, Key: req.key, Up: false, OneShot: true})})
	}
}
func (r *Relay) authLocked(s *Socket, kind string, data []byte) {
	q := s.room
	if kind != "text" {
		r.closeLocked(s, wire.CloseRefused)
		return
	}
	if s.badTag {
		auth.Verify(nil, false, auth.AuthMessage(q.id, wire.RoleMac, s.challenge[:]), make([]byte, 64))
		r.closeLocked(s, wire.CloseRefused)
		return
	}
	keys := roomKeys{q}
	a, e := auth.CheckAuth(r.cfg.ProvisionKey, q.id, s.challenge[:], data, keys)
	if e != nil {
		r.closeLocked(s, wire.CloseRefused)
		return
	}
	s.timeout()
	s.role = a.Role
	s.key = append(ed25519.PublicKey(nil), a.Key...)
	s.authed = true
	if a.Provision {
		q.macKey = append(ed25519.PublicKey(nil), a.Key...)
	}
	if a.Role == wire.RoleMac {
		if q.mac != nil && q.mac != s {
			old := q.mac
			r.closeLocked(old, wire.CloseSuperseded)
		}
		q.mac = s
		peers := []wire.PeerInfo{}
		for p := range q.sockets {
			if p.authed && p.role == wire.RolePhone {
				peers = append(peers, wire.PeerInfo{SID: p.sid, Role: p.role, Key: p.key})
				p.knownByMac = true
			}
		}
		sort.Slice(peers, func(i, j int) bool { return peers[i].SID.String() < peers[j].SID.String() })
		s.push(Result{Kind: "text", Data: wire.Encode(wire.OKMac{Peers: peers})})
		return
	}
	n := 0
	for p := range q.sockets {
		if p.authed && p.role == wire.RolePhone {
			n++
		}
	}
	if n > wire.MaxPhones {
		r.closeLocked(s, wire.CloseFull)
		return
	}
	if !r.random(s.sid[:]) {
		r.closeLocked(s, wire.CloseRefused)
		return
	}
	if q.mac == nil {
		s.push(Result{Kind: "text", Data: wire.Encode(wire.OKPhone{SID: s.sid})})
		return
	}
	ack := make(chan bool, 1)
	s.knownByMac = true
	s.pendingOK = true
	q.phoneAcks[s.sid] = s
	q.mac.acks[s.sid] = ack
	q.mac.push(Result{Kind: "text", Data: wire.Encode(wire.Peer{SID: s.sid, Role: wire.RolePhone, Key: s.key, Up: true})})
	timer := r.clock.AfterFunc(wire.PeerAckWait, func() {
		r.mu.Lock()
		defer r.mu.Unlock()
		if s.closed {
			return
		}
		if _, ok := q.phoneAcks[s.sid]; !ok {
			return
		}
		delete(q.phoneAcks, s.sid)
		s.push(Result{Kind: "text", Data: wire.Encode(wire.OKPhone{SID: s.sid, Peer: false})})
		if q.mac != nil {
			r.closeLocked(q.mac, wire.CloseMacGone)
		}
	})
	_ = timer
}

type roomKeys struct{ q *room }

func (k roomKeys) MacKey() ed25519.PublicKey { return k.q.macKey }
func (k roomKeys) DeviceRole(pk ed25519.PublicKey) (wire.Role, bool) {
	d, ok := k.q.devices[string(pk)]
	return d.Role, ok
}
func (r *Relay) devicesLocked(s *Socket, c wire.DevicesChange) {
	q := s.room
	next := map[string]wire.Device{}
	for k, v := range q.devices {
		next[k] = v
	}
	for _, d := range c.Add {
		next[string(d.Key)] = d
	}
	for _, k := range c.Remove {
		delete(next, string(k))
	}
	if len(next) > wire.MaxDevices {
		r.closeLocked(s, wire.CloseUnsupported)
		return
	}
	for p := range q.sockets {
		if p.authed && p.role == wire.RolePhone {
			d, ok := next[string(p.key)]
			old := q.devices[string(p.key)]
			if !ok || d.Role != old.Role {
				r.closeLocked(p, wire.CloseRefused)
			}
		}
	}
	for sid, req := range q.pending {
		d, ok := next[string(req.key)]
		if !ok || d.Role != wire.RoleWatch {
			delete(q.pending, sid)
			delete(s.acks, sid)
			if req.timer != nil {
				req.timer()
			}
			req.out <- Result{Kind: "http", Status: 503}
			r.peerDownLocked(q, req)
		}
	}
	q.devices = next
	ds := make([]wire.Device, 0, len(next))
	for _, d := range next {
		ds = append(ds, d)
	}
	s.push(Result{Kind: "text", Data: wire.Encode(wire.DevicesList{Keys: ds})})
}
func (r *Relay) Devices(id wire.RoomID) []wire.Device {
	r.mu.Lock()
	defer r.mu.Unlock()
	q := r.getRoom(id, false)
	out := []wire.Device{}
	if q != nil {
		for _, d := range q.devices {
			out = append(out, d)
		}
	}
	sort.Slice(out, func(i, j int) bool { return wire.B64(out[i].Key) < wire.B64(out[j].Key) })
	return out
}
func (r *Relay) MacKey(id wire.RoomID) ed25519.PublicKey {
	r.mu.Lock()
	defer r.mu.Unlock()
	q := r.getRoom(id, false)
	if q == nil {
		return nil
	}
	return append(ed25519.PublicKey(nil), q.macKey...)
}
func (r *Relay) MacConnected(id wire.RoomID) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	q := r.getRoom(id, false)
	return q != nil && q.mac != nil && !q.mac.closed
}
func (r *Relay) CloseMac(id wire.RoomID, code int) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if q := r.getRoom(id, false); q != nil && q.mac != nil {
		r.closeLocked(q.mac, code)
	}
}
func (r *Relay) Evict() {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, q := range r.rooms {
		devices := q.devices
		nonces := q.nonces
		sockets := q.sockets
		pending := q.pending
		q.devices = map[string]wire.Device{}
		q.nonces = map[[16]byte]time.Time{}
		q.sockets = map[*Socket]bool{}
		q.watchBuckets = map[string]*auth.Bucket{}
		q.pending = map[wire.SID]*request{}
		q.phoneAcks = map[wire.SID]*Socket{}
		q.mac = nil
		for k, v := range devices {
			q.devices[k] = v
		}
		for n, expiry := range nonces {
			q.nonces[n] = expiry
		}
		for s := range sockets {
			if s.closed {
				continue
			}
			q.sockets[s] = true
			if s.authed && s.role == wire.RoleMac {
				q.mac = s
			}
		}
		if q.mac != nil {
			q.mac.acks = map[wire.SID]chan bool{}
		}
		for s := range q.sockets {
			if s.authed && s.role == wire.RolePhone && s.pendingOK {
				q.phoneAcks[s.sid] = s
				if q.mac != nil {
					q.mac.acks[s.sid] = make(chan bool, 1)
				}
			}
		}
		for _, req := range pending {
			if req.timer != nil {
				req.timer()
			}
			req.out <- Result{Kind: "http", Status: 503}
		}
		q.lastSeen = r.clock.Now()
	}
}
func (r *Relay) Handler() http.Handler { return http.HandlerFunc(r.serveHTTP) }
func addrKey(s string) string {
	host, _, e := net.SplitHostPort(s)
	if e == nil {
		s = host
	}
	ip := net.ParseIP(s)
	if v := ip.To4(); v != nil {
		return v.String()
	}
	if v := ip.To16(); v != nil {
		return net.IP(v[:8]).String() + "/64"
	}
	return s
}
func (r *Relay) rate(addr string) bool {
	b := r.addresses[addr]
	if b == nil {
		b = &auth.Bucket{Rate: 1, Burst: 60}
		r.addresses[addr] = b
	}
	return b.Allow(r.clock.Now())
}

type route struct {
	kind   string
	id     wire.RoomID
	tagged bool
	status int
}

func (r *Relay) workerRoute(method, path, rawQuery, roomHeader string, upgrade bool) route {
	if rawQuery != "" {
		return route{status: 404}
	}
	id, err := wire.ParseRoom(roomHeader)
	if err != nil {
		return route{status: 404}
	}
	valid := auth.CheckRoom(r.cfg.RoomTagKey, id)
	if method == http.MethodGet && path == wire.PathWS && upgrade {
		return route{kind: "ws", id: id, tagged: valid, status: 101}
	}
	if method == http.MethodPost && path == wire.PathCmd && valid {
		return route{kind: "cmd", id: id, tagged: true, status: 200}
	}
	return route{status: 404}
}
func (r *Relay) serveHTTP(w http.ResponseWriter, req *http.Request) {
	r.mu.Lock()
	ok := r.rate(addrKey(req.RemoteAddr))
	r.mu.Unlock()
	if !ok {
		w.WriteHeader(429)
		return
	}
	rt := r.workerRoute(req.Method, req.URL.Path, req.URL.RawQuery, req.Header.Get(wire.Header), strings.EqualFold(req.Header.Get("Upgrade"), "websocket"))
	if req.URL.ForceQuery {
		rt = route{status: 404}
	}
	if rt.kind == "ws" {
		s, yes := r.ConnectTagged(rt.id, rt.tagged)
		if !yes {
			w.WriteHeader(429)
			return
		}
		c, e := websocketAccept(w, req)
		if e != nil {
			return
		}
		go r.serveSocket(c, s)
		return
	}
	if rt.kind == "cmd" {
		body, e := io.ReadAll(io.LimitReader(req.Body, wire.MaxFrame+1))
		if e != nil || len(body) > wire.MaxFrame {
			write404(w)
			return
		}
		res := r.post(rt.id, body)
		if res.Status == 0 {
			write404(w)
			return
		}
		for k, v := range res.Header {
			w.Header()[k] = v
		}
		w.WriteHeader(res.Status)
		_, _ = w.Write(res.Data)
		return
	}
	write404(w)
}
func write404(w http.ResponseWriter) { w.WriteHeader(http.StatusNotFound) }
func websocketAccept(w http.ResponseWriter, r *http.Request) (*websocket.Conn, error) {
	return websocket.Accept(w, r, &websocket.AcceptOptions{InsecureSkipVerify: true})
}
func (r *Relay) serveSocket(c *websocket.Conn, s *Socket) {
	ctx := context.Background()
	done := make(chan struct{})
	go func() {
		for {
			select {
			case <-done:
				return
			case x := <-s.out:
				if x.Kind == "close" {
					_ = c.Close(websocket.StatusCode(x.Code), "")
					return
				}
				if x.Kind == "text" || x.Kind == "binary" {
					typ := websocket.MessageText
					if x.Kind == "binary" {
						typ = websocket.MessageBinary
					}
					if c.Write(ctx, typ, x.Data) != nil {
						return
					}
				}
			}
		}
	}()
	for {
		typ, b, e := c.Read(ctx)
		if e != nil {
			break
		}
		kind := "text"
		if typ == websocket.MessageBinary {
			kind = "binary"
		}
		s.Receive(kind, b)
	}
	s.r.mu.Lock()
	s.r.closeLocked(s, wire.CloseNormal)
	s.r.mu.Unlock()
	close(done)
	_ = c.Close(websocket.StatusNormalClosure, "")
}

func (r *Relay) post(id wire.RoomID, body []byte) Result {
	return <-r.startPost(id, body)
}
func (r *Relay) startPost(id wire.RoomID, body []byte) chan Result {
	out := make(chan Result, 1)
	r.mu.Lock()
	q := r.getRoom(id, true)
	checked, e := auth.CheckRequest(id, body, roomKeys{q}, q, roomBucket{q}, r.clock.Now())
	if e != nil {
		r.mu.Unlock()
		if e == auth.ErrRate {
			out <- Result{Status: 429}
			return out
		}
		out <- Result{Status: 404}
		return out
	}
	if q.mac == nil {
		r.mu.Unlock()
		out <- Result{Status: 503}
		return out
	}
	if len(q.pending) >= wire.MaxPending {
		r.mu.Unlock()
		out <- Result{Status: 429}
		return out
	}
	sid := wire.SID{}
	for {
		if !r.random(sid[:]) {
			r.mu.Unlock()
			out <- Result{Status: 503}
			return out
		}
		if q.pending[sid] == nil {
			break
		}
	}
	ack := make(chan bool, 1)
	req := &request{sid: sid, key: checked.Key, body: append([]byte(nil), body...), out: out, ack: ack}
	q.pending[sid] = req
	q.mac.acks[sid] = ack
	q.mac.push(Result{Kind: "text", Data: wire.Encode(wire.Peer{SID: sid, Role: wire.RoleWatch, Key: checked.Key, Up: true, OneShot: true})})
	req.timer = r.clock.AfterFunc(wire.PeerAckWait, func() {
		r.mu.Lock()
		defer r.mu.Unlock()
		if q.pending[sid] != req {
			return
		}
		delete(q.pending, sid)
		delete(q.mac.acks, sid)
		req.out <- Result{Kind: "http", Status: 503}
		if q.mac != nil {
			r.closeLocked(q.mac, wire.CloseMacGone)
		}
	})
	r.mu.Unlock()
	return out
}

type roomBucket struct{ q *room }

func (l roomBucket) Allow(key ed25519.PublicKey, now time.Time) bool {
	k := string(key)
	b := l.q.watchBuckets[k]
	if b == nil {
		b = &auth.Bucket{Rate: 2, Burst: 10}
		l.q.watchBuckets[k] = b
	}
	return b.Allow(now)
}
func (q *room) Has(n [wire.NonceLen]byte, now time.Time) bool {
	e, ok := q.nonces[n]
	return ok && now.Before(e)
}
func (q *room) Add(n [wire.NonceLen]byte, e time.Time) { q.nonces[n] = e }
