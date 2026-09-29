package meeting

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/caoer/mega-asr/internal/session"
)

// WriteSegments writes segments.json: the list, in time order.
func WriteSegments(path string, segs []Segment) error {
	if segs == nil {
		segs = []Segment{}
	}
	b, err := json.MarshalIndent(segs, "", " ")
	if err != nil {
		return err
	}
	return writeWhole(path, append(b, '\n'))
}

// turnGap is the longest silence inside one turn: a speaker's segments
// closer than this read as one turn.
const turnGap = 3.0

// WriteTranscript writes transcript.md: one line per turn,
// "HH:MM:SS speaker: text", a turn being a speaker's consecutive segments.
func WriteTranscript(path string, segs []Segment) error {
	var b strings.Builder
	for i := 0; i < len(segs); {
		j, parts := i, []string{}
		for ; j < len(segs) && segs[j].Speaker == segs[i].Speaker && (j == i || segs[j].StartS-segs[j-1].EndS < turnGap); j++ {
			parts = append(parts, segs[j].Text)
		}
		if text := session.Join(parts); text != "" {
			fmt.Fprintf(&b, "%s %s: %s\n", clock(segs[i].StartS), segs[i].Speaker, text)
		}
		i = j
	}
	return writeWhole(path, []byte(b.String()))
}

func clock(s float64) string {
	t := int(s)
	return fmt.Sprintf("%02d:%02d:%02d", t/3600, t%3600/60, t%60)
}
