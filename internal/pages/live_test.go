//go:build live

// go test -tags live ./internal/pages — against a real ccc-pages service
// (PAGES_URL, required) with the owner identity from
// $UCC_HOME/user-env.sh. Each test publishes a private scratch page with a
// data store, works on it with an editor token, and deletes the page.
//
//	PAGES_LIVE_WAIT=31m   how long TestLive idles to span the grant's renewal
//	                      half-life (GRANT_TTL_S 3600 / 2); 0 skips the span
//	PAGES_LIVE_MULTIPART=1  also run TestLiveMultipart (needs the multipart routes)
package pages

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func liveBase(t *testing.T) string {
	u := os.Getenv("PAGES_URL")
	if u == "" {
		t.Skip("PAGES_URL unset: the ccc-pages service to test against")
	}
	return strings.TrimRight(u, "/")
}

// counting counts /unlock requests.
type counting struct{ unlocks atomic.Int32 }

func (c *counting) RoundTrip(r *http.Request) (*http.Response, error) {
	if r.URL.Path == "/unlock" {
		c.unlocks.Add(1)
	}
	return http.DefaultTransport.RoundTrip(r)
}

// scratch publishes a private page with a data store and mints an editor
// token for it; the page (with its records, files and tokens) is deleted
// when the test ends.
func scratch(t *testing.T) (owner, editor *Client, tr *counting) {
	t.Helper()
	base := liveBase(t)
	bearer, err := OwnerBearer()
	if err != nil {
		t.Fatal(err)
	}
	rnd := make([]byte, 10)
	rand.Read(rnd)
	slug := "live-" + hex.EncodeToString(rnd)
	call := func(method, path string, body []byte, hdr map[string]string) []byte {
		req, _ := http.NewRequest(method, base+path, bytes.NewReader(body))
		req.Header.Set("Authorization", "Bearer "+bearer)
		for k, v := range hdr {
			req.Header.Set(k, v)
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		b, _ := io.ReadAll(resp.Body)
		if resp.StatusCode/100 != 2 {
			t.Fatalf("%s %s: %d %s", method, path, resp.StatusCode, b)
		}
		return b
	}
	call("PUT", "/a/"+slug, []byte("<!doctype html><title>megameet live test</title><p>scratch"), map[string]string{
		"content-type": "text/html", "x-page-visibility": "private", "x-page-data": "on",
	})
	t.Cleanup(func() { call("DELETE", "/a/"+slug, nil, nil); t.Logf("deleted %s", slug) })
	t.Logf("scratch page %s/a/%s", base, slug)
	mint, _ := json.Marshal(map[string]any{"scope": slug, "role": "editor", "label": "megameet-live-test",
		"expires": time.Now().Add(3 * time.Hour).UTC().Format(time.RFC3339)})
	var tok struct{ Secret string }
	json.Unmarshal(call("POST", "/t", mint, map[string]string{"content-type": "application/json"}), &tok)
	if tok.Secret == "" {
		t.Fatal("mint: no secret")
	}
	dir := t.TempDir()
	tr = &counting{}
	owner = &Client{Base: base, Slug: slug, Bearer: bearer}
	editor = &Client{Base: base, Slug: slug, Secret: tok.Secret, Jar: filepath.Join(dir, "jar"), Ledger: filepath.Join(dir, "uploads"),
		PartBytes: 16 << 20, HTTP: &http.Client{Transport: tr}}
	return owner, editor, tr
}

func grantValue(t *testing.T, c *Client) string {
	t.Helper()
	b, err := os.ReadFile(c.Jar)
	if err != nil {
		t.Fatal(err)
	}
	st, _ := os.Stat(c.Jar)
	if st.Mode().Perm() != 0o600 {
		t.Fatalf("jar mode %v", st.Mode().Perm())
	}
	var m map[string]string
	json.Unmarshal(b, &m)
	for k, v := range m {
		if strings.HasPrefix(k, "__Host-ccc_g_") {
			return v
		}
	}
	t.Fatalf("no grant cookie in the jar: %s", b)
	return ""
}

func TestLive(t *testing.T) {
	ctx := context.Background()
	owner, ed, tr := scratch(t)

	v, err := ed.Create(ctx, "rec.20200412-093000-file-live", "meeting@1", map[string]any{"state": "uploading"})
	if err != nil || v != 1 {
		t.Fatalf("create: v%d %v", v, err)
	}
	if _, err := ed.Create(ctx, "rec.20200412-093000-file-live", "meeting@1", map[string]any{}); Code(err) != "version_conflict" {
		t.Fatalf("second create: %v", err)
	}
	if v, err = ed.CAS(ctx, "rec.20200412-093000-file-live", "meeting@1", map[string]any{"state": "uploaded"}, 1); err != nil || v != 2 {
		t.Fatalf("cas: v%d %v", v, err)
	}
	if _, err := ed.CAS(ctx, "rec.20200412-093000-file-live", "meeting@1", map[string]any{}, 1); Code(err) != "version_conflict" {
		t.Fatalf("stale cas: %v", err)
	}

	body := make([]byte, 300<<10)
	rand.Read(body)
	path := filepath.Join(t.TempDir(), "mic.flac")
	os.WriteFile(path, body, 0o644)
	id, sum, err := ed.Upload(ctx, path, map[string]any{"rec": "rec.20200412-093000-file-live", "role": "mic"})
	want := sha256.Sum256(body)
	if err != nil || sum != hex.EncodeToString(want[:]) {
		t.Fatalf("upload: %s %s %v", id, sum, err)
	}
	rc, err := ed.Get(ctx, id, 1000, 4096)
	if err != nil {
		t.Fatal(err)
	}
	got, _ := io.ReadAll(rc)
	rc.Close()
	if !bytes.Equal(got, body[1000:1000+4096]) {
		t.Fatalf("range read: %d bytes, differ", len(got))
	}
	recs, err := owner.List(ctx, "rec.")
	if err != nil || len(recs) != 1 || recs[0].Version != 2 {
		t.Fatalf("owner list: %+v %v", recs, err)
	}
	files, err := ed.List(ctx, "f.")
	if err != nil || len(files) != 1 {
		t.Fatalf("editor list f.: %d %v", len(files), err)
	}
	if n := tr.unlocks.Load(); n != 1 {
		t.Fatalf("%d unlocks before the wait", n)
	}

	wait, _ := time.ParseDuration(os.Getenv("PAGES_LIVE_WAIT"))
	if wait <= 0 {
		t.Log("PAGES_LIVE_WAIT unset: the renewal half-life is not spanned")
		return
	}
	before := grantValue(t, ed)
	t.Logf("idling %v to span the grant's half-life (GRANT_TTL_S 3600, renewed past 1800 s)", wait)
	time.Sleep(wait)
	if v, err = ed.CAS(ctx, "rec.20200412-093000-file-live", "meeting@1", map[string]any{"state": "processing"}, 2); err != nil || v != 3 {
		t.Fatalf("cas after the half-life: v%d %v", v, err)
	}
	after := grantValue(t, ed)
	t.Logf("after %v: grant renewed %v, unlocks %d", wait, before != after, tr.unlocks.Load())
	if before == after {
		t.Fatal("no renewal Set-Cookie was kept past the half-life")
	}
	if n := tr.unlocks.Load(); n != 1 {
		t.Fatalf("%d unlocks: the renewal should have carried the grant", n)
	}
}

// A file above the single-request limit, cut off mid-way and resumed.
func TestLiveMultipart(t *testing.T) {
	if os.Getenv("PAGES_LIVE_MULTIPART") != "1" {
		t.Skip("PAGES_LIVE_MULTIPART=1 runs it against a service with the multipart routes")
	}
	_, ed, _ := scratch(t)
	body := make([]byte, 70<<20)
	rand.Read(body)
	path := filepath.Join(t.TempDir(), "remote.flac")
	os.WriteFile(path, body, 0o644)
	want := sha256.Sum256(body)

	// cut off after the second part lands
	ctx, cancel := context.WithCancel(context.Background())
	parts := 0
	inner := ed.HTTP.Transport
	ed.HTTP = &http.Client{Transport: roundTripper(func(r *http.Request) (*http.Response, error) {
		resp, err := inner.RoundTrip(r)
		if r.Method == "PUT" && strings.Contains(r.URL.Path, "/parts/") {
			if parts++; parts == 2 {
				cancel()
			}
		}
		return resp, err
	})}
	if _, _, err := ed.Upload(ctx, path, nil); err == nil {
		t.Fatal("the cut-off upload finished")
	}
	t.Logf("cut off after %d part requests", parts)
	ed.HTTP = &http.Client{Transport: inner}
	parts = 0
	id, sum, err := ed.Upload(context.Background(), path, nil)
	if err != nil || sum != hex.EncodeToString(want[:]) {
		t.Fatalf("resume: %s %v", sum, err)
	}
	rc, err := ed.Get(context.Background(), id, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	h := sha256.New()
	io.Copy(h, rc)
	rc.Close()
	if hex.EncodeToString(h.Sum(nil)) != sum {
		t.Fatal("the download's sha256 differs")
	}
}

type roundTripper func(*http.Request) (*http.Response, error)

func (f roundTripper) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }
