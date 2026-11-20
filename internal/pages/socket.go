package pages

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/coder/websocket"
)

// The page's live socket (ccc-pages data-and-endpoints.md § Live records):
// `GET /d/<slug>` upgraded, selecting prefixes, answers with a `hello` (the
// store's change number and the newest record under each prefix) and then
// one frame per committed write under them. A reader that wants to act on a
// change holds it instead of listing on a timer.

// Frame is one change the socket reports: Type is hello, put or del; Key is
// the record a put or del touched; Seq is the store's change number.
type Frame struct {
	Type string
	Key  string
	Seq  int64
}

// ErrSkipped is a socket that skipped a change number: what changed in the
// gap is unknown, so the session ends and the next hello is a fresh start.
var ErrSkipped = errors.New("pages: the live socket skipped a change")

const (
	// socketPing is how often the client pings; socketSilence is how long a
	// socket may send nothing, not even a pong, before it is taken for dead.
	socketPing    = 30 * time.Second
	socketSilence = 75 * time.Second
	// socketSessionMax ends a session before the store's 30 min admission
	// does: an expired socket is closed only on the next change, which it
	// would not deliver.
	socketSessionMax = 25 * time.Minute
	socketDial       = 15 * time.Second
	socketReadLimit  = 2 << 20
)

// Watch holds one socket selecting prefixes and calls on for each hello, put
// and del, in order. It returns nil when ctx ends or the session reaches its
// age limit, and an error when the socket could not open or ended otherwise;
// either way the caller dials again.
func (c *Client) Watch(ctx context.Context, prefixes []string, on func(Frame)) error {
	q := url.Values{"prefix": prefixes, "tail": {"1"}}
	u := c.Base + "/d/" + c.Slug + "?" + q.Encode()
	u = "ws" + strings.TrimPrefix(u, "http") // http → ws, https → wss

	header := http.Header{}
	if c.Bearer != "" {
		header.Set("Authorization", "Bearer "+c.Bearer)
	} else {
		c.mu.Lock()
		err := c.load()
		empty := len(c.cookies) == 0
		c.mu.Unlock()
		if err != nil {
			return err
		}
		if empty {
			if err := c.Unlock(ctx); err != nil {
				return err
			}
		}
		c.mu.Lock()
		var parts []string
		for name, v := range c.cookies {
			parts = append(parts, (&http.Cookie{Name: name, Value: v}).String())
		}
		c.mu.Unlock()
		header.Set("Cookie", strings.Join(parts, "; "))
	}

	dctx, cancel := context.WithTimeout(ctx, socketDial)
	conn, resp, err := websocket.Dial(dctx, u, &websocket.DialOptions{HTTPHeader: header, HTTPClient: c.HTTP})
	cancel()
	if err != nil {
		if ctx.Err() != nil {
			return nil
		}
		if resp != nil && resp.StatusCode != http.StatusSwitchingProtocols {
			return fmt.Errorf("pages: live socket refused: %d", resp.StatusCode)
		}
		return fmt.Errorf("pages: live socket: %w", err)
	}
	defer conn.CloseNow()
	conn.SetReadLimit(socketReadLimit)

	sctx, stop := context.WithTimeout(ctx, socketSessionMax)
	defer stop()
	go func() {
		t := time.NewTicker(socketPing)
		defer t.Stop()
		for {
			select {
			case <-sctx.Done():
				return
			case <-t.C:
				if conn.Write(sctx, websocket.MessageText, []byte(`{"type":"ping"}`)) != nil {
					return
				}
			}
		}
	}()

	var seq int64
	hello := false
	for {
		rctx, cancel := context.WithTimeout(sctx, socketSilence)
		_, data, err := conn.Read(rctx)
		silent := rctx.Err() != nil && sctx.Err() == nil
		cancel()
		if err != nil {
			switch {
			case sctx.Err() != nil:
				return nil // stopped, or the session aged out: the deferred CloseNow ends it
			case silent:
				return fmt.Errorf("pages: live socket silent for %s", socketSilence)
			}
			return fmt.Errorf("pages: live socket closed: %w", err)
		}
		var f struct {
			Type string `json:"type"`
			Key  string `json:"key"`
			Seq  int64  `json:"seq"`
			From int64  `json:"from"`
		}
		if err := json.Unmarshal(data, &f); err != nil {
			return fmt.Errorf("pages: live socket frame is not JSON: %w", err)
		}
		switch f.Type {
		case "hello":
			hello, seq = true, f.Seq
			on(Frame{Type: "hello", Seq: f.Seq})
		case "put", "del", "seq":
			from := f.Seq
			if f.Type == "seq" {
				from = f.From
			}
			if !hello || from != seq+1 {
				return fmt.Errorf("%w: %d after %d", ErrSkipped, from, seq)
			}
			seq = f.Seq
			if f.Type != "seq" {
				on(Frame{Type: f.Type, Key: f.Key, Seq: f.Seq})
			}
		}
		// Anything else (the pong) is the socket being alive.
	}
}
