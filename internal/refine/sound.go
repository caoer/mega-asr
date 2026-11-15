package refine

import (
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/mozillazg/go-pinyin"
)

// sounds is a text as the glossary's matching hears it: a unit per Chinese
// character, as its pinyin syllable, and per run of Latin letters and
// digits, as a word. A term written in one script is heard in the other:
// 海库 sounds like Haiku.
type sounds struct {
	lower string // the text in lower case, for a term written in it as is
	units []unit
}

// unit is one sounded piece of a text.
type unit struct {
	han bool
	syl syllable // a Chinese character's reading, folded
	key string   // its sound in classes (pinyinKey, wordKey)
}

// syllable is a pinyin reading under the dialect fold of the names scan:
// zh/z ch/c sh/s n/l, -ng/-n.
type syllable struct{ initial, final string }

func hearText(text string) sounds {
	s := sounds{lower: strings.ToLower(text)}
	rs := []rune(text)
	for i := 0; i < len(rs); {
		c := rs[i]
		switch {
		case han(c):
			p := reading(c)
			s.units = append(s.units, unit{han: true, syl: fold(p), key: pinyinKey(p)})
			i++
		case latin(c):
			j := i
			for j < len(rs) && latin(rs[j]) {
				j++
			}
			s.units = append(s.units, unit{key: wordKey(strings.ToLower(string(rs[i:j])))})
			i = j
		default:
			i++
		}
	}
	return s
}

// The most units a span heard as one term covers: compound is two
// characters, AskUserQuestion three words.
const maxSpan = 8

// match is how an entry sounds in a text: written in it, or the cost of
// the span that sounds most like it; ok false when none does.
type match struct {
	ok      bool
	written bool
	cost    int // in quarter steps (slip); 0 when written
	weight  int // the classes of the entry's sound: how much it says
}

// hear is how entry sounds in the text. A term all of Chinese characters
// sounds like a span of as many characters whose readings are the same,
// or, from three characters, differ in one that keeps its initial or its
// final. Any other term is heard as its consonants and vowels in classes
// (pinyinKey, wordKey) and sounds like a span that starts as it does and is
// within slip of it; a term of one syllable sounds only like Latin words,
// as too many Chinese characters read like it. A term of fewer than two
// consonants or of no vowel (an abbreviation, said letter by letter), or a
// single character, counts only as written.
func (s sounds) hear(entry string) match {
	if s.written(entry) {
		return match{ok: true, written: true}
	}
	e := hearText(entry)
	if len(e.units) == 0 {
		return match{}
	}
	if e.allHan() {
		if len(e.units) >= 2 && s.hanSpan(e.units) {
			return match{ok: true, weight: 2 * len(e.units)}
		}
		return match{}
	}
	var b strings.Builder
	for _, u := range e.units {
		b.WriteString(u.key)
	}
	k := b.String()
	c := consonants(k)
	limit := slip(c)
	if limit < 0 || c == len(k) {
		return match{}
	}
	latinOnly := len(k)-c < 2
	best := limit + 1
	col := make([]int, len(k)+1)
	for i := range s.units {
		if s.units[i].key == "" || !sameStart(s.units[i].key, k) || latinOnly && s.units[i].han {
			continue
		}
		for r := range col {
			col[r] = 0
			if r > 0 {
				col[r] = col[r-1] + drop(k, r-1)
			}
		}
		n := 0 // symbols of the span
		for j := i; j < len(s.units) && j < i+maxSpan && n <= len(k)+limit && !(latinOnly && s.units[j].han); j++ {
			for _, x := range []byte(s.units[j].key) {
				n++
				diag := col[0]
				col[0] += step(x)
				for r := 1; r <= len(k); r++ {
					up := col[r]
					col[r] = min(diag+swap(k[r-1], x), up+step(x), col[r-1]+drop(k, r-1))
					diag = up
				}
			}
			best = min(best, col[len(k)])
		}
	}
	if best > limit {
		return match{}
	}
	return match{ok: true, cost: best, weight: len(k)}
}

// better reports whether m is a better reason to carry an entry than o:
// written beats heard, and a sound that says more for its slip beats one
// that says less.
func (m match) better(o match) bool {
	switch {
	case m.ok != o.ok:
		return m.ok
	case m.written != o.written:
		return m.written
	}
	return m.evidence() > o.evidence()
}

// evidence is how much a heard match says: the term's sound, less its
// slip.
func (m match) evidence() int { return 4*m.weight - 3*m.cost }

