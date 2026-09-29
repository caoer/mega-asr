package pages

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"mime"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// singleMax is the largest file sent in one request; from it on a file
// goes in parts (the service's request cap is 100 MiB).
var singleMax int64 = 64 << 20

// types the upload names where the host's MIME table may not.
var types = map[string]string{
	".flac": "audio/flac", ".wav": "audio/wav", ".m4a": "audio/mp4", ".mp3": "audio/mpeg",
	".ogg": "audio/ogg", ".opus": "audio/ogg", ".mp4": "video/mp4", ".json": "application/json",
	".jsonl": "application/json", ".md": "text/plain; charset=utf-8", ".txt": "text/plain; charset=utf-8",
}

// uploadTTL is how long R2 keeps an unfinished multipart upload; an older
// one is restarted rather than resumed.
const uploadTTL = 7 * 24 * time.Hour

// fileRecord is what Upload reads back from a file's f.<id> record.
type fileRecord struct {
	SHA256         string `json:"sha256"`
	SHA256Declared string `json:"sha256_declared"`
	State          string `json:"state"`
	UploadID       string `json:"upload_id"`
	Size           int64  `json:"size"`
	PartBytes      int64  `json:"part_bytes"`
	Parts          []struct {
		N int `json:"n"`
	} `json:"parts"`
}

type created struct {
	ID   string     `json:"id"`
	File fileRecord `json:"file"`
}

// Upload stores the file at path on the page with meta as the record's
// meta, and returns its id and SHA-256. A file under 64 MiB goes in one
// request (the service computes the digest); a larger one goes in parts,
// serially, and a rerun after a failure resumes it from the parts its
// record says have landed.
func (c *Client) Upload(ctx context.Context, path string, meta map[string]any) (id, sum string, err error) {
	f, err := os.Open(path)
	if err != nil {
		return "", "", err
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil {
		return "", "", err
	}
	q := url.Values{"name": {filepath.Base(path)}}
	if len(meta) > 0 {
		m, err := json.Marshal(meta)
		if err != nil {
			return "", "", err
		}
		q.Set("meta", string(m))
	}
	ext := strings.ToLower(filepath.Ext(path))
	typ := types[ext]
	if typ == "" {
		typ = mime.TypeByExtension(ext)
	}
	if typ == "" {
		typ = "application/octet-stream"
	}
	if st.Size() < singleMax {
		return c.uploadSingle(ctx, f, st.Size(), typ, q)
	}
	return c.uploadParts(ctx, f, st.Size(), typ, q)
}

func (c *Client) uploadSingle(ctx context.Context, f *os.File, size int64, typ string, q url.Values) (string, string, error) {
	resp, err := c.do(ctx, request{
		method: http.MethodPost, path: "/f/" + c.Slug, query: q,
		header: http.Header{"Content-Type": {typ}},
		body: func() (io.Reader, int64, error) {
			return io.NewSectionReader(f, 0, size), size, nil
		},
	})
	if err != nil {
		return "", "", err
	}
	defer drain(resp)
	var out created
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return "", "", fmt.Errorf("pages: upload: %w", err)
	}
	return out.ID, out.File.SHA256, nil
}

// pending is the ledger's entry for a multipart upload in flight.
type pending struct {
	ID      string    `json:"id"`
	Started time.Time `json:"started"`
}

func (c *Client) ledgerPath(sum string) string {
	if c.Ledger == "" {
		return ""
	}
	return filepath.Join(c.Ledger, c.Slug+"-"+sum+".json")
}

func (c *Client) uploadParts(ctx context.Context, f *os.File, size int64, typ string, q url.Values) (string, string, error) {
	h := sha256.New()
	if _, err := io.Copy(h, io.NewSectionReader(f, 0, size)); err != nil {
		return "", "", err
	}
	sum := hex.EncodeToString(h.Sum(nil))
	ledger := c.ledgerPath(sum)
	for restart := 0; ; restart++ {
		id, landed, err := c.resume(ctx, ledger)
		if err != nil {
			return "", "", err
		}
		if id != "" && landed == nil { // already complete
			os.Remove(ledger)
			return id, sum, nil
		}
		part := c.PartBytes
		if id == "" {
			if id, part, err = c.createParts(ctx, size, typ, sum, q); err != nil {
				return "", "", err
			}
			landed = map[int]bool{}
			if ledger != "" {
				b, _ := json.Marshal(pending{ID: id, Started: time.Now()})
				if err := writeFile(ledger, b); err != nil {
					return "", "", err
				}
			}
		} else if part, err = c.partBytes(ctx, id); err != nil {
			return "", "", err
		}
		err = c.sendParts(ctx, f, size, id, part, landed)
		if err == nil {
			err = c.complete(ctx, id)
		}
		if Code(err) == "upload_gone" && restart == 0 {
			log.Printf("pages: %s: %v; starting it again", id, err)
			if err := c.DeleteFile(ctx, id); err != nil && Code(err) != "no_such_file" {
				return "", "", err
			}
			os.Remove(ledger)
			continue
		}
		if err != nil {
			return "", "", err
		}
		os.Remove(ledger)
		return id, sum, nil
	}
}

