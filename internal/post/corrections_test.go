package post

import (
	"bufio"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// crag's rows hold a note, an indented note, an empty row, a CRLF ending
// and a zh: rule, listed in an order the sort has to change.
const crag = "# crag mishearings\n" +
	"\n" +
	"bee lay\tbelay\n" +
	"Gri gri\tGriGri\n" +
	"镁分\t镁粉\n" +
	"  # abseil gear\n" +
	"rap\trappel\n" +
	"rap ring\tanchor ring\r\n" +
	"quick draw\tquickdraw\n" +
	"kuai挂\t快挂\n" +
	"zh:show\t收\n"

func parseCrag(t *testing.T) Corrections {
	t.Helper()
	c, err := ParseCorrections(strings.NewReader(crag))
	if err != nil {
		t.Fatal(err)
	}
	return c
}

// Sorting puts long mishearings ahead of short ones and, at equal byte
// length, a case-sensitive one ahead of a case-folding one. The CRLF is
// trimmed and zh: sets Zh.
func TestParseCorrections(t *testing.T) {
	c := parseCrag(t)
	var from []string
	for _, r := range c {
		from = append(from, r.From)
	}
	want := []string{"quick draw", "rap ring", "Gri gri", "bee lay", "kuai挂", "镁分", "show", "rap"}
	if !slices.Equal(from, want) {
		t.Errorf("order %q, want %q", from, want)
	}
	for _, r := range c {
		if r.Zh != (r.From == "show") {
			t.Errorf("%q: Zh = %v", r.From, r.Zh)
		}
		if r.From == "rap ring" && r.To != "anchor ring" {
			t.Errorf("rap ring -> %q, want anchor ring", r.To)
		}
	}
}

func TestCorrectionsApply(t *testing.T) {
	c := parseCrag(t)
	for _, tc := range []struct{ in, want string }{
		// a lower-case mishearing matches in any case
		{"a quick draw each", "a quickdraw each"},
		{"QUICK DRAW", "quickdraw"},
		{"Bee lay check", "belay check"},
		// a capital letter makes the match case-sensitive
		{"my Gri gri", "my GriGri"},
		{"my gri gri", "my gri gri"},
		// rap ring is rewritten before rap gets a chance to split it
		{"one rap ring, one rap", "one anchor ring, one rappel"},
		// Latin ends match at word boundaries; Chinese beside them counts as one
		{"rapid", "rapid"},
		{"拿kuai挂", "拿快挂"},
		{"akuai挂", "akuai挂"},
		{"一袋镁分", "一袋镁粉"},
		// Zh needs Han before or after, spaces skipped
		{"快show绳", "快收绳"},
		{"show  一点", "收  一点"},
		{"show us the topo", "show us the topo"},
		{"the show wall, 再 show", "the show wall, 再 收"},
		{"showing 岩点", "showing 岩点"},
	} {
		if got := c.Apply(tc.in); got != tc.want {
			t.Errorf("Apply(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

func TestParseCorrectionsErrors(t *testing.T) {
	for _, tc := range []struct{ table, want string }{
		{"bee lay\tbelay\nno tab on this row\n", "line 2"},
		{"# ropes\n\tempty mishearing\n", "line 2"},
		{"zh:\tempty after the prefix\n", "line 1"},
	} {
		_, err := ParseCorrections(strings.NewReader(tc.table))
		if err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("ParseCorrections(%q) = %v, want an error naming %s", tc.table, err, tc.want)
		}
	}
	long := "chalk\t" + strings.Repeat("b", bufio.MaxScanTokenSize) + "\n"
	if _, err := ParseCorrections(strings.NewReader(long)); !errors.Is(err, bufio.ErrTooLong) {
		t.Errorf("over-long line: %v, want %v", err, bufio.ErrTooLong)
	}
}

// CorrectionsFile rereads its path per call: when the table is absent,
// cannot be opened or fails to parse, the input comes back unchanged; a
// rewrite takes hold on the following call.
func TestCorrectionsFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "corrections.tsv")
	const in = "镁分用完了"
	if got := CorrectionsFile(path)(in); got != in {
		t.Errorf("missing file: %q", got)
	}
	if err := os.WriteFile(path, []byte("镁分 no tab\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if got := CorrectionsFile(filepath.Join(path, "below-a-file"))(in); got != in {
		t.Errorf("unopenable path: %q", got)
	}
	st := CorrectionsFile(path)
	if got := st(in); got != in {
		t.Errorf("malformed file: %q", got)
	}
	if err := os.WriteFile(path, []byte("镁分\t镁粉\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if got, want := st(in), "镁粉用完了"; got != want {
		t.Errorf("after the edit: %q, want %q", got, want)
	}
}

func TestChain(t *testing.T) {
	path := filepath.Join(t.TempDir(), "corrections.tsv")
	if err := os.WriteFile(path, []byte(crag), 0o644); err != nil {
		t.Fatal(err)
	}
	chain := Chain{CorrectionsFile(path), Filler(nil)}
	if got, want := chain.Apply("呃，拿kuai挂之前先show一下绳。"), "拿快挂之前先收一下绳。"; got != want {
		t.Errorf("Chain.Apply = %q, want %q", got, want)
	}
}
