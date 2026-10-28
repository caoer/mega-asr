package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/caoer/mega-asr/internal/app"
	"github.com/caoer/mega-asr/internal/update"
)

const (
	pageSlug  = "release-page-0123456789ab"
	appToken  = "tok-app-staging"
	feedKey   = "release.megavoice-mac"
	newBundle = "the v0.1.1 bundle"
)

// friendsPage is a fake of the release page: POST /unlock with the app
// token sets the grant cookie; the feed record answers with an ETag; /f/
// redirects to the bytes. revoke makes every grant and unlock fail.
type friendsPage struct {
	srv      *httptest.Server
	mu       sync.Mutex
	version  string
	revoked  bool
	requests atomic.Int32
}

func newFriendsPage(t *testing.T, version string) *friendsPage {
	p := &friendsPage{version: version}
	p.srv = httptest.NewTLSServer(http.HandlerFunc(p.serve))
	t.Cleanup(p.srv.Close)
	return p
}

func (p *friendsPage) set(version string, revoked bool) {
	p.mu.Lock()
	p.version, p.revoked = version, revoked
	p.mu.Unlock()
}

func (p *friendsPage) serve(w http.ResponseWriter, r *http.Request) {
	p.requests.Add(1)
	p.mu.Lock()
	version, revoked := p.version, p.revoked
	p.mu.Unlock()
	refuse := func(status int, code string) {
		w.WriteHeader(status)
		fmt.Fprintf(w, `{"code":%q}`, code)
	}
	if r.URL.Path == "/unlock" {
		var b struct{ Slug, Token string }
		json.NewDecoder(r.Body).Decode(&b)
		if revoked || b.Token != appToken || b.Slug != pageSlug {
			refuse(403, "token_revoked")
			return
		}
		http.SetCookie(w, &http.Cookie{Name: "ccc_grant", Value: "g", Path: "/", Secure: true, HttpOnly: true})
		fmt.Fprint(w, `{"ok":true}`)
		return
	}
	if c, err := r.Cookie("ccc_grant"); err != nil || c.Value != "g" || revoked {
		refuse(401, "page_locked")
		return
	}
	switch r.URL.Path {
	case "/d/" + pageSlug + "/" + feedKey:
		sum := sha256.Sum256([]byte(newBundle))
		w.Header().Set("ETag", `"`+version+`"`)
		if r.Header.Get("If-None-Match") == `"`+version+`"` {
			w.WriteHeader(http.StatusNotModified)
			return
		}
		json.NewEncoder(w).Encode(update.Release{Version: version, URL: "/f/" + pageSlug + "/dmg1", SHA256: hex.EncodeToString(sum[:]), Notes: "修复了几处问题"})
	case "/f/" + pageSlug + "/dmg1":
		http.Redirect(w, r, "/presigned/dmg1", http.StatusFound)
	case "/presigned/dmg1":
		fmt.Fprint(w, newBundle)
	default:
		http.NotFound(w, r)
	}
}

// fakeApp records what the update loop asked of the app.
type fakeApp struct {
	answer   int
	asked    []string
	flashes  []string
	status   string
	restarts int
}

func (f *fakeApp) ui() releaseUI {
	return releaseUI{
		ask:     func(rel *update.Release) int { f.asked = append(f.asked, rel.Version); return f.answer },
		flash:   func(msg string) { f.flashes = append(f.flashes, msg) },
		status:  func(line string) { f.status = line },
		restart: func() { f.restarts++ },
	}
}

