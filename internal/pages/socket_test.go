package pages

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/coder/websocket"
)

// socketServer upgrades GET /d/<slug>, checks the selection and the
// credential, and sends frames; it returns what the upgrade asked for.
func socketServer(t *testing.T, frames []string) (*httptest.Server, chan *http.Request) {
	t.Helper()
	asked := make(chan *http.Request, 1)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/d/"+slug {
			http.NotFound(w, r)
			return
		}
		asked <- r
		conn, err := websocket.Accept(w, r, nil)
		if err != nil {
			return
		}
		defer conn.CloseNow()
		ctx := r.Context()
		for _, f := range frames {
			if err := conn.Write(ctx, websocket.MessageText, []byte(f)); err != nil {
				return
			}
		}
		conn.Close(websocket.StatusNormalClosure, "done")
	}))
	t.Cleanup(srv.Close)
	return srv, asked
}

func TestWatchSelectsThePrefixesAndReportsEachChange(t *testing.T) {
	srv, asked := socketServer(t, []string{
		`{"type":"hello","seq":7,"keys":{"q.a":{"v":1,"t":1,"schema":"queue@1","value":{}}}}`,
		`{"type":"put","key":"q.b","seq":8,"v":1,"t":2,"schema":"queue@1","value":{}}`,
		`{"type":"seq","from":9,"seq":10}`,
		`{"type":"del","key":"rec.a","seq":11}`,
	})
	c := &Client{Base: srv.URL, Slug: slug, Bearer: "u1:t1"}
	var got []Frame
	err := c.Watch(context.Background(), []string{"q.", "rec."}, func(f Frame) { got = append(got, f) })
	if err == nil {
		t.Fatal("Watch returned nil after the server closed the socket; want an error to redial on")
	}
	r := <-asked
	if p := r.URL.Query()["prefix"]; len(p) != 2 || p[0] != "q." || p[1] != "rec." {
		t.Errorf("prefix = %v, want [q. rec.]", p)
	}
	if tail := r.URL.Query().Get("tail"); tail != "1" {
		t.Errorf("tail = %q, want 1: the watch needs to hear of a change, not the records", tail)
	}
	if a := r.Header.Get("Authorization"); a != "Bearer u1:t1" {
		t.Errorf("Authorization = %q", a)
	}
	want := []Frame{{Type: "hello", Seq: 7}, {Type: "put", Key: "q.b", Seq: 8}, {Type: "del", Key: "rec.a", Seq: 11}}
	if len(got) != len(want) {
		t.Fatalf("frames = %+v, want %+v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("frame %d = %+v, want %+v", i, got[i], want[i])
		}
	}
}

func TestWatchEndsOnASkippedChange(t *testing.T) {
	srv, _ := socketServer(t, []string{
		`{"type":"hello","seq":7,"keys":{}}`,
		`{"type":"put","key":"q.b","seq":9,"v":1,"t":2,"schema":"queue@1","value":{}}`,
	})
	c := &Client{Base: srv.URL, Slug: slug, Bearer: "u1:t1"}
	var got []Frame
	err := c.Watch(context.Background(), []string{"q."}, func(f Frame) { got = append(got, f) })
	if !errors.Is(err, ErrSkipped) {
		t.Fatalf("err = %v, want ErrSkipped", err)
	}
	if len(got) != 1 || got[0].Type != "hello" {
		t.Errorf("frames = %+v, want the hello alone: a put after a gap is not reported", got)
	}
}

func TestWatchReturnsNilWhenStopped(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := websocket.Accept(w, r, nil)
		if err != nil {
			return
		}
		defer conn.CloseNow()
		conn.Write(r.Context(), websocket.MessageText, []byte(`{"type":"hello","seq":1,"keys":{}}`))
		<-r.Context().Done()
	}))
	defer srv.Close()
	c := &Client{Base: srv.URL, Slug: slug, Bearer: "u1:t1"}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- c.Watch(ctx, []string{"q."}, func(Frame) { cancel() }) }()
	select {
	case err := <-done:
		if err != nil {
			t.Errorf("err = %v, want nil when the caller stopped it", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Watch did not return after its context ended")
	}
}
