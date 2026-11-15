package takes

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/caoer/mega-asr/internal/compare"
	"github.com/caoer/mega-asr/internal/session"
	"github.com/caoer/mega-asr/internal/store"
)

type fakeTarget string

func (f fakeTarget) String() string { return string(f) }

// fakeDeliverer records each delivery as "target|text" and each Enter as
// "target|⏎"; with block set, a delivery closes entered and waits for it.
type fakeDeliverer struct {
	mu      sync.Mutex
	got     []string
	entered chan struct{}
	block   chan struct{}
}

func (d *fakeDeliverer) Capture() session.Target { return fakeTarget("front") }
func (d *fakeDeliverer) Deliver(t session.Target, text string) error {
	d.mu.Lock()
	entered, block := d.entered, d.block
	d.entered = nil
	d.mu.Unlock()
	if entered != nil {
		close(entered)
		<-block
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	d.got = append(d.got, t.String()+"|"+text)
	return nil
}
func (d *fakeDeliverer) Submit(t session.Target) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.got = append(d.got, t.String()+"|⏎")
	return nil
}
func (d *fakeDeliverer) String() string {
	d.mu.Lock()
	defer d.mu.Unlock()
	return strings.Join(d.got, ";")
}

type nopUI struct{}

func (nopUI) Show(session.View) {}
func (nopUI) Flash(string)      {}
func (nopUI) Alert(string)      {}

// actions is the listener with the actions wired to a controller over dir
// that delivers to del; herdr lists panes, or does not answer when panes
// is nil. Its one engine is cloudasr, a cloud engine the config does not
// name, whose decode fails the test.
func actions(t *testing.T, dir string, del *fakeDeliverer, panes []Pane, apps []App) (http.Handler, *session.Controller) {
	g, c, _ := actionsOf(t, dir, del, panes, apps)
	return g, c
}

// actionsOf is actions with the Handler, for a test to change its engines.
func actionsOf(t *testing.T, dir string, del *fakeDeliverer, panes []Pane, apps []App) (http.Handler, *session.Controller, *Handler) {
	c := &session.Controller{Store: store.Store{Dir: dir}, Deliver: del, UI: nopUI{}}
	h := New(dir, &compare.Labels{Path: filepath.Join(t.TempDir(), "labels.jsonl")}, func() []Engine { return nil })
	h.Ctrl = c
	h.Resolve = func(to string) (session.Target, error) { return fakeTarget(to), nil }
	h.Engine = func(name string) (session.Engine, error) {
		return session.Engine{Name: name, Cloud: true, File: func(context.Context, string) (string, error) {
			t.Error("the cloud engine was called")
			return "", nil
		}}, nil
	}
	h.Panes = func() ([]Pane, error) {
		if panes == nil {
			return nil, errors.New("herdr: not running")
		}
		return panes, nil
	}
	h.Apps = func() []App { return apps }
	return &Guard{Next: h, Key: func() (string, error) { return key, nil }, Port: "7865", Policy: Policy()}, c, h
}

// refusal is the code and status of an answer the API refused.
func refusal(t *testing.T, w *httptest.ResponseRecorder) (int, string) {
	t.Helper()
	var r Refusal
	if err := json.Unmarshal(w.Body.Bytes(), &r); err != nil || r.Error == "" || w.Header().Get("Content-Type") != "application/json" {
		t.Fatalf("%d %q: not a refusal (%v)", w.Code, w.Body, err)
	}
	return w.Code, r.Code
}

// post is a POST of body as the page makes it, changed by edit.
func post(h http.Handler, path, body string, edit func(*http.Request)) *httptest.ResponseRecorder {
	r := httptest.NewRequest(http.MethodPost, path, strings.NewReader(body))
	r.Host = "127.0.0.1:7865"
	r.Header.Set("Sec-Fetch-Site", "same-origin")
	r.Header.Set(Header, key)
	r.Header.Set("Content-Type", "application/json")
	if edit != nil {
		edit(r)
	}
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	return w
}

