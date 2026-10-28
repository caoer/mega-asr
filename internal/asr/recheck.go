package asr

import (
	"bufio"
	"context"
	"log"
	"os"
	"path/filepath"
	"slices"
	"strings"
)

// A Recheck is a hot word given only to the clips that need it. A term the
// model mishears (it writes "password" for a spoken "passkey") is fixed by
// listing it, but a standing hot word shifts the decode of every clip; a
// Recheck lists it only when the clip's first decode holds the Trigger, and
// takes the decode with it in place of the first.
type Recheck struct {
	Trigger string   // matched in the first decode's text, any case
	Terms   []string // added to the clip's hot words for the second decode
}

// ReadRechecks reads `trigger<TAB>term[, term]...` lines from each path in
// turn; blank lines and # comments are skipped, a line without a tab, a
// trigger or a term is dropped with a log line. A missing file adds nothing.
func ReadRechecks(paths ...string) []Recheck {
	var rs []Recheck
	for _, path := range paths {
		f, err := os.Open(path)
		if err != nil {
			if !os.IsNotExist(err) {
				log.Printf("asr: rechecks: %v", err)
			}
			continue
		}
		sc := bufio.NewScanner(f)
		for n := 1; sc.Scan(); n++ {
			line := strings.TrimSpace(sc.Text())
			if line == "" || strings.HasPrefix(line, "#") {
				continue
			}
			trigger, terms, _ := strings.Cut(line, "\t")
			r := Recheck{Trigger: strings.TrimSpace(trigger)}
			for _, t := range strings.Split(terms, ",") {
				if t = strings.TrimSpace(t); t != "" {
					r.Terms = append(r.Terms, t)
				}
			}
			if r.Trigger == "" || len(r.Terms) == 0 {
				log.Printf("asr: rechecks: %s:%d dropped: want trigger<TAB>term[, term]", filepath.Base(path), n)
				continue
			}
			rs = append(rs, r)
		}
		f.Close()
	}
	return rs
}

// Rechecking decodes each clip with Transcriber and, when the text holds a
// Recheck's trigger, decodes it again with that Recheck's terms added to the
// hot words. A clip without a trigger gets the first decode unchanged; a
// failed second decode keeps the first.
type Rechecking struct {
	Transcriber
	Rechecks func() []Recheck // read per clip; nil means none
}

func (r Rechecking) Transcribe(ctx context.Context, wav string, whole bool) (string, error) {
	text, err := r.Transcriber.Transcribe(ctx, wav, whole)
	if err != nil || r.Rechecks == nil {
		return text, err
	}
	var add []string
	low := strings.ToLower(text)
	for _, rc := range r.Rechecks() {
		if strings.Contains(low, strings.ToLower(rc.Trigger)) {
			add = append(add, rc.Terms...)
		}
	}
	if len(add) == 0 {
		return text, nil
	}
	again, err := r.Transcriber.Transcribe(context.WithValue(ctx, extraKey{}, add), wav, whole)
	if err != nil {
		log.Printf("asr: recheck of %s: %v; the first decode stands", filepath.Base(wav), err)
		return text, nil
	}
	return again, nil
}

// TranscribeFunc is a function as a Transcriber.
type TranscribeFunc func(ctx context.Context, wav string, whole bool) (string, error)

func (f TranscribeFunc) Transcribe(ctx context.Context, wav string, whole bool) (string, error) {
	return f(ctx, wav, whole)
}

type extraKey struct{}

// withExtra is the clip's hot words: list, then the terms a Recheck added
// through ctx that list lacks, at the end; list gives way at its tail so the
// added terms fit MaxHotwords.
func withExtra(ctx context.Context, list []string) []string {
	extra, _ := ctx.Value(extraKey{}).([]string)
	var add []string
	for _, t := range extra {
		if !slices.Contains(list, t) && !slices.Contains(add, t) {
			add = append(add, t)
		}
	}
	if len(add) == 0 {
		return list
	}
	if keep := MaxHotwords - len(add); len(list) > keep {
		list = list[:max(keep, 0)]
	}
	return append(append([]string{}, list...), add...)
}
