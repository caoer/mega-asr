package takes

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/caoer/mega-asr/internal/audio"
	"github.com/caoer/mega-asr/internal/compare"
	"github.com/caoer/mega-asr/internal/store"
)

// key is the listener key the tests open the page with: 32 hex digits.
var key = strings.Repeat("ab", 16)

// take writes a delivered take with a record: WAV, texts, and start, stop,
// text and deliver lines.
func take(t *testing.T, dir, id, raw, text string) string {
	t.Helper()
	base := filepath.Join(dir, id)
	if err := audio.SaveWAV(base+".wav", make([]int16, audio.Rate)); err != nil {
		t.Fatal(err)
	}
	if err := (store.Store{Dir: dir}).SaveText(base, raw, text); err != nil {
		t.Fatal(err)
	}
	at := time.Date(2020, 3, 14, 9, 26, 53, 0, time.Local)
	for _, e := range []store.Event{
		{At: at, Ev: "start", Trigger: "tap", Target: &store.Target{Kind: "herdr", Pane: "w1:p2", Workspace: "basalt"}},
		{At: at.Add(time.Second), Ev: "stop", Kind: "tap", DurS: 1},
		{At: at.Add(2 * time.Second), Ev: "text", Engine: "funasr", Chars: len([]rune(text))},
		{At: at.Add(3 * time.Second), Ev: "deliver", N: 1, Via: "auto", OK: true, Submit: "ok"},
	} {
		if err := store.Append(base, e); err != nil {
			t.Fatal(err)
		}
	}
	return base
}

// listener is what serve mounts on compare.addr: the Takes page behind the
// guard.
func listener(t *testing.T, dir string) http.Handler {
	h := New(dir, &compare.Labels{Path: filepath.Join(t.TempDir(), "labels.jsonl")}, func() []Engine { return nil })
	return &Guard{Next: h, Key: func() (string, error) { return key, nil }, Port: "7865", Policy: Policy()}
}

// request is a request the page itself makes, changed by edit.
func request(h http.Handler, method, target string, edit func(*http.Request)) *httptest.ResponseRecorder {
	var r *http.Request
	if method == http.MethodPost {
		r = httptest.NewRequest(method, target, strings.NewReader(`{"id":"20200314-092653"}`))
		r.Header.Set("Content-Type", "application/json")
	} else {
		r = httptest.NewRequest(method, target, nil)
	}
	r.Host = "127.0.0.1:7865"
	r.Header.Set("Sec-Fetch-Site", "same-origin")
	r.Header.Set(Header, key)
	if edit != nil {
		edit(r)
	}
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	return w
}

func TestGuard(t *testing.T) {
	dir := t.TempDir()
	base := take(t, dir, "20200314-092653", "今天下午整理花园里的番茄架子", "今天下午整理花园里的番茄架子。")
	h := listener(t, dir)
	paths := []struct{ method, path string }{
		{"GET", "/takes/api/take/20200314-092653"},
		{"GET", "/takes/audio/20200314-092653.wav"},
		{"POST", "/takes/api/seen"},
		{"GET", "/takes/api/takes"},
		{"GET", "/takes/api/peaks/20200314-092653"},
	}
	host := func(h string) func(*http.Request) { return func(r *http.Request) { r.Host = h } }
	refusals := map[string]func(*http.Request){
		"foreign host":         host("studio.example:7865"),
		"another port":         host("127.0.0.1:7866"),
		"no port":              host("127.0.0.1"),
		"loopback as a suffix": host("127.0.0.1.studio.example:7865"),
		"empty host":           host(""),
		"localhost with a dot": host("localhost.:7865"),
		"loopback with a dot":  host("127.0.0.1.:7865"),
		"cross-site":           func(r *http.Request) { r.Header.Set("Sec-Fetch-Site", "cross-site") },
		"same-site":            func(r *http.Request) { r.Header.Set("Sec-Fetch-Site", "same-site") },
		"no Sec-Fetch-Site":    func(r *http.Request) { r.Header.Del("Sec-Fetch-Site") },
		"no key":               func(r *http.Request) { r.Header.Del(Header) },
		"wrong key":            func(r *http.Request) { r.Header.Set(Header, strings.Repeat("0", 32)) },
		"the key as a cookie": func(r *http.Request) {
			r.Header.Del(Header)
			r.AddCookie(&http.Cookie{Name: "megavoice-7865", Value: key})
		},
	}
	for _, p := range paths {
		for name, edit := range refusals {
			if w := request(h, p.method, p.path, edit); w.Code != http.StatusForbidden || w.Body.String() != Denied {
				t.Errorf("%s %s, %s: %d %q, want 403", p.method, p.path, name, w.Code, w.Body)
			}
		}
		for name, edit := range map[string]func(*http.Request){
			"from the page":             nil,
			"opened in the address bar": func(r *http.Request) { r.Header.Set("Sec-Fetch-Site", "none") },
			"as localhost":              host("localhost:7865"),
			"as [::1]":                  host("[::1]:7865"),
		} {
			if w := request(h, p.method, p.path, edit); w.Code != http.StatusOK {
				t.Errorf("%s %s %s: %d %s", p.method, p.path, name, w.Code, w.Body)
			}
		}
	}
	evs, _ := store.Read(base)
	if seen := evs[len(evs)-1]; seen.Ev != "seen" {
		t.Errorf("after POST /takes/api/seen the record ends in %q", seen.Ev)
	}
}