func record(t *testing.T, base string) string {
	t.Helper()
	b, err := os.ReadFile(base + ".events.jsonl")
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

const takeBody = `{"id":"20200314-092653","to":"pane:w1:p9"}`

// Each action answers only to the page: without the key, with a wrong one,
// from a foreign Host or cross-site it is 403, and a form POST is 415;
// nothing is delivered and nothing appended.
func TestActionsGuarded(t *testing.T) {
	dir := t.TempDir()
	base := take(t, dir, "20200314-092653", "a take written for the test", "A take written for the test.")
	del := &fakeDeliverer{}
	h, _ := actions(t, dir, del, []Pane{{ID: "w1:p9"}}, nil)
	before := record(t, base)
	refusals := map[string]func(*http.Request){
		"no key":     func(r *http.Request) { r.Header.Del(Header) },
		"wrong key":  func(r *http.Request) { r.Header.Set(Header, strings.Repeat("cd", 16)) },
		"Host":       func(r *http.Request) { r.Host = "studio.example:7865" },
		"cross-site": func(r *http.Request) { r.Header.Set("Sec-Fetch-Site", "cross-site") },
	}
	for name, edit := range refusals {
		if w := request(h, http.MethodGet, "/takes/api/targets?id=20200314-092653", edit); w.Code != http.StatusForbidden {
			t.Errorf("targets, %s: %d, want 403", name, w.Code)
		}
		for path, body := range map[string]string{"/takes/api/resend": takeBody, "/takes/api/retranscribe": `{"id":"20200314-092653","engine":"cloudasr"}`,
			"/takes/api/send": `{"id":"20200314-092653","text":"recognised"}`, "/takes/api/rerefine": `{"id":"20200314-092653","mode":"tidy"}`} {
			if w := post(h, path, body, edit); w.Code != http.StatusForbidden {
				t.Errorf("%s, %s: %d, want 403", path, name, w.Code)
			}
			form := func(r *http.Request) { r.Header.Set("Content-Type", "application/x-www-form-urlencoded") }
			if w := post(h, path, "id=20200314-092653&to=pane:w1:p9", form); w.Code != http.StatusUnsupportedMediaType {
				t.Errorf("%s as a form: %d, want 415", path, w.Code)
			}
		}
	}
	if del.String() != "" || record(t, base) != before {
		t.Fatalf("delivered %q; record changed: %v", del.String(), record(t, base) != before)
	}
}

// A resend from the page delivers the take's text to the target picked and
// appends deliver via page, without an Enter when send is absent; an
// unknown take gets no record.
func TestResendFromPage(t *testing.T) {
	dir := t.TempDir()
	base := take(t, dir, "20200314-092653", "a take written for the test", "A take written for the test.")
	del := &fakeDeliverer{}
	h, _ := actions(t, dir, del, []Pane{{ID: "w1:p9"}}, nil)
	w := post(h, "/takes/api/resend", takeBody, nil)
	if w.Code != http.StatusOK {
		t.Fatalf("%d %s", w.Code, w.Body)
	}
	if got := del.String(); got != "pane:w1:p9|A take written for the test." {
		t.Fatalf("delivered %q", got)
	}
	evs, _ := store.Read(base)
	last := evs[len(evs)-1]
	if last.Ev != "deliver" || last.Via != "page" || !last.OK || last.N != 2 || last.Submit != "none" || last.Source != "delivered" || last.Caller != nil {
		t.Fatalf("last line %+v", last)
	}
	var de store.Event
	if err := json.Unmarshal(w.Body.Bytes(), &de); err != nil || de.N != 2 || de.Via != "page" {
		t.Fatalf("answer %s (%v)", w.Body, err)
	}
	for _, body := range []string{`{"id":"20200314-092653"}`, `{"id":"20200314-092653","to":"front"}`, `{"id":"20200314-092653","to":"clipboard","send":true}`} {
		if w := post(h, "/takes/api/resend", body, nil); w.Code != http.StatusBadRequest {
			t.Errorf("%s: %d, want 400", body, w.Code)
		}
	}
	files := listing(t, dir)
	if w := post(h, "/takes/api/resend", `{"id":"20200101-000000","to":"clipboard"}`, nil); w.Code != http.StatusNotFound {
		t.Errorf("an unknown take: %d, want 404", w.Code)
	}
	if got := listing(t, dir); got != files {
		t.Errorf("an unknown take left files: %s, was %s", got, files)
	}
	if got := len(mustEvents(t, base)); got != len(evs) {
		t.Fatalf("a refused resend appended: %d lines, want %d", got, len(evs))
	}
}

// A resend whose text went out but whose line the record could not take is
// answered 200 with the line and not_recorded saying why: the page shows
// the text sent, and a second click does not paste it twice.
func TestResendFromPageNotRecorded(t *testing.T) {
	dir := t.TempDir()
	base := take(t, dir, "20200314-092653", "a take written for the test", "A take written for the test.")
	if err := os.Remove(base + ".events.jsonl"); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(base+".events.jsonl", 0o755); err != nil { // no line can be appended to it
		t.Fatal(err)
	}
	del := &fakeDeliverer{}
	h, _ := actions(t, dir, del, []Pane{{ID: "w1:p9"}}, nil)
	w := post(h, "/takes/api/resend", takeBody, nil)
	var out map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil {
		t.Fatalf("%d %s (%v)", w.Code, w.Body, err)
	}
	why, _ := out["not_recorded"].(string)
	if w.Code != http.StatusOK || out["ev"] != "deliver" || out["via"] != "page" || out["ok"] != true || !strings.Contains(why, session.ErrNotRecorded.Error()) {
		t.Fatalf("%d %s", w.Code, w.Body)
	}
	if got := del.String(); got != "pane:w1:p9|A take written for the test." {
		t.Fatalf("delivered %q", got)
	}
}

// listing is dir's file names.
func listing(t *testing.T, dir string) string {
	t.Helper()
	es, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, e := range es {
		names = append(names, e.Name())
	}
	return strings.Join(names, ",")
}

// send presses Enter after the paste, and text picks the text: both reach
// the controller as the page sent them.
func TestResendFromPageSendAndText(t *testing.T) {
	dir := t.TempDir()
	base := take(t, dir, "20200314-092653", "a take written for the test", "A take written for the test.")
	del := &fakeDeliverer{}
	h, _ := actions(t, dir, del, []Pane{{ID: "w1:p9"}}, nil)
	w := post(h, "/takes/api/resend", `{"id":"20200314-092653","to":"pane:w1:p9","send":true,"text":"raw"}`, nil)
	if w.Code != http.StatusOK {
		t.Fatalf("%d %s", w.Code, w.Body)
	}
	if got := del.String(); got != "pane:w1:p9|a take written for the test;pane:w1:p9|⏎" {
		t.Fatalf("delivered %q", got)
	}
	evs := mustEvents(t, base)
	if last := evs[len(evs)-1]; last.Submit != "ok" || last.Source != "raw" || last.Text != "a take written for the test" {
		t.Fatalf("last line %+v", last)
	}
}

// A target the picker listed that is gone by the click — a pane herdr no
// longer lists, herdr down, an app no longer running, a pid another app
// runs as now — is refused with 409, and so is an app that shows the page:
// the frontmost, the browser in use, and one whose front window is the
// page behind it. Nothing is delivered and the record is byte for byte
// what it was.
func TestResendFromPageRefusesUnreachable(t *testing.T) {
	dir := t.TempDir()
	base := take(t, dir, "20200314-092653", "a take written for the test", "A take written for the test.")
	apps := []App{
		{PID: 501, BundleID: "org.example.notes", Name: "Notes"},
		{PID: 502, BundleID: "org.example.browser", Name: "Browser", Front: true},
		{PID: 503, BundleID: "org.example.viewer", Name: "Viewer", Title: PageTitle + " — Viewer"},
	}
	before := record(t, base)
	for _, c := range []struct {
		name, to string
		panes    []Pane
		status   int
		code     string
	}{
		{"a closed pane", "pane:w1:p8", []Pane{{ID: "w1:p9"}}, http.StatusConflict, "target_gone"},
		{"a pane id shaped as a flag", "pane:--x", []Pane{{ID: "w1:p9"}}, http.StatusConflict, "target_gone"},
		{"herdr down", "pane:w1:p9", nil, http.StatusConflict, "no_herdr"},
		{"an app no longer running", "app:600:org.example.notes", nil, http.StatusConflict, "target_gone"},
		{"a pid another app runs as now", "app:501:org.example.chat", nil, http.StatusConflict, "target_gone"},
		{"the frontmost app", "app:502:org.example.browser", nil, http.StatusConflict, "front_app"},
		{"a window of the page behind the browser in use", "app:503:org.example.viewer", nil, http.StatusConflict, "page_window"},
		{"a pid without its bundle id", "app:501", nil, http.StatusBadRequest, "bad_request"},
		{"no pid", "app:notes", nil, http.StatusBadRequest, "bad_request"},
		{"no such kind", "window:1", nil, http.StatusBadRequest, "bad_request"},
	} {
		del := &fakeDeliverer{}
		h, _ := actions(t, dir, del, c.panes, apps)
		w := post(h, "/takes/api/resend", `{"id":"20200314-092653","to":"`+c.to+`"}`, nil)
		if status, code := refusal(t, w); status != c.status || code != c.code || del.String() != "" {
			t.Errorf("%s: %d %s, delivered %q; want %d %s", c.name, status, code, del.String(), c.status, c.code)
		}
	}
	if record(t, base) != before {
		t.Fatal("a refused resend changed the record")
	}
	h, _ := actions(t, dir, &fakeDeliverer{}, nil, apps)
	for _, to := range []string{"app:501:org.example.notes"} {
		if w := post(h, "/takes/api/resend", `{"id":"20200314-092653","to":"`+to+`"}`, nil); w.Code != http.StatusOK {
			t.Errorf("%s: %d %s", to, w.Code, w.Body)
		}
	}
}

// While a resend delivers, a second resend of the same take is refused with
// 409, and a resend of another take waits for the delivery lock and
// delivers once it is free.
func TestResendFromPageWaits(t *testing.T) {
	dir := t.TempDir()
	base := take(t, dir, "20200314-092653", "a take written for the test", "A take written for the test.")
	other := take(t, dir, "20200315-101500", "another take", "Another take.")
	del := &fakeDeliverer{entered: make(chan struct{}), block: make(chan struct{})}
	entered := del.entered
	h, _ := actions(t, dir, del, []Pane{{ID: "w1:p8"}, {ID: "w1:p9"}}, nil)
	first := make(chan *httptest.ResponseRecorder, 1)
	go func() { first <- post(h, "/takes/api/resend", takeBody, nil) }()
	<-entered // the first resend delivers, the lock held
	w := post(h, "/takes/api/resend", `{"id":"20200314-092653","to":"pane:w1:p8"}`, nil)
	if w.Code != http.StatusConflict || !strings.Contains(w.Body.String(), session.ErrResending.Error()) {
		t.Fatalf("a second resend of the take: %d %s, want 409 %s", w.Code, w.Body, session.ErrResending)
	}
	second := make(chan *httptest.ResponseRecorder, 1)
	go func() { second <- post(h, "/takes/api/resend", `{"id":"20200315-101500","to":"pane:w1:p8"}`, nil) }()
	select {
	case w := <-second:
		t.Fatalf("a resend of another take answered while the lock was held: %d %s", w.Code, w.Body)
	case <-time.After(100 * time.Millisecond):
	}
	close(del.block)
	for _, ch := range []chan *httptest.ResponseRecorder{first, second} {
		select {
		case w := <-ch:
			if w.Code != http.StatusOK {
				t.Fatalf("%d %s", w.Code, w.Body)
			}
		case <-time.After(2 * time.Second):
			t.Fatal("a resend did not deliver once the lock was free")
		}
	}
	if got := del.String(); got != "pane:w1:p9|A take written for the test.;pane:w1:p8|Another take." {
		t.Fatalf("delivered %q", got)
	}
	if len(mustEvents(t, base)) != 5 || len(mustEvents(t, other)) != 5 {
		t.Fatalf("records hold %d and %d lines, want 5 each", len(mustEvents(t, base)), len(mustEvents(t, other)))
	}
}

// A re-transcription of a take already being re-transcribed is refused with
// 409 until the first ends.
func TestRetranscribeFromPageOnce(t *testing.T) {
	dir := t.TempDir()
	base := take(t, dir, "20200314-092653", "a take written for the test", "A take written for the test.")
	h, _, th := actionsOf(t, dir, &fakeDeliverer{}, nil, nil)
	entered, release := make(chan struct{}), make(chan struct{})
	var once sync.Once
	th.Engine = func(name string) (session.Engine, error) {
		return session.Engine{Name: name, Cloud: true, Named: true, File: func(context.Context, string) (string, error) {
			once.Do(func() { close(entered) })
			<-release
			return "the take again", nil
		}}, nil
	}
	body := `{"id":"20200314-092653","engine":"cloudasr"}`
	first := make(chan *httptest.ResponseRecorder, 1)
	go func() { first <- post(h, "/takes/api/retranscribe", body, nil) }()
	<-entered
	second := make(chan *httptest.ResponseRecorder, 1)
	go func() { second <- post(h, "/takes/api/retranscribe", body, nil) }()
	select {
	case w := <-second:
		if w.Code != http.StatusConflict || !strings.Contains(w.Body.String(), session.ErrRetranscribing.Error()) {
			t.Fatalf("a second re-transcription: %d %s, want 409 %s", w.Code, w.Body, session.ErrRetranscribing)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("a second re-transcription is decoding beside the first")
	}
	close(release)
	if w := <-first; w.Code != http.StatusOK {
		t.Fatalf("%d %s", w.Code, w.Body)
	}
	if w := post(h, "/takes/api/retranscribe", body, nil); w.Code != http.StatusOK {
		t.Fatalf("once the first ended: %d %s", w.Code, w.Body)
	}
	if got := len(mustEvents(t, base)); got != 6 {
		t.Fatalf("record holds %d lines, want 6", got)
	}
}

// Each refusal has its status and the code the page words it by: 404 no
// take, 409 on its way or busy, 422 no text or an unnamed cloud engine —
// the two apart by code — 400 a label the file does not take, 500 a
// record the line could not be written to, a full disk or a file megavoice
// may not write.
func TestRefuseCodes(t *testing.T) {
	for _, c := range []struct {
		err    error
		status int
		code   string
	}{
		{session.ErrNoTake, http.StatusNotFound, "no_take"},
		{compare.ErrNoTake, http.StatusNotFound, "no_take"},
		{session.ErrRecording, http.StatusConflict, "recording"},
		{session.ErrTranscribing, http.StatusConflict, "transcribing"},
		{session.ErrResending, http.StatusConflict, "resending"},
		{session.ErrRetranscribing, http.StatusConflict, "retranscribing"},
		{session.ErrDeliveryBusy, http.StatusConflict, "delivery_busy"},
		{fmt.Errorf("%w: no answer of doubao in the compare record", session.ErrNoText), http.StatusUnprocessableEntity, "no_text"},
		{fmt.Errorf("%w: doubao sends the audio off this machine", session.ErrNotNamed), http.StatusUnprocessableEntity, "not_named"},
		{compare.ErrEmpty, http.StatusBadRequest, "label_empty"},
		{compare.ErrEngine, http.StatusBadRequest, "no_answer"},
		{compare.ErrBackup, http.StatusBadRequest, "backup_text"},
		{&fs.PathError{Op: "write", Path: "labels.jsonl", Err: syscall.ENOSPC}, http.StatusInternalServerError, "disk_full"},
		{&fs.PathError{Op: "open", Path: "labels.jsonl", Err: syscall.EACCES}, http.StatusInternalServerError, "no_permission"},
		{session.ErrNotRecorded, http.StatusInternalServerError, "failed"},
	} {
		w := httptest.NewRecorder()
		refuse(w, fmt.Errorf("wrapped: %w", c.err))
		if status, code := refusal(t, w); status != c.status || code != c.code {
			t.Errorf("%v: %d %s, want %d %s", c.err, status, code, c.status, c.code)
		}
	}
}

// A cloud engine the config does not name is refused with 422 before its
// decode runs, and the record is unchanged.
func TestRetranscribeRefusesUnnamedCloud(t *testing.T) {
	dir := t.TempDir()
	base := take(t, dir, "20200314-092653", "a take written for the test", "A take written for the test.")
	h, _ := actions(t, dir, &fakeDeliverer{}, nil, nil)
	before := record(t, base)
	w := post(h, "/takes/api/retranscribe", `{"id":"20200314-092653","engine":"cloudasr"}`, nil)
	if status, code := refusal(t, w); status != http.StatusUnprocessableEntity || code != "not_named" {
		t.Fatalf("%d %s, want 422 not_named", status, code)
	}
	if record(t, base) != before {
		t.Fatal("the record changed")
	}
}

// The picker leads with the take's own target and the targets of recent
// deliveries, then herdr's panes, the apps and the clipboard, each once: the
// own target among the recent ones is not repeated. A recent target that
// cannot be reached now is listed gone, with no "to". An app is named by pid
// and bundle id, and one without a bundle id is not offered; of two instances, a recent delivery's is the one with its
// pid. Without herdr it offers the apps and the clipboard, and the own pane
// and the recent panes are marked gone.
func TestTargets(t *testing.T) {
	dir := t.TempDir()
	take(t, dir, "20200314-092653", "a take written for the test", "A take written for the test.")
	other := take(t, dir, "20200315-101500", "another take", "Another take.")
	at := time.Date(2020, 3, 15, 10, 16, 0, 0, time.Local)
	for _, e := range []store.Event{
		{At: at, Ev: "deliver", N: 2, Via: "cli", OK: true, Target: &store.Target{Kind: "herdr", Pane: "w1:p3", Workspace: "basalt"}},
		{At: at.Add(time.Minute), Ev: "deliver", N: 3, Via: "cli", OK: true, Target: &store.Target{Kind: "app", App: "Notes", BundleID: "org.example.notes", PID: 502}},
		{At: at.Add(2 * time.Minute), Ev: "deliver", N: 4, Via: "cli", OK: false, Target: &store.Target{Kind: "herdr", Pane: "w1:p4"}},
		{At: at.Add(3 * time.Minute), Ev: "deliver", N: 5, Via: "cli", OK: true, Target: &store.Target{Kind: "herdr", Pane: "w1:p5"}},
		{At: at.Add(4 * time.Minute), Ev: "deliver", N: 6, Via: "cli", OK: true, Target: &store.Target{Kind: "herdr", Pane: "w1:p2", Workspace: "basalt"}},
	} {
		if err := store.Append(other, e); err != nil {
			t.Fatal(err)
		}
	}
	panes := []Pane{
		{ID: "w1:p2", Workspace: "basalt", Title: "draft", Cwd: "/work/pangolin"},
		{ID: "w1:p3", Workspace: "basalt", Agent: "cat"},
		{ID: "w1:p4", Workspace: "basalt", Title: "log"},
	}
	apps := []App{
		{PID: 501, BundleID: "org.example.notes", Name: "Notes", Title: "Shopping list"},
		{PID: 502, BundleID: "org.example.notes", Name: "Notes", Title: "Recipes"},
		{PID: 503, Name: "Unbundled Tool"}, // no bundle id: a resend cannot name it
	}
	list := func(panes []Pane) Targets {
		h, _ := actions(t, dir, &fakeDeliverer{}, panes, apps)
		w := request(h, http.MethodGet, "/takes/api/targets?id=20200314-092653", nil)
		var out Targets
		if w.Code != http.StatusOK || json.Unmarshal(w.Body.Bytes(), &out) != nil {
			t.Fatalf("%d %s", w.Code, w.Body)
		}
		return out
	}
	show := func(ts Targets) string {
		var s []string
		for _, t := range ts.Targets {
			s = append(s, t.Group+" "+t.To+" "+t.Label)
		}
		return strings.Join(s, "\n")
	}

	got := list(panes)
	want := strings.Join([]string{
		"own pane:w1:p2 basalt › draft · pangolin · w1:p2",
		"recent  w1:p5",
		"recent app:502:org.example.notes Notes — Recipes",
		"recent pane:w1:p3 basalt › cat · w1:p3",
		"pane pane:w1:p4 basalt › log · w1:p4",
		"app app:501:org.example.notes Notes — Shopping list",
		"clipboard clipboard 剪贴板",
	}, "\n")
	if !got.Herdr || show(got) != want {
		t.Errorf("with herdr (herdr %v):\n%s\nwant\n%s", got.Herdr, show(got), want)
	}

	got = list(nil)
	want = strings.Join([]string{
		"own  basalt › w1:p2",
		"recent  w1:p5",
		"recent app:502:org.example.notes Notes — Recipes",
		"recent  basalt › w1:p3",
		"app app:501:org.example.notes Notes — Shopping list",
		"clipboard clipboard 剪贴板",
	}, "\n")
	if got.Herdr || show(got) != want {
		t.Errorf("without herdr (herdr %v):\n%s\nwant\n%s", got.Herdr, show(got), want)
	}
	for _, i := range []int{0, 1, 3} {
		if x := got.Targets[i]; !x.Gone || !x.Pane {
			t.Errorf("without herdr, %q: gone %v pane %v, want both", x.Label, x.Gone, x.Pane)
		}
	}
}

// A take whose own target is the clipboard lists the clipboard once, as its
// own target.
func TestTargetsOwnClipboard(t *testing.T) {
	dir := t.TempDir()
	base := filepath.Join(dir, "20200316-080000")
	for _, e := range []store.Event{
		{At: time.Date(2020, 3, 16, 8, 0, 0, 0, time.Local), Ev: "start", Trigger: "tap", Target: &store.Target{Kind: "clipboard"}},
		{At: time.Date(2020, 3, 16, 8, 0, 1, 0, time.Local), Ev: "stop", Kind: "tap", DurS: 1},
	} {
		if err := store.Append(base, e); err != nil {
			t.Fatal(err)
		}
	}
	h, _ := actions(t, dir, &fakeDeliverer{}, []Pane{}, nil)
	w := request(h, http.MethodGet, "/takes/api/targets?id=20200316-080000", nil)
	var out Targets
	if w.Code != http.StatusOK || json.Unmarshal(w.Body.Bytes(), &out) != nil {
		t.Fatalf("%d %s", w.Code, w.Body)
	}
	if len(out.Targets) != 1 || out.Targets[0].Group != "own" || out.Targets[0].To != "clipboard" {
		t.Fatalf("targets %+v", out.Targets)
	}
}

func mustEvents(t *testing.T, base string) []store.Event {
	t.Helper()
	evs, err := store.Read(base)
	if err != nil {
		t.Fatal(err)
	}
	return evs
}

// TestTargetsLeaveOutPage: the frontmost app is the browser showing the
// page, and an app whose front window is the page shows it too, behind;
// the picker offers neither — not among the apps, not as a recent target,
// and a take spoken into one shows its own target without a "to".
func TestTargetsLeaveOutPage(t *testing.T) {
	dir := t.TempDir()
	base := take(t, dir, "20200314-092653", "a take written for the test", "A take written for the test.")
	browser := &store.Target{Kind: "app", App: "Browser", BundleID: "org.example.browser", PID: 700}
	at := time.Date(2020, 3, 14, 9, 30, 0, 0, time.Local)
	if err := store.Append(base, store.Event{At: at, Ev: "deliver", N: 2, Via: "cli", OK: true, Target: browser}); err != nil {
		t.Fatal(err)
	}
	own := filepath.Join(dir, "20200314-093500")
	for _, e := range []store.Event{
		{At: at.Add(5 * time.Minute), Ev: "start", Trigger: "tap", Target: browser},
		{At: at.Add(5*time.Minute + time.Second), Ev: "stop", Kind: "tap", DurS: 1},
	} {
		if err := store.Append(own, e); err != nil {
			t.Fatal(err)
		}
	}
	viewer := &store.Target{Kind: "app", App: "Viewer", BundleID: "org.example.viewer", PID: 710}
	if err := store.Append(base, store.Event{At: at.Add(time.Minute), Ev: "deliver", N: 3, Via: "cli", OK: true, Target: viewer}); err != nil {
		t.Fatal(err)
	}
	apps := []App{
		{PID: 700, BundleID: "org.example.browser", Name: "Browser", Title: "Takes", Front: true},
		{PID: 710, BundleID: "org.example.viewer", Name: "Viewer", Title: PageTitle},
		{PID: 501, BundleID: "org.example.notes", Name: "Notes", Title: "Shopping list"},
	}
	h, _ := actions(t, dir, &fakeDeliverer{}, nil, apps)
	w := request(h, http.MethodGet, "/takes/api/targets?id=20200314-093500", nil)
	var out Targets
	if w.Code != http.StatusOK || json.Unmarshal(w.Body.Bytes(), &out) != nil {
		t.Fatalf("%d %s", w.Code, w.Body)
	}
	var got []string
	for _, x := range out.Targets {
		got = append(got, fmt.Sprintf("%s %s %s front=%v", x.Group, x.To, x.Label, x.Front))
	}
	want := []string{
		"own  Browser — Takes front=true",
		"app app:501:org.example.notes Notes — Shopping list front=false",
		"clipboard clipboard 剪贴板 front=false",
	}
	if strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Errorf("targets:\n%s\nwant\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
}

// heldTake is a take held because the model step's call gave no answer to
// use: spoken into pane w1:p2, ended as stop says, its text as recognised
// kept.
func heldTake(t *testing.T, dir, id, stop string) string {
	t.Helper()
	base := filepath.Join(dir, id)
	if err := (store.Store{Dir: dir}).SaveText(base, "用海库跑一下 派森", "用海库跑一下 Python"); err != nil {
		t.Fatal(err)
	}
	at := time.Date(2020, 3, 14, 9, 26, 53, 0, time.Local)
	for _, e := range []store.Event{
		{At: at, Ev: "start", Trigger: "tap", Target: &store.Target{Kind: "herdr", Pane: "w1:p2", Workspace: "basalt"}},
		{At: at.Add(time.Second), Ev: "stop", Kind: stop, DurS: 1},
		{At: at.Add(2 * time.Second), Ev: "text", Engine: "funasr", Chars: 13},
		{At: at.Add(3 * time.Second), Ev: "refine", Mode: "fix-words", Model: "m", Why: "http", Err: "status 529: overloaded"},
		{At: at.Add(3 * time.Second), Ev: "hold", Why: session.HoldRefineFailed},
	} {
		if err := store.Append(base, e); err != nil {
			t.Fatal(err)
		}
	}
	return base
}

// stepIn is a Step that counts its calls and the modes they were made in,
// answering the dictation with "<mode>: <text>".
func stepIn(calls *[]string) func(string) (*session.RefineStep, error) {
	return func(mode string) (*session.RefineStep, error) {
		if mode != "fix-words" && mode != "tidy" {
			return nil, fmt.Errorf("refine.mode %q: want one of fix-words, tidy", mode)
		}
		return &session.RefineStep{Run: func(_ context.Context, in session.RefineInput) (string, *store.Event) {
			*calls = append(*calls, mode)
			return mode + ": " + in.Text, &store.Event{Ev: "refine", Mode: mode, Model: "m", LatencyMS: 200}
		}}, nil
	}
}

// Run again from the page on a held take is one call in the mode picked:
// its line is the answer and joins the record, nothing is delivered and the
// take stays held. Send then delivers the model's text to the take's own
// pane, once: a second send is 409 delivered and delivers nothing.
func TestRunAgainFromPageThenSend(t *testing.T) {
	dir := t.TempDir()
	base := heldTake(t, dir, "20200314-092653", "tap")
	del := &fakeDeliverer{}
	h, _, th := actionsOf(t, dir, del, []Pane{{ID: "w1:p2"}}, nil)
	var calls []string
	th.Step = stepIn(&calls)
	th.Modes = func() Modes {
		return Modes{On: true, Mode: "fix-words", Copy: "both", Modes: []Mode{{"fix-words", "纠正听错的词"}, {"tidy", "整理口语"}}}
	}
	if w := post(h, "/takes/api/rerefine", `{"id":"20200314-092653","mode":"poem"}`, nil); w.Code != http.StatusBadRequest || len(calls) != 0 {
		t.Fatalf("an unknown mode: %d %s, calls %v", w.Code, w.Body, calls)
	}
	w := post(h, "/takes/api/rerefine", `{"id":"20200314-092653","mode":"tidy"}`, nil)
	var line store.Event
	if err := json.Unmarshal(w.Body.Bytes(), &line); err != nil || w.Code != http.StatusOK {
		t.Fatalf("%d %s (%v)", w.Code, w.Body, err)
	}
	if len(calls) != 1 || calls[0] != "tidy" || line.Mode != "tidy" || line.N != 2 || line.Via != "page" || line.Text != "tidy: 用海库跑一下 Python" ||
		del.String() != "" || store.State(mustEvents(t, base)).Why != session.HoldRefineFailed {
		t.Fatalf("calls %v, line %+v, delivered %q, state %+v", calls, line, del.String(), store.State(mustEvents(t, base)))
	}
	w = post(h, "/takes/api/send", `{"id":"20200314-092653","text":"model"}`, nil)
	if w.Code != http.StatusOK || del.String() != "pane:w1:p2|tidy: 用海库跑一下 Python" {
		t.Fatalf("send: %d %s, delivered %q", w.Code, w.Body, del.String())
	}
	evs := mustEvents(t, base)
	if last := evs[len(evs)-1]; last.Ev != "deliver" || !last.OK || last.Via != "page" || last.Source != "model" || last.Submit != "none" ||
		store.State(evs).State != "pasted" {
		t.Fatalf("last line %+v, state %+v", last, store.State(evs))
	}
	before := record(t, base)
	for _, body := range []string{`{"id":"20200314-092653","text":"model"}`, `{"id":"20200314-092653","text":"recognised"}`} {
		if code, why := refusal(t, post(h, "/takes/api/send", body, nil)); code != http.StatusConflict || why != "delivered" {
			t.Errorf("%s again: %d %s", body, code, why)
		}
	}
	if code, why := refusal(t, post(h, "/takes/api/rerefine", `{"id":"20200314-092653","mode":"tidy"}`, nil)); code != http.StatusConflict || why != "delivered" || len(calls) != 1 {
		t.Errorf("run again on the delivered take: %d %s, calls %v", code, why, calls)
	}
	if del.String() != "pane:w1:p2|tidy: 用海库跑一下 Python" || record(t, base) != before {
		t.Fatalf("delivered %q; record changed: %v", del.String(), record(t, base) != before)
	}
}

// A held take sent as recognised delivers its .txt to its own pane, with
// Enter when the take ended with Enter, once. Its own pane gone, or the
// take's record naming none, the send is refused and nothing goes out.
func TestSendFromPageAsRecognised(t *testing.T) {
	dir := t.TempDir()
	base := heldTake(t, dir, "20200314-092653", "enter")
	del := &fakeDeliverer{}
	h, _, th := actionsOf(t, dir, del, []Pane{{ID: "w1:p9"}}, nil)
	th.Step = stepIn(new([]string))
	if code, why := refusal(t, post(h, "/takes/api/send", `{"id":"20200314-092653","text":"recognised"}`, nil)); code != http.StatusConflict || why != "target_gone" {
		t.Fatalf("own pane gone: %d %s", code, why)
	}
	if code, _ := refusal(t, post(h, "/takes/api/send", `{"id":"20200314-092653","text":"raw"}`, nil)); code != http.StatusBadRequest {
		t.Fatalf("text raw: %d", code)
	}
	h, _, _ = actionsOf(t, dir, del, []Pane{{ID: "w1:p2"}}, nil)
	if w := post(h, "/takes/api/send", `{"id":"20200314-092653","text":"recognised"}`, nil); w.Code != http.StatusOK {
		t.Fatalf("%d %s", w.Code, w.Body)
	}
	if got := del.String(); got != "pane:w1:p2|用海库跑一下 Python;pane:w1:p2|⏎" {
		t.Fatalf("delivered %q", got)
	}
	if code, why := refusal(t, post(h, "/takes/api/send", `{"id":"20200314-092653","text":"recognised"}`, nil)); code != http.StatusConflict || why != "delivered" {
		t.Fatalf("again: %d %s", code, why)
	}
	none := filepath.Join(dir, "20200314-100000")
	(store.Store{Dir: dir}).SaveText(none, "a", "A")
	store.Append(none, store.Event{Ev: "hold", Why: "cancelled"})
	if code, why := refusal(t, post(h, "/takes/api/send", `{"id":"20200314-100000","text":"recognised"}`, nil)); code != http.StatusConflict || why != "no_target" {
		t.Fatalf("no target: %d %s", code, why)
	}
	if got := del.String(); got != "pane:w1:p2|用海库跑一下 Python;pane:w1:p2|⏎" {
		t.Fatalf("delivered %q", got)
	}
	_ = base
}

// A take delivered normally is not sent from the page and the step is not
// run on it again: both are 409 delivered, nothing goes out and nothing is
// appended. Its stages read as they were.
func TestDeliveredTakeIsNotSentAgain(t *testing.T) {
	dir := t.TempDir()
	base := take(t, dir, "20200314-092653", "a take written for the test", "A take written for the test.")
	del := &fakeDeliverer{}
	h, _, th := actionsOf(t, dir, del, []Pane{{ID: "w1:p2"}}, nil)
	var calls []string
	th.Step = stepIn(&calls)
	before := record(t, base)
	for path, body := range map[string]string{"/takes/api/send": `{"id":"20200314-092653","text":"recognised"}`, "/takes/api/rerefine": `{"id":"20200314-092653","mode":"tidy"}`} {
		if code, why := refusal(t, post(h, path, body, nil)); code != http.StatusConflict || why != "delivered" {
			t.Errorf("%s: %d %s", path, code, why)
		}
	}
	if del.String() != "" || len(calls) != 0 || record(t, base) != before {
		t.Fatalf("delivered %q, calls %v, record changed: %v", del.String(), calls, record(t, base) != before)
	}
}

// The picture a take's call was sent is among its files and served to the
// page; the modes list is what Modes says, and none where it is not wired.
func TestScreenAndModes(t *testing.T) {
	dir := t.TempDir()
	base := heldTake(t, dir, "20200314-092653", "tap")
	if _, err := store.SaveScreen(base, []byte("\xff\xd8\xff jpeg")); err != nil {
		t.Fatal(err)
	}
	h, _, th := actionsOf(t, dir, &fakeDeliverer{}, nil, nil)
	w := request(h, http.MethodGet, "/takes/api/take/20200314-092653", nil)
	if !strings.Contains(w.Body.String(), "20200314-092653.screen.jpg") {
		t.Fatalf("files: %s", w.Body)
	}
	if w := request(h, http.MethodGet, "/takes/screen/20200314-092653.screen.jpg", nil); w.Code != http.StatusOK || w.Body.String() != "\xff\xd8\xff jpeg" {
		t.Fatalf("screen: %d %q", w.Code, w.Body)
	}
	if w := request(h, http.MethodGet, "/takes/screen/..%2f20200314-092653.screen.jpg", nil); w.Code != http.StatusNotFound {
		t.Fatalf("a path out of the store: %d", w.Code)
	}
	if w := request(h, http.MethodGet, "/takes/api/modes", nil); strings.TrimSpace(w.Body.String()) != `{"on":false,"mode":"","agent_copy":"","modes":[]}` {
		t.Fatalf("modes unwired: %s", w.Body)
	}
	th.Modes = func() Modes {
		return Modes{On: true, Mode: "tidy", Copy: "both", Modes: []Mode{{"tidy", "整理口语"}}}
	}
	if w := request(h, http.MethodGet, "/takes/api/modes", nil); strings.TrimSpace(w.Body.String()) != `{"on":true,"mode":"tidy","agent_copy":"both","modes":[{"name":"tidy","label":"整理口语"}]}` {
		t.Fatalf("modes: %s", w.Body)
	}
}

// A held take whose chunks failed keeps no .txt: run again reads its text
// as recognised off its hold line.
func TestRunAgainReadsTheHoldLinesText(t *testing.T) {
	dir := t.TempDir()
	base := filepath.Join(dir, "20200314-092653")
	at := time.Date(2020, 3, 14, 9, 26, 53, 0, time.Local)
	for _, e := range []store.Event{
		{At: at, Ev: "start", Trigger: "tap", Target: &store.Target{Kind: "herdr", Pane: "w1:p2"}},
		{At: at.Add(time.Second), Ev: "stop", Kind: "tap", DurS: 1},
		{At: at.Add(2 * time.Second), Ev: "text", Engine: "funasr", Chars: 13, FailedChunks: 1},
		{At: at.Add(3 * time.Second), Ev: "refine", Mode: "fix-words", Model: "m", Why: "http", Err: "status 529"},
		{At: at.Add(3 * time.Second), Ev: "hold", Why: session.HoldRefineFailed, Text: "用海库跑一下 Python"},
	} {
		if err := store.Append(base, e); err != nil {
			t.Fatal(err)
		}
	}
	h, _, th := actionsOf(t, dir, &fakeDeliverer{}, []Pane{{ID: "w1:p2"}}, nil)
	var calls []string
	th.Step = stepIn(&calls)
	w := post(h, "/takes/api/rerefine", `{"id":"20200314-092653","mode":"fix-words"}`, nil)
	var line store.Event
	if err := json.Unmarshal(w.Body.Bytes(), &line); err != nil || w.Code != http.StatusOK || line.Text != "fix-words: 用海库跑一下 Python" || len(calls) != 1 {
		t.Fatalf("%d %s, calls %v (%v)", w.Code, w.Body, calls, err)
	}
}
