package main

import (
	"strings"
	"testing"
	"time"

	"github.com/caoer/mega-asr/internal/app"
	"github.com/caoer/mega-asr/internal/l10n"
	"github.com/caoer/mega-asr/internal/update"
)

// every is each row title of rs and its submenus.
func every(rs []row) []string {
	var out []string
	for _, r := range rs {
		if r.sep {
			continue
		}
		out = append(out, r.title)
		out = append(out, every(r.sub)...)
	}
	return out
}

// The friends' build speaks the Mac's language: in English no menu line,
// restart or quit offer carries Chinese; in Chinese every line does, but
// for names that are the same in both (an input's or a model's own name).
func TestMenuSpeaksTheMacsLanguage(t *testing.T) {
	defer l10n.Set(l10n.Current())
	settled := voiceSettings{TapKey: "right_option", WholeMax: time.Minute, Engine: "funasr"}
	v := voiceState{Running: settled, File: settled, Staging: true, Release: "", Undelivered: 2,
		Takes: "http://127.0.0.1:7865/takes/", Phone: "http://127.0.0.1:7865/takes/pair.html",
		Capture: app.CaptureConfig{Source: "local", Mic: "default"},
		Inputs:  []inputView{{"builtin-mic", "MacBook Air Microphone", 1, true}, {"usb", "USB Audio", 4, false}},
		Remotes: []remoteView{{Name: "den", Addr: "192.0.2.22:7866", Offline: true}},
		Found:   []foundView{{Name: "box", Addr: "192.0.2.11:7866"}}}
	pending := v
	pending.File = voiceSettings{TapKey: "right_shift", WholeMax: 2 * time.Minute, Engine: "doubao"}
	rec := v
	rec.Take = voiceTake{ID: "t", Recording: true, Elapsed: 65 * time.Second}
	sameInBoth := map[string]bool{"MacBook Air Microphone": true, "USB Audio": true, "box": true, "den": true}
	var lines = func() []string {
		var out []string
		for _, s := range []voiceState{v, pending, rec} {
			out = append(out, every(meetState{Voice: s}.rows())...)
		}
		for _, cmd := range []string{"restart", "quit"} {
			for _, stream := range []bool{false, true} {
				o := exitOffer(cmd, 65*time.Second, stream)
				out = append(out, o.title, o.info)
				for _, c := range o.choices {
					out = append(out, c.label)
				}
			}
		}
		rel := &update.Release{Version: "0.1.4", Notes: "修复", NotesEN: "Fixes"}
		return append(out, releaseNotes(rel), takesRowTitle(v))
	}

	l10n.Set(l10n.EN)
	en := lines()
	for _, s := range en {
		if l10n.Chinese(s) {
			t.Errorf("English: %q", s)
		}
	}
	for _, want := range []string{"Dictation", "Open Recordings · 2 not delivered", "iPhone Remote…", "Check for Updates…", "Key: Right Option > ",
		"Engine: Fun-ASR (local)", "Stop and Paste", "Restart Mega Voice Dev", "Quit Mega Voice Dev", "Fixes"} {
		if !strings.Contains(strings.Join(en, "\n"), strings.TrimSuffix(want, " > ")) {
			t.Errorf("English menu lacks %q:\n%s", want, strings.Join(en, "\n"))
		}
	}

	t.Logf("English:\n%s", strings.Join(en, "\n"))
	l10n.Set(l10n.ZH)
	zh := lines()
	t.Logf("Chinese:\n%s", strings.Join(zh, "\n"))
	for _, s := range zh {
		if !l10n.Chinese(s) && !sameInBoth[s] {
			t.Errorf("Chinese: %q", s)
		}
	}
	for _, want := range []string{"听写", "打开录音历史 · 2 条未送达", "iPhone 遥控…", "检查更新…", "按键：右 Option", "引擎：Fun-ASR（本地）", "停止并粘贴", "重启 Mega Voice Dev", "退出 Mega Voice Dev", "修复"} {
		if !strings.Contains(strings.Join(zh, "\n"), want) {
			t.Errorf("Chinese menu lacks %q:\n%s", want, strings.Join(zh, "\n"))
		}
	}
}

// takesRowTitle is the Takes row's title.
func takesRowTitle(v voiceState) string { return v.takesRow().title }

// The update prompt's notes: English notes in English when the feed has
// them, else the Chinese notes (a feed from before notes_en).
func TestReleaseNotesFollowTheLanguage(t *testing.T) {
	defer l10n.Set(l10n.Current())
	both := &update.Release{Notes: "修复", NotesEN: "Fixes"}
	old := &update.Release{Notes: "修复"}
	for _, c := range []struct {
		l    l10n.Lang
		rel  *update.Release
		want string
	}{{l10n.EN, both, "Fixes"}, {l10n.ZH, both, "修复"}, {l10n.Orig, both, "修复"}, {l10n.EN, old, "修复"}} {
		l10n.Set(c.l)
		if got := releaseNotes(c.rel); got != c.want {
			t.Errorf("lang %v, notes_en %q: %q, want %q", c.l, c.rel.NotesEN, got, c.want)
		}
	}
}