// resume reads the ledger and the record it names: an id and the parts
// already landed for an upload to continue; an id and nil for one that is
// complete; "" for none (none recorded, gone, or older than R2 keeps it —
// the stale one is deleted).
func (c *Client) resume(ctx context.Context, ledger string) (string, map[int]bool, error) {
	if ledger == "" {
		return "", nil, nil
	}
	b, err := os.ReadFile(ledger)
	if errors.Is(err, os.ErrNotExist) {
		return "", nil, nil
	}
	if err != nil {
		return "", nil, err
	}
	var p pending
	if err := json.Unmarshal(b, &p); err != nil || p.ID == "" {
		os.Remove(ledger)
		return "", nil, nil
	}
	if time.Since(p.Started) > uploadTTL {
		log.Printf("pages: %s started %s, past R2's %v; starting it again", p.ID, p.Started.Format(time.RFC3339), uploadTTL)
		if err := c.DeleteFile(ctx, p.ID); err != nil && Code(err) != "no_such_file" {
			return "", nil, err
		}
		os.Remove(ledger)
		return "", nil, nil
	}
	o, err := c.Record(ctx, "f."+p.ID)
	if Code(err) == "no_such_object" {
		os.Remove(ledger)
		return "", nil, nil
	}
	if err != nil {
		return "", nil, err
	}
	var r fileRecord
	if err := json.Unmarshal(o.Value.Data, &r); err != nil {
		return "", nil, fmt.Errorf("pages: f.%s: %w", p.ID, err)
	}
	if r.State == "" || r.State == "ready" {
		return p.ID, nil, nil
	}
	landed := map[int]bool{}
	for _, x := range r.Parts {
		landed[x.N] = true
	}
	return p.ID, landed, nil
}

func (c *Client) partBytes(ctx context.Context, id string) (int64, error) {
	o, err := c.Record(ctx, "f."+id)
	if err != nil {
		return 0, err
	}
	var r fileRecord
	if err := json.Unmarshal(o.Value.Data, &r); err != nil || r.PartBytes <= 0 {
		return 0, fmt.Errorf("pages: f.%s has no part_bytes", id)
	}
	return r.PartBytes, nil
}

func (c *Client) createParts(ctx context.Context, size int64, typ, sum string, q url.Values) (string, int64, error) {
	part := c.PartBytes
	if part <= 0 {
		part = 16 << 20
	}
	mq := url.Values{}
	for k, v := range q {
		mq[k] = v
	}
	mq.Set("multipart", "1")
	mq.Set("bytes", strconv.FormatInt(size, 10))
	mq.Set("part_bytes", strconv.FormatInt(part, 10))
	mq.Set("sha256", sum)
	resp, err := c.do(ctx, request{method: http.MethodPost, path: "/f/" + c.Slug, query: mq, header: http.Header{"Content-Type": {typ}}})
	if err != nil {
		return "", 0, err
	}
	defer drain(resp)
	var out created
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil || out.ID == "" {
		return "", 0, fmt.Errorf("pages: multipart create: no id (%v)", err)
	}
	return out.ID, part, nil
}

func (c *Client) sendParts(ctx context.Context, f *os.File, size int64, id string, part int64, landed map[int]bool) error {
	count := int((size + part - 1) / part)
	for n := 1; n <= count; n++ {
		if landed[n] {
			continue
		}
		off := int64(n-1) * part
		length := min(part, size-off)
		resp, err := c.do(ctx, request{
			method: http.MethodPut, path: fmt.Sprintf("/f/%s/%s/parts/%d", c.Slug, id, n),
			header: http.Header{"Content-Type": {"application/octet-stream"}},
			body: func() (io.Reader, int64, error) {
				return io.NewSectionReader(f, off, length), length, nil
			},
		})
		if err != nil {
			return fmt.Errorf("part %d of %d: %w", n, count, err)
		}
		drain(resp)
	}
	return nil
}

func (c *Client) complete(ctx context.Context, id string) error {
	resp, err := c.do(ctx, request{
		method: http.MethodPost, path: "/f/" + c.Slug + "/" + id + "/complete",
		body: func() (io.Reader, int64, error) { return strings.NewReader(""), 0, nil },
	})
	if err != nil {
		return err
	}
	drain(resp)
	return nil
}
