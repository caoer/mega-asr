package names

import (
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"sort"
	"strings"
	"unicode"
	"unicode/utf8"
)

// Index is the people index as the meetings names pass reads it: each
// person's name, spoken forms and context-only mishearings, the pinyin scan
// over them, and what the guards check an edit against.
type Index struct {
	People  []Person            // Name with its gloss dropped; Spoken the forms the pass offers
	Heard   []map[string]string // per person: heard-context form → the form it stands for ("" when not given)
	Updated string              // the index's updated_at

	scan   *Roster
	listed map[string]bool  // every name and form, folded
	owner  map[string][]int // folded form → people
	byName map[string]int
	bySlug map[string]int // any page's slug → person
	heard  map[string][]int
}

// indexEntry is one people_index@1 entry with the names-list keys; an absent
// key reads empty.
type indexEntry struct {
	Slug         string     `json:"slug"`
	Name         string     `json:"name"`
	Aliases      []string   `json:"aliases"`
	Spoken       []string   `json:"spoken"`
	HeardContext heardForms `json:"heard-context"`
	Pages        []struct {
		Slug string `json:"slug"`
	} `json:"pages"`
}

// heardForms is heard-context: a map of heard form → the form meant, or a
// list of heard forms.
type heardForms map[string]string

func (h *heardForms) UnmarshalJSON(b []byte) error {
	var m map[string]string
	if err := json.Unmarshal(b, &m); err == nil {
		*h = m
		return nil
	}
	var l []string
	if err := json.Unmarshal(b, &l); err != nil {
		return err
	}
	*h = map[string]string{}
	for _, x := range l {
		(*h)[x] = ""
	}
	return nil
}

var gloss = regexp.MustCompile(`\s*[(（].*[)）]\s*$`)

// FromIndex reads a people.index record's value. A person's forms are its
// spoken forms; an entry without them falls back to its aliases other than
// its slugs and its name.
func FromIndex(data []byte) (*Index, error) {
	var doc struct {
		UpdatedAt string       `json:"updated_at"`
		People    []indexEntry `json:"people"`
	}
	if err := json.Unmarshal(data, &doc); err != nil {
		return nil, fmt.Errorf("people index: %w", err)
	}
	x := &Index{Updated: doc.UpdatedAt, listed: map[string]bool{}, owner: map[string][]int{}, byName: map[string]int{}, bySlug: map[string]int{}, heard: map[string][]int{}}
	for _, e := range doc.People {
		name := strings.TrimSpace(gloss.ReplaceAllString(e.Name, ""))
		if _, dup := x.byName[name]; e.Slug == "" || name == "" || dup {
			continue
		}
		skip := map[string]bool{fold(e.Slug): true, fold(name): true, fold(e.Name): true}
		for _, pg := range e.Pages {
			skip[fold(pg.Slug)] = true
		}
		src := e.Spoken
		if len(src) == 0 {
			src = e.Aliases
		}
		var forms []string
		for _, f := range src {
			if f = strings.TrimSpace(f); f != "" && !skip[fold(f)] {
				skip[fold(f)] = true
				forms = append(forms, f)
			}
		}
		heard := map[string]string{}
		for h, to := range e.HeardContext {
			if h = strings.TrimSpace(h); h != "" {
				heard[h] = strings.TrimSpace(to)
			}
		}
		i := len(x.People)
		x.People = append(x.People, Person{Slug: e.Slug, Name: name, Spoken: forms})
		x.Heard = append(x.Heard, heard)
		x.byName[name] = i
		x.bySlug[e.Slug] = i
		for _, pg := range e.Pages {
			if _, taken := x.bySlug[pg.Slug]; !taken && pg.Slug != "" {
				x.bySlug[pg.Slug] = i
			}
		}
		for _, f := range append([]string{name}, forms...) {
			k := fold(f)
			x.listed[k] = true
			x.owner[k] = append(x.owner[k], i)
		}
		for h := range heard {
			x.heard[h] = append(x.heard[h], i)
		}
	}
	x.scan = NewRoster(x.People)
	return x, nil
}

// Person is the person holding slug as any of its pages.
func (x *Index) Person(slug string) (int, bool) {
	i, ok := x.bySlug[slug]
	return i, ok
}

// Listed says whether s is a name or form of anyone.
func (x *Index) Listed(s string) bool { return x.listed[fold(s)] }

// fold compares forms: case, width and spaces aside.
func fold(s string) string {
	var b strings.Builder
	for _, c := range strings.ToLower(s) {
		if c >= '！' && c <= '～' {
			c -= 0xFEE0
		}
		if !unicode.IsSpace(c) {
			b.WriteRune(c)
		}
	}
	return b.String()
}

// Heard is a candidate that is a person's context-only mishearing.
const Heard Kind = "heard"

