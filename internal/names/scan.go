// Package names corrects the people's names a dictated take misspells. A
// pinyin scan finds the spans of the text that sound like a listed person's
// name; only when one of them sounds the same does the gate fire, and one
// Jev request (TypeSafe System One) then judges every candidate span of the
// take: one Choice per span among the people it sounds like and none. The
// scan proposes, the model decides from the sentence, and code applies an
// accepted span as the listed form.
package names

import (
	"encoding/json"
	"os"
	"sort"
	"strings"
	"unicode/utf8"

	"github.com/mozillazg/go-pinyin"
)

// Cap is the most candidates a text offers.
const Cap = 40

// Person is one entry of people.json, the people index snapshot the names
// pull writes: the keys of people_index@1 this package reads.
type Person struct {
	Slug    string   `json:"slug"`
	Name    string   `json:"name"`
	Aliases []string `json:"aliases"`
	// Spoken are the forms the user says, the name aside.
	Spoken []string `json:"spoken"`
}

// Roster is what the scan matches against.
type Roster struct {
	forms  []form              // the scanned forms, name first, in file order
	listed map[string]bool     // every form a person is written as
	others map[string][]string // person → their spoken forms
}

type form struct {
	person, text string
	syl          []syllable
}

// syllable is a character's reading under the dialect fold: zh/z ch/c sh/s
// n/l, -ng/-n.
type syllable struct{ initial, final string }

// ReadRoster reads people.json at path.
func ReadRoster(path string) (*Roster, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var doc struct {
		People []Person `json:"people"`
	}
	if err := json.Unmarshal(b, &doc); err != nil {
		return nil, err
	}
	return NewRoster(doc.People), nil
}

// NewRoster builds a roster. A person's scanned forms are the name and the
// spoken forms, or the aliases when there are none; a form is scanned when
// it is two or more Chinese characters. Every name, alias and spoken form is
// listed, and a listed form is never a candidate.
func NewRoster(people []Person) *Roster {
	r := &Roster{listed: map[string]bool{}, others: map[string][]string{}}
	for _, p := range people {
		if p.Name == "" {
			continue
		}
		spoken := p.Spoken
		if len(spoken) == 0 {
			spoken = p.Aliases
		}
		for _, f := range append(append([]string{p.Name}, p.Aliases...), p.Spoken...) {
			if f != "" {
				r.listed[f] = true
			}
		}
		seen := map[string]bool{}
		for _, f := range append([]string{p.Name}, spoken...) {
			if f == "" || seen[f] {
				continue
			}
			seen[f] = true
			if f != p.Name {
				r.others[p.Name] = append(r.others[p.Name], f)
			}
			if utf8.RuneCountInString(f) >= 2 && allHan(f) {
				r.forms = append(r.forms, form{person: p.Name, text: f, syl: syllables([]rune(f))})
			}
		}
	}
	return r
}

// Empty reports whether the roster scans nothing.
func (r *Roster) Empty() bool { return r == nil || len(r.forms) == 0 }

// Kind is how a candidate sounds against the form.
type Kind string

const (
	Same Kind = "same" // every syllable equal under the fold
	Near Kind = "near" // one syllable differs and still shares its initial or its final
)

// Candidate is a span of the text that sounds like a listed form.
type Candidate struct {
	Span   string `json:"span"`
	Form   string `json:"form"`
	Person string `json:"person"`
	Kind   Kind   `json:"kind"`
	at     int    // rune offset of the first occurrence found
}

