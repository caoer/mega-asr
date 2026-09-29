package main

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/caoer/mega-asr/internal/pages"
)

func (f *fakeReg) List(_ context.Context, prefix string) ([]pages.Object, error) {
	var out []pages.Object
	for k, o := range f.recs {
		if strings.HasPrefix(k, prefix) {
			out = append(out, o)
		}
	}
	sort.Slice(out, func(i, k int) bool { return out[i].Key < out[k].Key })
	return out, nil
}

func writeFile(t *testing.T, path, body string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

func git(t *testing.T, dir string, args ...string) string {
	t.Helper()
	out, err := exec.Command("git", append([]string{"-C", dir, "-c", "user.name=t", "-c", "user.email=t@t"}, args...)...).CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v %s", args, err, out)
	}
	return strings.TrimSpace(string(out))
}

// feishuWorld is a Feishu archive, a wiki checkout and a page registry, as
// register-feishu sees them on one host.
type feishuWorld struct {
	t                    *testing.T
	archive, wiki, stage string
	reg                  *fakeReg
	u                    *uploader
}

const feishuHost = "lab-host"

func newFeishuWorld(t *testing.T) *feishuWorld {
	dir := t.TempDir()
	w := &feishuWorld{t: t, archive: filepath.Join(dir, "archive"), wiki: filepath.Join(dir, "wiki"), stage: filepath.Join(dir, "stage"), reg: newFakeReg()}
	w.u = &uploader{reg: w.reg, ledger: ledger(filepath.Join(dir, "pending")), now: time.Now}
	os.MkdirAll(filepath.Join(w.wiki, "inbox/filed"), 0o755)
	git(t, w.wiki, "init", "-q")
	return w
}

// minutes writes the archive's state.json; each minute's transcript, when
// given, goes under minutes/<token>/.
func (w *feishuWorld) minutes(ms map[string]map[string]any, transcripts map[string]string) {
	w.t.Helper()
	b, err := json.Marshal(map[string]any{"minutes": ms})
	if err != nil {
		w.t.Fatal(err)
	}
	writeFile(w.t, filepath.Join(w.archive, "state.json"), string(b))
	for tok, body := range transcripts {
		writeFile(w.t, filepath.Join(w.archive, "minutes", tok, "transcript.json"), body)
	}
}

// file commits a bundle for token into the wiki the way filedMinutes reads
// one: inbox/filed/<bundle>/transcript.md naming the token, and its page,
// sources/meetings/<bundle>.md when sourcePage, else the bundle's about.md.
// It returns the page and the commit.
func (w *feishuWorld) file(bundle, token string, sourcePage bool) (string, string) {
	w.t.Helper()
	writeFile(w.t, filepath.Join(w.wiki, "inbox/filed", bundle, "transcript.md"), "---\nsource: 飞书妙记 "+token+"\n---\n")
	page := "inbox/filed/" + bundle + "/about.md"
	if sourcePage {
		page = "sources/meetings/" + bundle + ".md"
	}
	writeFile(w.t, filepath.Join(w.wiki, page), "# "+bundle+"\n")
	git(w.t, w.wiki, "add", "-A")
	git(w.t, w.wiki, "commit", "-qm", "t")
	return page, git(w.t, w.wiki, "rev-parse", "HEAD")
}

func (w *feishuWorld) run() (feishuTally, error) {
	return registerFeishu(context.Background(), w.reg, w.u, w.archive, w.wiki, w.stage, feishuHost)
}

// id is the record id register-feishu gives a minute created at created.
func (w *feishuWorld) id(created string) string {
	w.t.Helper()
	at, err := time.Parse(time.RFC3339, created)
	if err != nil {
		w.t.Fatal(err)
	}
	return at.Local().Format("20060102-150405") + "-feishu-" + feishuHost
}

