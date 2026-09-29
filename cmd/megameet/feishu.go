package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/caoer/mega-asr/internal/app"
	"github.com/caoer/mega-asr/internal/meeting"
	"github.com/caoer/mega-asr/internal/pages"
)

// minute is one entry of the Feishu archive's state.json
// (scripts/feishu-minutes/fetch.py); fetched is set once it is archived.
type minute struct {
	Token      string  `json:"token"`
	URL        string  `json:"url"` // the minute's page, e.g. https://<tenant>.feishu.cn/minutes/<token>
	Title      string  `json:"title"`
	Created    string  `json:"created"`
	DurationS  float64 `json:"duration_s"`
	Media      string  `json:"media"`
	MediaType  string  `json:"media_type"`
	MediaError string  `json:"media_error"`
	State      string  `json:"state"`
	Fetched    string  `json:"fetched"`
	Summary    *string `json:"summary"` // Feishu's summary; "" when the minute has none, nil until fetched
}

// fetchedMinutes are the archive's fetched minutes, oldest first.
func fetchedMinutes(archive string) ([]minute, error) {
	b, err := os.ReadFile(filepath.Join(archive, "state.json"))
	if err != nil {
		return nil, err
	}
	var st struct {
		Minutes map[string]minute `json:"minutes"`
	}
	if err := json.Unmarshal(b, &st); err != nil {
		return nil, fmt.Errorf("state.json: %w", err)
	}
	var out []minute
	for tok, m := range st.Minutes {
		if m.Fetched == "" || m.State != "done" {
			continue
		}
		m.Token = tok
		out = append(out, m)
	}
	sort.Slice(out, func(i, k int) bool { return out[i].Created < out[k].Created })
	return out, nil
}

// minuteJob is a fetched minute as an upload job: its media (none when
// export was denied) and Feishu's transcript, staged under stage as
// feishu-transcript.json, with the speakers in the order they first speak.
func minuteJob(archive, stage string, m minute, host string) (job, error) {
	dir := filepath.Join(archive, "minutes", m.Token)
	started, err := time.Parse(time.RFC3339, m.Created)
	if err != nil {
		return job{}, fmt.Errorf("%s: created %q: %w", m.Token, m.Created, err)
	}
	started = started.Local()
	j := job{
		ID: started.Format("20060102-150405") + "-feishu-" + host, Source: "feishu", Host: host, Title: m.Title,
		Started: started, Stopped: started.Add(time.Duration(m.DurationS * float64(time.Second))), DurationS: m.DurationS,
		Feishu: m.Token, FeishuURL: m.URL, Summary: m.Summary,
	}
	if m.Media != "" && m.MediaError == "" {
		p := filepath.Join(dir, m.Media)
		if _, err := os.Stat(p); err != nil {
			return j, err
		}
		codec := mediaCodecs[m.MediaType]
		if codec == "" {
			codec = codecOf(p)
		}
		j.Files = append(j.Files, jobFile{Role: "media", Path: p, Codec: codec})
	}
	tj, err := os.ReadFile(filepath.Join(dir, "transcript.json"))
	if errors.Is(err, os.ErrNotExist) {
		if len(j.Files) == 0 {
			return j, fmt.Errorf("%s: neither media nor transcript in the archive", m.Token)
		}
		return j, nil
	}
	if err != nil {
		return j, err
	}
	var t struct {
		Segments []struct {
			Speaker string `json:"speaker"`
		} `json:"segments"`
	}
	if err := json.Unmarshal(tj, &t); err != nil {
		return j, fmt.Errorf("%s transcript.json: %w", m.Token, err)
	}
	seen := map[string]bool{}
	for _, s := range t.Segments {
		if s.Speaker != "" && !seen[s.Speaker] {
			seen[s.Speaker] = true
			j.SpeakerNames = append(j.SpeakerNames, s.Speaker)
		}
	}
	p := filepath.Join(stage, m.Token, "feishu-transcript.json")
	if old, err := os.ReadFile(p); err != nil || !bytes.Equal(old, tj) {
		if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
			return j, err
		}
		if err := os.WriteFile(p, tj, 0o600); err != nil {
			return j, err
		}
	}
	j.Files = append(j.Files, jobFile{Role: "feishu-transcript", Path: p, Codec: "json"})
	return j, nil
}

// mediaCodecs names the archive's media by its content type: fetch.py saves
// a type it does not know as media.bin.
var mediaCodecs = map[string]string{"audio/mp4": "m4a", "video/mp4": "mp4", "audio/x-wav": "wav", "audio/wav": "wav"}

// feishuSource finds a minute's token in a transcript.md front matter line
// of the form `source: 飞书妙记 <token>`.
var feishuSource = regexp.MustCompile(`^source:\s*"?飞书妙记 (obcn[0-9a-z]+)`)

