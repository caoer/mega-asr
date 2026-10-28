package fakeroom

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/caoer/mega-asr/internal/relay/auth"
	"github.com/caoer/mega-asr/internal/relay/conformance"
	"github.com/caoer/mega-asr/internal/relay/wire"
	"github.com/coder/websocket"
)

type scriptRunner struct {
	t         *testing.T
	env       *conformance.Env
	r         *Relay
	clock     *VirtualClock
	conns     map[string]*Socket
	connRooms map[string]string
	http      map[string]chan Result
	vars      map[string]any
	bodies    map[string][]byte
	addr      string
	script    *conformance.Script
}

type deterministicReader struct {
	seed  []byte
	n     uint64
	block []byte
}

func (d *deterministicReader) Read(p []byte) (int, error) {
	written := 0
	for written < len(p) {
		if len(d.block) == 0 {
			h := sha256.New()
			h.Write(d.seed)
			var b [8]byte
			binary.BigEndian.PutUint64(b[:], d.n)
			d.n++
			h.Write(b[:])
			d.block = h.Sum(nil)
		}
		n := copy(p[written:], d.block)
		written += n
		d.block = d.block[n:]
	}
	return written, nil
}

func newScriptRunner(t *testing.T, e *conformance.Env, s *conformance.Script) *scriptRunner {
	seed, _ := hex.DecodeString(e.TestRNGSeed)
	vc := NewVirtualClock(time.UnixMilli(e.ClockStartMS))
	r := New(Config{RoomTagKey: e.RoomTagKey32(), ProvisionKey: e.ProvisionKey32(), Clock: vc, Rand: &deterministicReader{seed: seed}})
	x := &scriptRunner{t: t, env: e, r: r, clock: vc, conns: map[string]*Socket{}, connRooms: map[string]string{}, http: map[string]chan Result{}, vars: map[string]any{}, bodies: map[string][]byte{}, addr: "127.0.0.1", script: s}
	if s.Setup != nil {
		id, _ := e.Room(s.Setup.Room)
		sk, _ := e.Key(s.Setup.Key)
		sock, _ := r.Connect(id)
		x.conns[s.Setup.Mac] = sock
		x.connRooms[s.Setup.Mac] = s.Setup.Room
		<-sock.out
		frame := auth.AuthFrame(sk, id, wire.RoleMac, sock.challenge[:], auth.ProvisionProof(e.ProvisionKey32(), id, sk.Public().(ed25519.PublicKey)))
		sock.Receive("text", frame)
		<-sock.out
		ds := []wire.Device{}
		for _, d := range s.Setup.Devices {
			k, _ := e.Key(d.Key)
			ds = append(ds, wire.Device{Role: wire.Role(d.Role), Key: k.Public().(ed25519.PublicKey)})
		}
		sock.Receive("text", wire.Encode(wire.DevicesChange{Add: ds, Remove: []ed25519.PublicKey{}}))
		<-sock.out
	}
	return x
}