// The pages hold no take: a browser opens them without a header, so they
// pass without the key; nothing else does. / is the compare panel's old
// address and sends the browser on to the page.
func TestPagesWithoutKey(t *testing.T) {
	h := listener(t, t.TempDir())
	nokey := func(r *http.Request) { r.Header.Del(Header); r.Header.Set("Sec-Fetch-Site", "none") }
	for p, want := range map[string]int{"/": http.StatusFound, "/takes/": http.StatusOK} {
		if w := request(h, "GET", p, nokey); w.Code != want {
			t.Errorf("GET %s without the key: %d, want %d", p, w.Code, want)
		}
		if w := request(h, "GET", p, func(r *http.Request) { nokey(r); r.Host = "studio.example:7865" }); w.Code != http.StatusForbidden {
			t.Errorf("GET %s from a foreign host: %d, want 403", p, w.Code)
		}
	}
	for _, p := range []string{"/takes/api", "/takes/api/", "/takes/audio/", "/takes/nothing.js", "/takes/../api/takes", "/index.html"} {
		if w := request(h, "GET", p, nokey); w.Code != http.StatusForbidden {
			t.Errorf("GET %s without the key: %d, want 403", p, w.Code)
		}
	}
	if w := request(h, "POST", "/", func(r *http.Request) { r.Header.Del(Header) }); w.Code != http.StatusForbidden {
		t.Errorf("POST / without the key: %d, want 403", w.Code)
	}
	// a page is a regular file of the page named exactly: any other spelling
	// of one wants the key
	for _, p := range []string{"/takes/./app.js", "/takes//app.js", "/takes/fonts/", "/takes/APP.JS", "/takes/%2e%2e/api/takes"} {
		if w := request(h, "GET", p, nokey); w.Code != http.StatusForbidden {
			t.Errorf("GET %s without the key: %d, want 403", p, w.Code)
		}
	}
	if w := request(h, "HEAD", "/takes/app.js", nokey); w.Code != http.StatusOK {
		t.Errorf("HEAD /takes/app.js without the key: %d, want 200", w.Code)
	}
	if w := request(h, "HEAD", "/", nokey); w.Code != http.StatusFound {
		t.Errorf("HEAD / without the key: %d, want the redirect", w.Code)
	}
	// HEAD passes without the key for a page alone
	for _, p := range []string{"/takes/api/takes", "/takes/audio/20200314-092653.wav", "/api/takes"} {
		if w := request(h, "HEAD", p, nokey); w.Code != http.StatusForbidden {
			t.Errorf("HEAD %s without the key: %d, want 403", p, w.Code)
		}
	}
}

// The key passes as exactly one header value equal to the key; a key file
// that does not load lets nothing through.
func TestKeyHeaderValues(t *testing.T) {
	dir := t.TempDir()
	take(t, dir, "20200314-092653", "花园", "花园")
	h := listener(t, dir)
	for name, vs := range map[string][]string{
		"empty":          {""},
		"the key twice":  {key, key},
		"key then wrong": {key, strings.Repeat("0", 32)},
		"wrong then key": {strings.Repeat("0", 32), key},
		"the key padded": {" " + key},
		"a prefix":       {key[:31]},
	} {
		w := request(h, "GET", "/takes/api/takes", func(r *http.Request) { r.Header[Header] = vs })
		if w.Code != http.StatusForbidden {
			t.Errorf("%s %q: %d, want 403", name, vs, w.Code)
		}
	}
	broken := &Guard{Next: h.(*Guard).Next, Key: func() (string, error) { return "", fmt.Errorf("takes.key: not a key") }, Port: "7865"}
	for _, v := range []string{"", key} {
		if w := request(broken, "GET", "/takes/api/takes", func(r *http.Request) { r.Header.Set(Header, v) }); w.Code != http.StatusInternalServerError {
			t.Errorf("a key file that does not load, header %q: %d, want 500", v, w.Code)
		}
	}
}

