#!/bin/sh
# scripts/release-mac.sh [--dry-run] [--notes TEXT] [--notes-en TEXT] --out DIR TAG
#
# Release Mega Voice for macOS to friends from git tag TAG (v0.MINOR.PATCH).
# nix builds the staging MegaVoice.app at the tag (the release page's feed
# and app token baked in, the engine CLI inside, the version from the tag);
# SIGN_MACOS signs, notarizes and staples it and its dmg under 南山图's
# Developer ID; then the release page
# gets the model files it lacks (record models.megavoice-mac), the dmg as a
# /f/ file, and last the feed record release.megavoice-mac, which every
# staging app polls.
#
#   --dry-run   build, sign, notarize and checksum into DIR; upload and put
#               nothing, print what would go up. A TAG not cut yet builds HEAD.
#   --notes T   what the update prompt says about this version, in Chinese
#   --notes-en T  the same in English (the feed's notes_en); an app whose Mac
#               reads English shows it, an app before 0.1.4 reads notes only
#   --out DIR   the app, dmg, sha256 and release.log (a .noindex folder keeps
#               Spotlight from registering the app)
#
# RELEASE_SLUG    the release page's slug
# PAGES_ORIGIN    the ccc-pages origin it is served from (https://…)
# APP_TOKEN_FILE  a 0600 file holding the page's app-staging viewer token;
#                 never printed, and the log is checked for it
# SIGN_MACOS      the Developer ID signer, scripts/sign-macos.sh of the
#                 mega-update repo (Mega Speak releases through it too)
# PAGE_PUBLISH    the publisher, default page-publish.sh beside SIGN_MACOS:
#                 every upload and put retried over a line that drops, the
#                 feed put only after the dmg's stored sha256 matches
# DMG_HOST        another Mac to build the dmg's image on, for a Mac whose
#                 hdiutil cannot mount (SIGN_MACOS --dmg-host; DMG_SSH is
#                 the ssh command it uses)
# PUBLISH_VIA     an ssh host to publish from, for a Mac whose own uploads
#                 to the pages origin drop (page-publish.sh says what it needs)
# ANNEX_DIRS      where the models' git-annex objects are found (default this
#                 repo's .git/annex/objects)
#
# Runs on a Mac with nix and jq; page-file and page-data with the page owner's
# identity here, or on PUBLISH_VIA.
set -eu

usage() { sed -n '2,6p' "$0" | sed 's/^# //' >&2; exit 2; }
out="" prev=""
for a in "$@"; do
  [ "$prev" = --out ] && out=$a
  prev=$a
done
[ -n "$out" ] || usage
mkdir -p "$out"
out=$(cd "$out" && pwd)

# One log of the whole run, checked for the token at the end.
if [ -z "${RELEASE_MAC_LOGGING:-}" ]; then
  rcf=$out/.rc
  (rc=0; RELEASE_MAC_LOGGING=1 sh "$0" "$@" || rc=$?; echo $rc > "$rcf") 2>&1 | tee "$out/release.log"
  rc=$(cat "$rcf"); rm -f "$rcf"
  tokf=${APP_TOKEN_FILE:-}
  if [ -n "$tokf" ] && [ -s "$tokf" ] && grep -qF -- "$(cat "$tokf")" "$out/release.log"; then
    rm -f "$out/release.log"
    echo "release-mac: the log carried the app token; removed it" >&2
    exit 1
  fi
  exit "$rc"
fi

dry="" notes="" notes_en=""
while [ $# -gt 1 ]; do
  case $1 in
  --dry-run) dry=1; shift ;;
  --notes) notes=$2; shift 2 ;;
  --notes-en) notes_en=$2; shift 2 ;;
  --out) shift 2 ;;
  *) usage ;;
  esac
done
[ $# -eq 1 ] || usage
tag=$1
expr "$tag" : 'v0\.[0-9][0-9]*\.[0-9][0-9]*$' >/dev/null || { echo "release-mac: $tag is not v0.MINOR.PATCH" >&2; exit 2; }

repo=$(cd "$(dirname "$0")/.." && pwd)
slug=${RELEASE_SLUG:?RELEASE_SLUG names the release page}
origin=${PAGES_ORIGIN:?PAGES_ORIGIN is the ccc-pages origin}
sign=${SIGN_MACOS:?SIGN_MACOS is the signer, mega-update scripts/sign-macos.sh}
publish=${PAGE_PUBLISH:-$(dirname "$sign")/page-publish.sh}
[ -f "$publish" ] || { echo "release-mac: no publisher at $publish (set PAGE_PUBLISH)" >&2; exit 2; }
pub() { sh "$publish" "$@"; }
feed="$origin/d/$slug/release.megavoice-mac?raw=1"
ver=${tag#v}
tokf=${APP_TOKEN_FILE:?APP_TOKEN_FILE names the app-staging token file}
[ -s "$tokf" ] || { echo "release-mac: $tokf is empty" >&2; exit 1; }
case $(stat -f %Lp "$tokf") in 600 | 400) ;; *) echo "release-mac: $tokf is not 0600" >&2; exit 1 ;; esac
say() { echo "release-mac: $*"; }

# --- build ---------------------------------------------------------------
if rev=$(git -C "$repo" rev-parse -q --verify "refs/tags/$tag^{commit}"); then
  say "$tag is $rev"
elif [ -n "$dry" ]; then
  rev=$(git -C "$repo" rev-parse HEAD)
  say "$tag is not cut: the dry run builds HEAD $rev as $tag"
else
  echo "release-mac: no tag $tag (git tag -a $tag, push it, then release)" >&2
  exit 1
