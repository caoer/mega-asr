package remote

import (
	"bytes"
	"context"
	"crypto/ecdh"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/coder/websocket"

	"github.com/caoer/mega-asr/internal/l10n"
	"github.com/caoer/mega-asr/internal/relay/auth"
	"github.com/caoer/mega-asr/internal/relay/e2e"
	"github.com/caoer/mega-asr/internal/relay/wire"
)

// Link is the Mac's connection to its relay room (internal/relay/client).
// Send and Drop queue a frame and never block; Devices waits for the room's
// answer.
type Link interface {
	Send(sid wire.SID, data []byte) error
	Drop(sid wire.SID)
	Devices(ctx context.Context, change wire.DevicesChange) ([]wire.Device, error)
	// Switch moves the link to another room under another key: the old
	// socket closes and the new room is provisioned.
	Switch(room wire.RoomID, key ed25519.PrivateKey)
	Status() (online bool, why string)
}

// Identity is the Mac's side of the relay: its room id, its recipient key
// for the end-to-end channel and its signing key (PROTOCOL.md § 10).
type Identity struct {
	Room wire.RoomID
	Kem  *ecdh.PrivateKey
	Sig  ed25519.PrivateKey
}

// The identity's files in the server's state directory.
const (
	roomFile = "relay-room"
	kemFile  = "relay-kem.key"
	sigFile  = "relay-sig.key"
)

// maxRelayConns is how many relay sessions run at once: the room's phone
// sockets, counted apart from direct connections (maxConns).
const maxRelayConns = wire.MaxPhones

// LoadIdentity reads the relay identity from dir, minting one on first use.
// A room id whose tag does not verify under roomTagKey (the key was
// rotated) is replaced by a new one under the same keys.
func LoadIdentity(dir string, roomTagKey []byte) (Identity, error) {
	room, rerr := os.ReadFile(filepath.Join(dir, roomFile))
	kem, kerr := os.ReadFile(filepath.Join(dir, kemFile))
	sig, serr := os.ReadFile(filepath.Join(dir, sigFile))
	if errors.Is(rerr, os.ErrNotExist) && errors.Is(kerr, os.ErrNotExist) && errors.Is(serr, os.ErrNotExist) {
		id, err := NewIdentity(roomTagKey)
		if err != nil {
			return id, err
		}
		return id, id.save(dir)
	}
	for _, err := range []error{rerr, kerr, serr} {
		if err != nil {
			return Identity{}, fmt.Errorf("relay identity: %w", err)
		}
	}
	var id Identity
	k := auth.ParseSecret(string(kem))
	s := auth.ParseSecret(string(sig))
	if k == nil || s == nil {
		return id, fmt.Errorf("relay identity: %s or %s is not 64 hex characters", kemFile, sigFile)
	}
	var err error
	if id.Kem, err = ecdh.X25519().NewPrivateKey(k); err != nil {
		return id, fmt.Errorf("relay identity: %s: %w", kemFile, err)
	}
	id.Sig = ed25519.NewKeyFromSeed(s)
	id.Room, err = wire.ParseRoom(strings.TrimSpace(string(room)))
	if err != nil || !auth.CheckRoom(roomTagKey, id.Room) {
		if id.Room, err = auth.MintRoom(roomTagKey, rand.Reader); err != nil {
			return id, err
		}
		if err := writeFile(filepath.Join(dir, roomFile), []byte(id.Room.String()+"\n"), 0o600); err != nil {
			return id, err
		}
	}
	return id, nil
}

// NewIdentity mints a room id and fresh keys.
func NewIdentity(roomTagKey []byte) (Identity, error) {
	var id Identity
	var err error
	if id.Room, err = auth.MintRoom(roomTagKey, rand.Reader); err != nil {
		return id, err
	}
	if id.Kem, err = ecdh.X25519().GenerateKey(rand.Reader); err != nil {
		return id, err
	}
	_, id.Sig, err = ed25519.GenerateKey(rand.Reader)
	return id, err
}

