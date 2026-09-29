// Package scripts carries megameet's Python helpers in the binary: megameet
// writes them out and runs them with uv, which installs the Python and the
// pinned packages each declares inline, once, into its cache.
package scripts

import "embed"

//go:embed *.py
var FS embed.FS
