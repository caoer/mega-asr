package conformance

import (
	"bytes"
	"crypto/ed25519"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/caoer/mega-asr/internal/relay/auth"
	"github.com/caoer/mega-asr/internal/relay/wire"
)

func TestEnv(t *testing.T) {
	e, err := LoadEnv()
	if err != nil {
		t.Fatal(err)
	}
	if e.RoomTagKey32() == nil || e.ProvisionKey32() == nil {
		t.Fatal("env secrets are not 32 bytes")
	}
	if b, err := hex.DecodeString(e.TestRNGSeed); err != nil || len(b) == 0 || e.ClockStartMS <= 0 {
		t.Fatal("rng seed or clock")
	}
	for name := range e.Keys {
		if _, err := e.Key(name); err != nil {
			t.Fatal(err)
		}
	}
	for name := range e.Rooms {
		id, err := e.Room(name)
		if err != nil {
			t.Fatal(err)
		}
		if bad := strings.HasPrefix(name, "badtag"); auth.CheckRoom(e.RoomTagKey32(), id) == bad {
			t.Fatalf("room %s: tag valid = %v", name, !bad)
		}
	}
}

var codes = map[int]bool{
	wire.CloseNormal: true, wire.CloseUnsupported: true, wire.CloseRefused: true, wire.CloseTooBig: true,
	wire.CloseMacGone: true, wire.CloseFull: true, wire.CloseSuperseded: true, wire.CloseRate: true,
}

var statuses = map[int]bool{200: true, 404: true, 429: true, 503: true, 504: true}

type checker struct {
	e      *Env
	conns  map[string]string // name → "ws" or "http"
	vars   map[string]bool
	errs   []string
	script string
}

func (c *checker) fail(i int, format string, a ...any) {
	c.errs = append(c.errs, fmt.Sprintf("%s step %d: %s", c.script, i, fmt.Sprintf(format, a...)))
}

func (c *checker) key(i int, name string) {
	if _, err := c.e.Key(name); err != nil {
		c.fail(i, "%v", err)
	}
}

func (c *checker) room(i int, name string) {
	if _, err := c.e.Room(name); err != nil {
		c.fail(i, "%v", err)
	}
}

func (c *checker) ref(i int, s string) {
	if strings.HasPrefix(s, "$") && !c.vars[s[1:]] {
		c.fail(i, "%s used before it is captured", s)
	}
	for _, m := range keyRef.FindAllStringSubmatch(s, -1) {
		c.key(i, m[1])
	}
}

// walk checks the references in a JSON value and binds its captures.
func (c *checker) walk(i int, v any, capture bool) {
	switch v := v.(type) {
	case string:
		c.ref(i, v)
	case []any:
		for _, x := range v {
			c.walk(i, x, capture)
		}
	case map[string]any:
		if name, ok := v["$capture"].(string); ok && len(v) == 1 {
			if !capture {
				c.fail(i, "a capture in a frame that is sent")
			}
			c.vars[name] = true
			return
		}
		for _, x := range v {
			c.walk(i, x, capture)
		}
	}
}

func (c *checker) conn(i int, as, kind string) {
	if got, ok := c.conns[as]; !ok || got != kind {
		c.fail(i, "%q is not an open %s connection", as, kind)
	}
}

func (c *checker) object(i int, raw json.RawMessage, capture bool) {
	var v map[string]any
	if json.Unmarshal(raw, &v) != nil || v == nil {
		c.fail(i, "frame is not a JSON object")
		return
	}
	if _, ok := v["t"].(string); !ok {
		c.fail(i, "frame has no t")
	}
	c.walk(i, v, capture)
}

func (c *checker) binary(i int, b *Binary, toMac bool) {
	n := 0
	for _, set := range []bool{b.Hex != "", b.Len != nil, b.BodyOf != ""} {
		if set {
			n++
		}
	}
	if n > 1 {
		c.fail(i, "binary names more than one payload")
	}
	if _, err := hex.DecodeString(b.Hex); err != nil {
		c.fail(i, "binary hex: %v", err)
	}
	if b.BodyOf != "" {
		c.conn(i, b.BodyOf, "http")
	}
	if b.SID != "" {
		c.ref(i, b.SID)
	}
}