// save writes the identity's three files. All three are written beside
// their names first and renamed only once every write succeeded, so a write
// that fails (a full disk) leaves the old identity whole.
func (id Identity) save(dir string) error {
	files := []struct {
		name string
		b    []byte
	}{
		{roomFile, []byte(id.Room.String() + "\n")},
		{kemFile, []byte(hex.EncodeToString(id.Kem.Bytes()) + "\n")},
		{sigFile, []byte(hex.EncodeToString(id.Sig.Seed()) + "\n")},
	}
	tmps := make([]string, 0, len(files))
	defer func() {
		for _, t := range tmps {
			os.Remove(t) // a renamed one is gone already
		}
	}()
	for _, f := range files {
		t, err := stage(filepath.Join(dir, f.name), f.b, 0o600)
		if err != nil {
			return err
		}
		tmps = append(tmps, t)
	}
	for i, f := range files {
		if err := os.Rename(tmps[i], filepath.Join(dir, f.name)); err != nil {
			return err
		}
	}
	return nil
}

// Relay is what the server needs to serve phones and watches through the
// relay.
type Relay struct {
	URL        string // https://…, what phones and watches are told
	RoomTagKey []byte // to mint a room on rotation
	Identity   Identity
	Link       Link
}

// relaySide is the server's state for the relay: the sessions the room told
// it of, by sid.
type relaySide struct {
	Relay
	guard *e2e.ReplayGuard
	sync  chan struct{} // a reconcile of the room's devices is wanted
	rotMu sync.Mutex    // one rotation at a time

	mu    sync.Mutex
	id    Identity
	peers map[wire.SID]*relayPeer
}

// relayPeer is a session the room announced: awaiting its handshake, being
// handshaken (busy), or open (sess set).
type relayPeer struct {
	deadline time.Time
	oneshot  bool
	busy     bool
	sess     *relaySession
}

// AttachRelay serves phones and watches through the relay's link until ctx
// ends; call it before the link runs. The link reports to the server
// through LinkUp, LinkDown, Peer and Frame.
func (s *Server) AttachRelay(ctx context.Context, r Relay) {
	rs := &relaySide{Relay: r, id: r.Identity, guard: e2e.NewReplayGuard(s.now()), sync: make(chan struct{}, 1), peers: map[wire.SID]*relayPeer{}}
	s.relay.Store(rs)
	go s.reconcileLoop(ctx, rs)
}

func (s *Server) relaySide() *relaySide { return s.relay.Load() }

func (r *relaySide) identity() Identity {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.id
}

// relayOffer is the current relay destination sent on an authenticated direct connection.
func (s *Server) relayOffer() (relayMsg, bool) {
	r := s.relaySide()
	if r == nil {
		return relayMsg{}, false
	}
	id := r.identity()
	return relayMsg{Type: "relay", URL: r.URL, Room: id.Room.String(), MacKem: wire.B64(id.Kem.PublicKey().Bytes())}, true
}

// LinkUp is the link authenticated in its room. Phone sessions the room
// lists were opened before this socket, so their handshakes went nowhere:
// they are dropped, and the phones reconnect.
func (s *Server) LinkUp(peers []wire.PeerInfo) {
	r := s.relaySide()
	if r == nil {
		return
	}
	s.logf("remote: relay link up in room %s…", r.identity().Room.String()[:4])
	for _, p := range peers {
		r.Link.Drop(p.SID)
	}
	s.syncDevices()
}

// LinkDown is the link gone: every session it carried is gone with it.
func (s *Server) LinkDown() {
	r := s.relaySide()
	if r == nil {
		return
	}
	r.mu.Lock()
	peers := r.peers
	r.peers = map[wire.SID]*relayPeer{}
	r.mu.Unlock()
	for _, p := range peers {
		if p.sess != nil {
			p.sess.ended()
		}
	}
}