// The pages send the key under the header the guard reads.
func TestPagesSendTheHeader(t *testing.T) {
	js, err := page.ReadFile("page/app.js")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(js), `"`+Header+`": sessionStorage.getItem("megavoice-key")`) {
		t.Errorf("app.js sends no %q header from sessionStorage", Header)
	}
}

// A server on another port of the machine learns from the browser what the
// browser sends it: cookies of 127.0.0.1, and the address it was sent to.
// The listener sets no cookie, and the key in an address travels only in the
// fragment, so neither carries the key; a request carrying the key in a
// cookie or in the query is refused.
func TestAnotherPortLearnsNothing(t *testing.T) {
	dir := t.TempDir()
	take(t, dir, "20200314-092653", "花园", "花园")
	h := listener(t, dir)
	u := URL("127.0.0.1:7865", "/takes/", key)
	sent, frag, _ := strings.Cut(u, "#")
	if strings.Contains(sent, key) || frag != "k="+key {
		t.Errorf("URL %q: the key must be in the fragment alone", u)
	}
	opened := strings.TrimPrefix(sent, "http://127.0.0.1:7865")
	for name, w := range map[string]*httptest.ResponseRecorder{
		"the opened address":  request(h, "GET", opened, func(r *http.Request) { r.Header.Del(Header); r.Header.Set("Sec-Fetch-Site", "none") }),
		"the redirect":        request(h, "GET", "/", nil),
		"the list":            request(h, "GET", "/takes/api/takes", nil),
		"the audio":           request(h, "GET", "/takes/audio/20200314-092653.wav", nil),
		"seen":                request(h, "POST", "/takes/api/seen", nil),
		"a refusal":           request(h, "GET", "/api/takes", func(r *http.Request) { r.Header.Del(Header) }),
		"an address with ?k=": request(h, "GET", "/takes/?k="+key, func(r *http.Request) { r.Header.Del(Header); r.Header.Set("Sec-Fetch-Site", "none") }),
	} {
		if c := w.Header().Values("Set-Cookie"); len(c) != 0 {
			t.Errorf("%s (%d) sets a cookie: %q", name, w.Code, c)
		}
	}
	leaked := func(r *http.Request) {
		r.Header.Del(Header)
		r.AddCookie(&http.Cookie{Name: "megavoice-7865", Value: key})
		r.AddCookie(&http.Cookie{Name: "k", Value: key})
		r.Header.Set("Referer", u)
	}
	for _, p := range []string{"/takes/api/takes?k=" + key, "/takes/audio/20200314-092653.wav?k=" + key, "/api/takes?k=" + key, "/audio/20200314-092653.wav?k=" + key} {
		if w := request(h, "GET", p, leaked); w.Code != http.StatusForbidden {
			t.Errorf("GET %s with the key in a cookie, the query and the Referer: %d, want 403", p, w.Code)
		}
	}
}

// Paths that reach outside the store, or name a file that is not a take,
// are never served.
func TestPathsOutsideTheStore(t *testing.T) {
	parent := t.TempDir()
	dir := filepath.Join(parent, "data")
	os.Mkdir(dir, 0o700)
	take(t, dir, "20200314-092653", "花园", "花园")
	if err := os.WriteFile(filepath.Join(parent, "secret.wav"), []byte("RIFF outside the store"), 0o644); err != nil {
		t.Fatal(err)
	}
	h := listener(t, dir)
	for _, p := range []string{
		"/takes/audio/..%2fsecret.wav", "/takes/audio/%2e%2e%2fsecret.wav", "/takes/audio/../../secret.wav",
		"/takes/audio/20200314-092653%00.wav", "/takes/audio/%00.wav",
		"/takes/api/take/..%2fsecret", "/takes/api/take/..", "/takes/api/take/20200314-092653%00", "/takes/api/take/%00",
		"/takes/api/peaks/..%2fsecret", "/takes/api/peaks/20200314-092653%2f..%2f..%2fsecret",
		"/audio/..%2fsecret.wav", "/audio/%2e%2e/secret.wav", "/audio/20200314-092653%00.wav",
	} {
		w := request(h, "GET", p, nil)
		if w.Code == http.StatusOK || strings.Contains(w.Body.String(), "outside the store") {
			t.Errorf("GET %s: %d %q", p, w.Code, w.Body)
		}
	}
}

