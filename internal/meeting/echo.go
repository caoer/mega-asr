package meeting

import (
	"strings"
	"unicode"
)

const (
	echoShare  = 0.6 // share of a mic segment's text found in the remote text that marks it an echo
	echoWindow = 2.0 // seconds either side of the mic segment the remote text is taken from
)

// DropEchoes drops the mic segments that are the call played back into the
// mic: at least 60 % of a mic segment's text reappears, in order, in the
// remote text spoken within ±2 s of it. It returns the kept segments and
// how many were dropped.
func DropEchoes(segs []Segment) ([]Segment, int) {
	kept := make([]Segment, 0, len(segs))
	dropped := 0
	for _, m := range segs {
		if m.Track != "mic" || !echo(m, segs) {
			kept = append(kept, m)
			continue
		}
		dropped++
	}
	return kept, dropped
}

func echo(m Segment, segs []Segment) bool {
	mine := echoRunes(m.Text)
	if len(mine) == 0 {
		return false
	}
	var near []rune
	for _, r := range segs {
		if r.Track == "remote" && r.StartS < m.EndS+echoWindow && r.EndS > m.StartS-echoWindow {
			near = append(near, echoRunes(r.Text)...)
		}
	}
	return float64(lcs(mine, near)) >= echoShare*float64(len(mine))
}

// echoRunes is text as compared: letters and digits, lower case.
func echoRunes(s string) []rune {
	var out []rune
	for _, r := range strings.ToLower(s) {
		if unicode.IsLetter(r) || unicode.IsDigit(r) {
			out = append(out, r)
		}
	}
	return out
}

// lcs is the length of the longest common subsequence of a and b.
func lcs(a, b []rune) int {
	if len(a) == 0 || len(b) == 0 {
		return 0
	}
	prev, cur := make([]int, len(b)+1), make([]int, len(b)+1)
	for _, x := range a {
		for j, y := range b {
			switch {
			case x == y:
				cur[j+1] = prev[j] + 1
			case prev[j+1] >= cur[j]:
				cur[j+1] = prev[j+1]
			default:
				cur[j+1] = cur[j]
			}
		}
		prev, cur = cur, prev
	}
	return prev[len(b)]
}
