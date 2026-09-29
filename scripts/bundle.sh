#!/bin/sh
# bundle.sh <binary> <out/Name.app> [Info.plist] — wrap the binary in the app
# bundle macOS attributes TCC grants to. The plist (default packaging/Info.plist,
# MegaVoice's) names the bundle id and CFBundleExecutable, which must be the
# binary's name. Unsigned; install-app.sh signs.
set -eu
[ $# -ge 2 ] && [ $# -le 3 ] || { echo "usage: bundle.sh <binary> <Name.app> [Info.plist]" >&2; exit 2; }
here=$(cd "$(dirname "$0")/.." && pwd)
app=$2
plist=${3:-$here/packaging/Info.plist}
rm -rf "$app"
mkdir -p "$app/Contents/MacOS" "$app/Contents/Resources"
cp "$1" "$app/Contents/MacOS/$(basename "$1")"
cp "$plist" "$app/Contents/Info.plist"