func TestOutOfRange(t *testing.T) {
	dir := t.TempDir()
	for i := range 210 {
		id := time.Date(2020, 1, 1, 0, 0, 0, 0, time.Local).Add(time.Duration(i) * time.Minute).Format("20060102-150405")
		os.WriteFile(filepath.Join(dir, id+".txt"), []byte("第"+fmt.Sprint(i)+"条\n"), 0o644)
	}
	take(t, dir, "20200314-092653", "花园", "花园") // one second: 16,000 samples
	h := listener(t, dir)
	for _, q := range []string{"n=-1", "n=0", "n=201", "n=100000", "n=abc", "n=1e9", "n=99999999999999999999", "before=99999999-999999", "before=../..", "before=%00", "before=20200314-092653&n=1000"} {
		w := request(h, "GET", "/takes/api/takes?"+q, nil)
		var pg Page
		if err := json.Unmarshal(w.Body.Bytes(), &pg); w.Code != http.StatusOK || err != nil || len(pg.Takes) == 0 || len(pg.Takes) > 200 {
			t.Errorf("list ?%s: %d, %d rows, want 1 to 200", q, w.Code, len(pg.Takes))
		}
	}
	for _, q := range []string{"n=-1", "n=0", "n=4001", "n=100000", "n=abc"} {
		w := request(h, "GET", "/takes/api/peaks/20200314-092653?"+q, nil)
		var out struct{ Peaks []float64 }
		if err := json.Unmarshal(w.Body.Bytes(), &out); w.Code != http.StatusOK || err != nil || len(out.Peaks) == 0 || len(out.Peaks) > 4000 {
			t.Errorf("peaks ?%s: %d, %d buckets, want 1 to 4000", q, w.Code, len(out.Peaks))
		}
	}
}

// A route answers only its own method: a POST to a GET route and a GET to a
// POST route are refused and change nothing.
func TestWrongMethod(t *testing.T) {
	dir := t.TempDir()
	base := take(t, dir, "20200314-092653", "花园", "花园")
	h := listener(t, dir)
	for _, p := range []string{
		"/takes/api/takes", "/takes/api/take/20200314-092653", "/takes/api/peaks/20200314-092653", "/takes/api/engines",
		"/takes/audio/20200314-092653.wav", "/takes/", "/api/takes", "/audio/20200314-092653.wav", "/",
	} {
		if w := request(h, "POST", p, nil); w.Code == http.StatusOK {
			t.Errorf("POST %s: 200", p)
		}
	}
	if w := request(h, "GET", "/takes/api/seen", nil); w.Code != http.StatusMethodNotAllowed {
		t.Errorf("GET /takes/api/seen: %d, want 405: the route never reaches its handler", w.Code)
	}
	if w := request(h, "GET", "/api/label", nil); w.Code != http.StatusNotFound && w.Code != http.StatusMethodNotAllowed {
		t.Errorf("GET /api/label: %d, want 404 or 405: the route never reaches its handler", w.Code)
	}
	if evs, _ := store.Read(base); evs[len(evs)-1].Ev == "seen" {
		t.Error("a request by the wrong method appended to the record")
	}
}

func TestPolicyOnEveryResponse(t *testing.T) {
	dir := t.TempDir()
	take(t, dir, "20200314-092653", "花园", "花园")
	h := listener(t, dir)
	cases := map[string]*httptest.ResponseRecorder{
		"200 page": request(h, "GET", "/takes/", nil),
		"302":      request(h, "GET", "/", nil),
		"200 api":  request(h, "GET", "/takes/api/takes", nil),
		"403":      request(h, "GET", "/api/takes", func(r *http.Request) { r.Header.Del(Header) }),
		"403 host": request(h, "GET", "/takes/api/takes", func(r *http.Request) { r.Host = "studio.example:7865" }),
		"403 site": request(h, "GET", "/takes/api/takes", func(r *http.Request) { r.Header.Set("Sec-Fetch-Site", "cross-site") }),
		"404":      request(h, "GET", "/takes/api/take/nothing", nil),
		"415":      request(h, "POST", "/takes/api/seen", func(r *http.Request) { r.Header.Set("Content-Type", "text/plain") }),
	}
	for name, w := range cases {
		csp := w.Header().Get("Content-Security-Policy")
		if !strings.HasPrefix(csp, "default-src 'self'; script-src 'self'; style-src 'self';") || !strings.Contains(csp, "; media-src 'self' blob:;") || strings.Contains(csp, "http") || strings.Contains(csp, "*") {
			t.Errorf("%s (%d): Content-Security-Policy %q", name, w.Code, csp)
		}
		if rp := w.Header().Get("Referrer-Policy"); rp != "no-referrer" {
			t.Errorf("%s (%d): Referrer-Policy %q", name, w.Code, rp)
		}
		if ns := w.Header().Get("X-Content-Type-Options"); ns != "nosniff" {
			t.Errorf("%s (%d): X-Content-Type-Options %q", name, w.Code, ns)
		}
	}
}

