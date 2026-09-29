package session

import (
	"strings"
	"unicode/utf8"

	"github.com/caoer/mega-asr/internal/asr"
)

// Join stitches chunk texts in order: nothing between them when either side
// is CJK (the CLI's own join between VAD segments), one space between two
// Latin or digit sides. Empty chunks are skipped.
func Join(parts []string) string {
	var b strings.Builder
	var last rune
	for _, p := range parts {
		p = strings.TrimSpace(p)
		if p == "" {
			continue
		}
		first, _ := utf8.DecodeRuneInString(p)
		if b.Len() > 0 && !asr.CJK(last) && !asr.CJK(first) {
			b.WriteByte(' ')
		}
		b.WriteString(p)
		last, _ = utf8.DecodeLastRuneInString(p)
	}
	return b.String()
}
