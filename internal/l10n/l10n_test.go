package l10n

import (
	"errors"
	"testing"
)

// The Mac's first preferred language picks: any Chinese is Chinese, every
// other language English.
func TestFromTag(t *testing.T) {
	for tag, want := range map[string]Lang{
		"zh-Hans-CN": ZH, "zh-Hant-TW": ZH, "zh-HK": ZH, "zh": ZH, "yue-Hant-HK": ZH, "zh_CN": ZH,
		"en-US": EN, "en": EN, "ja-JP": EN, "de-DE": EN, "": EN, "zu": EN, "yue2": EN,
	} {
		if got := FromTag(tag); got != want {
			t.Errorf("FromTag(%q) = %v, want %v", tag, got, want)
		}
	}
}

// T shows the string as written in a dev build, else the one in the
// chosen language, whichever of the pair is written in it.
func TestT(t *testing.T) {
	defer Set(Current())
	for _, c := range []struct {
		l           Lang
		orig, other string
		want        string
	}{
		{Orig, "停止并粘贴", "Stop and Paste", "停止并粘贴"},
		{Orig, "Dictation", "听写", "Dictation"},
		{ZH, "停止并粘贴", "Stop and Paste", "停止并粘贴"},
		{ZH, "Dictation", "听写", "听写"},
		{EN, "停止并粘贴", "Stop and Paste", "Stop and Paste"},
		{EN, "Dictation", "听写", "Dictation"},
		{ZH, "Input: ", "输入：", "输入："},
	} {
		Set(c.l)
		if got := T(c.orig, c.other); got != c.want {
			t.Errorf("lang %v: T(%q, %q) = %q, want %q", c.l, c.orig, c.other, got, c.want)
		}
	}
	Set(EN)
	if got := F("新版本 %s", "Version %s", "0.1.4"); got != "Version 0.1.4" {
		t.Errorf("F = %q", got)
	}
	if HTML() != "en" {
		t.Errorf("HTML() in English = %q", HTML())
	}
	Set(Orig)
	if HTML() != "zh-CN" {
		t.Errorf("HTML() in a dev build = %q", HTML())
	}
}

// An Err made before Set speaks the language chosen after, and keeps its
// identity for errors.Is.
func TestErr(t *testing.T) {
	defer Set(Current())
	Set(Orig)
	e := Err("还在录音", "Still recording")
	wrapped := errors.Join(errors.New("x"), e)
	Set(EN)
	if e.Error() != "Still recording" || !errors.Is(wrapped, e) {
		t.Errorf("Err in English: %q, Is %v", e.Error(), errors.Is(wrapped, e))
	}
	Set(ZH)
	if e.Error() != "还在录音" {
		t.Errorf("Err in Chinese: %q", e.Error())
	}
}