func TestFormPostIs415(t *testing.T) {
	dir := t.TempDir()
	base := take(t, dir, "20200314-092653", "花园", "花园")
	h := listener(t, dir)
	for _, ct := range []string{"application/x-www-form-urlencoded", "multipart/form-data; boundary=x", "text/plain", ""} {
		if w := request(h, "POST", "/takes/api/seen", func(r *http.Request) { r.Header.Set("Content-Type", ct) }); w.Code != http.StatusUnsupportedMediaType {
			t.Errorf("POST as %q: %d, want 415", ct, w.Code)
		}
	}
	if evs, _ := store.Read(base); evs[len(evs)-1].Ev == "seen" {
		t.Error("a refused POST appended to the record")
	}
}

func TestSearchChinese(t *testing.T) {
	dir := t.TempDir()
	take(t, dir, "20200314-092653", "今天下午整理花园里的番茄架子", "今天下午整理花园里的番茄支架。")
	base := take(t, dir, "20200314-093010", "明天早上去码头看渔船", "明天早上去码头看渔船。")
	store.Append(base, store.Event{Ev: "retranscribe", N: 1, Engine: "doubao", Text: "明天早上去码头看渔船回港", Raw: "明天早上去码头看渔船回港"})
	take(t, dir, "20200314-094500", "Alice Example 和 Bob Example 的书单", "Alice Example 和 Bob Example 的书单")
	x := &Index{Dir: dir}
	for q, want := range map[string]struct{ id, source, before, hit string }{
		"番茄架子":     {"20200314-092653", "raw", "…午整理花园里的", "番茄架子"},
		"渔船回港":     {"20200314-093010", "retranscription 1", "…天早上去码头看", "渔船回港"},
		"alice ex": {"20200314-094500", "delivered", "", "Alice Ex"},
		"BOB exa":  {"20200314-094500", "delivered", "…mple 和 ", "Bob Exa"},
	} {
		pg, err := x.List(Query{Q: q, N: 50})
		if err != nil {
			t.Fatal(err)
		}
		if len(pg.Takes) != 1 || pg.Takes[0].ID != want.id {
			t.Errorf("q=%q: %+v", q, pg.Takes)
			continue
		}
		if m := pg.Takes[0].Match; m == nil || m.Source != want.source || m.Before != want.before || m.Hit != want.hit {
			t.Errorf("q=%q: match %+v", q, m)
		}
	}
	if pg, _ := x.List(Query{Q: "支架。明", N: 50}); len(pg.Takes) != 0 {
		t.Errorf("a query across two takes found %v", pg.Takes)
	}
}

func TestPagingThousandTakes(t *testing.T) {
	dir := t.TempDir()
	var want []string
	for i := range 1000 {
		id := time.Date(2020, 1, 1, 0, 0, 0, 0, time.Local).Add(time.Duration(i/12) * time.Minute).Format("20060102-150405")
		if n := i % 12; n > 0 {
			id += fmt.Sprintf("-%d", n+1) // up to -12: -10 sorts after -9
		}
		os.WriteFile(filepath.Join(dir, id+".txt"), []byte("第"+fmt.Sprint(i)+"条\n"), 0o644)
		want = append([]string{id}, want...)
	}
	x := &Index{Dir: dir}
	var got []string
	before := ""
	for {
		pg, err := x.List(Query{Before: before, N: 37})
		if err != nil {
			t.Fatal(err)
		}
		for _, r := range pg.Takes {
			got = append(got, r.ID)
		}
		if !pg.More {
			break
		}
		before = pg.Takes[len(pg.Takes)-1].ID
	}
	if strings.Join(got, " ") != strings.Join(want, " ") {
		t.Fatalf("paged %d takes; first difference:\n%s", len(got), firstDiff(got, want))
	}
}

