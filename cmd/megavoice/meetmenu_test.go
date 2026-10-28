package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/caoer/mega-asr/internal/app"
	"github.com/caoer/mega-asr/internal/audio"
)

func titles(rs []row) string {
	var b strings.Builder
	for _, r := range rs {
		switch {
		case r.sep:
			b.WriteString("---\n")
			continue
		case r.checked:
			b.WriteString("✓")
		}
		b.WriteString(strings.Repeat("  ", r.indent) + r.title)
		if r.act != nil && r.enabled {
			b.WriteString(" *")
		}
		if r.sub != nil {
			b.WriteString(" > [" + strings.ReplaceAll(strings.TrimSpace(titles(r.sub)), "\n", " | ") + "]")
		}
		b.WriteString("\n")
	}
	return b.String()
}

var inputs = []audio.Input{
	{UID: "handheld-bt", Name: "Handheld BT"},
	{UID: "podium-condenser", Name: "Podium Condenser"},
	{UID: "boom-mic", Name: "Boom Mic"},
	{UID: "builtin-mic", Name: "Built-in Microphone", Default: true},
	{UID: "stage-interface", Name: "Stage Interface"},
}

func TestMeetMenu(t *testing.T) {
	started := time.Date(2020, 6, 11, 16, 5, 0, 0, time.Local)
	const page = "https://pages.example/a/fieldwork-log/"
	rec := meetStatus{Recording: true, Seconds: 1985, Mic: &meetMic{Device: "Boom Mic", LevelDBFS: -24}}
	rec.Current = &struct {
		ID      string
		Title   string
		Started time.Time
	}{ID: "x", Title: "Roof repair estimate"}
	silent := rec
	silent.Mic = &meetMic{Device: "Boom Mic", LevelDBFS: -120, Silent: true, Since: time.Now().Add(-9 * time.Second)}
	ingested := &meetLast{ID: "20200611-160500-host-k2", Title: "Roof repair estimate", Started: started, DurationS: 2710, State: "ingested",
		Wiki: &struct {
			Page string `json:"page"`
		}{Page: "notes/roof-repair.md"}, WikiOpen: "/w/notes/roof-repair.md"}

	cases := []struct {
		name string
		st   meetState
		bar  barLook
		want string
	}{
		{"idle", meetState{Inputs: inputs, MicPick: "default", Page: page, Last: ingested}, barLook{symbol: "waveform"}, `Start Meeting… *
---
Mic: Built-in Microphone
Microphone > [✓System default (Built-in Microphone) * | --- | Handheld BT * | Podium Condenser * | Boom Mic * | Built-in Microphone * | Stage Interface *]
---
Last: Roof repair estimate — Jun 11 16:05, 45:10
  in the wiki
  Wiki: roof-repair *
Open Meetings Page *
`},
		{"recording", meetState{Status: rec, Inputs: inputs, MicPick: "boom-mic", Page: page}, barLook{"record.circle.fill", "red", "33:05"}, `● Recording 33:05 — Roof repair estimate
Stop Meeting *
---
Mic: Boom Mic
  ▰▰▰▰▰▰▰▰▰▰▰▰▱▱▱▱▱▱▱▱  -24 dBFS
Microphone > [System default (Built-in Microphone) * | --- | Handheld BT * | Podium Condenser * | ✓Boom Mic * | Built-in Microphone * | Stage Interface * | --- | A new pick applies at the next start]
---
No meeting recorded on this Mac yet
Open Meetings Page *
`},
		{"silent mic", meetState{Status: silent, Inputs: inputs, MicPick: "default", Page: page}, barLook{"exclamationmark.triangle.fill", "orange", "33:05"}, `● Recording 33:05 — Roof repair estimate
Stop Meeting *
---
Mic: Boom Mic
  ▱▱▱▱▱▱▱▱▱▱▱▱▱▱▱▱▱▱▱▱  no signal
  ⚠︎ No audio from this mic for 9 s
  Another mic takes effect at the next start
Microphone > [✓System default (Built-in Microphone) * | --- | Handheld BT * | Podium Condenser * | Boom Mic * | Built-in Microphone * | Stage Interface * | --- | A new pick applies at the next start]
---
No meeting recorded on this Mac yet
Open Meetings Page *
`},
		{"deleted", meetState{Down: "x", Page: page, Last: &meetLast{ID: "20200611-091200-host-k2", Title: "Supplier call", Started: started, DurationS: 1330, Deleted: true}}, barLook{symbol: "waveform"}, `MegaMeet is not running
  x
---
Last: Supplier call — Jun 11 16:05, 22:10
  deleted from the meetings page
Open Meetings Page *
`},
		{"down", meetState{Down: "no answer on /s/ctl.sock", Last: &meetLast{ID: "20200611-160500-host-k2", Started: started, DurationS: 380, State: "failed", RecError: "asr: no speech"}}, barLook{symbol: "waveform"}, `MegaMeet is not running
  no answer on /s/ctl.sock
---
Last: 20200611-160500-host-k2 — Jun 11 16:05, 6:20
  failed: asr: no speech
`},
	}
	if got := meetingsPage(app.MeetingPageConfig{URL: "https://pages.example/", Slug: "fieldwork-log"}); got != page {
		t.Errorf("meetingsPage %q, want %q", got, page)
	}
	if got := meetingsPage(app.MeetingPageConfig{URL: "https://pages.example"}); got != "" {
		t.Errorf("meetingsPage without a slug %q, want none", got)
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := c.st.bar(); got != c.bar {
				t.Errorf("bar %+v, want %+v", got, c.bar)
			}
			if got := titles(c.st.meetRows()); got != c.want {
				t.Errorf("rows:\n%s\nwant:\n%s", got, c.want)
			}
		})
	}
}