func (x *scriptRunner) fail(i int, f string, a ...any) {
	x.t.Helper()
	x.t.Fatalf("%s step %d: %s", x.script.Name, i, fmt.Sprintf(f, a...))
}
func (x *scriptRunner) id(name string) (wire.RoomID, error) { return x.env.Room(name) }
func (x *scriptRunner) resolved(s string) (string, error) {
	if v, ok := x.vars[s]; ok {
		return fmt.Sprint(v), nil
	}
	return x.env.Resolve(s, nil)
}
func replaceKeys(raw []byte, e *conformance.Env) ([]byte, error) {
	re := regexp.MustCompile(`@([a-z0-9]+)`)
	var err error
	out := re.ReplaceAllFunc(raw, func(m []byte) []byte {
		k, x := e.Key(string(m[1:]))
		if x != nil {
			err = x
			return m
		}
		return []byte(wire.B64(k.Public().(ed25519.PublicKey)))
	})
	return out, err
}
func (x *scriptRunner) connect(as, room string, header json.RawMessage, i int) {
	if x.script.Layer == "worker" && !x.r.rate(x.addr) {
		x.http[as] = resultChan(Result{Status: 429})
		return
	}
	id := wire.RoomID{}
	valid := true
	if x.script.Layer == "worker" {
		raw := ""
		if header != nil && string(header) != "null" {
			_ = json.Unmarshal(header, &raw)
		} else if room != "" {
			roomID, err := x.env.Room(room)
			if err != nil {
				x.fail(i, "room %v", err)
			}
			raw = roomID.String()
		}
		rt := x.r.workerRoute(http.MethodGet, wire.PathWS, "", raw, true)
		if rt.kind != "ws" {
			x.http[as] = resultChan(Result{Status: 404})
			return
		}
		id = rt.id
		valid = rt.tagged
	} else {
		var e error
		if room != "" {
			id, e = x.id(room)
			if e != nil {
				x.fail(i, "room %v", e)
			}
		}
		if header != nil {
			if string(header) == "null" {
				valid = false
			} else {
				var h string
				_ = json.Unmarshal(header, &h)
				id, e = wire.ParseRoom(h)
				valid = e == nil && auth.CheckRoom(x.r.cfg.RoomTagKey, id)
			}
		} else {
			valid = auth.CheckRoom(x.r.cfg.RoomTagKey, id)
		}
	}
	s, ok := x.r.ConnectTagged(id, valid)
	if !ok {
		x.fail(i, "room full on connect")
	}
	x.conns[as] = s
	x.connRooms[as] = room
}
func resultChan(r Result) chan Result { c := make(chan Result, 1); c <- r; return c }
func (x *scriptRunner) httpReq(as, method, path, room string, header json.RawMessage, upgrade bool, i int) {
	if !(path == wire.PathWS && method == "GET" && upgrade) && x.script.Layer == "worker" && !x.r.rate(x.addr) {
		x.http[as] = resultChan(Result{Status: 429})
		return
	}
	if x.script.Layer == "worker" {
		raw := ""
		if header != nil && string(header) != "null" {
			_ = json.Unmarshal(header, &raw)
		} else if room != "" {
			roomID, err := x.env.Room(room)
			if err != nil {
				x.fail(i, "room %v", err)
			}
			raw = roomID.String()
		}
		rt := x.r.workerRoute(method, path, "", raw, upgrade)
		if rt.kind == "ws" {
			s, ok := x.r.ConnectTagged(rt.id, rt.tagged)
			if !ok {
				x.fail(i, "room full")
			}
			x.conns[as] = s
			x.connRooms[as] = room
			return
		}
		x.http[as] = resultChan(Result{Status: rt.status})
		return
	}
	if path == wire.PathWS && method == "GET" && upgrade {
		x.connect(as, room, header, i)
		return
	}
	x.http[as] = resultChan(Result{Status: 404})
}