// Candidates is the pinyin scan's candidates (same-sound first, at most Cap),
// then every heard-context form in text.
func (x *Index) Candidates(text string) []Candidate {
	out := x.scan.Candidates(text)
	hs := make([]string, 0, len(x.heard))
	for h := range x.heard {
		hs = append(hs, h)
	}
	sort.Strings(hs)
	for _, h := range hs {
		if strings.Contains(text, h) {
			for _, i := range x.heard[h] {
				form := x.Heard[i][h]
				if form == "" {
					form = x.People[i].Name
				}
				out = append(out, Candidate{Span: h, Form: form, Person: x.People[i].Name, Kind: Heard})
			}
		}
	}
	return out
}

// Prompt is the system prompt of one call over one window of meeting text.
const Prompt = `You repair person names in speech-recognition transcripts. A recognizer often writes a person's name as a homophone, a similar-sounding word, or a transliteration.

You get PEOPLE (the people who may be mentioned, each with the forms said aloud), sometimes SPEAKERS (who is in this meeting), TEXT (one transcript passage, Chinese mixed with English), and CANDIDATES.

CANDIDATES are spots a pinyin scan found in TEXT that sound the same as, or (marked near) close to, a form of someone in PEOPLE: ` + "`<as written> ~ <form> (<person>)`" + `. The scan knows nothing about meaning: most candidates are ordinary words or other people, and a real mention can be missing from the list. Use the list to see what sounds alike; decide from the sentence whether a person is named there.

Find every place in TEXT where a person from PEOPLE is mentioned and the name is written wrong, and return the repair.

- Repair only where both hold: the written form sounds like one of that person's spoken forms (same or near pinyin, a dialect n/l, zh/z, -ng/-n slip, or a transliteration), and the sentence talks about or to a person at that spot.
- Write the form that was said: a given name stays a given name, a nickname stays a nickname. Do not expand it to the full name.
- "to" must be exactly one of the forms listed for that person in PEOPLE: the full name or a spoken form.
- Never edit a form that is itself listed in PEOPLE. It is already a correctly written name, of this person or of another.
- SPEAKERS are the people most likely to be mentioned or addressed; someone outside SPEAKERS can still be mentioned.
- Ordinary words stay, even when they sound like a name. Names of people who are not in PEOPLE stay as written.
- Change nothing else: no punctuation, no grammar, no other words.

Reply with compact JSON on one line and nothing else, no explanation and no second answer:
{"edits":[{"from":"<exact substring of TEXT, no longer than the wrong name>","to":"<the form said, spelled right>","person":"<that person's full name as listed in PEOPLE>"}]}
When nothing needs repair: {"edits":[]}`

// User is the user message of one call: PEOPLE, SPEAKERS when there are
// any, TEXT and CANDIDATES.
func (x *Index) User(text string, speakers []string) string {
	lines := make([]string, 0, len(x.People))
	for _, p := range x.People {
		if len(p.Spoken) > 0 {
			lines = append(lines, p.Name+": "+strings.Join(p.Spoken, ", "))
		} else {
			lines = append(lines, p.Name)
		}
	}
	parts := []string{"PEOPLE\n" + strings.Join(lines, "\n")}
	if len(speakers) > 0 {
		parts = append(parts, "SPEAKERS\n"+strings.Join(speakers, ", "))
	}
	parts = append(parts, "TEXT\n"+text)
	var cs []string
	for _, c := range x.Candidates(text) {
		line := fmt.Sprintf("%s ~ %s (%s)", c.Span, c.Form, c.Person)
		if c.Kind != Same {
			line += " " + string(c.Kind)
		}
		cs = append(cs, line)
	}
	if len(cs) == 0 {
		cs = []string{"(none)"}
	}
	parts = append(parts, "CANDIDATES\n"+strings.Join(cs, "\n"))
	return strings.Join(parts, "\n\n")
}

// ErrNoAnswer is a reply with no JSON object holding edits.
var ErrNoAnswer = errors.New("the reply holds no edits object")

// Parse reads the last JSON object with an `edits` list in a reply.
func Parse(reply string) ([]Edit, error) {
	var best []Edit
	found := false
	for i := 0; ; {
		j := strings.IndexByte(reply[i:], '{')
		if j < 0 {
			break
		}
		i += j
		dec := json.NewDecoder(strings.NewReader(reply[i:]))
		var d struct {
			Edits []json.RawMessage `json:"edits"`
		}
		if err := dec.Decode(&d); err != nil || d.Edits == nil {
			i++
			continue
		}
		best, found = best[:0], true
		for _, raw := range d.Edits {
			var e Edit
			if json.Unmarshal(raw, &e) == nil {
				best = append(best, e)
			}
		}
		i += int(dec.InputOffset())
	}
	if !found {
		return nil, ErrNoAnswer
	}
	return best, nil
}

