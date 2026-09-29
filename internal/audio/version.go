package audio

import (
	"fmt"
	"runtime/debug"
)

// ChunkerVersion names the cuts chunk.go, level.go and trim.go make: two
// builds with the same version cut the same audio at the same samples. A
// change to the cuts in any of the three files takes a new version
// (version_test.go pins each version's source), so data cut by one version is
// never mistaken for another's.
const ChunkerVersion = 1

// Version is what `asrbench version` and `megavoice version` print: a
// "chunker\t<ChunkerVersion>" line, then "revision\t<commit>" ("+dirty"
// when the tree had changes) when the build recorded its commit.
func Version() string {
	out := fmt.Sprintf("chunker\t%d\n", ChunkerVersion)
	bi, ok := debug.ReadBuildInfo()
	if !ok {
		return out
	}
	var rev, dirty string
	for _, kv := range bi.Settings {
		switch {
		case kv.Key == "vcs.revision":
			rev = kv.Value
		case kv.Key == "vcs.modified" && kv.Value == "true":
			dirty = "+dirty"
		}
	}
	if rev != "" {
		out += fmt.Sprintf("revision\t%s%s\n", rev, dirty)
	}
	return out
}