// Peer is the room's word that a session came or went. The role is the
// session's kind — a one-shot request is a watch, a socket a phone — never
// the frame's role field (PROTOCOL.md § 7.2).
func (s *Server) Peer(p wire.Peer) {
	r := s.relaySide()
	if r == nil {
		return
	}
	r.mu.Lock()
	held := r.peers[p.SID]
	if !p.Up {
		delete(r.peers, p.SID)
		r.mu.Unlock()
		if held != nil && held.sess != nil {
			held.sess.ended()
		}
		return
	}
	if held != nil {
		// a sid the Mac holds announced again: a second handshake on it
		// tears the session (PROTOCOL.md § 7.3)
		delete(r.peers, p.SID)
		r.mu.Unlock()
		s.logf("remote: relay session %s… announced twice; torn down", p.SID.String()[:4])
		if held.sess != nil {
			held.sess.tear()
		} else {
			r.Link.Drop(p.SID)
		}
		return
	}
	waiting := 0
	for _, q := range r.peers {
		if q.sess == nil {
			waiting++
		}
	}
	if waiting >= wire.MaxHandshake {
		r.mu.Unlock()
		s.logf("remote: relay session refused: %d awaiting a handshake", waiting)
		r.Link.Drop(p.SID)
		return
	}
	r.peers[p.SID] = &relayPeer{oneshot: p.OneShot, deadline: time.Now().Add(wire.ReplyWait)}
	r.mu.Unlock()
}

// Frame is a binary frame from the room for one session: the handshake if
// it is the session's first, else a record.
func (s *Server) Frame(sid wire.SID, data []byte) {
	r := s.relaySide()
	if r == nil {
		return
	}
	r.mu.Lock()
	held := r.peers[sid]
	switch {
	case held == nil:
		r.mu.Unlock()
		return // a session the Mac does not hold
	case held.sess != nil:
		r.mu.Unlock()
		held.sess.deliver(data)
		return
	case held.busy:
		// a frame before the Mac answered the handshake: a second handshake
		delete(r.peers, sid)
		r.mu.Unlock()
		r.Link.Drop(sid)
		return
	}
	held.busy = true
	r.mu.Unlock()
	if held.oneshot {
		go s.watchRequest(r, sid, data)
	} else {
		go s.phoneHandshake(r, sid, held, data)
	}
}

// forget removes sid if it still names held.
func (r *relaySide) forget(sid wire.SID, held *relayPeer) {
	r.mu.Lock()
	if r.peers[sid] == held {
		delete(r.peers, sid)
	}
	r.mu.Unlock()
}

// phoneHandshake answers a phone's first message through the relay
// (PROTOCOL.md § 7.3, § 8): it opens under this room, role phone and the
// sid, and its signing key must be a phone's in phones.toml; then the hello
// is answered and the session runs like a direct one. A message that does
// not open or verify gets no answer, only drop. The hello acts on nothing.
func (s *Server) phoneHandshake(r *relaySide, sid wire.SID, held *relayPeer, msg []byte) {
	id := r.identity()
	o, err := e2e.Open(id.Kem, e2e.Phone(id.Room, sid), msg)
	if err != nil {
		s.logf("remote: relay session %s…: handshake refused: %v", sid.String()[:4], err)
		r.forget(sid, held)
		r.Link.Drop(sid)
		return
	}
	refuse := func(rf refusedMsg) {
		if out, _, err := o.Reply(encode(rf)); err == nil {
			_ = r.Link.Send(sid, out)
		}
		r.forget(sid, held)
		r.Link.Drop(sid)
	}
	s.mu.Lock()
	p := s.byKey(o.DeviceKey, false)
	s.mu.Unlock()
	if p == nil {
		s.logf("remote: relay session %s…: refused: a phone key this Mac does not know", sid.String()[:4])
		refuse(refusedMsg{"refused", "token", reasonToken()})
		return
	}
	var m inMsg
	if json.Unmarshal(o.First, &m) != nil || m.Type != "hello" {
		s.logf("remote: relay session %s… from %q: refused: its first message is not hello", sid.String()[:4], p.Name)
		refuse(refusedMsg{"refused", "bad_request", reasonFirst()})
		return
	}
	// The phone's check, the slot and the answer are one hold of s.mu, so a
	// Revoke either finds this session among the phone's connections or the
	// session finds the phone gone, and every tracked conn has its session.
	// Relay sessions are counted apart from direct connections: a relay that
	// replays recorded handshakes fills only its own slots, never the LAN
	// path's.
	st, recent := s.state(), s.recent()
	var c *conn
	var rs *relaySession
	var out []byte
	var refused *refusedMsg
	s.mu.Lock()
	switch {
	case s.phone(p.Name) != p:
		refused = &refusedMsg{"refused", "token", reasonToken()}
	case s.count(true) >= maxRelayConns:
		refused = &refusedMsg{"refused", "busy", reasonBusy()}
	default:
		var keys *e2e.Session
		out, keys, err = o.Reply(encode(helloMsg{Type: "hello", Mac: s.macName(), State: st, Recent: recent}))
		if err == nil {
			rs = newRelaySession(r.Link, sid, keys)
			// held until the hello is out: a revoke meanwhile seals its
			// refusal after the hello, where the phone can open it
			rs.sealMu.Lock()
			c = newConn(rs, true)
			c.phone = p
			s.conns[c] = struct{}{}
		}
	}
	s.mu.Unlock()
	if refused != nil {
		s.logf("remote: relay session from %q refused: %s", p.Name, refused.Code)
		refuse(*refused)
		return
	}
	if err != nil {
		r.forget(sid, held)
		r.Link.Drop(sid)
		return
	}
	r.mu.Lock()
	installed := r.peers[sid] == held
	if installed {
		held.sess = rs // records arriving from now on queue on the session
	}
	r.mu.Unlock()
	sent := installed && r.Link.Send(sid, out) == nil
	rs.sealMu.Unlock()
	if !sent {
		rs.ended() // gone meanwhile: nothing more goes out
		s.untrack(c)
		return
	}
	s.logf("remote: %q connected through the relay (%s)", p.Name, rs.Addr())
	ctx, cancel := context.WithCancel(context.Background())
	go func() { <-rs.gone; cancel() }()
	go c.writer()
	go func() {
		defer s.untrack(c)
		s.loop(ctx, c)
	}()
}