// Guard says whether an edit may be applied to text, and the person it
// names; why says which guard refused it. The guards: from is in the text;
// to is a listed form of the person named (of anyone when none is named);
// from is not itself a listed form of anyone; and from sounds like to, or is
// a heard-context form of that person.
func (x *Index) Guard(text string, e Edit) (person int, why string) {
	f, t := strings.TrimSpace(e.From), strings.TrimSpace(e.To)
	switch {
	case f == "" || t == "" || f == t:
		return -1, "empty or unchanged"
	case !strings.Contains(text, f):
		return -1, "from is not in the text"
	}
	person = -1
	for _, i := range x.owner[fold(t)] {
		if e.Person == "" || fold(x.People[i].Name) == fold(e.Person) {
			person = i
			break
		}
	}
	if person < 0 {
		return -1, "to is not a listed form of the person named"
	}
	if x.listed[fold(f)] {
		return -1, "from is itself a listed name"
	}
	if _, ok := x.Heard[person][f]; ok {
		return person, ""
	}
	if !SoundsLike(f, t) {
		return -1, "from does not sound like to"
	}
	return person, ""
}

// SoundsLike: two Latin names within two letter edits; a Latin name written
// in Han; two Han forms with the same folded syllables, or of equal length
// two or more one syllable apart, or from one syllable longer with to's
// syllables in order inside it (a stutter).
func SoundsLike(from, to string) bool {
	fl, tl := !hasHan(from), !hasHan(to)
	if fl && tl {
		return levenshtein(strings.ToLower(strings.ReplaceAll(from, " ", "")), strings.ToLower(strings.ReplaceAll(to, " ", ""))) <= 2
	}
	if fl != tl {
		return tl
	}
	a, b := syllables([]rune(hanOnly(from))), syllables([]rune(hanOnly(to)))
	if len(a) == len(b) {
		d := 0
		for i := range a {
			if a[i] != b[i] {
				d++
			}
		}
		return d == 0 || (d == 1 && len(b) >= 2)
	}
	if len(b) >= 2 && len(a) == len(b)+1 {
		for skip := range a {
			rest := append(append([]syllable(nil), a[:skip]...), a[skip+1:]...)
			same := true
			for i := range rest {
				if rest[i] != b[i] {
					same = false
					break
				}
			}
			if same {
				return true
			}
		}
	}
	return false
}

// fire is an edit applied at one place.
type fire struct {
	at       int // byte offset in the text before the edit
	from, to string
	person   int
}

// applyFires applies edits (already guarded, each with its person) on every
// occurrence of their from, longest from first, never two over one
// character. It returns the new text and the fires, in text order.
func applyFires(text string, edits []Edit, persons []int) (string, []fire) {
	idx := make([]int, len(edits))
	for i := range idx {
		idx[i] = i
	}
	sort.SliceStable(idx, func(a, b int) bool {
		return len(strings.TrimSpace(edits[idx[a]].From)) > len(strings.TrimSpace(edits[idx[b]].From))
	})
	taken := make([]bool, len(text))
	var fires []fire
	for _, k := range idx {
		from, to := strings.TrimSpace(edits[k].From), strings.TrimSpace(edits[k].To)
		if from == "" {
			continue
		}
		for off := 0; ; {
			j := strings.Index(text[off:], from)
			if j < 0 {
				break
			}
			s, e := off+j, off+j+len(from)
			free := true
			for c := s; c < e; c++ {
				free = free && !taken[c]
			}
			if free {
				for c := s; c < e; c++ {
					taken[c] = true
				}
				fires = append(fires, fire{s, from, to, persons[k]})
			}
			off = s + 1
			for off < len(text) && !utf8.RuneStart(text[off]) {
				off++
			}
		}
	}
	sort.Slice(fires, func(a, b int) bool { return fires[a].at < fires[b].at })
	var b strings.Builder
	p := 0
	for _, f := range fires {
		b.WriteString(text[p:f.at])
		b.WriteString(f.to)
		p = f.at + len(f.from)
	}
	b.WriteString(text[p:])
	return b.String(), fires
}

// Join is texts read as one passage: a space only between two non-Han ends.
func Join(parts []string) string {
	var b strings.Builder
	var last rune
	for _, p := range parts {
		p = strings.TrimSpace(p)
		if p == "" {
			continue
		}
		first, _ := utf8.DecodeRuneInString(p)
		if b.Len() > 0 && !wide(last) && !wide(first) {
			b.WriteByte(' ')
		}
		b.WriteString(p)
		last, _ = utf8.DecodeLastRuneInString(p)
	}
	return b.String()
}

func wide(c rune) bool { return han(c) || (c >= 0x3000 && c <= 0x303f) || (c >= 0xff00 && c <= 0xffef) }

func hasHan(s string) bool {
	for _, c := range s {
		if han(c) {
			return true
		}
	}
	return false
}

func hanOnly(s string) string {
	var b strings.Builder
	for _, c := range s {
		if han(c) {
			b.WriteRune(c)
		}
	}
	return b.String()
}

func levenshtein(a, b string) int {
	x, y := []rune(a), []rune(b)
	prev := make([]int, len(y)+1)
	for j := range prev {
		prev[j] = j
	}
	for i := 1; i <= len(x); i++ {
		cur := make([]int, len(y)+1)
		cur[0] = i
		for j := 1; j <= len(y); j++ {
			c := 1
			if x[i-1] == y[j-1] {
				c = 0
			}
			cur[j] = min(prev[j]+1, cur[j-1]+1, prev[j-1]+c)
		}
		prev = cur
	}
	return prev[len(y)]
}