func (c *checker) step(i int, st Step) {
	set := 0
	for _, p := range []bool{st.Connect != nil, st.Send != nil, st.Ping != nil, st.Close != nil, st.Post != nil, st.HTTP != nil, st.Expect != nil, st.AdvanceMS != nil, st.Evict != nil} {
		if p {
			set++
		}
	}
	if set != 1 {
		c.fail(i, "%d actions in one step", set)
		return
	}
	switch {
	case st.Connect != nil:
		s := st.Connect
		if s.Header == nil || s.Room != "" {
			c.room(i, s.Room)
		}
		c.conns[s.As] = "ws"
	case st.Send != nil:
		s := st.Send
		c.conn(i, s.As, "ws")
		n := 0
		if s.Text != nil {
			n++
			c.object(i, s.Text, false)
		}
		if s.Raw != nil {
			n++
			c.ref(i, *s.Raw)
		}
		if s.Auth != nil {
			n++
			a := s.Auth
			if a.Role == "" {
				c.fail(i, "auth without a role")
			}
			c.key(i, a.Key)
			if a.Signer != "" {
				c.key(i, a.Signer)
			}
			if a.Room != "" {
				c.room(i, a.Room)
			}
			if a.Nonce != "" {
				c.ref(i, a.Nonce)
			}
			if a.Provision != "" && a.Provision != "valid" && a.Provision != "bad" {
				c.fail(i, "provision %q", a.Provision)
			}
		}
		if s.Binary != nil {
			n++
			c.binary(i, s.Binary, true)
		}
		if n != 1 {
			c.fail(i, "send carries %d payloads", n)
		}
	case st.Ping != nil:
		c.conn(i, st.Ping.As, "ws")
	case st.Close != nil:
		c.conn(i, st.Close.As, "ws")
	case st.Post != nil:
		p := st.Post
		c.room(i, p.Room)
		if (p.Request == nil) == (p.BodyLen == nil) {
			c.fail(i, "post needs request or body_len")
		}
		if r := p.Request; r != nil {
			c.key(i, r.Key)
			if r.Signer != "" {
				c.key(i, r.Signer)
			}
			if r.Nonce == "" {
				c.fail(i, "request without a nonce")
			}
		}
		c.conns[p.As] = "http"
	case st.HTTP != nil:
		h := st.HTTP
		if h.Room != "" {
			c.room(i, h.Room)
		}
		if h.Method == "" || !strings.HasPrefix(h.Path, "/") {
			c.fail(i, "http method or path")
		}
		c.conns[h.As] = "http"
	case st.Expect != nil:
		x := st.Expect
		kind, ok := c.conns[x.As]
		if !ok {
			c.fail(i, "expect on %q, which is not open", x.As)
		}
		n := 0
		if x.Frame != nil {
			n++
			c.object(i, x.Frame, true)
		}
		if x.Raw != nil {
			n++
			c.ref(i, *x.Raw)
		}
		if x.Binary != nil {
			n++
			c.binary(i, x.Binary, false)
		}
		if x.Close != nil {
			n++
			if !codes[*x.Close] {
				c.fail(i, "close code %d is not in PROTOCOL.md § 4.6", *x.Close)
			}
		}
		if x.Status != nil {
			n++
			if !statuses[*x.Status] {
				c.fail(i, "status %d", *x.Status)
			}
			if kind != "http" {
				c.fail(i, "status on a socket")
			}
		}
		if x.Hex != nil && x.Status == nil {
			c.fail(i, "hex without status")
		}
		if x.Nothing {
			n++
		}
		if n != 1 {
			c.fail(i, "expect names %d outcomes", n)
		}
		if kind == "http" && x.Status == nil && !x.Nothing {
			c.fail(i, "a frame expected on an http request")
		}
	case st.AdvanceMS != nil:
		if *st.AdvanceMS <= 0 {
			c.fail(i, "advance_ms %d", *st.AdvanceMS)
		}
	}
}