// slip is how far, in quarter steps, a span may sound from a term of c
// consonants: not at all for two, two vowels for three, a consonant or
// four vowels for four and five, and more from six; -1 below two, where
// too many spans of a long dictation would sound like it.
func slip(c int) int {
	switch {
	case c < 2:
		return -1
	case c == 2:
		return 0
	case c == 3:
		return 2
	case c <= 5:
		return 4
	}
	return 6
}

// sameStart reports whether a span of key x may sound like the term of key
// k: both start with the same consonant class, or both with a vowel.
func sameStart(x, k string) bool {
	return x[0] == k[0] || vowel(x[0]) && vowel(k[0])
}

// The costs, in quarter steps: a vowel heard as another, or one more or
// one fewer, costs one, as English spelling says little of a vowel and
// Chinese adds them; a consonant, four.
func step(x byte) int {
	if vowel(x) {
		return 1
	}
	return 4
}

// drop is the cost of a span that lacks the term's k[i]: the consonant
// that ends the term costs half a consonant, as Chinese drops it (compound
// heard as 康胖).
func drop(k string, i int) int {
	if i+1 == len(k) && !vowel(k[i]) {
		return 2
	}
	return step(k[i])
}

func swap(a, b byte) int {
	switch {
	case a == b:
		return 0
	case vowel(a) && vowel(b):
		return 1
	case vowel(a) || vowel(b):
		return 5
	}
	return 4
}

func vowel(x byte) bool { return x >= 'A' && x <= 'Z' }

func consonants(k string) int {
	n := 0
	for i := 0; i < len(k); i++ {
		if !vowel(k[i]) {
			n++
		}
	}
	return n
}

// written reports whether entry occurs in the text in any case, a Latin
// end of it not inside a longer word: Go is not in Google.
func (s sounds) written(entry string) bool {
	e := strings.ToLower(strings.TrimSpace(entry))
	if e == "" {
		return false
	}
	for i := 0; ; {
		j := strings.Index(s.lower[i:], e)
		if j < 0 {
			return false
		}
		at, end := i+j, i+j+len(e)
		first, _ := utf8.DecodeRuneInString(e)
		last, _ := utf8.DecodeLastRuneInString(e)
		before, _ := utf8.DecodeLastRuneInString(s.lower[:at])
		after, _ := utf8.DecodeRuneInString(s.lower[end:])
		if !(latin(first) && latin(before)) && !(latin(last) && latin(after)) {
			return true
		}
		i = at + 1
	}
}

func (s sounds) allHan() bool {
	for _, u := range s.units {
		if !u.han {
			return false
		}
	}
	return len(s.units) > 0
}

// hanSpan reports whether a run of the text's characters reads as e does,
// as hear says.
func (s sounds) hanSpan(e []unit) bool {
	n := len(e)
window:
	for i := 0; i+n <= len(s.units); i++ {
		diff := 0
		for j := 0; j < n; j++ {
			u, w := s.units[i+j], e[j]
			if !u.han {
				continue window
			}
			if u.syl == w.syl {
				continue
			}
			if n < 3 || diff > 0 || u.syl.initial != w.syl.initial && u.syl.final != w.syl.final {
				continue window
			}
			diff++
		}
		return true
	}
	return false
}

// han is the range of Chinese characters the matching reads, U+4E00–U+9FFF.
func han(c rune) bool { return c >= 0x4e00 && c <= 0x9fff }

func latin(c rune) bool { return c < unicode.MaxASCII && (unicode.IsLetter(c) || unicode.IsDigit(c)) }

var plain = pinyin.NewArgs()

// reading is c's most common pinyin reading without tone; c itself when it
// has none.
func reading(c rune) string {
	if p := pinyin.SinglePinyin(c, plain); len(p) > 0 {
		return p[0]
	}
	return string(c)
}

// pinyin's initials, longest first, y and w counted as initials.
var initials = []string{"zh", "ch", "sh", "b", "p", "m", "f", "d", "t", "n", "l", "g", "k", "h", "j", "q", "x", "r", "z", "c", "s", "y", "w"}

func split(p string) (initial, final string) {
	for _, x := range initials {
		if strings.HasPrefix(p, x) && len(p) > len(x) {
			return x, p[len(x):]
		}
	}
	return "", p
}

func fold(p string) syllable {
	ini, fin := split(p)
	switch ini {
	case "zh", "ch", "sh":
		ini = ini[:1]
	case "n":
		ini = "l"
	}
	if strings.HasSuffix(fin, "ng") {
		fin = fin[:len(fin)-1]
	}
	return syllable{ini, fin}
}