// TestMatchedByDay: the count of a query's takes and its day heads are the
// same on every page, and follow the filter.
func TestMatchedByDay(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "20200313-080000.txt"), []byte("晴。\n"), 0o644) // no record: its day is its name's
	take(t, dir, "20200314-080000", "阴", "阴。")
	base := take(t, dir, "20200314-090000", "雨", "雨。")
	store.Append(base, store.Event{Ev: "hold", Why: "deliver_failed"})
	take(t, dir, "20200314-100000", "雪", "雪。")
	x := &Index{Dir: dir}
	want := []Day{{"2020-03-14", 3, 1}, {"2020-03-13", 1, 0}}
	before := ""
	for page := 0; ; page++ {
		pg, err := x.List(Query{Before: before, N: 1})
		if err != nil {
			t.Fatal(err)
		}
		if pg.Matched != 4 || fmt.Sprint(pg.Days) != fmt.Sprint(want) {
			t.Errorf("page %d: matched %d, days %v; want 4, %v", page, pg.Matched, pg.Days, want)
		}
		if !pg.More {
			break
		}
		before = pg.Takes[len(pg.Takes)-1].ID
	}
	pg, _ := x.List(Query{State: "undelivered", N: 50})
	if pg.Matched != 1 || fmt.Sprint(pg.Days) != fmt.Sprint([]Day{{"2020-03-14", 1, 1}}) {
		t.Errorf("state=undelivered: matched %d, days %v", pg.Matched, pg.Days)
	}
}

func firstDiff(got, want []string) string {
	for i := range min(len(got), len(want)) {
		if got[i] != want[i] {
			return fmt.Sprintf("at %d: %s, want %s", i, got[i], want[i])
		}
	}
	return fmt.Sprintf("%d rows, want %d", len(got), len(want))
}

func TestNotATake(t *testing.T) {
	dir := t.TempDir()
	take(t, dir, "20200314-092653", "花园", "花园")
	// WAVs that are not takes: one in the store, one beside it
	os.WriteFile(filepath.Join(dir, "notatake.wav"), []byte("RIFF"), 0o644)
	os.WriteFile(filepath.Join(filepath.Dir(dir), "beside.wav"), []byte("RIFF"), 0o644)
	h := New(dir, nil, nil)
	for _, p := range []string{
		"/takes/api/take/labels", "/takes/api/take/20200314-09265", "/takes/api/take/20200101-000000",
		"/takes/audio/20200101-000000.wav", "/takes/audio/20200314-092653.txt",
		"/takes/api/peaks/20200101-000000", "/takes/audio/notatake.wav", "/takes/audio/..%2fbeside.wav",
	} {
		w := httptest.NewRecorder()
		h.ServeHTTP(w, httptest.NewRequest("GET", p, nil))
		if w.Code != http.StatusNotFound {
			t.Errorf("GET %s: %d, want 404", p, w.Code)
		}
	}
	w := httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest("POST", "/takes/api/seen", strings.NewReader(`{"id":"20200101-000000"}`)))
	if w.Code != http.StatusNotFound {
		t.Errorf("POST seen of no take: %d, want 404", w.Code)
	}
	w = httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest("GET", "/takes/api/take/20200314-092653", nil))
	var got Take
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil || got.State != "sent" || got.Target.Pane != "w1:p2" || len(got.Events) != 4 {
		t.Errorf("the take: %d %s", w.Code, w.Body)
	}
}

func TestKeyFile(t *testing.T) {
	k := KeyFile(filepath.Join(t.TempDir(), "megavoice", "takes.key"))
	a, err := k.Load()
	if err != nil || len(a) != 32 {
		t.Fatalf("Load: %q %v", a, err)
	}
	if st, _ := os.Stat(string(k)); st.Mode().Perm() != 0o600 {
		t.Errorf("mode %v, want 0600", st.Mode().Perm())
	}
	if b, _ := k.Load(); b != a {
		t.Errorf("a second Load gave another key")
	}
	if c, _ := k.Rotate(); c == a {
		t.Errorf("Rotate kept the key")
	} else if d, _ := k.Load(); d != c {
		t.Errorf("Load after Rotate: %q, want %q", d, c)
	}
	for _, body := range []string{"", "abc\n", key + "0\n"} {
		os.WriteFile(string(k), []byte(body), 0o600)
		if got, err := k.Load(); err == nil {
			t.Errorf("Load of %q: %q, want an error", body, got)
		}
	}
}

