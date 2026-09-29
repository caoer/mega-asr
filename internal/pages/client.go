// Package pages is megameet's client for a ccc-pages page: its records
// (/d/, compare-and-swap by version) and its files (/f/, single-shot or
// multipart). It authenticates as the page owner (a ucc Bearer) or as a
// token holder: the token's secret unlocks a grant cookie, the cookie jar
// keeps every Set-Cookie the service sends (renewals ride ordinary
// responses), and a 401 unlocks once more and retries.
package pages

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/caoer/mega-asr/internal/app"
)

// Client talks to one page.
type Client struct {
	Base string // the service, e.g. https://pages.example
	Slug string // the page

	// One credential: Bearer ("user:token", the owner's) or Secret (a
	// token's secret, exchanged at /unlock for a grant cookie).
	Bearer string
	Secret string

	Jar       string // grant cookies, kept 0600; "" keeps them in memory
	Ledger    string // multipart uploads in flight, for resume; "" resumes nothing across runs
	PartBytes int64  // multipart part size
	HTTP      *http.Client

	mu      sync.Mutex
	cookies map[string]string // name → value
	loaded  bool
}

// FromConfig builds the client [meeting.page] and [meeting.upload]
// describe; its jar and ledger live under [meeting] data.
func FromConfig(m app.MeetingConfig) (*Client, error) {
	p := m.Page
	if p.Slug == "" {
		return nil, errors.New("meeting.page.slug is empty: no page to talk to")
	}
	c := &Client{
		Base:      strings.TrimRight(p.URL, "/"),
		Slug:      p.Slug,
		Jar:       filepath.Join(m.Data, "pages", p.Slug+".jar"),
		Ledger:    filepath.Join(m.Data, "pages", "uploads"),
		PartBytes: int64(m.Upload.PartMiB) << 20,
	}
	switch {
	case p.Identity == "ucc":
		b, err := OwnerBearer()
		if err != nil {
			return nil, err
		}
		c.Bearer = b
	case p.TokenFile != "":
		b, err := os.ReadFile(p.TokenFile)
		if err != nil {
			return nil, fmt.Errorf("meeting.page.token_file: %w", err)
		}
		if c.Secret = strings.TrimSpace(string(b)); c.Secret == "" {
			return nil, fmt.Errorf("meeting.page.token_file: %s is empty", p.TokenFile)
		}
	default:
		return nil, errors.New("meeting.page: set token_file (a device) or identity = \"ucc\" (the owner)")
	}
	return c, nil
}

// OwnerBearer is the owner's credential, "UCC_USERNAME:UCC_AUTH_TOKEN":
// from the environment when both are set (a page hook's tick sets them),
// else read from $UCC_HOME/user-env.sh.
func OwnerBearer() (string, error) {
	if u, t := os.Getenv("UCC_USERNAME"), os.Getenv("UCC_AUTH_TOKEN"); u != "" && t != "" {
		return u + ":" + t, nil
	}
	home := os.Getenv("UCC_HOME")
	if home == "" {
		h, _ := os.UserHomeDir()
		home = filepath.Join(h, ".local", "share", "ucc")
	}
	path := filepath.Join(home, "user-env.sh")
	f, err := os.Open(path)
	if err != nil {
		return "", fmt.Errorf("owner identity: %w", err)
	}
	defer f.Close()
	vars := map[string]string{}
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := strings.TrimPrefix(strings.TrimSpace(sc.Text()), "export ")
		k, v, ok := strings.Cut(line, "=")
		if !ok || (k != "UCC_USERNAME" && k != "UCC_AUTH_TOKEN") {
			continue
		}
		v = strings.TrimSpace(v)
		if len(v) >= 2 && (v[0] == '"' || v[0] == '\'') && v[len(v)-1] == v[0] {
			v = v[1 : len(v)-1]
		}
		vars[k] = v
	}
	if vars["UCC_USERNAME"] == "" || vars["UCC_AUTH_TOKEN"] == "" {
		return "", fmt.Errorf("owner identity: %s sets no UCC_USERNAME and UCC_AUTH_TOKEN", path)
	}
	return vars["UCC_USERNAME"] + ":" + vars["UCC_AUTH_TOKEN"], nil
}

// Error is a refusal from the service: {error, code, version?, missing?}.
type Error struct {
	Status  int
	Code    string `json:"code"`
	Message string `json:"error"`
	Version int    `json:"version"` // version_conflict: the record's current version
	Missing []int  `json:"missing"` // parts_missing: part numbers not landed
}

func (e *Error) Error() string {
	return fmt.Sprintf("pages: %d %s: %s", e.Status, e.Code, e.Message)
}

// Code is the refusal code err carries, or "".
func Code(err error) string {
	var e *Error
	if errors.As(err, &e) {
		return e.Code
	}
	return ""
}