// The classes both scripts are heard in: a sound a recogniser writes one
// way in English and another in pinyin falls in one class. Consonants:
//
//	p: b p   t: d t   k: g k q c(hard)   s: s z j x c(soft) sh ch zh th
//	f: f v ph   n: m n ng   l: l r   h: h before a vowel
//
// and a vowel, or a run of them, by its main vowel: A E I O U. y and w
// before a vowel, and an r that ends a syllable, are left out.
var pinyinInitial = map[string]string{
	"b": "p", "p": "p", "d": "t", "t": "t", "g": "k", "k": "k", "h": "h", "f": "f",
	"m": "n", "n": "n", "l": "l", "r": "l",
	"j": "s", "q": "s", "x": "s", "z": "s", "c": "s", "s": "s", "zh": "s", "ch": "s", "sh": "s",
}

// pinyinKey is a syllable in classes: its initial's, its main vowel's, and
// n for a nasal final.
func pinyinKey(p string) string {
	ini, fin := split(p)
	k := pinyinInitial[ini]
	nasal := strings.HasSuffix(fin, "n") || strings.HasSuffix(fin, "ng")
	v := strings.TrimSuffix(strings.TrimSuffix(fin, "g"), "n")
	if len(v) > 1 && strings.ContainsRune("iuv", rune(v[0])) && strings.ContainsAny(v[1:], "aeiou") {
		v = v[1:] // a medial: ia, uo, ian, iong
	}
	if v != "" {
		k += string(vowelClass(v[0]))
	}
	if nasal {
		k += "n"
	}
	return k
}

func vowelClass(c byte) byte {
	switch c {
	case 'a':
		return 'A'
	case 'e':
		return 'E'
	case 'i', 'y':
		return 'I'
	case 'o':
		return 'O'
	}
	return 'U' // u, ü (v)
}

// wordKey is an English word in classes, a consonant class once where the
// word writes it twice in a row (ll, ss, ck), a run of vowels as one: its
// first, but oo as U and ee, ea, ie as I; a final e after a consonant is
// silent. A digit is itself.
func wordKey(w string) string {
	isV := func(i int) bool { return i < len(w) && strings.IndexByte("aeiouy", w[i]) >= 0 }
	var b strings.Builder
	last := -1 // where the letters of the last class put end
	var i, start int
	put := func(c byte) {
		if s := b.String(); len(s) == 0 || s[len(s)-1] != c || last != start {
			b.WriteByte(c)
		}
		last = -2
	}
	for i = 0; i < len(w); i++ {
		start = i
		c := w[i]
		next := byte(0)
		if i+1 < len(w) {
			next = w[i+1]
		}
		switch {
		case c >= '0' && c <= '9':
			b.WriteByte(c)
		case c == 'y' && i == 0 && isV(1), c == 'w' && isV(i+1):
			// a glide: the vowel after it is the sound
		case isV(i):
			j := i
			for j < len(w) && isV(j) {
				j++
			}
			run := w[i:j]
			switch {
			case c == 'e' && j == len(w) && j-i == 1 && i > 1:
				// silent
			case run == "oo" || strings.HasPrefix(run, "oo"):
				put('U')
			case strings.HasPrefix(run, "ee"), strings.HasPrefix(run, "ea"), strings.HasPrefix(run, "ie"):
				put('I')
			default:
				put(vowelClass(c))
			}
			i = j - 1
		case c == 'p' && next == 'h':
			put('f')
			i++
		case (c == 's' || c == 'c' || c == 't') && next == 'h':
			put('s')
			i++
		case c == 'c' && next == 'k':
			put('k')
			i++
		case c == 'n' && next == 'g':
			put('n')
			i++
		case c == 'g' && next == 'h':
			i++
		case c == 'c':
			if next == 'e' || next == 'i' || next == 'y' {
				put('s')
			} else {
				put('k')
			}
		case c == 'x':
			put('k')
			put('s')
		case c == 'h':
			if isV(i + 1) {
				put('h')
			}
		case c == 'r':
			if isV(i + 1) {
				put('l')
			}
		default:
			if k, ok := wordClass[c]; ok {
				put(k)
			}
		}
		if last == -2 {
			last = i + 1
		}
	}
	return b.String()
}

var wordClass = map[byte]byte{
	'b': 'p', 'p': 'p', 'd': 't', 't': 't', 'g': 'k', 'k': 'k', 'q': 'k',
	's': 's', 'z': 's', 'j': 's', 'f': 'f', 'v': 'f', 'm': 'n', 'n': 'n', 'l': 'l',
}