// watchRequest answers one watch request (PROTOCOL.md § 6.3): its signature
// under a watch key in phones.toml, its time, its nonce, then its handshake;
// its first message is the command, acted on once by id, and the reply is
// the command's reply. A request refused gets no answer, only drop.
func (s *Server) watchRequest(r *relaySide, sid wire.SID, body []byte) {
	id := r.identity()
	r.mu.Lock()
	held := r.peers[sid]
	r.mu.Unlock()
	live := func() bool {
		r.mu.Lock()
		defer r.mu.Unlock()
		return held != nil && r.peers[sid] == held && time.Now().Before(held.deadline)
	}
	isWatch := func(k ed25519.PublicKey) bool {
		s.mu.Lock()
		defer s.mu.Unlock()
		return s.byKey(k, true) != nil
	}
	defer func() {
		r.mu.Lock()
		delete(r.peers, sid)
		r.mu.Unlock()
	}()
	o, req, err := e2e.OpenRequest(id.Kem, id.Room, body, isWatch, r.guard, s.now())
	if err != nil {
		s.logf("remote: relay request %s…: refused: %v", sid.String()[:4], err)
		r.Link.Drop(sid)
		return
	}
	s.mu.Lock()
	from := "?"
	if p := s.byKey(req.Key, true); p != nil {
		from = p.Name
	}
	s.mu.Unlock()
	var m inMsg
	var rep reply
	if json.Unmarshal(o.First, &m) != nil || m.Type != "cmd" {
		rep = s.reply(reply{Type: "reply", ID: m.ID, Code: "bad_request", Reason: l10n.T("不认识的消息：", "Unknown message: ") + m.Type})
	} else {
		m.Via = "watch" // the session's kind, whatever the message says
		rep = s.commandWhile(nil, from+" (watch, relay)", m, live)
	}
	out, _, err := o.Reply(encode(rep))
	if err == nil {
		err = r.Link.Send(sid, out)
	}
	if err != nil {
		s.logf("remote: relay request %s…: reply: %v", sid.String()[:4], err)
	}
}