// filedMinutes are the Feishu minutes the wiki already holds. megameet's
// contract with the wiki: a minute is filed as a bundle directory anywhere
// under inbox/filed/ whose transcript.md names the token in its front
// matter (feishuSource); its page is sources/meetings/<bundle>.md, else the
// bundle's about.md. The result maps token → that page and the last commit
// that touched it; a page with no commit yet is not filed.
func filedMinutes(wiki string) (map[string]meeting.Wiki, error) {
	out := map[string]meeting.Wiki{}
	root := filepath.Join(wiki, "inbox", "filed")
	err := filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() || d.Name() != "transcript.md" {
			return err
		}
		tok := frontmatterToken(p)
		if tok == "" {
			return nil
		}
		bundle := filepath.Dir(p)
		page := filepath.Join("sources", "meetings", filepath.Base(bundle)+".md")
		if _, err := os.Stat(filepath.Join(wiki, page)); err != nil {
			rel, _ := filepath.Rel(wiki, bundle)
			page = filepath.Join(rel, "about.md")
		}
		c, err := exec.Command("git", "-C", wiki, "log", "-1", "--format=%H", "--", page).Output()
		if err != nil {
			return fmt.Errorf("git log %s: %w", page, err)
		}
		if commit := strings.TrimSpace(string(c)); commit != "" {
			out[tok] = meeting.Wiki{Page: filepath.ToSlash(page), Commit: commit}
		}
		return nil
	})
	return out, err
}