// TestStagingMenu: the build friends install shows dictation alone — no
// meeting rows (it ships no MegaMeet), no compare, hotwords, corrections or
// checks — while a dev build keeps every row.
func TestStagingMenu(t *testing.T) {
	settled := voiceSettings{TapKey: "right_option", WholeMax: time.Minute, Engine: "funasr"}
	v := voiceState{Running: settled, File: settled, Hotwords: "/c/hotwords.txt", Corrections: "/c/corrections.tsv",
		Takes: "http://127.0.0.1:7865/takes/", Logs: []string{"/L/megavoice.log"}}
	devOnly := []string{"MegaMeet is not running", "No meeting recorded on this Mac yet", "Compare Mode", "Compare Engines",
		"Edit Hotwords…", "Edit Corrections…", "Edits apply at the next take", "Checks", "MegaMeet Status"}
	dev := titles(meetState{Down: "no answer on /s/ctl.sock", Voice: v}.rows())
	for _, s := range devOnly {
		if !strings.Contains(dev, s) {
			t.Errorf("dev menu lacks %q:\n%s", s, dev)
		}
	}
	if strings.Contains(dev, "检查更新") {
		t.Errorf("dev menu offers 检查更新:\n%s", dev)
	}
	v.Staging = true
	staging := titles(meetState{Down: "no answer on /s/ctl.sock", Voice: v}.rows())
	for _, s := range append(devOnly, "Meeting", "megameet") {
		if strings.Contains(staging, s) {
			t.Errorf("staging menu shows %q:\n%s", s, staging)
		}
	}
	for _, s := range []string{"Dictation", "打开录音历史", "Key: Right Option", "Engine: Fun-ASR (local)", "Restart " + app.Name() + " *", "检查更新… *", "Quit " + app.Name() + " *"} {
		if !strings.Contains(staging, s) {
			t.Errorf("staging menu lacks %q:\n%s", s, staging)
		}
	}
}

// TestMenuConfig: the menu warns about the config file only when one is
// there and does not load, or a named one is missing; an absent default file
// means the defaults, with no warning.
func TestMenuConfig(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "megavoice")
	t.Setenv("XDG_CONFIG_HOME", filepath.Dir(dir))
	t.Setenv("MEGAVOICE_CONFIG", "")
	l, why := menuConfig("")
	if why != "" || l.Path != "" || l.Tap.Key != app.Default().Tap.Key {
		t.Fatalf("absent default file: warning %q, path %q, tap.key %q; want none, none, the default", why, l.Path, l.Tap.Key)
	}
	if _, why := menuConfig(filepath.Join(dir, "named.toml")); !strings.Contains(why, "no such file") {
		t.Errorf("a named file that is missing: warning %q, want no such file", why)
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "config.toml"), []byte("[tap]\nkey = \"caps\"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, why := menuConfig(""); !strings.Contains(why, "tap.key") {
		t.Errorf("a file that does not load: warning %q, want tap.key", why)
	}
}
