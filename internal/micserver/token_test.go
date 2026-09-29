package micserver

import (
	"context"
	"net/http"
	"os"
	"testing"
	"time"

	"github.com/coder/websocket"

	"github.com/caoer/mega-asr/internal/audio"
)

// A token minted over the control socket opens a stream like a PIN
// pairing's; minting again for the same name is re-pairing: the old token
// stops working, its live stream closes, and the name stays paired once.
func TestTokenReissueRevokesTheOld(t *testing.T) {
	srcs := make(chan *fakeSource, 2)
	b := newBox(t, func() audio.Source { f := newFake(make([]int16, FrameSamples)); srcs <- f; return f })
	if _, err := pair(b.addr, b.pin(t), "studio"); err != nil { // a PIN pairing of the same name is re-paired too
		t.Fatal(err)
	}
	old, err := b.Token("studio", 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(old) != 43 { // 32 bytes, unpadded base64url: pairing's encoding
		t.Errorf("token %q: %d chars, want 43", old, len(old))
	}
	c, _, err := dial(t, b.addr, old)
	if err != nil {
		t.Fatal(err)
	}
	<-(<-srcs).started
	waitFor(t, func() bool { cs := b.Clients(); return len(cs) == 1 && len(cs[0].Streams) == 1 })

	fresh, err := b.Token("studio", 1000)
	if err != nil {
		t.Fatal(err)
	}
	if fresh == old {
		t.Fatal("re-issue returned the same token")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	for {
		_, _, err := c.Read(ctx)
		if err == nil {
			continue
		}
		if s := websocket.CloseStatus(err); s != websocket.StatusPolicyViolation {
			t.Errorf("old token's stream ended with %v (status %d), want policy violation", err, s)
		}
		break
	}
	if _, resp, err := dial(t, b.addr, old); err == nil || resp == nil || resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("old token: %v %v, want 401", resp, err)
	}
	if cs := b.Clients(); len(cs) != 1 || cs[0].Name != "studio" {
		t.Errorf("clients %+v, want studio once", cs)
	}
	c2, _, err := dial(t, b.addr, fresh)
	if err != nil {
		t.Fatalf("new token: %v", err)
	}
	if _, _, err := c2.Read(ctx); err != nil {
		t.Errorf("new token's stream: %v", err)
	}
	c2.Close(websocket.StatusNormalClosure, "")
	if !b.log.has("paired studio via the control socket (uid 0)") || !b.log.has("paired studio via the control socket (uid 1000)") {
		t.Errorf("token not logged: %q", b.log.lines)
	}
}

// A name `mic revoke` could not take is refused before anything is paired.
func TestTokenBadNameRefused(t *testing.T) {
	b := newBox(t, nil)
	for _, name := range []string{"", "-x", "a b", "studio/../x"} {
		if tok, err := b.Token(name, 0); err == nil {
			t.Errorf("%q: token %q", name, tok)
		}
	}
	if cs := b.Clients(); len(cs) != 0 {
		t.Errorf("clients %+v", cs)
	}
}

// A re-issue the box cannot save pairs nothing new: the old token keeps
// working and its live stream stays open.
func TestTokenReissueSaveFailsKeepsTheOld(t *testing.T) {
	if os.Getuid() == 0 {
		t.Skip("root writes a read-only directory")
	}
	srcs := make(chan *fakeSource, 2)
	b := newBox(t, func() audio.Source { f := newFake(make([]int16, FrameSamples)); srcs <- f; return f })
	old, err := b.Token("studio", 0)
	if err != nil {
		t.Fatal(err)
	}
	c, _, err := dial(t, b.addr, old)
	if err != nil {
		t.Fatal(err)
	}
	src := <-srcs
	<-src.started
	waitFor(t, func() bool { cs := b.Clients(); return len(cs) == 1 && len(cs[0].Streams) == 1 })
	if err := os.Chmod(b.cfg.State, 0o500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.Chmod(b.cfg.State, 0o700) })
	if tok, err := b.Token("studio", 0); err == nil {
		t.Fatalf("re-issue saved into a read-only state dir: %q", tok)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if _, _, err := c.Read(ctx); err != nil {
		t.Errorf("old stream after a failed re-issue: %v", err)
	}
	if cs := b.Clients(); len(cs) != 1 || len(cs[0].Streams) != 1 {
		t.Errorf("clients %+v, want studio with its stream", cs)
	}
	os.Chmod(b.cfg.State, 0o700)
	c.Close(websocket.StatusNormalClosure, "")
	src.Stop()
	c2, _, err := dial(t, b.addr, old)
	if err != nil {
		t.Fatalf("old token after a failed re-issue: %v", err)
	}
	c2.Close(websocket.StatusNormalClosure, "")
}