// A write refused 429 is retried after rateWait, up to rateTries times: the
// service's write window is 60 s.
var (
	rateWait  = 10 * time.Second
	rateTries = 12
)

// request is one call; body is re-made for the retry after an unlock.
type request struct {
	method, path string
	query        url.Values
	header       http.Header
	body         func() (io.Reader, int64, error)
}

// do sends r with the credential, keeps every Set-Cookie, on a 401 unlocks
// and sends it once more, and on a 429 waits and retries. A non-2xx answer
// is an *Error.
func (c *Client) do(ctx context.Context, r request) (*http.Response, error) {
	if c.Bearer == "" {
		c.mu.Lock()
		err := c.load()
		empty := len(c.cookies) == 0
		c.mu.Unlock()
		if err != nil {
			return nil, err
		}
		if empty {
			if err := c.Unlock(ctx); err != nil {
				return nil, err
			}
		}
	}
	unlocked := false
	for limited := 0; ; {
		resp, err := c.send(ctx, r)
		if err != nil {
			return nil, err
		}
		if resp.StatusCode == http.StatusUnauthorized && c.Bearer == "" && !unlocked {
			drain(resp)
			unlocked = true
			if err := c.Unlock(ctx); err != nil {
				return nil, err
			}
			continue
		}
		// A grant gets 60 writes a minute per page per IP; a burst of writes
		// waits the window out instead of failing.
		if resp.StatusCode == http.StatusTooManyRequests && limited < rateTries {
			drain(resp)
			limited++
			select {
			case <-ctx.Done():
				return nil, ctx.Err()
			case <-time.After(rateWait):
			}
			continue
		}
		if resp.StatusCode/100 != 2 {
			defer drain(resp)
			return nil, refusal(resp)
		}
		return resp, nil
	}
}

func (c *Client) send(ctx context.Context, r request) (*http.Response, error) {
	u := c.Base + r.path
	if len(r.query) > 0 {
		u += "?" + r.query.Encode()
	}
	var body io.Reader
	var n int64
	if r.body != nil {
		var err error
		if body, n, err = r.body(); err != nil {
			return nil, err
		}
	}
	req, err := http.NewRequestWithContext(ctx, r.method, u, body)
	if err != nil {
		return nil, err
	}
	req.ContentLength = n
	if body != nil && n == 0 {
		req.Body = http.NoBody
	}
	for k, v := range r.header {
		req.Header[k] = v
	}
	if c.Bearer != "" {
		req.Header.Set("Authorization", "Bearer "+c.Bearer)
	} else {
		c.mu.Lock()
		for name, v := range c.cookies {
			req.AddCookie(&http.Cookie{Name: name, Value: v})
		}
		c.mu.Unlock()
		if r.method != http.MethodGet && r.method != http.MethodHead {
			req.Header.Set("x-ccc-auth", "cookie")
		}
	}
	resp, err := c.client().Do(req)
	if err != nil {
		return nil, err
	}
	if err := c.keep(resp); err != nil {
		drain(resp)
		return nil, err
	}
	return resp, nil
}

func (c *Client) client() *http.Client {
	if c.HTTP != nil {
		return c.HTTP
	}
	return http.DefaultClient
}

func drain(resp *http.Response) {
	io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<20))
	resp.Body.Close()
}

func refusal(resp *http.Response) error {
	e := &Error{Status: resp.StatusCode}
	b, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
	if json.Unmarshal(b, e) != nil || e.Code == "" {
		e.Code, e.Message = "http_"+strconv.Itoa(resp.StatusCode), strings.TrimSpace(string(b))
	}
	return e
}

// Unlock exchanges the token's secret for a grant cookie.
func (c *Client) Unlock(ctx context.Context) error {
	if c.Secret == "" {
		return errors.New("pages: no token secret to unlock with")
	}
	body, _ := json.Marshal(map[string]string{"slug": c.Slug, "token": c.Secret})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.Base+"/unlock", bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("content-type", "application/json")
	resp, err := c.client().Do(req)
	if err != nil {
		return err
	}
	defer drain(resp)
	if err := c.keep(resp); err != nil {
		return err
	}
	if resp.StatusCode != http.StatusOK {
		return refusal(resp)
	}
	return nil
}

// keep applies a response's Set-Cookie headers to the jar and saves it.
func (c *Client) keep(resp *http.Response) error {
	set := resp.Cookies()
	if len(set) == 0 {
		return nil
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if err := c.load(); err != nil {
		return err
	}
	for _, k := range set {
		if k.MaxAge < 0 || (!k.Expires.IsZero() && k.Expires.Before(time.Now())) {
			delete(c.cookies, k.Name)
		} else {
			c.cookies[k.Name] = k.Value
		}
	}
	return c.save()
}

// load reads the jar once; the caller holds mu.
func (c *Client) load() error {
	if c.loaded {
		return nil
	}
	c.loaded, c.cookies = true, map[string]string{}
	if c.Jar == "" {
		return nil
	}
	b, err := os.ReadFile(c.Jar)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("pages: jar: %w", err)
	}
	if err := json.Unmarshal(b, &c.cookies); err != nil {
		return fmt.Errorf("pages: jar %s: %w", c.Jar, err)
	}
	return nil
}

