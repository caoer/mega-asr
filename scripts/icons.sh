#!/bin/sh
# icons.sh <master.png> — derive the app icons from one square, opaque master
# (1024x1024, full bleed, no rounded corners) and write them into the repo:
#
#   ios/Resources/Assets.xcassets/AppIcon.appiconset/AppIcon.png
#             the master at 1024, alpha removed; iOS masks the corners itself.
#   packaging/MegaVoice.icns
#             macOS draws no mask, so the master sits on an 824-px rounded
#             tile with a soft shadow in a transparent 1024 canvas (Apple's
#             macOS icon grid), 16 px through 1024 px. bundle.sh copies it
#             into the app because packaging/Info.plist names it.
#
# Needs ImageMagick (magick) and iconutil; the outputs are committed, so a
# build needs neither.
set -eu
[ $# -eq 1 ] || { echo "usage: icons.sh <master.png>" >&2; exit 2; }
here=$(cd "$(dirname "$0")/.." && pwd)
tmp=$(mktemp -d)
trap 'rm -rf "$tmp"' EXIT

magick "$1" -resize '1024x1024!' -background white -alpha remove -alpha off -strip \
	"$here/ios/Resources/Assets.xcassets/AppIcon.appiconset/AppIcon.png"

magick "$1" -resize '824x824!' -alpha set \
	\( -size 824x824 xc:none -fill white -draw 'roundrectangle 0,0,823,823,185,185' \) \
	-compose DstIn -composite "$tmp/tile.png"
magick "$tmp/tile.png" \( +clone -background black -shadow 35x12+0+8 \) +swap \
	-background none -compose over -layers merge +repage \
	-gravity center -extent 1024x1024 -strip "$tmp/mac.png"

set=$tmp/MegaVoice.iconset
mkdir "$set"
for s in 16 32 128 256 512; do
	magick "$tmp/mac.png" -resize "${s}x${s}" "$set/icon_${s}x${s}.png"
	magick "$tmp/mac.png" -resize "$((s * 2))x$((s * 2))" "$set/icon_${s}x${s}@2x.png"
done
iconutil -c icns -o "$here/packaging/MegaVoice.icns" "$set"