// Fetched minutes become feishu records; a minute not done or not fetched
// waits. The media rides beside Feishu's transcript unless export was
// denied; speakers come in the order they first speak; a minute the wiki
// holds gets wiki; an upload that fails is resumed on the next run; a rerun
// writes nothing; a minute filed later gets wiki then.
func TestRegisterFeishuMinutes(t *testing.T) {
	w := newFeishuWorld(t)
	const hubAt, pedalAt = "2020-02-12T13:45:52Z", "2020-02-11T08:04:17Z"
	w.minutes(map[string]map[string]any{
		"obcn9s1": {"fetched": "x", "state": "fetching", "title": "Saddle heights", "created": "2020-02-10T17:30:41Z"},
		"obcn7q2": {"fetched": "x", "state": "done", "title": "Rear hub", "media_type": "audio/mp4", "created": hubAt, "media": "clip.bin", "duration_s": 95.5},
		"obcn6p4": {"state": "done", "title": "Chain wear", "created": "2020-02-13T10:02:09Z"},
		"obcn8r5": {"state": "done", "title": "Pedal threads", "media_error": "export_denied", "fetched": "x", "created": pedalAt, "duration_s": 42, "media": "clip.mp4"},
	}, map[string]string{
		"obcn7q2": `{"segments": [
	  {"speaker": "Bob Example", "start_s": 0.4, "text": "The rear hub clicks under load."},
	  {"speaker": "", "start_s": 3.1, "text": "(noise)"},
	  {"speaker": "Alice Example", "start_s": 5.9, "text": "Then the cones are loose."},
	  {"speaker": "Bob Example", "start_s": 9.2, "text": "Or a bearing is pitted."}]}`,
		"obcn8r5": `{"segments": [{"speaker": "Alice Example", "start_s": 2, "text": "Left pedal, reverse thread."}]}`,
	})
	writeFile(t, filepath.Join(w.archive, "minutes/obcn7q2/clip.bin"), "m4a bytes")
	hubPage, hubCommit := w.file("hub-cones", "obcn7q2", false)
	hub, pedal := w.id(hubAt), w.id(pedalAt)

	w.reg.failUpload = 2 // pedal's transcript goes first, then the hub's media fails
	if tl, err := w.run(); err == nil || tl.fetched != 2 || tl.registered != 1 || tl.transcriptOnly != 1 || tl.failed != 1 {
		t.Fatalf("first run: %+v, %v", tl, err)
	}
	if m, _ := w.reg.rec(t, hub); m["state"] != "uploading" {
		t.Fatalf("hub after its upload failed: %v", m["state"])
	}
	if tl, err := w.run(); err != nil || tl.registered != 1 || tl.known != 1 || tl.transcriptOnly != 0 {
		t.Fatalf("second run resumes the hub: %+v, %v", tl, err)
	}

	m, files := w.reg.rec(t, hub)
	if m["state"] != "uploaded" || m["source"] != "feishu" || m["host"] != feishuHost || len(files) != 2 {
		t.Fatalf("hub: %v", m)
	}
	if files[0].Role != "media" || files[0].Codec != "m4a" || files[1].Role != "feishu-transcript" || files[1].Codec != "json" {
		t.Fatalf("hub files: %+v", files)
	}
	var names []string
	for _, s := range m["speakers"].([]any) {
		names = append(names, s.(map[string]any)["name"].(string))
	}
	if strings.Join(names, "|") != "Bob Example|Alice Example" {
		t.Fatalf("hub speakers: %v", names)
	}
	if fe := m["feishu"].(map[string]any); fe["token"] != "obcn7q2" || fe["transcript_file"] != files[1].File {
		t.Fatalf("hub feishu: %v", fe)
	}
	if wk := m["wiki"].(map[string]any); wk["page"] != hubPage || wk["commit"] != hubCommit {
		t.Fatalf("hub wiki: %v", wk)
	}
	if _, ok := w.reg.recs["q."+hub]; !ok {
		t.Fatal("hub is not queued")
	}
	if m, files := w.reg.rec(t, pedal); len(files) != 1 || files[0].Role != "feishu-transcript" || m["wiki"] != nil || m["duration_s"] != 42.0 {
		t.Fatalf("pedal, transcript only: %v %+v", m, files)
	}

	ops := len(w.reg.ops)
	if tl, err := w.run(); err != nil || tl.known != 2 || tl.registered != 0 || len(w.reg.ops) != ops {
		t.Fatalf("rerun: %+v, %v, writes %v", tl, err, w.reg.ops[ops:])
	}

	pedalPage, pedalCommit := w.file("pedal-threads", "obcn8r5", true)
	if tl, err := w.run(); err != nil || tl.wikiSet != 1 || tl.registered != 0 {
		t.Fatalf("pedal filed since: %+v, %v", tl, err)
	}
	if wk := func() map[string]any { m, _ := w.reg.rec(t, pedal); return m["wiki"].(map[string]any) }(); wk["page"] != pedalPage || wk["commit"] != pedalCommit {
		t.Fatalf("pedal wiki: %v", wk)
	}
}