// save writes the jar 0600, whole or not at all; the caller holds mu.
func (c *Client) save() error {
	if c.Jar == "" {
		return nil
	}
	b, _ := json.Marshal(c.cookies)
	return writeFile(c.Jar, b)
}

func writeFile(path string, b []byte) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), ".tmp-*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if _, err := tmp.Write(b); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Chmod(0o600); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), path)
}

// Value is a record's envelope.
type Value struct {
	Schema string          `json:"schema"`
	Data   json.RawMessage `json:"data"`
}

// Object is a stored record.
type Object struct {
	Key       string `json:"key"`
	Value     Value  `json:"value"`
	Version   int    `json:"version"`
	UpdatedAt int64  `json:"updated_at"`
}

// Create writes a record that must not exist yet (v=0) and returns its
// version.
func (c *Client) Create(ctx context.Context, key, schema string, data any) (int, error) {
	return c.CAS(ctx, key, schema, data, 0)
}

// CAS replaces a record at version and returns the new version; another
// writer's change in between is a version_conflict *Error naming the
// current version.
func (c *Client) CAS(ctx context.Context, key, schema string, data any, version int) (int, error) {
	d, err := json.Marshal(data)
	if err != nil {
		return 0, err
	}
	body, _ := json.Marshal(Value{Schema: schema, Data: d})
	resp, err := c.do(ctx, request{
		method: http.MethodPut, path: "/d/" + c.Slug + "/" + key,
		query:  url.Values{"v": {strconv.Itoa(version)}},
		header: http.Header{"Content-Type": {"application/json"}},
		body:   func() (io.Reader, int64, error) { return bytes.NewReader(body), int64(len(body)), nil },
	})
	if err != nil {
		return 0, err
	}
	defer drain(resp)
	var out struct {
		Version int `json:"version"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return 0, fmt.Errorf("pages: put %s: %w", key, err)
	}
	return out.Version, nil
}

// Record reads one record; absent is a no_such_object *Error.
func (c *Client) Record(ctx context.Context, key string) (Object, error) {
	var o Object
	resp, err := c.do(ctx, request{method: http.MethodGet, path: "/d/" + c.Slug + "/" + key})
	if err != nil {
		return o, err
	}
	defer drain(resp)
	return o, json.NewDecoder(resp.Body).Decode(&o)
}

// Delete removes a record; absent is a no_such_object *Error.
func (c *Client) Delete(ctx context.Context, key string) error {
	resp, err := c.do(ctx, request{method: http.MethodDelete, path: "/d/" + c.Slug + "/" + key})
	if err != nil {
		return err
	}
	drain(resp)
	return nil
}

// List reads every record whose key starts with prefix, with its value,
// following the cursor.
func (c *Client) List(ctx context.Context, prefix string) ([]Object, error) {
	var all []Object
	after := ""
	for {
		q := url.Values{"prefix": {prefix}, "values": {"1"}}
		if after != "" {
			q.Set("after", after)
		}
		resp, err := c.do(ctx, request{method: http.MethodGet, path: "/d/" + c.Slug, query: q})
		if err != nil {
			return all, err
		}
		var page struct {
			Keys   []Object `json:"keys"`
			Cursor *string  `json:"cursor"`
		}
		err = json.NewDecoder(resp.Body).Decode(&page)
		drain(resp)
		if err != nil {
			return all, fmt.Errorf("pages: list %s: %w", prefix, err)
		}
		all = append(all, page.Keys...)
		if page.Cursor == nil || *page.Cursor == "" {
			return all, nil
		}
		after = *page.Cursor
	}
}

// Get reads a file's bytes from off; n > 0 reads that many, else to the end.
func (c *Client) Get(ctx context.Context, fileID string, off, n int64) (io.ReadCloser, error) {
	h := http.Header{}
	switch {
	case n > 0:
		h.Set("Range", fmt.Sprintf("bytes=%d-%d", off, off+n-1))
	case off > 0:
		h.Set("Range", fmt.Sprintf("bytes=%d-", off))
	}
	resp, err := c.do(ctx, request{method: http.MethodGet, path: "/f/" + c.Slug + "/" + fileID, header: h})
	if err != nil {
		return nil, err
	}
	return resp.Body, nil
}

// DeleteFile removes a file, its record and, for an upload in flight, the
// R2 multipart upload.
func (c *Client) DeleteFile(ctx context.Context, fileID string) error {
	resp, err := c.do(ctx, request{method: http.MethodDelete, path: "/f/" + c.Slug + "/" + fileID})
	if err != nil {
		return err
	}
	drain(resp)
	return nil
}