func frontmatterToken(path string) string {
	f, err := os.Open(path)
	if err != nil {
		return ""
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	for n := 0; sc.Scan() && n < 40; n++ {
		line := sc.Text()
		if n > 0 && line == "---" {
			break
		}
		if m := feishuSource.FindStringSubmatch(line); m != nil {
			return m[1]
		}
	}
	return ""
}

// feishuRegistry is the page as register-feishu uses it.
type feishuRegistry interface {
	registry
	List(ctx context.Context, prefix string) ([]pages.Object, error)
}

// registered is a feishu record already on the page.
type registered struct {
	id, state  string
	wiki       bool
	title      string
	hasSummary bool
	hasURL     bool
}

type feishuTally struct {
	fetched, registered, transcriptOnly, known, deleted, wikiSet, summarized, noSummary, urlSet, failed int
	bytes                                                                                               int64
}

// registerFeishu registers every fetched minute the page lacks as a feishu
// record; one left uploading by an earlier run is resumed. A deleted record
// (a tombstone) is left as it is: its minute is never registered again. A record whose
// minute the wiki has filed since gets wiki set, until it is ingested. A
// record without a summary gets the minute's title and Feishu's summary once
// the archive has fetched it, and one without feishu.url gets the minute's
// URL.
func registerFeishu(ctx context.Context, reg feishuRegistry, u *uploader, archive, wiki, stage, host string) (feishuTally, error) {
	var t feishuTally
	mins, err := fetchedMinutes(archive)
	if err != nil {
		return t, err
	}
	t.fetched = len(mins)
	filed, err := filedMinutes(wiki)
	if err != nil {
		return t, err
	}
	objs, err := reg.List(ctx, "rec.")
	if err != nil {
		return t, err
	}
	have := map[string]registered{}
	for _, o := range objs {
		var r struct {
			ID      string                      `json:"id"`
			Source  string                      `json:"source"`
			State   string                      `json:"state"`
			Title   string                      `json:"title"`
			Summary *string                     `json:"summary"`
			Feishu  struct{ Token, URL string } `json:"feishu"`
			Wiki    json.RawMessage             `json:"wiki"`
		}
		if json.Unmarshal(o.Value.Data, &r) != nil || r.Source != "feishu" || r.Feishu.Token == "" {
			continue
		}
		have[r.Feishu.Token] = registered{id: r.ID, state: r.State, wiki: len(r.Wiki) > 0 && string(r.Wiki) != "null",
			title: r.Title, hasSummary: r.Summary != nil, hasURL: r.Feishu.URL != ""}
	}
	var errs []error
	for _, m := range mins {
		h, ok := have[m.Token]
		if ok && h.state == meeting.Deleted {
			t.deleted++
			continue
		}
		if ok && h.state != "uploading" {
			t.known++
			if !h.hasSummary && m.Summary != nil {
				if err := setSummary(ctx, u, h.id, m.Title, *m.Summary); err != nil {
					t.failed++
					errs = append(errs, fmt.Errorf("%s summary: %w", m.Token, err))
				} else {
					t.summarized++
					if *m.Summary == "" {
						t.noSummary++
					}
				}
			}
			if !h.hasURL && m.URL != "" {
				if err := setFeishuURL(ctx, u, h.id, m.URL); err != nil {
					t.failed++
					errs = append(errs, fmt.Errorf("%s url: %w", m.Token, err))
				} else {
					t.urlSet++
				}
			}
			if w, f := filed[m.Token]; f && !h.wiki && h.state != "ingested" && h.state != "failed" {
				if err := setWiki(ctx, u, h.id, w); err != nil {
					errs = append(errs, fmt.Errorf("%s wiki: %w", m.Token, err))
				} else {
					t.wikiSet++
				}
			}
			continue
		}
		j, err := minuteJob(archive, stage, m, host)
		if err != nil {
			t.failed++
			errs = append(errs, err)
			continue
		}
		if ok {
			j.ID = h.id
		}
		if w, f := filed[m.Token]; f {
			j.Wiki = &w
		}
		if _, err := os.Stat(u.ledger.path(j.ID)); errors.Is(err, os.ErrNotExist) {
			if err := u.ledger.put(j); err != nil {
				return t, err
			}
		}
		if err := u.run(ctx, j.ID); err != nil {
			t.failed++
			errs = append(errs, fmt.Errorf("%s (%s): %w", m.Token, j.ID, err))
			continue
		}
		t.registered++
		if len(j.Files) == 1 && j.Files[0].Role == "feishu-transcript" {
			t.transcriptOnly++
		}
		for _, f := range j.Files {
			if st, err := os.Stat(f.Path); err == nil {
				t.bytes += st.Size()
			}
		}
	}
	return t, errors.Join(errs...)
}

// setSummary writes Feishu's summary onto a record by CAS, and the minute's
// title when the record has none, keeping every other field.
func setSummary(ctx context.Context, u *uploader, id, title, summary string) error {
	key := "rec." + id
	for try := 0; ; try++ {
		rec, v, _, err := u.read(ctx, key)
		if err != nil {
			return err
		}
		if _, ok := rec["summary"]; ok {
			return nil // written meanwhile
		}
		if t, _ := rec["title"].(string); strings.TrimSpace(t) == "" && title != "" {
			rec["title"] = title
		}
		rec["summary"] = summary
		rec["updated"] = u.now()
		_, err = u.reg.CAS(ctx, key, recSchema, rec, v)
		if pages.Code(err) == "version_conflict" && try < 5 {
			continue
		}
		return err
	}
}

// setFeishuURL writes the minute's URL as feishu.url onto a record by CAS,
// keeping every other field.
func setFeishuURL(ctx context.Context, u *uploader, id, url string) error {
	key := "rec." + id
	for try := 0; ; try++ {
		rec, v, _, err := u.read(ctx, key)
		if err != nil {
			return err
		}
		fe, _ := rec["feishu"].(map[string]any)
		if fe == nil {
			return fmt.Errorf("%s has no feishu", key)
		}
		if s, _ := fe["url"].(string); s != "" {
			return nil // written meanwhile
		}
		fe["url"] = url
		rec["updated"] = u.now()
		_, err = u.reg.CAS(ctx, key, recSchema, rec, v)
		if pages.Code(err) == "version_conflict" && try < 5 {
			continue
		}
		return err
	}
}

// setWiki writes wiki onto a record by CAS, keeping every other field.
func setWiki(ctx context.Context, u *uploader, id string, w meeting.Wiki) error {
	key := "rec." + id
	for try := 0; ; try++ {
		rec, v, _, err := u.read(ctx, key)
		if err != nil {
			return err
		}
		rec["wiki"] = w
		rec["updated"] = u.now()
		_, err = u.reg.CAS(ctx, key, recSchema, rec, v)
		if pages.Code(err) == "version_conflict" && try < 5 {
			continue
		}
		return err
	}
}

func registerFeishuCmd(o app.LoadOpts, args []string) error {
	l, err := app.Load(o)
	if err != nil {
		return err
	}
	c := l.Meeting
	fset := flag.NewFlagSet("register-feishu", flag.ContinueOnError)
	archive := fset.String("archive", c.Align.FeishuArchive, "the Feishu Minutes archive (state.json)")
	wiki := fset.String("wiki", c.Ingest.Wiki, "the wiki checkout, for the minutes already filed")
	if err := fset.Parse(args); err != nil {
		return err
	}
	if fset.NArg() > 0 {
		return fmt.Errorf("register-feishu: unexpected %q", fset.Args())
	}
	if *archive == "" {
		return errors.New("register-feishu: no archive (meeting.align.feishu_archive or --archive)")
	}
	if *wiki == "" {
		return errors.New("register-feishu: no wiki (meeting.ingest.wiki or --wiki): a minute already filed would be ingested twice")
	}
	cl, err := pages.FromConfig(c)
	if err != nil {
		return err
	}
	u := &uploader{reg: cl, ledger: ledger(filepath.Join(c.Data, "pending")), now: time.Now}
	t, err := registerFeishu(context.Background(), cl, u, *archive, *wiki, filepath.Join(c.Data, "feishu"), hostName())
	fmt.Printf("register-feishu: %d fetched, %d registered (%.1f MB, %d transcript-only), %d already registered, %d deleted (skipped), %d wiki set, %d summaries set (%d empty), %d urls set, %d failed\n",
		t.fetched, t.registered, float64(t.bytes)/1e6, t.transcriptOnly, t.known, t.deleted, t.wikiSet, t.summarized, t.noSummary, t.urlSet, t.failed)
	return err
}
