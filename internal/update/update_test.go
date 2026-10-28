package update

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

const (
	slug    = "release-page-x7k2"
	feedKey = "release.megavoice-mac"
	token   = "tok-app-staging"
)

// pages is a fake ccc-pages service: POST /unlock, the raw feed record with
// ETag, and /f/ answering 302 to a presigned blob — the wire in the
// ccc-pages skill's publishing.md and data-and-endpoints.md.
type pages struct {
	mu      sync.Mutex
	srv     *httptest.Server
	feed    Release
	version int // the record's version: its ETag
	gen     int // grant generation; bumping it expires every grant
	revoked bool
	blob    []byte
	block   chan struct{} // non-nil: the feed handler waits on it

	unlocks, feedGets, notModified, total atomic.Int32
}

func newPages(t *testing.T, ver string) *pages {
	p := &pages{version: 1, blob: []byte("new bundle bytes")}
	p.srv = httptest.NewTLSServer(http.HandlerFunc(p.serve))
	t.Cleanup(p.srv.Close)
	p.setFeed(ver)
	return p
}

func (p *pages) setFeed(ver string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	sum := sha256.Sum256(p.blob)
	p.feed = Release{Version: ver, URL: "/f/" + slug + "/b1", SHA256: hex.EncodeToString(sum[:]), Notes: "修复", NotesEN: "Fixes"}
	p.version++
}

func (p *pages) refuse(w http.ResponseWriter, status int, code string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	fmt.Fprintf(w, `{"error":%q,"code":%q}`, code, code)
}

func (p *pages) granted(r *http.Request) bool {
	c, err := r.Cookie("ccc_grant")
	p.mu.Lock()
	defer p.mu.Unlock()
	return err == nil && c.Value == fmt.Sprintf("g%d", p.gen) && !p.revoked
}

func (p *pages) serve(w http.ResponseWriter, r *http.Request) {
	p.total.Add(1)
	switch {
	case r.Method == http.MethodPost && r.URL.Path == "/unlock":
		p.unlocks.Add(1)
		var b struct{ Slug, Token string }
		json.NewDecoder(r.Body).Decode(&b)
		p.mu.Lock()
		revoked, gen := p.revoked, p.gen
		p.mu.Unlock()
		switch {
		case b.Slug != slug || b.Token != token:
			p.refuse(w, 401, "token_invalid")
		case revoked:
			p.refuse(w, 403, "token_revoked")
		default:
			http.SetCookie(w, &http.Cookie{Name: "ccc_grant", Value: fmt.Sprintf("g%d", gen), Path: "/", Secure: true, HttpOnly: true, MaxAge: 3600})
			fmt.Fprint(w, `{"ok":true,"role":"viewer","expires_in":3600}`)
		}
	case r.URL.Path == "/d/"+slug+"/"+feedKey && r.URL.Query().Get("raw") == "1":
		if !p.granted(r) {
			p.refuse(w, 401, "page_locked")
			return
		}
		p.feedGets.Add(1)
		if p.block != nil {
			<-p.block
		}
		p.mu.Lock()
		etag, feed := fmt.Sprintf(`"%d"`, p.version), p.feed
		p.mu.Unlock()
		w.Header().Set("ETag", etag)
		if r.Header.Get("If-None-Match") == etag {
			p.notModified.Add(1)
			w.WriteHeader(http.StatusNotModified)
			return
		}
		json.NewEncoder(w).Encode(feed)
	case r.URL.Path == "/f/"+slug+"/b1":
		if !p.granted(r) {
			p.refuse(w, 401, "page_locked")
			return
		}
		http.Redirect(w, r, "/r2/presigned?sig=x", http.StatusFound)
	case r.URL.Path == "/r2/presigned":
		p.mu.Lock()
		blob := p.blob
		p.mu.Unlock()
		http.ServeContent(w, r, "blob", time.Time{}, bytes.NewReader(blob))
	default:
		http.NotFound(w, r)
	}
}

