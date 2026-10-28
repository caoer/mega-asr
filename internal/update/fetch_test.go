package update

import (
	"context"
	"errors"
	"io"
	"net/http"
	"testing"
)

func TestFetchResolvesAgainstFeedWithGrantAndRange(t *testing.T) {
	p := newPages(t, "v0.2.0")
	u := mustNew(t, p.config(t, "v0.1.0"))
	// a record beside the feed, by a relative ref
	resp, err := u.Fetch(context.Background(), feedKey+"?raw=1", nil)
	if err != nil || resp.StatusCode != http.StatusOK {
		t.Fatalf("record: %v, %v", resp, err)
	}
	resp.Body.Close()
	// a file, resumed: Range survives the 302 to the presigned link
	resp, err = u.Fetch(context.Background(), "/f/"+slug+"/b1", http.Header{"Range": {"bytes=4-"}})
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusPartialContent || string(b) != string(p.blob[4:]) {
		t.Fatalf("ranged file: %s %q, want 206 %q", resp.Status, b, p.blob[4:])
	}
	if got := p.unlocks.Load(); got != 1 {
		t.Fatalf("unlocks = %d, want 1", got)
	}
}

func TestFetchDisabledMakesNoRequest(t *testing.T) {
	p := newPages(t, "v0.2.0")
	cfg := p.config(t, "")
	cfg.Channel = ""
	if _, err := mustNew(t, cfg).Fetch(context.Background(), "/f/"+slug+"/b1", nil); !errors.Is(err, ErrDisabled) {
		t.Fatalf("err = %v, want ErrDisabled", err)
	}
	if got := p.total.Load(); got != 0 {
		t.Fatalf("requests = %d, want 0", got)
	}
}
