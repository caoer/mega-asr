#!/bin/sh
# bundle.sh <binary> <out/Name.app> [Info.plist] — wrap the binary in the app
# bundle macOS attributes TCC grants to. The plist (default packaging/Info.plist,
# MegaVoice's) names the bundle id and CFBundleExecutable, which must be the
# binary's name. A plist that names CFBundleIconFile gets that .icns from its
# own directory. VERSION (v0.MINOR.PATCH, a release tag) sets the bundle's
# version strings. CHANNEL (a release channel, package.nix's channel) appends
# .CHANNEL to the plist's bundle id: a staging build is its own app to macOS,
# so its TCC grants, login item and preferences never touch a dev build's on
# the same Mac. NAME (package.nix's appName, default the plist's
# CFBundleName) is the name a person sees: CFBundleName, and for each of the
# plist's CFBundleLocalizations the <lang>.lproj/InfoPlist.strings beside it,
# copied with NAME for @NAME@. The plist's own CFBundleDisplayName stays the
# file name (MegaVoice): Finder shows the localized name only while the two
# match, so an update that keeps MegaVoice.app still reads NAME. Unsigned;
# install-app.sh or sign-macos.sh signs.
set -eu
[ $# -ge 2 ] && [ $# -le 3 ] || { echo "usage: bundle.sh <binary> <Name.app> [Info.plist]" >&2; exit 2; }
here=$(cd "$(dirname "$0")/.." && pwd)
app=$2
plist=${3:-$here/packaging/Info.plist}
rm -rf "$app"
mkdir -p "$app/Contents/MacOS" "$app/Contents/Resources"
cp "$1" "$app/Contents/MacOS/$(basename "$1")"
cp "$plist" "$app/Contents/Info.plist"
icon=$(sed -n '/<key>CFBundleIconFile<\/key>/{n;s/.*<string>\(.*\)<\/string>.*/\1/p;}' "$plist")
[ -z "$icon" ] || cp "$(dirname "$plist")/$icon.icns" "$app/Contents/Resources/$icon.icns"
if [ -n "${CHANNEL:-}" ]; then
  case $CHANNEL in *[!a-z0-9-]*) echo "bundle.sh: CHANNEL '$CHANNEL' is not [a-z0-9-]" >&2; exit 2 ;; esac
  sed "/<key>CFBundleIdentifier<\/key>/{n;s|</string>|.$CHANNEL</string>|;}" "$plist" >"$app/Contents/Info.plist"
fi
if [ -n "${NAME:-}" ]; then
  case $NAME in *[!A-Za-z0-9\ -]*) echo "bundle.sh: NAME '$NAME' is not [A-Za-z0-9 -]" >&2; exit 2 ;; esac
  NAME=$NAME perl -0pi -e 's|(<key>CFBundleName</key>\s*<string>)[^<]*|${1}$ENV{NAME}|' "$app/Contents/Info.plist"
fi
name=$(perl -0ne 'print $1 if m|<key>CFBundleName</key>\s*<string>([^<]*)|' "$app/Contents/Info.plist")
for lang in $(perl -0ne 'print "$1\n" if m|<key>CFBundleLocalizations</key>\s*<array>(.*?)</array>|s' "$plist" | sed -n 's|.*<string>\(.*\)</string>.*|\1|p'); do
  strings=$(dirname "$plist")/$lang.lproj/InfoPlist.strings
  [ -f "$strings" ] || { echo "bundle.sh: CFBundleLocalizations names $lang, but $strings is missing" >&2; exit 2; }
  mkdir -p "$app/Contents/Resources/$lang.lproj"
  NAME=$name perl -pe 's|\@NAME\@|$ENV{NAME}|g' "$strings" >"$app/Contents/Resources/$lang.lproj/InfoPlist.strings"
done
if [ -n "${VERSION:-}" ]; then
  v=${VERSION#v}
  perl -0pi -e "s|(<key>CFBundleShortVersionString</key>\\s*<string>)[^<]*|\${1}$v|; s|(<key>CFBundleVersion</key>\\s*<string>)[^<]*|\${1}$v|" "$app/Contents/Info.plist"
fi