func (p *pages) config(t *testing.T, current string) Config {
	return Config{
		Channel:    "staging",
		FeedURL:    p.srv.URL + "/d/" + slug + "/" + feedKey + "?raw=1",
		Token:      token,
		Current:    current,
		StateDir:   t.TempDir(),
		OSVersion:  "15.1",
		HTTPClient: p.srv.Client(),
	}
}

func mustNew(t *testing.T, cfg Config) *Updater {
	t.Helper()
	u, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	return u
}

func TestCheckNewer(t *testing.T) {
	p := newPages(t, "v0.2.0")
	u := mustNew(t, p.config(t, "v0.1.0"))
	rel, err := u.Check(context.Background())
	if err != nil || rel == nil {
		t.Fatalf("Check = %v, %v; want the v0.2.0 release", rel, err)
	}
	if rel.Version != "v0.2.0" || rel.URL != p.srv.URL+"/f/"+slug+"/b1" || rel.Notes != "修复" || rel.NotesEN != "Fixes" {
		t.Fatalf("release = %+v", rel)
	}
}

func TestCheckSameOrLowerIgnored(t *testing.T) {
	for _, ver := range []string{"v0.2.0", "v0.1.9", "0.1.12"} {
		p := newPages(t, ver)
		u := mustNew(t, p.config(t, "v0.2.0"))
		if rel, err := u.Check(context.Background()); rel != nil || err != nil {
			t.Errorf("feed %s over v0.2.0: Check = %+v, %v; want nothing", ver, rel, err)
		}
	}
}

func TestSkipIsPerVersion(t *testing.T) {
	p := newPages(t, "v0.2.0")
	cfg := p.config(t, "v0.1.0")
	u := mustNew(t, cfg)
	if err := u.Skip("v0.2.0"); err != nil {
		t.Fatal(err)
	}
	// the skip survives a relaunch: a new Updater on the same StateDir
	u = mustNew(t, cfg)
	if rel, err := u.Check(context.Background()); rel != nil || err != nil {
		t.Fatalf("skipped v0.2.0: Check = %+v, %v; want nothing", rel, err)
	}
	p.setFeed("v0.2.1")
	if rel, err := u.Check(context.Background()); err != nil || rel == nil || rel.Version != "v0.2.1" {
		t.Fatalf("v0.2.1 after skipping v0.2.0: Check = %+v, %v; want v0.2.1", rel, err)
	}
}

func TestNotModifiedUsesStoredFeed(t *testing.T) {
	p := newPages(t, "v0.2.0")
	u := mustNew(t, p.config(t, "v0.1.0"))
	for i := range 2 {
		rel, err := u.Check(context.Background())
		if err != nil || rel == nil || rel.Version != "v0.2.0" {
			t.Fatalf("check %d: %+v, %v", i, rel, err)
		}
	}
	if got := p.notModified.Load(); got != 1 {
		t.Fatalf("304s = %d, want 1 (the second check sends If-None-Match)", got)
	}
	// a skip after the 304 still applies to the stored feed
	u.Skip("v0.2.0")
	if rel, _ := u.Check(context.Background()); rel != nil {
		t.Fatalf("skipped after 304: got %+v", rel)
	}
}

func TestExpiredGrantReunlocksOnce(t *testing.T) {
	p := newPages(t, "v0.2.0")
	u := mustNew(t, p.config(t, "v0.1.0"))
	if _, err := u.Check(context.Background()); err != nil {
		t.Fatal(err)
	}
	p.mu.Lock()
	p.gen++ // the grant lapsed between daily checks
	p.mu.Unlock()
	rel, err := u.Check(context.Background())
	if err != nil || rel == nil {
		t.Fatalf("after grant expiry: Check = %+v, %v", rel, err)
	}
	if got := p.unlocks.Load(); got != 2 {
		t.Fatalf("unlocks = %d, want 2 (launch, then one re-unlock on 401)", got)
	}
}

