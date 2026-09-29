package main

import (
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
