package pages

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

const (
	slug   = "meetings-0123456789abcdef"
	secret = "s3cret"
	cookie = "__Host-ccc_g_tok1"
)

// fake is a ccc-pages page with the behaviour the client must survive: a
// grant cookie from /unlock, renewal by Set-Cookie on ordinary responses,
// 401 once the grant is gone, x-ccc-auth on cookie writes, CAS by version,
// the request cap, and the multipart routes with the record as the ledger
// of landed parts.
type fake struct {
	t  *testing.T
	mu sync.Mutex

	revoked bool
	grant   string // the cookie value that authorizes; "" = none issued
	renewTo string // next response renews the grant to this value
	unlocks int

	records map[string]*Object
	files   map[string][]byte
	parts   map[string]map[int][]byte // upload in flight: id → part → bytes
	gone    map[string]bool           // R2 no longer holds this upload
	putsN   []int                     // part numbers received, in order
	deletes []string
	failAt  int // a part PUT with this number fails 500 once
	limited int // this many writes are refused 429 too_many_writes
	cap     int64
	nextID  int
}

func newFake(t *testing.T) (*fake, *httptest.Server) {
	f := &fake{t: t, records: map[string]*Object{}, files: map[string][]byte{}, parts: map[string]map[int][]byte{}, gone: map[string]bool{}, cap: 100 << 20}
	s := httptest.NewServer(f)
	t.Cleanup(s.Close)
	return f, s
}

