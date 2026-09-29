package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"text/tabwriter"
	"time"

	"github.com/caoer/mega-asr/internal/app"
	"github.com/caoer/mega-asr/internal/audio"
	"github.com/caoer/mega-asr/internal/ctl"
	"github.com/caoer/mega-asr/internal/meeting"
	"github.com/caoer/mega-asr/internal/pages"
)

// Last is what the ctl `last` command answers, for MegaVoice's menu: the
// newest finished recording on this Mac and its record on the meetings page.
type Last struct {
	ID        string        `json:"id"`
	Title     string        `json:"title,omitempty"`
	Started   time.Time     `json:"started"`
	DurationS float64       `json:"duration_s"`
	Error     string        `json:"error,omitempty"`  // why the take ended on its own
	Upload    string        `json:"upload,omitempty"` // this serve's upload progress
	State     string        `json:"state,omitempty"`  // the record's: uploading … ingested, failed; "" when the page has none
	Stage     string        `json:"stage,omitempty"`
	RecError  string        `json:"rec_error,omitempty"` // the record's error when failed
	Wiki      *meeting.Wiki `json:"wiki,omitempty"`
	WikiOpen  string        `json:"wiki_open,omitempty"` // what opens the wiki page: its URL, or the file in meeting.ingest.wiki
	PageError string        `json:"page_error,omitempty"`
	Deleted   bool          `json:"deleted,omitempty"` // uploaded once, and the page's record is gone or a tombstone
}

// lastRecording answers `last`: nil when this Mac has no finished recording.
func lastRecording(ctx context.Context, r *recorder, reg *pages.Client, wiki string) (*Last, error) {
	metas, err := filepath.Glob(filepath.Join(r.dir, "*", "meta.toml"))
	if err != nil {
		return nil, err
	}
	slices.Sort(metas) // ids begin with the start time
	var m *Meta
	for i := len(metas) - 1; i >= 0 && m == nil; i-- {
		x, err := readMeta(filepath.Dir(metas[i]))
		if err == nil && !x.Stopped.IsZero() {
			m = &x
		}
	}
	if m == nil {
		return nil, nil
	}
	l := &Last{ID: m.ID, Title: m.Title, Started: m.Started, DurationS: m.DurationS, Error: m.Error}
	r.mu.Lock()
	l.Upload = r.uploads[m.ID]
	r.mu.Unlock()
	if reg == nil {
		l.PageError = "no meetings page configured (meeting.page.slug)"
		return l, nil
	}
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	o, err := reg.Record(ctx, "rec."+m.ID)
	switch {
	case pages.Code(err) == "no_such_object":
		l.Deleted = !m.UploadedAt.IsZero()
		return l, nil
	case err != nil:
		l.PageError = err.Error()
		return l, nil
	}
	var rec meeting.Record
	if err := json.Unmarshal(o.Value.Data, &rec); err != nil {
		l.PageError = fmt.Sprintf("rec.%s: %v", m.ID, err)
		return l, nil
	}
	if rec.State == meeting.Deleted {
		l.Deleted = true
		return l, nil
	}
	l.State, l.Stage, l.RecError, l.Wiki = rec.State, rec.Stage, rec.Error, rec.Wiki
	if w := rec.Wiki; w != nil && w.Page != "" {
		switch p := filepath.Join(wiki, w.Page); {
		case len(w.Page) > 8 && (w.Page[:7] == "http://" || w.Page[:8] == "https://"):
			l.WikiOpen = w.Page
		case wiki != "" && fileExists(p):
			l.WikiOpen = p
		}
	}
	return l, nil
}

func fileExists(p string) bool {
	_, err := os.Stat(p)
	return err == nil
}

// menuHandler adds `last` to the recorder's commands.
func menuHandler(h func(ctl.Request) ctl.Response, r *recorder, reg *pages.Client, wiki string) func(ctl.Request) ctl.Response {
	return func(req ctl.Request) ctl.Response {
		if req.Cmd != "last" {
			return h(req)
		}
		l, err := lastRecording(context.Background(), r, reg, wiki)
		if err != nil {
			return ctl.Fail(err)
		}
		return ctl.Reply(l)
	}
}

// mics prints the input devices and which one meeting.capture.mic selects.
func mics(o app.LoadOpts) error {
	in, err := audio.Inputs()
	if err != nil {
		return err
	}
	sel := "default"
	if l, err := app.Load(o); err == nil {
		sel = l.Meeting.Capture.Mic
	}
	w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintln(w, "USE\tDEFAULT\tUID\tNAME")
	yn := map[bool]string{true: "Y", false: "-"}
	for _, d := range in {
		use := d.UID == sel || (sel == "default" && d.Default)
		fmt.Fprintf(w, "%s\t%s\t%s\t%s\n", yn[use], yn[d.Default], d.UID, d.Name)
	}
	if err := w.Flush(); err != nil {
		return err
	}
	fmt.Println("set one: megavoice config set meeting.capture.mic <UID>  (or default)")
	return nil
}