func TestRevokedTokenStopsPolling(t *testing.T) {
	p := newPages(t, "v0.2.0")
	u := mustNew(t, p.config(t, "v0.1.0"))
	if _, err := u.Check(context.Background()); err != nil {
		t.Fatal(err)
	}
	p.mu.Lock()
	p.revoked = true
	p.mu.Unlock()
	_, err := u.Check(context.Background())
	if !errors.Is(err, ErrRevoked) {
		t.Fatalf("revoked token: err = %v, want ErrRevoked", err)
	}
	if got := UserMessage(err); got != "更新不可用，请从页面重新安装" {
		t.Fatalf("UserMessage = %q", got)
	}
	before := p.total.Load()
	if _, err := u.Check(context.Background()); !errors.Is(err, ErrRevoked) {
		t.Fatalf("second check: %v", err)
	}
	if p.total.Load() != before {
		t.Fatal("a revoked updater made a request")
	}
}

func TestUnlockRefusedIsRevoked(t *testing.T) {
	p := newPages(t, "v0.2.0")
	cfg := p.config(t, "v0.1.0")
	cfg.Token = "wrong"
	_, err := mustNew(t, cfg).Check(context.Background())
	if !errors.Is(err, ErrRevoked) || !strings.Contains(err.Error(), "token_invalid") {
		t.Fatalf("err = %v, want ErrRevoked (token_invalid)", err)
	}
}

func TestNoChannelMakesNoRequest(t *testing.T) {
	p := newPages(t, "v9.0.0")
	cfg := p.config(t, "")
	cfg.Channel = ""
	u := mustNew(t, cfg)
	if rel, err := u.Check(context.Background()); rel != nil || err != nil {
		t.Fatalf("Check = %+v, %v; want nothing", rel, err)
	}
	if err := u.Apply(context.Background(), &Release{}, t.TempDir()+"/X.app"); !errors.Is(err, ErrDisabled) {
		t.Fatalf("Apply = %v, want ErrDisabled", err)
	}
	if got := p.total.Load(); got != 0 {
		t.Fatalf("requests = %d, want 0", got)
	}
}

func TestOverlappingChecksShareOneRequest(t *testing.T) {
	p := newPages(t, "v0.2.0")
	p.block = make(chan struct{})
	u := mustNew(t, p.config(t, "v0.1.0"))
	var wg sync.WaitGroup
	rels := make([]*Release, 2)
	wg.Go(func() { rels[0], _ = u.Check(context.Background()) })
	for p.feedGets.Load() == 0 { // the first check is inside its request
		time.Sleep(time.Millisecond)
	}
	wg.Go(func() { rels[1], _ = u.Check(context.Background()) })
	time.Sleep(100 * time.Millisecond) // the second joins the first
	close(p.block)
	wg.Wait()
	if got := p.feedGets.Load(); got != 1 {
		t.Fatalf("feed GETs = %d, want 1", got)
	}
	if rels[0] == nil || rels[1] == nil {
		t.Fatalf("both callers want the release: %+v", rels)
	}
}

func TestMinMacOS(t *testing.T) {
	p := newPages(t, "v0.2.0")
	p.mu.Lock()
	p.feed.MinMacOS = "15.2"
	p.version++
	p.mu.Unlock()
	cfg := p.config(t, "v0.1.0")
	cfg.OSVersion = "15.1"
	if rel, err := mustNew(t, cfg).Check(context.Background()); rel != nil || err != nil {
		t.Fatalf("macOS 15.1 under min 15.2: Check = %+v, %v; want nothing", rel, err)
	}
}

func TestParseVersion(t *testing.T) {
	for in, ok := range map[string]bool{"v0.1.0": true, "0.12.3": true, "v0.1": false, "v0.01.0": false, "dev": false, "v0.1.-1": false} {
		if _, err := parseVersion(in); (err == nil) != ok {
			t.Errorf("parseVersion(%q) err = %v", in, err)
		}
	}
}
