// Package post rewrites a transcript before delivery: a correction table
// (corrections.go) and filler-word cleanup (filler.go).
package post

// Stage rewrites a transcript.
type Stage func(string) string

// Chain runs its stages in order.
type Chain []Stage

// Apply runs every stage.
func (c Chain) Apply(s string) string {
	for _, st := range c {
		s = st(s)
	}
	return s
}