// TestScripts checks every script against the format of PROTOCOL.md § 11:
// one action per step, known keys and rooms, connections opened before use,
// captures before references, close codes and statuses the protocol has.
func TestScripts(t *testing.T) {
	e, err := LoadEnv()
	if err != nil {
		t.Fatal(err)
	}
	scripts, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if len(scripts) < 15 {
		t.Fatalf("%d scripts", len(scripts))
	}
	for _, s := range scripts {
		c := &checker{e: e, conns: map[string]string{}, vars: map[string]bool{}, script: s.Name}
		if s.About == "" || (s.Layer != "room" && s.Layer != "worker") {
			c.fail(0, "about %q, layer %q", s.About, s.Layer)
		}
		if st := s.Setup; st != nil {
			c.room(0, st.Room)
			c.key(0, st.Key)
			for _, d := range st.Devices {
				c.key(0, d.Key)
				if d.Role != "phone" && d.Role != "watch" {
					c.fail(0, "setup device role %q", d.Role)
				}
			}
			c.conns[st.Mac] = "ws"
			if s.Layer != "room" {
				c.fail(0, "setup on a worker script")
			}
		}
		steps, err := Expand(s.Steps)
		if err != nil {
			t.Fatalf("%s: %v", s.Name, err)
		}
		for i, st := range steps {
			c.step(i+1, st)
		}
		for _, e := range c.errs {
			t.Error(e)
		}
	}
}

func TestBuilders(t *testing.T) {
	e, err := LoadEnv()
	if err != nil {
		t.Fatal(err)
	}
	room, _ := e.Room("main")
	nonce := bytes.Repeat([]byte{7}, wire.ChalLen)
	frame, err := e.AuthFrame(AuthSpec{Role: "mac", Key: "mac", Provision: "valid"}, "main", nonce)
	if err != nil {
		t.Fatal(err)
	}
	mk, _ := e.Key("mac")
	empty := keyset{}
	a, err := auth.CheckAuth(e.ProvisionKey32(), room, nonce, frame, empty)
	if err != nil || !a.Provision {
		t.Fatalf("a valid provision from the builder: %+v %v", a, err)
	}
	for _, spec := range []AuthSpec{{Role: "mac", Key: "mac", Provision: "bad"}, {Role: "mac", Key: "mac", Signer: "stranger", Provision: "valid"}, {Role: "mac", Key: "mac", Room: "other", Provision: "valid"}} {
		f, _ := e.AuthFrame(spec, "main", nonce)
		if _, err := auth.CheckAuth(e.ProvisionKey32(), room, nonce, f, empty); err == nil {
			t.Fatalf("%+v admitted", spec)
		}
	}
	padded, _ := e.AuthFrame(AuthSpec{Role: "phone", Key: "phone1", Pad: 1025}, "main", nonce)
	if len(padded) <= wire.MaxAuthFrame {
		t.Fatal("pad")
	}
	now := time.UnixMilli(e.ClockStartMS)
	body, err := e.RequestBody(Post{As: "w", Room: "main", Request: &RequestSpec{Key: "watch1", Nonce: "n1", TS: -1000}}, now)
	if err != nil {
		t.Fatal(err)
	}
	r, err := wire.ParseRequest(body)
	if err != nil || r.Nonce != NonceOf("n1") || int64(r.TS) != e.ClockStartMS-1000 || len(r.Ciphertext) != 128 {
		t.Fatalf("request %+v %v", r, err)
	}
	if !auth.VerifyRequest(room, r, true) {
		t.Fatal("builder's request signature")
	}
	forged, _ := e.RequestBody(Post{As: "w", Room: "main", Request: &RequestSpec{Key: "watch1", Signer: "stranger", Nonce: "n1"}}, now)
	fr, _ := wire.ParseRequest(forged)
	wk, _ := e.Key("watch1")
	if auth.VerifyRequest(room, fr, true) || !bytes.Equal(fr.Key, wk.Public().(ed25519.PublicKey)) {
		t.Fatal("forged request")
	}
	if s, err := e.Resolve("@mac", nil); err != nil || s != wire.B64(mk.Public().(ed25519.PublicKey)) {
		t.Fatal("resolve @mac", s, err)
	}
	if s, err := e.Resolve("$s1", map[string]string{"s1": "x"}); err != nil || s != "x" {
		t.Fatal("resolve $s1")
	}
}

type keyset struct{}

func (keyset) MacKey() ed25519.PublicKey                      { return nil }
func (keyset) DeviceRole(ed25519.PublicKey) (wire.Role, bool) { return "", false }