// relayKey takes a device's relay key (PROTOCOL.md § 8): the phone's once,
// on a direct connection. A hello can register an empty slot or confirm
// the same key; only a pairing may replace a registered key. The watch's
// key also rides a relay session. A key already registered for another
// device, or the Mac's own, is refused.
func (s *Server) relayKey(c *conn, m inMsg) any {
	refuse := func(reason string) any {
		s.logf("remote: relay key (%s) from %q (%s) refused: %s", m.Role, c.name(), c.sess.Addr(), reason)
		return s.reply(reply{Type: "reply", ID: m.ID, Code: "bad_request", Reason: reason})
	}
	r := s.relaySide()
	if r == nil {
		return refuse(l10n.T("这台 Mac 没有开启中继", "This Mac has the relay off"))
	}
	key, err := wire.Unb64(m.Sig, ed25519.PublicKeySize)
	if err != nil {
		return refuse(l10n.T("中继钥匙格式不对", "Bad relay key format"))
	}
	var watch bool
	switch m.Role {
	case "phone":
		if c.relay || c.keyDone {
			return refuse(l10n.T("iPhone 的中继钥匙只能在扫码配对的那次连接上登记一次", "An iPhone's relay key can be registered once, on the connection that paired by QR code"))
		}
	case "watch":
		watch = true
	default:
		return refuse(l10n.T("role 只能是 phone 或 watch", "role must be phone or watch"))
	}
	if bytes.Equal(key, r.identity().Sig.Public().(ed25519.PublicKey)) {
		return refuse(l10n.T("这把钥匙已经登记给别的设备", "This key is registered to another device"))
	}
	s.mu.Lock()
	p := c.phone
	if p == nil || s.phone(p.Name) != p {
		s.mu.Unlock()
		return refuse(l10n.T("这台 iPhone 已不在配对列表里", "This iPhone is no longer paired"))
	}
	if s.keyTaken(key, p, watch) {
		s.mu.Unlock()
		return refuse(l10n.T("这把钥匙已经登记给别的设备", "This key is registered to another device"))
	}
	registered := p.SigPK
	if watch {
		registered = p.WatchSigPK
	}
	if !c.relay && !c.paired && registered != "" && registered != wire.B64(key) {
		s.mu.Unlock()
		return refuse(l10n.T("这把中继钥匙与配对记录不同，请重新扫码配对", "This relay key differs from the pairing record; scan to pair again"))
	}
	keys := map[string]bool{}
	for _, phone := range s.phones {
		for _, k := range []string{phone.SigPK, phone.WatchSigPK} {
			if k != "" && k != registered {
				keys[k] = true
			}
		}
	}
	keys[wire.B64(key)] = true
	if len(keys) > wire.MaxDevices {
		s.mu.Unlock()
		return refuse(l10n.T("中继设备已满，请先移除不用的配对", "The relay is full; remove a pairing you no longer use"))
	}
	old := *p
	if watch {
		p.WatchSigPK = wire.B64(key)
	} else {
		p.SigPK = wire.B64(key)
	}
	if err := s.save(); err != nil {
		*p = old
		s.mu.Unlock()
		return refuse(l10n.T("Mac 存不下中继钥匙：", "The Mac can't save the relay key: ") + err.Error())
	}
	s.mu.Unlock()
	if !watch {
		c.keyDone = true
	}
	s.logf("remote: relay key (%s) of %q registered", m.Role, p.Name)
	s.syncDevices()
	return relayOKMsg{Type: "relay_ok", Role: m.Role}
}

// syncDevices asks for a reconcile of the room's devices; it never blocks.
func (s *Server) syncDevices() {
	r := s.relaySide()
	if r == nil {
		return
	}
	select {
	case r.sync <- struct{}{}:
	default:
	}
}

func (s *Server) reconcileLoop(ctx context.Context, r *relaySide) {
	for {
		select {
		case <-ctx.Done():
			return
		case <-r.sync:
		}
		if on, _ := r.Link.Status(); !on {
			continue // LinkUp asks again
		}
		if err := s.reconcile(ctx, r); err != nil {
			s.logf("remote: relay devices: %v; again at the next connect or change", err)
		}
	}
}