// installed is a MegaVoice.app with a marker file; the stand-in for ditto
// unpacks the download into one the same way.
func installed(t *testing.T, dir, marker string) string {
	t.Helper()
	bundle := filepath.Join(dir, "MegaVoice.app")
	if err := os.MkdirAll(filepath.Join(bundle, "Contents"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(bundle, "Contents", "marker"), []byte(marker), 0o644); err != nil {
		t.Fatal(err)
	}
	return bundle
}

func stagingReleases(t *testing.T, p *friendsPage, current string, f *fakeApp) (*releases, string) {
	t.Helper()
	bundle := installed(t, t.TempDir(), "v0.1.0")
	r, err := newReleases(update.Config{
		Channel:    "staging",
		FeedURL:    p.srv.URL + "/d/" + pageSlug + "/" + feedKey + "?raw=1",
		Token:      appToken,
		Current:    current,
		StateDir:   t.TempDir(),
		OSVersion:  "15.0",
		HTTPClient: p.srv.Client(),
		Extract: func(_ context.Context, archive, dir string) (string, error) {
			b, err := os.ReadFile(archive)
			if err != nil {
				return "", err
			}
			return installed(t, dir, string(b)), nil
		},
	}, bundle, f.ui())
	if err != nil {
		t.Fatal(err)
	}
	return r, bundle
}

func TestUpdateNewerPromptsInstallsAndRestarts(t *testing.T) {
	p := newFriendsPage(t, "v0.1.1")
	f := &fakeApp{answer: choiceUpdate}
	r, bundle := stagingReleases(t, p, "v0.1.0", f)
	r.check(context.Background())
	if len(f.asked) != 1 || f.asked[0] != "v0.1.1" {
		t.Fatalf("prompts = %v, want one for v0.1.1", f.asked)
	}
	if b, _ := os.ReadFile(filepath.Join(bundle, "Contents", "marker")); string(b) != newBundle {
		t.Fatalf("installed bundle = %q, want the downloaded one", b)
	}
	if b, _ := os.ReadFile(filepath.Join(update.BackupPath(bundle), "Contents", "marker")); string(b) != "v0.1.0" {
		t.Fatalf("backup = %q, want the previous bundle", b)
	}
	if f.restarts != 1 {
		t.Fatalf("restarts = %d, want 1 (restart --when-idle)", f.restarts)
	}
}

func TestUpdateSameVersionAsksNothing(t *testing.T) {
	p := newFriendsPage(t, "v0.1.0")
	f := &fakeApp{answer: choiceUpdate}
	r, _ := stagingReleases(t, p, "v0.1.0", f)
	r.check(context.Background())
	if len(f.asked) != 0 || f.restarts != 0 {
		t.Fatalf("same version: prompts %v, restarts %d; want none", f.asked, f.restarts)
	}
}

func TestUpdateSkippedVersionStaysSilent(t *testing.T) {
	p := newFriendsPage(t, "v0.1.1")
	f := &fakeApp{answer: choiceSkip}
	r, bundle := stagingReleases(t, p, "v0.1.0", f)
	r.check(context.Background())
	r.check(context.Background())
	if len(f.asked) != 1 {
		t.Fatalf("prompts after 跳过此版本 = %v, want the first only", f.asked)
	}
	if b, _ := os.ReadFile(filepath.Join(bundle, "Contents", "marker")); string(b) != "v0.1.0" {
		t.Fatal("a skipped version was installed")
	}
	p.set("v0.1.2", false)
	r.check(context.Background())
	if len(f.asked) != 2 || f.asked[1] != "v0.1.2" {
		t.Fatalf("prompts = %v, want v0.1.2 asked after skipping v0.1.1", f.asked)
	}
}

func TestUpdateLaterAsksAgain(t *testing.T) {
	p := newFriendsPage(t, "v0.1.1")
	f := &fakeApp{answer: choiceLater}
	r, _ := stagingReleases(t, p, "v0.1.0", f)
	r.check(context.Background())
	r.check(context.Background())
	if len(f.asked) != 2 {
		t.Fatalf("prompts after 稍后 = %v, want v0.1.1 asked again", f.asked)
	}
}

func TestUpdateRevokedTokenStopsPolling(t *testing.T) {
	p := newFriendsPage(t, "v0.1.0")
	f := &fakeApp{answer: choiceUpdate}
	r, _ := stagingReleases(t, p, "v0.1.0", f)
	r.check(context.Background())
	p.set("v0.1.1", true) // the app token is revoked: 401, then the unlock refused
	r.check(context.Background())
	want := "更新不可用，请从页面重新安装"
	if len(f.flashes) != 1 || f.flashes[0] != want || f.status != want {
		t.Fatalf("flashes %q, status %q; want %q once", f.flashes, f.status, want)
	}
	before := p.requests.Load()
	for range 3 {
		r.check(context.Background())
	}
	if got := p.requests.Load(); got != before {
		t.Fatalf("%d requests after the token was revoked, want none", got-before)
	}
	if len(f.flashes) != 1 || len(f.asked) != 0 {
		t.Fatalf("after revocation: flashes %q, prompts %v; want the one flash, no prompt", f.flashes, f.asked)
	}
}

// TestDevBuildNeverPolls: a build without the release ldflags — the dev
// install's — has no channel, no feed URL and no token, and its loop
// returns at once without a request.
func TestDevBuildNeverPolls(t *testing.T) {
	cfg := releaseConfig()
	switch {
	case app.Channel != "":
		// the staging build's own check phase: the ldflags are set on purpose
		cfg.Channel, cfg.Token = "", ""
	case app.FeedURL != "" || app.PageToken != "":
		t.Fatalf("a build with no channel carries feed %q, token set %v", app.FeedURL, app.PageToken != "")
	case cfg.Channel != "" || cfg.FeedURL != "" || cfg.Token != "":
		t.Fatalf("releaseConfig = %+v; want no channel, feed or token", cfg)
	}
	p := newFriendsPage(t, "v9.0.0")
	cfg.FeedURL = p.srv.URL + "/d/" + pageSlug + "/" + feedKey + "?raw=1" // even a feed it could reach
	cfg.HTTPClient = p.srv.Client()
	f := &fakeApp{answer: choiceUpdate}
	r, err := newReleases(cfg, t.TempDir()+"/MegaVoice.app", f.ui())
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan struct{})
	go func() { r.run(context.Background(), time.Millisecond); close(done) }()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("a dev build's update loop kept running")
	}
	r.check(context.Background())
	if got := p.requests.Load(); got != 0 || len(f.asked) != 0 {
		t.Fatalf("dev build: %d requests, prompts %v; want none", got, f.asked)
	}
}
