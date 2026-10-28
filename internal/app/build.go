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

// A release build's identity, set by -ldflags -X (package.nix channel,
// releaseVersion, feedURL, pageToken). A dev build sets none: Channel "" is
// the dev channel, which never asks a feed for an update.
var (
	Channel   string // "staging": the build friends install from the release page
	Version   string // the release tag, v0.MINOR.PATCH
	FeedURL   string // the release record, https://<host>/d/<slug>/release.megavoice-mac?raw=1
	PageToken string // the release page's app token, which POST /unlock spends
)

// ChannelName is Channel, or "dev" for a build with none.
func ChannelName() string {
	if Channel == "" {
		return "dev"
	}
	return Channel
}

// AppName is the name a person sees, package.nix's appName by -X: the
// bundle's CFBundleName and every menu, alert and prompt.
var AppName string

// Name is AppName, else "Mega Voice" for a release build and
// "Mega Voice Dev" for a dev build with none.
func Name() string {
	switch {
	case AppName != "":
		return AppName
	case Channel != "":
		return "Mega Voice"
	}
	return "Mega Voice Dev"
}