func TestConformance(t *testing.T) {
	env, e := conformance.LoadEnv()
	if e != nil {
		t.Fatal(e)
	}
	scripts, e := conformance.Load()
	if e != nil {
		t.Fatal(e)
	}
	for _, s := range scripts {
		s := s
		t.Run(s.Name, func(t *testing.T) {
			x := newScriptRunner(t, env, s)
			steps, e := conformance.Expand(s.Steps)
			if e != nil {
				t.Fatal(e)
			}
			for i, st := range steps {
				x.step(i+1, st)
			}
		})
	}
}
func (x *scriptRunner) step(i int, s conformance.Step) {
	switch {
	case s.Connect != nil:
		x.connect(s.Connect.As, s.Connect.Room, s.Connect.Header, i)
	case s.Send != nil:
		a := s.Send.As
		c := x.conns[a]
		if c == nil {
			x.fail(i, "missing socket %s", a)
		}
		switch {
		case s.Send.Auth != nil:
			spec := s.Send.Auth
			k, e := x.env.Key(spec.Key)
			if e != nil {
				x.fail(i, "auth key: %v", e)
			}
			signer := k
			if spec.Signer != "" {
				signer, e = x.env.Key(spec.Signer)
				if e != nil {
					x.fail(i, "signer: %v", e)
				}
			}
			room := c.room.id
			if spec.Room != "" {
				room, e = x.env.Room(spec.Room)
				if e != nil {
					x.fail(i, "auth room: %v", e)
				}
			}
			nonce := c.challenge[:]
			if spec.Nonce != "" {
				raw := spec.Nonce
				if strings.HasPrefix(raw, "$") {
					raw = fmt.Sprint(x.vars[strings.TrimPrefix(raw, "$")])
				}
				nonce, e = wire.Unb64(raw, wire.ChalLen)
				if e != nil {
					x.fail(i, "auth nonce: %v", e)
				}
			}
			v := wire.AuthVersion
			if spec.V != nil {
				v = *spec.V
			}
			f := wire.Auth{V: v, Role: wire.Role(spec.Role), Key: k.Public().(ed25519.PublicKey), Sig: ed25519.Sign(signer, auth.AuthMessage(room, wire.Role(spec.Role), nonce))}
			switch spec.Provision {
			case "valid":
				f.Proof = auth.ProvisionProof(x.env.ProvisionKey32(), c.room.id, f.Key)
			case "bad":
				f.Proof = make([]byte, 32)
			}
			frame := append(wire.Encode(f), bytes.Repeat([]byte(" "), spec.Pad)...)
			c.Receive("text", frame)
		case s.Send.Raw != nil:
			c.Receive("text", []byte(*s.Send.Raw))
		case s.Send.Text != nil:
			b, e := replaceKeys(s.Send.Text, x.env)
			if e != nil {
				x.fail(i, "resolve: %v", e)
			} // captures in sent frame
			for name, v := range x.vars {
				b = bytes.ReplaceAll(b, []byte("$"+name), []byte(fmt.Sprint(v)))
			}
			c.Receive("text", b)
		case s.Send.Binary != nil:
			b, e := conformance.BinaryBytes(*s.Send.Binary, x.bodies)
			if e != nil {
				x.fail(i, "binary: %v", e)
			}
			if s.Send.Binary.SID != "" {
				v := fmt.Sprint(x.vars[strings.TrimPrefix(s.Send.Binary.SID, "$")])
				z, e := wire.Unb64(v, wire.SIDLen)
				if e != nil {
					x.fail(i, "sid %v", e)
				}
				b = append(z, b...)
			}
			c.Receive("binary", b)
		}
	case s.Ping != nil:
		x.conns[s.Ping.As].Receive("text", []byte(wire.PingText))
	case s.Close != nil:
		c := x.conns[s.Close.As]
		x.r.mu.Lock()
		x.r.closeLocked(c, s.Close.Code)
		x.r.mu.Unlock()
	case s.Post != nil:
		if x.script.Layer == "worker" && !x.r.rate(x.addr) {
			x.http[s.Post.As] = resultChan(Result{Status: 429})
			break
		}
		id, _ := x.id(s.Post.Room)
		if x.script.Layer == "worker" {
			raw := id.String()
			if s.Post.Header != nil {
				if string(s.Post.Header) == "null" {
					raw = ""
				} else {
					_ = json.Unmarshal(s.Post.Header, &raw)
				}
			}
			rt := x.r.workerRoute(http.MethodPost, wire.PathCmd, "", raw, false)
			if rt.kind != "cmd" {
				x.http[s.Post.As] = resultChan(Result{Status: rt.status})
				break
			}
			id = rt.id
		}
		if x.script.Layer == "worker" && !auth.CheckRoom(x.r.cfg.RoomTagKey, id) {
			x.http[s.Post.As] = resultChan(Result{Status: 404})
			break
		}
		body, e := x.env.RequestBody(*s.Post, x.clock.Now())
		if e != nil {
			x.fail(i, "body: %v", e)
		}
		if s.Post.Header != nil {
			var h string
			if json.Unmarshal(s.Post.Header, &h) != nil || h != id.String() {
				x.http[s.Post.As] = resultChan(Result{Status: 404})
				break
			}
		}
		x.http[s.Post.As] = x.r.startPost(id, body)
		x.bodies[s.Post.As] = body
	case s.HTTP != nil:
		x.httpReq(s.HTTP.As, s.HTTP.Method, s.HTTP.Path, s.HTTP.Room, s.HTTP.Header, s.HTTP.Upgrade, i)
	case s.AdvanceMS != nil:
		x.clock.Advance(time.Duration(*s.AdvanceMS) * time.Millisecond)
	case s.Evict != nil:
		x.r.Evict()
	case s.Expect != nil:
		x.expect(i, *s.Expect)
	}
}

