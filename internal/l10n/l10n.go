// Package l10n picks the language of every string a person reads in
// Mega Voice: menus, alerts, prompts, the overlay, the update and download
// lines, a refusal a phone shows. A release build (the one friends install)
// speaks Chinese when the first of the Mac's preferred languages is Chinese
// and English otherwise; a dev build shows each string as it was written,
// the mix of English and Chinese it has always had.
//
// A call site writes the string as it reads today and its translation:
// T(orig, other), F(orig, other, args...). Exactly one of the two is Chinese
// (TestCallsSpeakBoth, in the packages that call it, holds every call to
// that), so the pair also says which text each language gets.
package l10n

import (
	"fmt"
	"strings"
	"sync/atomic"
	"unicode"
)

// Lang is the language strings are shown in.
type Lang int32

const (
	Orig Lang = iota // a dev build: each string as written
	ZH               // Chinese
	EN               // English
)

var cur atomic.Int32

// Set makes l the language of every string from now on.
func Set(l Lang) { cur.Store(int32(l)) }

// Current is the language Set last chose; Orig until then.
func Current() Lang { return Lang(cur.Load()) }

// FromTag is the language for a preferred-language tag (en-US, zh-Hans-CN,
// zh-TW, yue-Hant-HK): Chinese for zh and yue, English for any other.
func FromTag(tag string) Lang {
	t := strings.ToLower(strings.TrimSpace(tag))
	for _, p := range []string{"zh", "yue"} {
		if t == p || strings.HasPrefix(t, p+"-") || strings.HasPrefix(t, p+"_") {
			return ZH
		}
	}
	return EN
}

// HTML is the page language a served page takes: zh-CN or en. A dev build's
// pages were written in Chinese.
func HTML() string {
	if Current() == EN {
		return "en"
	}
	return "zh-CN"
}

// Chinese reports whether s carries a Han character or Chinese punctuation.
func Chinese(s string) bool {
	for _, r := range s {
		if unicode.Is(unicode.Han, r) || r >= 0x3000 && r <= 0x303f || r >= 0xff00 && r <= 0xffef {
			return true
		}
	}
	return false
}

// T is the string in the current language: orig as written, other its
// translation.
func T(orig, other string) string {
	switch Current() {
	case ZH:
		if Chinese(orig) {
			return orig
		}
		return other
	case EN:
		if Chinese(orig) {
			return other
		}
		return orig
	}
	return orig
}

// F is fmt.Sprintf of T(orig, other).
func F(orig, other string, args ...any) string { return fmt.Sprintf(T(orig, other), args...) }

// Err is an error whose text is T(orig, other) when it is read, so a
// sentinel made at init speaks the language Set chose later. errors.Is
// compares it by identity, as errors.New's.
func Err(orig, other string) error { return &msgErr{orig, other} }

type msgErr struct{ orig, other string }

func (e *msgErr) Error() string { return T(e.orig, e.other) }

// Pair is a table's string and its translation, (orig, other) as T takes
// them; String is the one to show.
type Pair [2]string

func (p Pair) String() string { return T(p[0], p[1]) }