fi
nixexpr='let f = builtins.getFlake "git+file://'"$repo"'?rev='"$rev"'"; in
  f.packages.aarch64-darwin.megavoice.override {
    channel = "staging"; releaseVersion = "'"$tag"'"; feedURL = "'"$feed"'";
    pageToken = builtins.getEnv "MEGAVOICE_PAGE_TOKEN";
  }'
pkg=$(MEGAVOICE_PAGE_TOKEN=$(cat "$tokf") nix build --impure --no-link --print-out-paths --expr "$nixexpr")
say "built $pkg"
app=$out/MegaVoice.app
rm -rf "$app"
ditto "$pkg/libexec/megavoice/MegaVoice.app" "$app"
chmod -R u+w "$app"
[ -x "$app/Contents/MacOS/llama-funasr-cli" ] || { echo "release-mac: the bundle has no engine CLI" >&2; exit 1; }
[ "$(plutil -extract CFBundleShortVersionString raw "$app/Contents/Info.plist")" = "$ver" ] ||
  { echo "release-mac: the bundle's version is not $ver" >&2; exit 1; }
[ "$(plutil -extract CFBundleIdentifier raw "$app/Contents/Info.plist")" = app.0xdao.megavoice.staging ] ||
  { echo "release-mac: the bundle id is not app.0xdao.megavoice.staging" >&2; exit 1; }
[ "$(plutil -extract CFBundleName raw "$app/Contents/Info.plist")" = "Mega Voice" ] ||
  { echo "release-mac: the bundle's name is not Mega Voice" >&2; exit 1; }
min=$(plutil -extract LSMinimumSystemVersion raw "$app/Contents/Info.plist")

# --- sign, notarize, staple, dmg -----------------------------------------
dmg=$out/Mega-Voice-$ver.dmg
sh "$sign" -e "$repo/packaging/megavoice.entitlements" \
  --dmg "$dmg" --volname "Mega Voice" ${DMG_HOST:+--dmg-host "$DMG_HOST"} "$app"
sha=$(shasum -a 256 "$dmg" | cut -d' ' -f1)
echo "$sha  $(basename "$dmg")" > "$dmg.sha256"
say "dmg $dmg ($(stat -f %z "$dmg") bytes) sha256 $sha"

# --- models: the page's record against the repo's annexed models/ ---------
# Only an absent record (exit 2) means no models; any other failure stops
# here rather than upload 1.3 GB again.
rc=0
have=$(pub get "$slug" models.megavoice-mac) || rc=$?
case $rc in 0) ;; 2) have='{"files":[]}' ;; *) echo "release-mac: reading models.megavoice-mac failed ($rc)" >&2; exit 1 ;; esac
dirs=${ANNEX_DIRS:-$repo/.git/annex/objects}
files='[]' changed=""
for n in $(git -C "$repo" ls-tree --name-only "$rev" models/ | sed 's|^models/||' | grep '\.gguf$'); do
  key=$(basename "$(git -C "$repo" cat-file -p "$rev:models/$n")") # SHA256E-s<size>--<sha>.gguf
  size=${key#SHA256E-s} size=${size%%--*}
  msha=${key#*--} msha=${msha%.gguf}
  url=$(printf '%s' "$have" | jq -r --arg n "$n" --arg s "$msha" '[.files[]? | select(.name == $n and .sha256 == $s)][0].url // empty')
  if [ -n "$url" ]; then
    say "model $n: on the page at $url"
  else
    obj=""
    for d in $dirs; do
      for f in "$d"/*/*/"$key/$key"; do [ -f "$f" ] && obj=$f && break 2; done
    done
    [ -n "$obj" ] || { echo "release-mac: model $n ($key) is in no annex dir: $dirs" >&2; exit 1; }
    if [ -n "$dry" ]; then
      say "model $n: would upload $obj ($size bytes)"
      url="/f/$slug/<upload>"
    else
      id=$(pub upload "$slug" "$obj" "$n" application/octet-stream)
      url="/f/$slug/$id"
      say "model $n: uploaded as $url"
    fi
    changed=1
  fi
  files=$(printf '%s' "$files" | jq -c --arg n "$n" --arg u "$url" --arg s "$msha" --argjson z "$size" '. + [{name: $n, url: $u, sha256: $s, size: $z}]')
done
models=$(jq -cn --argjson f "$files" '{files: $f}')

# --- publish: models, the dmg, then the feed ------------------------------
if [ -n "$dry" ]; then
  [ -z "$changed" ] || say "would put models.megavoice-mac (model-set@1): $models"
  say "would upload $dmg as $(basename "$dmg")"
  say "would put release.megavoice-mac (release-feed@1): $(jq -cn --arg v "$ver" --arg s "$sha" --arg n "$notes" --arg e "$notes_en" --arg m "$min" '{version: $v, url: "/f/<dmg>", sha256: $s, notes: $n, notes_en: $e, min_macos: $m}')"
  say "dry run done: nothing uploaded, nothing put"
  exit 0
fi
[ -z "$changed" ] || pub put "$slug" models.megavoice-mac model-set@1 "$models"
id=$(pub upload "$slug" "$dmg" "$(basename "$dmg")" application/x-apple-diskimage)
release=$(jq -cn --arg v "$ver" --arg u "/f/$slug/$id" --arg s "$sha" --arg n "$notes" --arg e "$notes_en" --arg m "$min" \
  '{version: $v, url: $u, sha256: $s, notes: $n, notes_en: $e, min_macos: $m}')
pub put "$slug" release.megavoice-mac release-feed@1 "$release"
say "released $tag: $release"