// Feishu's summary and the minute's URL ride on the record: at registration
// when the archive has them, else once it fetches them, keeping every other
// field. An empty summary is written and counted apart; a record whose title
// went blank gets the minute's title with its summary. A rerun writes nothing.
func TestRegisterFeishuLateFields(t *testing.T) {
	w := newFeishuWorld(t)
	const sealAt, valveAt = "2020-03-02T19:11:08Z", "2020-03-04T07:26:33Z"
	say := `{"segments": [{"speaker": "Bob Example", "start_s": 1, "text": "Sealant in, then the tyre."}]}`
	sealant := map[string]any{"title": "Tubeless sealant", "created": sealAt, "state": "done", "fetched": "x",
		"summary": "Sealant added.", "url": "https://tenant.example/minutes/obcn7q2"}
	valve := map[string]any{"title": "Presta valves", "created": valveAt, "state": "done", "fetched": "x"}
	w.minutes(map[string]map[string]any{"obcn7q2": sealant, "obcn9s1": valve}, map[string]string{"obcn7q2": say, "obcn9s1": say})

	if tl, err := w.run(); err != nil || tl.registered != 2 || tl.summarized != 0 || tl.urlSet != 0 {
		t.Fatalf("first run: %+v, %v", tl, err)
	}
	m, _ := w.reg.rec(t, w.id(sealAt))
	if fe := m["feishu"].(map[string]any); m["summary"] != sealant["summary"] || fe["url"] != sealant["url"] {
		t.Fatalf("sealant at registration: summary %v, feishu %v", m["summary"], fe)
	}
	b, _ := w.reg.rec(t, w.id(valveAt))
	before := b["feishu"].(map[string]any)
	if _, ok := b["summary"]; ok {
		t.Fatalf("valve has a summary before the archive fetched one: %v", b["summary"])
	}
	if _, ok := before["url"]; ok {
		t.Fatalf("valve has a URL before the archive fetched one: %v", before)
	}

	// The archive fetches an empty summary and the URL; the record's title went blank meanwhile.
	key := "rec." + w.id(valveAt)
	rec, v, _, _ := w.u.read(context.Background(), key)
	rec["title"] = " "
	w.reg.CAS(context.Background(), key, recSchema, rec, v)
	valve["summary"], valve["url"] = "", "https://tenant.example/minutes/obcn9s1"
	w.minutes(map[string]map[string]any{"obcn7q2": sealant, "obcn9s1": valve}, nil)
	w.reg.failCAS = 1
	if tl, err := w.run(); err != nil || tl.known != 2 || tl.summarized != 1 || tl.noSummary != 1 || tl.urlSet != 1 || tl.registered != 0 {
		t.Fatalf("late fields: %+v, %v", tl, err)
	}
	b, _ = w.reg.rec(t, w.id(valveAt))
	fe := b["feishu"].(map[string]any)
	if b["title"] != "Presta valves" || b["summary"] != "" || fe["url"] != valve["url"] || fe["token"] != "obcn9s1" || fe["transcript_file"] != before["transcript_file"] {
		t.Fatalf("valve after: title %q summary %q feishu %v", b["title"], b["summary"], fe)
	}

	ops := len(w.reg.ops)
	if tl, err := w.run(); err != nil || tl.summarized != 0 || tl.urlSet != 0 || len(w.reg.ops) != ops {
		t.Fatalf("rerun: %+v, %v, writes %v", tl, err, w.reg.ops[ops:])
	}
}