// The state directory is 0700 and the key file 0600 whatever they were
// before: a key file others could read is replaced by a new key.
func TestKeyFileModes(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "megavoice")
	os.Mkdir(dir, 0o755)
	os.Chmod(dir, 0o755)
	k := KeyFile(filepath.Join(dir, "takes.key"))
	os.WriteFile(string(k), []byte(key+"\n"), 0o644)
	os.Chmod(string(k), 0o644)
	got, err := k.Load()
	if err != nil || got == key || len(got) != 32 {
		t.Errorf("Load of a 0644 key file: %q %v, want a new key", got, err)
	}
	mode := func(p string) os.FileMode { st, _ := os.Stat(p); return st.Mode().Perm() }
	if m := mode(dir); m != 0o700 {
		t.Errorf("state directory %v after Load, want 0700", m)
	}
	if m := mode(string(k)); m != 0o600 {
		t.Errorf("key file %v after Load, want 0600", m)
	}
	// Rotate writes 0600 whatever the file or a leftover temporary file was
	os.Chmod(string(k), 0o644)
	os.WriteFile(string(k)+".tmp", nil, 0o644)
	os.Chmod(string(k)+".tmp", 0o644)
	os.Chmod(dir, 0o755)
	rotated, err := k.Rotate()
	if err != nil {
		t.Fatal(err)
	}
	if m := mode(string(k)); m != 0o600 {
		t.Errorf("key file %v after Rotate, want 0600", m)
	}
	if m := mode(dir); m != 0o700 {
		t.Errorf("state directory %v after Rotate, want 0700", m)
	}
	// Load keeps a 0600 key and corrects its directory
	os.Chmod(dir, 0o755)
	if got, err := k.Load(); err != nil || got != rotated {
		t.Errorf("Load of a 0600 key in a 0755 directory: %q %v, want the key kept", got, err)
	}
	if m := mode(dir); m != 0o700 {
		t.Errorf("state directory %v after Load of a kept key, want 0700", m)
	}
}