// Candidates lists the spans of text that sound like a listed form,
// same-sound first and then in order of appearance, at most Cap. A span that
// is itself listed is not one, nor one that overlaps a listed form written
// in the text: 林晓 beside a correctly written 林晓雨 is part of the name.
func (r *Roster) Candidates(text string) []Candidate {
	if r.Empty() {
		return nil
	}
	rs := []rune(text)
	sy := make([]*syllable, len(rs))
	for i, c := range rs {
		if han(c) {
			s := syllableOf(c)
			sy[i] = &s
		}
	}
	type key struct{ span, form string }
	seen := map[key]bool{}
	var out []Candidate
	for _, f := range r.forms {
		n := len(f.syl)
	window:
		for i := 0; i+n <= len(rs); i++ {
			diff := -1
			for j := 0; j < n; j++ {
				s := sy[i+j]
				if s == nil {
					continue window
				}
				if *s != f.syl[j] {
					if diff >= 0 {
						continue window
					}
					diff = j
				}
			}
			span := string(rs[i : i+n])
			if r.listed[span] || seen[key{span, f.text}] {
				continue
			}
			kind := Same
			if diff >= 0 {
				if sy[i+diff].initial != f.syl[diff].initial && sy[i+diff].final != f.syl[diff].final {
					continue
				}
				kind = Near
			}
			seen[key{span, f.text}] = true
			out = append(out, Candidate{Span: span, Form: f.text, Person: f.person, Kind: kind, at: i})
		}
	}
	sort.SliceStable(out, func(a, b int) bool {
		if (out[a].Kind == Same) != (out[b].Kind == Same) {
			return out[a].Kind == Same
		}
		return out[a].at < out[b].at
	})
	if len(out) > Cap {
		out = out[:Cap]
	}
	kept := out[:0]
	for _, c := range out {
		if !r.overlapsListed(text, c.Span) {
			kept = append(kept, c)
		}
	}
	return kept
}

// overlapsListed reports whether an occurrence of span in text overlaps an
// occurrence of a listed form.
func (r *Roster) overlapsListed(text, span string) bool {
	var at [][2]int
	for f := range r.listed {
		if f == span {
			continue
		}
		for _, i := range indexAll(text, f) {
			at = append(at, [2]int{i, i + len(f)})
		}
	}
	for _, i := range indexAll(text, span) {
		for _, x := range at {
			if x[0] < i+len(span) && i < x[1] {
				return true
			}
		}
	}
	return false
}

// Fires reports whether the gate fires on these candidates: one sounds the
// same as a listed form.
func Fires(cs []Candidate) bool {
	for _, c := range cs {
		if c.Kind == Same {
			return true
		}
	}
	return false
}

func indexAll(s, sub string) []int {
	var out []int
	for i := 0; ; {
		j := strings.Index(s[i:], sub)
		if j < 0 {
			return out
		}
		out = append(out, i+j)
		i += j + 1
	}
}

// han is the scan's range of Chinese characters, U+4E00–U+9FFF.
func han(c rune) bool { return c >= 0x4e00 && c <= 0x9fff }

func allHan(s string) bool {
	for _, c := range s {
		if !han(c) {
			return false
		}
	}
	return true
}

func syllables(rs []rune) []syllable {
	out := make([]syllable, len(rs))
	for i, c := range rs {
		out[i] = syllableOf(c)
	}
	return out
}

var plain = pinyin.NewArgs()

// initials, longest first, with y and w counted as initials.
var initials = []string{"zh", "ch", "sh", "b", "p", "m", "f", "d", "t", "n", "l", "g", "k", "h", "j", "q", "x", "r", "z", "c", "s", "y", "w"}

// syllableOf is c's most common reading split into initial and final, then
// folded. A character with no reading is its own syllable.
func syllableOf(c rune) syllable {
	p := pinyin.SinglePinyin(c, plain)
	if len(p) == 0 {
		return syllable{final: string(c)}
	}
	s := p[0]
	var ini string
	for _, x := range initials {
		if strings.HasPrefix(s, x) && len(s) > len(x) {
			ini = x
			break
		}
	}
	fin := s[len(ini):]
	switch ini {
	case "zh":
		ini = "z"
	case "ch":
		ini = "c"
	case "sh":
		ini = "s"
	case "n":
		ini = "l"
	}
	for _, x := range [][2]string{{"ing", "in"}, {"eng", "en"}, {"ang", "an"}, {"iong", "ong"}} {
		if strings.HasSuffix(fin, x[0]) {
			fin = fin[:len(fin)-len(x[0])] + x[1]
			break
		}
	}
	return syllable{ini, fin}
}

// Pinyin is s with tone marks, a syllable per character, for the question.
func Pinyin(s string) string {
	a := pinyin.NewArgs()
	a.Style = pinyin.Tone
	var out []string
	for _, c := range s {
		if p := pinyin.SinglePinyin(c, a); len(p) > 0 {
			out = append(out, p[0])
		} else {
			out = append(out, string(c))
		}
	}
	return strings.Join(out, " ")
}