func refuse(w http.ResponseWriter, status int, code, msg string, extra ...any) {
	body := map[string]any{"error": msg, "code": code}
	for i := 0; i+1 < len(extra); i += 2 {
		body[extra[i].(string)] = extra[i+1]
	}
	w.Header().Set("content-type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(body)
}

func reply(w http.ResponseWriter, status int, v any) {
	w.Header().Set("content-type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(v)
}

func (f *fake) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	parts := strings.Split(strings.Trim(r.URL.Path, "/"), "/")
	if parts[0] == "unlock" {
		var b struct{ Slug, Token string }
		json.NewDecoder(r.Body).Decode(&b)
		f.unlocks++
		switch {
		case b.Token != secret:
			refuse(w, 401, "token_invalid", "wrong")
		case f.revoked:
			refuse(w, 403, "token_revoked", "revoked")
		default:
			f.grant = "jwt" + strconv.Itoa(f.unlocks) + "~" + secret
			http.SetCookie(w, &http.Cookie{Name: cookie, Value: f.grant, Path: "/", MaxAge: 30 * 86400, Secure: true, HttpOnly: true})
			reply(w, 200, map[string]any{"ok": true, "slug": b.Slug, "role": "editor", "expires_in": 3600})
		}
		return
	}
	// every other route is the page's: the grant cookie, renewed on the way out
	c, err := r.Cookie(cookie)
	if err != nil || f.grant == "" || c.Value != f.grant {
		refuse(w, 401, "page_locked", "unlock first")
		return
	}
	if f.renewTo != "" {
		f.grant, f.renewTo = f.renewTo, ""
		http.SetCookie(w, &http.Cookie{Name: cookie, Value: f.grant, Path: "/", MaxAge: 30 * 86400})
	}
	if r.Method != "GET" && r.Method != "HEAD" && r.Header.Get("x-ccc-auth") != "cookie" {
		refuse(w, 403, "csrf_header_required", "a cookie-authenticated write must send `x-ccc-auth: cookie`")
		return
	}
	if r.Method != "GET" && r.Method != "HEAD" && f.limited > 0 {
		f.limited--
		refuse(w, 429, "too_many_writes", "too many writes for this page")
		return
	}
	switch {
	case parts[0] == "d" && len(parts) == 2 && r.Method == "GET":
		f.list(w, r)
	case parts[0] == "d" && len(parts) == 3 && r.Method == "GET":
		o := f.records[parts[2]]
		if o == nil {
			refuse(w, 404, "no_such_object", "no such object")
			return
		}
		reply(w, 200, o)
	case parts[0] == "d" && len(parts) == 3 && r.Method == "PUT":
		v, _ := strconv.Atoi(r.URL.Query().Get("v"))
		var val Value
		json.NewDecoder(r.Body).Decode(&val)
		cur := 0
		if o := f.records[parts[2]]; o != nil {
			cur = o.Version
		}
		if v != cur {
			refuse(w, 409, "version_conflict", "version moved", "version", cur)
			return
		}
		f.records[parts[2]] = &Object{Key: parts[2], Value: val, Version: cur + 1}
		reply(w, 200, map[string]any{"ok": true, "key": parts[2], "version": cur + 1})
	case parts[0] == "f" && len(parts) == 2 && r.Method == "POST":
		f.upload(w, r)
	case parts[0] == "f" && len(parts) == 3 && r.Method == "GET":
		b, ok := f.files[parts[2]]
		if !ok {
			refuse(w, 404, "no_such_file", "no such file")
			return
		}
		http.ServeContent(w, r, "", time.Time{}, bytes.NewReader(b))
	case parts[0] == "f" && len(parts) == 3 && r.Method == "DELETE":
		f.deletes = append(f.deletes, parts[2])
		if f.records["f."+parts[2]] == nil {
			refuse(w, 404, "no_such_file", "no such file")
			return
		}
		delete(f.records, "f."+parts[2])
		delete(f.parts, parts[2])
		delete(f.files, parts[2])
		reply(w, 200, map[string]any{"ok": true})
	case parts[0] == "f" && len(parts) == 5 && parts[3] == "parts" && r.Method == "PUT":
		f.part(w, r, parts[2], parts[4])
	case parts[0] == "f" && len(parts) == 4 && parts[3] == "complete" && r.Method == "POST":
		f.complete(w, parts[2])
	default:
		refuse(w, 404, "no_route", r.Method+" "+r.URL.Path)
	}
}

func (f *fake) list(w http.ResponseWriter, r *http.Request) {
	prefix, after := r.URL.Query().Get("prefix"), r.URL.Query().Get("after")
	var keys []string
	for k := range f.records {
		if strings.HasPrefix(k, prefix) && k > after {
			keys = append(keys, k)
		}
	}
	slicesSort(keys)
	out := []*Object{}
	var cursor *string
	for i, k := range keys {
		if i == 2 { // two per page, so the client must follow the cursor
			c := keys[i-1]
			cursor = &c
			break
		}
		out = append(out, f.records[k])
	}
	reply(w, 200, map[string]any{"ok": true, "keys": out, "cursor": cursor})
}

func slicesSort(s []string) {
	for i := range s {
		for j := i + 1; j < len(s); j++ {
			if s[j] < s[i] {
				s[i], s[j] = s[j], s[i]
			}
		}
	}
}

func (f *fake) newID() string {
	f.nextID++
	return fmt.Sprintf("id%d", f.nextID)
}

func (f *fake) setFile(id string, data map[string]any) {
	d, _ := json.Marshal(data)
	v := 1
	if o := f.records["f."+id]; o != nil {
		v = o.Version + 1
	}
	f.records["f."+id] = &Object{Key: "f." + id, Value: Value{Schema: "file@1", Data: d}, Version: v}
}

func (f *fake) upload(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	if q.Get("multipart") == "1" {
		size, _ := strconv.ParseInt(q.Get("bytes"), 10, 64)
		pb, _ := strconv.ParseInt(q.Get("part_bytes"), 10, 64)
		id := f.newID()
		data := map[string]any{"id": id, "size": size, "type": r.Header.Get("content-type"), "sha256_declared": q.Get("sha256"),
			"state": "uploading", "upload_id": "u-" + id, "part_bytes": pb, "parts": []any{}}
		f.setFile(id, data)
		f.parts[id] = map[int][]byte{}
		reply(w, 201, map[string]any{"slug": slug, "id": id, "key": "f." + id, "file": data})
		return
	}
	if r.ContentLength > f.cap {
		refuse(w, 413, "too_large", "file over the cap")
		return
	}
	b, _ := io.ReadAll(r.Body)
	sum := sha256.Sum256(b)
	id := f.newID()
	data := map[string]any{"id": id, "size": len(b), "type": r.Header.Get("content-type"), "sha256": hex.EncodeToString(sum[:]), "name": q.Get("name")}
	if m := q.Get("meta"); m != "" {
		var meta any
		json.Unmarshal([]byte(m), &meta)
		data["meta"] = meta
	}
	f.setFile(id, data)
	f.files[id] = b
	reply(w, 201, map[string]any{"slug": slug, "id": id, "key": "f." + id, "file": data})
}

func (f *fake) fileData(id string) map[string]any {
	var d map[string]any
	json.Unmarshal(f.records["f."+id].Value.Data, &d)
	return d
}

func (f *fake) part(w http.ResponseWriter, r *http.Request, id, ns string) {
	n, _ := strconv.Atoi(ns)
	if f.records["f."+id] == nil {
		refuse(w, 404, "no_such_file", "no such file")
		return
	}
	if f.gone[id] {
		refuse(w, 410, "upload_gone", "R2 no longer holds this upload")
		return
	}
	b, _ := io.ReadAll(r.Body)
	f.putsN = append(f.putsN, n)
	if n == f.failAt {
		f.failAt = 0
		refuse(w, 500, "part_failed", "send it again")
		return
	}
	d := f.fileData(id)
	pb, size := int64(d["part_bytes"].(float64)), int64(d["size"].(float64))
	count := int((size + pb - 1) / pb)
	want := pb
	if n == count {
		want = size - int64(count-1)*pb
	}
	if int64(len(b)) != want {
		refuse(w, 400, "bad_part_size", fmt.Sprintf("part %d is %d bytes, not %d", n, want, len(b)))
		return
	}
	f.parts[id][n] = b
	ps := d["parts"].([]any)
	d["parts"] = append(ps, map[string]any{"n": n, "etag": "e" + ns})
	f.setFile(id, d)
	reply(w, 200, map[string]any{"id": id, "n": n, "parts_landed": len(f.parts[id]), "part_count": count})
}

func (f *fake) complete(w http.ResponseWriter, id string) {
	d := f.fileData(id)
	pb, size := int64(d["part_bytes"].(float64)), int64(d["size"].(float64))
	count := int((size + pb - 1) / pb)
	var all []byte
	var missing []int
	for n := 1; n <= count; n++ {
		p, ok := f.parts[id][n]
		if !ok {
			missing = append(missing, n)
		}
		all = append(all, p...)
	}
	if len(missing) > 0 {
		refuse(w, 409, "parts_missing", "parts have not landed", "missing", missing)
		return
	}
	f.files[id] = all
	delete(d, "upload_id")
	delete(d, "parts")
	delete(d, "part_bytes")
	d["state"] = "ready"
	f.setFile(id, d)
	reply(w, 200, map[string]any{"id": id, "file": d})
}

func grantClient(t *testing.T, s *httptest.Server) *Client {
	dir := t.TempDir()
	return &Client{Base: s.URL, Slug: slug, Secret: secret, Jar: filepath.Join(dir, "jar"), Ledger: filepath.Join(dir, "uploads"), PartBytes: 5}
}

func jarValue(t *testing.T, path string) string {
	t.Helper()
	st, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if st.Mode().Perm() != 0o600 {
		t.Fatalf("jar mode %v", st.Mode().Perm())
	}
	var m map[string]string
	b, _ := os.ReadFile(path)
	json.Unmarshal(b, &m)
	return m[cookie]
}

// Unlock on first use, keep a renewal from an ordinary response, and after
// the grant is gone unlock again and retry the same write.
func TestGrantLifecycle(t *testing.T) {
	f, s := newFake(t)
	c := grantClient(t, s)
	ctx := context.Background()
	v, err := c.Create(ctx, "rec.a", "meeting@1", map[string]any{"state": "uploading"})
	if err != nil || v != 1 || f.unlocks != 1 {
		t.Fatalf("create: v%d %v, %d unlocks", v, err, f.unlocks)
	}
	if got := jarValue(t, c.Jar); got != f.grant {
		t.Fatalf("jar %q, grant %q", got, f.grant)
	}

	f.renewTo = "jwt-renewed~" + secret
	if _, err := c.Record(ctx, "rec.a"); err != nil {
		t.Fatal(err)
	}
	if got := jarValue(t, c.Jar); got != "jwt-renewed~"+secret {
		t.Fatalf("renewal not kept: jar %q", got)
	}
	// a fresh client on the same jar needs no unlock
	c2 := &Client{Base: s.URL, Slug: slug, Secret: secret, Jar: c.Jar}
	if _, err := c2.Record(ctx, "rec.a"); err != nil || f.unlocks != 1 {
		t.Fatalf("jar reuse: %v, %d unlocks", err, f.unlocks)
	}

	f.grant = "" // the grant lapsed: every page route answers 401
	if v, err = c.CAS(ctx, "rec.a", "meeting@1", map[string]any{"state": "uploaded"}, 1); err != nil || v != 2 || f.unlocks != 2 {
		t.Fatalf("401 → unlock → retry: v%d %v, %d unlocks", v, err, f.unlocks)
	}
}

func TestRefusals(t *testing.T) {
	f, s := newFake(t)
	ctx := context.Background()
	c := grantClient(t, s)
	if _, err := c.Create(ctx, "rec.a", "meeting@1", 1); err != nil {
		t.Fatal(err)
	}
	_, err := c.Create(ctx, "rec.a", "meeting@1", 2)
	var e *Error
	if Code(err) != "version_conflict" || !asError(err, &e) || e.Status != 409 || e.Version != 1 {
		t.Fatalf("create over an existing record: %v", err)
	}

	f.cap = 3
	path := filepath.Join(t.TempDir(), "a.flac")
	os.WriteFile(path, []byte("four"), 0o644)
	if _, _, err := c.Upload(ctx, path, nil); Code(err) != "too_large" {
		t.Fatalf("upload over the cap: %v", err)
	}

	// a write without x-ccc-auth is what the service refuses; the client
	// never sends one, so the refusal is replayed on a bare request
	req, _ := http.NewRequest("PUT", s.URL+"/d/"+slug+"/rec.b?v=0", strings.NewReader(`{}`))
	req.AddCookie(&http.Cookie{Name: cookie, Value: f.grant})
	resp, _ := http.DefaultClient.Do(req)
	if err := refusal(resp); Code(err) != "csrf_header_required" {
		t.Fatalf("bare cookie write: %v", err)
	}

	// the write window: a burst waits it out, a page that never lets up fails
	oldWait := rateWait
	rateWait = time.Millisecond
	t.Cleanup(func() { rateWait = oldWait })
	f.limited = 3
	if v, err := c.CAS(ctx, "rec.a", "meeting@1", 3, 1); err != nil || v != 2 || f.limited != 0 {
		t.Fatalf("429 × 3 then the write: v%d %v", v, err)
	}
	f.limited = rateTries + 1
	if _, err := c.CAS(ctx, "rec.a", "meeting@1", 4, 2); Code(err) != "too_many_writes" {
		t.Fatalf("429 past the retries: %v", err)
	}
	f.limited = 0

	f.revoked, f.grant = true, ""
	if _, err := c.Record(ctx, "rec.a"); Code(err) != "token_revoked" {
		t.Fatalf("revoked token: %v", err)
	}
	if f.unlocks != 2 {
		t.Fatalf("%d unlocks; a revoked token is asked once", f.unlocks)
	}
}

func asError(err error, e **Error) bool {
	x, ok := err.(*Error)
	*e = x
	return ok
}

func TestRecordsAndFiles(t *testing.T) {
	f, s := newFake(t)
	ctx := context.Background()
	c := grantClient(t, s)
	for _, k := range []string{"rec.1", "rec.2", "rec.3", "q.1"} {
		if _, err := c.Create(ctx, k, "meeting@1", map[string]string{"id": k}); err != nil {
			t.Fatal(err)
		}
	}
	got, err := c.List(ctx, "rec.")
	if err != nil || len(got) != 3 || got[2].Key != "rec.3" || !strings.Contains(string(got[0].Value.Data), "rec.1") {
		t.Fatalf("list across pages: %+v %v", got, err)
	}

	path := filepath.Join(t.TempDir(), "mic.flac")
	body := []byte("0123456789")
	os.WriteFile(path, body, 0o644)
	id, sum, err := c.Upload(ctx, path, map[string]any{"rec": "rec.1", "role": "mic"})
	want := sha256.Sum256(body)
	if err != nil || sum != hex.EncodeToString(want[:]) {
		t.Fatalf("upload: %s %s %v", id, sum, err)
	}
	d := f.fileData(id)
	if d["type"] != "audio/flac" || d["name"] != "mic.flac" || d["meta"].(map[string]any)["role"] != "mic" {
		t.Fatalf("record %+v", d)
	}
	rc, err := c.Get(ctx, id, 3, 4)
	if err != nil {
		t.Fatal(err)
	}
	b, _ := io.ReadAll(rc)
	rc.Close()
	if string(b) != "3456" {
		t.Fatalf("range read %q", b)
	}
}

func bigFile(t *testing.T, n int) (string, []byte, string) {
	b := make([]byte, n)
	rand.Read(b)
	path := filepath.Join(t.TempDir(), "remote.flac")
	os.WriteFile(path, b, 0o644)
	sum := sha256.Sum256(b)
	return path, b, hex.EncodeToString(sum[:])
}

func withSingleMax(t *testing.T, n int64) {
	old := singleMax
	singleMax = n
	t.Cleanup(func() { singleMax = old })
}

// A part that fails stops the upload; the rerun reads the record and sends
// only the parts it lacks.
func TestMultipartResume(t *testing.T) {
	withSingleMax(t, 8)
	f, s := newFake(t)
	ctx := context.Background()
	c := grantClient(t, s)
	path, body, sum := bigFile(t, 23) // parts of 5: 5 5 5 5 3

	f.failAt = 3
	if _, _, err := c.Upload(ctx, path, nil); Code(err) != "part_failed" {
		t.Fatalf("first run: %v", err)
	}
	if fmt.Sprint(f.putsN) != "[1 2 3]" {
		t.Fatalf("first run sent %v", f.putsN)
	}
	f.putsN = nil
	id, got, err := c.Upload(ctx, path, nil)
	if err != nil || got != sum {
		t.Fatalf("resume: %s %v", got, err)
	}
	if fmt.Sprint(f.putsN) != "[3 4 5]" {
		t.Fatalf("resume sent %v, want only the missing parts", f.putsN)
	}
	if !bytes.Equal(f.files[id], body) || f.fileData(id)["state"] != "ready" || f.fileData(id)["sha256_declared"] != sum {
		t.Fatalf("assembled file differs, record %+v", f.fileData(id))
	}
	if entries, _ := os.ReadDir(c.Ledger); len(entries) != 0 {
		t.Fatalf("ledger kept %d entries after completion", len(entries))
	}
	// a rerun after completion uploads nothing new
	f.putsN = nil
	if _, _, err := c.Upload(ctx, path, nil); err != nil || f.nextID != 2 || len(f.putsN) != 5 {
		// the ledger is gone, so a rerun is a fresh upload: one create, five parts
		t.Fatalf("rerun: %v, ids %d, parts %v", err, f.nextID, f.putsN)
	}
}

// An upload older than R2 keeps it is deleted and started again; one R2
// has dropped (upload_gone) likewise.
func TestMultipartRestart(t *testing.T) {
	withSingleMax(t, 8)
	f, s := newFake(t)
	ctx := context.Background()
	c := grantClient(t, s)
	path, body, sum := bigFile(t, 12)

	f.failAt = 2
	c.Upload(ctx, path, nil)
	b, _ := json.Marshal(pending{ID: "id1", Started: time.Now().Add(-8 * 24 * time.Hour)})
	os.WriteFile(c.ledgerPath(sum), b, 0o600)
	id, _, err := c.Upload(ctx, path, nil)
	if err != nil || id == "id1" || fmt.Sprint(f.deletes) != "[id1]" || !bytes.Equal(f.files[id], body) {
		t.Fatalf("stale upload: id %s, deletes %v, %v", id, f.deletes, err)
	}

	f.failAt = 2
	path2, body2, _ := bigFile(t, 13)
	c.Upload(ctx, path2, nil)
	stale := fmt.Sprintf("id%d", f.nextID)
	f.gone[stale] = true
	id, _, err = c.Upload(ctx, path2, nil)
	if err != nil || id == stale || !bytes.Equal(f.files[id], body2) || f.deletes[len(f.deletes)-1] != stale {
		t.Fatalf("upload_gone: id %s, deletes %v, %v", id, f.deletes, err)
	}
}

func TestOwnerBearer(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("UCC_HOME", dir)
	t.Setenv("UCC_USERNAME", "")
	t.Setenv("UCC_AUTH_TOKEN", "")
	os.WriteFile(filepath.Join(dir, "user-env.sh"), []byte("# env\nexport UCC_AUTH_TOKEN=\"tok\"\nexport UCC_USERNAME='alice'\nOTHER=1\n"), 0o600)
	b, err := OwnerBearer()
	if err != nil || b != "alice:tok" {
		t.Fatalf("%q %v", b, err)
	}
	// The environment wins when it names both: a page hook runs with them
	// set, on a host whose user-env.sh computes the token.
	t.Setenv("UCC_USERNAME", "alice")
	t.Setenv("UCC_AUTH_TOKEN", "env-tok")
	if b, err := OwnerBearer(); err != nil || b != "alice:env-tok" {
		t.Fatalf("env: %q %v", b, err)
	}
	t.Setenv("UCC_USERNAME", "")
	t.Setenv("UCC_AUTH_TOKEN", "")
	var seen string
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen = r.Header.Get("Authorization") + "|" + r.Header.Get("x-ccc-auth")
		reply(w, 200, map[string]any{"version": 1})
	}))
	defer s.Close()
	c := &Client{Base: s.URL, Slug: slug, Bearer: b}
	if _, err := c.Create(context.Background(), "rec.a", "meeting@1", 1); err != nil || seen != "Bearer alice:tok|" {
		t.Fatalf("owner write sent %q, %v", seen, err)
	}
}