// TestBackupTrack: a take decoded from the backup input (its main input
// delivered nothing) plays and draws the backup's WAV, and says so.
func TestBackupTrack(t *testing.T) {
	dir := t.TempDir()
	base := take(t, dir, "20200314-092653", "a take written for the test", "A take written for the test.")
	speech := make([]int16, audio.Rate/2)
	for i := range speech {
		speech[i] = 16384
	}
	if err := os.MkdirAll(filepath.Join(dir, store.BackupDir), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := audio.SaveWAV(filepath.Join(dir, store.BackupDir, "20200314-092653.wav"), speech); err != nil {
		t.Fatal(err)
	}
	h := listener(t, dir)
	track := func() (string, int, float64) {
		var tk Take
		json.Unmarshal(request(h, http.MethodGet, "/takes/api/take/20200314-092653", nil).Body.Bytes(), &tk)
		var pk struct{ Peaks []float64 }
		json.Unmarshal(request(h, http.MethodGet, "/takes/api/peaks/20200314-092653?n=10", nil).Body.Bytes(), &pk)
		top := 0.0
		for _, p := range pk.Peaks {
			top = max(top, p)
		}
		return tk.Track, request(h, http.MethodGet, "/takes/audio/20200314-092653.wav", nil).Body.Len(), top
	}
	if tr, n, top := track(); tr != "main" || n != 44+2*audio.Rate || top != 0 {
		t.Errorf("text from the main input: track %q, %d bytes, peak %v; want main, the silent main WAV", tr, n, top)
	}
	if err := store.Append(base, store.Event{Ev: "text", Engine: "funasr", Audio: "backup"}); err != nil {
		t.Fatal(err)
	}
	if tr, n, top := track(); tr != "backup" || n != 44+audio.Rate || top != 0.5 {
		t.Errorf("text from the backup input: track %q, %d bytes, peak %v; want backup, the backup WAV", tr, n, top)
	}
}

// Counted, the menu's count, is the store's undelivered takes not seen
// since: never a delivered or cancelled one, and a take that turns
// undelivered later is counted at the first call past the index's
// once-a-second limit, as the menu's 5 s poll makes it.
func TestCounted(t *testing.T) {
	dir := t.TempDir()
	add := func(id string, evs ...store.Event) string {
		base := take(t, dir, id, "花园", "花园")
		for _, e := range evs {
			e.At = time.Date(2020, 3, 14, 9, 30, 0, 0, time.Local)
			if err := store.Append(base, e); err != nil {
				t.Fatal(err)
			}
		}
		return base
	}
	failed := store.Event{Ev: "hold", Why: "deliver_failed"}
	later := add("20200314-092653")
	add("20200314-092753", failed)
	add("20200314-092853", failed, store.Event{Ev: "seen"})
	add("20200314-092953", store.Event{Ev: "hold", Why: "cancelled"})
	h := New(dir, nil, nil)
	if n, err := h.Counted(); n != 1 || err != nil {
		t.Fatalf("counted %d %v, want 1", n, err)
	}
	if err := store.Append(later, failed); err != nil {
		t.Fatal(err)
	}
	h.index.mu.Lock()
	h.index.at = h.index.at.Add(-time.Second) // the limit has passed since the last read began
	h.index.mu.Unlock()
	if n, err := h.Counted(); n != 2 || err != nil {
		t.Fatalf("after a take turned undelivered: counted %d %v, want 2", n, err)
	}
}

// Dismissing clears 未送达 like marking read: every count drops (the menu's,
// the strip's and pill's, a day's), the takes stay with their state and
// text, an undo brings them back, and a take that fails afterwards counts.
func TestDismiss(t *testing.T) {
	dir := t.TempDir()
	failed := store.Event{Ev: "hold", Why: "deliver_failed"}
	add := func(id string, evs ...store.Event) {
		base := take(t, dir, id, "花园", "花园")
		for _, e := range evs {
			if err := store.Append(base, e); err != nil {
				t.Fatal(err)
			}
		}
	}
	add("20200314-092653", failed)
	add("20200314-092753", failed, store.Event{Ev: "seen"})
	add("20200314-092853", store.Event{Ev: "hold", Why: "interrupted"})
	add("20200314-092953")
	h := New(dir, nil, nil)
	idx := h.index
	post := func(body string) []string {
		t.Helper()
		w := request(h, "POST", "/takes/api/dismiss", func(r *http.Request) { r.Body = io.NopCloser(strings.NewReader(body)) })
		var out struct{ IDs []string }
		if w.Code != http.StatusOK || json.Unmarshal(w.Body.Bytes(), &out) != nil {
			t.Fatalf("POST %s: %d %s", body, w.Code, w.Body)
		}
		return out.IDs
	}
	counts := func() (menu, strip, dismissed, filter, day int) {
		t.Helper()
		idx.mu.Lock()
		idx.at = time.Time{}
		idx.mu.Unlock()
		menu, _ = idx.Counted()
		pg, err := idx.List(Query{State: "undelivered", Facets: true, N: 50})
		if err != nil {
			t.Fatal(err)
		}
		all, _ := idx.List(Query{N: 50})
		return menu, pg.Facets.States["undelivered"], pg.Facets.States["dismissed"], len(pg.Takes), all.Days[0].Undelivered
	}
	if m, s, d, f, dy := counts(); m != 2 || s != 3 || d != 0 || f != 3 || dy != 3 {
		t.Fatalf("before: menu %d strip %d dismissed %d filter %d day %d; want 2 3 0 3 3", m, s, d, f, dy)
	}

	ids := post(`{"all":true}`)
	if len(ids) != 3 {
		t.Fatalf("dismiss all: %v, want the 3 undelivered takes", ids)
	}
	if m, s, d, f, dy := counts(); m != 0 || s != 0 || d != 3 || f != 0 || dy != 0 {
		t.Errorf("after dismiss all: menu %d strip %d dismissed %d filter %d day %d; want 0 0 3 0 0", m, s, d, f, dy)
	}
	tk, _ := idx.Take("20200314-092653")
	if tk.State != "undelivered" || tk.Why != "deliver_failed" || !tk.Dismissed || tk.Text == nil || !tk.Audio {
		t.Errorf("a dismissed take: %+v; want undelivered deliver_failed, dismissed, its text and audio kept", tk.Row)
	}
	if again := post(`{"all":true}`); len(again) != 0 {
		t.Errorf("dismiss all again: %v, want none", again)
	}

	add("20200314-093053", failed)
	if m, s, _, _, _ := counts(); m != 1 || s != 1 {
		t.Errorf("a new failed take: menu %d strip %d, want 1 1", m, s)
	}

	if back := post(`{"ids":["20200314-092653","20200314-092953"],"undo":true}`); len(back) != 1 || back[0] != "20200314-092653" {
		t.Errorf("undo: %v, want only the dismissed take back", back)
	}
	if m, s, d, _, _ := counts(); m != 2 || s != 2 || d != 2 {
		t.Errorf("after the undo of one: menu %d strip %d dismissed %d; want 2 2 2", m, s, d)
	}
}
