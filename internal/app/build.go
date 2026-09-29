package app

import "runtime/debug"

// Build is the source revision package.nix passes by -X; a plain go build
// leaves it empty.
var Build string

// BuildID names this build: Build when set, else the VCS revision go build
// stamps (short, "+dirty" for a modified tree), else "dev".
func BuildID() string {
	if Build != "" {
		return Build
	}
	bi, ok := debug.ReadBuildInfo()
	if !ok {
		return "dev"
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
	if rev == "" {
		return "dev"
	}
	return rev[:min(7, len(rev))] + dirty
}
