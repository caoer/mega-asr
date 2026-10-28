package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
)

// modelsRef is the record on the release page that lists the engine's model
// files, beside the feed: {"files": [{name, url, sha256, size}]}, url a /f/
// file. scripts/release-mac.sh writes it from the repo's annexed models/.
const modelsRef = "models.megavoice-mac?raw=1"

type modelFile struct {
	Name   string `json:"name"`
	URL    string `json:"url"`
	SHA256 string `json:"sha256"`
	Size   int64  `json:"size"`
}

// fetcher GETs a ref on the release page with the page grant
// (update.Updater.Fetch).
type fetcher interface {
	Fetch(ctx context.Context, ref string, header http.Header) (*http.Response, error)
}

// fetchModels downloads the named model files into root/models from the
// release page: each goes to <name>.part, resumed by Range after an
// interruption, is checked against the record's sha256 and only then renamed
// into place. progress gets the bytes on disk and the total.
func fetchModels(ctx context.Context, f fetcher, root string, names []string, progress func(done, total int64)) error {
	resp, err := f.Fetch(ctx, modelsRef, http.Header{"Accept": {"application/json"}})
	if err != nil {
		return err
	}
	var set struct {
		Files []modelFile `json:"files"`
	}
	err = json.NewDecoder(resp.Body).Decode(&set)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("models record: %s", resp.Status)
	}
	if err != nil {
		return fmt.Errorf("models record: %w", err)
	}
	dir := filepath.Join(root, "models")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	var want []modelFile
	var total, done int64
	for _, n := range names {
		i := -1
		for j, m := range set.Files {
			if m.Name == n {
				i = j
			}
		}
		if i < 0 {
			return fmt.Errorf("models record lists no %s", n)
		}
		want = append(want, set.Files[i])
		total += set.Files[i].Size
		if fi, err := os.Stat(filepath.Join(dir, n+".part")); err == nil {
			done += min(fi.Size(), set.Files[i].Size)
		}
	}
	progress(done, total)
	for _, m := range want {
		before := done
		if fi, err := os.Stat(filepath.Join(dir, m.Name+".part")); err == nil {
			before -= min(fi.Size(), m.Size)
		}
		if err := fetchModel(ctx, f, dir, m, func(n int64) { progress(before+n, total) }); err != nil {
			return fmt.Errorf("%s: %w", m.Name, err)
		}
		done = before + m.Size
	}
	return nil
}

var errModelChecksum = errors.New("sha256 mismatch: the download is discarded")

func fetchModel(ctx context.Context, f fetcher, dir string, m modelFile, progress func(onDisk int64)) error {
	want, err := hex.DecodeString(strings.ToLower(m.SHA256))
	if err != nil || len(want) != sha256.Size {
		return fmt.Errorf("record sha256 %q", m.SHA256)
	}
	part := filepath.Join(dir, m.Name+".part")
	h := sha256.New()
	var have int64
	if p, err := os.Open(part); err == nil {
		have, _ = io.Copy(h, p)
		p.Close()
	}
	if have > m.Size {
		os.Remove(part)
		h.Reset()
		have = 0
	}
	if have < m.Size {
		var hdr http.Header
		if have > 0 {
			hdr = http.Header{"Range": {fmt.Sprintf("bytes=%d-", have)}}
		}
		resp, err := f.Fetch(ctx, m.URL, hdr)
		if err != nil {
			return err
		}
		defer resp.Body.Close()
		flags := os.O_CREATE | os.O_WRONLY
		switch {
		case resp.StatusCode == http.StatusPartialContent && have > 0:
			flags |= os.O_APPEND
		case resp.StatusCode == http.StatusOK: // the range was not honoured: from the start
			flags |= os.O_TRUNC
			h.Reset()
			have = 0
		default:
			return fmt.Errorf("download: %s", resp.Status)
		}
		out, err := os.OpenFile(part, flags, 0o644)
		if err != nil {
			return err
		}
		w := &progressWriter{n: have, fn: progress}
		_, err = io.Copy(io.MultiWriter(out, h, w), resp.Body)
		if cerr := out.Close(); err == nil {
			err = cerr
		}
		if err != nil {
			return fmt.Errorf("download: %w", err) // the part stays: the next try resumes it
		}
	}
	if got := h.Sum(nil); string(got) != string(want) {
		os.Remove(part)
		return errModelChecksum
	}
	progress(m.Size)
	return os.Rename(part, filepath.Join(dir, m.Name))
}

type progressWriter struct {
	n  int64
	fn func(int64)
}

func (w *progressWriter) Write(b []byte) (int, error) {
	w.n += int64(len(b))
	w.fn(w.n)
	return len(b), nil
}
