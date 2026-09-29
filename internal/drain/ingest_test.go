package drain

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func writeJSON(t *testing.T, path string, v any) {
	t.Helper()
	os.MkdirAll(filepath.Dir(path), 0o700)
	b, _ := json.Marshal(v)
	if err := os.WriteFile(path, b, 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestBuildRecord(t *testing.T) {
	dir := t.TempDir()
	writeJSON(t, filepath.Join(dir, "segments.json"), []map[string]any{
		{"track": "remote", "speaker": "remote", "start_s": 5.0, "text": "你好"},
		{"track": "mic", "speaker": "mic", "start_s": 1.0, "text": "hi"},
		{"track": "mic", "speaker": "mic", "start_s": 2.0, "text": "  "},
	})
	os.MkdirAll(filepath.Join(dir, "tracks"), 0o700)
	for _, n := range []string{"mic.flac", "remote.flac"} {
		os.WriteFile(filepath.Join(dir, "tracks", n), []byte("x"), 0o600)
	}
	rec := map[string]any{"id": "20200708-091500-mac-host-a", "source": "mac", "title": "Loan desk hours", "started": "2020-07-08T09:15:00Z",
		"duration_s": 60.0, "speakers": []any{map[string]any{"name": "Alice Example", "role": "mic"}, map[string]any{"name": "Bob Example", "role": "remote"}},
		"engine": map[string]any{"root": "/x/funasr-root", "sha": "ab12"}}
	r, err := BuildRecord(dir, rec)
	if err != nil {
		t.Fatal(err)
	}
	if len(r.Turns) != 2 || r.Turns[0].Text != "hi" || r.Speakers["remote"] != "Bob Example" || r.Engine != "megameet funasr-root@ab12" {
		t.Fatalf("%+v", r)
	}
	if len(r.Media) != 2 || r.Media[0].Name != "mic" || !strings.HasSuffix(r.Media[1].Ref, "tracks/remote.flac") {
		t.Fatalf("media %+v", r.Media)
	}

	// A Feishu minute is ingested from Feishu's own turns.
	writeJSON(t, filepath.Join(dir, "feishu-transcript.json"), map[string]any{"segments": []map[string]any{{"speaker": "Bob Example", "start_s": 0.5, "text": "嗯"}}})
	rec["source"] = "feishu"
	if r, err = BuildRecord(dir, rec); err != nil || len(r.Turns) != 1 || r.Turns[0].Speaker != "Bob Example" || r.Engine != "Feishu Minutes" {
		t.Fatalf("%+v %v", r, err)
	}
	// Its record.json carries the minute's URL as url, once feishu.url has it.
	if b, _ := json.Marshal(r); strings.Contains(string(b), `"url"`) {
		t.Fatalf("url without feishu.url: %s", b)
	}
	rec["feishu"] = map[string]any{"token": "obcnaaa", "url": "https://t.feishu.cn/minutes/obcnaaa"}
	r, _ = BuildRecord(dir, rec)
	if b, _ := json.Marshal(r); !strings.Contains(string(b), `"url":"https://t.feishu.cn/minutes/obcnaaa"`) {
		t.Fatalf("record.json: %s", b)
	}
}

// The drain runs the ingest, reads the commit and page it reports, and
// keeps rec.json beside the audio in the assets checkout.
func TestIngestRunsTheCommandAndBacksUpTheRecord(t *testing.T) {
	for _, bin := range []string{"git", "git-annex"} {
		if _, err := exec.LookPath(bin); err != nil {
			t.Skip(bin + " not on PATH")
		}
	}
	root := t.TempDir()
	t.Cleanup(func() { exec.Command("chmod", "-R", "u+w", root).Run() }) // annex objects are read-only
	sh := func(dir string, args ...string) {
		t.Helper()
		cmd := exec.Command(args[0], args[1:]...)
		cmd.Dir = dir
		cmd.Env = append(os.Environ(), "GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@t", "GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@t")
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("%v: %v\n%s", args, err, out)
		}
	}
	t.Setenv("GIT_AUTHOR_NAME", "t")
	t.Setenv("GIT_AUTHOR_EMAIL", "t@t")
	t.Setenv("GIT_COMMITTER_NAME", "t")
	t.Setenv("GIT_COMMITTER_EMAIL", "t@t")
	bare := filepath.Join(root, "remote", "media-archive.git")
	repos := filepath.Join(root, "repos")
	assets := filepath.Join(repos, "media-archive")
	os.MkdirAll(bare, 0o755)
	sh(bare, "git", "init", "-q", "--bare", "-b", "main")
	sh(root, "git", "clone", "-q", bare, assets)
	sh(assets, "git", "annex", "init", "-q", "test")
	os.MkdirAll(filepath.Join(root, "store-dir"), 0o755)
	sh(assets, "git", "annex", "initremote", "store-dir", "type=directory", "directory="+filepath.Join(root, "store-dir"), "encryption=none")
	sh(assets, "git", "commit", "-q", "--allow-empty", "-m", "init")
	sh(assets, "git", "push", "-q", "origin", "main", "git-annex")

	// The ingest: writes meeting.json and reports its commit, as the ingest does.
	script := filepath.Join(root, "ingest.sh")
	os.WriteFile(script, []byte(`#!/bin/sh
set -e
[ "$1" = run ] || exit 2
out=$4
grep -q '"text": "hi"' "$2"
printf '{"page": "minutes/m-r1.md", "bundle": "attachments/r1", "audio": []}' > "$out/meeting.json"
printf -- '- What goes on the new shelf?\n- Who locks up on Sundays?\n- Does the insurance cover loaned tools?\n' > "$out/notes.md"
echo "commit: media-archive 1234567"
echo "commit: notes 89abcde"
echo "args: $*" >&2
`), 0o755)

	dir := filepath.Join(root, "pull")
	writeJSON(t, filepath.Join(dir, "segments.json"), []map[string]any{{"speaker": "mic", "start_s": 1.0, "text": "hi"}})
	var lines []string
	in := &Ingester{Command: []string{"sh", script}, Repos: repos, Runs: filepath.Join(root, "runs"), Notify: func(l string) { lines = append(lines, l) },
		Wiki: "notes", Assets: assets, AssetsRemote: "origin", AssetsAnnexRemote: "store-dir"}
	rec := map[string]any{"id": "r1", "source": "mac", "title": "Loan desk hours", "started": "2020-07-08T09:15:00Z", "state": "aligned",
		"claim": map[string]any{"by": "h"}}
	w, err := in.Ingest(context.Background(), "r1", dir, rec, false)
	if err != nil {
		t.Fatal(err)
	}
	if w.Commit != "89abcde" || w.Page != "minutes/m-r1.md" {
		t.Fatalf("wiki %+v", w)
	}
	// One message for the meeting: its page and the run's questions.
	want := "megameet: «Loan desk hours» is in the wiki: minutes/m-r1.md (89abcde, rec.r1)\nQuestions:\n- What goes on the new shelf?\n- Who locks up on Sundays?\n- Does the insurance cover loaned tools?"
	if len(lines) != 1 || lines[0] != want {
		t.Fatalf("notify %q", lines)
	}
	// rec.json reached the remote's main, annexed, its content on the annex remote.
	check := filepath.Join(root, "check")
	sh(root, "git", "clone", "-q", bare, check)
	sh(check, "git", "annex", "init", "-q", "check")
	sh(check, "git", "annex", "enableremote", "store-dir", "directory="+filepath.Join(root, "store-dir"))
	sh(check, "git", "annex", "get", "-q", "attachments/r1/audio/rec.json")
	var got map[string]any
	b, err := os.ReadFile(filepath.Join(check, "attachments/r1/audio/rec.json"))
	if err != nil {
		t.Fatal(err)
	}
	json.Unmarshal(b, &got)
	if got["state"] != "ingested" || got["claim"] != nil || got["wiki"].(map[string]any)["commit"] != "89abcde" {
		t.Fatalf("rec.json %s", b)
	}
	// A finished run is not resumed: the next ingest gets a new folder.
	runs, _ := filepath.Glob(filepath.Join(root, "runs", "r1", "*", "wiki.json"))
	if len(runs) != 1 {
		t.Fatalf("run folders %v", runs)
	}
}

// A Feishu minute the backfill filed since it was registered is not
// ingested again: its wiki is written instead.
func TestIngestSkipsAFiledMinute(t *testing.T) {
	in := &Ingester{Command: []string{"false"}, Runs: t.TempDir(), Filed: func(token string) (Wiki, bool, error) {
		return Wiki{Page: "minutes/x.md", Commit: "c0ffee1"}, token == "obcn", nil
	}}
	rec := map[string]any{"id": "f1", "source": "feishu", "feishu": map[string]any{"token": "obcn"}}
	w, err := in.Ingest(context.Background(), "f1", t.TempDir(), rec, false)
	if err != nil || w.Commit != "c0ffee1" {
		t.Fatalf("%+v %v", w, err)
	}
	// Asked again (reingest): the check is not the ingest's to skip.
	if _, err := in.Ingest(context.Background(), "f1", t.TempDir(), rec, true); err == nil {
		t.Fatal("replace skipped the ingest")
	}
}

// The primary commit: meeting.json's wikis[] primary when it names one,
// else the configured wiki's `commit: <name> <sha>` line, whatever order
// the lines come in; with no wiki configured, wikis[] alone.
func TestPrimaryCommit(t *testing.T) {
	legacy := "commit: media-archive 1234567\ncommit: notes 89abcde\n"
	multi := "commit: wiki-one 1111111\ncommit: wiki-two 2222222\n"
	var none runMeeting
	var named, bare runMeeting
	json.Unmarshal([]byte(`{"wikis": [{"name": "wiki-one", "role": "delta", "commit": "1111111"}, {"name": "wiki-two", "role": "primary", "commit": "abcdef0"}]}`), &named)
	json.Unmarshal([]byte(`{"wikis": [{"name": "wiki-two", "role": "primary"}]}`), &bare)
	for _, c := range []struct {
		name, stdout string
		m            runMeeting
		fallback     string
		wiki, sha    string
		ok           bool
	}{
		{"script output: the configured wiki's line, not the first", legacy, none, "notes", "notes", "89abcde", true},
		{"wikis[] primary carries its commit", multi, named, "notes", "wiki-two", "abcdef0", true},
		{"wikis[] primary without a commit: its line", multi, bare, "notes", "wiki-two", "2222222", true},
		{"no wikis[] and no configured wiki's line", multi, none, "notes", "notes", "", false},
		{"no wikis[] and no wiki configured", legacy, none, "", "", "", false},
		{"wikis[] primary with no line", legacy, bare, "notes", "wiki-two", "", false},
	} {
		wiki, sha, ok := primaryCommit(c.stdout, c.m, c.fallback)
		if wiki != c.wiki || sha != c.sha || ok != c.ok {
			t.Errorf("%s: got %s %s %v", c.name, wiki, sha, ok)
		}
	}
}

// Status reads the current run's ingest.json as the record's ingest key.
func TestIngestStatusReadsTheRun(t *testing.T) {
	root := t.TempDir()
	script := filepath.Join(root, "ingest.sh")
	os.WriteFile(script, []byte(`#!/bin/sh
out=$4
printf '{"run": "20200708T091500Z-r1", "state": "failed", "usd": 1.25, "page_slug": "ingest-x", "passes": {"write": {}},
  "error": "pass 3 of 4\\n  ran out of budget", "updated_at": "2020-07-08T09:31:00Z"}' > "$out/ingest.json"
exit 1
`), 0o755)
	dir := filepath.Join(root, "pull")
	writeJSON(t, filepath.Join(dir, "segments.json"), []map[string]any{{"speaker": "mic", "start_s": 1.0, "text": "hi"}})
	in := &Ingester{Command: []string{"sh", script}, Runs: filepath.Join(root, "runs")}
	if _, ok := in.Status("r1"); ok {
		t.Fatal("status before any run")
	}
	rec := map[string]any{"id": "r1", "source": "mac", "title": "t", "started": "2020-07-08T09:15:00Z"}
	if _, err := in.Ingest(context.Background(), "r1", dir, rec, false); err == nil {
		t.Fatal("a failed run ingested")
	}
	s, ok := in.Status("r1")
	want := map[string]any{"run": "20200708T091500Z-r1", "state": "failed", "usd": 1.25, "page_slug": "ingest-x",
		"error": "pass 3 of 4 ran out of budget", "updated_at": "2020-07-08T09:31:00Z"}
	if b, w := mustJSON(s), mustJSON(want); !ok || b != w {
		t.Fatalf("status %s, want %s", b, w)
	}
	// A running pass: state running, step the pass.
	runs, _ := filepath.Glob(filepath.Join(root, "runs", "r1", "*"))
	writeJSON(t, filepath.Join(runs[0], "ingest.json"), map[string]any{"run": "x", "state": "review", "wiki_page": "minutes/p.md"})
	if s, _ = in.Status("r1"); s["state"] != "running" || s["step"] != "review" || s["wiki_page"] != "minutes/p.md" {
		t.Fatalf("status %v", s)
	}
}

func mustJSON(v any) string {
	b, _ := json.Marshal(v)
	return string(b)
}
