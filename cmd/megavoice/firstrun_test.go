package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/caoer/mega-asr/internal/app"
)

// modelPage serves the models record and the model files with byte ranges,
// and records the Range header of each file request.
type modelPage struct {
	srv    *httptest.Server
	files  map[string][]byte
	served map[string][]byte // what /f/ serves; files unless a test corrupts it
	mu     sync.Mutex
	ranges []string
}

func newModelPage(t *testing.T) *modelPage {
	p := &modelPage{files: map[string][]byte{}, served: map[string][]byte{}}
	for i, n := range app.Models {
		b := bytes.Repeat([]byte{byte('a' + i)}, 1000*(i+1))
		p.files[n], p.served[n] = b, b
	}
	p.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/d/s/models.megavoice-mac" {
			var fs []modelFile
			for _, n := range app.Models {
				sum := sha256.Sum256(p.files[n])
				fs = append(fs, modelFile{Name: n, URL: "/f/s/" + n, SHA256: hex.EncodeToString(sum[:]), Size: int64(len(p.files[n]))})
			}
			json.NewEncoder(w).Encode(map[string]any{"files": fs})
			return
		}
		n := strings.TrimPrefix(r.URL.Path, "/f/s/")
		b, ok := p.served[n]
		if !ok {
			http.NotFound(w, r)
			return
		}
		p.mu.Lock()
		p.ranges = append(p.ranges, n+" "+r.Header.Get("Range"))
		p.mu.Unlock()
		http.ServeContent(w, r, n, time.Time{}, bytes.NewReader(b))
	}))
	t.Cleanup(p.srv.Close)
	return p
}

// Fetch resolves ref against the feed's place, as update.Updater.Fetch does.
func (p *modelPage) Fetch(ctx context.Context, ref string, hdr http.Header) (*http.Response, error) {
	base, _ := url.Parse(p.srv.URL + "/d/s/release.megavoice-mac?raw=1")
	r, _ := url.Parse(ref)
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, base.ResolveReference(r).String(), nil)
	for k, v := range hdr {
		req.Header[k] = v
	}
	return http.DefaultClient.Do(req)
}

func TestFetchModelsIntoAnEmptyRoot(t *testing.T) {
	p := newModelPage(t)
	root := t.TempDir()
	var last, total int64
	if err := fetchModels(context.Background(), p, root, app.MissingModels(root), func(d, t int64) { last, total = d, t }); err != nil {
		t.Fatal(err)
	}
	if m := app.MissingModels(root); len(m) != 0 {
		t.Fatalf("missing after the fetch: %v", m)
	}
	for _, n := range app.Models {
		if b, _ := os.ReadFile(filepath.Join(root, "models", n)); !bytes.Equal(b, p.files[n]) {
			t.Fatalf("%s: wrong bytes", n)
		}
	}
	if total != 6000 || last != total {
		t.Fatalf("progress ended at %d of %d, want 6000 of 6000", last, total)
	}
}

func TestFetchModelsResumesByRange(t *testing.T) {
	p := newModelPage(t)
	root := t.TempDir()
	n := app.Models[1]
	os.MkdirAll(filepath.Join(root, "models"), 0o755)
	os.WriteFile(filepath.Join(root, "models", n+".part"), p.files[n][:700], 0o644) // interrupted at 700 of 2000
	if err := fetchModels(context.Background(), p, root, []string{n}, func(int64, int64) {}); err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(filepath.Join(root, "models", n)); !bytes.Equal(b, p.files[n]) {
		t.Fatal("resumed file differs")
	}
	if len(p.ranges) != 1 || p.ranges[0] != n+" bytes=700-" {
		t.Fatalf("file requests %q, want one with Range bytes=700-", p.ranges)
	}
}

func TestFetchModelsDiscardsABadDownload(t *testing.T) {
	p := newModelPage(t)
	root := t.TempDir()
	n := app.Models[0]
	p.served[n] = bytes.Repeat([]byte{'x'}, len(p.files[n]))
	err := fetchModels(context.Background(), p, root, []string{n}, func(int64, int64) {})
	if !errors.Is(err, errModelChecksum) {
		t.Fatalf("err = %v, want the sha256 mismatch", err)
	}
	for _, f := range []string{n, n + ".part"} {
		if _, err := os.Stat(filepath.Join(root, "models", f)); err == nil {
			t.Fatalf("%s left behind after a bad download", f)
		}
	}
}

func TestFetchModelsAsksOnlyForTheMissing(t *testing.T) {
	p := newModelPage(t)
	root := t.TempDir()
	os.MkdirAll(filepath.Join(root, "models"), 0o755)
	os.WriteFile(filepath.Join(root, "models", app.Models[0]), p.files[app.Models[0]], 0o644)
	if err := fetchModels(context.Background(), p, root, app.MissingModels(root), func(int64, int64) {}); err != nil {
		t.Fatal(err)
	}
	if len(p.ranges) != 2 || strings.HasPrefix(p.ranges[0], app.Models[0]) || strings.HasPrefix(p.ranges[1], app.Models[0]) {
		t.Fatalf("file requests %q, want the two missing only", p.ranges)
	}
}