func (x *scriptRunner) expect(i int, e conformance.Expect) {
	if c := x.conns[e.As]; c != nil {
		select {
		case got := <-c.out:
			x.match(i, e, got)
		default:
			if e.Nothing {
				return
			}
			x.fail(i, "no socket result pending (closed=%v authed=%v queued=%d expected=%+v)", c.closed, c.authed, len(c.out), e)
		}
		return
	}
	ch := x.http[e.As]
	if ch == nil {
		x.fail(i, "no HTTP exchange %s", e.As)
	}
	select {
	case got := <-ch:
		x.match(i, e, got)
	default:
		if e.Nothing {
			return
		}
		x.fail(i, "no HTTP result pending")
	}
}
func (x *scriptRunner) match(i int, want conformance.Expect, got Result) {
	if want.Nothing {
		x.fail(i, "unexpected pending result %+v", got)
	}
	if want.Close != nil {
		if got.Kind != "close" || got.Code != *want.Close || got.Reason != "" {
			x.fail(i, "want close %d got %+v", *want.Close, got)
		}
		return
	}
	if want.Status != nil {
		if got.Status != *want.Status {
			x.fail(i, "want status %d got %+v", *want.Status, got)
		}
		if want.Hex != nil && hex.EncodeToString(got.Data) != *want.Hex {
			x.fail(i, "want body %s got %x", *want.Hex, got.Data)
		}
		return
	}
	if want.Raw != nil {
		if got.Kind != "text" || string(got.Data) != *want.Raw {
			x.fail(i, "want raw %q got %+v", *want.Raw, got)
		}
		return
	}
	if want.Frame != nil {
		var actual, expected any
		wantBytes, _ := replaceKeys(want.Frame, x.env)
		for name, val := range x.vars {
			wantBytes = bytes.ReplaceAll(wantBytes, []byte("$"+name), []byte(fmt.Sprint(val)))
		}
		if got.Kind != "text" || json.Unmarshal(got.Data, &actual) != nil || json.Unmarshal(wantBytes, &expected) != nil {
			x.fail(i, "expected text frame, got %+v", got)
		}
		if !x.equal(i, expected, actual) {
			x.fail(i, "frame mismatch want %s got %s", want.Frame, got.Data)
		}
		return
	}
	if want.Binary != nil {
		body, e := conformance.BinaryBytes(*want.Binary, x.bodies)
		if e != nil {
			x.fail(i, "binary expected %v", e)
		}
		if want.Binary.SID != "" {
			sid := fmt.Sprint(x.vars[strings.TrimPrefix(want.Binary.SID, "$")])
			raw, _ := wire.Unb64(sid, wire.SIDLen)
			body = append(raw, body...)
		}
		if got.Kind != "binary" || !bytes.Equal(got.Data, body) {
			x.fail(i, "binary mismatch expected %x got %+v", body, got)
		}
	}
}
func (x *scriptRunner) equal(i int, w, a any) bool {
	switch v := w.(type) {
	case map[string]any:
		if cap, ok := v["$capture"].(string); ok {
			x.vars[cap] = a
			return true
		}
		am, ok := a.(map[string]any)
		if !ok || len(am) != len(v) {
			return false
		}
		for k, z := range v {
			av, ok := am[k]
			if !ok {
				return false
			}
			if z == "*" {
				continue
			}
			if str, ok := z.(string); ok && strings.HasPrefix(str, "$") {
				bound := x.vars[str[1:]]
				if fmt.Sprint(bound) != fmt.Sprint(av) {
					return false
				}
				continue
			}
			if !x.equal(i, z, av) {
				return false
			}
		}
		return true
	case []any:
		aa, ok := a.([]any)
		if !ok || len(v) != len(aa) {
			return false
		}
		for j := range v {
			if !x.equal(i, v[j], aa[j]) {
				return false
			}
		}
		return true
	default:
		return fmt.Sprint(w) == fmt.Sprint(a)
	}
}

func TestHTTPAdapter(t *testing.T) {
	env, e := conformance.LoadEnv()
	if e != nil {
		t.Fatal(e)
	}
	id, _ := env.Room("main")
	macKey, _ := env.Key("mac")
	phoneKey, _ := env.Key("phone1")
	watchKey, _ := env.Key("watch1")
	r := New(Config{RoomTagKey: env.RoomTagKey32(), ProvisionKey: env.ProvisionKey32(), Rand: bytes.NewReader(bytes.Repeat([]byte{7}, 4096))})
	ts := httptest.NewServer(r.Handler())
	defer ts.Close()
	wsurl := "ws" + strings.TrimPrefix(ts.URL, "http") + wire.PathWS
	ctx := context.Background()
	hdr := http.Header{wire.Header: []string{id.String()}}
	dial := func() *websocket.Conn {
		c, _, err := websocket.Dial(ctx, wsurl, &websocket.DialOptions{HTTPHeader: hdr})
		if err != nil {
			t.Fatal(err)
		}
		return c
	}
	readText := func(c *websocket.Conn) []byte {
		typ, b, err := c.Read(ctx)
		if err != nil || typ != websocket.MessageText {
			t.Fatalf("read text: %v %v", typ, err)
		}
		return b
	}
	sendAuth := func(c *websocket.Conn, sk ed25519.PrivateKey, role wire.Role, proof []byte) {
		ch, err := wire.DecodeFromRoom(readText(c))
		if err != nil {
			t.Fatal(err)
		}
		challenge := ch.(wire.Challenge)
		if err = c.Write(ctx, websocket.MessageText, auth.AuthFrame(sk, id, role, challenge.Nonce[:], proof)); err != nil {
			t.Fatal(err)
		}
	}
	mac := dial()
	sendAuth(mac, macKey, wire.RoleMac, auth.ProvisionProof(env.ProvisionKey32(), id, macKey.Public().(ed25519.PublicKey)))
	if _, err := wire.DecodeFromRoom(readText(mac)); err != nil {
		t.Fatal(err)
	}
	add, _ := json.Marshal(map[string]any{"t": "devices", "add": []any{map[string]any{"role": "phone", "key": wire.B64(phoneKey.Public().(ed25519.PublicKey))}, map[string]any{"role": "watch", "key": wire.B64(watchKey.Public().(ed25519.PublicKey))}}, "remove": []any{}})
	_ = mac.Write(ctx, websocket.MessageText, add)
	if _, err := wire.DecodeFromRoom(readText(mac)); err != nil {
		t.Fatal(err)
	}
	phone := dial()
	sendAuth(phone, phoneKey, wire.RolePhone, nil)
	peer, err := wire.DecodeFromRoom(readText(mac))
	if err != nil {
		t.Fatal(err)
	}
	p := peer.(wire.Peer)
	_ = mac.Write(ctx, websocket.MessageText, wire.Encode(wire.PeerAck{SID: p.SID}))
	ok, err := wire.DecodeFromRoom(readText(phone))
	if err != nil || !ok.(wire.OKPhone).Peer {
		t.Fatalf("phone ok %v %v", ok, err)
	}
	_ = phone.Write(ctx, websocket.MessageBinary, []byte{1, 2})
	typ, b, err := mac.Read(ctx)
	if err != nil || typ != websocket.MessageBinary {
		t.Fatalf("Mac phone frame %v %v", typ, err)
	}
	sid, payload, err := wire.SplitSID(b)
	if err != nil || sid != p.SID || !bytes.Equal(payload, []byte{1, 2}) {
		t.Fatalf("phone frame %x %v", b, err)
	}
	_ = mac.Write(ctx, websocket.MessageBinary, wire.JoinSID(p.SID, []byte{3, 4}))
	typ, b, err = phone.Read(ctx)
	if err != nil || typ != websocket.MessageBinary || !bytes.Equal(b, []byte{3, 4}) {
		t.Fatalf("phone reply %x %v", b, err)
	}
	postDone := make(chan *http.Response, 1)
	go func() {
		body, err := env.RequestBody(conformance.Post{Room: "main", Request: &conformance.RequestSpec{Key: "watch1", Nonce: "http-adapter"}}, time.Now())
		if err != nil {
			t.Error(err)
			return
		}
		req, _ := http.NewRequest(http.MethodPost, ts.URL+wire.PathCmd, bytes.NewReader(body))
		req.Header.Set(wire.Header, id.String())
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Error(err)
			return
		}
		postDone <- resp
	}()
	peer, err = wire.DecodeFromRoom(readText(mac))
	if err != nil {
		t.Fatal(err)
	}
	wp := peer.(wire.Peer)
	_ = mac.Write(ctx, websocket.MessageText, wire.Encode(wire.PeerAck{SID: wp.SID}))
	typ, b, err = mac.Read(ctx)
	if err != nil || typ != websocket.MessageBinary {
		t.Fatalf("watch frame %v %v", typ, err)
	}
	sid, _, err = wire.SplitSID(b)
	if err != nil || sid != wp.SID {
		t.Fatalf("watch sid %v", err)
	}
	_ = mac.Write(ctx, websocket.MessageBinary, wire.JoinSID(wp.SID, []byte{9, 8, 7}))
	resp := <-postDone
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != 200 || !bytes.Equal(body, []byte{9, 8, 7}) {
		t.Fatalf("watch response %d %x", resp.StatusCode, body)
	}
	bad, _, err := websocket.Dial(ctx, "ws"+strings.TrimPrefix(ts.URL, "http")+"/outside", &websocket.DialOptions{HTTPHeader: hdr})
	if err == nil {
		_ = bad.Close(websocket.StatusNormalClosure, "")
		t.Fatal("outside path upgraded")
	}
}