// reconcile makes the room's devices those of phones.toml (PROTOCOL.md
// § 10): the room's list, then the keys it should not have removed, then
// the keys it lacks added.
func (s *Server) reconcile(ctx context.Context, r *relaySide) error {
	want := map[string]wire.Role{}
	s.mu.Lock()
	phones := append([]*phone(nil), s.phones...)
	sort.SliceStable(phones, func(i, j int) bool {
		if !phones[i].LastSeen.Equal(phones[j].LastSeen) {
			return phones[i].LastSeen.After(phones[j].LastSeen)
		}
		return phones[i].Name < phones[j].Name
	})
	overflow := 0
	for _, p := range phones {
		if p.SigPK != "" && len(want) < wire.MaxDevices {
			want[p.SigPK] = wire.RolePhone
		} else if p.SigPK != "" {
			overflow++
		}
		if p.WatchSigPK != "" && len(want) < wire.MaxDevices {
			want[p.WatchSigPK] = wire.RoleWatch
		} else if p.WatchSigPK != "" {
			overflow++
		}
	}
	s.mu.Unlock()
	if overflow > 0 {
		s.logf("remote: relay device limit %d: %d older keys omitted; remove unused pairings", wire.MaxDevices, overflow)
	}
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	have, err := r.Link.Devices(ctx, wire.DevicesChange{Add: []wire.Device{}, Remove: []ed25519.PublicKey{}})
	if err != nil {
		return err
	}
	got := map[string]wire.Role{}
	var remove []ed25519.PublicKey
	for _, d := range have {
		k := wire.B64(d.Key)
		if want[k] == d.Role {
			got[k] = d.Role
			continue
		}
		remove = append(remove, d.Key)
	}
	var add []wire.Device
	for k, role := range want {
		if _, ok := got[k]; ok {
			continue
		}
		key, err := wire.Unb64(k, ed25519.PublicKeySize)
		if err != nil {
			continue
		}
		add = append(add, wire.Device{Role: role, Key: key})
	}
	sort.Slice(add, func(i, j int) bool { return wire.B64(add[i].Key) < wire.B64(add[j].Key) })
	if len(remove) > 0 {
		if _, err := r.Link.Devices(ctx, wire.DevicesChange{Add: []wire.Device{}, Remove: remove}); err != nil {
			return err
		}
	}
	if len(add) > 0 {
		if _, err := r.Link.Devices(ctx, wire.DevicesChange{Add: add, Remove: []ed25519.PublicKey{}}); err != nil {
			return err
		}
	}
	if len(remove)+len(add) > 0 {
		s.logf("remote: relay devices: %d removed, %d added", len(remove), len(add))
	}
	return nil
}

// RotateRelay gives the Mac a new room and new keys (`megavoice remote
// rotate`): the old room is abandoned, every relay session ends with bye
// 1008 and every phone's and watch's relay key is forgotten, so each phone
// pairs again to reach the new room. Direct pairings keep working. It
// returns the new room's first 4 characters.
func (s *Server) RotateRelay() (string, error) {
	r := s.relaySide()
	if r == nil {
		return "", errors.New("the relay is off")
	}
	r.rotMu.Lock()
	defer r.rotMu.Unlock()
	id, err := NewIdentity(r.RoomTagKey)
	if err != nil {
		return "", err
	}
	if err := id.save(s.cfg.State); err != nil {
		return "", err
	}
	s.mu.Lock()
	for _, p := range s.phones {
		p.SigPK, p.WatchSigPK = "", ""
	}
	err = s.save()
	for c := range s.conns {
		if c.relay {
			c.close(websocket.StatusPolicyViolation, "rotated", nil)
		}
	}
	s.mu.Unlock()
	r.mu.Lock()
	r.id = id
	peers := r.peers
	r.peers = map[wire.SID]*relayPeer{}
	r.mu.Unlock()
	for _, p := range peers {
		if p.sess != nil {
			p.sess.ended()
		}
	}
	r.Link.Switch(id.Room, id.Sig)
	s.logf("remote: relay rotated to room %s…; phones pair again to use it", id.Room.String()[:4])
	return id.Room.String()[:4], err
}

// RelayView is the relay's state on the pairing page.
type RelayView struct {
	Enabled bool   `json:"enabled"`
	Online  bool   `json:"online"`
	Room    string `json:"room_prefix,omitempty"` // the room id's first 4 characters
	Why     string `json:"why,omitempty"`         // why it is not online
}

// RelayFailed says why the relay that [remote.relay] turns on is not
// running, for the pairing page.
func (s *Server) RelayFailed(why string) { s.relayWhy.Store(&why) }

func (s *Server) relayView() *RelayView {
	r := s.relaySide()
	if r == nil {
		if why := s.relayWhy.Load(); why != nil {
			return &RelayView{Enabled: true, Why: *why}
		}
		return &RelayView{}
	}
	on, why := r.Link.Status()
	return &RelayView{Enabled: true, Online: on, Room: r.identity().Room.String()[:4], Why: why}
}
